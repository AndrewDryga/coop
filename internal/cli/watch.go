package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	watchPoll     = 400 * time.Millisecond // how often a live board (fleet or tasks) re-reads its state
	watchIdleExit = 3                      // consecutive settled ticks before a board auto-exits
)

// runWatchLoop drives the scaffolding shared by `coop fleet watch` and `coop tasks watch`: the
// alternate screen (enter/leave), signal handling, the poll ticker, and the settled-debounce
// auto-exit — with the final frame printed on the NORMAL screen via the LIFO-defer trick (the
// finalFrame printer is registered before screen.Leave, so it runs after it). Each board injects its
// differences: tick is called every poll — it paints the current frame (via screen.Frame) and
// returns that frame plus whether this tick counts toward the idle-exit debounce (the differing
// torn-read / startup predicates live there); done prints the closing summary after the final frame.
// Ctrl-C / SIGTERM exit 0 with no final frame.
func runWatchLoop(screen *ui.AltScreen, tick func(spin int) (frame []string, settled bool), done func()) (int, error) {
	screen.Enter()
	var finalFrame []string
	defer func() {
		if finalFrame == nil {
			return
		}
		for _, line := range finalFrame {
			fmt.Println(line)
		}
		done()
	}()
	defer screen.Leave()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	t := time.NewTicker(watchPoll)
	defer t.Stop()

	settled := 0
	for spin := 0; ; spin++ {
		frame, ok := tick(spin)
		if ok {
			settled++
		} else {
			settled = 0
		}
		if settled >= watchIdleExit {
			finalFrame = frame
			return 0, nil
		}
		select {
		case <-sig:
			return 0, nil
		case <-t.C:
		}
	}
}
