package networkgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

func collectorFixture(t *testing.T) (*Collector, *BootInstant) {
	t.Helper()
	now := BootInstant(time.Hour)
	controller := testController(t, func(context.Context, string) error { return nil })
	controller.clock.read = func() (BootInstant, error) { return now, nil }
	r, err := NewResolver(controller.policy, nil, nil, controller.clock, func(context.Context, []byte) ([]byte, error) { return nil, Failure("unused_fixture") })
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGuard(controller.policy, controller.clock, r, ControllerClient{Identity: controller.identity, Clock: controller.clock}, NewGuardEvents(controller.clock))
	if err != nil {
		t.Fatal(err)
	}
	doh, err := NewDoH(netip.MustParseAddrPort("1.1.1.1:443"), "cloudflare-dns.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(doh.Close)
	c, err := NewCollector(g, NewEnvoyEvents(controller.clock), doh)
	if err != nil {
		t.Fatal(err)
	}
	return c, &now
}

func registration(sequence uint64, now BootInstant, flow string) GuardEvent {
	return GuardEvent{Sequence: sequence, BootAt: now, At: time.Unix(100, 0), Kind: "flow_registered", FlowID: flow, Name: "api.example.com", RuleID: "rule", Peer: netip.MustParseAddr("93.184.216.34"), Port: 443}
}
func proxyEvent(sequence uint64, now BootInstant, flow, phase string, sent, received, duration uint64) EnvoyEvent {
	return EnvoyEvent{Sequence: sequence, BootAt: now, At: time.Unix(100, 0), FlowID: flow, ConnectionID: "1", Phase: phase,
		Peer: netip.MustParseAddrPort("93.184.216.34:443"), Local: netip.MustParseAddrPort("172.17.0.2:32000"), Sent: &sent, Received: &received, DurationMillis: &duration}
}
func publishFixture(c *Collector, now BootInstant, rows []SocketRow) {
	publishOwnedFixture(c, now, rows, nil)
}
func publishOwnedFixture(c *Collector, now BootInstant, rows []SocketRow, owned []maintenanceSocket) {
	c.publish(KernelSample{Sequence: 1, BootAt: now, At: time.Unix(100, 0), EnforcerReady: true, Counters: &KernelCounters{}}, nil, rows, nil, owned, true)
}

func TestCollectorCumulativeReplayAndUnknownMeters(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("a", 32)
	start := proxyEvent(1, *now, flow, "TcpConnectionStart", 0, 0, 0)
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{start}, EnvoyTotals{Sequence: 1})
	if c.flows[flow].row.SentBytes != nil || c.connections != 0 {
		t.Fatal("start placeholder became measured connection/bytes")
	}
	connected := proxyEvent(2, *now, flow, "TcpUpstreamConnected", 0, 0, 5)
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{connected}, EnvoyTotals{Sequence: 2})
	*now = now.Add(time.Second)
	periodic := proxyEvent(3, *now, flow, "TcpPeriodic", 1<<53+1, 73, 1005)
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{periodic}, EnvoyTotals{Sequence: 3})
	if uint64(c.sent) != 1<<53+1 || c.received != 73 || c.connections != 1 {
		t.Fatal("lost cumulative precision")
	}
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{periodic}, EnvoyTotals{Sequence: 3})
	if uint64(c.sent) != 1<<53+1 {
		t.Fatal("replayed source event counted twice")
	}
	*now = now.Add(time.Second)
	end := proxyEvent(4, *now, flow, "TcpConnectionEnd", 1<<53+3, 80, 2005)
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{end}, EnvoyTotals{Sequence: 4})
	if uint64(c.sent) != 1<<53+3 || c.received != 80 || len(c.flows) != 0 || len(c.closed) != 1 {
		t.Fatal("end added cumulative bytes or retained live flow")
	}
	end.Sequence++
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{end}, EnvoyTotals{Sequence: 5})
	if uint64(c.sent) != 1<<53+3 || !c.proxyPartial {
		t.Fatal("evicted flow resurrected totals")
	}
}

func TestCollectorIncludesBrokerOnlyResolverFailures(t *testing.T) {
	c, now := collectorFixture(t)
	brokerResolver, err := NewResolver(c.resolvers[0].policy, nil, nil, c.clock,
		func(context.Context, []byte) ([]byte, error) { return nil, errors.New("broker dns failed") })
	if err != nil {
		t.Fatal(err)
	}
	c.resolvers = append(c.resolvers, brokerResolver)
	if _, err := brokerResolver.Resolve(context.Background(), "api.example.com"); err == nil {
		t.Fatal("broker resolver unexpectedly succeeded")
	}
	publishFixture(c, *now, nil)
	snapshot := c.Snapshot()
	if snapshot.Counters == nil || snapshot.Counters.MaintenanceQueries == nil || *snapshot.Counters.MaintenanceQueries != 1 ||
		snapshot.Counters.MaintenanceFailures == nil || *snapshot.Counters.MaintenanceFailures != 1 {
		t.Fatalf("broker resolver accounting = %#v", snapshot.Counters)
	}
	if snapshot.Health.Resolver.Status != "degraded" {
		t.Fatalf("broker resolver failure was reported as healthy: %#v", snapshot.Health.Resolver)
	}
}

func TestCollectorProducerRatesSuppressCatchUpAndExpireWithPresentSocket(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("b", 32)
	connected := proxyEvent(1, *now, flow, "TcpUpstreamConnected", 0, 0, 10)
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{connected}, EnvoyTotals{Sequence: 1})
	*now = now.Add(time.Second)
	periodic := proxyEvent(2, *now, flow, "TcpPeriodic", 100, 200, 1010)
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{periodic}, EnvoyTotals{Sequence: 2})
	if c.flows[flow].row.Rate == nil || c.flows[flow].row.Rate.SentPerSecond != 100 {
		t.Fatal("normal producer rate missing")
	}
	rows := []SocketRow{{Tuple: c.flows[flow].tuple, UID: 65532, Inode: 1, State: "open"}}
	*now = now.Add(4 * time.Second)
	publishFixture(c, *now, rows)
	if c.snapshot.Connections[0].Rate != nil || c.snapshot.Connections[0].State != "stale" {
		t.Fatal("inventory kept old rate fresh")
	}
	a, b := uint64(1000), uint64(2000)
	if producerRate(&a, &b, *now, now.Add(time.Millisecond), 100, 100) != nil {
		t.Fatal("buffered log batch fabricated upload spike")
	}
}

func TestCollectorOrphanRetirementAndSameSampleReplacementRemainBounded(t *testing.T) {
	c, now := collectorFixture(t)
	for i := 0; i < MaxGuardFlows; i++ {
		id := fmt.Sprintf("%032x", i)
		c.ingest([]GuardEvent{registration(uint64(i+1), *now, id)}, GuardTotals{Sequence: uint64(i + 1)}, nil, EnvoyTotals{})
	}
	old, newID := fmt.Sprintf("%032x", 0), fmt.Sprintf("%032x", MaxGuardFlows)
	c.ingest([]GuardEvent{registration(MaxGuardFlows+1, *now, newID)}, GuardTotals{Sequence: MaxGuardFlows + 1},
		[]EnvoyEvent{proxyEvent(1, *now, old, "TcpUpstreamConnected", 0, 0, 0), proxyEvent(2, *now, old, "TcpConnectionEnd", 5, 7, 1)}, EnvoyTotals{Sequence: 2})
	if c.flows[newID] == nil || len(c.flows) != MaxGuardFlows || c.detailLost != 0 {
		t.Fatal("batched replacement lost despite old ending in same drain")
	}
	c.ingest([]GuardEvent{{Sequence: MaxGuardFlows + 2, Kind: "private_flow_closed", FlowID: newID, BootAt: *now}}, GuardTotals{Sequence: MaxGuardFlows + 2}, nil, EnvoyTotals{Sequence: 2})
	*now = now.Add(4 * time.Second)
	publishFixture(c, *now, nil)
	if c.flows[newID] != nil || !c.proxyPartial {
		t.Fatal("missing proxy ending permanently occupied flow capacity")
	}
}

func TestCollectorUnknownSocketsAndKernelResetCannotClaimExactZero(t *testing.T) {
	c, now := collectorFixture(t)
	rows := []SocketRow{{UID: 65532, Inode: 1, State: "open", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:31000"), Peer: netip.MustParseAddrPort("1.1.1.1:443")}}}
	publishFixture(c, *now, rows)
	s := c.Snapshot()
	if len(s.Connections) != 1 || s.Connections[0].NameSource != "unattributed" || s.Connections[0].SentBytes != nil || *s.UnknownConnections != 1 || s.Coverage.BoundaryAttribution.Status != "lower-bound" || s.Coverage.ProxyBytes.Status != "exact" || s.Health.Collector.Status != "ready" {
		t.Fatalf("unknown socket mislabeled maintenance or complete zero: %#v", s)
	}
	k := KernelSample{Sequence: 2, BootAt: *now, Counters: &KernelCounters{DeniedAgent: 100}, EnforcerReady: true}
	c.publish(k, nil, nil, nil, nil, true)
	k.Sequence++
	k.Counters = &KernelCounters{DeniedAgent: 1}
	c.publish(k, nil, nil, nil, nil, true)
	if *c.snapshot.Counters.DeniedPackets != 100 || c.snapshot.Coverage.KernelPackets.Status != "lower-bound" {
		t.Fatal("kernel reset lowered cumulative total or remained exact")
	}
}

func TestCollectorConsumedCursorNotProducerWatermarkAndSnapshotDetached(t *testing.T) {
	c, now := collectorFixture(t)
	c.ingest([]GuardEvent{{Sequence: 1, Kind: "dns_denied", Name: "private.example.com", At: time.Unix(100, 0)}}, GuardTotals{Sequence: 2, DeniedDNS: 2}, nil, EnvoyTotals{})
	if c.guardCursor != 1 || c.guardTotals.Sequence != 2 {
		t.Fatal("producer watermark swallowed undrained evidence")
	}
	c.ingest([]GuardEvent{{Sequence: 2, Kind: "dns_denied", Name: "other.example.com", At: time.Unix(100, 0)}}, GuardTotals{Sequence: 2, DeniedDNS: 2}, nil, EnvoyTotals{})
	publishFixture(c, *now, nil)
	s := c.Snapshot()
	if len(s.Denials) != 2 || s.Denials[0].Port != nil || *s.Counters.DeniedDNSQueries != 2 {
		t.Fatal("DNS invented a port or double-counted cumulative denial totals")
	}
	s.Denials[0].Name = "changed"
	if c.Snapshot().Denials[0].Name == "changed" {
		t.Fatal("snapshot aliases collector")
	}
	data, err := json.Marshal(c.Snapshot().Project(false))
	if err != nil || strings.Contains(string(data), "private.example.com") {
		t.Fatal("private names leaked")
	}
}

func TestCollectorMissingMeterResetsRateBaseline(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("c", 32)
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{proxyEvent(1, *now, flow, "TcpUpstreamConnected", 0, 0, 0)}, EnvoyTotals{Sequence: 1})
	*now = now.Add(time.Second)
	e := proxyEvent(2, *now, flow, "TcpPeriodic", 10, 10, 1000)
	e.Sent = nil
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{e}, EnvoyTotals{Sequence: 2})
	*now = now.Add(time.Second)
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{proxyEvent(3, *now, flow, "TcpPeriodic", 20, 20, 2000)}, EnvoyTotals{Sequence: 3})
	if c.flows[flow].row.Rate != nil || c.sent != networkview.Count(20) {
		t.Fatal("missing meter invented a short-window rate or erased eventual cumulative count")
	}
}

func TestCollectorCloseAfterInventoryKeepsExactBoundedJoin(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("d", 32)
	c.ingest([]GuardEvent{registration(1, *now, flow)}, GuardTotals{Sequence: 1}, []EnvoyEvent{proxyEvent(1, *now, flow, "TcpUpstreamConnected", 0, 0, 0)}, EnvoyTotals{Sequence: 1})
	f := c.flows[flow]
	f.inode = 42
	rows := []SocketRow{{Tuple: f.tuple, UID: 65532, Inode: 42, State: "open"}}
	c.ingest(nil, GuardTotals{Sequence: 1}, []EnvoyEvent{proxyEvent(2, *now, flow, "TcpConnectionEnd", 100, 100, 10)}, EnvoyTotals{Sequence: 2})
	publishFixture(c, *now, rows)
	if c.proxyPartial || *c.snapshot.UnknownConnections != 0 || *c.snapshot.LiveConnections != 0 {
		t.Fatal("older inventory resurrected authoritatively closed socket")
	}
	rows[0].Inode = 43
	publishFixture(c, *now, rows)
	if *c.snapshot.UnknownConnections != 1 {
		t.Fatal("close tombstone hid a reused socket inode")
	}
}

func TestCollectorUnavailableEnforcerCannotReportOverallAvailable(t *testing.T) {
	c, now := collectorFixture(t)
	c.publish(KernelSample{Sequence: 1, BootAt: *now, Counters: &KernelCounters{}}, nil, nil, nil, nil, true)
	if c.snapshot.Availability != "degraded" || c.snapshot.Health.Enforcer.Status != "unavailable" {
		t.Fatal("failed enforcer reported available network")
	}
}

func TestBlockedDirectSocketIsSampledPolicyEvidenceNotExternalTraffic(t *testing.T) {
	c, now := collectorFixture(t)
	row := SocketRow{UID: 1000, Inode: 42, State: "connecting", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:32000"), Peer: netip.MustParseAddrPort("169.254.169.254:80")}}
	publishFixture(c, *now, []SocketRow{row})
	s := c.Snapshot()
	if len(s.Denials) != 1 || *s.LiveConnections != 0 || *s.UnknownConnections != 0 || s.Coverage.ProxyBytes.Status != "exact" || *s.Counters.DeniedPackets != 0 {
		t.Fatal("sampled blocked socket invented external traffic or packet counter")
	}
	d := s.Denials[0]
	if d.Kind != "direct_tcp_attempt" || d.Source != "socket-inventory" || d.Basis != "observed-socket-and-captured-policy" || d.Peer != row.Tuple.Peer.String() || d.Candidate != nil {
		t.Fatalf("wrong sampled attempt evidence: %#v", d)
	}
	publishFixture(c, *now, []SocketRow{row})
	if len(c.denials) != 1 {
		t.Fatal("persistent socket spammed denial details")
	}
	private, _ := json.Marshal(c.Snapshot().Project(false))
	if strings.Contains(string(private), row.Tuple.Peer.String()) {
		t.Fatal("socket denial peer leaked through projection")
	}
	publishFixture(c, *now, nil)
	publishFixture(c, *now, []SocketRow{row})
	if len(c.denials) != 2 || c.denials[0].ID == c.denials[1].ID {
		t.Fatal("new observed lifetime reused old event identity")
	}
}

func TestUnknownOpenAgentSocketOrUnavailableEnforcerIsNotCalledBlocked(t *testing.T) {
	for _, scenario := range []string{"open", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			c, now := collectorFixture(t)
			row := SocketRow{UID: 1000, Inode: 1, State: "connecting", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:32000"), Peer: netip.MustParseAddrPort("169.254.169.254:80")}}
			k := KernelSample{Sequence: 1, BootAt: *now, EnforcerReady: true, Counters: &KernelCounters{}}
			if scenario == "open" {
				row.State = "open"
			} else {
				k.EnforcerReady = false
			}
			c.publish(k, nil, []SocketRow{row}, nil, nil, true)
			if len(c.denials) != 0 || *c.snapshot.UnknownConnections != 1 || c.snapshot.Coverage.BoundaryAttribution.Status != "lower-bound" || c.snapshot.Coverage.ProxyBytes.Status != "exact" || !c.snapshot.Loss.Unknown {
				t.Fatal("unexpected/unverified socket silently called blocked")
			}
			if scenario == "unavailable" && (c.snapshot.Health.Enforcer.Reason != "enforcement_unavailable" ||
				slices.Contains(c.snapshot.Loss.Reasons, "unexpected_agent_connection") || !slices.Contains(c.snapshot.Loss.Reasons, "agent_attempt_unverified")) {
				t.Fatal("connecting attempt invented establishment or hid enforcer failure")
			}
			if scenario == "open" && !slices.Contains(c.snapshot.Loss.Reasons, "unexpected_agent_connection") {
				t.Fatal("observed open agent socket lost its security reason")
			}
		})
	}
}

func TestSampledSocketEvidencePreservesDedupeAcrossInventoryFailure(t *testing.T) {
	c, now := collectorFixture(t)
	row := SocketRow{UID: 1000, Inode: 42, State: "connecting", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:32000"), Peer: netip.MustParseAddrPort("169.254.169.254:80")}}
	publishFixture(c, *now, []SocketRow{row})
	c.publish(KernelSample{Sequence: 1, BootAt: *now, Counters: &KernelCounters{}, EnforcerReady: true}, nil, nil, Failure("socket_inventory_unavailable"), nil, true)
	publishFixture(c, *now, []SocketRow{row})
	if len(c.denials) != 1 || !c.inventoryGap {
		t.Fatal("failed inventory erased identity or gap evidence")
	}
}

func TestStaleEnforcerCannotClassifyDirectSocketAsBlocked(t *testing.T) {
	c, now := collectorFixture(t)
	row := SocketRow{UID: 1000, Inode: 42, State: "connecting", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:32000"), Peer: netip.MustParseAddrPort("169.254.169.254:80")}}
	c.publish(KernelSample{Sequence: 1, BootAt: now.Add(-4 * time.Second), Counters: &KernelCounters{}, EnforcerReady: true}, nil, []SocketRow{row}, nil, nil, true)
	if len(c.denials) != 0 || *c.snapshot.UnknownConnections != 1 {
		t.Fatal("stale enforcement produced a blocked-socket assertion")
	}
}

func TestSocketDenialStormBoundsDetailWithoutInventingPacketTotals(t *testing.T) {
	c, now := collectorFixture(t)
	var rows []SocketRow
	for i := 0; i < MaxDenialDetails+20; i++ {
		rows = append(rows, SocketRow{UID: 1000, Inode: uint64(i + 1), State: "connecting", Tuple: SocketTuple{Local: netip.AddrPortFrom(netip.MustParseAddr("172.17.0.2"), uint16(32000+i)), Peer: netip.MustParseAddrPort("169.254.169.254:80")}})
	}
	publishFixture(c, *now, rows)
	if len(c.denials) != MaxDenialDetails || c.detailLost != 20 || *c.snapshot.Counters.DeniedPackets != 0 || *c.snapshot.LiveConnections != 0 {
		t.Fatal("socket storm broke retention or counter units")
	}
	ids := make(map[string]bool)
	for _, d := range c.denials {
		if ids[d.ID] {
			t.Fatal("same-sample evidence reused ID")
		}
		ids[d.ID] = true
	}
}
