package forkctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
)

// commitLines commits a file of n lines in dir.
func commitLines(t *testing.T, dir, name string, n int) {
	t.Helper()
	writeTaskFile(t, filepath.Join(dir, name), strings.Repeat("line\n", n))
	git(t, dir, "add", name)
	git(t, dir, "commit", "-qm", name)
}

func requireChanges(t *testing.T, ws, when string, wantIns, wantDel int) {
	t.Helper()
	if ins, del := forkspace.WorkspaceChanges(ws); ins != wantIns || del != wantDel {
		t.Errorf("%s: changes = +%d −%d, want +%d −%d", when, ins, del, wantIns, wantDel)
	}
}

// After a merge that keeps the fork, coop fork ls shows no changes until the fork makes new ones. The
// merge rebased the fork onto a parent that had moved on, and the parent took its work, so counting
// from the parent commit the fork was cut from would show the parent's own commit and the merged work.
func TestAKeptMergedForkShowsNoChangesUntilItWorksAgain(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	commitLines(t, ws, "perf.txt", 2)
	commitLines(t, repo, "parent.txt", 3)
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	listing := func() string { return captureStdout(t, func() { _, _ = c.ForkLs(nil) }) }

	requireChanges(t, ws, "before the merge, with a parent-only commit", 2, 0)
	if out := listing(); !strings.Contains(out, "Changes: +2 −0") {
		t.Errorf("the listing before the merge:\n%s", out)
	}
	if landed, err := mergeOneForTest(t, c, repo, "", "perf", false); !landed || err != nil {
		t.Fatalf("merge = (%v, %v)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "perf.txt")) || !pathExists(ws) {
		t.Fatal("the work did not land, or the fork was not kept")
	}
	requireChanges(t, ws, "after the merge", 0, 0)
	if out := listing(); !strings.Contains(out, "perf") || strings.Contains(out, "Changes:") {
		t.Errorf("a merged fork lists changes it no longer has:\n%s", out)
	}
	commitLines(t, ws, "more.txt", 1)
	commitLines(t, repo, "later.txt", 4)
	requireChanges(t, ws, "after new work and another parent-only commit", 1, 0)
	if out := listing(); !strings.Contains(out, "Changes: +1 −0") {
		t.Errorf("the listing after new work:\n%s", out)
	}
}

// A merge whose checks fail leaves the fork rebased onto the parent. The parent's commits it now holds
// are not the fork's changes, so the listing keeps counting only what the fork did.
func TestAForkRebasedByAFailedMergeCountsOnlyItsOwnWork(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	commitLines(t, ws, "perf.txt", 2)
	commitLines(t, repo, "parent.txt", 3)
	c := &Control{cfg: &config.Config{ConfigDir: t.TempDir()}, gateOK: func(_, _, _ string) bool { return false }}
	landed, err := mergeOneForTest(t, c, repo, "gate-image", "perf", false)
	if landed || err == nil || !strings.Contains(err.Error(), "gate failed") {
		t.Fatalf("merge with a red gate = (%v, %v)", landed, err)
	}
	if !pathExists(filepath.Join(ws, "parent.txt")) {
		t.Fatal("the fork was not rebased onto the parent, so this proves nothing")
	}
	requireChanges(t, ws, "after the rebase of a merge that did not land", 2, 0)
}

// The same holds when the fork lands through its reviewed task candidate.
func TestATaskLandedForkShowsNoChangesAfterTheMerge(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidate(t, "task-base")
	commitLines(t, repo, "parent.txt", 3)
	requireChanges(t, ws, "before the merge", 1, 0)
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); !landed || err != nil {
		t.Fatalf("merge = (%v, %v)", landed, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
		t.Fatalf("the fork's commit did not land: %v", err)
	}
	requireChanges(t, ws, "after the task candidate landed", 0, 0)
}
