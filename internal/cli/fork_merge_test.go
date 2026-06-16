package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestMergeOneNoGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}} // no COOP_GATE → no box needed

	ws, err := setupFork(repo, "perf")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")

	landed, err := a.mergeOne(repo, "", "perf")
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merge did not land the fork's file")
	}
}

func TestMergeOneConflictRollsBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}

	ws, err := setupFork(repo, "a")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	// Fork and parent edit the same line → a merge conflict.
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("fork-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "commit", "-aqm", "fork edit")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("parent-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-aqm", "parent edit")

	landed, err := a.mergeOne(repo, "", "a")
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want (false, error)", landed, err)
	}
	// The conflicted merge must be fully aborted: tree clean, parent content intact.
	if gitDirty(repo) {
		t.Error("working tree left dirty after a conflicted merge")
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(data) != "parent-version\n" {
		t.Errorf("README.md = %q, want %q", data, "parent-version\n")
	}
}
