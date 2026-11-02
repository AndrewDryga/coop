package box

import (
	"fmt"
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

// TestMain keeps the package's tests out of the developer's real Coop state.
var cleanupFilesystemRuntime func() error

func TestMain(m *testing.M) {
	ownsState := os.Getenv("COOP_TEST_STATE_ROOT") == ""
	os.Exit(teststate.Isolate(func() int {
		code := m.Run()
		if ownsState && cleanupFilesystemRuntime != nil {
			if err := cleanupFilesystemRuntime(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		return code
	}))
}
