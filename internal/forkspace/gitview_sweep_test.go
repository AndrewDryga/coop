package forkspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSweepGitViewsRemovesOnlyViewsOfGoneRepositories(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(t.TempDir(), "objects") // a repository that still exists
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "deleted-repo", ".git", "objects")
	view := func(name, objects string) string {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if objects != "" {
			if err := os.Symlink(objects, filepath.Join(dir, "objects")); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	live := view("aaaaaaaaaaaaaaaa", store)
	dangling := view("bbbbbbbbbbbbbbbb", gone)
	// Removal unlinks a view's links and never follows one: refs points at a live folder.
	refs := filepath.Join(t.TempDir(), "refs")
	if err := os.MkdirAll(refs, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(refs, "canary")
	if err := os.WriteFile(canary, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(refs, filepath.Join(dangling, "refs")); err != nil {
		t.Fatal(err)
	}
	inProgress := view("cccccccccccccccc", "")
	notAView := view("not-a-view", gone)
	// An unfinished rebase lives only in its view: kept even while its repository is away.
	rebasing := view("eeeeeeeeeeeeeeee", gone)
	if err := os.Mkdir(filepath.Join(rebasing, "rebase-merge"), 0o700); err != nil {
		t.Fatal(err)
	}
	// An entry under a view's name that links elsewhere is not followed, even to a dangling view.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(gone, filepath.Join(outside, "objects")); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "dddddddddddddddd")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	kept := []string{live, inProgress, notAView, rebasing, linked, filepath.Join(outside, "objects"), canary}
	// A store that cannot be checked (here, a folder that refuses to be searched) is kept.
	if os.Geteuid() != 0 {
		locked := filepath.Join(t.TempDir(), "locked")
		if err := os.MkdirAll(filepath.Join(locked, "objects"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		kept = append(kept, view("ffffffffffffffff", filepath.Join(locked, "objects")))
	}

	removed, err := sweepGitViews(root)
	if err != nil || removed != 1 {
		t.Fatalf("sweepGitViews = %d, %v; want only the view of the deleted repository removed", removed, err)
	}
	if _, err := os.Lstat(dangling); !os.IsNotExist(err) {
		t.Fatal("the view of a deleted repository was kept", err)
	}
	for _, path := range kept {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
	if _, err := os.Stat(store); err != nil {
		t.Fatal("the live repository's store was touched", err)
	}
}

// A test binary never sweeps the real root: with no root named, SweepGitViews leaves a dangling view
// under HOME alone, and with one named it sweeps that.
func TestSweepGitViewsUsesOnlyTheRootItIsPointedAtUnderTest(t *testing.T) {
	danglingView := func(root string) string {
		t.Helper()
		dir := filepath.Join(root, "eeeeeeeeeeeeeeee")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "gone", "objects"), filepath.Join(dir, "objects")); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := danglingView(filepath.Join(home, ".local", "state", "coop", "gitviews"))
	t.Setenv(GitViewRootEnv, "")
	if removed, err := SweepGitViews(); removed != 0 || err != nil {
		t.Fatalf("a test binary swept the default view root: %d, %v", removed, err)
	}
	if _, err := os.Lstat(real); err != nil {
		t.Fatal("a test binary removed a view under HOME", err)
	}
	named := t.TempDir()
	danglingView(named)
	t.Setenv(GitViewRootEnv, named)
	if removed, err := SweepGitViews(); removed != 1 || err != nil {
		t.Fatalf("SweepGitViews(%s) = %d, %v", named, removed, err)
	}
}

// A process keeps its views; when another coop's sweep removed one while its repository was away,
// the next command lays it out again instead of failing until a restart.
func TestOpenGitViewLaysOutASweptViewAgain(t *testing.T) {
	t.Setenv(GitViewRootEnv, t.TempDir())
	CloseGitViews()
	t.Cleanup(CloseGitViews)
	repo := initRepo(t)
	view, _, err := openGitView(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(view.dir); err != nil {
		t.Fatal(err)
	}
	again, _, err := openGitView(context.Background(), repo, nil)
	if err != nil {
		t.Fatalf("a swept view did not come back: %v", err)
	}
	if _, err := os.Readlink(filepath.Join(again.dir, "objects")); err != nil {
		t.Fatal("the view came back without its objects link", err)
	}
}
