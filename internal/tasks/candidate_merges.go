package tasks

import (
	"errors"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// LastReviewedForkHead is the HEAD of the fork's last published review round in a live phase, the
// one a new round must build on, if there is one.
func LastReviewedForkHead(repo string, identity forkspace.Identity) (string, bool, error) {
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists || state.manifest.Current == nil ||
		state.manifest.Phase == forkCandidateLanded || state.manifest.Phase == forkCandidateDiscarded {
		return "", false, err
	}
	candidate, err := candidateFromManifest(repo, *state.manifest)
	if err != nil {
		return "", false, err
	}
	return candidate.Head, true, nil
}

// MergeCommitsOutside returns the merge commits in head's history that are not in below's, read
// from raw objects as the audit reads history: a graft, shallow file or replacement a box
// writes cannot hide one. A fork candidate holding a merge commit is refused, because the review sees
// the merge's own diff while `git rebase` at merge flattens it and replays every commit beneath it:
// a change that a merge resolution dropped would land without any reviewer having seen it.
func MergeCommitsOutside(repo, below, head string) ([]string, error) {
	commits, err := rawCommitsOutside(repo, below, head)
	if err != nil {
		return nil, err
	}
	var merges []string
	for _, commit := range commits {
		if commit.parents > 1 {
			merges = append(merges, commit.sha)
		}
	}
	return merges, nil
}

func firstOf(parents []string) string {
	if len(parents) == 0 {
		return ""
	}
	return parents[0]
}

// InRawHistory reports whether commit is in head's history, read from raw objects: Git's own
// answer reads a shallow file or commit-graph the box can write.
func InRawHistory(repo, commit, head string) (bool, error) {
	outside, err := rawCommitsOutside(repo, head, commit)
	return err == nil && len(outside) == 0, err
}

// ForkCommitsBase is where head's own commits, those parent's history lacks, start: the commit under
// the first of them, or head itself when it has none. The commits must be one straight line, as a
// fork candidate's are; anything else is refused, never guessed. Read from raw objects.
func ForkCommitsBase(repo, parent, head string) (string, error) {
	outside, err := rawCommitsOutside(repo, parent, head)
	if err != nil {
		return "", err
	}
	if len(outside) == 0 {
		return forkspace.GitOutErr(repo, "rev-parse", "--verify", head+"^{commit}")
	}
	own := make(map[string]bool, len(outside))
	for _, commit := range outside {
		own[commit.sha] = true
	}
	base := ""
	for _, commit := range outside {
		if commit.parents > 1 {
			return "", errors.New("the fork's commits include a merge commit")
		}
		if !own[commit.firstParent] {
			if base != "" || commit.firstParent == "" {
				return "", errors.New("the fork's commits are not one straight line on the parent's history")
			}
			base = commit.firstParent
		}
	}
	return base, nil
}
