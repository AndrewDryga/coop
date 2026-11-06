package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/tasks"
)

func seedForkDecisionTask(t *testing.T, repo, id string) {
	t.Helper()
	dir := filepath.Join(repo, ".agent", "tasks", "00_todo", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nid: " + id + "\ntitle: Fork decision fixture " + id + "\nlabels: []\nupdated: 2026-11-05T00:00:00Z\n---\n\n" +
		"# Fork decision fixture " + id + "\n\n" +
		"**Context:** a fork loop task that a person may have to decide.\n\n" +
		"**Acceptance criteria:** the fork keeps working its other tasks while this one waits.\n\n" +
		"**Approach:** one checklist item.\n\n" +
		"## Subtasks\n\n- [ ] Do the work\n"
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// parkingForkPass stands in for a loop pass whose agent parks its one task on a decision, as
// tasks_block does, and then exits as coop loop does. It records which task each pass got.
func parkingForkPass(t *testing.T, ran *[]string) func(loop.RunSpec) (int, error) {
	return func(spec loop.RunSpec) (int, error) {
		if len(spec.Queues) != 1 || spec.CandidateReview != nil {
			t.Errorf("fork pass = queues %v, candidate review %v; want one assigned task", spec.Queues, spec.CandidateReview)
			return 1, nil
		}
		queue := filepath.Join(spec.Repo, spec.Queues[0])
		entries, err := os.ReadDir(filepath.Join(queue, tasks.StateInProgress))
		if err != nil || len(entries) != 1 {
			t.Errorf("projected work = %v, %v; want one task in progress", entries, err)
			return 1, nil
		}
		id := entries[0].Name()
		*ran = append(*ran, id)
		blocked := filepath.Join(queue, tasks.StateBlocked, id)
		if err := os.Rename(filepath.Join(queue, tasks.StateInProgress, id), blocked); err != nil {
			t.Error(err)
			return 1, nil
		}
		decision := tasks.RenderDecision(id, tasks.Decision{
			Question: "Which way for " + id + "?", Why: "Only the owner can choose.",
			Options: []string{"this", "that"}, Recommendation: "this",
		})
		if err := os.WriteFile(filepath.Join(blocked, "decision.md"), []byte(decision), 0o644); err != nil {
			t.Error(err)
			return 1, nil
		}
		return loopDecisionExit, nil
	}
}

func setUpForkForDecision(t *testing.T, repo, name string) (string, forkspace.Identity) {
	t.Helper()
	ws, err := forkspace.Setup(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	return ws, bindTestFork(t, repo, name)
}

func runForkLoopForDecision(t *testing.T, a *app, repo, ws string, identity forkspace.Identity) (int, string, error) {
	t.Helper()
	var code int
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, runErr = a.runForkLoop(repo, ws, identity, "claude", "", "", "", "", nil, networkFlags{}, false)
		})
	})
	return code, stdout + stderr, runErr
}

func assertForkTaskWaitsForDecision(t *testing.T, repo, id string) {
	t.Helper()
	queue := filepath.Join(repo, ".agent", "tasks")
	if _, err := os.Stat(filepath.Join(queue, tasks.StateBlocked, id, "decision.md")); err != nil {
		t.Errorf("task %s is not blocked with its decision in the project's queue: %v", id, err)
	}
	record, owned, err := tasks.ReadTaskOwnerRecord(queue, id)
	if err != nil || !owned || record.Fork == nil || record.Fork.Phase != tasks.ForkAssignmentBlocked {
		t.Errorf("task %s owner = (%+v, owned=%v, err=%v), want the fork's blocked assignment", id, record.Fork, owned, err)
	}
}

func TestForkLoopWorksItsOtherTasksPastADecision(t *testing.T) {
	repo := initRepo(t)
	first, second := "2026-11-05-fork-decision-a", "2026-11-05-fork-decision-b"
	seedForkDecisionTask(t, repo, first)
	seedForkDecisionTask(t, repo, second)
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	var ran []string
	a.forkLoopRun = parkingForkPass(t, &ran)

	ws, identity := setUpForkForDecision(t, repo, "decide")
	code, output, err := runForkLoopForDecision(t, a, repo, ws, identity)
	if !slices.Equal(ran, []string{first, second}) {
		t.Fatalf("fork worked %v, want %s and then, after it blocked, %s\n%s", ran, first, second, output)
	}
	if code != loopDecisionExit || err != nil {
		t.Errorf("fork loop = (%d, %v), want (%d, nil) once nothing else is workable", code, err, loopDecisionExit)
	}
	for _, want := range []string{"Fork decide looks for other work while that task waits for your decision.", "Fork decide is waiting for your decision.", "Answer it: coop tasks decisions -i"} {
		if !strings.Contains(output, want) {
			t.Errorf("fork loop output lacks %q:\n%s", want, output)
		}
	}
	assertForkTaskWaitsForDecision(t, repo, first)
	assertForkTaskWaitsForDecision(t, repo, second)
	decisions := captureStdout(t, func() {
		if code, err := tasks.CmdTasksFolder(repo, filepath.Join(repo, ".agent", "tasks"), []string{"decisions"}); code != 0 || err != nil {
			t.Errorf("coop tasks decisions = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"Which way for " + first + "?", "Which way for " + second + "?"} {
		if !strings.Contains(decisions, want) {
			t.Errorf("coop tasks decisions does not list %q:\n%s", want, decisions)
		}
	}
}

func TestForkLoopStopsWhenOnlyADecisionIsLeft(t *testing.T) {
	repo := initRepo(t)
	id := "2026-11-05-fork-decision-only"
	seedForkDecisionTask(t, repo, id)
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	var ran []string
	a.forkLoopRun = parkingForkPass(t, &ran)

	ws, identity := setUpForkForDecision(t, repo, "alone")
	code, output, err := runForkLoopForDecision(t, a, repo, ws, identity)
	if !slices.Equal(ran, []string{id}) {
		t.Fatalf("fork worked %v, want only %s\n%s", ran, id, output)
	}
	if code != loopDecisionExit || err != nil || !strings.Contains(output, "Fork alone is waiting for your decision.") ||
		!strings.Contains(output, "Continue:  coop fork alone claude --loop") {
		t.Errorf("fork loop = (%d, %v), want it to stop for the decision and say how to go on:\n%s", code, err, output)
	}
	assertForkTaskWaitsForDecision(t, repo, id)
}

// A task answered in the project goes back to the same fork. If its agent parks it again, the new
// question must reach the project's decisions, not be undone by the restore a failed run gets.
func TestForkLoopKeepsAResumedTaskBlockedWhenItParksAgain(t *testing.T) {
	repo := initRepo(t)
	id := "2026-11-05-fork-decision-again"
	seedForkDecisionTask(t, repo, id)
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	var ran []string
	a.forkLoopRun = parkingForkPass(t, &ran)
	ws, identity := setUpForkForDecision(t, repo, "again")
	if code, output, err := runForkLoopForDecision(t, a, repo, ws, identity); code != loopDecisionExit || err != nil {
		t.Fatalf("first fork loop = (%d, %v)\n%s", code, err, output)
	}
	queue := filepath.Join(repo, ".agent", "tasks")
	captureStdout(t, func() {
		if code, err := tasks.CmdTasksFolder(repo, queue, []string{"unblock", id, "Go with this."}); code != 0 || err != nil {
			t.Fatalf("coop tasks unblock = (%d, %v)", code, err)
		}
	})

	code, output, err := runForkLoopForDecision(t, a, repo, ws, identity)
	if !slices.Equal(ran, []string{id, id}) || code != loopDecisionExit || err != nil {
		t.Fatalf("resumed fork loop worked %v = (%d, %v), want the answered task once more and a stop for its new decision\n%s", ran, code, err, output)
	}
	assertForkTaskWaitsForDecision(t, repo, id)
}
