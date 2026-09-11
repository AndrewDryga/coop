package loop

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// The loop's closing report must not claim the work passed when the final review reopened it —
// which it does by moving done tasks back into 10_in_progress/, not 00_todo/. Regression: the
// check looked at 00_todo/ only, so a reopened task in in_progress fell through to the green pass.
func TestFinalVerdict(t *testing.T) {
	reopened := []taskLine{{id: "fix-login", title: "Fix login retries", state: "in progress"}}
	got := captureStderr(t, func() {
		printFinalVerdict(tasks.TaskCounts{Done: 2, Doing: 3}, reopened, nil, "coop loop claude")
	})
	for _, want := range []string{"⚠ Final review left 3 tasks to finish", "Fix login retries is back in in progress.", "Continue: coop loop claude"} {
		if !strings.Contains(got, want) {
			t.Errorf("reopened report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "passed final review") {
		t.Errorf("a reopened queue must not read as passed:\n%s", got)
	}

	// Reopened into todo: the sentence uses the state the task is ACTUALLY in.
	todo := []taskLine{{id: "fix-login", title: "Fix login retries", state: "todo"}}
	got = captureStderr(t, func() {
		printFinalVerdict(tasks.TaskCounts{Done: 4, Todo: 1}, todo, nil, "coop loop claude")
	})
	if !strings.Contains(got, "Final review left 1 task to finish") || !strings.Contains(got, "is back in todo.") {
		t.Errorf("todo reopen report = %q", got)
	}

	// Nothing reopened, work parked on a decision: not a pass, and it names the task.
	blocked := []taskLine{{id: "2026-09-11-choose-session-duration", title: "Choose how long sessions last"}}
	got = captureStderr(t, func() {
		printFinalVerdict(tasks.TaskCounts{Done: 2, Blocked: 1}, nil, blocked, "coop loop claude")
	})
	for _, want := range []string{"Stopped for your decision · 2/3 tasks done", "Choose how long sessions last", "2026-09-11-choose-session-duration", "Answer it: coop tasks decisions"} {
		if !strings.Contains(got, want) {
			t.Errorf("blocked report missing %q:\n%s", want, got)
		}
	}

	// A clean run is the ONLY case that may claim the review passed.
	got = captureStderr(t, func() {
		printFinalVerdict(tasks.TaskCounts{Done: 5}, nil, nil, "coop loop claude")
	})
	if !strings.Contains(got, "✓ All tasks passed final review · 5/5 done") {
		t.Errorf("clean report = %q", got)
	}
}

// The loop's exit code lets cron/CI branch without parsing stderr: a review-reopened queue is
// a failure, 3 means only human-blocked work remains, and 0 means verified done.
func TestLoopExitCode(t *testing.T) {
	cases := []struct {
		cf   tasks.TaskCounts
		want int
	}{
		{tasks.TaskCounts{Done: 3, Blocked: 2}, 3}, // blocked, nothing actionable → 3
		{tasks.TaskCounts{Done: 5}, 0},             // verified done → 0
		{tasks.TaskCounts{Done: 3, Doing: 1}, 1},   // audit reopened into in_progress → unverified
		{tasks.TaskCounts{Todo: 2, Blocked: 1}, 1}, // actionable work takes precedence over blocked
	}
	for _, c := range cases {
		if got := loopExitCode(c.cf); got != c.want {
			t.Errorf("loopExitCode(%+v) = %d, want %d", c.cf, got, c.want)
		}
	}
}

// A stop the user asked for and a limit the user set are both successes — and neither may read as
// a verdict. Each says the final review has not run, and how to carry on.
func TestLoopIntentionalAndInterruptedStopsAreDistinct(t *testing.T) {
	if LoopInterruptedExitCode != 130 {
		t.Fatalf("loop interrupt exit = %d, want conventional SIGINT status 130", LoopInterruptedExitCode)
	}
	cf := tasks.TaskCounts{Done: 3, Todo: 2}
	got := captureStderr(t, func() { printInterrupted(cf, "coop loop claude", false) })
	if !strings.Contains(got, "Stopped before final review · 3/5 tasks done") || !strings.Contains(got, "Continue: coop loop claude") {
		t.Errorf("interrupt report = %q", got)
	}
	// Interrupted DURING the review says so, rather than implying it never started.
	got = captureStderr(t, func() { printInterrupted(cf, "coop loop claude", true) })
	if !strings.Contains(got, "Stopped before final review completed") {
		t.Errorf("mid-review interrupt report = %q", got)
	}

	limit := loopTaskLimit{max: 1, settled: 1, lastID: "task-a", lastTitle: "Fix login retries", lastState: stateDone}
	got = captureStderr(t, func() { printTaskLimitReached(limit, "coop loop claude") })
	for _, want := range []string{"Paused after 1 of 1 requested task", "Completed: Fix login retries", "Final review has not run.", "Continue: coop loop claude"} {
		if !strings.Contains(got, want) {
			t.Errorf("task-limit report missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "✓") {
		t.Errorf("an intentional pause is not a verified result:\n%s", got)
	}

	// Fewer tasks than requested: the pause says the queue ran out, not that the limit was met.
	partial := loopTaskLimit{max: 3, settled: 1, lastID: "task-a", lastTitle: "Fix login retries", lastState: stateDone}
	got = captureStderr(t, func() { printTaskLimitReached(partial, "coop loop claude") })
	if !strings.Contains(got, "Paused after 1 task — no more tasks are ready") || !strings.Contains(got, "Final review has not run.") {
		t.Errorf("partial task-limit report = %q", got)
	}

	// A blocked last task sends the reader to the decision, not back into the loop.
	blocked := loopTaskLimit{max: 2, settled: 2, lastID: "task-b", lastTitle: "Choose how long sessions last", lastState: stateBlocked}
	got = captureStderr(t, func() { printTaskLimitReached(blocked, "coop loop claude") })
	if !strings.Contains(got, "Blocked: Choose how long sessions last") || !strings.Contains(got, "Answer it: coop tasks decisions") {
		t.Errorf("blocked task-limit report = %q", got)
	}
}

// An empty queue is reported as what it is. With work parked on a decision it says how much, and
// where to answer it — never a checkmark.
func TestNoActionableTasksReport(t *testing.T) {
	got := captureStderr(t, func() { printNoActionableTasks(tasks.TaskCounts{Done: 4}) })
	if strings.TrimSpace(got) != "No tasks ready to work on." {
		t.Errorf("empty report = %q", got)
	}
	got = captureStderr(t, func() { printNoActionableTasks(tasks.TaskCounts{Done: 4, Blocked: 2}) })
	if !strings.Contains(got, "No tasks ready to work on. 2 tasks need your decision.") ||
		!strings.Contains(got, "Answer them: coop tasks decisions") {
		t.Errorf("blocked empty report = %q", got)
	}
}

// Work another controller holds is not a drained queue, and the report names what it observed.
func TestBusyQueueReport(t *testing.T) {
	got := captureStderr(t, func() { printBusyQueue(tasks.TaskLeaseSummary{Owned: 1, Busy: 2}) })
	if !strings.Contains(got, "No task is available to this loop.") ||
		!strings.Contains(got, "reserved by another agent (1 owned, 2 busy).") {
		t.Errorf("busy report = %q", got)
	}
	if strings.Contains(got, "done") {
		t.Errorf("a reserved queue must not read as a result:\n%s", got)
	}
}

// The task header is the one place a reader learns which task an attempt is working, and with
// which agent. Ordinals are per TASK, so a retry keeps the number already on screen.
func TestTaskHeader(t *testing.T) {
	task := taskLine{id: "2026-09-11-fix-login-retries", title: "Fix login retries", scope: "internal/auth"}
	got := captureStderr(t, func() { printTaskHeader(1, task, "claude:opus@personal", false) })
	want := "\nTask 1 · Fix login retries\n  2026-09-11-fix-login-retries · internal/auth\n  Agent:  claude:opus@personal\n"
	if got != want {
		t.Errorf("task header =\n%q\nwant\n%q", got, want)
	}
	// The repo's own queue has no scope to distinguish: the id stands alone.
	root := taskLine{id: "2026-09-11-fix-login-retries", title: "Fix login retries"}
	got = captureStderr(t, func() { printTaskHeader(2, root, "claude", false) })
	if !strings.Contains(got, "  2026-09-11-fix-login-retries\n") || strings.Contains(got, " · \n") {
		t.Errorf("root-queue header = %q", got)
	}
	// A custom work.command runs the command, so the header names the command.
	got = captureStderr(t, func() { printTaskHeader(3, root, "make work", true) })
	if !strings.Contains(got, "  Command: make work") || strings.Contains(got, "Agent:") {
		t.Errorf("custom-command header = %q", got)
	}

	ordinals := newTaskOrdinals()
	if a, b, c := ordinals.of("one"), ordinals.of("two"), ordinals.of("one"); a != 1 || b != 2 || c != 1 {
		t.Errorf("task ordinals = %d, %d, %d — a retry must keep its task's number", a, b, c)
	}
	if a, b := ordinals.attempt("one"), ordinals.attempt("one"); a != 1 || b != 2 {
		t.Errorf("task attempts = %d, %d — each launch is the next attempt at that task", a, b)
	}
}

// A queue root maps to the subproject shown beside a task id; the repository's own queue has none.
func TestQueueScope(t *testing.T) {
	for queue, want := range map[string]string{
		".agent/tasks":                  "",
		"internal/auth/.agent/tasks":    "internal/auth",
		"services/api/web/.agent/tasks": "services/api/web",
	} {
		if got := queueScope(queue); got != want {
			t.Errorf("queueScope(%q) = %q, want %q", queue, got, want)
		}
	}
}

func TestProgressLine(t *testing.T) {
	// The mid-iteration line the live bar shows: done/total, blocked only when there is some,
	// and the active task.
	if got := progressLine(tasks.TaskCounts{Done: 8, Blocked: 1, Todo: 11}, "Add a password reset link"); got != "8/20 done · 1 blocked · Add a password reset link" {
		t.Errorf("progressLine = %q", got)
	}
	if got := progressLine(tasks.TaskCounts{Done: 20}, ""); got != "20/20 done" {
		t.Errorf("done-only progressLine = %q", got)
	}
	// A long title is truncated, not printed whole: the counts never give way to it.
	long := strings.Repeat("x", 80)
	if got := progressLine(tasks.TaskCounts{Todo: 1}, long); !strings.Contains(got, "…") || strings.Contains(got, long) {
		t.Errorf("long title not truncated: %q", got)
	}
}

func TestProgressLineWidthUsesAvailableTerminalWidth(t *testing.T) {
	activity := "Reconcile every provider credential rotation before the deployment cutover"
	wide := progressLineWidth(tasks.TaskCounts{Doing: 1}, activity, 120)
	if !strings.Contains(wide, activity) {
		t.Errorf("wide progress line should show activity past the old fixed cap: %q", wide)
	}

	const narrowWidth = 44
	narrow := progressLineWidth(tasks.TaskCounts{Doing: 1}, activity, narrowWidth)
	if strings.Contains(narrow, activity) || !strings.Contains(narrow, "…") {
		t.Errorf("narrow progress line should elide activity: %q", narrow)
	}
	if got := len([]rune(narrow)); got > narrowWidth {
		t.Errorf("narrow progress line width = %d, want at most %d: %q", got, narrowWidth, narrow)
	}
}
