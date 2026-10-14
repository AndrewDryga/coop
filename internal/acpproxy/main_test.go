//go:build !acpe2e

package acpproxy_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Exit(runWithFixtureBinaries(m.Run))
}
