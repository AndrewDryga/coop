package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// The loop wires ui's live sink to the bar, so coop's own status lines (including box.Run's
// "shadowed" / "starting sibling services") scroll into the bar's history instead of overprinting
// it. This proves a ui.Info reaches the bar's region rather than going straight to stderr.
func TestLoopBarReceivesUILines(t *testing.T) {
	var buf bytes.Buffer
	region := ui.NewRegion(&buf, func() int { return 80 })
	bar := newLoopBar(region, time.Now(), taskCounts{Todo: 1}, "demo")
	ui.SetLiveSink(bar.history)
	defer ui.SetLiveSink(nil)

	ui.Info("starting sibling services (compose.agent.yml)")
	if !strings.Contains(buf.String(), "starting sibling services") {
		t.Errorf("ui.Info should funnel into the loop bar's region, got:\n%q", buf.String())
	}
}
