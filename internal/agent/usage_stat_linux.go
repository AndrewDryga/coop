package agent

import "syscall"

func usageChangeTime(stat *syscall.Stat_t) int64 {
	return stat.Ctim.Sec*1e9 + stat.Ctim.Nsec
}
