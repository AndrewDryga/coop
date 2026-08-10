package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
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

// TestGitOutErr: the erroring read tells a FAILED git call apart from one that legitimately printed
// nothing, and carries git's own stderr so a caller can hand a human the cause. gitOut, which every
// display-only site still uses, keeps conflating the two.
func TestGitOutErr(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	if head, err := gitOutErr(repo, "rev-parse", "HEAD"); err != nil || head == "" {
		t.Fatalf("gitOutErr(rev-parse HEAD) = (%q, %v), want a sha and no error", head, err)
	}
	// Empty output from a command that SUCCEEDED (a clean tree) is not a failure.
	if out, err := gitOutErr(repo, "status", "--porcelain"); err != nil || out != "" {
		t.Fatalf("gitOutErr(status --porcelain) on a clean tree = (%q, %v), want empty and no error", out, err)
	}
	out, err := gitOutErr(repo, "rev-parse", "no-such-ref")
	if err == nil || out != "" {
		t.Fatalf("gitOutErr(rev-parse no-such-ref) = (%q, %v), want empty and an error", out, err)
	}
	if !strings.Contains(err.Error(), "rev-parse no-such-ref") {
		t.Errorf("gitOutErr error %q does not name the command that failed", err)
	}
	if gitOut(repo, "rev-parse", "no-such-ref") != "" {
		t.Error("gitOut must keep swallowing a failure as empty — display sites depend on it")
	}
	// git's stderr is the whole reason the error is actionable; a stub git proves it survives
	// without depending on the locale of the real git's messages.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf 'fatal: bad revision\\n' >&2\nexit 128\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if _, err := gitOutErr(repo, "log", "bogus..HEAD"); err == nil || !strings.Contains(err.Error(), "fatal: bad revision") {
		t.Errorf("gitOutErr = %v, want git's own stderr carried into the error", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate(short) = %q, want %q", got, "hello")
	}
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("truncate(long) = %q, want %q", got, "hell…")
	}
	// A non-positive width must not panic on the negative slice index — return empty.
	for _, n := range []int{0, -1, -5} {
		if got := truncate("hello", n); got != "" {
			t.Errorf("truncate(%q, %d) = %q, want empty", "hello", n, got)
		}
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

func TestPaintCount(t *testing.T) {
	paint := func(s string) string { return "<" + s + ">" }
	if got := paintCount(0, paint); got != "0" {
		t.Errorf("zero should stay plain, got %q", got)
	}
	if got := paintCount(3, paint); got != "<3>" {
		t.Errorf("nonzero should be painted, got %q", got)
	}
}

func TestColWidth(t *testing.T) {
	// Empty / all-short → clamps up to min (the header width).
	if got := colWidth(nil, 4, 24); got != 4 {
		t.Errorf("empty colWidth = %d, want min 4", got)
	}
	if got := colWidth([]string{"a", "bb"}, 4, 24); got != 4 {
		t.Errorf("all-short colWidth = %d, want min 4", got)
	}
	// Widest value within [min,max] wins.
	if got := colWidth([]string{"a", "abcdef"}, 4, 24); got != 6 {
		t.Errorf("colWidth = %d, want 6", got)
	}
	// Over max → clamps down to max.
	if got := colWidth([]string{strings.Repeat("x", 40)}, 4, 24); got != 24 {
		t.Errorf("over-max colWidth = %d, want 24", got)
	}
	// Width counts runes, not bytes: a 3-rune name with a multibyte glyph is width 3.
	if got := colWidth([]string{"ab…"}, 1, 24); got != 3 {
		t.Errorf("multibyte colWidth = %d, want 3 runes", got)
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
