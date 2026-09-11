package forkspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Bounds on one measurement. The shared-inode set is the only thing a scan has to remember, and a
// fork root with more entries than a worker could plausibly own is reported as unknown rather than
// walked until the process runs out of memory.
const (
	maxUsageSharedInodes = 1 << 20
	maxUsageEntries      = 1 << 22
	usageBlockBytes      = 512 // stat(2) st_blocks is 512-byte units on both Linux and Darwin
)

// Usage is the storage one directory tree PHYSICALLY holds.
//
// It is allocated blocks, not apparent size: a sparse file, a compressed extent, and a 5-byte file
// in a 4 KiB cluster all charge what the volume actually gave them, which is the only number that
// changes when the tree is removed.
//
// SharedBytes is the part of AllocatedBytes sitting on inodes with more than one link — a fork's
// git objects hardlinked from the parent checkout, most of the time. Removing the fork does not
// return those bytes, so only ExclusiveBytes is reclaimable, and a caller that treats
// AllocatedBytes as reclaimable would promise storage it cannot deliver.
//
// Unknown marks a measurement that could not see everything: an unreadable directory, an inode
// whose identity the kernel would not give up, a tree past the bounds above. The totals then read
// as a FLOOR. Unknown is never zero.
//
// Nothing here opens a file. The measurement is Lstat only, so it can run over credential-bearing
// private state (an agent's ACP home) without reading a byte of it.
type Usage struct {
	AllocatedBytes int64
	SharedBytes    int64
	Entries        int64
	Unknown        bool
}

// ExclusiveBytes is what removing this tree would actually return to the volume.
func (u Usage) ExclusiveBytes() int64 { return u.AllocatedBytes - u.SharedBytes }

// Add folds one subtree's measurement into another, preserving the unknown flag: a total built
// from an incomplete part is itself incomplete.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		AllocatedBytes: u.AllocatedBytes + other.AllocatedBytes,
		SharedBytes:    u.SharedBytes + other.SharedBytes,
		Entries:        u.Entries + other.Entries,
		Unknown:        u.Unknown || other.Unknown,
	}
}

// Filesystem is the volume a path lives on. FreeBytes is what an unprivileged writer may still
// use, not the kernel's reserved remainder, because that is the space a new fork can actually take.
type Filesystem struct {
	CapacityBytes int64
	FreeBytes     int64
}

// UsageScan measures several trees under ONE accounting. A fork's git objects are hardlinked from
// the checkout it was cloned from and from every sibling fork, so measuring each tree in isolation
// would charge the same baseline to all of them and report a fork root far larger than the volume
// holds. Inside one scan a shared inode is charged exactly once, to whichever tree reaches it
// first; single-link — that is, reclaimable — bytes never depend on that order.
type UsageScan struct {
	shared  map[[2]uint64]struct{}
	entries int64
}

func NewUsageScan() *UsageScan { return &UsageScan{shared: make(map[[2]uint64]struct{})} }

// MeasureUsage measures one tree on its own. Use a UsageScan when several trees may share inodes.
func MeasureUsage(path string) (Usage, error) { return NewUsageScan().Measure(path) }

// Measure walks one tree and reports what it physically holds. It never follows a symlink —
// neither as the root nor inside — so a link planted in a workspace cannot charge an unrelated
// volume to this fork or send the scan out of the fork root.
func (s *UsageScan) Measure(path string) (Usage, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Usage{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Usage{}, fmt.Errorf("measurement root %q is not a real directory", path)
	}
	usage := Usage{}
	shared := s.shared
	walkErr := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory coop cannot read still holds bytes. Skip it, keep the rest, and say so.
			usage.Unknown = true
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if s.entries >= maxUsageEntries {
			usage.Unknown = true
			return fs.SkipAll
		}
		s.entries++
		usage.Entries++
		stat, ok := entryStat(entry)
		if !ok {
			usage.Unknown = true
			return nil
		}
		blocks := int64(stat.Blocks) * usageBlockBytes
		if blocks < 0 {
			usage.Unknown = true
			return nil
		}
		if stat.Nlink <= 1 {
			usage.AllocatedBytes += blocks
			return nil
		}
		key := [2]uint64{uint64(stat.Dev), stat.Ino}
		if _, seen := shared[key]; seen {
			return nil
		}
		if len(shared) >= maxUsageSharedInodes {
			// Past this point the same inode may be counted twice. Keep measuring — a floor that
			// is too high is still information — but never call the answer exact.
			usage.Unknown = true
		} else {
			shared[key] = struct{}{}
		}
		usage.AllocatedBytes += blocks
		usage.SharedBytes += blocks
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return usage, walkErr
	}
	return usage, nil
}

func entryStat(entry fs.DirEntry) (*syscall.Stat_t, bool) {
	info, err := entry.Info()
	if err != nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok && stat != nil
}

// MeasureFilesystem reports the volume behind path. It is the cheap half of storage accounting —
// one statfs, no walk — which is why the pre-allocation pressure check can afford to re-read it
// every time instead of trusting a cached tree scan.
func MeasureFilesystem(path string) (Filesystem, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Filesystem{}, fmt.Errorf("read filesystem for %q: %w", path, err)
	}
	block := int64(stat.Bsize)
	if block <= 0 {
		return Filesystem{}, fmt.Errorf("filesystem for %q reports an invalid block size", path)
	}
	capacity := int64(stat.Blocks) * block
	free := int64(stat.Bavail) * block
	if capacity <= 0 || free < 0 {
		return Filesystem{}, fmt.Errorf("filesystem for %q reports an invalid capacity", path)
	}
	if free > capacity {
		free = capacity
	}
	return Filesystem{CapacityBytes: capacity, FreeBytes: free}, nil
}
