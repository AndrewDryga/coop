package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	pressure := err == nil && s.companionSnapshotPressure(volume)
	count, problems := pruneCompanionSnapshotsSweep(ctx, s.stateRoot, time.Now(), limits.DisposableBudgetBytes, limits.MaxReclaimPerPass, pressure, &s.storage.snapshotSweep)
	result.SnapshotsPruned += count
	result.Problems = append(result.Problems, problems...)
}

// companionSnapshotPressure says the disk is low enough to drop unused warm snapshots before their
// hour is up. The reserve must come from the measured capacity: the default policy's reserve for
// an unknown capacity is zero, which would count only a completely full disk.
func (s *Service) companionSnapshotPressure(volume forkspace.Filesystem) bool {
	return volume.FreeBytes <= 2*s.storageLimitsFor(volume.CapacityBytes).ReserveBytes
}

type companionSnapshotSweep struct {
	cursor          int
	beforeCandidate func(string) // deterministic filesystem/deadline fixture
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
	// Never walk whole payloads before deciding to reclaim. Budget sizes were measured
	// while sealing the immutable cache, and are only a warm-cache eviction hint.
	// Physical telemetry continues to measure live inodes independently.
	var overBudget map[string]bool
	if !pressure {
		overBudget = unusedSnapshotsOverBudget(ctx, parent, entries, budget)
	}
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
		if deleting != nil && !pressure && !overBudget[name] && now.Sub(info.ModTime()) < companionSnapshotWarmTime {
			continue
		}
		if unfinished && !retiredSnapshotName(name) && now.Sub(info.ModTime()) < companionSnapshotWarmTime {
			continue
		}
		attempts++
		unlock, err := forkspace.LockStateContext(ctx, stateRoot, forkspace.CompanionSnapshotLockPrefix+receipt.Key)
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
		}
	}
	return removed, problems
}

// unusedSnapshotsOverBudget names the unused complete snapshots to evict before their warm hour is
// up: the budget covers only snapshots no session uses, since a used one frees nothing when
// evicted. Counting the used ones too left a worker whose live sessions held more than the budget
// with no warm snapshot at all, so every next job paid a full clone. The oldest go first, only
// until the rest fit. Nothing is walked while every snapshot together fits; the use probe is a
// hint, and pruneCompanionSnapshot still proves a snapshot unused before evicting it.
func unusedSnapshotsOverBudget(ctx context.Context, parent string, entries []fs.DirEntry, budget int64) map[string]bool {
	type unused struct {
		name     string
		bytes    int64
		modified time.Time
	}
	var snapshots []unused
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if ctx.Err() != nil {
			return nil
		}
		if !entry.IsDir() || !validSessionDigest(name) {
			continue
		}
		path := filepath.Join(parent, name)
		receipt, err := readCompanionSnapshotReceipt(path)
		info, infoErr := entry.Info()
		if err != nil || infoErr != nil || !receipt.Complete || receipt.Key != name {
			continue // the sweep reports it
		}
		snapshots = append(snapshots, unused{name, receipt.AllocatedBytes, info.ModTime()})
		total += receipt.AllocatedBytes
	}
	if total <= budget {
		return nil
	}
	total = 0
	kept := snapshots[:0]
	for _, snapshot := range snapshots {
		if ctx.Err() != nil {
			return nil
		}
		if used, err := companionSnapshotInUse(filepath.Join(parent, snapshot.name, "tree")); err == nil && !used {
			kept = append(kept, snapshot)
			total += snapshot.bytes
		}
	}
	slices.SortFunc(kept, func(a, b unused) int { return a.modified.Compare(b.modified) })
	evict := map[string]bool{}
	for _, snapshot := range kept {
		if total <= budget {
			break
		}
		evict[snapshot.name] = true
		total -= snapshot.bytes
	}
	return evict
}

// companionSnapshotInUse reads the link count of the first payload file a projection shares: a
// projection links every one of them, so a single file tells. It stops there, where
// companionSnapshotHeld walks a whole unused tree.
func companionSnapshotInUse(tree string) (bool, error) {
	used := false
	found := false
	err := filepath.WalkDir(tree, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(tree, path)
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || !sharedCompanionFile(relative) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("snapshot link count is unavailable")
		}
		used, found = stat.Nlink > 1, true
		return fs.SkipAll
	})
	if err == nil && !found {
		err = errors.New("snapshot has no shared payload")
	}
	return used, err
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
