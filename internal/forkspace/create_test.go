package forkspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	if err != nil || !strings.Contains(string(data), ".coop/") {
		t.Fatalf("fork exclusion = %q, %v; want .coop/", data, err)
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
