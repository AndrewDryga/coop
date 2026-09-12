package tasks

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// UncommittedCompletionCanRetry admits only a genuine no-change attempt. Advancing the base
// after an unbound code commit would hide its diff from the next attempt's gate/signoff audit.
// A raw walk also prevents a graft, shallow boundary, or replacement from hiding an old binding.
func UncommittedCompletionCanRetry(repo, base, head, id string) bool {
	if base == "" || base != head {
		return false
	}
	status, err := gitOutErr(repo, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return false
	}
	commits, err := rawReachableAuditCommits(repo, head)
	if err != nil {
		return false
	}
	for _, commit := range commits {
		if commit.taskBindingInvalid {
			return false
		}
		for _, value := range commit.taskValues {
			if value == id || strings.HasPrefix(value, id+" ") || strings.HasPrefix(value, id+"\t") {
				return false
			}
		}
	}
	return true
}

// ParkUncommittedCompletion runs under the controller's existing task lease and ref window,
// after a repeated clean no-change refusal. It never blesses the task's claimed completion.
func ParkUncommittedCompletion(task QueuedTask) error {
	id := task.Item.ID
	if task.Item.State != StateInProgress {
		return fmt.Errorf("park uncommitted task %s from %s: want in progress", id, StateLabel(task.Item.State))
	}
	metadata, err := snapshotTaskMetadata(task.Item.Dir, "decision.md", "log.md", "state.md")
	if err != nil {
		return fmt.Errorf("snapshot uncommitted task %s: %w", id, err)
	}
	if err := MoveTaskDir(task.Root, task.Item, StateBlocked); err != nil {
		return err
	}
	dir := filepath.Join(task.Root, StateBlocked, id)
	rollback := func(cause error) error {
		metadataErr := restoreTaskMetadata(dir, metadata)
		current := task.Item
		current.State, current.Dir = StateBlocked, dir
		return errors.Join(cause, metadataErr, MoveTaskDir(task.Root, current, StateInProgress))
	}
	if previous := metadata["decision.md"]; previous.exists {
		if err := AppendTaskLogStrict(dir, "Previous decision before completion repair was exhausted:\n"+string(previous.body)); err != nil {
			return rollback(err)
		}
	}
	if err := WriteDecision(dir, id, task.Item.Title, Decision{
		Question: "Two attempts claimed completion without a task-bound commit. No source or Git history changed, so Coop preserved the task as unfinished and continued the queue.",
		Options: []string{
			"A — confirm a no-code outcome is allowed: verify the acceptance and required checks, record the actual conclusion in one meaningful decision commit, then retry.",
			"B — clarify unfinished implementation or missing acceptance: update the task and unblock it for another attempt.",
		},
		Recommendation: "Inspect the evidence in log.md and choose A only if all acceptance is genuinely met; an empty receipt cannot substitute for unfinished work or a failed gate.",
	}); err != nil {
		return rollback(err)
	}
	if err := AppendTaskLogStrict(dir, "host parked this task after two no-commit completion refusals; no completion was accepted and the clean checkout is safe for the next task"); err != nil {
		return rollback(err)
	}
	if err := NormalizeTaskState(id, dir, "blocked — completion needs a commit", "resolve decision.md, then explicitly unblock this task", "completion was refused twice without source or history changes", "this task is not complete; preserve its acceptance and required gates"); err != nil {
		return rollback(err)
	}
	return nil
}
