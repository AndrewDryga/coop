package loop

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The loop's closing report must not claim the work passed when the final review reopened it —
// which it does by moving done tasks back into 10_in_progress/, not 00_todo/. Regression: the
// check looked at 00_todo/ only, so a reopened task in in_progress fell through to the green pass.
func TestFinalVerdict(t *testing.T) {
	reopened := []taskLine{{id: "fix-login", title: "Fix login retries", state: "in progress"}}
	got := captureStderr(t, func() {
		printFinalVerdict(tasks.TaskCounts{Done: 2, Doing: 3}, reopened, nil, "coop loop claude")
	})
	for _, want := range []string{"⚠ Final review left 3 tasks to finish", "Fix login retries is back in progress.", "Continue: coop loop claude"} {
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
	for _, want := range []string{"Stopped for your decision · 2/3 tasks done", "Choose how long sessions last", "2026-09-11-choose-session-duration", "Answer it: coop tasks decisions -i"} {
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

func TestFailedFinalVerificationCannotReportSuccess(t *testing.T) {
	cf := tasks.TaskCounts{Done: 5}
	got := captureStderr(t, func() { printFailedVerificationVerdict(cf) })
	if !strings.Contains(got, "✗ Final verification failed · 5/5 done · completed work is unverified") {
		t.Fatalf("failed verification verdict = %q", got)
	}
	if strings.Contains(got, "passed final review") {
		t.Fatalf("failed verification reported success: %q", got)
	}
	if code := loopExitCodeAfterVerification(cf, true); code != 1 {
		t.Fatalf("failed verification exit = %d, want 1", code)
	}
	if code := loopExitCodeAfterVerification(cf, false); code != 0 {
		t.Fatalf("successful verification exit = %d, want 0", code)
	}
	if code := loopExitCodeAfterVerification(tasks.TaskCounts{Done: 4, Blocked: 1}, true); code != 3 {
		t.Fatalf("blocked queue exit = %d, want existing decision exit 3", code)
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
	if !strings.Contains(got, "Blocked: Choose how long sessions last") || !strings.Contains(got, "Answer it: coop tasks decisions -i") {
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
		!strings.Contains(got, "Answer them: coop tasks decisions -i") {
		t.Errorf("blocked empty report = %q", got)
	}
}

// Work another controller holds is not a drained queue, and the report names what it observed.
func TestBusyQueueReport(t *testing.T) {
	got := captureStderr(t, func() { printBusyQueue(tasks.TaskLeaseSummary{Owned: 1, Busy: 2}) })
	if got != "No task is available to this loop. The remaining work is reserved by another agent.\n" {
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
	got := captureStderr(t, func() {
		printTaskHeader(1, 1, task, "claude:opus@personal", false, tasks.TaskCounts{Done: 4, Doing: 1, Todo: 7, Blocked: 2})
	})
	rule := strings.Repeat("━", 64)
	want := "\n" + rule + "\n Task 1 - Attempt 1\n\n Fix login retries\n\n Project  internal/auth\n Agent  claude:opus@personal\n Queue  4 completed · 1 active · 7 pending · 2 blocked\n" + rule + "\n\n"
	if got != want {
		t.Errorf("task header =\n%q\nwant\n%q", got, want)
	}
	// The repo's own queue has no scope to distinguish: the id stands alone.
	root := taskLine{id: "2026-09-11-fix-login-retries", title: "Fix login retries"}
	got = captureStderr(t, func() { printTaskHeader(2, 3, root, "claude", false, tasks.TaskCounts{Doing: 1}) })
	if strings.Contains(got, "Project") || strings.Contains(got, root.id) || !strings.Contains(got, "Task 2 - Attempt 3") {
		t.Errorf("root-queue header = %q", got)
	}
	// A custom work.command runs the command, so the header names the command.
	got = captureStderr(t, func() { printTaskHeader(3, 1, root, "make work", true, tasks.TaskCounts{Doing: 1}) })
	if !strings.Contains(got, " Command  make work") || strings.Contains(got, "Agent  ") {
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

func TestTaskBannerColorsOnlyTheStructure(t *testing.T) {
	task := taskLine{title: "Publish model [1m] metadata", scope: "internal/cli"}
	lines := taskBannerLines(ui.Colored(), 64, 1, 2, task, "codex:gpt-5.6-terra/xhigh@work", false, tasks.TaskCounts{Doing: 1})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "\x1b[2m━") || !strings.Contains(joined, "\x1b[2m Task 1 - Attempt 2") ||
		!strings.Contains(joined, "\x1b[2m Agent  codex:gpt-5.6-terra/xhigh@work") ||
		!strings.Contains(joined, "\x1b[2m Queue  0 completed · 1 active") {
		t.Fatalf("banner structure is not dimmed:\n%q", joined)
	}
	titleLine := " Publish model [1m] metadata"
	if len(lines) < 4 || lines[3] != titleLine || strings.Contains(lines[3], "\x1b") {
		t.Fatalf("title must remain normal foreground and preserve literal model-like text:\n%q", joined)
	}
}

func TestTaskBannerWrapsAndNeutralizesTerminalControls(t *testing.T) {
	lines := taskBannerLines(ui.For(nil), 32, 12, 3,
		taskLine{title: "Publish \x1b[31mthe marketplace\x1b[0m", scope: "a/very/long/subproject/name"},
		"codex:a-very-long-model-name/high@personal", false,
		tasks.TaskCounts{Done: 28, Doing: 1, Todo: 74, Blocked: 2})
	for _, line := range lines {
		if strings.Contains(line, "\x1b") {
			t.Fatalf("banner retained injected terminal control: %q", line)
		}
		if width := len([]rune(line)); width > 32 {
			t.Fatalf("banner line width = %d, want at most 32: %q", width, line)
		}
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Publish the marketplace", " Agent\n   codex:a-very-long-model-name/", "Queue  28 completed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapped banner missing %q:\n%s", want, joined)
		}
	}
}

func TestNarrowTaskBannerKeepsCanonicalTargetIntactWhenItFits(t *testing.T) {
	lines := taskBannerLines(ui.For(nil), 41, 19, 2,
		taskLine{title: "Make the settings page keyboard accessible"},
		"codex:gpt-5.6-terra/xhigh@personal", false,
		tasks.TaskCounts{Done: 17, Doing: 1, Todo: 56, Blocked: 1})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, " Agent\n   codex:gpt-5.6-terra/xhigh@personal") {
		t.Fatalf("narrow banner split a target that fits on its own row:\n%s", joined)
	}
	for _, line := range lines {
		if len([]rune(line)) > 41 {
			t.Fatalf("narrow banner line is too wide: %q", line)
		}
	}
}

func TestLoopPreparationAndPreflightUseTheApprovedHierarchy(t *testing.T) {
	if got := loopConfigLine(true); got != "Using .agent/loop.yaml" {
		t.Fatalf("configured intro = %q", got)
	}
	if got := loopConfigLine(false); got != "Using built-in defaults" {
		t.Fatalf("default intro = %q", got)
	}

	got := captureStderr(t, func() {
		printLoopPreparation(Preparation{RecoveredFilteredRuns: 4}, []string{
			"box image is stale — the box Dockerfile/.tool-versions changed since it was built; run 'coop build'",
		}, true)
	})
	want := "\nPreparing loop\n" +
		"  ⚠ Recovered 4 interrupted filtered network runs\n" +
		"    Their Coop processes are no longer running.\n" +
		"    Details: coop net runs\n" +
		"\n  ⚠ Box image is out of date\n" +
		"    The box Dockerfile or .tool-versions changed since it was built.\n" +
		"    To rebuild: coop build\n" +
		"\n" +
		"  ✓ Keeping this Mac awake using caffeinate\n"
	if got != want {
		t.Fatalf("loop preparation = %q, want %q", got, want)
	}

	root := filepath.Join(t.TempDir(), tasksRoot)
	const id = "2026-09-12-publish-plugin"
	writeTaskFile(t, filepath.Join(root, stateTodo, id, "task.md"), "# Publish the Emisar Cursor Marketplace plugin\n")
	got = captureStderr(t, func() { printPreflight([]string{root}, preflightResult{unblocked: []string{id}}) })
	want = "\nRunning pre-flight checks\n" +
		"  Resolving answered blockers\n" +
		"  ✓ Unblocked “Publish the Emisar Cursor Marketplace plugin” - resolution filled in\n"
	if got != want {
		t.Fatalf("preflight = %q, want %q", got, want)
	}
}

func TestReviewBannersAreCompactBoundedAndGrammatical(t *testing.T) {
	got := captureStderr(t, func() {
		printReviewBanner("Reviewing project checks", "codex:a-very-long-model-name/high@work",
			reviewField{label: "Files", values: []string{strings.Repeat("a", 80)}})
	})
	for _, line := range strings.Split(got, "\n") {
		if width := len([]rune(line)); width > 64 {
			t.Fatalf("review line width = %d, want at most 64: %q", width, line)
		}
	}
	for _, want := range []string{"Reviewing project checks", "Agent  codex:a-very-long-model-name/high@work", " Files\n   "} {
		if !strings.Contains(got, want) {
			t.Errorf("review banner missing %q:\n%s", want, got)
		}
	}
	afterDecision := captureStderr(t, func() { printFinalReview(4, 3, "codex:test", 1) })
	if !strings.Contains(afterDecision, "Final review · After decision") || strings.Contains(afterDecision, "Round 4 of 3") {
		t.Fatalf("post-decision review banner = %q", afterDecision)
	}

	one := captureStderr(t, func() { printReopened("Final review", []string{"a"}, []string{"Fix login retries"}) })
	if !strings.Contains(one, "Final review · 1 task needs more work") || !strings.Contains(one, "Continuing the task queue") {
		t.Fatalf("one-task reopen = %q", one)
	}
	two := captureStderr(t, func() { printReopened("Verification", []string{"a", "b"}, []string{"One", "Two"}) })
	if !strings.Contains(two, "Verification · 2 tasks need more work") || strings.Contains(two, "2 tasks needs") {
		t.Fatalf("multi-task reopen = %q", two)
	}
	long := strings.Repeat("long-title-", 10)
	completed := captureStderr(t, func() { printTaskCompleted(taskLine{title: long}) })
	for _, line := range strings.Split(completed, "\n") {
		if len([]rune(line)) > 79 {
			t.Fatalf("completion history exceeded the static terminal width: %q", line)
		}
	}
	wrapped := captureStderr(t, func() {
		printReopened("Final review", []string{"a"}, []string{long})
		printReviewLimit([]string{long}, 3)
	})
	for _, line := range strings.Split(wrapped, "\n") {
		if len([]rune(line)) > 64 {
			t.Fatalf("completion/review history exceeded its bounded width: %q", line)
		}
	}
}

func TestLoopResultTextNeutralizesTaskControls(t *testing.T) {
	bad := "Fix \x1b[31mlogin\x1b[0m\nINJECT"
	got := captureStderr(t, func() {
		printFinalVerdict(tasks.TaskCounts{Todo: 1}, []taskLine{{title: bad, state: "todo"}}, nil, "coop loop")
		printTaskLimitReached(loopTaskLimit{max: 1, settled: 1, lastTitle: bad, lastState: tasks.StateDone}, "coop loop")
		h := newLoopHealth()
		h.noteReopen([]string{"task"})
		printRunSummary([]taskLine{{id: "task", title: bad}}, runCost{}, h)
		ui.Alert("This in-progress task already has a commit", alreadyCommittedCause(bad, "abc123"))
	})
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\nINJECT") {
		t.Fatalf("loop result text retained injectable controls: %q", got)
	}
	for _, want := range []string{"Fix loginINJECT is back in todo.", "Completed: Fix loginINJECT", "Fix loginINJECT was reopened", "Fix loginINJECT is linked to abc123"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitized result missing %q:\n%s", want, got)
		}
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
	if got := progressLineWidth(tasks.TaskCounts{Doing: 1}, "Fix \x1b[31mlogin\x1b[0m\nINJECT", 80); strings.Contains(got, "\x1b") || strings.Contains(got, "\n") || !strings.Contains(got, "Fix loginINJECT") {
		t.Errorf("live progress retained task-title controls: %q", got)
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
