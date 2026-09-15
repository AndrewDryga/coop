package loop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
)

var errCompletionBinding = errors.New("completion needs a task-bound commit")

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
	head, err := gitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read completion HEAD: %w", err)
	}
	touched := map[string]bool{id: true}
	for taskID, state := range baseline {
		if state == tasks.StateDone {
			touched[taskID] = true
		}
	}
	missing, _ := tasks.CompletionUnbindableTasks(repo, base, head, []string{id}, reopen, touched)
	if len(missing) == 0 {
		return nil
	}
	if reopen != nil {
		return fmt.Errorf("host-authorized audit rework needs zero new commits for verification-only completion, or exactly one real repair commit without a Coop-Task trailer preserving the reviewed history")
	}
	return fmt.Errorf("%w: exactly one new and reachable Coop-Task: %s binding is required; commit the verified work first (a permitted no-code decision uses a meaningful --allow-empty --only decision commit), or repair its missing trailer without including unrelated staged work; never add a second binding or rewrite an older task commit", errCompletionBinding, id)
}

func checkNoChangeCompletion(repo, base, id, baselineStatus string) error {
	head, err := gitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("read completion HEAD: %w", err)
	}
	return tasks.NoChangeCompletionAllowed(repo, base, head, id, baselineStatus)
}
