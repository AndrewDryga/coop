package loop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

var errCompletionBinding = errors.New("implementation completion needs a task-bound commit")

// errAuditReworkOutcome names the call that re-closes review rework: workers whose finding proved
// false sent outcome already_satisfied first, because "already satisfied" is what the tool's
// description offers, and were refused.
var errAuditReworkOutcome = errors.New("audit rework already has a task binding, so outcome is refused: re-close it with a normal completion, calling tasks_complete without outcome; a verification-only re-close needs zero new commits")

const maxWorkerTerminalCorrections = 2

func workerTerminalCorrectionPrompt(id string, correction int, refusal taskmcp.AssignedTerminalRefusal) string {
	detail := refusal.Detail
	if detail == "" {
		detail = fmt.Sprintf("task %s is still in progress: finish it with tasks_complete, or use tasks_block only when a human decision is genuinely required", id)
	}
	action := refusal.Action
	if action == "" {
		action = "terminal task action"
	}
	return fmt.Sprintf("TERMINAL TASK CORRECTION ONLY (%d of %d). Do not inspect or re-analyze source, invoke delegates or reviewers, rerun tests, or restart services. Keep the work and context already in this session. Repair only the rejected %s action described below, update the task record only if that repair requires it, then call tasks_complete or tasks_block.\n\nValidation error:\n%s",
		correction, maxWorkerTerminalCorrections, action, boundedCorrectionDetail(detail))
}

func boundedCorrectionDetail(detail string) string {
	const maxRunes = 2048
	detail = strings.TrimSpace(detail)
	if len([]rune(detail)) <= maxRunes {
		return detail
	}
	return truncate(detail, maxRunes)
}

// checkAssignedCompletion is early feedback, not completion authority. The box is still running,
// so the full post-exit ref/lease/window audit must recheck everything before accepting the move.
func checkAssignedCompletion(repo, base, id string, reopen *tasks.AuditReopenRecord, baseline map[string]string) error {
	head, err := forkspace.GitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read completion HEAD: %w", err)
	}
	done := map[string]bool{}
	for taskID, state := range baseline {
		if state == tasks.StateDone {
			done[taskID] = true
		}
	}
	touched := touchedTasks(id, nil, nil, done)
	missing, _ := tasks.CompletionUnbindableTasks(repo, base, head, []string{id}, reopen, touched)
	if len(missing) == 0 {
		return nil
	}
	if reopen != nil {
		return fmt.Errorf("host-authorized audit rework needs zero new commits for verification-only completion, or exactly one real repair commit without a Coop-Task trailer preserving the reviewed history")
	}
	if bound := tasks.CommitsForTask(repo, base, id); len(bound) == 1 && len(tasks.CommitsForTask(repo, base+".."+head, id)) == 0 {
		return fmt.Errorf("%w: %s", errCompletionBinding, boundReworkRecovery(id, bound[0]))
	}
	cause := ""
	if problems := tasks.UnbindableTaskProblems(repo, base, head, missing, touched); len(problems) > 0 {
		cause = " (" + strings.Join(problems, "; ") + ")"
	}
	return fmt.Errorf("%w%s: exactly one new and reachable Coop-Task: %s binding is required; commit the verified implementation work, or repair its missing trailer without including unrelated staged work; never add a second binding or rewrite an older task commit. If existing implementation already satisfies the task, retry tasks_complete with outcome already_satisfied; an investigation may use could_not_reproduce or wont_fix. Include concrete reason and evidence after required verification. No-change completion requires unchanged Git history and checkout/index state, with no existing task binding. Do not create an empty commit; human decisions use tasks_block", errCompletionBinding, cause, id)
}

// boundReworkRecovery answers a task that was bound before this attempt and gained no binding in it:
// reopened work, usually committed as a follow-up. Both retries workers reached for fail — a normal
// completion needs a binding in this attempt's range, and an outcome is refused for a bound task —
// and two forks stopped for a person this way, so it names the shape that passes: the rework folded
// into the bound commit, while nothing but that rework sits after it.
func boundReworkRecovery(id, bound string) string {
	return fmt.Sprintf("task %[1]s already has its bound commit %[2]s from before this attempt, so outcome is refused and a commit after it does not bind: put this task's rework into that commit. If `git log %[2]s..HEAD` shows only this task's own rework, run `git reset --soft %[2]s` (your changes stay in the checkout), then `git commit --amend --no-edit --only -- <changed paths>`; when nothing needed changing, run `git commit --amend --only --no-edit --trailer \"Coop-Recovery: <current UTC timestamp>\"` instead. Then retry tasks_complete without outcome. If another task's commit sits after %[2]s, never rewrite it: block with tasks_block", id, bound)
}

// touchedTasks maps every task id a binding check must refuse a foreign trailer for to why it
// counts: the leased task, the task held under review-reopen authority, each archive the completion
// window saw change, and each task already done when the window opened. All of it is host-side
// knowledge the box cannot write. Later entries win, so the most specific reason is the one quoted.
func touchedTasks(leased string, reopen *tasks.AuditReopenRecord, changed []tasks.QueuedTask, alreadyDone map[string]bool) map[string]string {
	touched := map[string]string{}
	for id := range alreadyDone {
		touched[id] = "was already done before this attempt started"
	}
	for _, t := range changed {
		if touched[t.Item.ID] != "" {
			touched[t.Item.ID] = "was already done, and its archived folder changed during this attempt"
		} else {
			touched[t.Item.ID] = "moved to done during this attempt"
		}
	}
	if reopen != nil {
		touched[reopen.TaskID] = "is under review rework in this attempt"
	}
	touched[leased] = "is the task this attempt works on"
	return touched
}

// failRejectedCompletion tells the operator why the run stopped on a task binding Coop cannot verify:
// problems name each commit and task that broke it, and the next row how to retry.
func failRejectedCompletion(active, id, continueCmd string, problems []string) {
	cause := "The new commit range does not verify one unique task binding."
	for _, problem := range problems {
		cause += "\n" + capitalize(problem) + "."
	}
	ui.Failure("Task completion rejected",
		cause+fmt.Sprintf("\n%s is still in progress; its work and recovery notes are preserved.\nCoop cannot safely advance to another task.", active),
		[2]string{"Inspect:", "coop tasks path " + id},
		[2]string{"Retry this task:", continueCmd})
}

func checkNoChangeCompletion(repo, base, id, baseline string) error {
	head, err := forkspace.GitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read completion HEAD: %w", err)
	}
	return tasks.NoChangeCompletionAllowed(repo, base, head, id, baseline)
}
