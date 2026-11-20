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
// missing folder is os.ErrNotExist. With create, a folder a release removed while it was being
// opened (pruneEmptyStateFolders) is opened again: it is gone, or another writer made a new one at
// its name.
func OpenStateRoot(repo, sub string, create bool) (root *os.Root, err error) {
	if !create {
		return openStateRoot(repo, sub, false)
	}
	for range stateFolderAttempts {
		if root, err = openStateRoot(repo, sub, true); !stateFolderRemoved(err) {
			return root, err
		}
	}
	return nil, stateFolderGone(repo, err)
}

// errStatePathChanged is a folder of the fork records that was another folder by the time it was
// opened by name: removed while empty and made again by another writer, or swapped by someone else.
var errStatePathChanged = errors.New("fork state path changed while opening")

// stateFolderRemoved says err is what a writer gets when a release removed its empty folder under it.
func stateFolderRemoved(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, errStatePathChanged)
}

// stateFolderGone is the plain answer when a folder kept being removed under a writer after every
// attempt: other Coop processes ending their runs remove it once it is empty.
func stateFolderGone(repo string, err error) error {
	if gone := (*stateFolderGoneError)(nil); errors.As(err, &gone) {
		return gone
	}
	return &stateFolderGoneError{dir: StateDir(repo), err: err}
}

type stateFolderGoneError struct {
	dir string
	err error
}

func (e *stateFolderGoneError) Error() string {
	return "Coop's fork records folder " + e.dir + " changed under Coop " + fmt.Sprint(stateFolderAttempts) +
		" times while it wrote there. Other Coop runs that end at the same moment remove it when it is empty. Run the command again."
}

func (e *stateFolderGoneError) Unwrap() error { return e.err }

func openStateRoot(repo, sub string, create bool) (*os.Root, error) {
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
		return nil, errors.Join(err, fmt.Errorf("%w: %q", errStatePathChanged, path))
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
	_, err := publishNoReplace(root, tmp, name)
	return err
}

// publishNoReplace is PublishNoReplace that also reports whether name was linked: once it is, the
// file is published, even when removing tmp then fails.
func publishNoReplace(root *os.Root, tmp, name string) (linked bool, err error) {
	if err := root.Link(tmp, name); err != nil {
		return false, err
	}
	return true, removePublishedTemp(root, tmp)
}

// removePublishedTemp removes a temporary name after its file was linked; tests replace it.
var removePublishedTemp = func(root *os.Root, tmp string) error { return root.Remove(tmp) }

// tempName is prefix plus 8 random hex digits, no longer than os.CreateTemp's suffix, so a name
// that fit with CreateTemp still fits.
func tempName(prefix string) string { return prefix + randid.Hex(4) }

// Darwin can return ENOENT when simultaneous openat(O_CREAT) calls first make
// one file in a stable directory. Claim creation exclusively; an existing file
// takes a plain open with the same NOFOLLOW/access flags. This is not a retry:
// disappearance after EEXIST and every other I/O failure remain visible.
func openFileAt(dirfd int, name string, flags int, mode uint32) (int, error) {
	if flags&unix.O_CREAT == 0 || flags&unix.O_EXCL != 0 {
		return unix.Openat(dirfd, name, flags, mode)
	}
	fd, err := unix.Openat(dirfd, name, flags|unix.O_EXCL, mode)
	if errors.Is(err, unix.EEXIST) {
		return unix.Openat(dirfd, name, flags&^unix.O_CREAT, mode)
	}
	return fd, err
}
