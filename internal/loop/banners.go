package loop

import (
	"fmt"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// loopExitCode is the machine-readable companion to loopClosingBanner so cron/fleet/CI can branch on
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

// loopClosingBanner picks the loop's final line from the post-review queue counts: reopened work
// (todo, or reopened into in_progress) and tasks blocked on a human decision are NOT "done", so only
// a truly drained queue earns the green "verified done". With loop-until-accepted the loop normally
// exits either accepted (nothing reopened) or with the stuck task blocked, but the reopened branch
// stays as a defensive fallback (e.g. a custom work.command run). Pure, so the outcomes are
// unit-tested without running the loop.
func loopClosingBanner(cf tasks.TaskCounts, completed int) string {
	switch {
	case cf.Todo+cf.Doing > 0:
		return ui.Bold(ui.Yellow(fmt.Sprintf(
			"⚠ review left %s actionable — run 'coop loop' to work them", ui.Count(cf.Todo+cf.Doing, "task"))))
	case cf.Blocked > 0:
		// Tasks parked in 50_blocked/ on a human decision are NOT done — don't report success.
		return ui.Bold(ui.Yellow(fmt.Sprintf(
			"stopped — %d/%d done, %d blocked on a decision; resolve them (coop tasks decisions), then re-run",
			cf.Done, cf.Total(), cf.Blocked)))
	default:
		msg := fmt.Sprintf("✓ queue verified done — %d/%d", cf.Done, cf.Total())
		if completed > 0 {
			msg += fmt.Sprintf(" in %d iterations", completed)
		}
		return ui.Bold(ui.Green(msg))
	}
}

// LoopInterruptedExitCode is the conventional SIGINT status a Ctrl-C'd run exits with, so a
// cron/fleet caller can tell "you stopped it" from the queue verdicts loopExitCode reports.
const LoopInterruptedExitCode = 130

func loopInterruptedBanner(cf tasks.TaskCounts) string {
	return ui.Bold(ui.Yellow(fmt.Sprintf("■ interrupted before queue verification — %d/%d done; run 'coop loop' to resume", cf.Done, cf.Total())))
}

func loopTaskLimitBanner(cf tasks.TaskCounts, limit loopTaskLimit) string {
	if limit.settled == 0 {
		if cf.Blocked > 0 {
			return ui.Bold(ui.Yellow(fmt.Sprintf("■ task-limited run idle — no actionable task; %d blocked on a decision; no box started", cf.Blocked)))
		}
		return ui.Bold(ui.Green("✓ task-limited run idle — no actionable task; no box started"))
	}
	last := fmt.Sprintf("last: %s %s", limit.lastID, tasks.StateLabel(limit.lastState))
	if limit.settled >= limit.max {
		noun := "tasks"
		if limit.max == 1 {
			noun = "task"
		}
		msg := fmt.Sprintf("task limit reached — %d/%d %s settled (%s); paused before another task or final signoff", limit.settled, limit.max, noun, last)
		if limit.lastState == tasks.StateBlocked {
			return ui.Bold(ui.Yellow("■ " + msg))
		}
		return ui.Bold(ui.Green("✓ " + msg))
	}
	return ui.Bold(ui.Green(fmt.Sprintf("✓ task-limited run paused — %d/%d tasks settled (%s); no actionable task remains; final signoff not run", limit.settled, limit.max, last)))
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

// progressLine is the queue's at-a-glance state: done/total (done greened when nonzero), a
// blocked tally only when there is one, and the task being worked. The loop prints it both
// in the per-iteration banner and live, on its own, whenever a task changes state mid-run.
func progressLine(c tasks.TaskCounts, activity string) string {
	s := progressState(c)
	if activity != "" {
		s += " · now: " + truncate(activity, progressActivityWidth)
	}
	return s
}

// progressLineWidth fits the optional activity into a complete line budget. Structural queue
// state is never abbreviated; on an impossibly narrow row Region remains the final clip guard.
func progressLineWidth(c tasks.TaskCounts, activity string, width int) string {
	s := progressState(c)
	const separator = " · now: "
	activityW := width - progressStateWidth(c) - len([]rune(separator))
	if activity == "" || activityW <= 0 {
		return s
	}
	return s + separator + truncate(activity, activityW)
}

// progressBanner is progressLine prefixed with the iteration number, printed at the top of
// each loop iteration.
func progressBanner(n int, c tasks.TaskCounts, active string) string {
	return fmt.Sprintf("iteration %d · %s", n, progressLine(c, active))
}

func progressBannerWidth(n int, c tasks.TaskCounts, active string, width int) string {
	prefix := fmt.Sprintf("iteration %d · ", n)
	return prefix + progressLineWidth(c, active, width-len([]rune(prefix)))
}
