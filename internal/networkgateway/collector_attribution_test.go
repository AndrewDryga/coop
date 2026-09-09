package networkgateway

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestCollectorDelayedConnectedEventReconcilesWithoutLosingProxyCounters(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("a", 32)
	event := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 3, 7, 10)
	row := SocketRow{Tuple: SocketTuple{Local: event.Local, Peer: event.Peer}, UID: 65532, Inode: 42, State: "connecting"}
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, nil, EnvoyTotals{})
	publishFixture(c, *now, []SocketRow{row})
	if c.snapshot.PendingConnections != 1 || *c.snapshot.UnknownConnections != 1 || c.snapshot.Coverage.BoundaryAttribution.Reason != "socket_join_pending" || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("pending socket hidden or incorrectly called proxy counter loss")
	}
	*now = now.Add(time.Second)
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{event}, EnvoyTotals{Sequence: 1})
	row.State = "open"
	publishFixture(c, *now, []SocketRow{row})
	if c.snapshot.PendingConnections != 0 || *c.snapshot.UnknownConnections != 0 || c.snapshot.Coverage.BoundaryAttribution.Status != "exact" || c.snapshot.Loss.Unknown || *c.snapshot.Counters.SentBytes != 3 {
		t.Fatal("same retained inode did not reconcile or duplicated bytes")
	}
}

func TestCollectorPendingDisappearanceTimeoutAndTerminalRemainHistoryGaps(t *testing.T) {
	for _, scenario := range []string{"disappeared", "expired-earlier", "terminal"} {
		t.Run(scenario, func(t *testing.T) {
			c, now := collectorFixture(t)
			flow := strings.Repeat("b", 32)
			event := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 1, 2, 10)
			row := SocketRow{Tuple: SocketTuple{Local: event.Local, Peer: event.Peer}, UID: 65532, Inode: 42, State: "open"}
			publishFixture(c, *now, []SocketRow{row})
			var rows []SocketRow
			if scenario == "terminal" {
				c.terminal = true
				c.envoyTotals.Stopped = true
			} else {
				*now = now.Add(ObservationStaleAfter)
			}
			if scenario == "expired-earlier" {
				publishFixture(c, *now, nil)
				*now = now.Add(time.Second)
				event.BootAt = *now
				c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{event}, EnvoyTotals{Sequence: 1})
				rows = []SocketRow{row}
			}
			publishFixture(c, *now, rows)
			if !c.snapshot.Loss.Unknown || c.snapshot.Coverage.BoundaryAttribution.Status != "lower-bound" || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
				t.Fatal("unresolved history vanished or destroyed independent proxy totals")
			}
		})
	}
}

func TestCollectorSameSampleKnownJoinPrecedesPendingExpiry(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("d", 32)
	event := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 1, 2, 10)
	row := SocketRow{Tuple: SocketTuple{Local: event.Local, Peer: event.Peer}, UID: 65532, Inode: 42, State: "connecting"}
	publishFixture(c, *now, []SocketRow{row})
	*now = now.Add(ObservationStaleAfter)
	event.BootAt = *now
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{event}, EnvoyTotals{Sequence: 1})
	row.State = "open"
	publishFixture(c, *now, []SocketRow{row})
	if c.snapshot.PendingConnections != 0 || c.boundaryGap != "" || c.snapshot.Loss.Unknown || c.snapshot.Coverage.BoundaryAttribution.Status != "exact" {
		t.Fatal("same-sample retained inode join lost to expiry ordering")
	}
}

func TestCollectorPendingCapacityDoesNotHideInventoryOrClaimCompleteness(t *testing.T) {
	c, now := collectorFixture(t)
	rows := make([]SocketRow, MaxPendingJoins+1)
	for i := range rows {
		rows[i] = SocketRow{UID: 65532, Inode: uint64(i + 1), State: "open", Tuple: SocketTuple{Local: netip.MustParseAddrPort(fmt.Sprintf("172.17.0.2:%d", 30000+i)), Peer: netip.MustParseAddrPort("1.1.1.1:443")}}
	}
	publishFixture(c, *now, rows)
	if len(c.pending) != MaxPendingJoins || *c.snapshot.UnknownConnections != MaxPendingJoins+1 || !c.snapshot.Loss.Unknown || c.snapshot.Coverage.BoundaryAttribution.Reason != "socket_join_capacity" || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("pending bound corrupted completeness or total inventory")
	}
}

func TestCollectorKernelClosingRemnantsNeverBecomeLiveStreamsOrZeroMeters(t *testing.T) {
	for _, scenario := range []string{"closing-zero", "closing-inode", "open-zero", "connecting-zero"} {
		t.Run(scenario, func(t *testing.T) {
			c, now := collectorFixture(t)
			row := SocketRow{UID: 65532, State: "closing", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:32000"), Peer: netip.MustParseAddrPort("1.1.1.1:443")}}
			switch scenario {
			case "closing-inode":
				row.Inode = 42
			case "open-zero":
				row.State = "open"
			case "connecting-zero":
				row.State = "connecting"
			}
			publishFixture(c, *now, []SocketRow{row})
			s := c.Snapshot()
			if len(s.Connections) != 1 || s.Connections[0].SentBytes != nil || s.Connections[0].ReceivedBytes != nil || s.Connections[0].Rate != nil {
				t.Fatal("kernel sample hidden or invented byte meter")
			}
			if scenario == "closing-zero" {
				if *s.LiveConnections != 0 || *s.KernelClosingSockets != 1 || s.Connections[0].State != "kernel-closing" || s.Coverage.ProxyBytes.Status != "exact" || s.Loss.Unknown {
					t.Fatal("kernel remnant became application connection or proxy loss")
				}
			} else if *s.LiveConnections != 1 || *s.KernelClosingSockets != 0 || *s.UnknownConnections != 1 {
				t.Fatal("non-retired unknown socket hidden as remnant")
			}
		})
	}
}

func TestCollectorUnboundShortCloseDoesNotInventChangedInodeOrOwnership(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("c", 32)
	connected := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 1, 2, 10)
	c.ingest([]GuardEvent{registration(1, *now, flow), {Sequence: 2, Kind: "private_flow_closed", FlowID: flow, BootAt: *now}}, GuardTotals{Sequence: 2}, []EnvoyEvent{connected}, EnvoyTotals{Sequence: 1})
	row := SocketRow{Tuple: SocketTuple{Local: connected.Local, Peer: connected.Peer}, UID: 65532, Inode: 42, State: "open"}
	publishFixture(c, *now, []SocketRow{row})
	if c.boundaryGap != "" || c.flows[flow].inode != 0 || *c.snapshot.UnknownConnections != 1 || c.snapshot.PendingConnections != 1 || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("unbound is not changed or proof of a closed flow's inode")
	}
	c.ingest(nil, GuardTotals{Sequence: 2}, []EnvoyEvent{proxyEvent(2, *now, flow, "TcpConnectionEnd", 3, 4, 20)}, EnvoyTotals{Sequence: 2})
	c.terminal = true
	c.envoyTotals.Stopped = true
	publishFixture(c, *now, nil)
	if *c.snapshot.Counters.SentBytes != 3 || c.snapshot.Coverage.ProxyBytes.Status != "exact" || c.snapshot.Coverage.BoundaryAttribution.Status != "lower-bound" || !c.snapshot.Loss.Unknown {
		t.Fatal("unjoined short lifetime erased its real meters or fabricated complete attribution")
	}
}

func TestCollectorMaintenanceBirthReconcilesOnlyTheSameRetainedInode(t *testing.T) {
	c, now := collectorFixture(t)
	row := SocketRow{UID: 65532, Inode: 42, State: "open", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:49153"), Peer: netip.MustParseAddrPort("1.1.1.1:443")}}
	publishFixture(c, *now, []SocketRow{row})
	conn := c.doh.sockets.track(closeHookConn{closeHook: func() {}})
	defer conn.Close()
	*now = now.Add(time.Second)
	row.Inode = 43 // a different socket at the same tuple cannot settle inode 42
	c.publish(KernelSample{Sequence: 1, BootAt: *now, EnforcerReady: true, Counters: &KernelCounters{}}, nil, []SocketRow{row}, nil, c.doh.sockets.snapshot(), true)
	if c.snapshot.PendingConnections != 1 || c.snapshot.Coverage.BoundaryAttribution.Status != "lower-bound" {
		t.Fatalf("maintenance tuple reuse borrowed prior pending ownership: pending=%v owned=%+v coverage=%+v", c.pending, c.doh.sockets.snapshot(), c.snapshot.Coverage)
	}
	*now = now.Add(ObservationStaleAfter)
	publishFixture(c, *now, nil)
	if !c.snapshot.Loss.Unknown || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("unmatched maintenance history disappeared or became proxy meter loss")
	}
}
