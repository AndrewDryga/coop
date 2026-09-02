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
