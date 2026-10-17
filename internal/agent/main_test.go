package agent

import (
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// TestMain keeps the package's tests out of the developer's real Coop state.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }
