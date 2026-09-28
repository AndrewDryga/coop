package eval

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// A suite may be edited by another process while it is frozen. Keep each named tree anchored to
// opened directories: a path check followed by ordinary path-based copying can cross a swapped
// symlink even when before/after content hashes agree.
type openedSuiteTree struct {
	root  *os.Root
	rel   string
	chain []os.FileInfo
}

func openSuiteTree(base *os.Root, rel string) (_ *openedSuiteTree, err error) {
	current, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = current.Close()
		}
	}()
	chain := make([]os.FileInfo, 0, 4)
	for _, name := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if name == "." || name == ".." || name == "" {
			return nil, fmt.Errorf("invalid suite tree path %q", rel)
		}
		child, openErr := openStableSuiteDir(current, name)
		if openErr != nil {
			return nil, fmt.Errorf("suite tree %q: %w", rel, openErr)
		}
		info, statErr := child.Stat(".")
		_ = current.Close()
		if statErr != nil {
			_ = child.Close()
			return nil, statErr
		}
		chain = append(chain, info)
		current = child
	}
	return &openedSuiteTree{root: current, rel: rel, chain: chain}, nil
}

func openStableSuiteDir(parent *os.Root, name string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is not a real directory", name)
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := child.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = child.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("directory %q changed while opening", name)
	}
	return child, nil
}

func suiteTreesOverlap(a, b *openedSuiteTree) bool {
	if a == nil || b == nil {
		return false
	}
	for _, parent := range a.chain {
		if os.SameFile(parent, b.chain[len(b.chain)-1]) {
			return true
		}
	}
	for _, parent := range b.chain {
		if os.SameFile(parent, a.chain[len(a.chain)-1]) {
			return true
		}
	}
	return false
}

func (t *openedSuiteTree) stillNamed(base *os.Root) error {
	current, err := openSuiteTree(base, t.rel)
	if err != nil {
		return err
	}
	defer current.root.Close()
	if !os.SameFile(current.chain[len(current.chain)-1], t.chain[len(t.chain)-1]) {
		return fmt.Errorf("suite tree %q changed while freezing", t.rel)
	}
	return nil
}

const stageMaxEntries = 200_000

// walkOpenedSuiteTree never resolves a source pathname outside an os.Root. A selected tree must
// be an export, but nested submodule metadata is omitted just as workspace preparation omits it.
// Special files are rejected rather than silently omitted from the frozen workload identity.
func walkOpenedSuiteTree(root *os.Root, visit func(rel string, parent *os.Root, name string, info os.FileInfo) error) error {
	seen := 0
	var walk func(*os.Root, string) error
	walk = func(parent *os.Root, prefix string) error {
		dir, err := parent.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(stageMaxEntries + 1)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			seen++
			if seen > stageMaxEntries {
				return fmt.Errorf("suite tree exceeds the %d-entry staging limit", stageMaxEntries)
			}
			name := entry.Name()
			if strings.EqualFold(name, ".git") {
				if prefix == "" {
					return fmt.Errorf("suite tree contains .git; export a clean tree before freezing")
				}
				continue
			}
			info, err := parent.Lstat(name)
			if err != nil {
				return err
			}
			rel := name
			if prefix != "" {
				rel = filepath.Join(prefix, name)
			}
			if err := visit(rel, parent, name, info); err != nil {
				return err
			}
			if !info.IsDir() {
				continue
			}
			child, err := openStableSuiteDir(parent, name)
			if err != nil {
				return err
			}
			err = errors.Join(walk(child, rel), child.Close())
			if err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, "")
}

func openedTreeDigest(root *os.Root) (Fingerprint, error) {
	w := newHasher()
	var read int64
	err := walkOpenedSuiteTree(root, func(rel string, parent *os.Root, name string, info os.FileInfo) error {
		// The copy preserves type and permissions, not sticky/setid bits.
		stagedMode := info.Mode().Type() | info.Mode().Perm()
		w.text("path", filepath.ToSlash(rel)).text("mode", stagedMode.String())
		switch {
		case info.Mode().IsRegular():
			if info.Size() > SnapshotLimit-read {
				return fmt.Errorf("suite tree exceeds the %d GiB signature limit at %q", SnapshotLimit>>30, rel)
			}
			file, err := openStableSuiteFile(parent, name, info)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, copyErr := io.CopyN(h, file, info.Size())
			var extra [1]byte
			n, extraErr := file.Read(extra[:])
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			if n != 0 || !errors.Is(extraErr, io.EOF) {
				return fmt.Errorf("suite file %q changed while hashing", rel)
			}
			read += info.Size()
			w.bytes("content", h.Sum(nil))
		case info.Mode()&os.ModeSymlink != 0:
			target, err := parent.Readlink(name)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) {
				return fmt.Errorf("suite symlink %q has an absolute target", rel)
			}
			if _, err := root.Stat(rel); err != nil {
				return fmt.Errorf("suite symlink %q does not resolve inside its tree: %w", rel, err)
			}
			w.text("link", target)
		case !info.IsDir():
			return fmt.Errorf("suite entry %q is not a file, directory or confined symlink", rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return w.sum(), nil
}

func openStableSuiteFile(parent *os.Root, name string, before os.FileInfo) (*os.File, error) {
	file, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("suite file %q changed while opening", name)
	}
	return file, nil
}

func copyOpenedSuiteTree(root *os.Root, dest string, remainingBytes *int64, remainingEntries *int) error {
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	var dirs []struct {
		path string
		mode os.FileMode
	}
	err := walkOpenedSuiteTree(root, func(rel string, parent *os.Root, name string, info os.FileInfo) error {
		if *remainingEntries == 0 {
			return fmt.Errorf("suite exceeds the %d-entry staging limit", stageMaxEntries)
		}
		*remainingEntries -= 1
		target := filepath.Join(dest, rel)
		switch {
		case info.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			dirs = append(dirs, struct {
				path string
				mode os.FileMode
			}{target, info.Mode().Perm()})
			return nil
		case info.Mode().IsRegular():
			if info.Size() > *remainingBytes {
				return fmt.Errorf("suite exceeds the %d GiB staging limit at %q", SnapshotLimit>>30, rel)
			}
			file, err := openStableSuiteFile(parent, name, info)
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				_ = file.Close()
				return err
			}
			_, copyErr := io.CopyN(out, file, info.Size())
			var extra [1]byte
			n, extraErr := file.Read(extra[:])
			closeErr := errors.Join(out.Close(), file.Close())
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			if n != 0 || !errors.Is(extraErr, io.EOF) {
				return fmt.Errorf("suite file %q changed while copying", rel)
			}
			*remainingBytes -= info.Size()
			return os.Chmod(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := parent.Readlink(name)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) {
				return fmt.Errorf("suite symlink %q has an absolute target", rel)
			}
			if _, err := root.Stat(rel); err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return fmt.Errorf("suite entry %q is not a file, directory or confined symlink", rel)
		}
	})
	if err != nil {
		return err
	}
	// Create directories writable, then restore source modes from leaves upward.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i].path, dirs[i].mode); err != nil {
			return err
		}
	}
	return nil
}
