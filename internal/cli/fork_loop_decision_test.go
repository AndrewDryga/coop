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
	seedForkDecisionTaskIn(t, filepath.Join(repo, ".agent", "tasks"), id)
}

func seedForkDecisionTaskIn(t *testing.T, queue, id string) {
	t.Helper()
	dir := filepath.Join(queue, "00_todo", id)
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
	return runForkLoopOnQueues(t, a, repo, ws, identity, nil)
}

// runForkLoopOnQueues runs the fork's loop on the queues given as --tasks, or the project's own queues
// when there are none.
func runForkLoopOnQueues(t *testing.T, a *app, repo, ws string, identity forkspace.Identity, queues []string) (int, string, error) {
	t.Helper()
	var code int
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, runErr = a.runForkLoop(repo, ws, identity, "claude", queues, "", "", "", nil, networkFlags{}, false)
		})
	})
	return code, stdout + stderr, runErr
}

// answerDecisionFile fills in the Resolution line of the decision.md at path, as a person does by hand.
func answerDecisionFile(t *testing.T, path, answer string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "**Resolution:**") {
			lines[i] = "**Resolution:** " + answer
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
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
		!strings.Contains(output, "Continue:  coop fork alone claude --loop\n") { // no --tasks it was not given
		t.Errorf("fork loop = (%d, %v), want it to stop for the decision and say how to go on:\n%s", code, err, output)
	}
	assertForkTaskWaitsForDecision(t, repo, id)
}

// A fork loop started on chosen queues says to continue on those queues, in order: pasted without its
// --tasks, the Continue line would start the fork on every project queue.
func TestForkLoopContinueLineKeepsItsChosenQueues(t *testing.T) {
	repo := initRepo(t)
	seedForkDecisionTask(t, repo, "2026-11-06-fork-continue-queue")
	web := filepath.Join(repo, "web", ".agent", "tasks")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	var ran []string
	a.forkLoopRun = parkingForkPass(t, &ran)
	ws, identity := setUpForkForDecision(t, repo, "chosen")
	queue := filepath.Join(repo, ".agent", "tasks")
	var code int
	var err error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, err = a.runForkLoop(repo, ws, identity, "claude", []string{web, queue}, "", "", "", nil, networkFlags{}, false)
		})
	})
	if want := "Continue:  coop fork chosen claude --loop --tasks " + shellWord(web) + " --tasks " + shellWord(queue) + "\n"; code != loopDecisionExit || err != nil || !strings.Contains(stdout+stderr, want) {
		t.Errorf("fork loop = (%d, %v), want it to stop for the decision and continue with %q:\n%s", code, err, want, stdout+stderr)
	}
	if want := "Answer it: coop tasks --tasks " + shellWord(web) + " --tasks " + shellWord(queue) + " decisions -i\n"; !strings.Contains(stdout+stderr, want) {
		t.Errorf("fork loop output lacks %q:\n%s", want, stdout+stderr)
	}
}

// A task answered in the project goes back to the same fork, whose worker reads the answer in the
// fork's own copy of the task. If its agent parks it again, the new question must reach the project's
// decisions, not be undone by the restore a failed run gets.
func TestForkLoopKeepsAResumedTaskBlockedWhenItParksAgain(t *testing.T) {
	repo := initRepo(t)
	id := "2026-11-05-fork-decision-again"
	seedForkDecisionTask(t, repo, id)
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	var ran, answers []string
	park := parkingForkPass(t, &ran)
	a.forkLoopRun = func(spec loop.RunSpec) (int, error) {
		answer, err := tasks.TaskResolution(filepath.Join(spec.Repo, spec.Queues[0], tasks.StateInProgress, id))
		if err != nil {
			t.Errorf("read the fork's copy of the decision: %v", err)
		}
		answers = append(answers, answer)
		return park(spec)
	}
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
	if !slices.Equal(answers, []string{"", "Go with this."}) {
		t.Errorf("answers the fork's passes read = %q, want none and then the project's answer", answers)
	}
	assertForkTaskWaitsForDecision(t, repo, id)
}

// A fork that stops for its decisions names each task and what it still needs. An answer written in
// the project's decision.md still waits for the unblock that sends it back, and one written in the
// fork's own copy, which the fork's box can write too, is not taken as the answer: resuming the fork
// never works the task again by itself.
func TestForkLoopSaysWhatEachDecisionStillNeeds(t *testing.T) {
	const id = "2026-11-06-fork-decision-said"
	for _, tc := range []struct {
		name, answerIn, want string
		answerIt             bool
	}{
		{name: "unanswered", want: `Needs your answer: coop tasks unblock ` + id + ` "<answer>"`, answerIt: true},
		{name: "answered in the project", answerIn: "project", want: "Answered · finish it: coop tasks unblock " + id + "\n"},
		{name: "answered in the fork's copy", answerIn: "fork", answerIt: true,
			want: `Answered only in fork said's copy · send it from the project: coop tasks unblock ` + id + ` "<answer>"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			seedForkDecisionTask(t, repo, id)
			commitForkNetworkPreflightFixture(t, repo)
			a := forkNetworkPreflightApp(t, repo)
			var ran []string
			a.forkLoopRun = parkingForkPass(t, &ran)
			ws, identity := setUpForkForDecision(t, repo, "said")
			if code, output, err := runForkLoopForDecision(t, a, repo, ws, identity); code != loopDecisionExit || err != nil {
				t.Fatalf("first fork loop = (%d, %v)\n%s", code, err, output)
			}
			queue := filepath.Join(repo, ".agent", "tasks")
			blocked := filepath.Join(queue, tasks.StateBlocked, id)
			if state, err := tasks.ReadTaskState(blocked); err != nil || !strings.Contains(state.NextAction, "coop tasks unblock "+id) {
				t.Errorf("project's next action for the parked task = %+v, %v; want the unblock command", state, err)
			}
			record, owned, err := tasks.ReadTaskOwnerRecord(queue, id)
			if err != nil || !owned || record.Fork == nil {
				t.Fatalf("owner = (%+v, %v, %v)", record, owned, err)
			}
			switch tc.answerIn {
			case "project":
				answerDecisionFile(t, filepath.Join(blocked, "decision.md"), "Go with this.")
			case "fork":
				answerDecisionFile(t, filepath.Join(record.Fork.Projection, tasks.StateBlocked, id, "decision.md"), "Go with this.")
			}

			code, output, err := runForkLoopForDecision(t, a, repo, ws, identity)
			if code != loopDecisionExit || err != nil || !slices.Equal(ran, []string{id}) {
				t.Fatalf("resumed fork loop worked %v = (%d, %v), want it to stop with the task still waiting\n%s", ran, code, err, output)
			}
			for _, want := range []string{"Fork said is waiting for your decision.", "Fork decision fixture " + id, tc.want} {
				if !strings.Contains(output, want) {
					t.Errorf("fork loop output lacks %q:\n%s", want, output)
				}
			}
			if got := strings.Contains(output, "Answer it: coop tasks decisions -i"); got != tc.answerIt {
				t.Errorf("output names the decisions browser = %v, want %v:\n%s", got, tc.answerIt, output)
			}
			assertForkTaskWaitsForDecision(t, repo, id)
		})
	}
}

// coop tasks unblock <id> "<answer>" answers a fork's task in the project whether it runs there or in
// the fork's own checkout, never in a queue the fork's box wrote in that checkout. The restart it
// names works where the person is.
func TestTasksUnblockAnswersAForksTaskFromTheForksCheckout(t *testing.T) {
	const id, answer = "2026-11-06-fork-decision-checkout", "Go with this."
	for _, where := range []string{"project", "fork"} {
		t.Run(where, func(t *testing.T) {
			repo := initRepo(t)
			seedForkDecisionTask(t, repo, id)
			commitForkNetworkPreflightFixture(t, repo)
			a := forkNetworkPreflightApp(t, repo)
			var ran []string
			a.forkLoopRun = parkingForkPass(t, &ran)
			ws, identity := setUpForkForDecision(t, repo, "here")
			if code, output, err := runForkLoopForDecision(t, a, repo, ws, identity); code != loopDecisionExit || err != nil {
				t.Fatalf("fork loop = (%d, %v)\n%s", code, err, output)
			}
			queue := filepath.Join(repo, ".agent", "tasks")
			record, owned, err := tasks.ReadTaskOwnerRecord(queue, id)
			if err != nil || !owned || record.Fork == nil {
				t.Fatalf("owner = (%+v, %v, %v)", record, owned, err)
			}
			planted := filepath.Join(ws, ".agent", "tasks", tasks.StateBlocked, id)
			if err := os.MkdirAll(planted, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"task.md":     "---\nid: " + id + "\ntitle: Planted\nlabels: []\nupdated: 2026-11-06T00:00:00Z\n---\n\n# Planted\n",
				"decision.md": "# Decision: Planted?\n\n**Resolution:** <!-- answer -->\n",
			} {
				if err := os.WriteFile(filepath.Join(planted, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			project, err := filepath.EvalSymlinks(repo)
			if err != nil {
				t.Fatal(err)
			}
			restart := "resume it: coop fork here --loop -d\n"
			if where == "fork" {
				t.Chdir(ws)
				restart = "resume it: COOP_REPO=" + shellWord(project) + " coop fork here --loop -d\n"
			} else {
				t.Chdir(repo)
			}
			a.cfg.RepoOverride = ""
			var code int
			out := captureStderr(t, func() {
				captureStdout(t, func() { code, err = a.cmdTasks([]string{"unblock", id, answer}) })
			})
			if code != 0 || err != nil || !strings.Contains(out, restart) {
				t.Fatalf("coop tasks unblock in the %s = (%d, %v), want it to answer the task and say %q:\n%s", where, code, err, restart, out)
			}
			if got, err := tasks.TaskResolution(filepath.Join(queue, tasks.StateTodo, id)); err != nil || got != answer {
				t.Errorf("the project's task answer = %q, %v; want it back in todo with %q", got, err, answer)
			}
			if got, err := tasks.TaskResolution(filepath.Join(record.Fork.Projection, tasks.StateBlocked, id)); err != nil || got != answer {
				t.Errorf("the fork's copy of the answer = %q, %v; want %q", got, err, answer)
			}
			if got, err := tasks.TaskResolution(planted); err != nil || got != "" {
				t.Errorf("the queue in the fork's checkout got answer %q, %v; want it left alone", got, err)
			}
		})
	}
}

// A fork started on a queue the project's task commands do not list parks its decision there, and
// coop tasks unblock <id>, the command its decision.md names, finds it there without --tasks. A queue
// a person names is searched as named.
func TestTasksUnblockFindsATaskInAForksChosenQueue(t *testing.T) {
	const id = "2026-11-06-fork-decision-queue-b"
	repo := initRepo(t)
	queue := filepath.Join(repo, ".agent", "tasks-b")
	seedForkDecisionTaskIn(t, queue, id)
	commitForkNetworkPreflightFixture(t, repo)
	a := forkNetworkPreflightApp(t, repo)
	var ran []string
	a.forkLoopRun = parkingForkPass(t, &ran)
	ws, identity := setUpForkForDecision(t, repo, "queueb")
	code, output, err := runForkLoopOnQueues(t, a, repo, ws, identity, []string{queue})
	if code != loopDecisionExit || err != nil {
		t.Fatalf("fork loop = (%d, %v)\n%s", code, err, output)
	}
	if want := "Answer it: coop tasks --tasks " + shellWord(queue) + " decisions -i"; !strings.Contains(output, want) {
		t.Errorf("fork loop output lacks %q:\n%s", want, output)
	}
	blocked := filepath.Join(queue, tasks.StateBlocked, id)

	if code, err := a.cmdTasks([]string{"unblock", "2026-11-06-absent", "Go with this."}); code == 0 || err == nil || !strings.Contains(err.Error(), "No task matching") {
		t.Errorf("unblock of an unknown task = (%d, %v), want No task matching", code, err)
	}
	if code, err := a.cmdTasks([]string{"--tasks", ".agent/tasks", "unblock", id, "Go with this."}); code == 0 || err == nil {
		t.Errorf("unblock in a queue that does not hold the task = (%d, %v), want a refusal", code, err)
	}
	if _, err := os.Stat(blocked); err != nil {
		t.Fatalf("a refused unblock moved the task: %v", err)
	}
	captureStdout(t, func() {
		if code, err := a.cmdTasks([]string{"unblock", id, "Go with this."}); code != 0 || err != nil {
			t.Errorf("coop tasks unblock without --tasks = (%d, %v), want the fork's queue found", code, err)
		}
	})
	if answer, err := tasks.TaskResolution(filepath.Join(queue, tasks.StateTodo, id)); err != nil || answer != "Go with this." {
		t.Errorf("unblocked task's answer = %q, %v; want it saved in the fork's chosen queue", answer, err)
	}
}
