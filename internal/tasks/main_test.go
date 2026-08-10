package tasks

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives this package's own test binary a hermetic task-lease authority root — mirrors
// internal/cli/main_test.go exactly (a separate test binary needs its own copy of this setup, the
// same reason gitOut does; see git.go). Without it, leaseAuthorityRoots falls through to the real
// ~/.local/state/coop/task-leases, and `go test` would read and write the developer's own durable
// completion-trust state.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "coop-test-task-leases-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv(TestLeaseAuthorityRootEnv, root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
