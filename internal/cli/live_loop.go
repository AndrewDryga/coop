package cli

import (
	"bytes"
	"fmt"
	"sync"
	"time"

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
// bar, the done/total counts, the active task, and elapsed time — pinned below the agent's
// scrolling activity. history() funnels one line of agent/loop output into the scrollback
// above the bar so the bar stays correctly positioned. Built only for a fully interactive run.
type loopBar struct {
	region *ui.Region
	start  time.Time
	mu     sync.Mutex
	c      taskCounts
	active string
	spin   int
}

func newLoopBar(region *ui.Region, start time.Time, c taskCounts, active string) *loopBar {
	return &loopBar{region: region, start: start, c: c, active: active}
}

// line renders the bar from current state (caller holds b.mu).
func (b *loopBar) line() string {
	return fmt.Sprintf("%s %s %s %s",
		ui.SpinFrame(b.spin),
		ui.ProgressBarStates(b.c.Done, b.c.Blocked, b.c.total(), 20),
		progressLine(b.c, b.active),
		ui.Dim(elapsed(b.start)))
}

func (b *loopBar) render(history string) {
	b.mu.Lock()
	line := b.line()
	b.mu.Unlock()
	b.region.Update(history, []string{line})
}

func (b *loopBar) history(s string) { b.render(s) }

func (b *loopBar) setProgress(c taskCounts, active string) {
	b.mu.Lock()
	b.c, b.active = c, active
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
