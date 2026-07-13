package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestFinishedTasksAndReconcileDecision(t *testing.T) {
	before := map[string]string{"a": stateInProgress, "b": stateTodo, "c": stateDone}
	after := map[string]string{"a": stateDone, "b": stateTodo, "c": stateDone}
	if got := finishedTasks(before, after); len(got) != 1 || got[0] != "a" {
		t.Errorf("finishedTasks = %v, want [a]", got)
	}
	// reconcileMerged: a landed todo/in_progress task moves; a landed blocked task is flagged (no
	// move); an unlanded task is ignored entirely.
	states := map[string]string{"todo1": stateTodo, "wip1": stateInProgress, "blk1": stateBlocked, "safe": stateTodo}
	landed := map[string]bool{"todo1": true, "wip1": true, "blk1": true} // "safe" did NOT land
	acts := reconcileMerged(states, landed)
	got := map[string]bool{}
	for _, a := range acts {
		got[a.ID] = a.Move
	}
	if len(acts) != 3 || !got["todo1"] || !got["wip1"] || got["blk1"] {
		t.Errorf("reconcileMerged = %+v; want todo1/wip1 move, blk1 flagged, safe absent", acts)
	}
	if _, present := got["safe"]; present {
		t.Error("an unlanded task must not be reconciled")
	}
}

func TestResumeLine(t *testing.T) {
	// No landed commit → empty (blind-resume path stays byte-identical).
	if resumeLine("x", nil) != "" {
		t.Error("no commits should yield no resume line")
	}
	// A landed commit → a line that names the sha and BOTH cases (finish-the-move vs reopened-rework),
	// so it never falsely asserts the task is done.
	l := resumeLine("my-task", []string{"abc123"})
	for _, want := range []string{"my-task", "abc123", "log.md", "REOPENED", "finish the move"} {
		if !strings.Contains(l, want) {
			t.Errorf("resume line missing %q:\n%s", want, l)
		}
	}
}

// TestCommitsForTaskAndUntrailered drives the real git trailer read: a commit tagged Coop-Task is
// found by id; the untrailered check flags a finished task whose range has no such commit.
func TestCommitsForTaskAndUntrailered(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "did the work\n\nCoop-Task: task-42")
	head := gitOut(repo, "rev-parse", "HEAD")

	if c := commitsForTask(repo, "", "task-42"); len(c) != 1 {
		t.Errorf("commitsForTask(task-42) = %v, want 1", c)
	}
	if c := commitsForTask(repo, "", "task-99"); len(c) != 0 {
		t.Errorf("commitsForTask(task-99) = %v, want none", c)
	}
	// A finished task WITH a trailer commit in range is bindable (not untrailered); one WITHOUT is.
	if m := untrailered(repo, base, head, []string{"task-42"}); len(m) != 0 {
		t.Errorf("task-42 is trailered in range, should not be flagged: %v", m)
	}
	if m := untrailered(repo, base, head, []string{"task-42", "task-99"}); len(m) != 1 || m[0] != "task-99" {
		t.Errorf("untrailered = %v, want [task-99]", m)
	}
	// landedTasks sees the trailer across all history.
	if !landedTasks(repo)["task-42"] {
		t.Error("landedTasks should include task-42")
	}
}

// TestReconcileQueueAfterMerge: a queued task whose Coop-Task trailer just landed moves to done;
// a blocked task with a landed trailer is NOT moved (flagged for a human); an unlanded task stays.
func TestReconcileQueueAfterMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	q := filepath.Join(repo, tasksRoot)
	writeTaskFile(t, filepath.Join(q, stateTodo, "todo1", "task.md"), "# todo1\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "task.md"), "# wip1\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "task.md"), "# blk1\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "decision.md"), "# blocked\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "safe", "task.md"), "# safe\n")
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	// A landed commit for todo1, wip1, and blk1 (as a merged fork would carry); "safe" did not land.
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed queue")
	git("commit", "-q", "--allow-empty", "-m", "todo1 work\n\nCoop-Task: todo1")
	git("commit", "-q", "--allow-empty", "-m", "wip1 work\n\nCoop-Task: wip1")
	git("commit", "-q", "--allow-empty", "-m", "blk1 work\n\nCoop-Task: blk1")

	a := &app{cfg: &config.Config{}}
	a.reconcileQueueAfterMerge(repo, "fork1")

	if !pathExists(filepath.Join(q, stateDone, "todo1")) || pathExists(filepath.Join(q, stateTodo, "todo1")) {
		t.Error("a landed todo task should have moved to done")
	}
	if !pathExists(filepath.Join(q, stateDone, "wip1")) {
		t.Error("a landed in_progress task should have moved to done")
	}
	if !pathExists(filepath.Join(q, stateBlocked, "blk1")) || pathExists(filepath.Join(q, stateDone, "blk1")) {
		t.Error("a blocked task must be flagged, never auto-moved")
	}
	if !pathExists(filepath.Join(q, stateTodo, "safe")) {
		t.Error("an unlanded task must stay put")
	}
	// The reconciled task got a note in its log.md.
	if data, _ := os.ReadFile(filepath.Join(q, stateDone, "todo1", "log.md")); !strings.Contains(string(data), "reconciled: landed by fork fork1") {
		t.Errorf("reconcile note missing from todo1 log.md: %q", data)
	}
}
