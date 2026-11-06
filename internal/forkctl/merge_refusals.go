package forkctl

import (
	"errors"
	"fmt"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The refusals a merge draws in the shared block. The code under it returns typed errors whose own
// text is one plain line, which a batch merge and a log show as it is; the merge command turns them
// into the block at its edge, as internal/cli does for a failure reported as data.

// forkTrackedChangesError is a fork with uncommitted changes to tracked files. Git refuses to rebase
// over them, so a merge refuses first, before it touches the fork or asks Git anything.
type forkTrackedChangesError struct{ name string }

func (e *forkTrackedChangesError) Error() string {
	return "fork " + e.name + " has uncommitted changes to tracked files — commit or stash them in the fork, then merge again"
}

// gateStartError is project checks that never ran, because the box for them could not start. They
// neither passed nor failed, so a merge says that, with the box's own reason and what it asks for.
type gateStartError struct {
	name  string
	cause error
}

func (e *gateStartError) Error() string {
	return fmt.Sprintf("%s: the project checks could not start, so nothing was merged — %v", e.name, e.cause)
}

func (e *gateStartError) Unwrap() error { return e.cause }

// requireTrackedClean refuses a fork whose tracked files differ from its last commit. An untracked
// file does not stop a rebase, and a merge keeps such a fork with a note afterwards. A rebase a
// crashed land left unfinished is the merge's to recover (recoverInterruptedRebase), and its status
// shows that rebase's own changes, so it is not judged here.
func requireTrackedClean(ws, name string) error {
	if leftoverRebaseState(ws) != "" || untrustedRebaseState(ws) != "" {
		return nil
	}
	changes, err := forkspace.GitOutErr(ws, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fmt.Errorf("inspect fork %s worktree: %w", name, err)
	}
	if changes != "" {
		return &forkTrackedChangesError{name: name}
	}
	return nil
}

// mergeRefusal is err as the block a person reads, or nil when err has no plain form of its own.
func mergeRefusal(err error) *ui.UsageError {
	var start *gateStartError
	var tracked *forkTrackedChangesError
	var activity *forkspace.ForkActivityError
	var block *ui.UsageError
	switch {
	case errors.As(err, &start):
		return ui.CommandFailed("The project checks could not start for fork "+start.name, start.cause.Error(),
			[2]string{"", "Nothing was merged. Fix this, then merge again."})
	case errors.As(err, &tracked):
		return ui.CommandFailed("Fork "+tracked.name+" has uncommitted changes to tracked files",
			"Merging rebases the fork, and Git refuses to rebase over them.",
			[2]string{"", "Commit or stash them in the fork, then merge again."},
			[2]string{"Open the fork:", "coop fork open " + tracked.name})
	case errors.As(err, &activity):
		return forkBusyBlock(activity)
	case errors.As(err, &block):
		return block
	}
	return nil
}

// forkBusyBlock says a fork's box is still there, by how it stands, in words that do not need the
// sandbox's record: that is Coop's bookkeeping, and nothing a person could act on.
func forkBusyBlock(activity *forkspace.ForkActivityError) *ui.UsageError {
	name := activity.Fork
	switch activity.State {
	case forkspace.ActivityRunning:
		return ui.CommandFailed("Fork "+name+" is still running",
			"Its agent has a box open, and merging would change the files under it.",
			[2]string{"", "Exit its agent or run coop fork stop " + name + ", then merge again."})
	case forkspace.ActivityCleanupPending:
		return ui.CommandFailed("Fork "+name+" has a stopped box left to clean up",
			"Merging waits until Coop has removed it.",
			[2]string{"", "Run coop fork stop " + name + ", then merge again."})
	}
	return ui.CommandFailed("Coop cannot tell whether fork "+name+" is still running",
		"It has a box Coop cannot check.",
		[2]string{"", "Run coop fork ls to see why, then merge again."})
}

// blockExit is the exit status of a refusal drawn as a block: its own, or 2 for rejected input.
func blockExit(block *ui.UsageError) int {
	if block.ExitCode != 0 {
		return block.ExitCode
	}
	return 2
}
