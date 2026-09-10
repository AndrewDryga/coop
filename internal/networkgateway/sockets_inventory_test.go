package networkgateway

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func proc6Address(value string) string {
	address := netip.MustParseAddrPort(value)
	raw := address.Addr().As16()
	return fmt.Sprintf("%08X%08X%08X%08X:%04X", binary.NativeEndian.Uint32(raw[:4]), binary.NativeEndian.Uint32(raw[4:8]),
		binary.NativeEndian.Uint32(raw[8:12]), binary.NativeEndian.Uint32(raw[12:]), address.Port())
}

func proc6Row(peer string, uid, inode int) string {
	return fmt.Sprintf("0: %s %s 02 00:00 00:00 0 %d 0 %d 1 0\n", proc6Address("[2001:db8:1234:5678::2]:32000"), proc6Address(peer), uid, inode)
}

func TestSocketInventoryIPv6WordsMappedAddressesAndCapturePredicate(t *testing.T) {
	for _, value := range []string{"[2001:4860:4860::8888]:443", "[::1]:53", "[::ffff:1.2.3.4]:443", "[::ffff:169.254.169.254]:443"} {
		got, err := procSocketAddress(proc6Address(value), true)
		want := netip.MustParseAddrPort(value)
		want = netip.AddrPortFrom(want.Addr().Unmap(), want.Port())
		if err != nil || got != want {
			t.Fatalf("tcp6 word decoding: %s => %s, %v", value, got, err)
		}
	}
	for _, tc := range []struct {
		peer string
		rows int
	}{
		{"[::ffff:1.2.3.4]:443", 0}, {"[::ffff:1.2.3.4]:53", 0},
		{"[::ffff:169.254.169.254]:443", 1}, {"[2001:4860:4860::8888]:443", 1},
		{"[2001:4860:4860::8888]:53", 1},
	} {
		rows, err := parseSocketTables([]socketTable{{reader: strings.NewReader(procHeader + proc6Row(tc.peer, 1000, 42)), ipv6: true}}, boundary{})
		if err != nil || len(rows) != tc.rows {
			t.Fatalf("capture predicate %s: rows=%d err=%v", tc.peer, len(rows), err)
		}
	}
}

func TestSocketInventoryCombinedBoundPrioritizesLiveServiceOverRemnants(t *testing.T) {
	for _, floodUID := range []int{1000, 65532} {
		flood := procRow(floodUID, "05", 0)
		flood = strings.Replace(flood, "01010101:01BB", "01010101:0050", 1)
		v4 := procHeader + strings.Repeat(flood, MaxSocketInventory+3)
		v6 := procHeader + proc6Row("[2001:4860:4860::8888]:443", 65532, 4242)
		rows, err := parseSocketTables([]socketTable{{reader: strings.NewReader(v4)}, {reader: strings.NewReader(v6), ipv6: true}}, boundary{})
		var truncated *inventoryTruncated
		if !errors.As(err, &truncated) || truncated.omitted != 4 || len(rows) != MaxSocketInventory || rows[0].Inode != 4242 {
			t.Fatalf("combined bound lost live service row: rows=%d first=%+v err=%v", len(rows), rows[0], err)
		}
		rows, err = parseSocketTables([]socketTable{{reader: strings.NewReader(v4 + "bad trailing row\n")}}, boundary{})
		if err == nil || rows != nil {
			t.Fatal("overflow hid malformed trailing input")
		}
	}
}
