package sessionsvc

import (
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// TestMain keeps the package's tests out of the developer's real Coop state; a child this binary
// starts for itself keeps the roots its parent chose. Without it, the session workspace tests wrote
// the developer's real ~/.local/state/coop/task-leases.
//
// The ACP child helper is the exception. The session service starts it with every COOP_ variable
// but COOP_TEST_SESSION_* removed, so it would get a folder of its own, and the service stops it
// before that folder could be removed. It touches no Coop state; if it ever did, Coop's refusal of
// an unnamed root would fail it.
func TestMain(m *testing.M) {
	if os.Getenv("COOP_TEST_SESSION_CHILD") == "1" {
		os.Exit(m.Run())
	}
	os.Exit(teststate.Isolate(m.Run))
}
