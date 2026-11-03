package loop

import "github.com/AndrewDryga/coop/internal/forkspace"

// noHeadCause says why the loop could not read HEAD: the folder is not a Git repository at all, or
// it is one with no commit yet. The fixes differ, and telling someone outside Git to commit sends
// them looking for a repository that does not exist.
func noHeadCause(repo string) string {
	if _, inGit, err := forkspace.RepositoryRoot(repo); err == nil && !inGit {
		return "This folder is not a Git repository.\nRun git init and commit the project files, then start the loop again."
	}
	return "This repository has no readable Git commit yet.\nCommit the initial project files, then start the loop again."
}
