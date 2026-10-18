package forkspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/randid"
)

// OpenStateRoot opens repo's fork records folder (StateDir), or the folder sub inside it, as an
// os.Root without following a link at any step: the folder is opened by descriptor first (made,
// owner-only, when create), and the root must then be that same folder, so every open, rename and
// removal through it stays there even if a name on the way is swapped afterwards. Without create, a
// missing folder is os.ErrNotExist.
func OpenStateRoot(repo, sub string, create bool) (*os.Root, error) {
	dir, err := openStateFolder(repo, sub, create)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	want, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(StateDir(repo), sub)
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	if got, err := root.Stat("."); err != nil || !os.SameFile(want, got) {
		_ = root.Close()
		return nil, errors.Join(err, fmt.Errorf("fork state path %q changed while opening", path))
	}
	return root, nil
}

// openStateFolder opens the fork records folder, or the folder sub inside it (one or more
// components), by descriptor without following a link at any step.
func openStateFolder(repo, sub string, create bool) (*os.File, error) {
	var dir *os.File
	var err error
	if create {
		dir, err = openStateDir(repo)
	} else {
		dir, err = openExistingStateDir(repo)
	}
	if err != nil || sub == "" {
		return dir, err
	}
	if !filepath.IsLocal(sub) {
		_ = dir.Close()
		return nil, fmt.Errorf("fork state folder %q is not inside the fork records folder", sub)
	}
	path := StateDir(repo)
	for _, component := range strings.Split(filepath.Clean(sub), string(filepath.Separator)) {
		next, err := openStateChild(dir, filepath.Join(path, component), component, create)
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		dir, path = next, filepath.Join(path, component)
	}
	return dir, nil
}

// openStateChild opens the folder name in dir without following a link, making it owner-only when
// create, with the parent barrier every folder Coop makes gets.
func openStateChild(dir *os.File, path, name string, create bool) (*os.File, error) {
	if create {
		if err := unix.Mkdirat(int(dir.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, &os.PathError{Op: "mkdir", Path: path, Err: err}
		}
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("fork state path %q is not a real directory", path)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	folder := os.NewFile(uintptr(fd), path)
	if create {
		folderChecked(dir.Name())
		if err := folder.Chmod(0o700); err != nil {
			_ = folder.Close()
			return nil, fmt.Errorf("make fork state path %q: %w", path, err)
		}
	}
	return folder, nil
}

// openExistingStateDir is openStateDir for a reader or a writer that must not make the record
// folder: a missing one is os.ErrNotExist.
func openExistingStateDir(repo string) (*os.File, error) {
	return openForkRecords(repo, false)
}

// CreateTempIn creates a new owner-only file in root named prefix plus a random suffix, never
// following a link, and returns it with its name in root.
func CreateTempIn(root *os.Root, prefix string) (*os.File, string, error) {
	for range 10000 {
		name := tempName(prefix)
		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, name, err
	}
	return nil, "", fmt.Errorf("could not name a temporary file for %s in %s", prefix, root.Name())
}

// MkdirTempIn creates a new owner-only folder in root named prefix plus a random suffix, and
// returns its name in root.
func MkdirTempIn(root *os.Root, prefix string) (string, error) {
	for range 10000 {
		name := tempName(prefix)
		err := root.Mkdir(name, 0o700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return name, err
	}
	return "", fmt.Errorf("could not name a temporary folder for %s in %s", prefix, root.Name())
}

// PublishNoReplace gives the file tmp in root the name name, failing with os.ErrExist when name is
// taken: a hard link, which never replaces, then tmp's own name removed.
func PublishNoReplace(root *os.Root, tmp, name string) error {
	if err := root.Link(tmp, name); err != nil {
		return err
	}
	return root.Remove(tmp)
}

// tempName is prefix plus 8 random hex digits, no longer than os.CreateTemp's suffix, so a name
// that fit with CreateTemp still fits.
func tempName(prefix string) string { return prefix + randid.Hex(4) }
