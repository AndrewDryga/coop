package networkgateway

import (
	"encoding/binary"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// readOriginalDestination returns the destination the KERNEL recorded for this
// connection before the capture chain redirected it. It is the only source of
// an upstream port in a filtered run: a client cannot write it, and a
// connection nothing redirected answers with the guard's own listener instead.
func readOriginalDestination(conn net.Conn) (netip.AddrPort, error) {
	source, ok := conn.(syscall.Conn)
	if !ok {
		return netip.AddrPort{}, Failure("gateway_destination_unknown")
	}
	raw, err := source.SyscallConn()
	if err != nil {
		return netip.AddrPort{}, Failure("gateway_destination_unknown")
	}
	var recorded *unix.IPv6Mreq
	var optErr error
	// SO_ORIGINAL_DST answers with a sockaddr_in, and IPv6Mreq is the fixed
	// 20-byte carrier x/sys/unix exposes for a getsockopt of that width:
	// family, port in network order, then the IPv4 address.
	controlErr := raw.Control(func(fd uintptr) {
		recorded, optErr = unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, unix.SO_ORIGINAL_DST)
	})
	if controlErr != nil || optErr != nil || recorded == nil ||
		binary.NativeEndian.Uint16(recorded.Multiaddr[0:2]) != unix.AF_INET {
		return netip.AddrPort{}, Failure("gateway_destination_unknown")
	}
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte(recorded.Multiaddr[4:8])), binary.BigEndian.Uint16(recorded.Multiaddr[2:4])), nil
}
