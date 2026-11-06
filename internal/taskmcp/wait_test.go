package taskmcp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// committedQueue is a fixture queue inside a repository with one commit, as a loop box mounts it.
func committedQueue(t *testing.T, ids map[string]string) (string, func(...string)) {
	t.Helper()
	t.Setenv(tasks.TestLeaseAuthorityRootEnv, t.TempDir())
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	return queueIn(t, repo, filepath.Join(".agent", "tasks"), ids), git
}

// tasks_wait hands the assigned task back to todo with the task it waits for named, and tasks_list
// shows the wait. Every other use is refused with nothing changed, including one where Coop cannot
// read the history it checks for the task's commit.
func TestWaitReturnsTheAssignedTaskToTodo(t *testing.T) {
	unreadable := queue(t, map[string]string{"t1": tasks.StateInProgress, "t2": tasks.StateTodo})
	if text := newSession(t, newServer(t, unreadable, "t1")).mustRefuse("tasks_wait", map[string]any{"id": "t1", "on": "t2", "reason": "order"}); !strings.Contains(text, "could not read the history") {
		t.Fatalf("wait outside a repository = %s", text)
	}
	root, _ := committedQueue(t, map[string]string{"t1": tasks.StateInProgress, "t2": tasks.StateTodo, "t3": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	for _, refused := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"id": "t3", "on": "t2", "reason": "order"}, "only the task assigned to this run can wait"},
		{map[string]any{"id": "t1", "on": "nope", "reason": "order"}, "no task nope is in this loop's queues"},
		{map[string]any{"id": "t1", "on": "t2"}, "missing required fields: reason"},
	} {
		if text := sess.mustRefuse("tasks_wait", refused.args); !strings.Contains(text, refused.want) {
			t.Errorf("tasks_wait %v = %s, want %q", refused.args, text, refused.want)
		}
	}
	if item, _, _ := tasks.CurrentTask(root, "t1"); item.State != tasks.StateInProgress || item.WaitsOn != "" {
		t.Fatalf("refusals changed the task: %+v", item)
	}

	if text := sess.mustCall("tasks_wait", map[string]any{"id": "t1", "on": "t2", "reason": "the docs must not land before the fix"}); !strings.Contains(text, "waits for t2") {
		t.Fatalf("tasks_wait = %s", text)
	}
	item, ok, err := tasks.CurrentTask(root, "t1")
	if err != nil || !ok || item.State != tasks.StateTodo || item.WaitsOn != "t2" {
		t.Fatalf("waiting task = %+v, %v, %v", item, ok, err)
	}
	if listed := sess.mustCall("tasks_list", map[string]any{"state": "todo"}); !strings.Contains(listed, `"waits_on": "t2"`) {
		t.Fatalf("tasks_list = %s", listed)
	}
}

// The loop judges what was committed after a wait from the commit tasks_wait started from, so the
// tool records it only for a wait it accepted, and a base it cannot read refuses the wait.
func TestWaitRecordsTheCommitItStartsFrom(t *testing.T) {
	root, _ := committedQueue(t, map[string]string{"t1": tasks.StateInProgress, "t2": tasks.StateTodo})
	repo := filepath.Dir(filepath.Dir(root))
	serve := func(base func() (string, error)) *Server {
		s, err := New(Authority{Workspace: repo, QueueRoots: []string{root}, Assigned: "t1", WaitBase: base,
			Owner: tasks.TaskLeaseOwner{RunID: "test-run", PID: os.Getpid(), Provider: "test", Target: "test"}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	unreadable := serve(func() (string, error) { return "", errors.New("HEAD is gone") })
	if text := newSession(t, unreadable).mustRefuse("tasks_wait", map[string]any{"id": "t1", "on": "t2", "reason": "order"}); !strings.Contains(text, "HEAD is gone") {
		t.Fatalf("wait from an unreadable base = %s", text)
	}
	if item, _, _ := tasks.CurrentTask(root, "t1"); item.State != tasks.StateInProgress || unreadable.AssignedWaitBase() != "" {
		t.Fatalf("a refused wait moved the task or kept a base: %+v, %q", item, unreadable.AssignedWaitBase())
	}
	head := forkspace.GitOut(repo, "rev-parse", "HEAD")
	s := serve(func() (string, error) { return head, nil })
	newSession(t, s).mustCall("tasks_wait", map[string]any{"id": "t1", "on": "t2", "reason": "order"})
	if got := s.AssignedWaitBase(); head == "" || got != head {
		t.Fatalf("recorded wait base = %q, want %q", got, head)
	}
}

// A fork's projection holds one task, and a task that already has its commit completes with it, so
// neither may wait.
func TestWaitRefusesAForkTaskAndACommittedTask(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress, "t2": tasks.StateTodo})
	fork, err := New(Authority{Workspace: filepath.Dir(filepath.Dir(root)), QueueRoots: []string{root}, Assigned: "t1", ProposalOutbox: outboxIn(t, root)})
	if err != nil {
		t.Fatal(err)
	}
	if text := newSession(t, fork).mustRefuse("tasks_wait", map[string]any{"id": "t1", "on": "t2", "reason": "order"}); !strings.Contains(text, "a fork works one task") {
		t.Fatalf("fork wait = %s", text)
	}

	committed, git := committedQueue(t, map[string]string{"t1": tasks.StateInProgress, "t2": tasks.StateTodo})
	repo := filepath.Dir(filepath.Dir(committed))
	if err := os.WriteFile(filepath.Join(repo, "half.txt"), []byte("safe half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "half.txt")
	git("commit", "-qm", "Land the safe half\n\nCoop-Task: t1")
	if text := newSession(t, newServer(t, committed, "t1")).mustRefuse("tasks_wait", map[string]any{"id": "t1", "on": "t2", "reason": "order"}); !strings.Contains(text, "already has a commit with its Coop-Task trailer") {
		t.Fatalf("committed wait = %s", text)
	}
	if item, _, _ := tasks.CurrentTask(committed, "t1"); item.State != tasks.StateInProgress {
		t.Fatalf("committed task moved: %+v", item)
	}
}
