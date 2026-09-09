package networkgateway

import (
	"strings"
	"testing"
	"time"
)

func TestCollectorTruncationRetainsPartialInventoryAndStickyHistory(t *testing.T) {
	c, now := collectorFixture(t)
	event := proxyEvent(1, *now, strings.Repeat("e", 32), "TcpUpstreamConnected", 0, 0, 10)
	row := SocketRow{Tuple: SocketTuple{Local: event.Local, Peer: event.Peer}, UID: 65532, Inode: 42, State: "open"}
	publishFixture(c, *now, []SocketRow{row})
	*now = now.Add(time.Second)
	c.publish(KernelSample{Sequence: 2, BootAt: *now, EnforcerReady: true, Counters: &KernelCounters{}}, nil, nil, &inventoryTruncated{omitted: 9}, nil, true)
	s := c.Snapshot()
	if s.PendingConnections != 1 || s.LiveConnections == nil || *s.LiveConnections != 0 || s.Coverage.SocketInventory.Status != "lower-bound" || s.Coverage.BoundaryAttribution.Status != "lower-bound" || !s.Loss.DetailTruncated || *s.Loss.OmittedDetails != 9 {
		t.Fatal("truncation erased pending ownership or claimed a complete empty sample")
	}
	*now = now.Add(time.Second)
	c.ingest([]GuardEvent{registration(1, *now, event.FlowID)}, GuardTotals{Sequence: 1}, []EnvoyEvent{event}, EnvoyTotals{Sequence: 1})
	publishFixture(c, *now, []SocketRow{row})
	s = c.Snapshot()
	if s.PendingConnections != 0 || s.Coverage.SocketInventory.Status != "exact" || s.Coverage.BoundaryAttribution.Status != "lower-bound" || s.Coverage.ProxyBytes.Status != "exact" || !s.Loss.Unknown {
		t.Fatal("later complete inventory erased an omitted historical connection")
	}
}

func TestCollectorAbsentSocketIsPartialNotProofOfClosedFlow(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		c, now := collectorFixture(t)
		flow := strings.Repeat("f", 32)
		c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{proxyEvent(1, *now, flow, "TcpUpstreamConnected", 3, 7, 10)}, EnvoyTotals{Sequence: 1})
		var inventoryErr error
		wantReason := "socket_absent"
		if truncated {
			inventoryErr, wantReason = &inventoryTruncated{omitted: 1}, "socket_observation_unavailable"
		}
		c.publish(KernelSample{Sequence: 1, BootAt: *now, EnforcerReady: true, Counters: &KernelCounters{}}, nil, nil, inventoryErr, nil, true)
		s := c.Snapshot()
		if len(s.Connections) != 1 || !s.Connections[0].Partial || s.Connections[0].Reason != wantReason || s.Connections[0].State == "closed" || s.Coverage.ProxyBytes.Status != "exact" {
			t.Fatal("inventory absence became complete live attribution or invented stream ending")
		}
	}
}
