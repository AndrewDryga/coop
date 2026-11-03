package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

const companionSnapshotWarmTime = time.Hour
const retiredSnapshotPrefix = ".snapshot-retired-"

// The filesystem's links are the durable references: publishing a projection
// adds them before its session is committed, and removing a session only drops
// its links. GC never has a crash window between a reference counter and files.
func (s *Service) reclaimCompanionSnapshots(ctx context.Context, result *StorageReclaim) {
	ctx, cancel := context.WithTimeout(ctx, companionReclaimTimeout)
	defer cancel()
	limits := s.storageLimitsSnapshot()
	volume, err := forkspace.MeasureFilesystem(s.stateRoot)
	pressure := err == nil && volume.FreeBytes <= 2*limits.ReserveBytes
	count, problems := pruneCompanionSnapshotsSweep(ctx, s.stateRoot, time.Now(), limits.DisposableBudgetBytes, limits.MaxReclaimPerPass, pressure, &s.storage.snapshotSweep)
	result.SnapshotsPruned += count
	result.Problems = append(result.Problems, problems...)
}

type companionSnapshotSweep struct {
	cursor               int
	bytes, previousBytes int64
	beforeCandidate      func(string) // deterministic filesystem/deadline fixture
}

func pruneCompanionSnapshots(ctx context.Context, stateRoot string, now time.Time, budget int64, max int, pressure bool) (int, []string) {
	return pruneCompanionSnapshotsSweep(ctx, stateRoot, now, budget, max, pressure, &companionSnapshotSweep{})
}

func pruneCompanionSnapshotsSweep(ctx context.Context, stateRoot string, now time.Time, budget int64, max int, pressure bool, sweep *companionSnapshotSweep) (int, []string) {
	canonical, err := filepath.EvalSymlinks(stateRoot)
	if err != nil {
		return 0, []string{err.Error()}
	}
	stateRoot = canonical
	parent := filepath.Join(stateRoot, companionSnapshotRoot)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, []string{err.Error()}
	}
	var problems []string
	if len(entries) == 0 || max <= 0 {
		return 0, nil
	}
	// Never walk payloads before deciding to reclaim. Budget sizes were measured
	// while sealing the immutable cache, and are only a warm-cache eviction hint.
	// Physical telemetry continues to measure live inodes independently.
	start := sweep.cursor % len(entries)
	removed, attempts := 0, 0
	scanLimit := len(entries)
	if max <= len(entries)/16 {
		scanLimit = max * 16
	}
	for offset := 0; offset < scanLimit && attempts < max && ctx.Err() == nil; offset++ {
		index := (start + offset) % len(entries)
		// Advance before any ownership check, lock or recursive work can time out.
		sweep.cursor = index + 1
		if index == 0 {
			sweep.previousBytes = sweep.bytes
			sweep.bytes = 0
		}
		entry := entries[index]
		name := entry.Name()
		if !entry.IsDir() || (!validSessionDigest(name) && !strings.HasPrefix(name, ".snapshot-")) {
			continue
		}
		path := filepath.Join(parent, name)
		receipt, err := readCompanionSnapshotReceipt(path)
		if err != nil {
			// A receipt-less retired directory is removable only when empty.
			if retiredSnapshotName(name) {
				if removeErr := os.Remove(path); removeErr == nil {
					continue
				}
			}
			problems = append(problems, err.Error())
			continue
		}
		if validSessionDigest(name) && name != receipt.Key {
			problems = append(problems, "snapshot name does not match receipt")
			continue
		}
		sweep.bytes += receipt.AllocatedBytes
		info, err := entry.Info()
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if sweep.beforeCandidate != nil {
			sweep.beforeCandidate(path)
		}
		if ctx.Err() != nil {
			break
		}
		_, deleting := os.Lstat(filepath.Join(path, "deleting"))
		unfinished := strings.HasPrefix(name, ".snapshot-")
		overBudget := sweep.bytes > budget || sweep.previousBytes > budget
		if deleting != nil && !pressure && !overBudget && now.Sub(info.ModTime()) < companionSnapshotWarmTime {
			continue
		}
		if unfinished && !retiredSnapshotName(name) && now.Sub(info.ModTime()) < companionSnapshotWarmTime {
			continue
		}
		attempts++
		unlock, err := forkspace.LockStateContext(ctx, stateRoot, "companion-snapshot-"+receipt.Key)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		didRemove, err := pruneCompanionSnapshot(ctx, path, receipt.Key, unfinished)
		unlock()
		if err != nil {
			problems = append(problems, fmt.Sprintf("reclaim companion snapshot: %v", err))
			continue
		}
		if didRemove {
			removed++
			sweep.bytes -= receipt.AllocatedBytes
		}
	}
	return removed, problems
}

func pruneCompanionSnapshot(ctx context.Context, path, key string, unfinished bool) (bool, error) {
	receipt, err := readCompanionSnapshotReceipt(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if receipt.Key != key {
		return false, errors.New("snapshot identity changed during eviction")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return false, err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return false, err
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil || !identity.sameDirectory(receipt.Directory) {
		return false, errors.New("snapshot changed while opening")
	}
	marker, markerErr := root.Lstat("deleting")
	deleting := markerErr == nil
	if deleting && (!marker.Mode().IsRegular() || marker.Size() != 0) {
		return false, errors.New("snapshot eviction marker changed")
	}
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return false, markerErr
	}
	if !deleting {
		if err := removeSnapshotProjectionStages(ctx, path); err != nil {
			return false, err
		}
		held, err := companionSnapshotHeld(ctx, path)
		if err != nil {
			return false, err
		}
		if held {
			return false, nil
		}
		if !unfinished {
			binding := session.CompanionRepository{Name: "cached", Workspace: filepath.Join(path, "tree"), BaseCommit: receipt.Commit}
			if err = verifyCompanionSnapshot(ctx, path, key, binding); err != nil {
				return false, err
			}
		}
		marker, err := root.OpenFile("deleting", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return false, err
		}
		err = errors.Join(marker.Sync(), marker.Close())
		if err != nil {
			return false, err
		}
	}
	if err = syncSnapshotDirectory(path); err != nil {
		return false, err
	}
	// Move out of the reusable key namespace before deleting any payload.
	// The receipt follows the inode, so interruption leaves either a valid
	// cache entry with a durable marker or authenticated retired garbage.
	path, err = retireCompanionSnapshot(path, key)
	if err != nil {
		return false, err
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// Ownership is the receipt's directory identity. Remove within the pinned
	// root so a renamed parent or planted symlink cannot redirect reclamation.
	directory, err := root.Open(".")
	if err != nil {
		return false, err
	}
	entries, err := directory.ReadDir(-1)
	_ = directory.Close()
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() == "receipt.json" || entry.Name() == "deleting" {
			continue
		}
		if err = root.RemoveAll(entry.Name()); err != nil {
			return false, err
		}
	}
	if err = root.Remove("deleting"); err != nil {
		return false, err
	}
	if err = syncSnapshotDirectory(path); err != nil {
		return false, err
	}
	if err = root.Remove("receipt.json"); err != nil {
		return false, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return false, errors.New("snapshot changed before retirement")
	}
	if err = os.Remove(path); err != nil {
		return false, err
	}
	return true, syncSnapshotDirectory(filepath.Dir(path))
}

func companionSnapshotHeld(ctx context.Context, path string) (bool, error) {
	held := false
	err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return errors.New("snapshot link count is unavailable")
			}
			if stat.Nlink > 1 {
				held = true
				return fs.SkipAll
			}
		}
		return nil
	})
	return held, err
}

func retiredSnapshotName(name string) bool {
	if !strings.HasPrefix(name, retiredSnapshotPrefix) {
		return false
	}
	rest := strings.TrimPrefix(name, retiredSnapshotPrefix)
	return len(rest) > 65 && rest[64] == '-' && validSessionDigest(rest[:64])
}

func syncSnapshotDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// Called with the key lock and an authenticated, unreferenced receipt.
func retireCompanionSnapshot(path, key string) (string, error) {
	if retiredSnapshotName(filepath.Base(path)) {
		return path, nil
	}
	retired, err := os.MkdirTemp(filepath.Dir(path), retiredSnapshotPrefix+key+"-")
	if err != nil {
		return path, err
	}
	if err = os.Remove(retired); err != nil {
		return path, err
	}
	if err = os.Rename(path, retired); err != nil {
		_ = os.Remove(retired)
		return path, err
	}
	return retired, syncSnapshotDirectory(filepath.Dir(path))
}
