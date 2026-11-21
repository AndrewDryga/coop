package loop

import (
	"fmt"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// noHeadCause says why the loop could not read HEAD: the folder is not a Git repository at all, or
// it is one with no commit yet. The fixes differ, and telling someone outside Git to commit sends
// them looking for a repository that does not exist.
func noHeadCause(repo string) string {
	if _, inGit, err := forkspace.RepositoryRoot(repo); err == nil && !inGit {
		return "This folder is not a Git repository.\nRun git init and commit the project files, then start the loop again."
	}
	return "This repository has no readable Git commit yet.\nCommit the initial project files, then start the loop again."
}

// RequireHead refuses a loop whose repository has no readable HEAD before anything is prepared:
// the container runtime, the base image and network access all come after it. Run reads HEAD
// again before its first box; this only moves the refusal ahead of that slow preparation.
func RequireHead(repo string) error {
	head, err := forkspace.GitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		ui.Failure("Could not start the loop", noHeadCause(repo))
		return ui.Reported(fmt.Errorf("read HEAD of %s: %w", repo, err))
	}
	return RefuseShallowHistory(repo, head, "")
}

// RefuseShallowHistory refuses a loop in a shallow clone whose history the audit cannot read, before
// any agent runs: every completion reads head's whole history from raw commits, so none could pass.
// fork names the fork the loop works, "" for a local loop. It changes only when the refusal comes.
func RefuseShallowHistory(repo, head, fork string) error {
	if !tasks.ShallowCheckout(repo) || tasks.HistoryReadable(repo, head) {
		return nil
	}
	if fork == "" {
		ui.Failure("Could not start the loop",
			"This checkout is a shallow clone, and Coop reads the whole history to check an attempt, so no task could be completed here. No agent ran.",
			[2]string{"Fetch the full history, then start the loop again:", "git fetch --unshallow"})
	} else {
		ui.Failure("Could not start the loop",
			"Fork "+fork+" was made from a shallow clone, and Coop reads the whole history to check an attempt, so no task could be completed in it. No agent ran.",
			[2]string{"Fetch the full history into your checkout:", "git fetch --unshallow"},
			[2]string{"Then remove this fork and start a new one:", "coop fork rm " + fork + " --force"})
	}
	return ui.Reported(fmt.Errorf("%s is a shallow clone whose history the audit cannot read", repo))
}
