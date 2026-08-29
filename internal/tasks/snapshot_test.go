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
		Kind: forkspace.WorkspaceReservationRemoteSession, OwnerID: "session_snapshot", CreatedAt: testLeaseOwner().Now(),
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
		Workspace: workspace, Fork: &identity, SourceID: "snapshot-run", ReservationOwner: "session_snapshot",
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
	if len(snapshot.Problems) == 0 || !strings.Contains(strings.Join(snapshot.Problems, "\n"), "task entry is not a real directory") {
		t.Fatalf("untrusted queue entry disappeared from snapshot: %v", snapshot.Problems)
	}
}
