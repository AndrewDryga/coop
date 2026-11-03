package loop

import (
	"fmt"

	"github.com/AndrewDryga/coop/internal/forkspace"
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
	if _, err := forkspace.GitOutErr(repo, "rev-parse", "HEAD"); err != nil {
		ui.Failure("Could not start the loop", noHeadCause(repo))
		return ui.Reported(fmt.Errorf("read HEAD of %s: %w", repo, err))
	}
	return nil
}
