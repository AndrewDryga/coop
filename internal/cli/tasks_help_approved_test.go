package cli

import "testing"

// Each task page ends with its own next step, so none of them gets the generic
// "Run 'coop help' for all commands." footer.
func TestApprovedTaskHelpPagesEndThemselves(t *testing.T) {
	for _, key := range []string{"tasks", "tasks ls", "tasks add", "tasks block", "tasks watch"} {
		if !selfContained(key) {
			t.Errorf("%q must print without the all-commands footer", key)
		}
	}
}
