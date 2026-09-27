package forkspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func committedSetupRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo := initRepo(t)
	gitIn(t, repo, "checkout", "-q", "-b", "main")
	gitIn(t, repo, "config", "user.email", "t@t")
	gitIn(t, repo, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "README.md")
	gitIn(t, repo, "commit", "-qm", "base")
	return repo
}

func TestSetupAcceptsCloneAlreadyOnRequestedBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "main")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if branch, err := gitOutputContext(context.Background(), ws, "symbolic-ref", "--short", "HEAD"); err != nil || branch != "main" {
		t.Fatalf("fork branch = %q, %v; want main", branch, err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil || !strings.Contains(string(data), ".coop/") ||
		!strings.Contains(string(data), "/"+GenerationMarkerName) {
		t.Fatalf("fork exclusion = %q, %v; want bookkeeping and identity markers", data, err)
	}
}

func TestPinnedSetupCarriesGlobalExcludesWithoutGitTemplates(t *testing.T) {
	repo := committedSetupRepo(t)
	ignore := filepath.Join(t.TempDir(), "ignore")
	if err := os.WriteFile(ignore, []byte("local-output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "config", "--global", "core.excludesfile", ignore)
	commit, err := gitOutputContext(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ws, err := SetupPinnedContext(context.Background(), repo, "pinned", commit)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".git", "info", "exclude"))
	if err != nil || !strings.Contains(string(data), "local-output\n") || !strings.Contains(string(data), ".coop/") {
		t.Fatalf("pinned fork exclusions = %q, %v", data, err)
	}
}

func TestSetupRefusesNameWhilePriorSessionWorkspaceIsStaged(t *testing.T) {
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "remote")
	if err != nil {
		t.Fatal(err)
	}
	identity := ensureTestGeneration(t, repo, "remote")
	handle, info, err := Pin(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	unlock, err := LockState(repo, "remote")
	if err != nil {
		t.Fatal(err)
	}
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerID: "remote_staged", CreatedAt: time.Now().UTC(),
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
	_, setupErr := Setup(repo, "remote")
	unlock()
	if setupErr == nil || !strings.Contains(setupErr.Error(), "still being discarded") {
		t.Fatalf("Setup after interrupted discard = %v, want staged-discard refusal", setupErr)
	}
	if _, err := os.Lstat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused Setup left a replacement workspace: %v", err)
	}
}

func TestSetupRemovesCloneWhenBranchCannotBeCreated(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	name := "bad..name"
	ws, err := Setup(repo, name)
	if err == nil || !strings.Contains(err.Error(), "check out fork branch") {
		t.Fatalf("Setup = %q, %v; want checkout error", ws, err)
	}
	if _, statErr := os.Lstat(ws); !os.IsNotExist(statErr) {
		t.Fatalf("incomplete workspace remains at %s: %v", ws, statErr)
	}
}

func TestSetupConfirmsWorkspaceParentAndDurablyCleansUpOnFailure(t *testing.T) {
	repo := committedSetupRepo(t)
	previous := syncForkDirectoryEntry
	t.Cleanup(func() { syncForkDirectoryEntry = previous })
	failure := errors.New("synthetic workspace parent sync failure")
	homeSyncs := 0
	syncForkDirectoryEntry = func(dir *os.File) error {
		if filepath.Clean(dir.Name()) == filepath.Clean(Home(repo)) {
			homeSyncs++
			if homeSyncs == 1 {
				return failure
			}
		}
		return dir.Sync()
	}
	ws, err := Setup(repo, "durable")
	if !errors.Is(err, failure) {
		t.Fatalf("Setup = %v, want %v", err, failure)
	}
	if homeSyncs < 2 {
		t.Fatalf("workspace create/cleanup used %d parent barriers, want both", homeSyncs)
	}
	if _, err := os.Lstat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed durable creation left a workspace: %v", err)
	}

	syncForkDirectoryEntry = previous
	if retry, err := Setup(repo, "durable"); err != nil || retry != ws {
		t.Fatalf("Setup retry = (%q, %v), want %q", retry, err, ws)
	}
}

func TestDestroyConfirmsWorkspaceAbsenceBeforeReturningSuccess(t *testing.T) {
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "remove-durable")
	if err != nil {
		t.Fatal(err)
	}
	previous := syncForkDirectoryEntry
	t.Cleanup(func() { syncForkDirectoryEntry = previous })
	failure := errors.New("synthetic workspace removal sync failure")
	syncForkDirectoryEntry = func(dir *os.File) error {
		if filepath.Clean(dir.Name()) == filepath.Clean(Home(repo)) {
			return failure
		}
		return dir.Sync()
	}
	if err := Destroy(repo, "remove-durable"); !errors.Is(err, failure) {
		t.Fatalf("Destroy = %v, want %v", err, failure)
	}
	if _, err := os.Lstat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remained after visible removal: %v", err)
	}

	confirmed := false
	syncForkDirectoryEntry = func(dir *os.File) error {
		if filepath.Clean(dir.Name()) == filepath.Clean(Home(repo)) {
			confirmed = true
		}
		return dir.Sync()
	}
	if err := Destroy(repo, "remove-durable"); err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("Destroy retry did not confirm the already-visible workspace absence")
	}
}

func TestSetupPinnedCommitAvoidsLocalHardlinks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("selected\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "README.md")
	gitIn(t, repo, "commit", "-qm", "selected")
	pinned, err := gitOutputContext(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "reset", "--hard", "-q", "HEAD^")
	marker := filepath.Join(t.TempDir(), "upload-pack-ran")
	gitIn(t, repo, "config", "uploadpack.packObjectsHook", "touch "+marker)

	ws, err := SetupPinnedContext(context.Background(), repo, "session", pinned)
	if err != nil {
		t.Fatal(err)
	}
	if head, err := gitOutputContext(context.Background(), ws, "rev-parse", "HEAD"); err != nil || head != pinned {
		t.Fatalf("workspace HEAD = %q, %v; want %s", head, err, pinned)
	}
	if origin, err := gitOutputContext(context.Background(), ws, "config", "--get", "remote.origin.url"); err != nil || origin != repo {
		t.Fatalf("origin = %q, %v; want %s", origin, err, repo)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("workspace has a persistent object alternate: %v", err)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("source upload-pack hook ran: %v", err)
	}
	sourceObject := filepath.Join(repo, ".git", "objects", pinned[:2], pinned[2:])
	workspaceObject := filepath.Join(ws, ".git", "objects", pinned[:2], pinned[2:])
	sourceInfo, sourceErr := os.Stat(sourceObject)
	workspaceInfo, workspaceErr := os.Stat(workspaceObject)
	if sourceErr != nil || workspaceErr != nil {
		t.Fatalf("inspect selected commit objects: source=%v workspace=%v", sourceErr, workspaceErr)
	}
	if os.SameFile(sourceInfo, workspaceInfo) {
		t.Fatal("session workspace hardlinked the selected object from the mutable source")
	}
}

func TestCheckoutNewBranchIgnoresRepositoryFSMonitor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	evil := filepath.Join(repo, ".git", "evil-fsmonitor")
	marker := evil + ".ran"
	if err := os.WriteFile(evil, []byte("#!/bin/sh\ntouch \"$0.ran\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "config", "core.fsmonitor", evil)
	if err := gitCheckoutNewBranchContext(context.Background(), repo, "safe"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Fatal("hardened checkout executed the repository's core.fsmonitor")
	}
	_ = exec.Command("git", "-C", repo, "status", "--porcelain").Run()
	if _, err := os.Lstat(marker); err != nil {
		t.Fatal("positive control failed: raw git did not execute the planted core.fsmonitor")
	}
}

func TestRequiredForkMetadataFailuresAreReturned(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	t.Run("identity destination", func(t *testing.T) {
		err := PropagateGitIdentity(repo, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "set fork Git") {
			t.Fatalf("PropagateGitIdentity error = %v", err)
		}
	})
	t.Run("exclude wrong type", func(t *testing.T) {
		ws := t.TempDir()
		exclude := filepath.Join(ws, ".git", "info", "exclude")
		if err := os.MkdirAll(exclude, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := Exclude(ws, ".coop/"); err == nil {
			t.Fatal("Exclude succeeded with a directory at .git/info/exclude")
		}
	})
}

func TestExcludeUsesLinkedWorktreeMetadataAndRefusesSymlinkRedirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "linked", linked)
	if err := ExcludeIfRepository(linked, "/.coop-network-approval"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".coop-network-approval"), []byte("marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := gitOutputContext(context.Background(), linked, "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("linked worktree marker status = %q, %v", status, err)
	}

	exclude := filepath.Join(repo, ".git", "info", "exclude")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(exclude); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, exclude); err != nil {
		t.Fatal(err)
	}
	if err := Exclude(repo, "/.another-marker"); err == nil {
		t.Fatal("symlinked info/exclude redirected a host write")
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "unchanged\n" {
		t.Fatalf("exclude redirect changed victim: %q, %v", data, err)
	}
}

func TestExcludeIfRepositoryRefusesRepositoryControlledGitRedirect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	victim := committedSetupRepo(t)
	exclude := filepath.Join(victim, ".git", "info", "exclude")
	before, err := os.ReadFile(exclude)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		plant func(string) error
	}{
		{"symlink", func(path string) error { return os.Symlink(filepath.Join(victim, ".git"), path) }},
		{"gitdir pointer", func(path string) error {
			return os.WriteFile(path, []byte("gitdir: "+filepath.Join(victim, ".git")+"\n"), 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := t.TempDir()
			if err := test.plant(filepath.Join(project, ".git")); err != nil {
				t.Fatal(err)
			}
			if err := ExcludeIfRepository(project, "/.coop-network-approval"); err == nil {
				t.Fatal("repository-controlled .git redirect was accepted")
			}
			after, err := os.ReadFile(exclude)
			if err != nil || string(after) != string(before) {
				t.Fatalf("redirect changed another repository's exclude: %q, %v", after, err)
			}
		})
	}
}

func TestExcludeRejectsGitPointerFIFOsWithoutBlocking(t *testing.T) {
	assertRefusedPromptly := func(t *testing.T, project, fifo string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- ExcludeIfRepository(project, "/.coop-network-approval") }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("repository-controlled Git FIFO was accepted")
			}
		case <-time.After(2 * time.Second):
			// Unblock a regressed os.ReadFile so the test process can finish cleanly.
			writer, _ := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if writer != nil {
				_ = writer.Close()
			}
			t.Fatal("host Git metadata inspection blocked on a repository-controlled FIFO")
		}
	}

	t.Run("dot git", func(t *testing.T) {
		project := t.TempDir()
		fifo := filepath.Join(project, ".git")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefusedPromptly(t, project, fifo)
	})
	t.Run("common directory", func(t *testing.T) {
		project := committedSetupRepo(t)
		fifo := filepath.Join(project, ".git", "commondir")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefusedPromptly(t, project, fifo)
	})
	t.Run("linked worktree backlink", func(t *testing.T) {
		repo := committedSetupRepo(t)
		project := filepath.Join(t.TempDir(), "linked")
		gitIn(t, repo, "worktree", "add", "-q", "-b", "linked-fifo", project)
		pointer, err := os.ReadFile(filepath.Join(project, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		admin := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(pointer)), "gitdir:"))
		fifo := filepath.Join(admin, "gitdir")
		if err := os.Remove(fifo); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		assertRefusedPromptly(t, project, fifo)
	})
}

func TestExcludeRejectsGitEntrySymlinkedToDevice(t *testing.T) {
	if _, err := os.Lstat("/dev/null"); err != nil {
		t.Skip("/dev/null is unavailable")
	}
	project := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(project, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := ExcludeIfRepository(project, "/.coop-network-approval"); err == nil {
		t.Fatal("device-backed .git symlink was accepted")
	}
}
