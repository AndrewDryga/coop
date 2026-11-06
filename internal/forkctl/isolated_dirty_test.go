package forkctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A checkout that differs from its last commit is refused in the shared block: what differs, the exact
// command that clears it, and then merge again. Before, each of these was one line in the code's own
// words ("isolated land refuses staged changes or an unmerged index").
func TestIsolatedDirtyParentRefusalsAreOneBlockThatNamesTheFilesAndTheCommand(t *testing.T) {
	const commit, stash = dirtyCommit, `git stash push -m "before coop fork merge"`
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, repo string)
		want   []string // each must be in the block
		absent []string // none may be
	}{
		{name: "staged new file", setup: func(t *testing.T, repo string) {
			writeTaskFile(t, filepath.Join(repo, "dirty.txt"), "staged\n")
			git(t, repo, "add", "dirty.txt")
		}, want: []string{"Your project has uncommitted changes", "A merge needs your checkout to match its last commit.",
			`Staged: "dirty.txt"`, "Commit them:", commit, "Or set them aside:", stash, "Then merge again."},
			absent: []string{"Changed:"}},
		{name: "staged then edited", setup: func(t *testing.T, repo string) {
			writeTaskFile(t, filepath.Join(repo, "dirty.txt"), "staged\n")
			git(t, repo, "add", "dirty.txt")
			writeTaskFile(t, filepath.Join(repo, "dirty.txt"), "edited after\n")
		}, want: []string{"Your project has uncommitted changes", `Staged: "dirty.txt"`, `Changed: "dirty.txt"`, commit, stash}},
		{name: "edited tracked file", setup: func(t *testing.T, repo string) {
			writeTaskFile(t, filepath.Join(repo, "README.md"), "edited\n")
		}, want: []string{"Your project has uncommitted changes", `Changed: "README.md"`, commit, stash, "Then merge again."},
			absent: []string{"Staged:"}},
		{name: "deleted tracked file", setup: func(t *testing.T, repo string) {
			if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"Your project has uncommitted changes", `Changed: "README.md"`, commit, stash}},
		{name: "file marked assume-unchanged", setup: func(t *testing.T, repo string) {
			git(t, repo, "update-index", "--assume-unchanged", "README.md")
		}, want: []string{"Some files in your project are hidden from Git's change checks", `Marked assume-unchanged: "README.md"`,
			"Undo assume-unchanged:", "git update-index --no-assume-unchanged -- README.md", "Then merge again."},
			absent: []string{"stash"}},
		{name: "file marked skip-worktree", setup: func(t *testing.T, repo string) {
			git(t, repo, "update-index", "--skip-worktree", "README.md")
		}, want: []string{"Some files in your project are hidden from Git's change checks", `Marked skip-worktree: "README.md"`,
			"Undo skip-worktree:", "git update-index --no-skip-worktree -- README.md"},
			absent: []string{"stash"}},
		{name: "unresolved merge conflict", setup: func(t *testing.T, repo string) {
			git(t, repo, "checkout", "-q", "-b", "other")
			writeTaskFile(t, filepath.Join(repo, "README.md"), "other side\n")
			git(t, repo, "commit", "-qam", "other side")
			git(t, repo, "checkout", "-q", "main")
			writeTaskFile(t, filepath.Join(repo, "README.md"), "main side\n")
			git(t, repo, "commit", "-qam", "main side")
			_ = exec.Command("git", "-C", repo, "merge", "-q", "other").Run() // stops on the conflict, as it should
		}, want: []string{"Your project has unresolved merge conflicts", `In conflict: "README.md"`,
			"Resolve them and commit, or abort the merge, rebase or cherry-pick in progress, then merge again."},
			absent: []string{"stash", "Commit them:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ws, identity, c := isolatedTestFork(t)
			isolatedTestCommit(t, ws, "new.txt", "from the fork\n")
			tc.setup(t, repo)
			before := parentFingerprint(t, repo)

			result, err := c.mergeOne(repo, "", identity.Name, false)
			assertDirtyBlock(t, err, result.landed, tc.want, tc.absent)
			if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
				t.Errorf("the refusal left a journal: %v, %v", pending, err)
			}
			if after := parentFingerprint(t, repo); after != before {
				t.Errorf("the refusal changed the parent")
			}
		})
	}
}

// A submodule that differs is named by its own folder, and the commands run there.
func TestIsolatedDirtySubmoduleRefusalNamesItsFolder(t *testing.T) {
	repo := initRepo(t)
	leaf := initRepo(t)
	git(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", leaf, "leaf")
	git(t, repo, "commit", "-qm", "add the submodule")
	repo, ws, identity, c := isolatedTestForkFromRepo(t, repo)
	isolatedTestCommit(t, ws, "new.txt", "from the fork\n")
	writeTaskFile(t, filepath.Join(repo, "leaf", "README.md"), "edited inside the submodule\n")
	child, err := filepath.EvalSymlinks(filepath.Join(repo, "leaf"))
	if err != nil {
		t.Fatal(err)
	}

	result, err := c.mergeOne(repo, "", identity.Name, false)
	assertDirtyBlock(t, err, result.landed, []string{
		"A submodule of your project has uncommitted changes", "This one is " + child + ".", `Changed: "README.md"`,
		"git -C " + child + ` commit -a -m "your message"`, "git -C " + child + ` stash push -m "before coop fork merge"`}, nil)
}

// A dirty checkout met while finishing an interrupted publication is part of that publication's own
// half-made state, so the refusal keeps the recovery text and never tells anyone to commit or stash.
func TestIsolatedDirtyParentAfterAnInterruptedPublicationKeepsTheRecoveryText(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "new.txt", "candidate\n")
	candidate, err := prepareIsolatedCandidate(repo, identity.Name, identity, false)
	if err != nil {
		t.Fatal(err)
	}
	intent := isolatedLandIntent{Version: 2, Fork: identity, SourceHead: candidate.sourceHead,
		SourceTree: candidate.sourceTree, Upstream: candidate.upstream, Parent: candidate.parent,
		PublicationHead: candidate.head, PublicationTree: candidate.tree,
		Custody: filepath.Base(candidate.dir), Phase: "started", CreatedAt: time.Now().UTC()}
	if err := writeIsolatedRecord(repo, landIntentPath(repo, identity), intent); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.ImportIsolatedObjects(context.Background(), candidate.dir, repo, candidate.head); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.CheckoutIsolatedTree(context.Background(), repo, candidate.parent.Head, candidate.head); err != nil {
		t.Fatal(err) // the checkout the interrupted publication had begun: new.txt is staged now
	}

	_, err = c.mergeOne(repo, "", identity.Name, false)
	var block *ui.UsageError
	if err == nil || errors.As(err, &block) {
		t.Fatalf("finishing over a half-made checkout = %v, want the recovery text and no block", err)
	}
	text := err.Error()
	for _, want := range []string{"custody/journal retained", "reconcile without resetting"} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, text)
		}
	}
	for _, advice := range []string{"stash", "commit -a", "Commit them"} {
		if strings.Contains(text, advice) {
			t.Errorf("the refusal tells a person to %q over a half-published checkout:\n%s", advice, text)
		}
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || !pending {
		t.Fatalf("the refusal dropped the journal: %v, %v", pending, err)
	}
}

// Changes made while the question is open are caught when the merge checks the parent again, in the same
// block, and leave nothing to finish.
func TestIsolatedDirtyParentFoundAfterTheQuestionIsRefusedInTheBlock(t *testing.T) {
	repo, ws, identity, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "new.txt", "from the fork\n")
	ask := func(isolatedCandidate) bool {
		writeTaskFile(t, filepath.Join(repo, "dirty.txt"), "staged while the question was open\n")
		git(t, repo, "add", "dirty.txt")
		return true
	}

	result, err := c.mergeIsolatedLocked(repo, "", identity.Name, identity, false, false, ask)
	assertDirtyBlock(t, err, result.landed, []string{"Your project has uncommitted changes", `Staged: "dirty.txt"`, dirtyCommit, "Then merge again."}, nil)
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Errorf("the refusal left a journal: %v, %v", pending, err)
	}
	if left := isolatedRecords(t, repo); len(left) != 0 {
		t.Errorf("the refusal kept private copies: %v", left)
	}
}

const dirtyCommit = `git commit -a -m "your message"`

// assertDirtyBlock checks that err is one shared block that exits 1, says every want and none of
// absent, and uses none of the code's own words for the refusal.
func assertDirtyBlock(t *testing.T, err error, landed bool, want, absent []string) {
	t.Helper()
	var block *ui.UsageError
	if err == nil || landed || !errors.As(err, &block) {
		t.Fatalf("the merge was not refused with a block: landed=%v, %v", landed, err)
	}
	if block.ExitCode != 1 || len(block.Rows) == 0 {
		t.Errorf("the refusal exits %d with %d action rows, want exit 1 and an action", block.ExitCode, len(block.Rows))
	}
	plain := block.Render(ui.Palette{})
	for _, text := range want {
		if !strings.Contains(plain, text) {
			t.Errorf("the refusal does not say %q:\n%s", text, plain)
		}
	}
	for _, text := range append(absent, "isolated land", "isolated parent", "unmerged index", "index flags", "exit status") {
		if strings.Contains(plain, text) {
			t.Errorf("the refusal says %q:\n%s", text, plain)
		}
	}
	if strings.Count(plain, "✗") != 1 {
		t.Errorf("the refusal is not one block:\n%s", plain)
	}
}
