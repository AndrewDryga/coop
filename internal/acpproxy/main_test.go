//go:build !acpe2e

package acpproxy_test

import (
	"os"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/teststate"
)

func TestMain(m *testing.M) {
	os.Exit(teststate.Isolate(func() int { return runWithFixtureBinaries(m.Run) }))
}
