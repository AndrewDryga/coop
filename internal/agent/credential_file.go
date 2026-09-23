package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ReadCredentialArtifact reads one provider-home credential through the opened inode, without
// following a final symlink or blocking on a FIFO/device. Provider homes are writable inside a
// normal agent box, so every later host-side status, refresh, broker or restricted-mode read must
// treat their contents as untrusted input.
func ReadCredentialArtifact(path string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("credential artifact needs a positive byte limit")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > limit {
		return nil, errors.New("credential artifact is not a bounded regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() < 0 || after.Size() > limit {
		return nil, errors.New("credential artifact changed while opening or exceeds its byte limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("credential artifact exceeds its %d-byte limit", limit)
	}
	return data, nil
}

// ReadOptionalCredentialArtifact is the projection caller's form: absence is ordinary, while an
// unsafe entry remains an error rather than being mistaken for a profile with no stored login.
func ReadOptionalCredentialArtifact(path string, limit int64) ([]byte, bool, error) {
	data, err := ReadCredentialArtifact(path, limit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}
