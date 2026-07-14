package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestHasYes(t *testing.T) {
	if !hasYes([]string{"x", "--yes"}) || !hasYes([]string{"-y"}) {
		t.Error("hasYes should detect --yes and -y")
	}
	if hasYes([]string{"x", "--yesss", "yes"}) {
		t.Error("hasYes must match only the exact -y/--yes flags")
	}
}

// destroyGate proceeds with --yes; without it in a non-TTY (the test env) it refuses and names --yes,
// so a pipe/CI can't delete on its own. (The TTY default-No prompt path needs a terminal to exercise.)
func TestDestroyGate(t *testing.T) {
	if err := destroyGate("delete X", true); err != nil {
		t.Errorf("destroyGate(yes) = %v, want nil (proceed)", err)
	}
	if err := destroyGate("delete task Y (todo)", false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("destroyGate(no, piped) = %v, want a refusal naming --yes", err)
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
	// Two queue dirs; queueProgress sums both, active = the first in_progress task.
	q1 := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q1, stateDone, "a", "task.md"), "# shipped\n")
	writeTaskFile(t, filepath.Join(q1, stateInProgress, "b", "task.md"), "# wiring it up\n")
	writeTaskFile(t, filepath.Join(q1, stateTodo, "c", "task.md"), "# later\n")
	q2 := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q2, stateDone, "d", "task.md"), "# also done\n")
	writeTaskFile(t, filepath.Join(q2, stateTodo, "e", "task.md"), "# another\n")
	writeTaskFile(t, filepath.Join(q2, stateBlocked, "f", "task.md"), "# stuck\n")
	c, active := queueProgress([]string{q1, q2})
	if c.Done != 2 || c.Doing != 1 || c.Todo != 2 || c.Blocked != 1 || c.total() != 6 {
		t.Errorf("counts = %+v (total %d), want Done2 Doing1 Todo2 Blocked1 total6", c, c.total())
	}
	// The active task is the first in_progress across the queues, not a later todo.
	if active != "wiring it up" {
		t.Errorf("active = %q, want %q", active, "wiring it up")
	}
	// A missing queue contributes nothing and doesn't panic.
	if c2, a2 := queueProgress([]string{filepath.Join(t.TempDir(), "nope")}); c2.total() != 0 || a2 != "" {
		t.Errorf("missing queue = %+v %q, want zero/empty", c2, a2)
	}
}

func TestProgressBanner(t *testing.T) {
	// Colors are off when stderr isn't a tty (as under `go test`), so the banner renders
	// plain — assert the structure.
	if got := progressBanner(3, taskCounts{Todo: 9, Doing: 1, Done: 4}, "Wire up the portal auth callback"); got != "iteration 3 · 4/14 done · now: Wire up the portal auth callback" {
		t.Errorf("banner = %q", got)
	}
	// Blocked is shown only when nonzero.
	if got := progressBanner(1, taskCounts{Done: 2, Blocked: 1, Todo: 1}, ""); got != "iteration 1 · 2/4 done · 1 blocked" {
		t.Errorf("blocked banner = %q", got)
	}
	// No active task → no "now:" clause; no blocked → no blocked clause.
	if got := progressBanner(2, taskCounts{Done: 5}, ""); got != "iteration 2 · 5/5 done" {
		t.Errorf("plain banner = %q", got)
	}
	// A long title is truncated, not printed whole.
	long := strings.Repeat("x", 80)
	if got := progressBanner(1, taskCounts{Todo: 1}, long); !strings.Contains(got, "…") || strings.Contains(got, long) {
		t.Errorf("long title not truncated: %q", got)
	}
}

func TestProgressLine(t *testing.T) {
	// The mid-iteration line the monitor prints live: done/total, blocked only when there
	// is some, and the active task — no "iteration N" prefix.
	if got := progressLine(taskCounts{Done: 8, Blocked: 1, Todo: 11}, "Task 9"); got != "8/20 done · 1 blocked · now: Task 9" {
		t.Errorf("progressLine = %q", got)
	}
	if got := progressLine(taskCounts{Done: 20}, ""); got != "20/20 done" {
		t.Errorf("done-only progressLine = %q", got)
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
