package forkctl

import (
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// TestMain keeps the package's tests out of the developer's real Coop state; a child this binary
// starts for itself keeps the roots its parent chose. Without it, the fork merge and rm tests wrote
// the developer's real ~/.local/state/coop/task-leases.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }
