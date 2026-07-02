package cli

import (
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

// TestSleepInhibitorCmd: macOS uses caffeinate tied to our pid via -w; a stripped caffeinate or a
// platform coop has no inhibitor for yields nil (the loop then runs without one).
func TestSleepInhibitorCmd(t *testing.T) {
	has := func(string) bool { return true }
	none := func(string) bool { return false }

	got := sleepInhibitorCmd("darwin", 4242, has)
	want := []string{"caffeinate", "-i", "-m", "-s", "-w", "4242"}
	if !slices.Equal(got, want) {
		t.Errorf("darwin inhibitor = %v, want %v", got, want)
	}
	if got := sleepInhibitorCmd("darwin", 1, none); got != nil {
		t.Errorf("darwin without caffeinate should be nil, got %v", got)
	}
	if got := sleepInhibitorCmd("linux", 1, has); got != nil {
		t.Errorf("linux has no wired inhibitor yet, want nil, got %v", got)
	}
}

// TestArmKeepAwakeDisabled: with COOP_CAFFEINATE off, arming is a no-op that spawns nothing and
// returns a callable stop func (so `defer armKeepAwake(cfg)()` is always safe).
func TestArmKeepAwakeDisabled(t *testing.T) {
	stop := armKeepAwake(&config.Config{Caffeinate: false})
	if stop == nil {
		t.Fatal("armKeepAwake returned a nil stop func")
	}
	stop() // must not panic
}
