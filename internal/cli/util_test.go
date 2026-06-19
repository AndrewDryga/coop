package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueueHasTodo(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "TASKS.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const legend = "# [ ] todo   [w] claimed   [x] done   [B] blocked\n"

	// The legend documents "[ ]" and the # Example block uses [E]; neither is work.
	if queueHasTodo(write(legend + "\n# Example\n- [E] sample task\n\n## Active\n")) {
		t.Error("legend + [E] example must not count as a todo")
	}
	// Claimed/done/blocked items aren't open todos either.
	if queueHasTodo(write(legend + "## Active\n- [x] done\n- [w] in progress\n- [B] blocked\n")) {
		t.Error("[x]/[w]/[B] must not count as a todo")
	}
	// A real unclaimed task does.
	if !queueHasTodo(write(legend + "## Active\n- [ ] do the thing\n")) {
		t.Error("an open - [ ] task should count")
	}
	// A missing file is not a todo.
	if queueHasTodo(filepath.Join(t.TempDir(), "nope.md")) {
		t.Error("a missing queue should be false")
	}
}

func TestQueueProgress(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "TASKS.md")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Two queues: the first has the claimed (active) task; queueProgress sums both.
	q1 := write("## Active\n- [x] shipped\n- [w] wiring it up\n- [ ] later\n")
	q2 := write("## Active\n- [x] also done\n- [ ] another\n- [B] stuck\n")
	c, active := queueProgress([]string{q1, q2})
	if c.Done != 2 || c.Doing != 1 || c.Todo != 2 || c.Blocked != 1 || c.total() != 6 {
		t.Errorf("counts = %+v (total %d), want Done2 Doing1 Todo2 Blocked1 total6", c, c.total())
	}
	// The active task is the first [w] across the queues, not a later todo.
	if active != "wiring it up" {
		t.Errorf("active = %q, want %q", active, "wiring it up")
	}
	// A missing queue contributes nothing and doesn't panic.
	if c2, a2 := queueProgress([]string{filepath.Join(t.TempDir(), "nope.md")}); c2.total() != 0 || a2 != "" {
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
