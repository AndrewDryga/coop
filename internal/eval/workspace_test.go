package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// A materialized workspace is a faithful copy of the fixture under a FRESH repository: the files are
// there, but nothing of a source `.git` — no history, ref, object or hook — travels into it, and the
// object store holds only the one synthetic commit.
func TestPrepareWorkspaceCopiesTheTreeWithNoSourceHistory(t *testing.T) {
	gitAvailable(t)
	fixture := t.TempDir()
	write(t, fixture, "app.go", "package app\n")
	write(t, fixture, "sub/note.txt", "hi\n")
	if err := os.Chmod(filepath.Join(fixture, "sub", "note.txt"), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "trial")
	commit, err := PrepareWorkspace(context.Background(), fixture, dest)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "app.go")); err != nil || string(b) != "package app\n" {
		t.Errorf("app.go not copied faithfully: %q %v", b, err)
	}
	if info, err := os.Stat(filepath.Join(dest, "sub", "note.txt")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode not preserved: %v %v", info, err)
	}
	// Exactly one commit, no alternates/packed-refs, no unreachable objects, and a fresh (empty) hooks.
	if c := strings.TrimSpace(git(t, dest, "rev-list", "--all", "--count")); c != "1" {
		t.Errorf("trial has %s commits, want exactly the synthetic one", c)
	}
	if strings.TrimSpace(git(t, dest, "rev-parse", "HEAD")) != commit {
		t.Error("returned commit is not the trial HEAD")
	}
	for _, forbidden := range []string{".git/objects/info/alternates", ".git/packed-refs"} {
		if _, err := os.Stat(filepath.Join(dest, forbidden)); !os.IsNotExist(err) {
			t.Errorf("%s exists in the trial repo", forbidden)
		}
	}
	if fsck := git(t, dest, "fsck", "--no-progress", "--unreachable"); strings.TrimSpace(fsck) != "" {
		t.Errorf("trial object store has unreachable objects:\n%s", fsck)
	}
	if entries, _ := os.ReadDir(filepath.Join(dest, ".git", "hooks")); len(entries) != 0 {
		t.Errorf("trial .git/hooks is not empty: %d entries", len(entries))
	}
}

// The critical one: a fixture that is (or contains) a live checkout is REFUSED, so init/commit can
// never adopt the source repository and write into the developer's own history.
func TestPrepareWorkspaceRefusesAFixtureWithAGitEntry(t *testing.T) {
	gitAvailable(t)
	for _, shape := range []struct {
		name string
		make func(t *testing.T, dir string)
	}{
		{"top-level .git dir", func(t *testing.T, dir string) { mkdir(t, filepath.Join(dir, ".git")) }},
		{"worktree .git file", func(t *testing.T, dir string) { write(t, dir, ".git", "gitdir: /somewhere/.git/worktrees/wt\n") }},
		{".git symlink", func(t *testing.T, dir string) {
			mkdir(t, filepath.Join(dir, "meta"))
			if err := os.Symlink("meta", filepath.Join(dir, ".git")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			fixture := t.TempDir()
			write(t, fixture, "app.go", "package app\n")
			shape.make(t, fixture)
			if _, err := PrepareWorkspace(context.Background(), fixture, filepath.Join(t.TempDir(), "trial")); err == nil {
				t.Fatal("a fixture with a .git entry was accepted")
			}
		})
	}
}

// A NESTED .git (submodule gitfile) deeper in the tree is skipped, never carried as a gitlink.
func TestPrepareWorkspaceSkipsANestedGitEntry(t *testing.T) {
	gitAvailable(t)
	fixture := t.TempDir()
	write(t, fixture, "sub/main.go", "package sub\n")
	write(t, fixture, "sub/.git", "gitdir: /somewhere/.git/modules/sub\n")
	dest := filepath.Join(t.TempDir(), "trial")
	if _, err := PrepareWorkspace(context.Background(), fixture, dest); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", ".git")); !os.IsNotExist(err) {
		t.Error("a nested .git travelled into the trial")
	}
	// sub is ordinary files, not a gitlink.
	if out := git(t, dest, "ls-files", "--stage", "sub/main.go"); !strings.HasPrefix(out, "100644") {
		t.Errorf("sub/main.go is not an ordinary tracked file: %q", out)
	}
}

func TestPrepareWorkspaceRefusesEscapingAndAbsoluteSymlinks(t *testing.T) {
	gitAvailable(t)
	// A link whose target, resolved through the kernel, lands outside the fixture is refused — the
	// physical resolution (EvalSymlinks) catches an escape a lexical clean would miss.
	t.Run("escaping link", func(t *testing.T) {
		fixture := t.TempDir()
		if err := os.Symlink("..", filepath.Join(fixture, "escape")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := PrepareWorkspace(context.Background(), fixture, filepath.Join(t.TempDir(), "trial")); err == nil {
			t.Fatal("a link resolving outside the fixture was accepted")
		}
	})
	// A benign RELATIVE link that resolves inside the fixture is copied faithfully (not over-refused).
	t.Run("in-fixture link is kept", func(t *testing.T) {
		fixture := t.TempDir()
		write(t, fixture, "real.txt", "hi\n")
		if err := os.Symlink("real.txt", filepath.Join(fixture, "alias")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		dest := filepath.Join(t.TempDir(), "trial")
		if _, err := PrepareWorkspace(context.Background(), fixture, dest); err != nil {
			t.Fatalf("a benign in-fixture link was refused: %v", err)
		}
		if got, err := os.Readlink(filepath.Join(dest, "alias")); err != nil || got != "real.txt" {
			t.Errorf("in-fixture link not copied: %q %v", got, err)
		}
	})
	t.Run("absolute target", func(t *testing.T) {
		fixture := t.TempDir()
		write(t, fixture, "data.txt", "x\n")
		if err := os.Symlink(filepath.Join(fixture, "data.txt"), filepath.Join(fixture, "peek")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		_, err := PrepareWorkspace(context.Background(), fixture, filepath.Join(t.TempDir(), "trial"))
		if err == nil || !strings.Contains(err.Error(), "absolute target") {
			t.Fatalf("an absolute symlink was not refused: %v", err)
		}
	})
}

// A fixture cannot steer git through a shipped ~/.config/git/ignore: the synthetic commit is made
// with a hermetic HOME, so an ignore file the fixture carries does not drop authored files.
func TestPrepareWorkspaceIgnoresAFixtureSuppliedGitHome(t *testing.T) {
	gitAvailable(t)
	fixture := t.TempDir()
	write(t, fixture, "main.go", "package main\n")
	write(t, fixture, ".config/git/ignore", "*.go\n")
	dest := filepath.Join(t.TempDir(), "trial")
	if _, err := PrepareWorkspace(context.Background(), fixture, dest); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if out := git(t, dest, "ls-files"); !strings.Contains(out, "main.go") {
		t.Errorf("a fixture-supplied git ignore dropped main.go from the commit: %q", out)
	}
}

// A fixture's own .gitignore is honored (a real-checkout behavior): the ignored file stays on disk
// but out of the initial commit.
func TestPrepareWorkspaceHonorsAFixtureGitignore(t *testing.T) {
	gitAvailable(t)
	fixture := t.TempDir()
	write(t, fixture, "keep.go", "package x\n")
	write(t, fixture, ".gitignore", "secret.txt\n")
	write(t, fixture, "secret.txt", "shh\n")
	dest := filepath.Join(t.TempDir(), "trial")
	if _, err := PrepareWorkspace(context.Background(), fixture, dest); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "secret.txt")); err != nil {
		t.Error("an ignored file should still be on disk in the workspace")
	}
	if out := git(t, dest, "ls-files"); strings.Contains(out, "secret.txt") {
		t.Errorf("an ignored file was committed: %q", out)
	}
}

func TestPrepareWorkspaceRefusesAnExistingDest(t *testing.T) {
	gitAvailable(t)
	if _, err := PrepareWorkspace(context.Background(), t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("prepare wrote into an existing workspace")
	}
}

// --- helpers ---

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
