package forkctl

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// testWorkerArgsEnv names a file. A detached start that re-executes this test binary as its worker
// gets a stand-in that writes the command line it was given there, then waits to be stopped as a
// worker would.
const testWorkerArgsEnv = "COOP_FORKCTL_TEST_WORKER_ARGS"

// TestMain keeps the package's tests out of the developer's real Coop state; a child this binary
// starts for itself keeps the roots its parent chose. Without it, the fork merge and rm tests wrote
// the developer's real ~/.local/state/coop/task-leases.
func TestMain(m *testing.M) {
	if out := os.Getenv(testWorkerArgsEnv); out != "" {
		if os.WriteFile(out+".partial", []byte(strings.Join(os.Args[1:], "\n")+"\n"), 0o600) == nil {
			_ = os.Rename(out+".partial", out) // whole, so the test never reads half of it
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(teststate.Isolate(m.Run))
}
