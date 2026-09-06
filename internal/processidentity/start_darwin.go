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

func platformParent(pid int) int {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return 0
	}
	return int(kp.Eproc.Ppid)
}

func platformCommand(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return ""
	}
	return unix.ByteSliceToString(kp.Proc.P_comm[:])
}
