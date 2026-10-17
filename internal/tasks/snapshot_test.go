package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestProjectSnapshotIncludesExternalCanonicalQueueAndExactSandbox(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	external := filepath.Join(t.TempDir(), "external-tasks")
	taskForLease(t, external, StateTodo, "outside")
	workspace, identity := testAssignmentFork(t, repo, "worker")
	assignment, err := AssignForkTask([]string{external}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	ownerRecord, owned, err := ReadTaskOwnerRecord(external, assignment.Task.Item.ID)
	if err != nil || !owned || ownerRecord.Task == nil {
		t.Fatalf("assigned owner = %+v, owned=%v err=%v", ownerRecord, owned, err)
	}
	reservation := forkspace.WorkspaceReservation{
		Version: forkspace.WorkspaceReservationVersion, Fork: identity,
		Kind: forkspace.WorkspaceReservationRemoteSession, OwnerStoreID: "store_snapshot", OwnerID: "session_snapshot", CreatedAt: testLeaseOwner().Now(),
	}
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	err = forkspace.ReserveWorkspaceLocked(repo, reservation)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	record, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionRemoteSession, Role: forkspace.ExecutionRoleController,
		Workspace: workspace, Fork: &identity, SourceID: "snapshot-run", ReservationOwner: "session_snapshot", ReservationStoreID: "store_snapshot",
		Task: &forkspace.ExecutionTaskRef{
			QueueID: ownerRecord.Task.Ref.QueueID, TaskID: ownerRecord.Task.Ref.TaskID,
			ID: assignment.Task.Item.ID, Assignment: assignment.Owner.AssignmentID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, record) })

	snapshot := ReadProjectSnapshot(repo, nil)
	if len(snapshot.Queues) != 1 || snapshot.Queues[0].Root != external || snapshot.Queues[0].ID == "" {
		t.Fatalf("external queue was not discovered from the reverse index: %+v", snapshot.Queues)
	}
	if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Fork == nil || *snapshot.Tasks[0].Fork != identity ||
		snapshot.Tasks[0].AssignmentID != assignment.Owner.AssignmentID || len(snapshot.Tasks[0].Executions) != 1 ||
		snapshot.Tasks[0].QueueLabel != external || snapshot.Tasks[0].QueueID != snapshot.Queues[0].ID {
		t.Fatalf("canonical task snapshot = %+v", snapshot.Tasks)
	}
	if snapshot.ActiveExecutions() != 1 || len(snapshot.Forks) != 1 || snapshot.Forks[0].Reservation == nil ||
		snapshot.Forks[0].Assignments != 1 || snapshot.Forks[0].ActiveExecutions != 1 || !snapshot.Forks[0].WorkspaceValid {
		t.Fatalf("fork snapshot = %+v, active=%d, problems=%v", snapshot.Forks, snapshot.ActiveExecutions(), snapshot.Problems)
	}
	if len(snapshot.Problems) != 0 {
		t.Fatalf("healthy snapshot problems = %v", snapshot.Problems)
	}
}

// A fork that took its task while COOP_REPO reached the project through a link above it recorded
// the queue in that spelling, and a fork made after the update records the real one. The snapshot
// behind coop tasks watch and coop fork ls still reads that queue once, lists each task once and
// finds both owners in their reverse index.
func TestProjectSnapshotKeysQueuesByFolder(t *testing.T) {
	top, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, link := filepath.Join(top, "code", "project"), filepath.Join(top, "link")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(repo), link); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(link, "project")
	root := filepath.Join(repo, ".agent", "tasks")
	taskForLease(t, root, StateTodo, "before")
	taskForLease(t, root, StateTodo, "after")
	forks := map[string]forkspace.Identity{}
	for _, assign := range []struct{ repo, fork string }{{linked, "old"}, {repo, "new"}} {
		workspace, identity := testAssignmentFork(t, assign.repo, assign.fork)
		assignment, err := AssignForkTask([]string{filepath.Join(assign.repo, ".agent", "tasks")}, ForkAssignmentRequest{
			AuthorityRepo: assign.repo, Fork: identity, WorkspaceRoot: workspace,
			BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
		})
		if err != nil || assignment.Outcome != ForkAssignmentSelected {
			t.Fatalf("assign %s = %+v, %v", assign.fork, assignment, err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		forks[assignment.Task.Item.ID] = identity
	}
	// coop fork ls passes no queue, coop tasks watch the configured one.
	for _, roots := range [][]string{nil, {root}} {
		snapshot := ReadProjectSnapshot(repo, roots)
		if len(snapshot.Queues) != 1 || len(snapshot.Tasks) != 2 || len(snapshot.Problems) != 0 {
			t.Fatalf("snapshot with queues %v: queues %+v, %d tasks, problems %v", roots, snapshot.Queues, len(snapshot.Tasks), snapshot.Problems)
		}
		if roots != nil && snapshot.Queues[0].Root != root {
			t.Errorf("queue is named %s, want the configured %s", snapshot.Queues[0].Root, root)
		}
		for _, task := range snapshot.Tasks {
			if task.Fork == nil || *task.Fork != forks[task.ID] {
				t.Errorf("task %s owner = %v, want fork %v", task.ID, task.Fork, forks[task.ID])
			}
		}
	}
}

func TestProjectSnapshotKeepsStaleExecutionVisibleWithoutCallingItActive(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	workspace := t.TempDir()
	record, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionInteractive, Role: forkspace.ExecutionRoleWarm, Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, record) })
	snapshot := ReadProjectSnapshot(repo, nil)
	if len(snapshot.Executions) != 1 || !snapshot.Executions[0].Running || snapshot.Executions[0].Active || snapshot.ActiveExecutions() != 0 {
		t.Fatalf("parked execution snapshot = %+v", snapshot.Executions)
	}
}

func TestProjectSnapshotReportsReplacedGenerationWorkspace(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "replaced")
	old := workspace + "-old"
	if err := os.Rename(workspace, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := ReadProjectSnapshot(repo, nil)
	var replaced *ProjectForkSnapshot
	for index := range snapshot.Forks {
		if snapshot.Forks[index].Identity != nil && *snapshot.Forks[index].Identity == identity {
			replaced = &snapshot.Forks[index]
		}
	}
	if replaced == nil || replaced.WorkspaceValid {
		t.Fatalf("replaced fork snapshot = %+v", snapshot.Forks)
	}
	if len(snapshot.Problems) == 0 || !strings.Contains(strings.Join(snapshot.Problems, "\n"), "no longer matches") {
		t.Fatalf("replacement problem = %v", snapshot.Problems)
	}
}

func TestProjectSnapshotReportsQueueEntriesTheTaskReaderCannotTrust(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(repo, TasksRoot)
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, StateTodo, "redirected")); err != nil {
		t.Fatal(err)
	}
	snapshot := ReadProjectSnapshot(repo, []string{root})
	if len(snapshot.Problems) == 0 || !strings.Contains(strings.Join(snapshot.Problems, "\n"), "is not a real directory") {
		t.Fatalf("untrusted queue entry disappeared from snapshot: %v", snapshot.Problems)
	}
}
