//go:build darwin

package cli

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// platformProcStartToken uses Darwin's kernel process start timeval, preserving microseconds and
// avoiding formatted ps output whose identity changes with timezone and locale.
func platformProcStartToken(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return ""
	}
	start := kp.Proc.P_starttime
	return fmt.Sprintf("darwin-kinfo-v1:%d:%d", start.Sec, start.Usec)
}
