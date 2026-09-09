package networkgateway

import (
	"golang.org/x/sys/unix"
	"net"
)

func servicePeer(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	allowed := false
	err = raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		// PID is not a useful identity across these separate PID namespaces.
		allowed = err == nil && credentials.Uid == 65532
	})
	return err == nil && allowed
}
