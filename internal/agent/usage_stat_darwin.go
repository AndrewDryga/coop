package agent

import "syscall"

func usageChangeTime(stat *syscall.Stat_t) int64 {
	return stat.Ctimespec.Sec*1e9 + stat.Ctimespec.Nsec
}
