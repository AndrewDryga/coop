package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// PrivateRunfiles returns Coop's owner-only runfiles folder, creating it when absent. It holds each
// ordinary run's generated files and the client configs Coop hands the docker CLI, which the system
// temp directory cannot keep private: a long-lived box with a broad /tmp bind could watch a later
// run there, and another user could recreate a removed Docker client folder with a config of their
// own. Every launch protects the folder, so a launch whose binds would expose it fails its own
// authority validation, and the daily sweep clears what killed runs leave in it.
//
// The folder itself must be a real directory owned by this user; it is closed to everyone else and
// returned with links resolved. The Coop home above it may be a link, as dotfiles setups make it.
func PrivateRunfiles(cfg *Config) (string, error) {
	root := runfilesPath(cfg)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return validateRunfiles(root)
}

// ExistingRunfiles locates the runfiles folder for housekeeping without creating it: a command that
// never staged a run must not acquire state because the daily sweep was due.
func ExistingRunfiles(cfg *Config) (string, bool, error) {
	root := runfilesPath(cfg)
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	root, err := validateRunfiles(root)
	return root, err == nil, err
}

// runfilesPath is below cfg's BoxHome, which Load sets to RootDir; a test's Config without one uses
// its ConfigDir. Code with no Config, such as the container runtime making a Docker client folder,
// passes nil and gets RootDir's: the folder the daily sweep visits.
func runfilesPath(cfg *Config) string {
	home := RootDir()
	if cfg != nil && cfg.BoxHome != "" {
		home = cfg.BoxHome
	} else if cfg != nil && cfg.ConfigDir != "" {
		home = cfg.ConfigDir
	}
	return filepath.Join(home, "runfiles")
}

func validateRunfiles(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.Join(err, fmt.Errorf("private runfiles folder %s is not a real directory", root))
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("private runfiles folder %s is not owned by the current user", root)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}
