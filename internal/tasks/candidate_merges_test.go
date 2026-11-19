package tasks

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// MergeCommitsOutside reads parents from raw objects: a shallow file a box writes, which makes Git
// itself see the merge as a root commit, never makes it report no merge (it reports the merge or
// fails, and a failure refuses), and merges below the boundary are not the fork's.
func TestMergeCommitsOutsideSeesThroughAShallowFile(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	ws := t.TempDir()
	git(t, ws, "init", "-q", "-b", "main")
	commit := func(file string) {
		t.Helper()
		writeTaskFile(t, filepath.Join(ws, file), file+"\n")
		git(t, ws, "add", file)
		git(t, ws, "commit", "-qm", file)
	}
	commit("base.txt")
	git(t, ws, "checkout", "-q", "-b", "old")
	commit("old.txt")
	git(t, ws, "checkout", "-q", "main")
	git(t, ws, "merge", "-q", "--no-ff", "-m", "an old merge below the boundary", "old")
	boundary := forkspace.GitOut(ws, "rev-parse", "HEAD")
	commit("fork.txt")
	git(t, ws, "checkout", "-q", "-b", "side")
	commit("side.txt")
	git(t, ws, "checkout", "-q", "main")
	git(t, ws, "merge", "-q", "--no-ff", "-m", "the fork's merge", "side")
	merge := forkspace.GitOut(ws, "rev-parse", "HEAD")
	commit("after.txt")
	head := forkspace.GitOut(ws, "rev-parse", "HEAD")
	gitDir := forkspace.GitOut(ws, "rev-parse", "--absolute-git-dir")
	if merges, err := MergeCommitsOutside(ws, boundary, head); err != nil || !slices.Equal(merges, []string{merge}) {
		t.Fatalf("merges = %v, %v; want only the fork's %s", merges, err, merge)
	}
	writeTaskFile(t, filepath.Join(gitDir, "shallow"), merge+"\n")
	if merges, err := MergeCommitsOutside(ws, boundary, head); err == nil && !slices.Equal(merges, []string{merge}) {
		t.Fatalf("with a shallow file: merges = %v; want the fork's %s or a refusal", merges, merge)
	}
	if err := os.Remove(filepath.Join(gitDir, "shallow")); err != nil {
		t.Fatal(err)
	}
	if merges, err := MergeCommitsOutside(ws, merge, head); err != nil || len(merges) != 0 {
		t.Fatalf("merges above the fork's merge = %v, %v; want none", merges, err)
	}
}
