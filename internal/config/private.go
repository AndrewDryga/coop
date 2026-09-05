package config

import (
	"errors"
	"fmt"
	"os"
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
