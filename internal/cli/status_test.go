package cli

import "testing"

func TestScanTasks(t *testing.T) {
	const queue = `# .agent/TASKS.md — the work queue.
# [ ] todo   [w] claimed   [x] done   [B] blocked

## Example

- [E] an example task that must not be counted
  - **Context:** prose with a [ ] that is not a task line.

## Active

- [x] shipped one
- [x] shipped two
- [w] the one in flight
- [ ] next up
- [ ] later
- [B] waiting on a decision
  - **Context:** indented "- [ ] " lines like this are not tasks either.
`
	c, active := scanTasks(queue)
	if c.Done != 2 {
		t.Errorf("Done = %d, want 2", c.Done)
	}
	if c.Doing != 1 {
		t.Errorf("Doing = %d, want 1", c.Doing)
	}
	if c.Todo != 2 {
		t.Errorf("Todo = %d, want 2", c.Todo)
	}
	if c.Blocked != 1 {
		t.Errorf("Blocked = %d, want 1", c.Blocked)
	}
	if c.total() != 6 {
		t.Errorf("total = %d, want 6 (the [E] example and indented lines excluded)", c.total())
	}
	// Active is the in-flight [w] task, not the first [ ].
	if active != "the one in flight" {
		t.Errorf("active = %q, want %q", active, "the one in flight")
	}
}

func TestScanTasksSkipsFencedTasks(t *testing.T) {
	// A "- [ ]" documented inside a ``` code fence (at column 0) must NOT be counted — otherwise
	// the loop never sees the queue empty and the bar/split see phantom tasks.
	queue := "## Active\n\n- [ ] real one\n\n" +
		"```\n- [ ] fenced phantom\n- [x] also fenced\n```\n\n" +
		"- [x] done\n"
	c, active := scanTasks(queue)
	if c.Todo != 1 || c.Done != 1 || c.total() != 2 {
		t.Errorf("counts = %+v, want Todo 1 / Done 1 / total 2 (fenced lines excluded)", c)
	}
	if active != "real one" {
		t.Errorf("active = %q, want %q", active, "real one")
	}
}

func TestScanTasksActiveFallsBackToTodo(t *testing.T) {
	// No [w] claimed → the active task is the first unclaimed [ ].
	c, active := scanTasks("- [x] done\n- [ ] do this next\n- [ ] and then this\n")
	if active != "do this next" {
		t.Errorf("active = %q, want first todo %q", active, "do this next")
	}
	if c.Done != 1 || c.Todo != 2 {
		t.Errorf("counts = %+v, want Done 1 Todo 2", c)
	}
}

func TestScanTasksEmpty(t *testing.T) {
	c, active := scanTasks("")
	if c.total() != 0 || active != "" {
		t.Errorf("empty queue: counts=%+v active=%q, want zero/empty", c, active)
	}
}

func TestActiveCell(t *testing.T) {
	cases := []struct {
		name string
		s    forkStatus
		want string
	}{
		{"no queue", forkStatus{}, "(no queue)"},
		{"all done", forkStatus{Counts: taskCounts{Done: 3}}, "✓ done"},
		{"in flight", forkStatus{Active: "fix the bug", Counts: taskCounts{Doing: 1}}, "fix the bug"},
	}
	for _, c := range cases {
		if got := c.s.activeCell(); got != c.want {
			t.Errorf("%s: activeCell() = %q, want %q", c.name, got, c.want)
		}
	}
}
