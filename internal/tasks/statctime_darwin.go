//go:build darwin

package tasks

import "syscall"

func statChangeTime(stat *syscall.Stat_t) (int64, int64) {
	return stat.Ctimespec.Sec, stat.Ctimespec.Nsec
}
