package forkctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// hiddenByMerge commits the task's feature, then a change E on a side branch, and merges that branch
// with a resolution that drops E: the candidate's tree, the one a review reads, has no E, while a
// rebase that flattens the merge replays E onto the parent.
func hiddenByMerge(t *testing.T, ws, id string) {
	t.Helper()
	name := forkspace.GitOut(ws, "symbolic-ref", "--short", "HEAD")
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "feature.txt")
	git(t, ws, "commit", "-qm", "implement canonical task\n\nCoop-Task: "+id)
	git(t, ws, "checkout", "-q", "-b", "side")
	if err := os.WriteFile(filepath.Join(ws, "e.txt"), []byte("hidden from the review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "e.txt")
	git(t, ws, "commit", "-qm", "add e")
	git(t, ws, "checkout", "-q", name)
	git(t, ws, "merge", "-q", "--no-ff", "--no-commit", "side")
	git(t, ws, "rm", "-q", "-f", "e.txt")
	git(t, ws, "commit", "-qm", "merge side, keeping nothing of it")
	git(t, ws, "branch", "-q", "-D", "side")
	if _, err := os.Stat(filepath.Join(ws, "e.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the reviewed tree has e.txt: %v", err)
	}
}

// A candidate reviewed with a merge commit in it (as before Coop refused them at review) is not
// landed: the merge's flattening rebase would land E, which no review saw. The refusal records a
// rebase request, so the fork's loop straightens the history and the result is reviewed in full; a
// rebase that still leaves the merge is not finished, so that loop never takes the merge for done.
func TestReviewedCandidateWithAMergeCommitIsNotLanded(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-merge-hidden", false, hiddenByMerge, nil)
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	var block *ui.UsageError
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || !errors.As(err, &block) || block.Headline != "Fork "+identity.Name+"'s reviewed commits include a merge commit" ||
		!strings.Contains(err.Error(), "coop fork "+identity.Name+" --loop") {
		t.Fatalf("merge of a candidate with a merge commit = (%v, %v)", landed, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "e.txt")); !errors.Is(err, os.ErrNotExist) || forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatalf("content no review saw reached the parent (e.txt: %v)", err)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("land journal after the refusal = %v, %v", pending, err)
	}
	rebase, ok, err := tasks.ReadForkCandidateRebase(repo, identity)
	if err != nil || !ok || rebase.Onto != parent {
		t.Fatalf("rebase request = %+v, %v, %v; want one onto %s", rebase, ok, err, parent)
	}
	// The parent did not move, so onto is already in the candidate's history: only the merge makes
	// the rebase unfinished, and a loop that took it as finished would merge, be refused, and repeat.
	if got := CheckRebase(ws, identity.Name, rebase.Onto); got != RebaseMerges {
		t.Fatalf("rebase of a history with a merge = %v, want it named as keeping a merge", got)
	}
	if err := RefuseCandidateMergeCommits(repo, identity, forkspace.GitOut(ws, "rev-parse", "HEAD")); err == nil {
		t.Fatal("a review round could be frozen over the merge commit")
	}
	git(t, ws, "rebase", "-q", rebase.Onto)
	if got := CheckRebase(ws, identity.Name, rebase.Onto); got != RebaseFinished {
		t.Fatalf("straightened history = %v, want finished", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "e.txt")); err != nil {
		t.Fatalf("the straightened history the next review reads lacks E: %v", err)
	}
	if err := RefuseCandidateMergeCommits(repo, identity, forkspace.GitOut(ws, "rev-parse", "HEAD")); err != nil {
		t.Fatalf("a straight line of commits was refused: %v", err)
	}
}

// The land journal replays without the pre-land checks, so the land itself makes sure a rebase onto
// a parent with nothing new lands the reviewed tree: anything else is content no review saw.
func TestLandRefusesATreeTheRebaseChangedWhenTheParentDidNotMove(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-merge-tree", false, hiddenByMerge, nil)
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("candidate = %v, %v", ok, err)
	}
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	intent := landIntent{Version: landIntentVersion, Candidate: candidate, ParentBefore: parent, Phase: landPreparing, CreatedAt: time.Now().UTC()}
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := writeLandIntent(repo, intent); err != nil {
		t.Fatal(err)
	}
	_, landed, err := c.advanceTaskLand(repo, ws, identity.Name, "", intent)
	if landed || err == nil || !strings.Contains(err.Error(), "changed the reviewed tree") {
		t.Fatalf("land of a flattened merge = (%v, %v)", landed, err)
	}
	if forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatal("the parent moved")
	}
	if forkspace.GitOut(ws, "rev-parse", "HEAD") != candidate.Head {
		t.Fatal("the fork is not back on its reviewed commit")
	}
	if _, ok, err := readLandIntent(repo, identity); err != nil || ok {
		t.Fatalf("land journal after the refusal = %v, %v", ok, err)
	}
}

// An isolated fork's reviewed candidate with a merge commit is refused too; no rebase pass can
// straighten an isolated source, so the way on is to discard it.
func TestIsolatedCandidateWithAMergeCommitIsNotLanded(t *testing.T) {
	repo, _, _, identity, c := prepareForkTaskCandidateHistory(t, "task-merge-isolated", true, hiddenByMerge, nil)
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "include a merge commit") || !strings.Contains(err.Error(), "coop fork rm "+identity.Name+" --force") {
		t.Fatalf("isolated merge of a candidate with a merge commit = (%v, %v)", landed, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "e.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("content no review saw reached the parent: %v", err)
	}
}

// The freeze check counts only the fork's own commits: a merge commit the parent's history already
// holds is not the fork's to answer for.
func TestFreezeCheckIgnoresMergesTheParentAlreadyHas(t *testing.T) {
	repo, ws, _, identity, _ := prepareForkTaskCandidate(t, "task-merge-parent")
	git(t, repo, "checkout", "-q", "-b", "topic")
	if err := os.WriteFile(filepath.Join(repo, "topic.txt"), []byte("topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "topic.txt")
	git(t, repo, "commit", "-qm", "topic")
	git(t, repo, "checkout", "-q", "main")
	git(t, repo, "merge", "-q", "--no-ff", "-m", "merge topic", "topic")
	git(t, ws, "fetch", "-q", repo, "HEAD")
	git(t, ws, "rebase", "-q", "FETCH_HEAD")
	if err := RefuseCandidateMergeCommits(repo, identity, forkspace.GitOut(ws, "rev-parse", "HEAD")); err != nil {
		t.Fatalf("a fork on top of the parent's own merge was refused: %v", err)
	}
}
