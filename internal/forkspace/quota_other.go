//go:build !linux

package forkspace

import "syscall"

func filesystemWithQuota(_ string, _ *syscall.Statfs_t, volume Filesystem) (Filesystem, error) {
	return volume, nil
}
