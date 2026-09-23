// Package safefile provides descriptor-relative reads that never traverse a symbolic link.
//
// os.Root confines paths, but intentionally resolves symlinks itself even when an OpenFile caller
// supplies O_NOFOLLOW. Host control files selected from a writable repository need the stronger
// property: every fixed path component is opened with raw openat(2), and a special-file leaf is
// rejected before any read can block.
package safefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenRoot pins a real directory at path. Callers that deliberately accept a symlink spelling for
// the selected root must resolve that spelling first; descendants are always strict.
func OpenRoot(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("%s is not a real directory", path)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), path)
	after, err := dir.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = dir.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	return dir, nil
}

// OpenDir opens rel below root without following a symlink in any component.
func OpenDir(root *os.File, rel string) (*os.File, error) {
	return openAt(root, rel, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK)
}

// CloneDir opens a fresh descriptor for the same pinned directory. Unlike dup(2), the clone has
// an independent directory-read offset, so bounded scanners cannot consume a caller's descriptor.
func CloneDir(dir *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), ".",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), dir.Name()), nil
}

// OpenRegular opens a regular-file rel below root without following a symlink in any component.
func OpenRegular(root *os.File, rel string) (*os.File, error) {
	f, err := openAt(root, rel, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return f, nil
}

// ReadRegular reads at most limit bytes from a strict regular-file path.
func ReadRegular(root *os.File, rel string, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, errors.New("negative file limit")
	}
	f, err := OpenRegular(root, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", rel, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", rel, limit)
	}
	return data, nil
}

// ReadLink reads one literal child link relative to an already-pinned directory. It never follows
// the link and grows its buffer rather than silently truncating a long target.
func ReadLink(dir *os.File, name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid link name %q", name)
	}
	for size := 256; size <= 64<<10; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(int(dir.Fd()), name, buf)
		if err != nil {
			return "", err
		}
		if n < len(buf) {
			return string(buf[:n]), nil
		}
	}
	return "", errors.New("symbolic link target exceeds 64 KiB")
}

// LstatMode reports one literal child's type and permissions relative to a pinned directory.
// os.DirEntry.Info can fall back to the File.Name path on some platforms; fstatat keeps this
// metadata lookup under the same descriptor authority as the subsequent open/readlink.
func LstatMode(dir *os.File, name string) (os.FileMode, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return 0, fmt.Errorf("invalid child name %q", name)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	mode := os.FileMode(stat.Mode & 0o777)
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFDIR:
		mode |= os.ModeDir
	case unix.S_IFLNK:
		mode |= os.ModeSymlink
	case unix.S_IFIFO:
		mode |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= os.ModeSocket
	case unix.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	case unix.S_IFBLK:
		mode |= os.ModeDevice
	default:
		mode |= os.ModeIrregular
	}
	return mode, nil
}

func openAt(root *os.File, rel string, finalFlags int) (*os.File, error) {
	parts, err := localParts(rel)
	if err != nil {
		return nil, err
	}
	current := int(root.Fd())
	owned := false
	closeCurrent := func() {
		if owned {
			_ = unix.Close(current)
		}
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(current, part,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if openErr != nil {
			closeCurrent()
			return nil, openErr
		}
		closeCurrent()
		current, owned = next, true
	}
	fd, err := unix.Openat(current, parts[len(parts)-1], finalFlags, 0)
	closeCurrent()
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

func localParts(rel string) ([]string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("path %q is not a non-empty local path", rel)
	}
	clean := filepath.Clean(rel)
	if !filepath.IsLocal(clean) || clean == "." {
		return nil, fmt.Errorf("path %q is not a non-empty local path", rel)
	}
	parts := strings.Split(clean, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("path %q is not a clean local path", rel)
		}
	}
	return parts, nil
}
