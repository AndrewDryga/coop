//go:build !linux

package networkgateway

import "net"

// Enforcement runs in the qualified Linux gateway, not on the host OS.
func servicePeer(*net.UnixConn) bool { return false }
