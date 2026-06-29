package cli

import (
	"bytes"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// runWatchLoop auto-exits after watchIdleExit consecutive settled ticks, then runs done. Drives the
// shared loop headlessly via a buffer-backed alt-screen (no TTY needed).
func TestRunWatchLoop(t *testing.T) {
	screen := ui.NewAltScreen(&bytes.Buffer{}, func() int { return 80 })
	ticks, doneCalled := 0, false
	code, err := runWatchLoop(screen, func(spin int) ([]string, bool) {
		ticks++
		return []string{"frame"}, true // always settled → the debounce reaches watchIdleExit and exits
	}, func() { doneCalled = true })
	if code != 0 || err != nil {
		t.Fatalf("runWatchLoop = (%d, %v), want (0, nil)", code, err)
	}
	if ticks != watchIdleExit {
		t.Errorf("tick called %d times, want %d (exit after watchIdleExit settled ticks)", ticks, watchIdleExit)
	}
	if !doneCalled {
		t.Error("done callback must run on auto-exit")
	}
}
