package networkgateway

import (
	"context"
	"net"
	"net/netip"
	"testing"
)

// The owner registry and kernel inventory are independent observations. These
// interleavings require no timers and must not manufacture historical ownership.
func TestCollectorMaintenanceLifecycleAcrossInventoryRetainsAttributionGap(t *testing.T) {
	for _, phase := range []string{"birth", "retirement"} {
		t.Run(phase, func(t *testing.T) {
			c, _ := collectorFixture(t)
			row := SocketRow{UID: 65532, Inode: 42, State: "open", Tuple: SocketTuple{
				Local: netip.MustParseAddrPort("172.17.0.2:49153"), Peer: netip.MustParseAddrPort("1.1.1.1:443")}}
			var conn net.Conn
			if phase == "retirement" {
				conn = c.doh.sockets.track(closeHookConn{closeHook: func() {}})
			}
			c.inventory = func() ([]SocketRow, error) {
				if phase == "birth" {
					conn = c.doh.sockets.track(closeHookConn{closeHook: func() {}})
				} else if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
				return []SocketRow{row}, nil
			}
			c.Sample(context.Background(), true)
			if c.snapshot.PendingConnections != 1 || *c.snapshot.UnknownConnections != 1 || c.boundaryGap != "" {
				t.Fatal("unjoined inventory did not remain pending")
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			c.inventory = func() ([]SocketRow, error) { return nil, nil }
			c.envoy.Finish(nil)
			c.sample(context.Background(), false, true, 0)
			if c.snapshot.PendingConnections != 0 || *c.snapshot.UnknownConnections != 0 ||
				c.snapshot.Coverage.BoundaryAttribution.Reason != "unattributed_socket" || !c.snapshot.Loss.Unknown ||
				c.snapshot.Coverage.ProxyBytes.Status != "exact" || c.snapshot.Coverage.MaintenanceBytes.Status != "exact" {
				t.Fatal("empty final inventory erased unresolved history or corrupted independent meters")
			}
			if len(c.snapshot.Connections) != 1 || c.snapshot.Connections[0].NameSource != "unattributed-history" || c.snapshot.Connections[0].Reason != "socket_join_terminal" {
				t.Fatal("terminal reconciliation discarded its unresolved socket evidence")
			}
		})
	}
}
