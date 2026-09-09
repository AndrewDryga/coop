//go:build linux

package networkgateway

import (
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func envoyProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func verifyServiceRole(role string) error {
	uid, caps := 65532, uint64(0)
	if role == "controller" {
		uid, caps = 0, 1<<12 // CAP_NET_ADMIN, never a general root capability set
	} else if role != "guard" {
		return Failure("gateway_role_invalid")
	}
	if os.Getuid() != uid || os.Geteuid() != uid || os.Getgid() != 65532 || os.Getegid() != 65532 {
		return Failure("gateway_role_invalid")
	}
	file, err := os.Open("/proc/thread-self/status")
	if err != nil {
		return Failure("gateway_role_invalid")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return Failure("gateway_role_invalid")
	}
	return checkRoleStatus(string(data), caps)
}

func checkRoleStatus(data string, caps uint64) error {
	wanted := map[string]uint64{"CapEff:": caps, "CapPrm:": caps, "CapBnd:": caps, "CapInh:": 0, "CapAmb:": 0, "NoNewPrivs:": 1, "Seccomp:": 2}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		expected, found := wanted[fields[0]]
		if !found {
			continue
		}
		base := 16
		if fields[0] == "NoNewPrivs:" || fields[0] == "Seccomp:" {
			base = 10
		}
		value, err := strconv.ParseUint(fields[1], base, 64)
		if err != nil || value != expected {
			return Failure("gateway_role_invalid")
		}
		delete(wanted, fields[0])
	}
	if len(wanted) != 0 {
		return Failure("gateway_role_invalid")
	}
	return nil
}
