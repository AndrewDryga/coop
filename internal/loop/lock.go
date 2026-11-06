package loop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// stoppingHolder ends the lock note of a loop that is tearing down after a hard stop.
const stoppingHolder = ", stopping"

// stoppingWait bounds how long a loop waits for one that is stopping: past the teardown's box
// grace and its 30-second service cleanup, short of waiting on a teardown that is stuck.
var stoppingWait = time.Minute

// lockLoopCheckout makes `coop loop` exclusive PER CHECKOUT. Two loops sharing one working tree
// each commit their own task, and each one's completion range then contains the other's
// task-bound commit — so unbindableTasks rejects BOTH and reopens finished work. Measured: two
// commits five minutes apart, bound to different tasks, both completions rejected. The per-task
// lease in completionwindow.go cannot catch this; it stops two loops taking the SAME task, not
// two loops working DIFFERENT tasks in the same tree.
//
// The key is the resolved checkout path, never the repo name: parallel forks run one loop per
// WORKTREE (cli's fork command hands Run each fork's own worktree), so concurrent forks hold
// different locks. Isolate the state, don't serialize the workers.
//
// A hard stop still removes the box and the run's services before the lock is released, which
// takes seconds. markStopping says so in the lock note, and a loop started meanwhile waits for it
// instead of refusing. A closed terminal is the common case: the loop gets only the hangup, and
// whoever started the next loop cannot see it still tearing down.
func lockLoopCheckout(cfg *config.Config, repo string) (release, markStopping func(), err error) {
	checkout := repo
	if resolved, err := filepath.EvalSymlinks(checkout); err == nil {
		checkout = resolved
	} else if absolute, absErr := filepath.Abs(checkout); absErr == nil {
		checkout = absolute
	}
	waitUntil := time.Now().Add(stoppingWait)
	for waited := false; ; {
		release, holder, err := cfg.TryLock("loop", checkout)
		if err == nil {
			return release, func() {
				_ = cfg.NoteLock("loop", checkout, fmt.Sprintf("pid %d%s", os.Getpid(), stoppingHolder))
			}, nil
		}
		if !errors.Is(err, config.ErrLockHeld) {
			return nil, nil, err
		}
		stopping := strings.HasSuffix(holder, stoppingHolder)
		if stopping && time.Now().Before(waitUntil) {
			if !waited {
				waited = true
				ui.Note("Waiting for the loop that is stopping in this checkout (%s) to finish.", strings.TrimSuffix(holder, stoppingHolder))
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if holder != "" {
			holder = " (" + holder + ")"
		}
		if stopping {
			return nil, nil, fmt.Errorf("the coop loop stopping in %s%s did not finish within %s — wait for it to exit, then run this again", checkout, holder, stoppingWait)
		}
		return nil, nil, fmt.Errorf("another coop loop is already working %s%s — two loops in one checkout commit over each other and both completions get rejected; wait for it to finish, or give this one its own worktree with 'coop fork add'", checkout, holder)
	}
}
