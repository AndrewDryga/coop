//go:build darwin

package processidentity

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const platformTokenPrefix = "darwin-kinfo-v1:"

func platformStartToken(pid int) string {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid {
		return ""
	}
	start := kp.Proc.P_starttime
	return fmt.Sprintf("%s%d:%d", platformTokenPrefix, start.Sec, start.Usec)
}

// macOS has no PID namespaces: every process on the host sees the same pids.
func platformPIDNamespace() (string, bool) { return "", true }

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
