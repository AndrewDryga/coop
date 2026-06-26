package cli

import "testing"

// Task tallying/active-task logic is tested in taskdir_test.go (taskTreeCounts); here we cover
// the status cell that renders it.
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
