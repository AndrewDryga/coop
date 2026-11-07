package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// killedReviewFork is a fork whose worker was killed with its task in the projection's done folder, and
// coop fork stop has paused the assignment. The completion window the worker held is left in the
// journal. In its final review that window archived the folder in its baseline; in its work stage it
// began before the folder was done.
type killedReviewFork struct {
	repo, root, queue string        // the project, the canonical queue the task came from, the fork's copy of it
	owner             ForkTaskOwner // the resumed assignment, as the next start of the loop takes it
}

func newKilledReviewFork(t *testing.T, root, id string) killedReviewFork {
	return newKilledFork(t, root, id, false)
}

func newKilledFork(t *testing.T, root, id string, inWorkStage bool) killedReviewFork {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "project")
	taskWithCompletedChecklist(t, root, StateTodo, id)
	workspace, identity := testAssignmentFork(t, repo, "killed-review-worker")
	request := ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("e", 40), LeaseOwner: testLeaseOwner(),
	}
	assignment, err := AssignForkTask([]string{root}, request)
	if err != nil || assignment.Outcome != ForkAssignmentSelected {
		t.Fatalf("assign = %+v, %v", assignment, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	queue := assignment.Owner.Projection
	var window *CompletionWindowSet
	if inWorkStage {
		window, err = BeginWorkCompletionWindows([]string{queue}, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	projected, _ := mustCurrentTask(t, queue, id)
	if err := MoveTaskDir(queue, projected, StateDone); err != nil {
		t.Fatal(err)
	}
	if !inWorkStage {
		window, err = BeginReviewCompletionWindows([]string{queue}, []string{id})
		if err != nil {
			t.Fatal(err)
		}
	}
	// kill -9: the process is gone with its locks, and the journal keeps the record.
	if err := window.Abandon(); err != nil {
		t.Fatal(err)
	}
	if err := PauseForkAssignments(repo, identity); err != nil {
		t.Fatal(err)
	}
	resumed, err := AssignForkTask([]string{root}, request)
	if err != nil || resumed.Outcome != ForkAssignmentSelected || resumed.Owner.AssignmentID != assignment.Owner.AssignmentID {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	if err := resumed.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	return killedReviewFork{repo: repo, root: root, queue: queue, owner: resumed.Owner}
}

// windows is how many completion windows the journal holds for the fork's copy of the queue.
func (f killedReviewFork) windows(t *testing.T) int {
	t.Helper()
	index, err := ReadCompletionWindowIndex(f.queue)
	if err != nil {
		t.Fatal(err)
	}
	return len(index.Windows)
}

// The first resume after a worker was killed in its final review used to end in "completion ownership
// changed before recovery": the resume moved the done folder back to in progress, and the loop's
// recovery then found a window that archived it and a folder that was no longer there. The restore now
// settles the window first, so the loop's own recovery finds nothing and the resume goes on. The
// canonical queue is no part of it: a queue outside the project behaves as the project's own.
func TestPrepareForkProjectionSettlesTheWindowAKilledFinalReviewLeft(t *testing.T) {
	for name, queue := range map[string]func(t *testing.T) string{
		"the project's own queue":     func(t *testing.T) string { return filepath.Join(t.TempDir(), "project", ".agent", "tasks") },
		"a queue outside the project": func(t *testing.T) string { return filepath.Join(t.TempDir(), "queue") },
	} {
		t.Run(name, func(t *testing.T) {
			fork := newKilledReviewFork(t, queue(t), "killed-review")
			if got := fork.windows(t); got != 1 {
				t.Fatalf("the killed review left %d windows in the journal, want 1", got)
			}
			if err := PrepareForkProjectionForRun(fork.repo, fork.root, "killed-review", fork.owner); err != nil {
				t.Fatalf("resume prepare = %v", err)
			}
			// What loop.Run does first, on the folder the restore left.
			if err := ReconcileInterruptedCompletions([]string{fork.queue}); err != nil {
				t.Fatalf("loop start, interrupted completions = %v", err)
			}
			if _, err := ReconcileCompletionWindowsWithActivity([]string{fork.queue}); err != nil {
				t.Fatalf("loop start, completion windows = %v; the first resume after a kill must not fail here", err)
			}
			if current, ok := mustCurrentTask(t, fork.queue, "killed-review"); !ok || current.State != StateInProgress {
				t.Fatalf("the fork's copy is %+v, want it back in progress for a fresh attempt", current)
			}
			if got := fork.windows(t); got != 0 {
				t.Fatalf("%d windows remain after the resume settled them", got)
			}
		})
	}
}

// A resume that does not own what it restores settles nothing: it is refused before any journal is
// touched, so the window stays for whoever does, and the folder stays done. Both ways to be wrong are
// tried: another task of this assignment's fork, and an assignment the fork does not hold.
func TestPrepareForkProjectionNeverSettlesTheJournalOfAnotherOwner(t *testing.T) {
	fork := newKilledReviewFork(t, filepath.Join(t.TempDir(), "tasks"), "not-yours")
	foreign := fork.owner
	foreign.AssignmentID = strings.Repeat("0", len(foreign.AssignmentID))
	for name, attempt := range map[string]func() error{
		"another task":       func() error { return PrepareForkProjectionForRun(fork.repo, fork.root, "someone-elses", fork.owner) },
		"another assignment": func() error { return PrepareForkProjectionForRun(fork.repo, fork.root, "not-yours", foreign) },
	} {
		if err := attempt(); err == nil {
			t.Fatalf("a resume for %s was accepted", name)
		}
		if got := fork.windows(t); got != 1 {
			t.Fatalf("a refused resume for %s left %d windows, want the killed review's one untouched", name, got)
		}
		if current, _ := mustCurrentTask(t, fork.queue, "not-yours"); current.State != StateDone {
			t.Fatalf("a refused resume for %s moved the folder to %s", name, current.State)
		}
	}
}

// A window somebody still holds belongs to its holder: the resume settles the dead one beside it and
// leaves that one in the journal. (A live loop and a restore of the same copy do not happen together;
// this is the net under that.) The holder's own close still finds its subject gone from done.
func TestPrepareForkProjectionLeavesAWindowItsHolderStillHoldsAlone(t *testing.T) {
	fork := newKilledReviewFork(t, filepath.Join(t.TempDir(), "tasks"), "held")
	held, err := BeginReviewCompletionWindows([]string{fork.queue}, []string{"held"})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if got := fork.windows(t); got != 2 {
		t.Fatalf("the journal holds %d windows, want the killed review's and the holder's", got)
	}
	if err := PrepareForkProjectionForRun(fork.repo, fork.root, "held", fork.owner); err != nil {
		t.Fatalf("prepare = %v", err)
	}
	if got := fork.windows(t); got != 1 {
		t.Fatalf("the journal holds %d windows, want only the holder's", got)
	}
	if _, err := held.FinishReview(); err == nil {
		t.Fatal("the holder's review closed cleanly although its subject left done under it")
	}
}

// A change the journal cannot explain still stops the resume, and the folder stays as it was. Here an
// archived task that the killed review's window recorded is no longer in the queue, which no restore
// of this folder accounts for.
func TestPrepareForkProjectionStopsOnAWindowChangeItCannotExplain(t *testing.T) {
	fork := newKilledReviewFork(t, filepath.Join(t.TempDir(), "tasks"), "explained")
	taskForLease(t, fork.queue, StateDone, "ghost")
	window, err := BeginReviewCompletionWindows([]string{fork.queue}, []string{"explained"})
	if err != nil {
		t.Fatal(err)
	}
	if err := window.Abandon(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(fork.queue, StateDone, "ghost")); err != nil {
		t.Fatal(err)
	}
	err = PrepareForkProjectionForRun(fork.repo, fork.root, "explained", fork.owner)
	if err == nil || !strings.Contains(err.Error(), "recover interrupted completion window") || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("prepare over an unexplained departure = %v, want the recovery's refusal naming the task", err)
	}
	if current, _ := mustCurrentTask(t, fork.queue, "explained"); current.State != StateDone {
		t.Fatalf("a refused resume moved the folder to %s", current.State)
	}
}

// A worker killed in its work stage, after the box moved its task to done by hand and before the host
// accepted it, leaves a work window whose recovery restores that unreceipted folder itself. The restore
// then has nothing left to move, and the resume goes on instead of failing on a missing folder.
func TestPrepareForkProjectionAcceptsAFolderTheRecoveryAlreadyReopened(t *testing.T) {
	fork := newKilledFork(t, filepath.Join(t.TempDir(), "tasks"), "moved-by-hand", true)
	if err := PrepareForkProjectionForRun(fork.repo, fork.root, "moved-by-hand", fork.owner); err != nil {
		t.Fatalf("prepare over a folder the recovery reopened = %v", err)
	}
	if current, ok := mustCurrentTask(t, fork.queue, "moved-by-hand"); !ok || current.State != StateInProgress {
		t.Fatalf("the fork's copy is %+v, want it in progress", current)
	}
	if got := fork.windows(t); got != 0 {
		t.Fatalf("%d windows remain after the resume settled them", got)
	}
}
