package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

var fixtureBinaries *procharness.BinaryCache

// detachedChildEnv runs this test binary as the coop CLI, the way a detached start re-executes coop
// for its background worker.
const detachedChildEnv = "COOP_TEST_DETACHED_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(detachedChildEnv) != "" {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(teststate.Isolate(func() int {
		var err error
		fixtureBinaries, err = procharness.NewBinaryCache(filepath.Join(teststate.Dir(), "binaries"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return guardHostCLIs(m.Run)
	}))
}
