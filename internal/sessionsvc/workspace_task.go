package sessionsvc

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// Call only after proving the controller task identity. These exact files are checkpointed
// independently of Git; ignoring the folder would also conceal unexpected user changes.
func excludeWorkspaceTaskMetadata(workspace, taskID string) error {
	if taskID == "" || taskID == "." || taskID == ".." || strings.ContainsAny(taskID, "/\\*?[]\r\n") {
		return errors.New("controller task identity cannot form an exact local Git exclusion")
	}
	if err := excludeWorkspaceTaskFile(workspace, "/"+tasks.TasksRoot+"/.coop-queue.json"); err != nil {
		return err
	}
	for _, state := range tasks.TaskStates {
		for _, name := range []string{"task.md", "log.md", "state.md", ".coop-task.json", ".coop-controller-task.json", ".coop-controller-draft.json"} {
			pattern := "/" + filepath.ToSlash(filepath.Join(tasks.TasksRoot, state, taskID, name))
			if err := excludeWorkspaceTaskFile(workspace, pattern); err != nil {
				return err
			}
		}
	}
	return nil
}

func excludeWorkspaceTaskFile(workspace, pattern string) error {
	// Git filename exclusions also match directories; reopen that exact directory so an
	// unexpected subtree cannot disappear from status or authorize a clean-workspace discard.
	if err := forkspace.Exclude(workspace, pattern); err != nil {
		return err
	}
	return forkspace.Exclude(workspace, "!"+pattern+"/")
}
