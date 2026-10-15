package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"

	"github.com/AndrewDryga/coop/internal/tasks"
)

var fixtureBinaries *procharness.BinaryCache

// detachedChildEnv runs this test binary as the coop CLI, the way a detached start re-executes coop
// for its background worker.
const detachedChildEnv = "COOP_TEST_DETACHED_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(detachedChildEnv) != "" {
		os.Exit(Main(os.Args[1:]))
	}
	root, err := os.MkdirTemp("", "coop-test-task-leases-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv(tasks.TestLeaseAuthorityRootEnv, root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	fixtureBinaries, err = procharness.NewBinaryCache(filepath.Join(root, "binaries"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.RemoveAll(root)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
