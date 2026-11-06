package forkspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeLines(t *testing.T, path string, lines int) {
	t.Helper()
	data := ""
	for i := 0; i < lines; i++ {
		data += "line\n"
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantChanges(t *testing.T, ws, when string, wantIns, wantDel int) {
	t.Helper()
	if ins, del := WorkspaceChanges(ws); ins != wantIns || del != wantDel {
		t.Errorf("%s: changes = +%d −%d, want +%d −%d", when, ins, del, wantIns, wantDel)
	}
}

// A fork's changes are what it did since its base. The base is the parent commit the fork was cut
// from, which a merge moves: to the parent commit the fork was rebased onto, then to the commit the
// parent took. The parent's own commits never count, and nothing counts for work that already landed.
func TestWorkspaceChangesCountFromTheForksBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := committedSetupRepo(t)
	ws, err := Setup(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	writeLines(t, filepath.Join(ws, "fork.txt"), 2)
	gitIn(t, ws, "add", "fork.txt")
	gitIn(t, ws, "commit", "-qm", "fork work")
	wantChanges(t, ws, "after the fork's commit", 2, 0)

	// The parent moves on by itself. Its commit is not in the fork, so it is no change of the fork's.
	writeLines(t, filepath.Join(repo, "parent.txt"), 3)
	gitIn(t, repo, "add", "parent.txt")
	gitIn(t, repo, "commit", "-qm", "parent moves")
	parentHead := GitOut(repo, "rev-parse", "HEAD")
	wantChanges(t, ws, "after a parent-only commit", 2, 0)

	// A merge rebases the fork onto the parent. From the clone's old base that would count the
	// parent's three lines as the fork's, which is what a kept fork's listing used to show.
	gitIn(t, ws, "fetch", "-q", repo)
	gitIn(t, ws, "rebase", "-q", parentHead, "perf")
	wantChanges(t, ws, "after the rebase, from the old base", 5, 0)
	if err := SetWorkspaceBase(ws, parentHead); err != nil {
		t.Fatal(err)
	}
	wantChanges(t, ws, "after the rebase, from the new base", 2, 0)

	// The parent takes the fork's work: nothing of it is a change any more.
	forkHead := GitOut(ws, "rev-parse", "HEAD")
	if err := SetWorkspaceBase(ws, forkHead); err != nil {
		t.Fatal(err)
	}
	wantChanges(t, ws, "after the parent took the work", 0, 0)
	if got := GitOut(ws, "rev-parse", "origin/HEAD"); got != forkHead {
		t.Errorf("origin/HEAD = %s, want the landed commit %s", got, forkHead)
	}

	// What the fork does next counts alone, and so do edits it has not committed.
	writeLines(t, filepath.Join(ws, "more.txt"), 1)
	gitIn(t, ws, "add", "more.txt")
	gitIn(t, ws, "commit", "-qm", "more work")
	wantChanges(t, ws, "after new work", 1, 0)
	writeLines(t, filepath.Join(repo, "later.txt"), 4)
	gitIn(t, repo, "add", "later.txt")
	gitIn(t, repo, "commit", "-qm", "the parent moves again")
	wantChanges(t, ws, "after another parent-only commit", 1, 0)
	writeLines(t, filepath.Join(ws, "fork.txt"), 5)
	wantChanges(t, ws, "with an uncommitted edit of a tracked file", 1+3, 0)

	// A commit that is not one is refused, and the base stays where it was.
	if err := SetWorkspaceBase(ws, "main"); err == nil {
		t.Error("SetWorkspaceBase accepted a name that is not a commit id")
	}
	if got := GitOut(ws, "rev-parse", "origin/HEAD"); got != forkHead {
		t.Errorf("a refused base moved origin/HEAD to %s", got)
	}
}
