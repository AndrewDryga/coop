package tasks

import "github.com/AndrewDryga/coop/internal/forkspace"

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
	older, err := rawReachableAuditCommits(repo, below)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(older))
	for _, commit := range older {
		known[commit.sha] = true
	}
	commits, err := rawReachableAuditCommitsLimit(repo, head, auditReachableHistoryLimit, auditReachableEdgeLimit, auditReachableByteLimit,
		func(parent string) bool { return known[parent] })
	if err != nil {
		return nil, err
	}
	var merges []string
	for _, commit := range commits {
		if !known[commit.sha] && commit.parents > 1 {
			merges = append(merges, commit.sha)
		}
	}
	return merges, nil
}
