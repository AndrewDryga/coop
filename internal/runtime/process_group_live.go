//go:build cooplivetest

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/liveprocess"
)

var removeLiveCredentialTree = os.RemoveAll

func interruptibleProcessEnvironment() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if key != liveprocess.ControlFDEnv && key != liveprocess.ProcessDirEnv &&
			key != liveprocess.CleanupIDEnv && key != liveprocess.RevokePathEnv {
			out = append(out, item)
		}
	}
	return out
}

// A live-test runtime inherits the test helper's already-owned PGID only when an authenticated
// control file is present on an inherited descriptor. The default and an invalid activation keep
// production isolation; an explicitly requested but invalid activation fails before exec.
func interruptibleProcessGroup() (*syscall.SysProcAttr, error) {
	raw := os.Getenv(liveprocess.ControlFDEnv)
	if raw == "" {
		return &syscall.SysProcAttr{Setpgid: true}, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd != 3 || !liveprocess.ValidateControlFD(fd) {
		return nil, errors.New("invalid live process control")
	}
	return nil, nil
}

// beforeInterruptibleCancel revokes the tagged helper's projected credential path before the
// runtime receives its first signal. The authenticated descriptor limits this path to isolated live
// binaries; default builds do not contain this behavior.
func beforeInterruptibleCancel() error {
	fd, err := strconv.Atoi(os.Getenv(liveprocess.ControlFDEnv))
	if err != nil || fd != 3 || !liveprocess.ValidateControlFD(fd) {
		return errors.New("invalid live process control")
	}
	configDir := filepath.Clean(os.Getenv("COOP_CONFIG_DIR"))
	tombstone := filepath.Clean(os.Getenv(liveprocess.RevokePathEnv))
	if !filepath.IsAbs(configDir) || filepath.Base(configDir) != "config" ||
		!liveprocess.ValidRevocationPath(configDir, tombstone) {
		return errors.New("invalid live credential directory")
	}
	parentInfo, err := os.Lstat(filepath.Dir(configDir))
	if err != nil || !privateOwnedRuntimeDir(parentInfo) {
		return errors.New("invalid live credential parent")
	}
	if _, err := os.Lstat(tombstone); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("live credential revocation path already exists")
	}
	if err := os.Rename(configDir, tombstone); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return errors.New("revoke live credential directory")
	}
	if err := removeLiveCredentialTree(tombstone); err != nil {
		return errors.New("remove revoked live credential directory")
	}
	return nil
}

func privateOwnedRuntimeDir(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
