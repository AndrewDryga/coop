package cli

import (
	"os"
	"sync/atomic"
	"testing"
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
