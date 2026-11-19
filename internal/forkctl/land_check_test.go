package forkctl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// The box writes the reviewed commit into the fork's shallow file, so git treats it as a root: the
// land rebase would replay it as all-new (a deletion it made would not land) and git's ancestry
// answers would skip the tree check. The land refuses a shallow fork, and so does the rebase check.
func TestLandRefusesAShallowFork(t *testing.T) {
	deletes := func(t *testing.T, ws, id string) {
		git(t, ws, "rm", "-q", "README.md")
		git(t, ws, "commit", "-qm", "remove the readme\n\nCoop-Task: "+id)
	}
	repo, ws, _, identity, c := prepareForkTaskCandidateHistory(t, "task-shallow", false, deletes, nil)
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("candidate = %v, %v", ok, err)
	}
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(ws, ".git", "shallow"), []byte(candidate.Head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "repository is shallow") || !strings.Contains(err.Error(), "rm ") {
		t.Fatalf("merge of a shallow fork = (%v, %v)", landed, err)
	}
	if forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatal("a shallow fork's merge moved the parent")
	}
	if got := CheckRebase(ws, identity.Name, parent); got != RebaseShallow {
		t.Fatalf("rebase check of a shallow fork = %v, want shallow", got)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("land journal after the refusal = %v, %v", pending, err)
	}
}

// Reviewed commits A (removes the readme's line), B (adds it back) and C (adds g.txt): the reviewed
// change is g.txt alone. The branch meanwhile gains a commit identical to A. A plain rebase skips A
// as already applied, replays B, and would put the line back on the branch though no reviewer saw
// that change. The land refuses, and records a rebase so the fork's loop has it reviewed against the
// branch.
func TestLandRefusesAChangeTheRebaseWouldAlter(t *testing.T) {
	history := func(t *testing.T, ws, id string) {
		for _, step := range []struct{ content, message string }{
			{"", "remove the line"},
			{"hi\n", "add the line back"},
		} {
			if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte(step.content), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, ws, "commit", "-qam", step.message)
		}
		if err := os.WriteFile(filepath.Join(ws, "g.txt"), []byte("g\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "g.txt")
		git(t, ws, "commit", "-qm", "add g\n\nCoop-Task: "+id)
	}
	repo, _, _, identity, c := prepareForkTaskCandidateHistory(t, "task-cherry", false, history, nil)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-qam", "main removes the line as well") // the same patch as A, another commit
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "would land changed") || !strings.Contains(err.Error(), "coop fork "+identity.Name+" --loop") {
		t.Fatalf("merge whose rebase alters the reviewed change = (%v, %v)", landed, err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "README.md")); len(data) != 0 || forkspace.GitOut(repo, "rev-parse", "HEAD") != parent {
		t.Fatalf("the parent changed: README %q", data)
	}
	if rebase, ok, err := tasks.ReadForkCandidateRebase(repo, identity); err != nil || !ok || rebase.Onto != parent {
		t.Fatalf("rebase request = %+v, %v, %v; want one onto %s", rebase, ok, err, parent)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("land journal after the refusal = %v, %v", pending, err)
	}
}

// An honest land onto a moved branch passes the check: the branch's own commit touches another file.
func TestLandAcceptsTheReviewedChangeOnAMovedBranch(t *testing.T) {
	repo, _, _, identity, c := prepareForkTaskCandidate(t, "task-moved")
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "other.txt")
	git(t, repo, "commit", "-qm", "the branch moves on")
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); !landed || err != nil {
		t.Fatalf("honest land onto a moved branch = (%v, %v)", landed, err)
	}
	for _, file := range []string{"feature.txt", "other.txt"} {
		if _, err := os.Stat(filepath.Join(repo, file)); errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s did not land", file)
		}
	}
}
