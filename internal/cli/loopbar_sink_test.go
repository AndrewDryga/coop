package cli

import (
	"bytes"
	"path/filepath"
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

func TestLoopBarSpinnerCanFreezeWithoutColor(t *testing.T) {
	saved := ui.SpinFrames
	ui.SpinFrames = []string{"S", "T"}
	defer func() { ui.SpinFrames = saved }()
	t.Setenv("COOP_SPINNER", "0")

	var buf bytes.Buffer
	bar := newLoopBar(ui.NewRegion(&buf, func() int { return 80 }), time.Now(), taskCounts{Todo: 1}, "demo")
	bar.render("")
	bar.tick()
	out := buf.String()
	if strings.Contains(out, "T") || !strings.Contains(out, "S") {
		t.Fatalf("frozen loop bar should keep its first frame, got %q", out)
	}
	if strings.Contains(out, "\033[36mS") {
		t.Fatalf("loop spinner should not carry cyan styling, got %q", out)
	}
}

func TestLoopBarShowsTinyActiveShare(t *testing.T) {
	bar := newLoopBar(nil, time.Now(), taskCounts{Todo: 99, Doing: 1}, "demo")
	if line := bar.line(); !strings.Contains(line, "█") {
		t.Errorf("loop bar should keep a visible active cell: %q", line)
	}
}

func TestLoopBarActivityDoesNotFollowQueueSelection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		activity string
		setup    func(t *testing.T, root string) taskItem
	}{
		{
			name:     "work remains assigned after completion exposes next todo",
			activity: "Assigned work",
			setup: func(t *testing.T, root string) taskItem {
				assigned := taskItem{ID: "assigned", State: stateInProgress, Dir: filepath.Join(root, stateInProgress, "assigned")}
				writeTaskFile(t, filepath.Join(assigned.Dir, "task.md"), "# Assigned work\n")
				writeTaskFile(t, filepath.Join(root, stateTodo, "next", "task.md"), "# Next queued task\n")
				return assigned
			},
		},
		{
			name:     "review remains on subject when next todo is claimed",
			activity: "signoff: reviewed-task",
			setup: func(t *testing.T, root string) taskItem {
				writeTaskFile(t, filepath.Join(root, stateDone, "reviewed-task", "task.md"), "# Reviewed task\n")
				next := taskItem{ID: "next", State: stateTodo, Dir: filepath.Join(root, stateTodo, "next")}
				writeTaskFile(t, filepath.Join(next.Dir, "task.md"), "# Next queued task\n")
				return next
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			moving := tc.setup(t, root)
			counts, _ := queueProgress([]string{root})
			var output bytes.Buffer
			bar := newLoopBar(ui.NewRegion(&output, func() int { return 100 }), time.Now(), counts, tc.activity)
			newState := stateDone
			if moving.State == stateTodo {
				newState = stateInProgress
			}
			if err := moveTaskDir(root, moving, newState); err != nil {
				t.Fatal(err)
			}
			updated := updateLoopBarCounts([]string{root}, counts, bar)
			if updated == counts {
				t.Fatal("fixture did not change queue counts")
			}
			if bar.activity != tc.activity {
				t.Errorf("bar activity drifted to %q, want %q", bar.activity, tc.activity)
			}
			if line := bar.line(); !strings.Contains(line, tc.activity) || strings.Contains(line, "Next queued task") {
				t.Errorf("bar line followed queue selection instead of fixed activity: %q", line)
			}
		})
	}
}

func TestReviewActivityNamesStageAndSubjectsCompactly(t *testing.T) {
	for _, stage := range []string{"between audit", "protected audit", "signoff", "verify"} {
		if got := reviewActivity(stage, []string{"task-a"}); got != stage+": task-a" {
			t.Errorf("%s activity = %q", stage, got)
		}
	}
	got := reviewActivity("signoff", []string{"2026-07-14-a-very-long-task-name-that-needs-truncation", "task-b", "task-c"})
	if !strings.HasPrefix(got, "signoff: 2026-07-14-") || !strings.HasSuffix(got, " +2") || len([]rune(got)) > progressActivityWidth {
		t.Errorf("multi-subject review activity = %q", got)
	}
}
