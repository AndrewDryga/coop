package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

func TestLoopBarSupported(t *testing.T) {
	for _, c := range []struct {
		name                 string
		termProgram          string
		stdoutTTY, stderrTTY bool
		want                 bool
	}{
		{"regular terminal", "Apple_Terminal", true, true, true},
		{"terminal without identifier", "", true, true, true},
		{"Warp terminal", "WarpTerminal", true, true, true},
		{"stdout pipe", "Apple_Terminal", false, true, false},
		{"stderr pipe", "Apple_Terminal", true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := loopBarSupported(c.termProgram, c.stdoutTTY, c.stderrTTY); got != c.want {
				t.Errorf("loopBarSupported(%q, %t, %t) = %t, want %t", c.termProgram, c.stdoutTTY, c.stderrTTY, got, c.want)
			}
		})
	}
}

// The loop wires ui's live sink to the bar, so coop's own status lines (including box.Run's
// "shadowed" / "starting sibling services") scroll into the bar's history instead of overprinting
// it. This proves a ui.Info reaches the bar's region rather than going straight to stderr.
func TestLoopBarReceivesUILines(t *testing.T) {
	var buf bytes.Buffer
	region := ui.NewRegion(&buf, func() int { return 80 })
	bar := newLoopBar(region, time.Now(), taskCounts{Todo: 1}, "demo")
	ui.SetLiveSink(bar.history)
	defer ui.SetLiveSink(nil)

	ui.Info("starting sibling services (compose.yml)")
	if !strings.Contains(buf.String(), "starting sibling services") {
		t.Errorf("ui.Info should funnel into the loop bar's region, got:\n%q", buf.String())
	}
}
