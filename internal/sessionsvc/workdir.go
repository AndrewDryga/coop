package sessionsvc

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/box"
)

const stableWorkdirMarker = ".stable-workdir"

// SessionWorkdir keeps native history from pre-stable-workdir sessions at its original cwd.
// The marker belongs to the host-private per-session state, never the fork or the box.
func SessionWorkdir(privateRoot, hostWorkspace string) (string, error) {
	marker := filepath.Join(privateRoot, stableWorkdirMarker)
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return hostWorkspace, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.Join(errors.New("session workdir marker is unsafe"), err)
	}
	return box.BareWorkdir, nil
}

func prepareSessionWorkdir(privateRoot, hostWorkspace, nativeID string) (string, error) {
	if nativeID == "" {
		if err := ensurePrivateDirectory(privateRoot); err != nil {
			return "", err
		}
		marker := filepath.Join(privateRoot, stableWorkdirMarker)
		file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if file != nil {
			if err := file.Close(); err != nil {
				return "", err
			}
		}
	}
	workdir, err := SessionWorkdir(privateRoot, hostWorkspace)
	if err != nil || nativeID != "" {
		return workdir, err
	}
	// The native ID is committed after session/new. Make its path choice durable first,
	// so a host reboot cannot keep the ID but forget where its history lives.
	file, err := os.Open(filepath.Join(privateRoot, stableWorkdirMarker))
	if err != nil {
		return "", err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return "", err
	}
	dir, err := os.Open(privateRoot)
	if err != nil {
		return "", err
	}
	syncErr = dir.Sync()
	closeErr = dir.Close()
	return workdir, errors.Join(syncErr, closeErr)
}
