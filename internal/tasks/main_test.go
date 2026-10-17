package tasks

import (
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// TestMain keeps ordinary package tests out of the developer's real Coop state. Lease helper
// subprocesses deliberately inherit their parent's roots so cross-process contention exercises the
// same host authority. Without this setup, `go test` would mutate the developer's durable
// completion-trust state in ~/.local/state/coop/task-leases.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }
