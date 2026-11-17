package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A box can swap a checked projection folder for a FIFO before the walk opens it. The walk then
// fails at once instead of waiting on the FIFO while it holds the host's locks.
func TestProjectionWalkDoesNotWaitOnASwappedInFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan error, 1)
	go func() {
		done <- walkOpenedProjectionRoot(root, func(rel string, _ *os.Root, _ string, _ os.FileInfo) error {
			if rel != "artifacts" {
				return nil
			}
			path := filepath.Join(dir, "artifacts")
			if err := os.Remove(path); err != nil {
				return err
			}
			return syscall.Mkfifo(path, 0o644)
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("walk opened a FIFO as a folder")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("walk waited on the swapped-in FIFO")
	}
}

// A projection holding a top-level name the sync does not carry is refused before anything is
// written, so the canonical folder is never left half-updated.
func TestSyncProjectionTreeRefusesBeforeWriting(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{
		"artifacts/report.txt": "new evidence\n",
		"decision.md":          "# Decision\n",
		"gate.log":             "a stray log\n",
	} {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shown := filepath.Join("/fork", "tasks", StateInProgress, "task")
	err := syncProjectionTree(src, dst, shown, false)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(shown, "gate.log")) ||
		!strings.Contains(err.Error(), "move it into artifacts/ or delete it") {
		t.Fatalf("syncProjectionTree = %v, want gate.log refused by its full path with the fix", err)
	}
	if entries, err := os.ReadDir(dst); err != nil || len(entries) != 0 {
		t.Fatalf("refused sync wrote into the canonical folder: %v, %v", entries, err)
	}
}

// A file outside the task-folder names stops a task both ways with the same rule, by its full path
// and with the fix: a project folder holding gate.log is not sent to the fork, and a fork copy holding
// one is not handed back. Once it moves into artifacts/, the task goes and comes back.
func TestAStrayTopLevelFileStopsAForkTaskBothWaysUntilMoved(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent", "tasks")
	item := taskWithCompletedChecklist(t, root, StateTodo, "stray")
	writeTaskFile(t, filepath.Join(item.Dir, "gate.log"), "a stray log\n")
	authorityRepo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, authorityRepo, "a")
	request := ForkAssignmentRequest{
		AuthorityRepo: authorityRepo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	}
	_, err := AssignForkTask([]string{root}, request)
	canonical, _ := mustCurrentTask(t, root, "stray")
	if err == nil || !strings.Contains(err.Error(), filepath.Join(canonical.Dir, "gate.log")) ||
		!strings.Contains(err.Error(), "move it into artifacts/ or delete it") {
		t.Fatalf("assigning a task holding gate.log = %v, want a refusal naming %s and the fix", err, filepath.Join(canonical.Dir, "gate.log"))
	}
	moveIntoArtifacts := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "gate.log"), filepath.Join(dir, "artifacts", "gate.log")); err != nil {
			t.Fatal(err)
		}
	}
	moveIntoArtifacts(canonical.Dir)
	assignment, err := AssignForkTask([]string{root}, request)
	if err != nil || assignment.Outcome != ForkAssignmentSelected {
		t.Fatalf("assigning after the move = %+v, %v", assignment, err)
	}
	defer assignment.Lease.Release()
	projected := mustReadTaskTree(t, assignment.Owner.Projection)
	if len(projected) != 1 {
		t.Fatalf("projection = %+v, want the one task", projected)
	}
	writeTaskFile(t, filepath.Join(projected[0].Dir, "gate.log"), "the fork's own log\n")
	if err := MoveTaskDir(assignment.Owner.Projection, projected[0], StateDone); err != nil {
		t.Fatal(err)
	}
	done := filepath.Join(assignment.Owner.Projection, StateDone, "stray")
	_, err = AcceptForkProjection(authorityRepo, root, "stray", assignment.Owner)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(done, "gate.log")) ||
		!strings.Contains(err.Error(), "move it into artifacts/ or delete it") {
		t.Fatalf("hand-back of a done copy holding gate.log = %v, want a refusal naming %s and the fix", err, filepath.Join(done, "gate.log"))
	}
	if _, err := os.Stat(filepath.Join(canonical.Dir, "gate.log")); !os.IsNotExist(err) {
		t.Fatalf("refused hand-back wrote gate.log into the project's folder: %v", err)
	}
	moveIntoArtifacts(done)
	if _, err := AcceptForkProjection(authorityRepo, root, "stray", assignment.Owner); err != nil {
		t.Fatalf("hand-back after the move = %v", err)
	}
	if got := readFileString(filepath.Join(canonical.Dir, "artifacts", "gate.log")); got != "the fork's own log\n" {
		t.Fatalf("handed-back artifacts/gate.log = %q", got)
	}
}
