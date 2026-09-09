package networkgateway

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

func historicalSocket() SocketRow {
	return SocketRow{UID: 65532, Inode: 42, State: "open", Tuple: SocketTuple{
		Local: netip.MustParseAddrPort("172.17.0.2:49153"), Peer: netip.MustParseAddrPort("1.1.1.1:443")}}
}

func TestCollectorExpiredSocketHistoryIsImmutableAndNotALiveCount(t *testing.T) {
	c, now := collectorFixture(t)
	row := historicalSocket()
	publishFixture(c, *now, []SocketRow{row})
	*now = now.Add(ObservationStaleAfter)
	publishFixture(c, *now, []SocketRow{row})
	if len(c.closed) != 1 || c.closed[0].NameSource != "unattributed-history" || c.closed[0].Reason != "socket_join_expired" {
		t.Fatal("expiry discarded the historical socket identity")
	}
	history := c.closed[0]
	for range MaxClosedDetails + 1 {
		*now = now.Add(ObservationStaleAfter)
		publishFixture(c, *now, []SocketRow{row})
		if len(c.snapshot.Connections) != 1 || *c.snapshot.LiveConnections != 1 || *c.snapshot.UnknownConnections != 1 || c.snapshot.PendingConnections != 0 {
			t.Fatal("persistent socket duplicated history or reentered the pending queue")
		}
	}
	if len(c.closed) != 1 || c.closed[0] != history || c.detailLost != 0 {
		t.Fatal("resampling rewrote history or fabricated eviction")
	}
	publishFixture(c, *now, nil)
	s := c.Snapshot()
	if len(s.Connections) != 1 || s.Connections[0] != history || *s.LiveConnections != 0 || *s.UnknownConnections != 0 || !s.Loss.Unknown || s.Counters == nil || *s.Counters.Connections != 0 {
		t.Fatal("disappearance erased history or turned it into a live/metered connection")
	}
	if history.State != "unknown" || !history.Partial || history.StartedAt != nil || history.SentBytes != nil || history.ReceivedBytes != nil || history.Rate != nil {
		t.Fatal("historical unknown invented a lifetime or meters")
	}
	redacted, err := json.Marshal(s.Project(false))
	if err != nil || strings.Contains(string(redacted), row.Tuple.Peer.String()) || !strings.Contains(string(redacted), "socket_join_expired") {
		t.Fatal("projection leaked peer or hid historical loss")
	}
}

func TestCollectorSecurityReasonsSurviveEarlierSamplingGapAndHealthyInventory(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		uid   uint32
		inode uint64
	}{
		{"unexpected_agent_connection", 1000, 7},
		{"unexpected_socket_owner", 1234, 7},
		{"socket_inode_unavailable", 65532, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, now := collectorFixture(t)
			row := historicalSocket()
			publishFixture(c, *now, []SocketRow{row})
			*now = now.Add(ObservationStaleAfter)
			publishFixture(c, *now, nil)
			row.UID, row.Inode = scenario.uid, scenario.inode
			publishFixture(c, *now, []SocketRow{row})
			*now = now.Add(time.Second)
			c.terminal, c.envoyTotals.Stopped = true, true
			publishFixture(c, *now, nil)
			if !slices.Contains(c.snapshot.Loss.Reasons, "unattributed_socket") || !slices.Contains(c.snapshot.Loss.Reasons, scenario.name) || !c.snapshot.Loss.Unknown {
				t.Fatal("earlier attribution gap or healthy final inventory hid a security-relevant observation")
			}
			if !slices.ContainsFunc(c.snapshot.Connections, func(r networkview.Connection) bool {
				return r.NameSource == "unattributed-history" && r.Reason == scenario.name && r.Peer == row.Tuple.Peer.String()
			}) {
				t.Fatal("security-relevant socket disappeared without diagnostic evidence")
			}
		})
	}
}

func TestCollectorLateOwnershipCannotEraseRetainedExpiry(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("d", 32)
	event := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 3, 7, 10)
	row := SocketRow{UID: 65532, Inode: 42, State: "open", Tuple: SocketTuple{Local: event.Local, Peer: event.Peer}}
	publishFixture(c, *now, []SocketRow{row})
	*now = now.Add(ObservationStaleAfter)
	publishFixture(c, *now, []SocketRow{row})
	history := c.closed[0]
	event.BootAt = *now
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{event}, EnvoyTotals{Sequence: 1})
	publishFixture(c, *now, []SocketRow{row})
	if len(c.pending) != 0 || c.closed[0] != history || !c.snapshot.Loss.Unknown || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("late join erased earlier uncertainty or corrupted valid proxy meters")
	}
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{proxyEvent(2, *now, flow, "TcpConnectionEnd", 5, 9, 20)}, EnvoyTotals{Sequence: 2, Stopped: true})
	c.terminal = true
	publishFixture(c, *now, nil)
	if len(c.snapshot.Connections) != 2 || c.closed[0] != history || *c.snapshot.LiveConnections != 0 || *c.snapshot.Counters.SentBytes != 5 || !c.snapshot.Loss.Unknown {
		t.Fatal("terminal closure changed historical attribution or counted its unknown bytes")
	}
}

func TestCollectorUnknownHistoryBoundReportsRealEvictions(t *testing.T) {
	c, now := collectorFixture(t)
	for i := range MaxClosedDetails + 1 {
		row := historicalSocket()
		row.Inode = uint64(i + 1)
		publishFixture(c, *now, []SocketRow{row})
		*now = now.Add(ObservationStaleAfter)
		publishFixture(c, *now, nil)
	}
	if len(c.closed) != MaxClosedDetails || len(c.pending) != 0 || c.detailLost != 1 || !c.snapshot.Loss.DetailTruncated ||
		c.snapshot.Loss.OmittedDetails == nil || *c.snapshot.Loss.OmittedDetails != 1 || *c.snapshot.Counters.Connections != 0 || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("bounded history invented unique flows, lost eviction reporting or changed meters")
	}
	for range 3 {
		*now = now.Add(ObservationStaleAfter)
		publishFixture(c, *now, nil)
	}
	if c.detailLost != 1 {
		t.Fatal("idle sampling manufactured history eviction")
	}
}

func TestCollectorHistoryDedupeSurvivesIncompleteInventory(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			c, now := collectorFixture(t)
			rows := make([]SocketRow, MaxClosedDetails+1)
			for i := range rows {
				rows[i] = historicalSocket()
				rows[i].UID, rows[i].Inode = 1234, uint64(i+1)
			}
			publishFixture(c, *now, rows)
			if c.detailLost != 1 {
				t.Fatal("history fixture did not exceed capacity")
			}
			var err error = Failure("socket_inventory_unavailable")
			if truncated {
				err = &inventoryTruncated{omitted: uint64(len(rows))}
			}
			c.publish(KernelSample{Sequence: 1, BootAt: *now, EnforcerReady: true, Counters: &KernelCounters{}}, nil, nil, err, nil, true)
			publishFixture(c, *now, rows)
			if c.detailLost != 1 {
				t.Fatalf("incomplete inventory manufactured history evictions: %d", c.detailLost)
			}

			row := historicalSocket()
			publishFixture(c, *now, []SocketRow{row})
			*now = now.Add(ObservationStaleAfter)
			publishFixture(c, *now, []SocketRow{row})
			history := slices.Clone(c.closed)
			c.publish(KernelSample{Sequence: 1, BootAt: *now, EnforcerReady: true, Counters: &KernelCounters{}}, nil, nil, err, nil, true)
			publishFixture(c, *now, []SocketRow{row})
			if c.snapshot.PendingConnections != 0 || !slices.Equal(c.closed, history) {
				t.Fatal("unproven absence reset expired identity or rewrote history")
			}
		})
	}
}

func TestCollectorIncompleteHistoryIdentityBoundDoesNotInventAnOmissionCount(t *testing.T) {
	c, now := collectorFixture(t)
	c.previousUnknown = make(map[socketAttemptKey]struct{})
	row := historicalSocket()
	row.UID = 1234
	for i := range MaxSocketInventory {
		c.previousUnknown[socketAttemptKey{Tuple: row.Tuple, UID: row.UID, Inode: uint64(i + 1)}] = struct{}{}
	}
	row.Inode = MaxSocketInventory + 1
	kernel := KernelSample{Sequence: 1, BootAt: *now, EnforcerReady: true, Counters: &KernelCounters{}}
	for range 3 {
		c.publish(kernel, nil, []SocketRow{row}, &inventoryTruncated{omitted: 1}, nil, true)
	}
	if len(c.previousUnknown) != MaxSocketInventory || len(c.closed) != 0 || c.detailLost != 0 ||
		!c.snapshot.Loss.DetailTruncated || c.snapshot.Loss.OmittedDetails != nil || !slices.Contains(c.snapshot.Loss.Reasons, "socket_history_identity_capacity") || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("unproven absence grew identity state, invented evictions or corrupted independent meters")
	}
	publishFixture(c, *now, []SocketRow{row})
	if len(c.previousUnknown) != 1 || len(c.closed) != 1 || !c.snapshot.Loss.Unknown || c.snapshot.Loss.OmittedDetails != nil {
		t.Fatal("complete inventory failed to retire old identities or erased historical loss")
	}
}

func TestCollectorCurrentDetailLimitCannotBeBypassedThroughHistory(t *testing.T) {
	c, now := collectorFixture(t)
	rows := make([]SocketRow, MaxUnknownDetails+1)
	for i := range rows {
		rows[i] = historicalSocket()
		rows[i].UID, rows[i].Inode = 1234, uint64(i+1)
	}
	for range 3 {
		publishFixture(c, *now, rows)
		if len(c.snapshot.Connections) != MaxUnknownDetails || c.detailLost != 1 || *c.snapshot.UnknownConnections != MaxUnknownDetails+1 {
			t.Fatal("live detail escaped its bound through the historical ring")
		}
		if slices.ContainsFunc(c.snapshot.Connections, func(r networkview.Connection) bool { return r.NameSource == "unattributed-history" }) {
			t.Fatal("present socket published as history because its current row was omitted")
		}
	}
	publishFixture(c, *now, nil)
	if len(c.snapshot.Connections) != MaxClosedDetails || *c.snapshot.UnknownConnections != 0 || c.detailLost != 1 {
		t.Fatal("disappearance erased retained history or changed its eviction count")
	}
}

func TestCollectorFirstHistoryDoesNotHideLaterSameSocketEscalation(t *testing.T) {
	c, now := collectorFixture(t)
	row := historicalSocket()
	row.UID, row.State = 1000, "connecting"
	kernel := KernelSample{Sequence: 1, BootAt: *now, Counters: &KernelCounters{}}
	c.publish(kernel, nil, []SocketRow{row}, nil, nil, true)
	history := c.closed[0]
	row.State = "open"
	c.publish(kernel, nil, []SocketRow{row}, nil, nil, true)
	if len(c.snapshot.Connections) != 1 || c.snapshot.Connections[0].Reason != "unexpected_agent_connection" || c.snapshot.Health.Enforcer.Reason != "unexpected_agent_connection" {
		t.Fatal("first history masked the current escalation")
	}
	publishFixture(c, *now, nil)
	if len(c.closed) != 1 || c.closed[0] != history || !slices.Contains(c.snapshot.Loss.Reasons, "agent_attempt_unverified") || !slices.Contains(c.snapshot.Loss.Reasons, "unexpected_agent_connection") {
		t.Fatal("later healthy sample erased escalation or rewrote the immutable first example")
	}
}

func TestCollectorClockGapDoesNotInventElapsedExpiryOrDisappearOnRecovery(t *testing.T) {
	c, now := collectorFixture(t)
	row := historicalSocket()
	*now = 0
	publishFixture(c, *now, []SocketRow{row})
	if len(c.closed) != 1 || c.closed[0].Reason != "socket_join_clock_unavailable" || c.closed[0].StartedAt != nil || !c.snapshot.Loss.Unknown {
		t.Fatal("clock loss invented a measured timeout or complete history")
	}
	*now = BootInstant(2 * time.Hour)
	publishFixture(c, *now, nil)
	if len(c.closed) != 1 || c.closed[0].Reason != "socket_join_clock_unavailable" || !c.snapshot.Loss.Unknown || c.snapshot.Coverage.ProxyBytes.Status != "exact" {
		t.Fatal("recovered clock erased historical uncertainty or damaged independent meters")
	}
}
