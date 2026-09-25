package eval

import (
	"fmt"
	"os"
	"syscall"
)

// Both starter extraction and run creation write below this root. Existing installations may
// have a 0755 root from starter extraction, so creation also tightens it before either write.
func ensurePrivateRoot(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create eval state root: %w", err)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect eval state root: %w", err)
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !owned || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("eval state root %q must be a real directory owned by this user", root)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("protect eval state root: %w", err)
	}
	return nil
}
