package loop

import (
	"bytes"
	"fmt"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

const spinInterval = 120 * time.Millisecond // live-bar spinner cadence

// loopBarSupported reports whether both output streams can host the bottom-pinned main-screen
// Region. Terminal identity is deliberately irrelevant: the live bar is an interactive feature
// everywhere, including Warp.
func loopBarSupported(_ string, stdoutTTY, stderrTTY bool) bool {
	return stdoutTTY && stderrTTY
}

// loopBar is the loop's sticky bottom status while an iteration runs: a spinner, a progress
// bar, the done/total counts, the immutable activity, and elapsed time — pinned below the agent's
// scrolling activity. history() funnels one line of agent/loop output into the scrollback
// above the bar so the bar stays correctly positioned. Built only for a fully interactive run.
type loopBar struct {
	region   *ui.Region
	width    func() int
	start    time.Time
	mu       sync.Mutex
	c        tasks.TaskCounts
	activity string
	spin     int
}

func newLoopBar(region *ui.Region, width func() int, start time.Time, c tasks.TaskCounts, activity string) *loopBar {
	return &loopBar{region: region, width: width, start: start, c: c, activity: activity}
}

// line renders the bar from current state (caller holds b.mu).
func (b *loopBar) line() string {
	elapsedText := elapsed(b.start)
	width := 80
	if b.width != nil {
		width = b.width()
	}
	// Reserve the screen's no-wrap column plus the fixed spinner, bar, separators, and elapsed
	// suffix. Counts and blocked state inside progressLineWidth win over optional activity text.
	progressW := width - 1 - ui.SpinnerWidth - (20 + 2) - len([]rune(elapsedText)) - 3
	return fmt.Sprintf("%s %s %s %s",
		ui.SpinFrame(b.spin),
		ui.ProgressBarStates(b.c.Done, b.c.Doing, b.c.Blocked, b.c.Total(), 20),
		progressLineWidth(b.c, b.activity, progressW),
		ui.Dim(elapsedText))
}

func (b *loopBar) render(history string) {
	b.mu.Lock()
	line := b.line()
	b.mu.Unlock()
	b.region.Update(history, []string{line})
}

func (b *loopBar) history(s string) { b.render(s) }

func (b *loopBar) setCounts(c tasks.TaskCounts) {
	b.mu.Lock()
	b.c = c
	b.mu.Unlock()
	b.render("")
}

func (b *loopBar) tick() {
	b.mu.Lock()
	b.spin++
	b.mu.Unlock()
	b.render("")
}

func (b *loopBar) stop() { b.region.Clear() }

// spinLoop animates the bar (spinner + elapsed clock) until stop is closed.
func spinLoop(bar *loopBar, stop <-chan struct{}) {
	t := time.NewTicker(spinInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			bar.tick()
		}
	}
}

// elapsed formats the time since start as m:ss.
func elapsed(start time.Time) string {
	d := time.Since(start)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%d:%02d", int(d/time.Minute), int(d%time.Minute/time.Second))
}

// lineWriter buffers bytes and calls fn for each complete line, so the agent's streamed output
// is funneled into the live bar's scroll history one whole line at a time. flush emits a
// trailing partial line.
type lineWriter struct {
	mu  sync.Mutex
	buf []byte
	fn  func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.fn(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.fn(string(w.buf))
		w.buf = nil
	}
}
