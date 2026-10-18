package loop

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
)

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
func lockLoopCheckout(cfg *config.Config, repo string) (func(), error) {
	checkout := repo
	if resolved, err := filepath.EvalSymlinks(checkout); err == nil {
		checkout = resolved
	} else if absolute, absErr := filepath.Abs(checkout); absErr == nil {
		checkout = absolute
	}
	release, holder, err := cfg.TryLock("loop", checkout)
	if errors.Is(err, config.ErrLockHeld) {
		if holder != "" {
			holder = " (" + holder + ")"
		}
		return nil, fmt.Errorf("another coop loop is already working %s%s — two loops in one checkout commit over each other and both completions get rejected; wait for it to finish, or give this one its own worktree with 'coop fork add'", checkout, holder)
	}
	return release, err
}
