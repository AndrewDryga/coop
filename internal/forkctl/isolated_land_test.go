package forkctl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
)

func isolatedTestFork(t *testing.T) (string, string, forkspace.Identity, *Control) {
	t.Helper()
	repo := initRepo(t)
	return isolatedTestForkFromRepo(t, repo)
}

func isolatedTestForkFromRepo(t *testing.T, repo string) (string, string, forkspace.Identity, *Control) {
	t.Helper()
	base, err := observed(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := forkspace.SetupIsolatedContext(context.Background(), repo, "isolated", base)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureIsolatedGenerationLocked(repo, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	return repo, ws, identity, &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
}

func isolatedTestLFSFile(t *testing.T, repo string, data []byte) []byte {
	t.Helper()
	oid := fmt.Sprintf("%x", sha256.Sum256(data))
	pointer := []byte(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, len(data)))
	metadata, err := forkspace.GitMetadataDirectories(repo)
	if err != nil {
		t.Fatal(err)
	}
	object := filepath.Join(metadata[len(metadata)-1], "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(object), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "data.bin"), pointer, 0o755); err != nil {
		t.Fatal(err)
	}
	return pointer
}

func TestIsolatedPublicationHydratesChangedNativeLFSAndLinkedParents(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint("linked=", linked), func(t *testing.T) {
			repo := initRepo(t)
			writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
			old := []byte("old native payload\n")
			isolatedTestLFSFile(t, repo, old)
			git(t, repo, "add", ".gitattributes", "data.bin")
			git(t, repo, "commit", "-qm", "trusted LFS baseline")
			if linked {
				path := filepath.Join(t.TempDir(), "linked")
				git(t, repo, "worktree", "add", "-qb", "linked-target", path)
				repo = path
			}
			if err := os.WriteFile(filepath.Join(repo, "data.bin"), old, 0o755); err != nil {
				t.Fatal(err)
			}
			repo, ws, _, c := isolatedTestForkFromRepo(t, repo)
			payload := bytes.Repeat([]byte("new native payload\n"), 20000)
			isolatedTestLFSFile(t, ws, payload)
			git(t, ws, "add", "data.bin")
			git(t, ws, "commit", "-qm", "LFS payload change")
			head, _ := observed(ws, "rev-parse", "HEAD")
			if err := forkspace.HydrateLFS(t.Context(), ws, head); err != nil {
				t.Fatal(err)
			}
			// Invalidate native cached stat data: byte-based qualification must still succeed.
			changedTime := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(repo, "data.bin"), changedTime, changedTime); err != nil {
				t.Fatal(err)
			}
			result, err := c.mergeOne(repo, "", "isolated", false)
			defer result.approval.close()
			if err != nil || !result.landed || result.approval == nil {
				t.Fatalf("changed hydrated LFS publication: %+v, %v", result, err)
			}
			published, _ := observed(repo, "rev-parse", "HEAD")
			if err := forkspace.VerifyLFS(t.Context(), repo, published); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(repo, "data.bin"))
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("parent got pointers/incomplete payload: %v", err)
			}
			if err := result.approval.validateRemoval(repo, "isolated"); err != nil {
				t.Fatalf("clean native LFS source falsely kept: %v", err)
			}
		})
	}
}

func TestIsolatedPublicationPreservesNativeGitlinksAndNestedUntrackedWork(t *testing.T) {
	repo := initRepo(t)
	leaf := initRepo(t)
	git(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", leaf, "leaf")
	git(t, repo, "commit", "-qam", "trusted child")
	parentChild := filepath.Join(repo, "leaf")
	metadata, err := forkspace.GitMetadataDirectories(parentChild)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := map[string][]byte{}
	for _, name := range []string{"HEAD", "config", "index"} {
		path := filepath.Join(metadata[0], name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		snapshots[path] = data
	}
	repo, ws, _, c := isolatedTestForkFromRepo(t, repo)
	isolatedTestCommit(t, ws, "new.txt", "parent only\n")
	writeTaskFile(t, filepath.Join(ws, "leaf", "valuable.txt"), "uncommitted child work\n")
	result, err := c.mergeOne(repo, "", "isolated", false)
	defer result.approval.close()
	if err != nil || !result.landed || result.approval == nil {
		t.Fatalf("unchanged native gitlink publication: %+v, %v", result, err)
	}
	for path, expected := range snapshots {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(expected, got) {
			t.Fatalf("parent child metadata changed: %s %v", path, err)
		}
	}
	if err := result.approval.validateRemoval(repo, "isolated"); err == nil {
		t.Fatal("nested untracked work permitted automatic deletion")
	}
	if data, err := os.ReadFile(filepath.Join(ws, "leaf", "valuable.txt")); err != nil || string(data) != "uncommitted child work\n" {
		t.Fatal("nested source work lost", err)
	}
}

func TestIsolatedPublicationDeniesAutomaticFilesystemAliases(t *testing.T) {
	for _, path := range []string{".GITATTRIBUTES", ".GITMODULES", ".AGENT/DOCKERFILE", ".CoDeX/config.toml", ".gita\u200bttributes"} {
		t.Run(path, func(t *testing.T) {
			repo, ws, _, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, path, "untrusted automatic input\n")
			before, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.mergeOne(repo, "", "isolated", false)
			if err == nil || result.landed {
				t.Fatalf("automatic filesystem alias accepted: %v", err)
			}
			after, err := captureIsolatedParent(repo)
			if err != nil || !sameIsolatedParent(before, after) {
				t.Fatal("refused alias changed parent", err)
			}
		})
	}
}

func TestIsolatedZeroChangeCandidateFinalizesOriginalTaskAuthority(t *testing.T) {
	repo, ws, root, identity, c := prepareForkTaskCandidateMode(t, "isolated-zero-change", true, false)
	result, err := c.mergeOneMode(repo, "", identity.Name, false, true)
	defer result.approval.close()
	if err != nil || !result.landed || result.skipped {
		t.Fatalf("zero-change candidate skipped task authority: %+v %v", result, err)
	}
	if item, ok := mustCurrentTask(t, root, "canonical-task"); !ok || item.State != tasks.StateDone {
		t.Fatalf("canonical no-change outcome not finalized: %+v %v", item, ok)
	}
	if state, err := tasks.ReadForkTaskStateSummary(repo, identity); err != nil || state.Active() {
		t.Fatalf("task authority remains active: %+v %v", state, err)
	}
	if _, err := os.Stat(ws); err != nil {
		t.Fatal("test land unexpectedly deleted source", err)
	}
}

func TestIsolatedReviewKeepsCanonicalAgentNotes(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidateMode(t, "isolated-notes", true, true)
	assignments, err := tasks.ForkAssignments(repo, identity)
	if err != nil || len(assignments) != 1 {
		t.Fatal("fixture assignment", err)
	}
	owner := assignments[0].Record.Fork
	item, ok := mustCurrentTask(t, owner.Projection, "canonical-task")
	if !ok {
		t.Fatal("fixture projection missing")
	}
	writeTaskFile(t, filepath.Join(item.Dir, "log.md"), "Exact canonical review note\n")
	out := captureStdout(t, func() {
		code, err := c.forkReviewIsolated(repo, ws, identity.Name, identity, true, false, false, false)
		if err != nil || code != 0 {
			t.Fatalf("review: %d, %v", code, err)
		}
	})
	if !strings.Contains(out, "Exact canonical review note") || !strings.Contains(out, "Into: main") {
		t.Fatal("review lost original authority/target", out)
	}
}

func isolatedTestCommit(t *testing.T, ws, path, text string) string {
	t.Helper()
	writeTaskFile(t, filepath.Join(ws, path), text)
	git(t, ws, "add", "--", path)
	git(t, ws, "commit", "-qm", "change "+path)
	head, err := observed(ws, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestIsolatedPublicationLeavesSourceAndSupportsKeptFork(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	first := isolatedTestCommit(t, ws, "first.txt", "first\n")
	// Force a rewritten candidate by independently advancing the trusted parent.
	isolatedTestCommit(t, repo, "parent.txt", "parent\n")
	result, err := c.mergeOne(repo, "", "isolated", false)
	defer result.approval.close()
	if err != nil || !result.landed || result.approval == nil {
		t.Fatalf("land: %+v, %v", result, err)
	}
	parent, _ := observed(repo, "rev-parse", "HEAD")
	source, _ := observed(ws, "rev-parse", "HEAD")
	if parent == first || source != first {
		t.Fatalf("private rewrite did not preserve source: parent=%s source=%s original=%s", parent, source, first)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("journal not retired: %v, %v", pending, err)
	}
	boundary, err := isolatedSourceBoundary(repo, identity, parent)
	if err != nil || boundary != first {
		t.Fatalf("next source boundary: %s, %v", boundary, err)
	}
	if ForkUnmerged(repo, ws) {
		t.Fatal("fully published privately rebased source falsely considered unmerged")
	}
	second := isolatedTestCommit(t, ws, "second.txt", "second\n")
	if !ForkUnmerged(repo, ws) {
		t.Fatal("new unpublished source work considered merged")
	}
	result2, err := c.mergeOne(repo, "", "isolated", false)
	defer result2.approval.close()
	if err != nil || !result2.landed {
		t.Fatalf("second land: %+v, %v", result2, err)
	}
	count, _ := observed(repo, "rev-list", "--count", parent+"..HEAD")
	if count != "1" {
		t.Fatalf("kept fork replayed already-landed source commits: %s", count)
	}
	source, _ = observed(ws, "rev-parse", "HEAD")
	if source != second {
		t.Fatal("second publication changed native model HEAD")
	}
	for _, path := range []string{"first.txt", "second.txt", "parent.txt"} {
		if _, err := os.Stat(filepath.Join(repo, path)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsolatedUnmergedRequiresCurrentPublicationEvidence(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	base, _ := observed(repo, "rev-parse", "HEAD")
	if ForkUnmerged(repo, ws) {
		t.Fatal("untouched isolated fork falsely considered unmerged")
	}
	isolatedTestCommit(t, ws, "source.txt", "source\n")
	isolatedTestCommit(t, repo, "parent.txt", "parent\n")
	result, err := c.mergeOne(repo, "", identity.Name, false)
	defer result.approval.close()
	if err != nil || !result.landed {
		t.Fatal("receipt fixture did not land", err)
	}
	var record isolatedSourceRecord
	if ok, err := readIsolatedRecord(isolatedSourcePath(repo, identity), &record); err != nil || !ok {
		t.Fatal("receipt missing", err)
	}
	if ForkUnmerged(repo, ws) {
		t.Fatal("completed receipt did not qualify publication")
	}
	isolatedTestCommit(t, repo, "later.txt", "later\n")
	if ForkUnmerged(repo, ws) {
		t.Fatal("publication ceased to qualify after unrelated parent advance")
	}
	parent, _ := observed(repo, "rev-parse", "HEAD")
	git(t, repo, "update-ref", "refs/heads/main", base, parent)
	if !ForkUnmerged(repo, ws) {
		t.Fatal("receipt qualified publication removed from current parent ancestry")
	}
	git(t, repo, "update-ref", "refs/heads/main", parent, base)
	for _, bad := range []isolatedSourceRecord{
		{Version: 1, Fork: identity, Head: record.Head, Tree: record.Tree},
		{Version: 2, Fork: identity, Head: record.Head, Tree: record.Tree, PublicationHead: record.PublicationHead, PublicationTree: record.Head},
		{Version: 2, Fork: forkspace.Identity{Name: identity.Name, Generation: forkspace.Generation("other")}, Head: record.Head, Tree: record.Tree, PublicationHead: record.PublicationHead, PublicationTree: record.PublicationTree},
	} {
		if _, err := writeIsolatedRecord(isolatedSourcePath(repo, identity), bad); err != nil {
			t.Fatal(err)
		}
		if !ForkUnmerged(repo, ws) {
			t.Fatalf("invalid/old receipt granted removal authority: %+v", bad)
		}
	}
	if _, err := writeIsolatedRecord(isolatedSourcePath(repo, identity), record); err != nil {
		t.Fatal(err)
	}
	if _, err := writeIsolatedRecord(landIntentPath(repo, identity), map[string]string{"phase": "started"}); err != nil {
		t.Fatal(err)
	}
	if !ForkUnmerged(repo, ws) {
		t.Fatal("malformed pending journal bypassed completion barrier")
	}
}

func TestIsolatedCutoffRefusesRemovedPublicationBeforeAnyEffect(t *testing.T) {
	for _, newWork := range []bool{false, true} {
		t.Run(fmt.Sprint("newWork=", newWork), func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, "first.txt", "first\n")
			previousParent := isolatedTestCommit(t, repo, "parent.txt", "parent\n")
			result, err := c.mergeOne(repo, "", identity.Name, false)
			defer result.approval.close()
			if err != nil || !result.landed {
				t.Fatal("first publication", err)
			}
			if newWork {
				isolatedTestCommit(t, ws, "second.txt", "second\n")
			}
			// This test owns the disposable parent; simulate an operator removing P1.
			git(t, repo, "reset", "--hard", "-q", previousParent)
			before, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal(err)
			}
			source, _ := observed(ws, "rev-parse", "HEAD")
			result, err = c.mergeOne(repo, "", identity.Name, false)
			defer result.approval.close()
			if err == nil || result.landed {
				t.Fatalf("removed publication silently became a cutoff: %+v %v", result, err)
			}
			after, err := captureIsolatedParent(repo)
			if err != nil || !sameIsolatedParent(before, after) {
				t.Fatal("cutoff refusal changed parent", err)
			}
			if got, _ := observed(ws, "rev-parse", "HEAD"); got != source {
				t.Fatal("cutoff refusal rewrote native source")
			}
			if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
				t.Fatal("cutoff refusal started publication", err)
			}
		})
	}
}

func TestIsolatedParentSnapshotBindsActualTrackedBytes(t *testing.T) {
	for _, lfs := range []bool{false, true} {
		t.Run(fmt.Sprint("lfs=", lfs), func(t *testing.T) {
			repo := initRepo(t)
			path := "native.txt"
			original, converted := []byte("native\n"), []byte("native\r\n")
			if lfs {
				path = "data.bin"
				converted = []byte("native payload\n")
				writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
				original = isolatedTestLFSFile(t, repo, converted)
			} else {
				writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.txt text eol=crlf\n")
				writeTaskFile(t, filepath.Join(repo, path), string(original))
			}
			git(t, repo, "add", ".gitattributes", path)
			git(t, repo, "commit", "-qm", "native byte representations")
			before, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, path), converted, 0o755); err != nil {
				t.Fatal(err)
			}
			if !lfs {
				if err := os.Chmod(filepath.Join(repo, path), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			after, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal("valid native representation refused", err)
			}
			if sameIsolatedParent(before, after) {
				t.Fatal("different tracked bytes granted the same publication precondition")
			}
			if err := os.WriteFile(filepath.Join(repo, path), original, 0o644); err != nil {
				t.Fatal(err)
			}
			restored, err := captureIsolatedParent(repo)
			if err != nil || !sameIsolatedParent(before, restored) {
				t.Fatal("exact original bytes did not restore the same semantic snapshot", err)
			}
		})
	}
}

func TestIsolatedPublicationBindsCheckedBytesAcrossCASAndReplay(t *testing.T) {
	for _, phase := range []string{"confirmation", "started", "landed"} {
		t.Run(phase, func(t *testing.T) {
			repo := initRepo(t)
			writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.txt text eol=crlf\n")
			git(t, repo, "add", ".gitattributes")
			git(t, repo, "commit", "-qm", "trusted conversion")
			repo, ws, identity, c := isolatedTestForkFromRepo(t, repo)
			isolatedTestCommit(t, ws, "new.txt", "incoming\n")
			c.afterLandFastForward = func() error {
				writeTaskFile(t, filepath.Join(repo, "new.txt"), "incoming\n")
				if phase != "confirmation" {
					return errors.New("injected interruption after external representation change")
				}
				return nil
			}
			result, err := c.mergeOne(repo, "", identity.Name, false)
			defer result.approval.close()
			if phase != "confirmation" {
				if err == nil || !result.landed {
					t.Fatal("post-CAS interruption fixture failed", err)
				}
				if phase == "landed" {
					intent, pending, err := readIsolatedIntent(repo, identity)
					if err != nil || !pending {
						t.Fatal("missing replay journal", err)
					}
					intent.Phase = "landed"
					if _, err := writeIsolatedRecord(landIntentPath(repo, identity), intent); err != nil {
						t.Fatal(err)
					}
				}
				c.afterLandFastForward = nil
				result, err = c.mergeOne(repo, "", identity.Name, false)
				defer result.approval.close()
			}
			if err == nil {
				t.Fatal("changed physical bytes accepted as coherent publication")
			}
			if _, pending, err := readIsolatedIntent(repo, identity); err != nil || !pending {
				t.Fatal("changed publication lost its recovery journal", err)
			}
			if got, err := os.ReadFile(filepath.Join(repo, "new.txt")); err != nil || string(got) != "incoming\n" {
				t.Fatal("refusal reset the external tracked bytes", err)
			}
		})
	}
}

func TestIsolatedNativeConversionUsesOnlySafeEffectiveHostSettings(t *testing.T) {
	for _, scope := range []string{"global", "system", "local-override"} {
		t.Run(scope, func(t *testing.T) {
			repo := initRepo(t)
			configScope := "GLOBAL"
			if scope == "system" {
				configScope = "SYSTEM"
			}
			git(t, repo, "config", "--file", os.Getenv("GIT_CONFIG_"+configScope), "core.autocrlf", "true")
			expected := "new\r\n"
			if scope == "local-override" {
				git(t, repo, "config", "core.autocrlf", "false")
				expected = "new\n"
			}
			writeTaskFile(t, filepath.Join(repo, "existing.txt"), strings.ReplaceAll(expected, "new", "existing"))
			git(t, repo, "add", "existing.txt")
			git(t, repo, "commit", "-qm", "effective native EOL baseline")
			repo, ws, _, c := isolatedTestForkFromRepo(t, repo)
			isolatedTestCommit(t, ws, "new.txt", "new\n")
			result, err := c.mergeOne(repo, "", "isolated", false)
			defer result.approval.close()
			if err != nil || !result.landed {
				t.Fatalf("native %s conversion refused: %+v %v", scope, result, err)
			}
			got, err := os.ReadFile(filepath.Join(repo, "new.txt"))
			if err != nil || string(got) != expected {
				t.Fatalf("native local precedence lost: %q want %q %v", got, expected, err)
			}
		})
	}
}

func TestIsolatedPrivateRebaseFailureGivesSafeSourceGuidance(t *testing.T) {
	for _, signing := range []bool{false, true} {
		t.Run(fmt.Sprint("signing=", signing), func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			boundary, err := forkspace.IsolatedCreationBase(repo, identity)
			if err != nil {
				t.Fatal(err)
			}
			if signing {
				isolatedTestCommit(t, ws, "new.txt", "incoming\n")
				isolatedTestCommit(t, repo, "parent.txt", "parent\n")
				git(t, repo, "config", "--global", "commit.gpgsign", "true")
				git(t, repo, "config", "--global", "gpg.program", "false")
			} else {
				isolatedTestCommit(t, ws, "README.md", "incoming\n")
				isolatedTestCommit(t, repo, "README.md", "parent\n")
			}
			before, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.mergeOne(repo, "", identity.Name, false)
			if err == nil || result.landed {
				t.Fatal("private failure fixture did not refuse", err)
			}
			for _, text := range []string{"private rebase failed", ws, boundary, "coop fork review isolated", "trusted signing settings"} {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("private failure missing safe guidance %q: %v", text, err)
				}
			}
			if strings.Contains(err.Error(), "isolated review conflicts against") {
				t.Fatal("every private failure mislabeled as a conflict", err)
			}
			after, captureErr := captureIsolatedParent(repo)
			if captureErr != nil || !sameIsolatedParent(before, after) {
				t.Fatal("private rebase failure changed parent", captureErr)
			}
		})
	}
}

func TestIsolatedPublicationPreservesNativeCRLFCheckout(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint("existing=", existing), func(t *testing.T) {
			repo := initRepo(t)
			writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.txt text eol=crlf\n")
			git(t, repo, "add", ".gitattributes")
			if existing {
				writeTaskFile(t, filepath.Join(repo, "existing.txt"), "existing\r\n")
				git(t, repo, "add", "existing.txt")
			}
			git(t, repo, "commit", "-qm", "trusted native EOL baseline")
			repo, ws, _, c := isolatedTestForkFromRepo(t, repo)
			isolatedTestCommit(t, ws, "new.txt", "new\r\n")
			result, err := c.mergeOne(repo, "", "isolated", false)
			defer result.approval.close()
			if err != nil || !result.landed {
				t.Fatalf("native CRLF publication: %+v %v", result, err)
			}
			got, err := os.ReadFile(filepath.Join(repo, "new.txt"))
			if err != nil || string(got) != "new\r\n" {
				t.Fatalf("native CRLF checkout changed: %q %v", got, err)
			}
			writeTaskFile(t, filepath.Join(repo, "new.txt"), "dirty\r\n")
			if _, err := captureIsolatedParent(repo); err == nil {
				t.Fatal("dirty CRLF content passed semantic capture")
			}
		})
	}
}

func TestIsolatedPublicationCancellationKeepsParentUnchanged(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	source := isolatedTestCommit(t, ws, "new.txt", "cancelled candidate\n")
	before, err := captureIsolatedParent(repo)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := observed(repo, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		t.Fatal(err)
	}
	snapshots := map[string][]byte{}
	for _, name := range []string{"HEAD", "index", "config"} {
		path := filepath.Join(repo, ".git", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		snapshots[path] = data
	}
	confirmations := 0
	result, err := c.mergeIsolatedLocked(repo, "", identity.Name, identity, false, false, func(candidate isolatedCandidate) bool {
		confirmations++
		if candidate.sourceHead != source || candidate.parent.Head != before.Head {
			t.Fatal("confirmation was not bound to exact source/base")
		}
		return false
	})
	if err != nil || result.landed || !result.skipped || confirmations != 1 {
		t.Fatal("cancelled publication did not stop", result, confirmations, err)
	}
	after, err := captureIsolatedParent(repo)
	if err != nil || !sameIsolatedParent(before, after) {
		t.Fatal("cancelled publication changed semantic parent", err)
	}
	for path, before := range snapshots {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("cancelled publication changed parent metadata", path, err)
		}
	}
	if after, err := observed(repo, "for-each-ref", "--format=%(refname) %(objectname)"); err != nil || after != refs {
		t.Fatal("cancelled publication changed parent refs", after, err)
	}
	if _, err := observed(repo, "cat-file", "-e", source); err == nil {
		t.Fatal("cancelled publication imported source objects")
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatal("cancelled publication created a journal", pending, err)
	}
	if head, err := observed(ws, "rev-parse", "HEAD"); err != nil || head != source {
		t.Fatal("cancelled publication changed source", head, err)
	}
}

func TestIsolatedPublicationRejectsGateMutationAndConcurrentParent(t *testing.T) {
	for _, mutateParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "gate", true: "parent"}[mutateParent], func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			source := isolatedTestCommit(t, ws, "new.txt", "candidate\n")
			before, _ := captureIsolatedParent(repo)
			c.gateOK = func(_, gateDir string, _ string) bool {
				if gateDir == ws {
					t.Fatal("gate received retained model workspace")
				}
				if mutateParent {
					isolatedTestCommit(t, repo, "outside.txt", "trusted concurrent work\n")
				} else {
					writeTaskFile(t, filepath.Join(gateDir, "new.txt"), "gate mutation\n")
				}
				base, err := observed(gateDir, "rev-parse", "refs/coop/session-parent")
				if err != nil || base != before.Head {
					t.Fatal("gate did not retain its captured parent base", base, err)
				}
				return true
			}
			result, err := c.mergeOne(repo, "fixture-image", "isolated", false)
			if err == nil || result.landed {
				t.Fatalf("mutation accepted: %+v, %v", result, err)
			}
			if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
				t.Fatalf("rejected review wrote journal: %v, %v", pending, err)
			}
			after, _ := captureIsolatedParent(repo)
			if !mutateParent && !sameIsolatedParent(before, after) {
				t.Fatal("rejected gate changed parent")
			}
			if mutateParent {
				if text, _ := os.ReadFile(filepath.Join(repo, "outside.txt")); string(text) != "trusted concurrent work\n" {
					t.Fatal("concurrent parent work was reset")
				}
			}
			got, _ := observed(ws, "rev-parse", "HEAD")
			if got != source {
				t.Fatal("rejected publication changed model source")
			}
		})
	}
}

func TestIsolatedReviewToolsUseRetainedCommittedCustody(t *testing.T) {
	for _, editor := range []bool{false, true} {
		t.Run(fmt.Sprint("editor=", editor), func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			base, _ := observed(repo, "rev-parse", "HEAD")
			source := isolatedTestCommit(t, ws, "safe.txt", "safe\n")
			writeTaskFile(t, filepath.Join(ws, "uncommitted.txt"), "must not enter custody\n")
			capture := filepath.Join(t.TempDir(), "preview-path")
			comparison := filepath.Join(t.TempDir(), "comparison-path")
			diff := filepath.Join(t.TempDir(), "comparison-diff")
			if editor {
				tool := filepath.Join(t.TempDir(), "editor")
				writeTaskFile(t, tool, "#!/bin/sh\nprintf '%s' \"$1\" > '"+capture+"'\n")
				if err := os.Chmod(tool, 0o700); err != nil {
					t.Fatal(err)
				}
				c.cfg.Editor = tool
			} else {
				c.cfg.ReviewCmd = "printf '%s' \"$COOP_FORK_PATH\" > '" + capture + "'; " +
					"pwd > '" + comparison + "'; git diff --name-only HEAD...\"$COOP_REVIEW_REF\" > '" + diff + "'"
			}
			out := captureStdout(t, func() {
				code, err := c.forkReviewIsolated(repo, ws, identity.Name, identity, false, false, editor, false)
				if code != 0 || err != nil {
					t.Fatalf("review tool: %d, %v", code, err)
				}
			})
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			preview := string(data)
			if preview == ws || filepath.Dir(preview) != forkspace.StateDir(repo) || !strings.Contains(out, "Into: main") {
				t.Fatal("tool/dossier did not use private captured context", preview, out)
			}
			if _, err := os.Stat(preview); err != nil {
				t.Fatal("GUI preview removed before tool completion", err)
			}
			content, err := os.ReadFile(filepath.Join(preview, "safe.txt"))
			if err != nil || string(content) != "safe\n" {
				t.Fatal("tool preview does not contain exact proposed files", string(content), err)
			}
			if head, err := observed(preview, "rev-parse", "HEAD"); err != nil || head != source {
				t.Fatal("tool preview does not retain exact publication HEAD", head, source, err)
			}
			if _, err := os.Stat(filepath.Join(preview, "uncommitted.txt")); !os.IsNotExist(err) {
				t.Fatal("raw source entered host preview")
			}
			if err := forkspace.ValidateIndependentGit(preview); err != nil {
				t.Fatal(err)
			}
			if !editor {
				data, err := os.ReadFile(comparison)
				if err != nil {
					t.Fatal(err)
				}
				cwd := strings.TrimSpace(string(data))
				stateDir, err := filepath.EvalSymlinks(forkspace.StateDir(repo))
				if err != nil {
					t.Fatal(err)
				}
				if cwd == preview || cwd == repo || cwd == ws || filepath.Dir(cwd) != stateDir {
					t.Fatal("custom tool did not retain a separate private comparison", cwd)
				}
				if head, err := observed(cwd, "rev-parse", "HEAD"); err != nil || head != base {
					t.Fatal("custom comparison does not retain captured parent HEAD", head, base, err)
				}
				if err := forkspace.ValidateIndependentGit(cwd); err != nil {
					t.Fatal(err)
				}
				data, err = os.ReadFile(diff)
				if err != nil || string(data) != "safe.txt\n" {
					t.Fatal("HEAD...COOP_REVIEW_REF comparison changed", string(data), err)
				}
			}
		})
	}
}

func TestIsolatedCommittedPreviewMaterializesAndCleansFailure(t *testing.T) {
	repo := initRepo(t)
	leaf := initRepo(t)
	git(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", leaf, "leaf")
	writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
	git(t, repo, "add", ".gitattributes")
	git(t, repo, "commit", "-qam", "trusted dependencies")
	repo, ws, identity, _ := isolatedTestForkFromRepo(t, repo)
	payload := []byte("new committed preview payload\n")
	isolatedTestLFSFile(t, ws, payload)
	git(t, ws, "add", "data.bin")
	git(t, ws, "commit", "-qm", "new payload")
	candidate, err := captureIsolatedCandidate(repo, identity.Name, identity, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.cleanup()
	preview, err := candidate.committedPreview(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(preview)
	content, err := os.ReadFile(filepath.Join(preview, "data.bin"))
	if err != nil || !bytes.Equal(content, payload) {
		t.Fatal("custom preview lost verified LFS payload", string(content), err)
	}
	if err := forkspace.ValidateIndependentGit(filepath.Join(preview, "leaf")); err != nil {
		t.Fatal("custom preview lost independent native child", err)
	}
	before, err := os.ReadDir(forkspace.StateDir(repo))
	if err != nil {
		t.Fatal(err)
	}
	invalid := candidate
	invalid.head = "not-a-commit"
	if path, err := invalid.committedPreview(repo); err == nil || path != "" {
		t.Fatal("invalid preview did not fail", path, err)
	}
	after, err := os.ReadDir(forkspace.StateDir(repo))
	if err != nil || len(after) != len(before) {
		t.Fatal("failed preview leaked custody", len(before), len(after), err)
	}
}

func TestIsolatedLandedReplayKeepsJournalForPointerOnlyLFS(t *testing.T) {
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
	git(t, repo, "add", ".gitattributes")
	git(t, repo, "commit", "-qm", "trusted LFS attributes")
	repo, ws, identity, c := isolatedTestForkFromRepo(t, repo)
	// The existing trusted attributes qualify a new payload without filter execution.
	pointer := isolatedTestLFSFile(t, ws, []byte("verified native payload\n"))
	git(t, ws, "add", "data.bin")
	git(t, ws, "commit", "-qm", "new LFS payload")
	head, _ := observed(ws, "rev-parse", "HEAD")
	if err := forkspace.HydrateLFS(t.Context(), ws, head); err != nil {
		t.Fatal(err)
	}
	c.afterLandFastForward = func() error { return errors.New("post-CAS pause") }
	result, err := c.mergeOne(repo, "", identity.Name, false)
	if err == nil || !result.landed {
		t.Fatal("fixture did not pause after landing", err)
	}
	c.afterLandFastForward = nil
	if err := os.WriteFile(filepath.Join(repo, "data.bin"), pointer, 0755); err != nil {
		t.Fatal(err)
	}
	result, err = c.mergeOne(repo, "", identity.Name, false)
	if err == nil {
		t.Fatal("pointer-only published checkout finalized")
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || !pending {
		t.Fatal("incomplete landed payload lost journal", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "data.bin"))
	if err != nil || !bytes.Equal(data, pointer) {
		t.Fatal("replay overwrote changed owner payload", err)
	}
}

func TestIsolatedPublicationRejectsIntermediateAutomaticSurfaceAndForce(t *testing.T) {
	repo, ws, _, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, ".gitattributes", "README.md filter=attack\n")
	git(t, ws, "rm", ".gitattributes")
	git(t, ws, "commit", "-qm", "remove evidence")
	before, _ := captureIsolatedParent(repo)
	for _, force := range []bool{false, true} {
		result, err := c.mergeOne(repo, "", "isolated", force)
		if err == nil || result.landed {
			t.Fatalf("unsafe intermediate history accepted (force=%v): %v", force, err)
		}
		after, _ := captureIsolatedParent(repo)
		if !sameIsolatedParent(before, after) {
			t.Fatal("policy rejection changed parent")
		}
	}
}

func TestIsolatedPublicationRetainsAllImmutableRiskPolicy(t *testing.T) {
	for _, tc := range []struct{ name, path, content string }{
		{"lifecycle", "package.json", `{"scripts":{"postinstall":"echo planted"}}`},
		{"malformed-package", "package.json", `{"scripts":`},
		{"secret-name", "credentials.json", `{}`},
		{"secret-content", "ordinary.txt", "github_token = ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij\n"},
		{"large-file", "oversized.bin", strings.Repeat("x", (5<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws, _, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, tc.path, tc.content)
			git(t, ws, "rm", tc.path)
			git(t, ws, "commit", "-qm", "remove risky terminal diff")
			before, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal(err)
			}
			for _, force := range []bool{false, true} {
				result, err := c.mergeOne(repo, "", "isolated", force)
				if err == nil || result.landed {
					t.Fatalf("risk policy bypassed (force=%v): %v", force, err)
				}
				after, err := captureIsolatedParent(repo)
				if err != nil || !sameIsolatedParent(before, after) {
					t.Fatal("risk refusal changed parent", err)
				}
			}
		})
	}
}

func TestIsolatedDirtyParentAllowsCommittedReviewButNotPublication(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "safe.txt", "committed review\n")
	file := filepath.Join(repo, "dirty.txt")
	writeTaskFile(t, file, "staged owner content\n")
	git(t, repo, "add", "dirty.txt")
	writeTaskFile(t, file, "unstaged owner content\n")
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	code, err := c.forkReviewIsolated(repo, ws, "isolated", identity, true, false, false, false)
	if err != nil || code != 0 {
		t.Fatalf("read-only review rejected shared owner work: %d, %v", code, err)
	}
	result, err := c.mergeOne(repo, "", "isolated", false)
	if err == nil || result.landed {
		t.Fatal("publication admitted dirty parent")
	}
	for path, expected := range map[string][]byte{
		filepath.Join(repo, ".git", "index"): index,
		filepath.Join(repo, ".git", "HEAD"):  head,
		file:                                 []byte("unstaged owner content\n"),
	} {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatal("review/refusal changed owner bytes", path, err)
		}
	}
}

func TestIsolatedPublicationReplaysCoherentLandWithoutNewGateOrApproval(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	source := isolatedTestCommit(t, ws, "new.txt", "published\n")
	c.afterLandFastForward = func() error { return errors.New("injected post-ref interruption") }
	result, err := c.mergeOne(repo, "", "isolated", false)
	if err == nil || !result.landed {
		t.Fatalf("crash boundary did not land: %+v, %v", result, err)
	}
	intent, pending, err := readIsolatedIntent(repo, identity)
	if err != nil || !pending {
		t.Fatalf("no exact forward journal: %v, %v", pending, err)
	}
	if intent.SourceHead != source || intent.Version != 2 {
		t.Fatal("journal lost original source authority")
	}
	c.afterLandFastForward = nil
	c.host.EnsureRuntime = func() (runtime.Runtime, error) { t.Fatal("replay resolved a new gate"); return runtime.Runtime{}, nil }
	// New publication in a pipe would require --yes; already-started finalization does not.
	code, err := c.ForkMerge([]string{"isolated"})
	if err != nil || code != 0 {
		t.Fatalf("coherent replay: %d, %v", code, err)
	}
}

func TestIsolatedPublicationNoEffectBindsOriginalBytes(t *testing.T) {
	repo, _, identity, c := isolatedTestFork(t)
	before, err := captureIsolatedParent(repo)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.mergeOne(repo, "", identity.Name, false)
	defer result.approval.close()
	if err != nil || !result.landed {
		t.Fatalf("exact no-effect publication refused: %+v %v", result, err)
	}
	after, err := captureIsolatedParent(repo)
	if err != nil || !sameIsolatedParent(before, after) {
		t.Fatal("no-effect publication changed parent snapshot", err)
	}
}

func TestIsolatedPublicationNoNewCommitsHydratesNativeLFSPointers(t *testing.T) {
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
	payload := []byte("native pointer-only parent payload\n")
	pointer := isolatedTestLFSFile(t, repo, payload)
	git(t, repo, "add", ".gitattributes", "data.bin")
	git(t, repo, "commit", "-qm", "native LFS baseline")
	repo, _, identity, c := isolatedTestForkFromRepo(t, repo)
	if got, err := os.ReadFile(filepath.Join(repo, "data.bin")); err != nil || !bytes.Equal(got, pointer) {
		t.Fatal("no-new-commits pointer fixture was already hydrated", err)
	}
	before, err := captureIsolatedParent(repo)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.mergeOne(repo, "", identity.Name, false)
	defer result.approval.close()
	if err != nil || !result.landed {
		t.Fatalf("pointer-only no-new-commits publication stuck: %+v %v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "data.bin")); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("native no-new-commits publication did not hydrate verified payload", err)
	}
	after, err := captureIsolatedParent(repo)
	if err != nil || after.Head != before.Head || after.Tree != before.Tree || after.Index != before.Index {
		t.Fatal("hydration changed logical no-new-commits publication", err)
	}
	if _, pending, err := readIsolatedIntent(repo, identity); err != nil || pending {
		t.Fatal("completed native hydration left recovery journal", err)
	}
}

func TestIsolatedParentSnapshotBindsRecursiveChildBytes(t *testing.T) {
	repo, leaf := initRepo(t), initRepo(t)
	writeTaskFile(t, filepath.Join(leaf, ".gitattributes"), "*.txt text eol=crlf\n")
	writeTaskFile(t, filepath.Join(leaf, "native.txt"), "child\n")
	git(t, leaf, "add", ".gitattributes", "native.txt")
	git(t, leaf, "commit", "-qm", "child native representations")
	git(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", leaf, "leaf")
	git(t, repo, "commit", "-qam", "trusted child")
	before, err := captureIsolatedParent(repo)
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(repo, "leaf", "native.txt"), "child\n")
	after, err := captureIsolatedParent(repo)
	if err != nil || sameIsolatedParent(before, after) {
		t.Fatal("recursive valid raw-byte change lost parent publication precondition", err)
	}
}

func TestIsolatedPublicationHydratedLFSNoEffectBindsOriginalBytes(t *testing.T) {
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
	payload := []byte("already hydrated native payload\n")
	isolatedTestLFSFile(t, repo, payload)
	git(t, repo, "add", ".gitattributes", "data.bin")
	git(t, repo, "commit", "-qm", "native LFS baseline")
	if err := os.WriteFile(filepath.Join(repo, "data.bin"), payload, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, _, identity, c := isolatedTestForkFromRepo(t, repo)
	before, err := captureIsolatedParent(repo)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.mergeOne(repo, "", identity.Name, false)
	defer result.approval.close()
	if err != nil || !result.landed {
		t.Fatalf("hydrated native no-effect publication refused: %+v %v", result, err)
	}
	after, err := captureIsolatedParent(repo)
	if err != nil || !sameIsolatedParent(before, after) {
		t.Fatal("hydrated native no-effect publication changed exact parent", err)
	}
}

func TestIsolatedPublicationMissingContentProofPreservesCustody(t *testing.T) {
	for _, missing := range []string{"parent", "publication-started", "publication-landed"} {
		t.Run(missing, func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, "new.txt", "published\n")
			c.afterLandFastForward = func() error { return errors.New("injected interruption") }
			result, err := c.mergeOne(repo, "", identity.Name, false)
			if err == nil || !result.landed {
				t.Fatal("post-CAS fixture failed", err)
			}
			intent, pending, err := readIsolatedIntent(repo, identity)
			if err != nil || !pending {
				t.Fatal("missing exact forward journal", err)
			}
			if missing == "parent" {
				intent.Parent.Content = ""
			} else {
				intent.PublicationContent = ""
				if missing == "publication-landed" {
					intent.Phase = "landed"
				}
			}
			if _, err := writeIsolatedRecord(landIntentPath(repo, identity), intent); err != nil {
				t.Fatal(err)
			}
			before, err := captureIsolatedParent(repo)
			if err != nil {
				t.Fatal(err)
			}
			c.afterLandFastForward = nil
			if _, err := c.mergeOne(repo, "", identity.Name, false); err == nil {
				t.Fatal("missing digest authority inferred from current representation")
			}
			after, err := captureIsolatedParent(repo)
			if err != nil || !sameIsolatedParent(before, after) {
				t.Fatal("missing proof recovery changed parent", err)
			}
			for _, path := range []string{landIntentPath(repo, identity), filepath.Join(forkspace.StateDir(repo), intent.Custody)} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatal("uncertain recovery discarded journal/custody", err)
				}
			}
		})
	}
}

func TestIsolatedPublicationRefusesPartialCheckoutAndUncertainLock(t *testing.T) {
	for _, lock := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "lock"}[lock], func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, "new.txt", "candidate\n")
			candidate, err := prepareIsolatedCandidate(repo, "isolated", identity, false)
			if err != nil {
				t.Fatal(err)
			}
			intent := isolatedLandIntent{Version: 2, Fork: identity, SourceHead: candidate.sourceHead,
				SourceTree: candidate.sourceTree, Upstream: candidate.upstream, Parent: candidate.parent,
				PublicationHead: candidate.head, PublicationTree: candidate.tree,
				Custody: filepath.Base(candidate.dir), Phase: "started", CreatedAt: time.Now().UTC()}
			if _, err := writeIsolatedRecord(landIntentPath(repo, identity), intent); err != nil {
				t.Fatal(err)
			}
			if lock {
				writeTaskFile(t, filepath.Join(repo, ".git", "index.lock"), "uncertain foreign lock\n")
			} else {
				if err := forkspace.ImportIsolatedObjects(context.Background(), candidate.dir, repo, candidate.head); err != nil {
					t.Fatal(err)
				}
				if err := forkspace.CheckoutIsolatedTree(context.Background(), repo, candidate.parent.Head, candidate.head); err != nil {
					t.Fatal(err)
				}
			}
			index, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))
			result, err := c.mergeOne(repo, "", "isolated", false)
			if err == nil || result.landed {
				t.Fatalf("ambiguous replay accepted: %+v, %v", result, err)
			}
			after, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))
			if string(index) != string(after) {
				t.Fatal("ambiguous replay reset index")
			}
			if pending, err := ForkHasPendingLand(repo, identity); err != nil || !pending {
				t.Fatalf("ambiguous journal discarded: %v, %v", pending, err)
			}
			if lock {
				if body, _ := os.ReadFile(filepath.Join(repo, ".git", "index.lock")); !strings.Contains(string(body), "foreign") {
					t.Fatal("recovery deleted or replaced foreign lock")
				}
			}
		})
	}
}

func TestIsolatedParentRejectsFlagsDirtyModesAndIntroducedIgnoredPaths(t *testing.T) {
	for _, kind := range []string{"assume", "skip", "symlink", "mode", "content", "ignored"} {
		t.Run(kind, func(t *testing.T) {
			repo, ws, _, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, "new.txt", "candidate\n")
			switch kind {
			case "assume":
				git(t, repo, "update-index", "--assume-unchanged", "README.md")
			case "skip":
				git(t, repo, "update-index", "--skip-worktree", "README.md")
			case "symlink":
				if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("new.txt", filepath.Join(repo, "README.md")); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(repo, "README.md"), 0755); err != nil {
					t.Fatal(err)
				}
			case "content":
				writeTaskFile(t, filepath.Join(repo, "README.md"), "changed\n")
			case "ignored":
				writeTaskFile(t, filepath.Join(repo, ".git", "info", "exclude"), "new.txt\n")
				writeTaskFile(t, filepath.Join(repo, "new.txt"), "outside ignored work\n")
			}
			result, err := c.mergeOne(repo, "", "isolated", false)
			if err == nil || result.landed {
				t.Fatalf("unsafe parent %s accepted: %v", kind, err)
			}
			if kind == "ignored" {
				if body, _ := os.ReadFile(filepath.Join(repo, "new.txt")); string(body) != "outside ignored work\n" {
					t.Fatal("ignored file overwritten")
				}
			}
		})
	}
}
