//go:build linux

package forkspace

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	btrfsMagic = 0x9123683e
	// Linux UAPI linux/btrfs.h: FS_INFO is 1024 bytes, INO_LOOKUP is 4096.
	btrfsFSInfo    = 0x8400941f
	btrfsInoLookup = 0xd0009412
)

// statfs describes Btrfs's whole backing filesystem, even when the containing
// subvolume has a much smaller hard quota. Read the kernel's own counters so
// admission and GC use the space this worker can actually allocate.
func filesystemWithQuota(path string, stat *syscall.Statfs_t, volume Filesystem) (Filesystem, error) {
	if uint32(stat.Type) != btrfsMagic {
		return volume, nil
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Filesystem{}, fmt.Errorf("open quota filesystem: %w", err)
	}
	defer file.Close()
	var info [1024]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), btrfsFSInfo, uintptr(unsafe.Pointer(&info[0]))); errno != 0 {
		return Filesystem{}, fmt.Errorf("read Btrfs filesystem identity: %w", errno)
	}
	lookup := struct {
		TreeID   uint64
		ObjectID uint64
		Name     [4080]byte
	}{ObjectID: 256}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), btrfsInoLookup, uintptr(unsafe.Pointer(&lookup))); errno != 0 {
		return Filesystem{}, fmt.Errorf("read Btrfs subvolume identity: %w", errno)
	}
	id := fmt.Sprintf("%x-%x-%x-%x-%x", info[16:20], info[20:22], info[22:24], info[24:26], info[26:32])
	filesystem := filepath.Join("/sys/fs/btrfs", id)
	if _, err := os.Stat(filesystem); err != nil {
		return Filesystem{}, fmt.Errorf("read Btrfs quota interface: %w", err)
	}
	return constrainBtrfsQuota(filepath.Join(filesystem, "qgroups", fmt.Sprintf("0_%d", lookup.TreeID)), volume)
}

func constrainBtrfsQuota(directory string, volume Filesystem) (Filesystem, error) {
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		// Qgroups depend on the filesystem's quota feature. No group means
		// there is no direct subvolume limit to add to statfs's constraints.
		return volume, nil
	} else if err != nil {
		return Filesystem{}, fmt.Errorf("read Btrfs quota directory: %w", err)
	}
	read := func(name string) (int64, error) {
		body, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return 0, fmt.Errorf("read Btrfs quota %s: %w", name, err)
		}
		value, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("btrfs quota %s is not a nonnegative byte count", name)
		}
		return value, nil
	}
	flags, err := read("limit_flags")
	if err != nil {
		return Filesystem{}, err
	}
	if flags&3 == 0 {
		return volume, nil
	}
	var reserved int64
	for _, name := range []string{"rsv_data", "rsv_meta_pertrans", "rsv_meta_prealloc"} {
		value, err := read(name)
		if err != nil {
			return Filesystem{}, err
		}
		if value > math.MaxInt64-reserved {
			return Filesystem{}, errors.New("btrfs quota reservations overflow")
		}
		reserved += value
	}
	for _, constraint := range []struct {
		flag        int64
		limit, used string
	}{{1, "max_referenced", "referenced"}, {2, "max_exclusive", "exclusive"}} {
		if flags&constraint.flag == 0 {
			continue
		}
		limit, err := read(constraint.limit)
		if err != nil {
			return Filesystem{}, err
		}
		if limit == 0 {
			return Filesystem{}, errors.New("btrfs quota has zero capacity")
		}
		used, err := read(constraint.used)
		if err != nil {
			return Filesystem{}, err
		}
		free := max(0, limit-used)
		free = max(0, free-reserved)
		volume.CapacityBytes = min(volume.CapacityBytes, limit)
		volume.FreeBytes = min(volume.FreeBytes, free, volume.CapacityBytes)
	}
	return volume, nil
}
