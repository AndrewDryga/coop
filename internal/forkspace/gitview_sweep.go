package forkspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// viewName is the name openGitView gives a view: the first eight bytes of its git dir's hash.
var viewName = regexp.MustCompile(`^[0-9a-f]{16}$`)

// SweepGitViews removes the persistent git views whose repository is gone and reports how many it
// removed. Every eval, session and temporary repository used to leave its view behind for good, and
// a view is rebuilt on its next use, so dropping one costs nothing. A test binary sweeps only a root
// it was pointed at, never the real one.
func SweepGitViews() (int, error) {
	path := os.Getenv(GitViewRootEnv)
	if path == "" {
		if strings.HasSuffix(os.Args[0], ".test") {
			return 0, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, err
		}
		path = filepath.Join(home, ".local", "state", "coop", "gitviews")
	}
	return sweepGitViews(path)
}

// sweepGitViews removes only a view whose objects link names a store that no longer exists. A view
// still being laid out has no link yet, a store that cannot be checked is kept, a view holding an
// unfinished rebase or merge is kept (only it holds that state, and its repository may just be on
// a disk that is not mounted), and nothing under the root is followed: an entry that is not a
// plain directory is left alone.
func sweepGitViews(path string) (int, error) {
	root, err := os.OpenRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, name := range names {
		if !viewName.MatchString(name) {
			continue
		}
		if info, err := root.Lstat(name); err != nil || !info.IsDir() {
			continue
		}
		target, err := root.Readlink(filepath.Join(name, "objects"))
		if err != nil || !filepath.IsAbs(target) {
			continue
		}
		if _, err := os.Stat(target); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if holdsOperation(root, name) {
			continue
		}
		if err := root.RemoveAll(name); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// holdsOperation is viewHoldsOperation inside the sweep's root, so a view swapped for a link after
// its check cannot steer the read elsewhere.
func holdsOperation(root *os.Root, view string) bool {
	for _, name := range viewOperationState {
		if _, err := root.Lstat(filepath.Join(view, name)); err == nil {
			return true
		}
	}
	return false
}
