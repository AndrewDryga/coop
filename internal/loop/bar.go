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
// everywhere, including Warp. TERM=dumb explicitly disables cursor control, including in a PTY
// used to supervise a real interactive invocation without recording progress-region repaints.
func loopBarSupported(term string, stdoutTTY, stderrTTY bool) bool {
	return term != "dumb" && stdoutTTY && stderrTTY
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
	progressW := width - 1 - ui.SpinnerWidth - (20 + 2) - ui.DisplayWidth(elapsedText) - 5
	return fmt.Sprintf("%s %s %s · %s",
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

// history scrolls one line above the bar. An empty line is a real blank row: the region reads an
// empty history as "nothing to print", so it is sent as a lone newline.
func (b *loopBar) history(s string) {
	if s == "" {
		s = "\n"
	}
	b.render(s)
}

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

// elapsed formats the time since start as mm:ss — a fixed-width clock, so the bar's right edge
// stops jumping a column at every tenth minute.
func elapsed(start time.Time) string {
	d := time.Since(start)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%02d:%02d", int(d/time.Minute), int(d%time.Minute/time.Second))
}

// lineWriter buffers bytes and calls fn for each complete line, so the agent's streamed output
// is funneled into the live bar's scroll history one whole line at a time. flush emits a
// trailing partial line.
type lineWriter struct {
	mu       sync.Mutex
	buf      []byte
	dropping bool // the rest of an over-long line is skipped up to its newline
	fn       func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		line := p
		if i >= 0 {
			line = p[:i]
		}
		if !w.dropping {
			w.appendFragment(line)
		}
		if i < 0 {
			break
		}
		if !w.dropping {
			w.fn(string(w.buf))
		}
		w.buf, w.dropping = nil, false
		p = p[i+1:]
	}
	return written, nil
}

// appendFragment holds part of a line up to maxStreamEventBytes. A box decides how long its lines
// are, so a longer one is shown up to the cap, marked, and the rest of it dropped up to the next
// newline, as the stderr filter does, instead of growing host memory without bound.
func (w *lineWriter) appendFragment(p []byte) {
	remaining := maxStreamEventBytes - len(w.buf)
	if len(p) <= remaining {
		w.buf = append(w.buf, p...)
		return
	}
	w.buf = append(w.buf, p[:remaining]...)
	w.fn(string(w.buf) + " [truncated]")
	w.buf, w.dropping = nil, true
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.fn(string(w.buf))
	}
	w.buf, w.dropping = nil, false
}
