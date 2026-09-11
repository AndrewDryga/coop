package loop

import (
	"fmt"
	"strings"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// loopExitCode is the machine-readable companion to the closing report so cron/CI can branch on
// the loop's outcome without parsing stderr prose: 1 when a final review left work actionable, 3
// when work is blocked on a human decision and nothing else is actionable, and 0 only when the
// queue is verified done. Other failures (1) and usage errors (2) surface from their own call sites.
func loopExitCode(cf tasks.TaskCounts) int {
	if cf.Todo+cf.Doing > 0 {
		return 1
	}
	if cf.Blocked > 0 {
		return 3
	}
	return 0
}

// maxReportedTasks bounds every closing list. A run that reopened forty tasks is not made clearer
// by forty lines; the queue commands have the rest.
const maxReportedTasks = 5

// printFinalVerdict is the loop's LAST word, and the only place that may say the queue passed.
// Reopened work and work parked on a human decision are not "done": each gets its own report,
// naming the tasks and the one command that moves them forward.
func printFinalVerdict(cf tasks.TaskCounts, actionable, blocked []taskLine, continueCmd string) {
	switch {
	case cf.Todo+cf.Doing > 0:
		ui.Alert(fmt.Sprintf("Final review left %s to finish", ui.Count(cf.Todo+cf.Doing, "task")),
			actionableCause(actionable), [2]string{"Continue:", continueCmd})
	case cf.Blocked > 0:
		ui.Note("")
		ui.Note("Stopped for your decision · %d/%d tasks done", cf.Done, cf.Total())
		printTaskList(blocked)
		ui.Note("")
		ui.Note("  %s %s", decisionsLabel(len(blocked)), "coop tasks decisions")
	default:
		ui.Note("")
		ui.OK("All tasks passed final review · %d/%d done", cf.Done, cf.Total())
	}
}

// actionableCause says, in the task's own words, what the review left to do — using the state the
// task is actually in, never forcing every reopen to read as "todo".
func actionableCause(actionable []taskLine) string {
	var lines []string
	for i, t := range actionable {
		if i == maxReportedTasks {
			lines = append(lines, fmt.Sprintf("… and %d more.", len(actionable)-maxReportedTasks))
			break
		}
		lines = append(lines, fmt.Sprintf("%s is back in %s.", t.title, t.state))
	}
	return strings.Join(lines, "\n")
}

// printTaskList is the closing report's task block: a title a person recognizes, with its full id
// (and scope) under it, so the task can be found in the queue without guessing.
func printTaskList(items []taskLine) {
	for i, t := range items {
		if i == maxReportedTasks {
			ui.Note("  … and %d more", len(items)-maxReportedTasks)
			return
		}
		ui.Note("")
		ui.Note("  %s", t.title)
		ui.Note("    %s", taskIdentity(t.id, t.scope))
	}
}

func decisionsLabel(n int) string {
	if n == 1 {
		return "Answer it:"
	}
	return "Answer them:"
}

// LoopInterruptedExitCode is the conventional SIGINT status a Ctrl-C'd run exits with, so a
// cron/CI caller can tell "you stopped it" from the queue verdicts loopExitCode reports.
const LoopInterruptedExitCode = 130

// printInterrupted closes a run the user stopped. It never claims a verdict: the final review
// either had not started, or was cut short — and the report says which.
func printInterrupted(cf tasks.TaskCounts, continueCmd string, duringReview bool) {
	headline := "Stopped before final review"
	if duringReview {
		headline = "Stopped before final review completed"
	}
	ui.Note("")
	ui.Note("%s · %d/%d tasks done", headline, cf.Done, cf.Total())
	ui.Note("")
	ui.Note("  Continue: %s", continueCmd)
}

// printNoActionableTasks is the honest empty result: nothing to work on. It is not a review
// verdict, so it never carries a checkmark — and when the queue is parked on decisions it says so.
func printNoActionableTasks(cf tasks.TaskCounts) {
	if cf.Blocked == 0 {
		ui.Note("No tasks ready to work on.")
		return
	}
	ui.Note("No tasks ready to work on. %s need your decision.", ui.Count(cf.Blocked, "task"))
	ui.Note("")
	ui.Note("  %s coop tasks decisions", decisionsLabel(cf.Blocked))
}

// printTaskLimitReached closes an intentional --max-tasks pause. It is a success, not a
// verification: the final review has not run, and the report says exactly that.
func printTaskLimitReached(limit loopTaskLimit, continueCmd string) {
	ui.Note("")
	if limit.settled >= limit.max {
		ui.Note("Paused after %d of %d requested %s", limit.settled, limit.max, taskNoun(limit.max))
	} else {
		ui.Note("Paused after %s — no more tasks are ready", ui.Count(limit.settled, "task"))
	}
	if limit.lastState == tasks.StateBlocked {
		ui.Note("  Blocked: %s", limit.lastTitle)
		ui.Note("")
		ui.Note("  Final review has not run.")
		ui.Note("  Answer it: coop tasks decisions")
		return
	}
	ui.Note("  Completed: %s", limit.lastTitle)
	ui.Note("")
	ui.Note("  Final review has not run.")
	ui.Note("  Continue: %s", continueCmd)
}

func taskNoun(n int) string {
	if n == 1 {
		return "task"
	}
	return "tasks"
}

// printBusyQueue reports work another live controller owns. A queue somebody else is draining is
// not a verified-done queue, so this says what it is and names the holders it actually observed.
func printBusyQueue(busy tasks.TaskLeaseSummary) {
	ui.Note("No task is available to this loop.")
	if detail := busyDetail(busy); detail != "" {
		ui.Note("  The remaining work is reserved by another agent (%s).", detail)
		return
	}
	ui.Note("  The remaining work is reserved by another agent.")
}

// busyDetail renders the observed holders as prose ("1 owned, 2 busy"), or "" when the summary
// carries no count to report.
func busyDetail(busy tasks.TaskLeaseSummary) string {
	var parts []string
	if busy.Owned > 0 {
		parts = append(parts, fmt.Sprintf("%d owned", busy.Owned))
	}
	if busy.Busy > 0 {
		parts = append(parts, fmt.Sprintf("%d busy", busy.Busy))
	}
	if busy.Stalled > 0 {
		parts = append(parts, fmt.Sprintf("%d stalled", busy.Stalled))
	}
	return strings.Join(parts, ", ")
}

const progressActivityWidth = 48

func progressState(c tasks.TaskCounts) string {
	s := fmt.Sprintf("%s/%d done", paintCount(c.Done, ui.Green), c.Total())
	if c.Blocked > 0 {
		s += fmt.Sprintf(" · %s blocked", paintCount(c.Blocked, ui.Red))
	}
	return s
}

func progressStateWidth(c tasks.TaskCounts) int {
	s := fmt.Sprintf("%d/%d done", c.Done, c.Total())
	if c.Blocked > 0 {
		s += fmt.Sprintf(" · %d blocked", c.Blocked)
	}
	return len([]rune(s))
}

// progressLine is the queue's at-a-glance state for the live bar: done/total (done greened when
// nonzero), a blocked tally only when there is one, and the task being worked. Structural queue
// state is never abbreviated; the title is what gives way on a narrow row.
func progressLine(c tasks.TaskCounts, activity string) string {
	s := progressState(c)
	if activity != "" {
		s += " · " + truncate(activity, progressActivityWidth)
	}
	return s
}

// progressLineWidth fits the optional activity into a complete line budget. On an impossibly
// narrow row Region remains the final clip guard.
func progressLineWidth(c tasks.TaskCounts, activity string, width int) string {
	s := progressState(c)
	const separator = " · "
	activityW := width - progressStateWidth(c) - len([]rune(separator))
	if activity == "" || activityW <= 0 {
		return s
	}
	return s + separator + truncate(activity, activityW)
}
