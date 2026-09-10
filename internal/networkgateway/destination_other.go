//go:build !linux

package networkgateway

import (
	"net"
	"net/netip"
)

// Capture runs in the qualified Linux gateway, not on the host OS.
func readOriginalDestination(net.Conn) (netip.AddrPort, error) {
	return netip.AddrPort{}, Failure("gateway_destination_unknown")
}
