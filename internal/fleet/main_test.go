package fleet_test

import (
	"github.com/AndrewDryga/coop/internal/testutil/teststate"
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(teststate.Isolate(m.Run)) }
