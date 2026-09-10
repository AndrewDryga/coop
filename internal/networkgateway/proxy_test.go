package networkgateway

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"testing"
)

func TestProxyHeaderPinsIPv4PortAndCorrelationOnlyTLV(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	header, err := ProxyHeader(netip.AddrPortFrom(netip.MustParseAddr("93.184.216.34"), 8443), id)
	if err != nil {
		t.Fatal(err)
	}
	if string(header[:12]) != "\r\n\r\n\x00\r\nQUIT\n" || header[12] != 0x21 || header[13] != 0x11 ||
		int(binary.BigEndian.Uint16(header[14:])) != len(header)-16 || binary.BigEndian.Uint16(header[26:]) != 8443 ||
		header[28] != 5 || binary.BigEndian.Uint16(header[29:]) != 32 || string(header[31:]) != id {
		t.Fatalf("unexpected trusted PROXY v2 layout: %x", header)
	}
	if ip := netip.AddrFrom4([4]byte(header[20:24])); ip.String() != "93.184.216.34" {
		t.Fatal("header changed validated destination")
	}
	for _, invalid := range []string{"", "-", strings.Repeat("g", 32), strings.Repeat("A", 32), strings.Repeat("a", 31) + "\n"} {
		if _, err := ProxyHeader(netip.AddrPortFrom(netip.MustParseAddr("1.1.1.1"), 443), invalid); err == nil {
			t.Fatalf("accepted hostile correlation ID %q", invalid)
		}
	}
	if _, err := ProxyHeader(netip.AddrPortFrom(netip.MustParseAddr("::ffff:1.1.1.1"), 443), id); err == nil {
		t.Fatal("mapped address bypassed first-slice family restriction")
	}
}
