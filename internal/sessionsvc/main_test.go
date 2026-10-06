package sessionsvc

import (
	"fmt"
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

// TestMain gives the package's tests a fresh task-lease authority root, as internal/tasks does. A
// child this binary starts for itself keeps the root its parent chose. Without it, the session
// workspace tests wrote the developer's real ~/.local/state/coop/task-leases.
func TestMain(m *testing.M) {
	if os.Getenv(tasks.TestLeaseAuthorityRootEnv) != "" {
		os.Exit(m.Run())
	}
	root, err := os.MkdirTemp("", "coop-test-task-leases-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv(tasks.TestLeaseAuthorityRootEnv, root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
