package networkgateway

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
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
	// The flow's own authoritative close explains the socket at its tuple, so the
	// join retires instead of expiring as unattributed. That is an explanation,
	// not ownership: no inode was ever adopted and the meters stay the flow's.
	if *c.snapshot.Counters.SentBytes != 3 || c.snapshot.Coverage.ProxyBytes.Status != "exact" ||
		c.snapshot.Coverage.BoundaryAttribution.Status != "exact" || c.snapshot.Loss.Unknown ||
		slices.ContainsFunc(c.closed, func(row networkview.Connection) bool { return row.NameSource == "unattributed-history" }) {
		t.Fatal("unjoined short lifetime erased its real meters or kept a gap its own close explains")
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

// The live smoke run flaked here: a single curl finishes inside one sampling
// interval, so the collector consumes the flow's connect and authoritative end
// together while the kernel still holds the upstream socket. Its signature is a
// terminal unattributed_socket at the exact peer of a cleanly closed flow.
func TestCollectorClosedFlowExplainsItsLingeringUpstreamSocket(t *testing.T) {
	for _, scenario := range []string{"lingering-into-terminal", "pending-before-close"} {
		t.Run(scenario, func(t *testing.T) {
			c, now := collectorFixture(t)
			flow := strings.Repeat("e", 32)
			connected := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 1, 2, 10)
			end := proxyEvent(2, *now, flow, "TcpConnectionEnd", 3, 4, 20)
			row := SocketRow{Tuple: SocketTuple{Local: connected.Local, Peer: connected.Peer}, UID: 65532, Inode: 42, State: "closing"}
			lingering := []SocketRow{row}
			if scenario == "pending-before-close" {
				// The socket is sampled before any proxy event carries its tuple, so
				// it opens a join the flow can no longer close once it has ended.
				c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, nil, EnvoyTotals{})
				publishFixture(c, *now, lingering)
				if c.snapshot.PendingConnections != 1 || c.boundaryGap != "" {
					t.Fatal("socket sampled before its flow's tuple did not open a bounded join")
				}
				lingering = nil // and the kernel releases it before the terminal sample
			}
			*now = now.Add(time.Second)
			connected.BootAt, end.BootAt = *now, *now
			c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{connected, end}, EnvoyTotals{Sequence: 2})
			publishFixture(c, *now, lingering)
			if c.boundaryGap != "" || c.snapshot.PendingConnections != 0 || *c.snapshot.UnknownConnections != 0 {
				t.Fatalf("accounted remnant became an unknown external socket: gap=%q pending=%d", c.boundaryGap, c.snapshot.PendingConnections)
			}
			*now = now.Add(time.Second)
			c.terminal = true
			c.envoyTotals.Stopped = true
			publishFixture(c, *now, lingering)
			if c.snapshot.Loss.Unknown || c.snapshot.Coverage.BoundaryAttribution.Status != "exact" || len(c.snapshot.Loss.Reasons) != 0 {
				t.Fatalf("terminal sample called a closed flow's own socket evidence loss: %v", c.snapshot.Loss)
			}
			if *c.snapshot.Counters.SentBytes != 3 || *c.snapshot.Counters.ReceivedBytes != 4 || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
				t.Fatal("reconciliation disturbed the measured proxy totals")
			}
			for _, connection := range c.snapshot.Connections {
				if connection.NameSource == "unattributed" || connection.NameSource == "unattributed-history" {
					t.Fatalf("accounted socket retained as unattributed history: %+v", connection)
				}
			}
		})
	}
}

func TestCollectorLingeringSocketWithoutItsOwnClosedFlowStaysUnattributed(t *testing.T) {
	for _, scenario := range []string{"no-flow", "other-inode", "second-identity"} {
		t.Run(scenario, func(t *testing.T) {
			c, now := collectorFixture(t)
			flow := strings.Repeat("f", 32)
			connected := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 1, 2, 10)
			end := proxyEvent(2, *now, flow, "TcpConnectionEnd", 3, 4, 20)
			row := SocketRow{Tuple: SocketTuple{Local: connected.Local, Peer: connected.Peer}, UID: 65532, Inode: 42, State: "open"}
			switch scenario {
			case "other-inode":
				// A sample bound this flow to inode 42, so a different socket at the
				// same tuple is a second identity its close cannot account for.
				c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{connected}, EnvoyTotals{Sequence: 1})
				publishFixture(c, *now, []SocketRow{row})
				if c.flows[flow].inode != 42 || c.snapshot.PendingConnections != 0 {
					t.Fatal("live join did not settle the sampled inode")
				}
				*now = now.Add(time.Second)
				c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{end}, EnvoyTotals{Sequence: 2})
				row.Inode = 43
			case "second-identity":
				// One ended stream explains ONE kernel socket: the close pins the
				// first identity it accounts for, never a later one at that tuple.
				c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{connected, end}, EnvoyTotals{Sequence: 2})
				publishFixture(c, *now, []SocketRow{row})
				if c.snapshot.PendingConnections != 0 || *c.snapshot.UnknownConnections != 0 {
					t.Fatal("the close did not account for its own lingering socket")
				}
				*now = now.Add(time.Second)
				row.Inode = 43
			}
			publishFixture(c, *now, []SocketRow{row})
			if c.snapshot.PendingConnections != 1 || *c.snapshot.UnknownConnections != 1 {
				t.Fatal("unaccounted socket was hidden instead of joined")
			}
			*now = now.Add(time.Second)
			c.terminal = true
			c.envoyTotals.Stopped = true
			publishFixture(c, *now, []SocketRow{row})
			if !c.snapshot.Loss.Unknown || c.snapshot.Coverage.BoundaryAttribution.Reason != "unattributed_socket" ||
				!slices.ContainsFunc(c.closed, func(row networkview.Connection) bool { return row.Reason == "socket_join_terminal" }) {
				t.Fatalf("socket no closed flow claims lost its attribution gap: %v", c.snapshot.Loss)
			}
		})
	}
}

// A retained close explains the remnant of ITS stream, which the kernel holds
// for a few samples — not a socket at the same tuple minutes later. Without a
// time bound one close with an unbound inode would explain the first socket at
// that ephemeral port forever, and a real gap would go unreported.
func TestCollectorClosedFlowExplanationExpires(t *testing.T) {
	fixture := func(t *testing.T, wait time.Duration) *Collector {
		t.Helper()
		c, now := collectorFixture(t)
		flow := strings.Repeat("a", 32)
		connected := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 1, 2, 10)
		end := proxyEvent(2, *now, flow, "TcpConnectionEnd", 3, 4, 20)
		c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{connected, end}, EnvoyTotals{Sequence: 2})
		publishFixture(c, *now, nil) // the close is retained with no socket bound to it
		*now = now.Add(wait)
		row := SocketRow{Tuple: SocketTuple{Local: connected.Local, Peer: connected.Peer}, UID: 65532, Inode: 77, State: "open"}
		publishFixture(c, *now, []SocketRow{row})
		return c
	}
	fresh := fixture(t, time.Second)
	if fresh.snapshot.PendingConnections != 0 || *fresh.snapshot.UnknownConnections != 0 || fresh.boundaryGap != "" {
		t.Fatalf("a remnant inside the bound stopped being explained: pending=%d gap=%q", fresh.snapshot.PendingConnections, fresh.boundaryGap)
	}
	stale := fixture(t, ObservationStaleAfter+time.Second)
	if stale.snapshot.PendingConnections != 1 || *stale.snapshot.UnknownConnections != 1 {
		t.Fatalf("a stale close still explained a reused tuple: pending=%d unknown=%v", stale.snapshot.PendingConnections, stale.snapshot.UnknownConnections)
	}
	if len(stale.closedSockets) != 0 {
		t.Fatalf("the expired close was retained: %v", stale.closedSockets)
	}
}
