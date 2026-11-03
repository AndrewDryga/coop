package sessionsvc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/safefile"
)

// CheckPrimarySnapshots inventories only the exact private fork namespaces a
// controller source can own. Historical independent forks have no receipt; they
// need no conversion, and unknown or damaged receipts are reported, never fixed.
func CheckPrimarySnapshots(stateRoot string) (CompanionSnapshotCheck, error) {
	result := CompanionSnapshotCheck{Version: primarySnapshotVersion}
	canonical, err := filepath.EvalSymlinks(stateRoot)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	parent := filepath.Join(canonical, "job-sources")
	sources, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	for _, source := range sources {
		if !source.IsDir() || !validSessionDigest(source.Name()) {
			continue
		}
		repo := filepath.Join(parent, source.Name(), "repository")
		home := forkspace.Home(repo)
		if err := ensureNoSymlinkPath(home); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			result.Damaged = append(result.Damaged, source.Name()+": unsafe fork directory")
			continue
		}
		forks, err := os.ReadDir(home)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result, err
		}
		var workspaces []string
		for _, fork := range forks {
			if fork.IsDir() && forkspace.ValidExistingName(fork.Name()) {
				workspaces = append(workspaces, filepath.Join(home, fork.Name()))
			}
		}
		staged, err := forkspace.StagedDiscards(repo, "")
		if err != nil {
			return result, err
		}
		workspaces = append(workspaces, staged...)
		for _, workspace := range workspaces {
			marker := filepath.Join(workspace, ".git", primarySnapshotMarker)
			if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
				continue
			}
			relative, _ := filepath.Rel(home, workspace)
			name := source.Name() + "/" + relative
			root, err := safefile.OpenRoot(filepath.Join(workspace, ".git"))
			if err != nil {
				result.Damaged = append(result.Damaged, name+": unreadable primary receipt")
				continue
			}
			data, readErr := safefile.ReadRegular(root, primarySnapshotMarker, 4096)
			_ = root.Close()
			var receipt primarySnapshotProjection
			if readErr != nil || json.Unmarshal(data, &receipt) != nil || receipt.Version < 1 {
				result.Damaged = append(result.Damaged, name+": unreadable primary receipt")
				continue
			}
			switch {
			case receipt.Version > primarySnapshotVersion:
				result.Newer++
			case receipt.Version < primarySnapshotVersion:
				result.Older++
			default:
				if _, present, err := forkspace.ReadImmutablePrimarySnapshot(workspace); err != nil || !present {
					result.Damaged = append(result.Damaged, name+": primary ownership changed")
				} else {
					result.Current++
				}
			}
		}
	}
	return result, nil
}
