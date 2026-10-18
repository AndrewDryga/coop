package forkspace

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

// recordFolder is where a record is read: an os.Root, or the filesystem by full path.
type recordFolder interface {
	Lstat(name string) (fs.FileInfo, error)
	OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error)
}

type byPath struct{}

func (byPath) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }
func (byPath) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

// ReadRecord reads path, a record a box can write: a regular file with one name and at most limit
// bytes, checked without following a link and read from that same inode, opened without waiting
// on a FIFO. A refusal names what and the kind of thing found; a missing record matches
// os.ErrNotExist.
func ReadRecord(path, what string, limit int64) ([]byte, error) {
	return readRecord(byPath{}, path, what, limit, true)
}

// ReadRecordIn is ReadRecord for name inside root.
func ReadRecordIn(root *os.Root, name, what string, limit int64) ([]byte, error) {
	return readRecord(root, name, what, limit, true)
}

// readRecord is ReadRecord in folder; singleLink false also accepts a file with other names, as a
// git pointer may have.
func readRecord(folder recordFolder, name, what string, limit int64, singleLink bool) ([]byte, error) {
	before, err := folder.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := boundedRecord(before, what, limit, singleLink); err != nil {
		return nil, err
	}
	f, err := folder.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, fmt.Errorf("%s changed while Coop was opening it", what)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", what, limit)
	}
	return data, nil
}

// boundedRecord refuses anything but a regular file of at most limit bytes (with one name when
// singleLink), naming what was found instead.
func boundedRecord(info fs.FileInfo, what string, limit int64, singleLink bool) error {
	mode := info.Mode()
	switch {
	case mode&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a link, not a file Coop wrote", what)
	case mode.IsDir():
		return fmt.Errorf("%s is a folder, not a file Coop wrote", what)
	case mode&fs.ModeNamedPipe != 0:
		return fmt.Errorf("%s is a named pipe, not a file Coop wrote", what)
	case mode&fs.ModeSocket != 0:
		return fmt.Errorf("%s is a socket, not a file Coop wrote", what)
	case mode&(fs.ModeDevice|fs.ModeCharDevice) != 0:
		return fmt.Errorf("%s is a device, not a file Coop wrote", what)
	case !mode.IsRegular():
		return fmt.Errorf("%s is not a regular file", what)
	case info.Size() < 0 || info.Size() > limit:
		return fmt.Errorf("%s is larger than %d bytes", what, limit)
	}
	if !singleLink {
		return nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New(what + " has no file status to check")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("%s has another name (a hard link), not a file Coop wrote", what)
	}
	return nil
}
