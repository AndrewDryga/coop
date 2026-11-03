package forkspace

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/AndrewDryga/coop/internal/safefile"
)

const ImmutablePrimaryVersion = 1
const ImmutablePrimaryMarker = "coop-primary-snapshot.json"

type ImmutablePrimarySnapshot struct {
	Version   int    `json:"version"`
	Key       string `json:"key"`
	Directory struct {
		Device uint64 `json:"device"`
		Inode  uint64 `json:"inode"`
	} `json:"directory"`
}

// RefuseImmutablePrimaryMutation is a denial, never an ownership grant. Even an
// unknown marker blocks native adoption and writable mounts during the gap
// between snapshot publication and its durable remote-session reservation.
func RefuseImmutablePrimaryMutation(workspace string) error {
	path, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return resolveErr
	}
	// Subdirectory/file binds and ancestor aliases must not bypass the marker.
	for {
		_, err := os.Lstat(filepath.Join(path, ".git", ImmutablePrimaryMarker))
		if err == nil {
			return errors.New("immutable read-only primary belongs to a controller session")
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

func RecordImmutablePrimarySnapshot(workspace, key string) error {
	info, err := os.Lstat(workspace)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return errors.New("immutable primary directory identity unavailable")
	}
	receipt := ImmutablePrimarySnapshot{Version: ImmutablePrimaryVersion, Key: key}
	receipt.Directory.Device, receipt.Directory.Inode = uint64(stat.Dev), stat.Ino
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	metadata := filepath.Join(workspace, ".git")
	file, err := os.OpenFile(filepath.Join(metadata, ImmutablePrimaryMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	// Publish the denial marker durably before a primary's public path exists.
	for _, path := range []string{metadata, workspace} {
		dir, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return nil
}

// ReadImmutablePrimarySnapshot verifies local custody without a source grant or
// network refresh. Existing independent forks have no marker; an unsupported or
// damaged marked format is preserved even when ordinary Git cleanliness passes.
func ReadImmutablePrimarySnapshot(workspace string) (ImmutablePrimarySnapshot, bool, error) {
	var receipt ImmutablePrimarySnapshot
	marker := filepath.Join(workspace, ".git", ImmutablePrimaryMarker)
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return receipt, false, nil
	}
	if err != nil {
		return receipt, true, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return receipt, true, errors.New("immutable primary receipt is not private")
	}
	root, err := safefile.OpenRoot(filepath.Join(workspace, ".git"))
	if err != nil {
		return receipt, true, err
	}
	defer root.Close()
	data, err := safefile.ReadRegular(root, ImmutablePrimaryMarker, 4096)
	if err != nil {
		return receipt, true, err
	}
	if json.Unmarshal(data, &receipt) != nil {
		return receipt, true, errors.New("immutable primary receipt is invalid")
	}
	key, keyErr := hex.DecodeString(receipt.Key)
	if receipt.Version != ImmutablePrimaryVersion || keyErr != nil || len(key) != 32 || hex.EncodeToString(key) != receipt.Key {
		return receipt, true, fmt.Errorf("unsupported or invalid immutable primary receipt version %d", receipt.Version)
	}
	info, err = os.Lstat(workspace)
	if err != nil {
		return receipt, true, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	// Like fork generation and session ownership, the inode survives a volume
	// remount whose device number can change; source scope and path remain bound.
	if !ok || !info.IsDir() || stat.Ino != receipt.Directory.Inode {
		return receipt, true, errors.New("immutable primary directory changed")
	}
	return receipt, true, nil
}
