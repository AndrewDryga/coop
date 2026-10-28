package tasks

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// readForkRegistryFile keeps migrated and current registry reads on the same
// bounded, single-link policy. Typed decoding and record custody stay with callers.
func readForkRegistryFile(root *os.Root, name, kind string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("%s is not a bounded single-link regular file", kind)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s changed while opening", kind)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s exceeds its size limit", kind)
	}
	return data, nil
}
