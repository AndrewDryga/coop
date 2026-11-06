package tasks

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

// A loop run's note tells the project snapshot two things about the task the run let go of, each only
// while it is true: a person stopped the run, or the run waits out a usage limit. A later run in the
// queue replaces the note, so an old stop never explains what came after it, a crash included.
func TestLoopNoteSaysAStopOrAWaitOnlyWhileItIsTrue(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(repo, ".agent", "tasks")
	task := taskForLease(t, root, StateInProgress, "wire-auth")
	hold := func(runID string) *TaskLease {
		t.Helper()
		owner := testLeaseOwner()
		owner.RunID = runID
		lease, _, err := TryTaskLease(root, task, owner)
		if err != nil || lease == nil {
			t.Fatalf("lease = %v, %v", lease, err)
		}
		return lease
	}
	read := func() *ProjectTaskLoop {
		t.Helper()
		snapshot := ReadProjectSnapshot(repo, []string{root})
		if len(snapshot.Tasks) != 1 || len(snapshot.Problems) != 0 {
			t.Fatalf("snapshot = %+v", snapshot)
		}
		return snapshot.Tasks[0].Loop
	}

	note := NoteLoopRun([]string{root}, "run-a")
	lease := hold("run-a")
	note.WaitingForLimit(time.Now().Add(time.Hour))
	if got := read(); got != nil {
		t.Errorf("a held task: loop = %+v, want nothing", got)
	}
	note.WaitingForLimit(time.Time{})
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != nil {
		t.Errorf("a run that says nothing: loop = %+v", got)
	}

	until := time.Now().Add(12 * time.Minute).Truncate(time.Second)
	waitOver := note.WaitingForLimit(until)
	if got := read(); got == nil || !got.UsageLimitUntil.Equal(until) || !got.StoppedAt.IsZero() {
		t.Errorf("waiting: loop = %+v, want the limit until %v", got, until)
	}
	waitOver()
	if got := read(); got != nil {
		t.Errorf("after the wait: loop = %+v", got)
	}
	note.WaitingForLimit(time.Now().Add(-time.Minute))
	if got := read(); got != nil {
		t.Errorf("a wait whose end has passed: loop = %+v", got)
	}
	start := note.record.Start
	note.record.Start = starttoken.Stale() // the run died while it waited
	note.WaitingForLimit(until)
	if got := read(); got != nil {
		t.Errorf("a gone run's wait: loop = %+v", got)
	}
	note.record.Start = start

	note.Stopped()
	if got := read(); got == nil || got.StoppedAt.IsZero() || !got.UsageLimitUntil.IsZero() {
		t.Fatalf("stopped: loop = %+v", got)
	}
	data, err := json.Marshal(ReadProjectSnapshot(repo, []string{root}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"loop":{"stopped_at":"`) {
		t.Errorf("the JSON does not carry the stop: %s", data)
	}

	// The next run started, then crashed before it took the task, or while it held it.
	NoteLoopRun([]string{root}, "run-b")
	if got := read(); got != nil {
		t.Errorf("a later run started: loop = %+v, want the stop forgotten", got)
	}
	crashed := hold("run-b")
	crashed.Quiesce()
	if err := unlockLeaseFile(crashed.authority); err != nil {
		t.Fatal(err)
	}
	crashed.authority = nil
	if got := read(); got != nil {
		t.Errorf("a later run crashed holding the task: loop = %+v, want no stop", got)
	}
	// A run waits on a limit for its own task, not for one a crashed run left.
	NoteLoopRun([]string{root}, "run-c").WaitingForLimit(until)
	if got := read(); got != nil {
		t.Errorf("another run's wait: loop = %+v", got)
	}
}

// A task whose box waits for the service launch lock reads so on the board and in the JSON, only
// while the wait lasts: the board says so from the box's own record, never from its narration.
func TestTaskWatchSaysWhileATasksBoxWaitsToStartItsServices(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(repo, ".agent", "tasks")
	task := taskForLease(t, root, StateInProgress, "wire-auth")
	lease, _, err := TryTaskLease(root, task, testLeaseOwner())
	if err != nil || lease == nil {
		t.Fatalf("lease = %v, %v", lease, err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	record, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionLocalLoop, Workspace: repo, Task: &forkspace.ExecutionTaskRef{ID: task.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, record) })
	board := func() string {
		t.Helper()
		return captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, []string{root}) })
	}
	const waiting = "waiting to start its services"
	if out := board(); strings.Contains(out, waiting) || !strings.Contains(out, "working") {
		t.Errorf("before the wait:\n%s", out)
	}
	waitOver := forkspace.NoteServiceLaunchWaiting(repo, record.ID)
	if out := board(); !strings.Contains(out, " wire-auth\n    codex:gpt-test@work · "+waiting+"\n") || !strings.HasPrefix(out, "0 todo · 1 working · 0 stopped") {
		t.Errorf("while the box waits:\n%s", out)
	}
	data, err := json.Marshal(ReadProjectSnapshot(repo, []string{root}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"WaitingForServices":true`) {
		t.Errorf("the JSON does not carry the wait: %s", data)
	}
	waitOver()
	if out := board(); strings.Contains(out, waiting) {
		t.Errorf("after the wait:\n%s", out)
	}
}

// A fork's task reads what the fork's loop noted on its own copy of the task, never a note on the
// project's queue, and a task a person claimed reads none.
func TestTheSnapshotReadsTheNoteOfTheLoopThatHeldTheTask(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(repo, ".agent", "tasks")
	taskWithCompletedChecklist(t, root, StateTodo, "forked")
	workspace, identity := testAssignmentFork(t, repo, "sweep-a")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace, BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := MaterializeForkProjection(assignment); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil { // the project's queue now holds the fork's run
		t.Fatal(err)
	}
	NoteLoopRun([]string{root}, testLeaseOwner().RunID).Stopped()
	projection := assignment.Owner.Projection
	projected := mustReadTaskTree(t, projection)
	owner := testLeaseOwner()
	owner.RunID = "fork-run"
	note := NoteLoopRun([]string{projection}, owner.RunID)
	lease, _, err := TryTaskLease(projection, projected[0], owner)
	if err != nil || lease == nil {
		t.Fatalf("lease = %v, %v", lease, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	loop := func(id string) *ProjectTaskLoop {
		t.Helper()
		for _, task := range ReadProjectSnapshot(repo, []string{root}).Tasks {
			if task.ID == id {
				return task.Loop
			}
		}
		t.Fatalf("no task %s", id)
		return nil
	}
	if got := loop("forked"); got != nil {
		t.Errorf("the project's queue note reached the fork's task: %+v", got)
	}
	note.Stopped()
	if got := loop("forked"); got == nil || got.StoppedAt.IsZero() {
		t.Errorf("the fork's loop was stopped: loop = %+v", got)
	}

	// The stopped run on the project's queue last held this task, then a person claimed it.
	held, _, err := TryTaskLease(root, taskForLease(t, root, StateTodo, "claimed"), testLeaseOwner())
	if err != nil || held == nil {
		t.Fatalf("lease = %v, %v", held, err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderMoveWith(root, []string{"claimed"}, StateInProgress, "claim", "claimed", claimOptions{actor: selfActor(t, "codex")}); code != 0 || err != nil {
		t.Fatalf("claim = %d, %v", code, err)
	}
	if got := loop("claimed"); got != nil {
		t.Errorf("a claimed task: loop = %+v", got)
	}
}
