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

	landed, err := a.mergeOne(repo, "", "perf", false)
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

	landed, err := a.mergeOne(repo, "", "a", false)
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

func TestMergeOnePolicyForce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}
	ws, err := setupFork(repo, "leak")
	if err != nil {
		t.Fatalf("setupFork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("S=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "leak")

	// Without --force the policy guard blocks the secret-like file.
	if landed, err := a.mergeOne(repo, "", "leak", false); landed || err == nil {
		t.Fatalf("mergeOne(force=false) = (%v, %v), want blocked", landed, err)
	}
	if pathExists(filepath.Join(repo, ".env")) {
		t.Fatal(".env landed despite the policy block")
	}
	// With --force it lands.
	if landed, err := a.mergeOne(repo, "", "leak", true); !landed || err != nil {
		t.Fatalf("mergeOne(force=true) = (%v, %v), want landed", landed, err)
	}
	if !pathExists(filepath.Join(repo, ".env")) {
		t.Error(".env not landed with --force")
	}
}

func TestForkMergeQueue(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	a := &app{cfg: &config.Config{}}
	// Two independent forks, each adding a distinct file.
	for _, n := range []string{"a", "b"} {
		ws, err := setupFork(repo, n)
		if err != nil {
			t.Fatalf("setupFork %s: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(ws, n+".txt"), []byte(n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", n)
	}
	if code, err := a.forkMergeAll(repo, "", false); err != nil || code != 0 {
		t.Fatalf("forkMergeAll = (%d, %v), want (0, nil)", code, err)
	}
	if !pathExists(filepath.Join(repo, "a.txt")) || !pathExists(filepath.Join(repo, "b.txt")) {
		t.Error("merge queue did not land both forks")
	}
	if got := forkNames(repo); len(got) != 0 {
		t.Errorf("forks remain after the queue closed them: %v", got)
	}
	// Rebasing must keep history linear — no merge commits.
	if merges := gitOut(repo, "rev-list", "--merges", "HEAD"); merges != "" {
		t.Errorf("rebase queue produced merge commits (history not linear):\n%s", merges)
	}
}
