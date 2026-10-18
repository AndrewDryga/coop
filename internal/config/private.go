package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// EnsurePrivateDir creates or tightens one host-owned directory without following a final link.
// It deliberately does not walk descendants: a private ancestor protects provider-owned state
// without changing the provider's own file modes.
func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create private directory %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private directory %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private path %s must be a real directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("make directory %s owner-only: %w", path, err)
	}
	return nil
}

// ensurePrivateDirIfPresent tightens an existing host-owned directory (refusing a link or a
// non-directory) and treats a missing one as nothing to protect yet. Load uses it because every
// command loads configuration, and `coop version` on a read-only or absent HOME must still answer;
// the paths that write into the root create it owner-only themselves.
func ensurePrivateDirIfPresent(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return EnsurePrivateDir(path)
}

func ensurePrivateFileIfPresent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect private file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("private path %s must be a regular file", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("make file %s owner-only: %w", path, err)
	}
	return nil
}

// ReadRecord decodes the JSON record at path into v and reports whether it exists. It is the one
// reader for coop's own host-only records (the task-lease registry, credential custody): no box
// mounts their folders and only coop writes them, so a read is a plain read and decode, and the
// caller checks the version and meaning. A file a box can reach goes through safefile's bounded,
// no-follow readers instead.
func ReadRecord(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	return true, nil
}

// ErrLockHeld is TryLock's answer when another process holds the lock.
var ErrLockHeld = errors.New("the lock is held by another process")

// TryLock takes the host-only lock ConfigDir/.locks/<kind>-<hash of key>.lock without waiting and
// returns its release. When another process holds it, the error is ErrLockHeld and holder is that
// process's "pid N" note, so the caller can name it. Nothing unlinks these files, so the flock on
// the one inode the name answers is the whole exclusion.
func (c *Config) TryLock(kind, key string) (release func(), holder string, err error) {
	// Production always carries a ConfigDir; an in-process test app may not, and joining "" would
	// drop a .locks/ directory into whatever the working directory happens to be.
	base := c.ConfigDir
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, ".locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256([]byte(key))
	path := filepath.Join(dir, fmt.Sprintf("%s-%x.lock", kind, sum[:12]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, "", err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// The flock is the authority; the body only names the holder, so nobody has to walk a
		// process tree to find a stuck one.
		note, _ := os.ReadFile(path)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, strings.TrimSpace(string(note)), ErrLockHeld
		}
		return nil, "", err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("pid %d\n", os.Getpid())), 0)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, "", nil
}
