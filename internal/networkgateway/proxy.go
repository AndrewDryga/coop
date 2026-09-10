package networkgateway

import (
	"encoding/binary"
	"encoding/hex"
	"net/netip"
)

// ProxyHeader writes only a trusted, validated IPv4 destination and opaque ID to
// the gateway-private Envoy socket. This function does not authorize the peer:
// the guard must first resolve SNI and obtain a controller lease for this exact
// address and port. Envoy dials the address:port written here, so the port must
// be the kernel's redirect record — never a value the client chose.
// PP2_TYPE_UNIQUE_ID is correlation metadata, never a routing override.
func ProxyHeader(peer netip.AddrPort, flowID string) ([]byte, error) {
	if !peer.IsValid() || !peer.Addr().Is4() || peer.Port() == 0 || len(flowID) != 32 {
		return nil, Failure("gateway_admission_invalid")
	}
	if _, err := hex.DecodeString(flowID); err != nil {
		return nil, Failure("gateway_admission_invalid")
	}
	for _, char := range flowID {
		if char >= 'A' && char <= 'F' {
			return nil, Failure("gateway_admission_invalid")
		}
	}
	const addressBytes = 12
	header := make([]byte, 16+addressBytes+3+len(flowID))
	copy(header, "\r\n\r\n\x00\r\nQUIT\n")
	header[12], header[13] = 0x21, 0x11 // PROXY v2, TCP over IPv4
	binary.BigEndian.PutUint16(header[14:16], uint16(addressBytes+3+len(flowID)))
	copy(header[16:20], []byte{127, 0, 0, 1})
	address := peer.Addr().As4()
	copy(header[20:24], address[:])
	binary.BigEndian.PutUint16(header[24:26], 1)
	binary.BigEndian.PutUint16(header[26:28], peer.Port())
	header[28] = 0x05
	binary.BigEndian.PutUint16(header[29:31], uint16(len(flowID)))
	copy(header[31:], flowID)
	return header, nil
}
