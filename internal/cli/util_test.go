package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestHasYes(t *testing.T) {
	if !hasYes([]string{"x", "--yes"}) || !hasYes([]string{"-y"}) {
		t.Error("hasYes should detect --yes and -y")
	}
	if hasYes([]string{"x", "--yesss", "yes"}) {
		t.Error("hasYes must match only the exact -y/--yes flags")
	}
}

func TestGitSignOutput(t *testing.T) {
	bin := t.TempDir()
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\nprintf 'git stdout\\n'\nprintf 'git diagnostic\\n' >&2\n[ \"$FAKE_GIT_FAIL\" != 1 ]\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GIT_TRACE", "")

	var stderr bytes.Buffer
	if err := gitSignTo(&stderr, t.TempDir(), "rebase", "--gpg-sign"); err != nil {
		t.Fatalf("successful gitSignTo = %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("successful gitSignTo leaked output: %q", stderr.String())
	}
	t.Setenv("GIT_TRACE", "0")
	if err := gitSignTo(&stderr, t.TempDir(), "rebase", "--gpg-sign"); err != nil {
		t.Fatalf("GIT_TRACE=0 gitSignTo = %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("GIT_TRACE=0 should stay quiet: %q", stderr.String())
	}

	t.Setenv("FAKE_GIT_FAIL", "1")
	stderr.Reset()
	if err := gitSignTo(&stderr, t.TempDir(), "rebase", "--gpg-sign"); err == nil {
		t.Fatal("failed gitSignTo returned nil")
	}
	for _, want := range []string{"git stdout", "git diagnostic"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("failed gitSignTo output %q missing %q", stderr.String(), want)
		}
	}

	t.Setenv("FAKE_GIT_FAIL", "")
	t.Setenv("GIT_TRACE", "1")
	stderr.Reset()
	if err := gitSignTo(&stderr, t.TempDir(), "rebase", "--gpg-sign"); err != nil {
		t.Fatalf("traced gitSignTo = %v", err)
	}
	if !strings.Contains(stderr.String(), "git diagnostic") {
		t.Errorf("traced gitSignTo did not replay output: %q", stderr.String())
	}
}

func TestQueueProgress(t *testing.T) {
	// Two queue dirs; queueProgress sums both and an in_progress task in a later queue still
	// beats a todo in the first queue.
	q1 := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q1, stateDone, "a", "task.md"), "# shipped\n")
	writeTaskFile(t, filepath.Join(q1, stateTodo, "c", "task.md"), "# later\n")
	q2 := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q2, stateDone, "d", "task.md"), "# also done\n")
	writeTaskFile(t, filepath.Join(q2, stateInProgress, "e", "task.md"), "# wiring it up\n")
	writeTaskFile(t, filepath.Join(q2, stateTodo, "g", "task.md"), "# another\n")
	writeTaskFile(t, filepath.Join(q2, stateBlocked, "f", "task.md"), "# stuck\n")
	c, active := queueProgress([]string{q1, q2})
	if c.Done != 2 || c.Doing != 1 || c.Todo != 2 || c.Blocked != 1 || c.Total() != 6 {
		t.Errorf("counts = %+v (total %d), want Done2 Doing1 Todo2 Blocked1 total6", c, c.Total())
	}
	// The active task is the first in_progress across the queues, not a later todo.
	if active != "wiring it up" {
		t.Errorf("active = %q, want %q", active, "wiring it up")
	}
	// A missing queue contributes nothing and doesn't panic.
	if c2, a2 := queueProgress([]string{filepath.Join(t.TempDir(), "nope")}); c2.Total() != 0 || a2 != "" {
		t.Errorf("missing queue = %+v %q, want zero/empty", c2, a2)
	}
}

func TestPadRight(t *testing.T) {
	if got := padRight("ab", 5); got != "ab   " {
		t.Errorf("padRight = %q, want %q", got, "ab   ")
	}
	// Already at/over width → unchanged (never truncates).
	if got := padRight("abcde", 3); got != "abcde" {
		t.Errorf("over-width padRight = %q, want unchanged", got)
	}
	// Pads by RUNES: "ab…" is 3 runes (5 bytes) — to width 5 it gets 2 spaces, not 0.
	if got := padRight("ab…", 5); got != "ab…  " {
		t.Errorf("multibyte padRight = %q, want 2 trailing spaces", got)
	}
}

func gitDirty(dir string) bool { return forkspace.GitOut(dir, "status", "--porcelain") != "" }
