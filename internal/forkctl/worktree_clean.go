package forkctl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ForkWorktreeClean proves that the workspace and every populated submodule are
// clean through separate trusted Git views. A failed inspection is not clean.
// Removal and recreation use the same check before confirmation and under the
// lifecycle lock; their explicit --force policy may override a dirty result.
func ForkWorktreeClean(ws string) error {
	remaining := 1024
	return forkWorktreeClean(ws, 0, &remaining)
}

// Status trusts index flags and submodule ignore configuration. Inspect every
// populated gitlink ourselves; active=false must not hide a child from deletion.
func forkWorktreeClean(ws string, depth int, remaining *int) error {
	if depth > 32 || *remaining <= 0 {
		return errors.New("too many nested submodules; keeping the workspace")
	}
	*remaining--
	top, err := gitRawOutErr(ws, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("inspect fork Git root: %w", err)
	}
	top = strings.TrimSuffix(top, "\n") // remove Git's terminator, not part of the path
	root, err := os.OpenRoot(ws)
	if err != nil {
		return err
	}
	defer root.Close()
	actual, actualErr := os.Stat(top)
	expected, expectedErr := root.Stat(".")
	if err := errors.Join(actualErr, expectedErr); err != nil {
		return err
	}
	if !os.SameFile(actual, expected) {
		return errors.New("fork Git root does not match its workspace")
	}
	entries, err := gitRawOutErr(ws, "ls-files", "--stage", "-v", "-z")
	if err != nil {
		return fmt.Errorf("inspect fork index: %w", err)
	}
	for entry := range strings.SplitSeq(entries, "\x00") {
		if entry == "" {
			continue
		}
		if entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z' {
			return errors.New("fork index hides worktree changes with assume-unchanged or skip-worktree; keeping its workspace")
		}
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 4 || !filepath.IsLocal(path) || filepath.Clean(path) == "." {
			return errors.New("cannot inspect fork index entry")
		}
		if fields[1] != "160000" || fields[3] != "0" {
			continue
		}
		// Gitlink workspaces are directories, not symlink aliases. Check ancestors
		// too, so recursion cannot leave the approved tree or return to its parent.
		present := true
		prefix := ""
		for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
			prefix = filepath.Join(prefix, part)
			info, err := root.Lstat(prefix)
			if errors.Is(err, os.ErrNotExist) {
				present = false
				break
			}
			if err != nil {
				return fmt.Errorf("inspect submodule %q: %w", path, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("submodule %q has a non-directory or symlinked workspace", path)
			}
		}
		if !present {
			continue // uninitialized submodule; there is no child worktree to erase
		}
		child, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		info, statErr := child.Stat()
		if statErr != nil || !info.IsDir() {
			child.Close()
			return errors.Join(fmt.Errorf("submodule %q changed while opening its workspace", path), statErr)
		}
		contents, readErr := child.ReadDir(1)
		if errors.Is(readErr, io.EOF) {
			readErr = nil
		}
		closeErr := child.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(contents) == 0 {
			continue // an empty uninitialized submodule is safe too
		}
		if _, err := root.Lstat(filepath.Join(path, ".git")); err != nil {
			return fmt.Errorf("cannot inspect populated submodule %q: %w", path, err)
		}
		if err := forkWorktreeClean(filepath.Join(ws, path), depth+1, remaining); err != nil {
			return fmt.Errorf("submodule %q: %w", path, err)
		}
	}
	// Each populated child was checked through its own trusted view above. Let Git
	// compare gitlink HEADs, but never run child status using box-written config.
	status, err := gitOutErr(ws, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=dirty")
	if err != nil {
		return fmt.Errorf("inspect fork worktree: %w", err)
	}
	if status != "" {
		return errors.New("fork has uncommitted changes; keeping its workspace")
	}
	return nil
}
