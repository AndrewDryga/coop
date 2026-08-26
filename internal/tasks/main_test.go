package tasks

import (
	"fmt"
	"os"
	"testing"
)

// TestMain gives ordinary package tests a fresh task-lease authority root. Lease helper subprocesses
// deliberately inherit their parent's root so cross-process contention exercises the same host
// authority. Without this setup, leaseAuthorityRoots falls through to the developer's real
// ~/.local/state/coop/task-leases and `go test` would mutate durable completion-trust state.
func TestMain(m *testing.M) {
	if os.Getenv("COOP_LEASE_HELPER") != "" && os.Getenv(TestLeaseAuthorityRootEnv) != "" {
		os.Exit(m.Run())
	}
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
