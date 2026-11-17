package loop

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func loopWidth(n int) func() int { return func() int { return n } }

func TestLoopBarSupported(t *testing.T) {
	for _, c := range []struct {
		name                 string
		term                 string
		stdoutTTY, stderrTTY bool
		want                 bool
	}{
		{"regular terminal", "xterm-256color", true, true, true},
		{"terminal without identifier", "", true, true, true},
		{"ANSI terminal", "xterm", true, true, true},
		{"supervised dumb terminal", "dumb", true, true, false},
		{"stdout pipe", "xterm-256color", false, true, false},
		{"stderr pipe", "xterm-256color", true, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := loopBarSupported(c.term, c.stdoutTTY, c.stderrTTY); got != c.want {
				t.Errorf("loopBarSupported(%q, %t, %t) = %t, want %t", c.term, c.stdoutTTY, c.stderrTTY, got, c.want)
			}
		})
	}
}

// The loop wires ui's live sink to the bar, so coop's own status lines (including box.Run's
// "shadowed" / "starting sibling services") scroll into the bar's history instead of overprinting
// it. This proves a ui.Info reaches the bar's region rather than going straight to stderr.
func TestLoopBarReceivesUILines(t *testing.T) {
	var buf bytes.Buffer
	width := loopWidth(80)
	region := ui.NewRegion(&buf, width)
	bar := newLoopBar(region, width, time.Now(), tasks.TaskCounts{Todo: 1}, "demo")
	ui.SetLiveSink(bar.history)
	defer ui.SetLiveSink(nil)

	ui.Note("starting sibling services (compose.yml)")
	if !strings.Contains(buf.String(), "starting sibling services") {
		t.Errorf("ui.Info should funnel into the loop bar's region, got:\n%q", buf.String())
	}
}

// A blank line (ui.Note(""), a blank line of agent output) stays a blank row above the bar instead
// of vanishing, as it does when the same run is redirected.
func TestLoopBarKeepsBlankLines(t *testing.T) {
	var buf bytes.Buffer
	width := loopWidth(80)
	region := ui.NewRegion(&buf, width)
	bar := newLoopBar(region, width, time.Now(), tasks.TaskCounts{Todo: 1}, "demo")
	bar.history("first")
	bar.history("")
	bar.history("second")
	if !strings.Contains(buf.String(), "\x1b[J\x1b[K\n\x1b[K") {
		t.Errorf("blank line lost above the bar:\n%q", buf.String())
	}
	out := &lineWriter{fn: bar.history}
	buf.Reset()
	_, _ = out.Write([]byte("third\n\nfourth\n"))
	if !strings.Contains(buf.String(), "\x1b[J\x1b[K\n\x1b[K") {
		t.Errorf("blank output line lost above the bar:\n%q", buf.String())
	}
}

// A box decides how long its output lines are. The funnel holds at most maxStreamEventBytes of
// one: the line shows up to the cap, marked, the rest is dropped to its newline, and the next line
// reads normally.
func TestLineWriterCapsAnOverLongLine(t *testing.T) {
	var lines []string
	w := &lineWriter{fn: func(s string) { lines = append(lines, s) }}
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for range 32 { // 2 MiB without a newline
		_, _ = w.Write(chunk)
		if len(w.buf) > maxStreamEventBytes {
			t.Fatalf("held %d bytes, cap is %d", len(w.buf), maxStreamEventBytes)
		}
	}
	_, _ = w.Write([]byte("tail of the long line\nnext\npart"))
	w.flush()
	if len(lines) != 3 || len(lines[0]) != maxStreamEventBytes+len(" [truncated]") ||
		!strings.HasSuffix(lines[0], " [truncated]") || lines[1] != "next" || lines[2] != "part" {
		t.Fatalf("lines = %d %q…", len(lines), lines[len(lines)-1])
	}
}

func TestLoopBarSpinnerCanFreezeWithoutColor(t *testing.T) {
	saved := ui.SpinFrames
	ui.SpinFrames = []string{"S", "T"}
	defer func() { ui.SpinFrames = saved }()
	t.Setenv("COOP_SPINNER", "0")

	var buf bytes.Buffer
	width := loopWidth(80)
	bar := newLoopBar(ui.NewRegion(&buf, width), width, time.Now(), tasks.TaskCounts{Todo: 1}, "demo")
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
	bar := newLoopBar(nil, loopWidth(80), time.Now(), tasks.TaskCounts{Todo: 99, Doing: 1}, "demo")
	if line := bar.line(); !strings.ContainsAny(line, "█▒") {
		t.Errorf("loop bar should keep a visible active cell: %q", line)
	}
}

// Without color (NO_COLOR, or no terminal as under go test) the bar's states differ only by their
// characters: a queue with one task in progress and none done must not draw a full bar.
func TestLoopBarWithoutColorDoesNotReadAsComplete(t *testing.T) {
	bar := newLoopBar(nil, loopWidth(80), time.Now(), tasks.TaskCounts{Doing: 1}, "demo")
	line := bar.line()
	if strings.Contains(line, "█") || !strings.Contains(line, "["+strings.Repeat("▒", 20)+"] 0/1 done") {
		t.Errorf("a colorless bar with no task done reads as complete: %q", line)
	}
}

func TestLoopBarActivityUsesAvailableTerminalWidth(t *testing.T) {
	activity := "Reconcile every provider credential rotation before the deployment cutover"
	width := 160
	bar := newLoopBar(nil, func() int { return width }, time.Now(), tasks.TaskCounts{Doing: 1}, activity)

	wide := bar.line()
	if !strings.Contains(wide, activity) {
		t.Errorf("wide loop bar should show activity past the old fixed cap: %q", wide)
	}

	width = 60 // the callback is sampled again, just as it is after a terminal resize
	narrow := bar.line()
	if strings.Contains(narrow, activity) || !strings.Contains(narrow, "…") {
		t.Errorf("narrow loop bar should elide activity: %q", narrow)
	}
	if !strings.HasSuffix(narrow, "0:00") {
		t.Errorf("narrow loop bar should preserve elapsed suffix: %q", narrow)
	}
	if got, max := len([]rune(narrow)), width-1; got > max {
		t.Errorf("narrow loop bar width = %d, want at most %d: %q", got, max, narrow)
	}
}

// A CJK task title takes two columns a character. Measured in runes, the bar fit on paper but not
// on screen: it wrapped, and each 120 ms repaint left a stale bar row in scrollback.
func TestLoopBarFitsAWideTitle(t *testing.T) {
	title := strings.Repeat("界", 30)
	bar := newLoopBar(nil, loopWidth(80), time.Now(), tasks.TaskCounts{Todo: 3, Doing: 1, Done: 2}, title)
	line := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(bar.line(), "")
	columns := 0
	for _, r := range line {
		if unicode.Is(unicode.Han, r) {
			columns += 2
		} else {
			columns++
		}
	}
	if columns > 79 {
		t.Errorf("bar with a 30-character CJK title at width 80 = %d columns, want at most 79: %q", columns, line)
	}
	if !strings.Contains(line, "界") || !strings.Contains(line, "…") || !strings.HasSuffix(line, "0:00") {
		t.Errorf("bar should show the elided title and keep its elapsed suffix: %q", line)
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
			width := loopWidth(100)
			bar := newLoopBar(ui.NewRegion(&output, width), width, time.Now(), counts, tc.activity)
			newState := stateDone
			if moving.State == stateTodo {
				newState = stateInProgress
			}
			if err := moveTaskDir(root, moving, newState); err != nil {
				t.Fatal(err)
			}
			updated, _ := updateLoopBarCounts([]string{root}, counts, bar)
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

// The bar's 2 s tick once re-read the whole queue, a few file calls per archived task, so a loop
// waiting on its agent stayed busy over a thousand done tasks. A tick now reads only after a state
// folder changed, and refreshes anyway after progressRefresh for what the stamps cannot show.
func TestProgressTickReadsTheQueueOnlyAfterAStateFolderChanges(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateDone, "old", "task.md"), "# Old\n")
	next := taskItem{ID: "next", State: stateTodo, Dir: filepath.Join(root, stateTodo, "next")}
	writeTaskFile(t, filepath.Join(next.Dir, "task.md"), "# Next\n")
	start := time.Now()
	progress := newProgressTick([]string{root}, start)
	var output bytes.Buffer
	width := loopWidth(100)
	bar := newLoopBar(ui.NewRegion(&output, width), width, start, progress.last, "work")

	// A file removed inside a done folder changes the count but no state folder: an idle tick
	// skips the read, and the refresh catches it.
	if err := os.Remove(filepath.Join(root, stateDone, "old", "task.md")); err != nil {
		t.Fatal(err)
	}
	progress.tick(start.Add(progressPoll), bar)
	if progress.last.Done != 1 {
		t.Fatalf("an idle tick re-read the queue: %+v", progress.last)
	}
	progress.tick(start.Add(progressRefresh), bar)
	if progress.last.Done != 0 || progress.last.Todo != 1 {
		t.Fatalf("the refresh missed the change: %+v", progress.last)
	}

	// A claim moves a folder between states, which the very next tick reads.
	if err := moveTaskDir(root, next, stateInProgress); err != nil {
		t.Fatal(err)
	}
	progress.tick(start.Add(progressRefresh+progressPoll), bar)
	if progress.last.Doing != 1 || progress.last.Todo != 0 {
		t.Fatalf("a state change waited for the refresh: %+v", progress.last)
	}
}

// An attempt starts its bar from zero counts when the first queue read fails, and the next tick
// reads again. The attempt once ended there with no completion windows, and its caller crashed.
func TestProgressTickRecoversFromAFailedFirstRead(t *testing.T) {
	root := t.TempDir()
	todo := filepath.Join(root, stateTodo)
	if err := os.WriteFile(todo, []byte("not a folder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tasks.QueueProgress([]string{root}); err == nil {
		t.Skip("this queue read did not fail, so the fixture proves nothing")
	}
	start := time.Now()
	progress := newProgressTick([]string{root}, start)
	if progress.stamps != "" || progress.last != (tasks.TaskCounts{}) {
		t.Fatalf("a failed read left a baseline: %+v", progress)
	}
	var output bytes.Buffer
	width := loopWidth(100)
	bar := newLoopBar(ui.NewRegion(&output, width), width, start, progress.last, "work")
	if err := os.Remove(todo); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(todo, "next", "task.md"), "# Next\n")
	progress.tick(start.Add(progressPoll), bar)
	if progress.last.Todo != 1 {
		t.Fatalf("the tick after a failed first read did not read the queue: %+v", progress.last)
	}
}

func TestReviewActivityNamesStageAndSubjectsCompactly(t *testing.T) {
	for _, stage := range []string{"between audit", "protected audit", "signoff", "verify"} {
		if got := reviewActivity(stage, []string{"task-a"}); got != stage+": task-a" {
			t.Errorf("%s activity = %q", stage, got)
		}
	}
	first := "2026-07-14-a-very-long-task-name-that-must-reach-the-live-width-renderer"
	got := reviewActivity("signoff", []string{first, "task-b", "task-c"})
	if want := "signoff: " + first + " +2"; got != want {
		t.Errorf("multi-subject review activity = %q, want untruncated %q", got, want)
	}
}
