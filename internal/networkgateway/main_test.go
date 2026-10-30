package networkgateway

import (
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// Adapter-backed tests link per-user state; keep every test in temporary roots.
func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }
