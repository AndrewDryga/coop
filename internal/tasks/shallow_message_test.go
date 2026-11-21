package tasks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// shallowCloneOf is a `git clone --depth 1` of a three-commit repository: it lacks two commits.
func shallowCloneOf(t *testing.T) (string, func(...string)) {
	t.Helper()
	src, srcRun := gitRepo(t)
	srcRun("commit", "-q", "--allow-empty", "-m", "c1")
	srcRun("commit", "-q", "--allow-empty", "-m", "c2")
	srcRun("commit", "-q", "--allow-empty", "-m", "c3")
	return gitrepo.ShallowClone(t, src, 1)
}

// A completion in a shallow clone is still refused, as before, but says why and how to go on
// instead of naming the commit beyond the boundary it could not read (results/forks.md, F1).
func TestCompletionInAShallowCloneNamesTheCause(t *testing.T) {
	clone, run := shallowCloneOf(t)
	base := forkspace.GitOut(clone, "rev-parse", "HEAD")
	run("commit", "-q", "--allow-empty", "-m", "work\n\nCoop-Task: task-a")
	head := forkspace.GitOut(clone, "rev-parse", "HEAD")

	if missing, _ := CompletionUnbindableTasks(clone, base, head, []string{"task-a"}, nil, nil); !slices.Equal(missing, []string{"task-a"}) {
		t.Fatalf("completion in a shallow clone = missing %v; want it still refused", missing)
	}
	problems := UnbindableTaskProblems(clone, base, head, []string{"task-a"}, nil)
	if !slices.Equal(problems, []string{ShallowCheckoutCause}) {
		t.Fatalf("problems = %q; want only the shallow cause", problems)
	}
	if !strings.Contains(ShallowCheckoutCause, "git fetch --unshallow") || strings.Contains(ShallowCheckoutCause, "resolve raw audit") {
		t.Fatalf("cause = %q", ShallowCheckoutCause)
	}
	if !ShallowCheckout(clone) || HistoryReadable(clone, head) {
		t.Fatal("a shallow clone should read as shallow with unreadable history")
	}
	if err := auditShallowRefusal(clone, "task-a"); err == nil || !strings.Contains(err.Error(), "git fetch --unshallow") || strings.Contains(err.Error(), "remove") {
		t.Fatalf("Git's reading of a shallow clone = %v; want it refused without advice to delete the file", err)
	}

	// A repository with full history, or a shallow file that hides nothing gone, reads as before.
	repo, fullRun := gitRepo(t)
	fullRun("commit", "-q", "--allow-empty", "-m", "root")
	root := forkspace.GitOut(repo, "rev-parse", "HEAD")
	if ShallowCheckout(repo) || !HistoryReadable(repo, root) {
		t.Fatal("a full repository should read as whole")
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "shallow"), []byte(root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ShallowCheckout(repo) || !HistoryReadable(repo, root) {
		t.Fatal("a planted shallow file over present history should read as shallow but whole")
	}
}
