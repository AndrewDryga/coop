package forkspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/fsidentity"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

func makeGenerationWorkspace(t *testing.T, repo, name string) {
	t.Helper()
	if err := os.MkdirAll(Workspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ensureTestGeneration(t *testing.T, repo, name string) Identity {
	t.Helper()
	unlock, err := LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	identity, err := EnsureGenerationLocked(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestForkGenerationIsStableAndFencesWorkspaceReplacement(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	first := ensureTestGeneration(t, repo, "perf")
	second := ensureTestGeneration(t, repo, "perf")
	if first != second {
		t.Fatalf("resume generation changed: %+v -> %+v", first, second)
	}
	if err := ValidateGenerationWorkspace(repo, first); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(Workspace(repo, "perf")); err != nil {
		t.Fatal(err)
	}
	makeGenerationWorkspace(t, repo, "perf")
	if err := ValidateGenerationWorkspace(repo, first); err == nil {
		t.Fatal("replacement workspace inherited the old generation")
	}

	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveGenerationIfMatchesLocked(repo, first); err != nil {
		unlock()
		t.Fatal(err)
	}
	replacement, err := EnsureGenerationLocked(repo, "perf")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if replacement.Generation == first.Generation {
		t.Fatal("reused fork name received the same generation")
	}
}

// A fork is bound to its record by a hard link from its folder, which cannot cross disks, so a fork
// whose folder is on another disk than Coop's private state is refused, naming both folders, and
// nothing is recorded.
func TestForkGenerationRefusesAProjectOnAnotherDisk(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	before := sameDisk
	sameDisk = func(string, string) (bool, error) { return false, nil }
	t.Cleanup(func() { sameDisk = before })
	unlock, err := LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = EnsureGenerationLocked(repo, "perf")
	if err == nil || !strings.Contains(err.Error(), Workspace(repo, "perf")) || !strings.Contains(err.Error(), StateDir(repo)) || !strings.Contains(err.Error(), "one disk") {
		t.Fatalf("fork on another disk = %v; want a refusal naming %s and %s", err, Workspace(repo, "perf"), StateDir(repo))
	}
	if _, err := os.Lstat(GenerationPath(repo, "perf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused fork left a generation record: %v", err)
	}
}

func TestForkGenerationRefusesNameWhilePriorSessionWorkspaceIsStaged(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "remote")
	identity := ensureTestGeneration(t, repo, "remote")
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: "remote_staged", CreatedAt: time.Now().UTC(),
	}
	handle, info, err := Pin(Workspace(repo, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	unlock, err := LockState(repo, "remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := ReserveWorkspaceLocked(repo, record); err != nil {
		unlock()
		t.Fatal(err)
	}
	if _, err := StageWorkspaceDiscardLocked(repo, "remote", info); err != nil {
		unlock()
		t.Fatal(err)
	}
	if err := RemoveGenerationIfMatchesLocked(repo, identity); err != nil {
		unlock()
		t.Fatal(err)
	}
	// Simulate a caller that bypassed Setup and recreated the public directory. The authority
	// boundary must still reject it while the prior session's staged bytes/name tombstone remain.
	makeGenerationWorkspace(t, repo, "remote")
	_, ensureErr := EnsureGenerationLocked(repo, "remote")
	unlock()
	if ensureErr == nil || !strings.Contains(ensureErr.Error(), "still being discarded") {
		t.Fatalf("replacement generation error = %v, want staged-discard refusal", ensureErr)
	}
	if _, present, err := ReadGeneration(repo, "remote"); err != nil || present {
		t.Fatalf("replacement generation after refusal: present=%v err=%v", present, err)
	}
}

func TestForkGenerationDoesNotPersistReusableAllocatorIdentity(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != forkGenerationVersion || record.WorkspaceAnchor == "" {
		t.Fatalf("generation record is not anchored: %+v", record)
	}
	data, err := os.ReadFile(GenerationPath(repo, identity.Name))
	if err != nil || strings.Contains(string(data), "workspace_inode") || strings.Contains(string(data), "workspace_device") {
		t.Fatalf("generation record retained reusable allocator identity: %s, %v", data, err)
	}
}

// Versions 1 and 2 (inode, and inode plus birth time) were converted by builds up to v12.1.0 when
// the stopped fork next started. This one refuses them by name, never reads one as no generation,
// and leaves the file as it is; fork stop and every read report the same refusal.
func TestOldForkGenerationRecordsAreRefusedByName(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			repo := committedSetupRepo(t)
			if _, err := Setup(repo, "old"); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"version":` + string(rune('0'+version)) + `,"name":"old","generation":"0123456789abcdef0123456789abcdef",` +
				`"workspace_device":1,"workspace_inode":2,"workspace_birth_sec":3,"created_at":"2026-01-02T03:04:05Z"}` + "\n")
			if err := EnsureStateDir(repo); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(GenerationPath(repo, "old"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			check := func(what string, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "generation record from Coop v10.1.2 or earlier") ||
					!strings.Contains(err.Error(), "v12.1.0") || !strings.Contains(err.Error(), "coop fork stop old") {
					t.Fatalf("%s = %v; want a refusal naming the version and the converting build", what, err)
				}
			}
			_, present, err := ReadGeneration(repo, "old")
			check("ReadGeneration", err)
			if present {
				t.Fatal("an old generation record read as present")
			}
			unlock, err := LockState(repo, "old")
			if err != nil {
				t.Fatal(err)
			}
			_, err = EnsureGenerationLocked(repo, "old")
			unlock()
			check("EnsureGenerationLocked", err)
			_, problems := GenerationIdentities(repo)
			if len(problems) != 1 {
				t.Fatalf("GenerationIdentities problems = %v; want the old record reported", problems)
			}
			check("ValidateGenerationWorkspace", ValidateGenerationWorkspace(repo, Identity{Name: "old", Generation: "0123456789abcdef0123456789abcdef"}))
			if after, err := os.ReadFile(GenerationPath(repo, "old")); err != nil || string(after) != string(body) {
				t.Fatalf("the old generation record changed: %q, %v", after, err)
			}
		})
	}
}

func TestForkGenerationAnchorKeepsLongValidNames(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	// This fits the atomic .generation-* writer but would overflow the v3 anchor if that private
	// filename repeated the human name.
	name := strings.Repeat("a", 230)
	if !ValidExistingName(name) {
		t.Fatal("long path-safe fork name was unexpectedly invalid")
	}
	makeGenerationWorkspace(t, repo, name)
	identity := ensureTestGeneration(t, repo, name)
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatalf("long-name generation was not usable: %v", err)
	}
	record, err := readGenerationRecord(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.WorkspaceAnchor) >= len(name) || strings.Contains(record.WorkspaceAnchor, name) {
		t.Fatalf("private anchor still embeds the human fork name: %q", record.WorkspaceAnchor)
	}
}

func TestForkGenerationMarkerStaysOutOfGitStatus(t *testing.T) {
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "clean")
	if err != nil {
		t.Fatal(err)
	}
	ensureTestGeneration(t, repo, "clean")
	status, err := gitOutputContext(context.Background(), ws, "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("anchored fork status = %q, %v; want clean", status, err)
	}
}

func TestForkGenerationNormalizesInterruptedSetupExclusions(t *testing.T) {
	repo := committedSetupRepo(t)
	name := "interrupted"
	ws := Workspace(repo, name)
	if err := os.MkdirAll(Home(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := GitClone(repo, ws); err != nil {
		t.Fatal(err)
	}
	if err := gitCheckoutNewBranchContext(context.Background(), ws, name); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".coop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".coop", "partial"), []byte("state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ensureTestGeneration(t, repo, name)
	ensureTestGeneration(t, repo, name) // idempotent retry must not duplicate either rule
	status, err := gitOutputContext(context.Background(), ws, "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("adopted interrupted workspace status = %q, %v; want clean", status, err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{".coop/", "/" + GenerationMarkerName} {
		count := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("exclude pattern %q count = %d, want 1:\n%s", pattern, count, data)
		}
	}
}

func TestForkGenerationRecoversAnchorPublishedBeforeRecord(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "recover")
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	state, err := os.OpenRoot(StateDir(repo))
	if err != nil {
		t.Fatal(err)
	}
	generation := Generation("fedcba9876543210fedcba9876543210")
	record := generationRecord{Version: forkGenerationVersion, Name: "recover", Generation: generation,
		WorkspaceAnchor: forkGenerationAnchorName("recover", generation), CreatedAt: time.Now().UTC()}
	root, err := fsidentity.Create(forkGenerationBinding(repo, record, state))
	if err != nil {
		_ = state.Close()
		t.Fatal(err)
	}
	_ = root.Close()
	_ = state.Close()
	identity := ensureTestGeneration(t, repo, "recover")
	if identity.Generation != generation {
		t.Fatalf("recovery minted %s, want published %s", identity.Generation, generation)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatal(err)
	}
}

func TestForkGenerationRemovalKeepsRecordUntilAnchorRetires(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "retire")
	identity := ensureTestGeneration(t, repo, "retire")
	record, err := readGenerationRecord(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(Workspace(repo, identity.Name)); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(StateDir(repo), record.WorkspaceAnchor)
	if err := os.Chmod(anchor, 0o644); err != nil {
		t.Fatal(err)
	}

	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	removeErr := RemoveGenerationIfMatchesLocked(repo, identity)
	unlock()
	if removeErr == nil {
		t.Fatal("generation removal succeeded before its private anchor could retire")
	}
	if current, present, err := ReadGeneration(repo, identity.Name); err != nil || !present || current != identity {
		t.Fatalf("failed anchor retirement lost retry record: %+v, present=%v, err=%v", current, present, err)
	}

	if err := os.Chmod(anchor, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err = LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = RemoveGenerationIfMatchesLocked(repo, identity)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, present, err := ReadGeneration(repo, identity.Name); err != nil || present {
		t.Fatalf("retry left generation record: present=%v, err=%v", present, err)
	}
	if _, err := os.Lstat(anchor); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry left private anchor: %v", err)
	}
}

func TestResolveProjectBindingRequiresExactGenerationWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	workspace := Workspace(repo, "perf")
	authority, bound, err := ResolveProjectBinding(workspace)
	if err != nil || authority != repo || bound == nil || *bound != identity {
		t.Fatalf("binding = %q %+v, %v", authority, bound, err)
	}
	plain := filepath.Join(t.TempDir(), "ordinary")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if authority, bound, err := ResolveProjectBinding(plain); err != nil || authority != plain || bound != nil {
		t.Fatalf("ordinary binding = %q %+v, %v", authority, bound, err)
	}
	if err := os.RemoveAll(workspace); err != nil {
		t.Fatal(err)
	}
	makeGenerationWorkspace(t, repo, "perf")
	if _, _, err := ResolveProjectBinding(workspace); err == nil {
		t.Fatal("replacement workspace inherited canonical binding")
	}
}

func TestForkGenerationAdoptsOnlyStoppedLegacyWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "legacy")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// A record of Coop v8.1.0 or earlier, written by hand: it names no generation.
	if err := os.WriteFile(PidPath(repo, "legacy"), []byte("owner-v1\n2147483646\n"+starttoken.Stale()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockState(repo, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureGenerationLocked(repo, "legacy"); err == nil {
		unlock()
		t.Fatal("legacy worker state was silently adopted")
	}
	if err := os.Remove(PidPath(repo, "legacy")); err != nil {
		unlock()
		t.Fatal(err)
	}
	if _, err := EnsureGenerationLocked(repo, "legacy"); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()
}

func TestForkGenerationRecordFailsClosed(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "bad")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(GenerationPath(repo, "bad"), []byte(`{"version":2,"name":"bad"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGeneration(repo, "bad"); err == nil {
		t.Fatal("unsupported generation record was treated as absent")
	}
	if err := os.Remove(GenerationPath(repo, "bad")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", GenerationPath(repo, "bad")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadGeneration(repo, "bad"); err == nil {
		t.Fatal("symlinked generation record was accepted")
	}
}

func TestForkGenerationPublicationNeverReplacesExistingAuthority(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	path := GenerationPath(repo, "race")
	want := []byte("existing authority\n")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeGenerationAtomic(repo, "race", []byte("replacement\n")); err == nil {
		t.Fatal("generation publication replaced an existing authority path")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Fatalf("existing generation changed: %q, %v", got, err)
	}
}

func TestForkGenerationPublicationLeavesOneLinkAndNoTemporaryAuthority(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := writeGenerationAtomic(repo, "clean", []byte("authority\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(GenerationPath(repo, "clean"))
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		t.Fatalf("published authority link count = %v", stat)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("published authority mode = %04o, want 0600", got)
	}
	matches, err := filepath.Glob(filepath.Join(StateDir(repo), ".clean.generation-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary generation authorities remain: %v", matches)
	}
}
