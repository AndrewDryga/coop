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

// The same contract without a kernel: a released identity whose inode the registry holds explains
// its own remnant, a release no inode or clock backs explains nothing, and the registry hands each
// release to one sample only, keeping the newest when a burst outruns the bound.
func TestCollectorRetiresReleasedResolverIdentities(t *testing.T) {
	for _, inode := range []uint64{42, 0} {
		c, now := collectorFixture(t)
		conn := c.doh.sockets.track(closeHookConn{closeHook: func() {}}).(*maintenanceConn)
		conn.inode = inode // the fd read a fake connection cannot give
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		row := SocketRow{UID: 65532, Inode: 42, State: "closing", Tuple: conn.tuple}
		sampleReleasedFixture(c, *now, []SocketRow{row})
		if explained := c.snapshot.PendingConnections == 0; explained != (inode != 0) {
			t.Fatalf("inode %d: remnant explained = %v", inode, explained)
		}
		if len(c.doh.sockets.drainReleased()) != 0 {
			t.Fatal("a release was handed to a second sample")
		}
	}
	var untimed maintenanceSockets // no clock: the release has no instant to expire from
	conn := untimed.track(closeHookConn{closeHook: func() {}}).(*maintenanceConn)
	conn.inode = 42
	_ = conn.Close()
	if released := untimed.drainReleased(); len(released) != 0 {
		t.Fatalf("an untimed release was retained: %+v", released)
	}
	c, _ := collectorFixture(t)
	for i := range maxReleasedMaintenance + 3 {
		conn := c.doh.sockets.track(closeHookConn{closeHook: func() {}}).(*maintenanceConn)
		conn.inode = uint64(i + 1)
		_ = conn.Close()
	}
	if released := c.doh.sockets.drainReleased(); len(released) != maxReleasedMaintenance || released[0].Inode != 4 {
		t.Fatalf("a burst kept %d releases, want the newest %d", len(released), maxReleasedMaintenance)
	}
}
