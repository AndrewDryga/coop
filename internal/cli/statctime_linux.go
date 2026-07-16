//go:build linux

package cli

import "syscall"

func statChangeTime(stat *syscall.Stat_t) (int64, int64) {
	return stat.Ctim.Sec, stat.Ctim.Nsec
}
