package tasks

import (
	"errors"
	"fmt"
)

// ErrIncompleteChecklist distinguishes unfinished work from a failed metadata
// write or cleanup. A checked item is still not independent verification evidence.
var ErrIncompleteChecklist = errors.New("task checklist is unfinished")

// RequireCompletedChecklist enforces the structural completion contract using
// the same parsed checkboxes as task progress, including its code-fence rules.
func RequireCompletedChecklist(task Item) error {
	done, total := task.doneSubtasks(), len(task.Subtasks)
	if total > 0 && done == total {
		return nil
	}
	action := "finish the remaining work and required checks; pending verification is not complete"
	if total == 0 {
		action = "record a nonempty, truthful checklist before completing"
	}
	return fmt.Errorf("%w: %s has %d/%d subtasks done; %s", ErrIncompleteChecklist, task.ID, done, total, action)
}

func requireCurrentCompletedChecklist(taskDir string) error {
	task, ok, err := parseTaskFolder(taskDir, StateDone)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("completion needs a readable task.md in %s", taskDir)
	}
	return RequireCompletedChecklist(task)
}
