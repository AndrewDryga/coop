//go:build darwin

package processidentity

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func platformStartToken(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return ""
	}
	start := kp.Proc.P_starttime
	return fmt.Sprintf("darwin-kinfo-v1:%d:%d", start.Sec, start.Usec)
}
