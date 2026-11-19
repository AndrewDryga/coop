package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const (
	forkCandidateRebaseVersion = 1
	forkCandidateBranchLimit   = 255
	// forkRebaseVerificationLimit bounds the rebase pass's reported checks a request carries.
	forkRebaseVerificationLimit = 4096
)

// forkCandidateRebase records that a reviewed round's merge conflicted with the parent branch, so the
// fork's loop rebases that round onto the parent's exact commit and reviews the result as the next
// round. It is open only while the round it names is the manifest's current one in a live phase:
// publishing the next round, a land or a discard closes it without touching the file. It lives
// beside the manifest rather than in it, so a build that predates it reads the manifest unchanged.
// Only the host writes it: the merge that conflicted, and the loop's rebase pass, which moves Onto
// to where Branch is when the pass starts and then keeps what the pass reported checking.
type forkCandidateRebase struct {
	Version     int                `json:"version"`
	Fork        forkspace.Identity `json:"fork"`
	CandidateID string             `json:"candidate_id"`
	Onto        string             `json:"onto"`
	Branch      string             `json:"branch"`
	// Verification is the checks the rebase pass's agent reported running on the rebased tree
	// VerifiedTree, recorded by the host from the pass's final reply; empty until a pass reports.
	Verification string    `json:"verification,omitempty"`
	VerifiedTree string    `json:"verified_tree,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ForkCandidateRebase is an open rebase request: the parent commit the reviewed round is to be
// rebased onto, and the parent branch that commit was the head of when the host recorded it.
// Verification is what the rebase pass reported checking on the tree VerifiedTree, if it did.
type ForkCandidateRebase struct {
	Onto         string
	Branch       string
	Verification string
	VerifiedTree string
}

func forkCandidateRebasePath(repo string, identity forkspace.Identity) string {
	return filepath.Join(forkspace.StateDir(repo), identity.Name+"."+string(identity.Generation)+".candidate-rebase.json")
}

func validRebaseRequest(rebase forkCandidateRebase, identity forkspace.Identity) bool {
	return rebase.Version == forkCandidateRebaseVersion && rebase.Fork == identity && validAssignmentID(rebase.CandidateID) &&
		rebase.Onto != "" && validCommitID(rebase.Onto) &&
		rebase.Branch != "" && ValidTaskText(rebase.Branch, false, forkCandidateBranchLimit) && !rebase.CreatedAt.IsZero() &&
		(rebase.Verification == "") == (rebase.VerifiedTree == "") && validCommitID(rebase.VerifiedTree) &&
		(rebase.Verification == "" || ValidTaskText(rebase.Verification, true, forkRebaseVerificationLimit))
}

// openForkCandidateRebase returns the request for the manifest's current round, or nil when there is
// none open. A malformed request refuses rather than reading as none.
func openForkCandidateRebase(repo string, manifest forkCandidateManifest) (*forkCandidateRebase, error) {
	if manifest.Current == nil || manifest.Phase == forkCandidateLanded || manifest.Phase == forkCandidateDiscarded {
		return nil, nil
	}
	data, exists, err := readForkCandidateControlFile(forkCandidateRebasePath(repo, manifest.Fork), "fork candidate rebase request")
	if err != nil || !exists {
		return nil, err
	}
	var rebase forkCandidateRebase
	if err := decodeStrictTaskJSON(bytes.NewReader(data), &rebase, "fork candidate rebase request"); err != nil {
		return nil, err
	}
	if !validRebaseRequest(rebase, manifest.Fork) {
		return nil, errors.New("invalid fork candidate rebase request")
	}
	if rebase.CandidateID != manifest.Current.ID {
		return nil, nil
	}
	return &rebase, nil
}

// buildsOnReviewedRound is the ancestry fence for the next review round: HEAD descends from the
// reviewed round, or, while that round's open rebase request names a parent commit, from that commit
// with commits of its own. Either way only a full review of the exact new HEAD makes it landable.
func buildsOnReviewedRound(repo string, manifest forkCandidateManifest, reviewedHead, head string) (bool, error) {
	workspace := forkspace.Workspace(repo, manifest.Fork.Name)
	if _, err := forkspace.GitOutErr(workspace, "merge-base", "--is-ancestor", reviewedHead, head); err == nil {
		return true, nil
	}
	rebase, err := openForkCandidateRebase(repo, manifest)
	if err != nil || rebase == nil || head == rebase.Onto {
		return false, err
	}
	_, err = forkspace.GitOutErr(workspace, "merge-base", "--is-ancestor", rebase.Onto, head)
	return err == nil, nil
}

// activeRebaseManifest is the manifest a rebase request may be written against: candidate is the
// active, current round.
func activeRebaseManifest(repo string, candidate ForkCandidate) (forkCandidateManifest, error) {
	state, exists, err := readForkCandidateState(repo, candidate.Fork)
	if err != nil || !exists {
		return forkCandidateManifest{}, errors.Join(err, errors.New("fork candidate disappeared before its rebase request"))
	}
	manifest := *state.manifest
	if manifest.Phase != forkCandidateActive || manifest.Current == nil || manifest.Current.ID != candidate.ID {
		return forkCandidateManifest{}, errors.New("fork candidate changed before its rebase request")
	}
	return manifest, nil
}

func newForkCandidateRebase(identity forkspace.Identity, candidateID, onto, branch string) forkCandidateRebase {
	return forkCandidateRebase{
		Version: forkCandidateRebaseVersion, Fork: identity, CandidateID: candidateID, Onto: onto, Branch: branch,
		CreatedAt: time.Now().UTC(),
	}
}

func writeForkCandidateRebase(repo string, rebase forkCandidateRebase) error {
	if !validRebaseRequest(rebase, rebase.Fork) {
		return errors.New("fork candidate rebase request needs an exact parent commit and branch, and a bounded verification")
	}
	body, err := json.Marshal(rebase)
	if err != nil {
		return err
	}
	root, err := forkspace.OpenStateRoot(repo, "", true)
	if err != nil {
		return err
	}
	defer root.Close()
	tmp, name, err := forkspace.CreateTempIn(root, "."+rebase.Fork.Name+".candidate-rebase-")
	if err != nil {
		return err
	}
	defer root.Remove(name)
	_, writeErr := tmp.Write(append(body, '\n'))
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	return root.Rename(name, filepath.Base(forkCandidateRebasePath(repo, rebase.Fork)))
}

// RequestForkCandidateRebaseLocked records that the active reviewed candidate's commits conflict
// with the parent commit onto, the head of branch, so the fork's loop rebases them there and reviews
// the result as a new round. It replaces any earlier request. The caller holds the fork lifecycle
// lock and has retired its land journal. What may land does not change: the reviewed candidate stays
// active until the next round is published.
func RequestForkCandidateRebaseLocked(repo string, candidate ForkCandidate, onto, branch string) error {
	if _, err := activeRebaseManifest(repo, candidate); err != nil {
		return err
	}
	return writeForkCandidateRebase(repo, newForkCandidateRebase(candidate.Fork, candidate.ID, onto, branch))
}

// RefreshForkCandidateRebase moves the open request of the active candidate to onto, where branch is
// now, so the rebase pass about to start rebases onto the parent the next merge meets, and the next
// review covers what lands from there. It changes nothing once the request is closed.
func RefreshForkCandidateRebase(repo string, candidate ForkCandidate, onto, branch string) error {
	return updateOpenForkCandidateRebase(repo, candidate, func(*forkCandidateRebase) forkCandidateRebase {
		return newForkCandidateRebase(candidate.Fork, candidate.ID, onto, branch)
	})
}

// RecordForkCandidateRebaseVerification keeps what the rebase pass reported checking on the rebased
// tree with the open request, so the next review of exactly that tree has the rebase's own
// verification: the reviewed round's handoff predates the conflict resolution.
func RecordForkCandidateRebaseVerification(repo string, candidate ForkCandidate, tree, verification string) error {
	return updateOpenForkCandidateRebase(repo, candidate, func(open *forkCandidateRebase) forkCandidateRebase {
		next := *open
		next.Verification, next.VerifiedTree = verification, tree
		return next
	})
}

func updateOpenForkCandidateRebase(repo string, candidate ForkCandidate, change func(*forkCandidateRebase) forkCandidateRebase) error {
	unlock, err := forkspace.LockState(repo, candidate.Fork.Name)
	if err != nil {
		return err
	}
	defer unlock()
	manifest, err := activeRebaseManifest(repo, candidate)
	if err != nil {
		return err
	}
	open, err := openForkCandidateRebase(repo, manifest)
	if err != nil || open == nil {
		return errors.Join(err, errors.New("fork candidate has no open rebase request"))
	}
	return writeForkCandidateRebase(repo, change(open))
}

// RebaseBindingProblems compares the Coop-Task bindings of the fork's own commits before its rebase
// onto onto (onto..before, the commits a rebase replays) with those after it (onto..after), and says
// what changed, one sentence per binding: a rebase keeps every commit message, so each task keeps
// exactly the bound commits it had, none dropped or doubled, and no commit binds a task it did not
// bind before. Commits without a trailer, such as a fix the rebase needed, are not compared.
//
// Both sides are read from raw objects with the audits' own trailer parser, so a shallow file, graft
// or replacement the box writes cannot change what either counts.
func RebaseBindingProblems(workspace, onto, before, after string) ([]string, error) {
	count := func(head string) (map[string]int, error) {
		commits, err := rawCommitsOutside(workspace, onto, head)
		if err != nil {
			return nil, err
		}
		counts := map[string]int{}
		for _, commit := range commits {
			if len(commit.taskValues) == 0 && !commit.taskBindingInvalid {
				continue
			}
			key := strings.Join(commit.taskValues, ", ")
			if commit.taskBindingInvalid {
				key = "a malformed trailer (" + key + ")"
			}
			counts[key]++
		}
		return counts, nil
	}
	was, err := count(before)
	if err != nil {
		return nil, err
	}
	is, err := count(after)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(was)+len(is))
	for key := range was {
		keys = append(keys, key)
	}
	for key := range is {
		if _, ok := was[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var problems []string
	for _, key := range keys {
		if was[key] != is[key] {
			problems = append(problems, fmt.Sprintf("Coop-Task: %s is on %d commit(s) after the rebase and was on %d before it.", markHidden(key), is[key], was[key]))
		}
	}
	return problems, nil
}

// rawCommitsOutside is every commit in head's history that is not in below's, from raw objects; below
// need not be an ancestor of head. The whole of below's history is read: bounding the walk by commit
// dates, or by Git's own reachability, would trust what a box can write.
func rawCommitsOutside(repo, below, head string) ([]rawReachableAuditCommit, error) {
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
	outside := make([]rawReachableAuditCommit, 0, len(commits))
	for _, commit := range commits {
		if !known[commit.sha] {
			outside = append(outside, commit)
		}
	}
	return outside, nil
}

// ReadForkCandidateRebase returns the fork's open rebase request: from the conflicting merge until
// the rebased round is published, or a land or discard closes it.
func ReadForkCandidateRebase(repo string, identity forkspace.Identity) (ForkCandidateRebase, bool, error) {
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists {
		return ForkCandidateRebase{}, false, err
	}
	rebase, err := openForkCandidateRebase(repo, *state.manifest)
	if err != nil || rebase == nil {
		return ForkCandidateRebase{}, false, err
	}
	return ForkCandidateRebase{Onto: rebase.Onto, Branch: rebase.Branch, Verification: rebase.Verification, VerifiedTree: rebase.VerifiedTree}, true, nil
}
