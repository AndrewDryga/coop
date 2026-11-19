package forkctl

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A fork candidate is reviewed as its diff, but `git rebase` lands it as the commits beneath that
// diff: it flattens a merge commit and replays every commit under it. A change a merge resolution
// dropped is not in the reviewed diff and comes back at merge, so a candidate holding a merge commit
// is refused when its round is frozen for review and again before it lands.

// mergeCommitsCause says which merge commits a fork's candidate holds, and why that stops it.
func mergeCommitsCause(merges []string) string {
	shown := make([]string, 0, 3)
	for _, sha := range merges {
		if len(shown) == 3 {
			break
		}
		shown = append(shown, sha[:12])
	}
	more := ""
	if len(merges) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(merges)-len(shown))
	}
	return "Commit " + strings.Join(shown, ", ") + more + " merges two lines of history. A merge lands the fork as a straight line of " +
		"its commits, which can bring back changes that the merge's own diff, the one a review reads, does not show. " +
		"Coop reviews and merges only a straight line of commits."
}

// refuseReviewedMergeCommits stops the land of an ordinary fork's reviewed candidate that holds a
// merge commit the parent's HEAD does not have, a round reviewed before Coop refused them at review.
// It records a rebase request, as a conflict does, so the fork's loop rebases the commits into a
// straight line onto the branch and the result is reviewed in full. The caller holds the lifecycle
// lock and has fetched the fork's commits into the parent.
func (c *Control) refuseReviewedMergeCommits(repo, name string, candidate tasks.ForkCandidate) error {
	merges, err := tasks.MergeCommitsOutside(repo, "HEAD", candidate.Head)
	if err != nil {
		return fmt.Errorf("%s: check its reviewed commits for merge commits: %w", name, err)
	}
	if len(merges) == 0 {
		return nil
	}
	branch := gitBranch(repo)
	if err := tasks.RequestForkCandidateRebaseLocked(repo, candidate, forkspace.GitOut(repo, "rev-parse", "HEAD"), branch); err != nil {
		return err
	}
	return ui.CommandFailed("Fork "+name+"'s reviewed commits include a merge commit",
		mergeCommitsCause(merges)+" Nothing was merged. The fork's loop rebases them into a straight line onto "+
			parentBranchWords(branch)+", and the result is reviewed again before it can merge.",
		[2]string{"Rebase and review:", "coop fork " + name + " --loop"},
		[2]string{"Then merge it:", "coop fork merge " + name},
		[2]string{"Or discard its work:", "coop fork rm " + name + " --force"})
}

// isolatedUnpublishedBase is the source commit above which an isolated fork's work is not published
// yet: the last publication's source head, or the creation base before any.
func isolatedUnpublishedBase(repo string, identity forkspace.Identity) (string, error) {
	var record isolatedSourceRecord
	ok, err := readIsolatedRecord(isolatedSourcePath(repo, identity), &record)
	if err != nil {
		return "", err
	}
	if !ok {
		return forkspace.IsolatedCreationBase(repo, identity)
	}
	if err := validateIsolatedSource(record, identity); err != nil {
		return "", errors.New("invalid isolated published source boundary")
	}
	return record.Head, nil
}

// RefuseCandidateMergeCommits refuses to freeze a review round at head when the fork's own commits,
// those its parent branch does not have, include a merge commit. An ordinary fork's are the ones the
// parent's HEAD does not reach, which the host fetches into the fork to tell; an isolated fork's are
// the ones above its published source boundary.
func RefuseCandidateMergeCommits(repo string, identity forkspace.Identity, head string) error {
	ws := forkspace.Workspace(repo, identity.Name)
	name := identity.Name
	var base string
	if _, isolated, err := isolatedForkIdentity(repo, name); err != nil {
		return err
	} else if isolated {
		if base, err = isolatedUnpublishedBase(repo, identity); err != nil {
			return err
		}
	} else {
		if base = forkspace.GitOut(repo, "rev-parse", "--verify", "HEAD^{commit}"); base == "" {
			return nil // a parent with no commit yet has nothing a merge could hide below
		}
		if err := forkspace.GitRun(ws, "fetch", "--quiet", repo, "HEAD"); err != nil {
			return fmt.Errorf("fork %s: fetching the parent into the fork to check its commits: %w", name, err)
		}
	}
	merges, err := tasks.MergeCommitsOutside(ws, base, head)
	if err != nil {
		return fmt.Errorf("fork %s: check its commits for merge commits: %w", name, err)
	}
	if len(merges) == 0 {
		return nil
	}
	// A later round must keep the last reviewed round's commits, so the straight line starts there
	// when that round has no merge of its own.
	if reviewed, ok, err := tasks.LastReviewedForkHead(repo, identity); err == nil && ok {
		if below, err := tasks.MergeCommitsOutside(ws, base, reviewed); err == nil && len(below) == 0 {
			base = reviewed
		}
	}
	return MergeCommitsError(ws, name, base, merges, " Nothing was reviewed.")
}

// MergeCommitsError is the one refusal of a fork whose commits above base include a merge commit,
// which loop, merge and ls all give: the git rebase onto base that makes them a straight line, then
// the loop. A rebase flattens a merge, so a change a merge resolution dropped comes back, and the
// review of the straight line sees it.
func MergeCommitsError(ws, name, base string, merges []string, tail string) error {
	cause := "They include a merge commit. Coop reviews and merges only a straight line of commits."
	if len(merges) > 0 {
		cause = mergeCommitsCause(merges)
	}
	return ui.CommandFailed("Fork "+name+"'s commits include a merge commit", cause+tail,
		[2]string{"Make its commits a straight line, then review them:", shellLine([]string{"git", "-C", ws, "rebase", base}) + " && coop fork " + name + " --loop"},
		[2]string{"Or discard its work:", "coop fork rm " + name + " --force"})
}
