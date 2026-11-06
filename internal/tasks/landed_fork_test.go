package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// pausedForkTask is a task fork sweep-a holds after a pass that stopped short of review, as a box
// that keeps failing a check leaves it: the canonical task is in progress and the assignment paused.
// The project is a Git repository with one commit, then one more for each message in before,
// committed before the fork takes the task.
func pausedForkTask(t *testing.T, id string, before ...string) (string, string, ForkAssignment) {
	t.Helper()
	repo := initRepo(t)
	for _, message := range before {
		git(t, repo, "commit", "-q", "--allow-empty", "-m", message)
	}
	root := filepath.Join(repo, TasksRoot)
	taskWithCompletedChecklist(t, root, StateTodo, id)
	workspace, identity := testAssignmentFork(t, repo, "sweep-a")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: forkspace.GitOut(repo, "rev-parse", "HEAD"), LeaseOwner: testLeaseOwner(),
	})
	if err != nil || assignment.Outcome != ForkAssignmentSelected {
		t.Fatalf("assign fixture: %+v, %v", assignment, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, id, assignment.Owner); err != nil {
		t.Fatal(err)
	}
	if record, _, err := ReadTaskOwnerRecord(root, id); err != nil || record.Fork == nil || record.Fork.Phase != ForkAssignmentPaused {
		t.Fatalf("fixture owner = %+v, %v; want a paused fork assignment", record, err)
	}
	return repo, root, assignment
}

// heldByFork fails the test unless the fork still holds id exactly as it did.
func heldByFork(t *testing.T, repo, root, id string, assignment ForkAssignment) {
	t.Helper()
	record, owned, err := ReadTaskOwnerRecord(root, id)
	if err != nil || !owned || record.Fork == nil || !sameForkAssignment(*record.Fork, assignment.Owner) {
		t.Fatalf("owner after a refusal = %+v, %v, %v; want the fork's assignment", record, owned, err)
	}
	if indexes, problems := IndexedForkAssignments(repo, assignment.Owner.Fork); len(problems) != 0 || len(indexes) != 1 {
		t.Fatalf("assignment index after a refusal = %+v, %v", indexes, problems)
	}
	if current, _ := mustCurrentTask(t, root, id); current.State != StateInProgress {
		t.Fatalf("refused task moved to %s", current.State)
	}
}

// A person completes a fork's task whose commit they already brought to the project's branch: the
// task is done, the fork no longer holds it, its log says where the work landed, the task the fork
// had found is added to the queue, and the fork itself is kept.
func TestDoneFromForkCompletesATaskWhoseCommitLanded(t *testing.T) {
	forceAtTerminal(t)
	const id = "fix-login-retries"
	repo, root, assignment := pausedForkTask(t, id)
	proposal := testForkProposal(strings.Repeat("b", 32), "Fix the retry counter")
	writeForkProposal(t, assignment.Owner, proposal)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "Fix login retries\n\nCoop-Task: "+id)
	commit := forkspace.GitOut(repo, "rev-parse", "HEAD")

	out := captureStderr(t, func() {
		if code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"}); code != 0 || err != nil {
			t.Fatalf("done --from-fork = %d, %v", code, err)
		}
	})
	for _, want := range []string{"Added discovered task:", "Completed task:", "Commit " + commit[:12], "fork sweep-a no longer holds it"} {
		if !strings.Contains(out, want) {
			t.Errorf("output omitted %q:\n%s", want, out)
		}
	}
	done, _ := mustCurrentTask(t, root, id)
	if done.State != StateDone || !taskCompletionRecorded(root, done) {
		t.Fatalf("task after done --from-fork = %s, receipt %v", done.State, taskCompletionRecorded(root, done))
	}
	if _, owned, err := ReadTaskOwnerRecord(root, id); err != nil || owned {
		t.Fatalf("owner after done --from-fork = %v, %v; want none", owned, err)
	}
	if log := readFileString(filepath.Join(done.Dir, "log.md")); !strings.Contains(log, "Completed with coop tasks done --from-fork: commit "+commit[:12]) ||
		!strings.Contains(log, "fork sweep-a no longer holds this task") {
		t.Fatalf("log.md does not record where the work landed:\n%s", log)
	}
	summary, err := ReadForkTaskStateSummary(repo, assignment.Owner.Fork)
	if err != nil || summary.Active() {
		t.Fatalf("fork task state after release = %+v, %v; want none", summary, err)
	}
	if _, exists, err := forkspace.ReadGeneration(repo, "sweep-a"); err != nil || !exists {
		t.Fatalf("the fork was not kept: %v, %v", exists, err)
	}
	discovered := 0
	for _, item := range mustReadTaskTree(t, root) {
		if item.State == StateTodo {
			discovered++
		}
	}
	if discovered != 1 {
		t.Fatalf("todo tasks after release = %d, want the one the fork found", discovered)
	}
	if again, err := ImportForkProposals(repo, assignment.Owner.Fork); err != nil || len(again) != 0 {
		t.Fatalf("a later import by the fork = %+v, %v; want nothing left to import", again, err)
	}
}

// Without a commit carrying its trailer on the branch the fork keeps the task, and the plain done
// refusal names both ways to finish it.
func TestDoneFromForkRefusesWithoutALandedCommit(t *testing.T) {
	forceAtTerminal(t)
	const id = "unlanded"
	repo, root, assignment := pausedForkTask(t, id)
	code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"})
	var usage *ui.UsageError
	if code == 0 || !errors.As(err, &usage) {
		t.Fatalf("done --from-fork without a landed commit = %d, %v", code, err)
	}
	got := usage.Render(ui.Palette{})
	for _, want := range []string{"Could not complete " + id + " from fork sweep-a", `No commit on this branch carries "Coop-Task: ` + id + `"`, "Bring the fork's commit here first"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal omitted %q:\n%s", want, got)
		}
	}
	heldByFork(t, repo, root, id, assignment)

	// A paused fork has nothing to merge yet, so only --from-fork is offered.
	code, err = CmdTasksFolder(repo, root, []string{"done", id})
	if code == 0 || !errors.Is(err, ErrTaskSandboxOwned) || !errors.As(err, &usage) {
		t.Fatalf("plain done of a fork's task = %d, %v; want the drawn fork refusal", code, err)
	}
	got = usage.Render(ui.Palette{})
	for _, want := range []string{"assigned to fork sweep-a (paused)", "completes when the fork's reviewed work is merged", "coop tasks done " + id + " --from-fork"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain refusal omitted %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "coop fork merge") {
		t.Errorf("plain refusal offered a merge the paused fork cannot make:\n%s", got)
	}
	heldByFork(t, repo, root, id, assignment)

	plain := taskWithCompletedChecklist(t, root, StateInProgress, "no-fork")
	if _, err := CompleteLandedForkTask(repo, root, plain); !errors.As(err, &usage) || !strings.Contains(usage.Cause, "is not assigned to a fork") {
		t.Fatalf("from-fork completion of an unassigned task = %v", err)
	}
}

// A commit the branch already had when the fork took the task, such as an earlier completion of a
// task someone reopened, is not the fork's work landing.
func TestDoneFromForkRefusesACommitTheForkStartedFrom(t *testing.T) {
	forceAtTerminal(t)
	const id = "reopened"
	repo, root, assignment := pausedForkTask(t, id, "First try\n\nCoop-Task: "+id)
	code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"})
	var usage *ui.UsageError
	if code == 0 || !errors.As(err, &usage) || !strings.Contains(usage.Cause, "was already on the branch when fork sweep-a took the task") {
		t.Fatalf("done --from-fork on the fork's own starting commit = %d, %v", code, err)
	}
	heldByFork(t, repo, root, id, assignment)
}

// A fork whose reviewed work is ready to merge completes the task by merging, so --from-fork leaves
// the task to that merge, and the plain refusal offers it.
func TestDoneFromForkLeavesAReadyCandidateToItsMerge(t *testing.T) {
	forceAtTerminal(t)
	const id = "ready"
	repo := initRepo(t)
	root := filepath.Join(repo, TasksRoot)
	taskWithCompletedChecklist(t, root, StateTodo, id)
	workspace, identity := testAssignmentFork(t, repo, "sweep-a")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: forkspace.GitOut(repo, "rev-parse", "HEAD"), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, id)
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, id, assignment.Owner); err != nil {
		t.Fatal(err)
	}
	if _, published, err := reviewAndPublishForkCandidate(t, repo, identity, strings.Repeat("b", 40), strings.Repeat("c", 40)); err != nil || !published {
		t.Fatalf("publish candidate = %v, %v", published, err)
	}
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "Do it\n\nCoop-Task: "+id)
	var usage *ui.UsageError
	for _, args := range [][]string{{"done", id, "--from-fork"}, {"done", id}} {
		code, err := CmdTasksFolder(repo, root, args)
		if code == 0 || !errors.As(err, &usage) || !strings.Contains(usage.Render(ui.Palette{}), "coop fork merge sweep-a") {
			t.Fatalf("coop tasks %v for a ready fork = %d, %v; want the merge offered", args, code, err)
		}
	}
	if record, owned, err := ReadTaskOwnerRecord(root, id); err != nil || !owned || record.Fork.Phase != ForkAssignmentReady {
		t.Fatalf("owner after the refusals = %+v, %v, %v; want the ready assignment", record, owned, err)
	}
}

// The fork must be stopped: a running loop still works on the task. Once it is, the same command
// completes it.
func TestDoneFromForkWaitsForTheForkToStop(t *testing.T) {
	forceAtTerminal(t)
	const id = "running"
	repo, root, assignment := pausedForkTask(t, id)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "Do it\n\nCoop-Task: "+id)
	pid := forkspace.PidPath(repo, "sweep-a")
	if err := os.WriteFile(pid, []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"})
	var usage *ui.UsageError
	if code == 0 || !errors.As(err, &usage) || !strings.Contains(usage.Render(ui.Palette{}), "coop fork stop sweep-a") {
		t.Fatalf("done --from-fork of a running fork's task = %d, %v", code, err)
	}
	heldByFork(t, repo, root, id, assignment)
	if err := os.Remove(pid); err != nil {
		t.Fatal(err)
	}
	if code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"}); code != 0 || err != nil {
		t.Fatalf("done --from-fork once the fork stopped = %d, %v", code, err)
	}
}

// A run interrupted after the move to done leaves the task held by the fork; running the command
// again finishes the release.
func TestDoneFromForkFinishesAnInterruptedRun(t *testing.T) {
	forceAtTerminal(t)
	const id = "interrupted"
	repo, root, assignment := pausedForkTask(t, id)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "Do it\n\nCoop-Task: "+id)
	old := taskTmpCleaner
	t.Cleanup(func() { taskTmpCleaner = old })
	taskTmpCleaner = func(string) error { return errors.New("injected cleanup failure") }
	if code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"}); code == 0 || err == nil {
		t.Fatal("the injected failure did not fail the completion")
	}
	taskTmpCleaner = old
	if record, owned, err := ReadTaskOwnerRecord(root, id); err != nil || !owned || !sameForkAssignment(*record.Fork, assignment.Owner) {
		t.Fatalf("owner after the interrupted run = %+v, %v, %v; want the fork's", record, owned, err)
	}
	if code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"}); code != 0 || err != nil {
		t.Fatalf("rerun of done --from-fork = %d, %v", code, err)
	}
	done, _ := mustCurrentTask(t, root, id)
	if _, owned, _ := ReadTaskOwnerRecord(root, id); done.State != StateDone || owned {
		t.Fatalf("after the rerun the task is %s, owned %v", done.State, owned)
	}
	if indexes, problems := IndexedForkAssignments(repo, assignment.Owner.Fork); len(problems) != 0 || len(indexes) != 0 {
		t.Fatalf("assignment index after the rerun = %+v, %v", indexes, problems)
	}
}

// --from-fork is a person's tool. Without a terminal, which is how agents run commands, it is
// refused before anything is read or moved.
func TestDoneFromForkNeedsATerminal(t *testing.T) {
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })
	const id = "agent-run"
	repo, root, assignment := pausedForkTask(t, id)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "Do it\n\nCoop-Task: "+id)
	code, err := CmdTasksFolder(repo, root, []string{"done", id, "--from-fork"})
	if code != 2 || !errors.Is(err, errFromForkNeedsTerminal) {
		t.Fatalf("done --from-fork without a terminal = %d, %v; want the terminal refusal", code, err)
	}
	heldByFork(t, repo, root, id, assignment)
}
