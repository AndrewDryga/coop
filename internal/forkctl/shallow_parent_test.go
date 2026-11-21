package forkctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// shallowParentRepo is a project that is a `git clone --depth 1` of a three-commit repository: it
// lacks the first two commits, as a real shallow clone does.
func shallowParentRepo(t *testing.T) string {
	t.Helper()
	src := initRepo(t)
	for _, name := range []string{"two.txt", "three.txt"} {
		writeTaskFile(t, filepath.Join(src, name), name+"\n")
		git(t, src, "add", name)
		git(t, src, "commit", "-qm", "add "+name)
	}
	clone, _ := gitrepo.ShallowClone(t, src, 1)
	repo, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	if out, err := observed(repo, "rev-parse", "--is-shallow-repository"); err != nil || out != "true" {
		t.Fatalf("fixture is not shallow: %q, %v", out, err)
	}
	return repo
}

func commitFile(t *testing.T, dir, path, text, message string) string {
	t.Helper()
	writeTaskFile(t, filepath.Join(dir, path), text)
	git(t, dir, "add", "--", path)
	git(t, dir, "commit", "-qm", message)
	return forkspace.GitOut(dir, "rev-parse", "HEAD")
}

// An isolated fork of a shallow clone is still refused, as before, but with the reason and a way
// on instead of "resolve raw audit commit <sha>" (results/forks.md, F1 and F2). With full history
// the same fork merges, and the same duplicate gets the reviewed-change refusal.
func TestIsolatedForkOfAShallowCloneIsRefusedByItsReason(t *testing.T) {
	work := func(t *testing.T, ws, id string) {
		commitFile(t, ws, "feature.txt", "feature\n", "add feature\n\nCoop-Task: "+id)
	}
	for _, parentHas := range []bool{false, true} {
		for _, shallow := range []bool{true, false} {
			repo := initRepo(t)
			if shallow {
				repo = shallowParentRepo(t)
			}
			repo, _, _, identity, c := prepareForkTaskCandidateIn(t, repo, "iso", true, work, nil)
			if parentHas {
				commitFile(t, repo, "feature.txt", "feature\n", "the same change, made by hand")
			} else {
				commitFile(t, repo, "parent.txt", "parent\n", "parent moves on")
			}
			before := forkspace.GitOut(repo, "rev-parse", "HEAD")
			landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			switch {
			case shallow:
				if landed || !strings.Contains(msg, "Coop cannot read fork iso's whole history") || !strings.Contains(msg, "git fetch --unshallow") ||
					!strings.Contains(msg, "coop fork rm iso --force") || strings.Contains(msg, "resolve raw audit") || strings.Contains(msg, "could not check") {
					t.Errorf("merge from a shallow parent (parent has the commit: %v) = %v, %v; want the shallow refusal", parentHas, landed, err)
				}
				if forkspace.GitOut(repo, "rev-parse", "HEAD") != before {
					t.Error("the shallow refusal moved the parent")
				}
			case parentHas:
				if landed || !strings.Contains(msg, "reviewed change would land changed") {
					t.Errorf("full-history control with the commit on main = %v, %v", landed, err)
				}
			default:
				if !landed || err != nil {
					t.Errorf("full-history control = %v, %v; want it landed", landed, err)
				}
			}
		}
	}
}

// An ordinary fork of a shallow clone is shallow too. Its merge says so truthfully and names a way
// on that keeps the fork's history whole; it never says to delete the shallow file or a line of it,
// which leaves the fork without the commits that file stands in for (results/forks.md, F3).
func TestOrdinaryForkOfAShallowCloneIsRefusedTruthfully(t *testing.T) {
	repo := shallowParentRepo(t)
	ws, err := forkspace.Setup(repo, "s")
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, ws, "feature.txt", "feature\n", "add feature")
	before := forkspace.GitOut(repo, "rev-parse", "HEAD")
	landed, err := mergeOneForTest(t, &Control{}, repo, "", "s", false)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	for _, want := range []string{"made from a shallow clone", "git fetch --unshallow",
		shellLine([]string{"git", "-C", ws, "fetch", "--unshallow"}), "start a new fork from a full clone"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q", want)
		}
	}
	for _, never := range []string{"rm /", "Remove", "remove the", "never shallow", "written in the fork"} {
		if strings.Contains(msg, never) {
			t.Errorf("refusal says %q", never)
		}
	}
	if landed || err == nil || forkspace.GitOut(repo, "rev-parse", "HEAD") != before {
		t.Fatalf("merge of an ordinary fork of a shallow clone = %v, %v\n%s", landed, err, msg)
	}

	// Without an origin no fetch into the fork can work, so none is offered.
	git(t, ws, "remote", "rename", "origin", "elsewhere")
	if err := RefuseShallowFork(ws, "s"); err == nil || strings.Contains(err.Error(), "git -C") || !strings.Contains(err.Error(), "start a new fork from it") {
		t.Fatalf("refusal of a shallow fork with no origin = %v", err)
	}
	git(t, ws, "remote", "rename", "elsewhere", "origin")

	// The way it names works: with the full history fetched into the parent and then into the
	// fork, the fork is whole and the shallow refusal is gone.
	git(t, repo, "fetch", "-q", "--unshallow")
	git(t, ws, "fetch", "-q", "--unshallow")
	if err := RefuseShallowFork(ws, "s"); err != nil {
		t.Fatalf("after the named way on, the fork is still refused: %v", err)
	}
}

// A person who deleted the shallow file of a fork of a shallow clone left it without the commits
// the file stood in for. Its merge never reports success with commits left behind: it lands
// exactly the fork's work, or refuses.
func TestOrdinaryForkWithoutItsShallowFileNeverMergesEmpty(t *testing.T) {
	work := func(t *testing.T, ws, id string) {
		commitFile(t, ws, "feature.txt", "feature\n", "add feature\n\nCoop-Task: "+id)
	}
	repo, ws, _, identity, c := prepareForkTaskCandidateIn(t, shallowParentRepo(t), "s", false, work, nil)
	if err := os.Remove(filepath.Join(ws, ".git", "shallow")); err != nil {
		t.Fatal(err)
	}
	before := forkspace.GitOut(repo, "rev-parse", "HEAD")
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed {
		if !pathExists(filepath.Join(repo, "feature.txt")) {
			t.Fatal("merge reported success without the fork's work")
		}
		return
	}
	if err == nil || forkspace.GitOut(repo, "rev-parse", "HEAD") != before {
		t.Fatalf("merge = %v, %v", landed, err)
	}
}
