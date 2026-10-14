package acpproxy_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

var fixtureBinaries *procharness.BinaryCache

func runWithFixtureBinaries(run func() int) int {
	root, err := os.MkdirTemp("", "coop-acp-fixture-binaries-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(root)
	fixtureBinaries, err = procharness.NewBinaryCache(filepath.Join(root, "binaries"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return run()
}
