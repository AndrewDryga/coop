package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

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
		// blocked-only (nothing actionable, but parked on a decision) must NOT read as done.
		{"blocked only", forkStatus{Counts: taskCounts{Done: 1, Blocked: 2}}, "blocked"},
	}
	for _, c := range cases {
		if got := c.s.activeCell(); got != c.want {
			t.Errorf("%s: activeCell() = %q, want %q", c.name, got, c.want)
		}
	}
}

// `coop status` (watch or not) rejects a stray argument instead of silently ignoring it.
func TestStatusRejectsStrayArgs(t *testing.T) {
	a := &app{cfg: &config.Config{RepoOverride: t.TempDir()}}
	for _, args := range [][]string{{"bogus"}, {"-w", "bogus"}, {"bogus", "--watch"}} {
		if code, err := a.cmdStatus(args); code != 2 || err == nil {
			t.Errorf("cmdStatus(%v) = (%d, %v), want (2, usage error)", args, code, err)
		}
	}
}

// captureStderr returns whatever fn writes to os.Stderr (ui.Info goes there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestStatusShowsLocalQueueWhenNoForks: with no forks but a local queue, `coop status` reports
// the local queue's progress (the single-loop workflow) instead of a bare "no forks yet"; an
// empty repo still gets the plain message.
func TestStatusShowsLocalQueueWhenNoForks(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, tasksRoot)
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-a", "task.md"), "# Wire auth\n")
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-02-b", "task.md"), "# shipped\n")
	a := &app{cfg: &config.Config{RepoOverride: repo, TasksFiles: []string{tasksRoot}}}

	out := captureStderr(t, func() {
		if code, err := a.cmdStatus(nil); code != 0 || err != nil {
			t.Fatalf("status: code=%d err=%v", code, err)
		}
	})
	if !strings.Contains(out, "local queue:") || !strings.Contains(out, "1/2 done") {
		t.Errorf("status with no forks should report the local queue progress:\n%s", out)
	}

	// No forks AND no queue → the plain message stays.
	empty := t.TempDir()
	b := &app{cfg: &config.Config{RepoOverride: empty, TasksFiles: []string{tasksRoot}}}
	out2 := captureStderr(t, func() { _, _ = b.cmdStatus(nil) })
	if !strings.Contains(out2, "no forks yet") {
		t.Errorf("an empty repo should still show 'no forks yet':\n%s", out2)
	}
}
