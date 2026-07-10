package cli

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// watchInterrupt is the loop's two-stage stop brain: first signal → soft, second → hard, and a
// closed channel (loop() closes it on exit) ends the watch without a spurious hard stop.
func TestWatchInterrupt(t *testing.T) {
	t.Run("first soft, second hard", func(t *testing.T) {
		sig := make(chan os.Signal, 2)
		var soft, hard atomic.Int32
		done := make(chan struct{})
		go func() {
			watchInterrupt(sig, func() { soft.Add(1) }, func() { hard.Add(1) })
			close(done)
		}()
		sig <- os.Interrupt
		sig <- os.Interrupt
		<-done
		if soft.Load() != 1 || hard.Load() != 1 {
			t.Fatalf("soft=%d hard=%d, want 1,1", soft.Load(), hard.Load())
		}
	})

	t.Run("one signal then close: soft only", func(t *testing.T) {
		sig := make(chan os.Signal, 2)
		var soft, hard atomic.Int32
		done := make(chan struct{})
		go func() {
			watchInterrupt(sig, func() { soft.Add(1) }, func() { hard.Add(1) })
			close(done)
		}()
		sig <- os.Interrupt
		close(sig) // as loop() does on exit — must return without calling onHard
		<-done
		if soft.Load() != 1 || hard.Load() != 0 {
			t.Fatalf("soft=%d hard=%d, want 1,0", soft.Load(), hard.Load())
		}
	})

	t.Run("immediate close: neither", func(t *testing.T) {
		sig := make(chan os.Signal)
		var soft, hard atomic.Int32
		done := make(chan struct{})
		go func() {
			watchInterrupt(sig, func() { soft.Add(1) }, func() { hard.Add(1) })
			close(done)
		}()
		close(sig)
		<-done
		if soft.Load() != 0 || hard.Load() != 0 {
			t.Fatalf("soft=%d hard=%d, want 0,0", soft.Load(), hard.Load())
		}
	})
}

// On the plain line-oriented path (no live bar: piped output) the notice starts on a fresh
// line, so it never glues onto the terminal's ^C echo or a partial agent line.
func TestLoopInterruptInfoStartsFreshLine(t *testing.T) {
	out := captureStderr(t, func() { loopInterruptInfo("stopping") })
	if !strings.HasPrefix(out, "\n") {
		t.Fatalf("interrupt notice must start on a fresh line after the terminal's ^C echo: %q", out)
	}
	if !strings.Contains(out, "coop: stopping\n") {
		t.Errorf("interrupt notice missing status line: %q", out)
	}
}

// While the live bar is up, the notice must go through the sink alone: the region scrolls it
// above the bar itself, and a raw stderr newline would move the cursor off the bar line and
// desync the region's erase math (the very bug that used to leak stale bar frames on Ctrl-C).
func TestLoopInterruptInfoLiveBarNoRawNewline(t *testing.T) {
	var got []string
	ui.SetLiveSink(func(s string) { got = append(got, s) })
	defer ui.SetLiveSink(nil)

	out := captureStderr(t, func() { loopInterruptInfo("stopping") })
	if out != "" {
		t.Errorf("nothing may bypass the live sink to stderr while the bar is up, got %q", out)
	}
	if len(got) != 1 || !strings.Contains(got[0], "stopping") {
		t.Errorf("notice should reach the live sink: %v", got)
	}
}
