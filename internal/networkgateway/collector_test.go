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

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

func collectorFixture(t *testing.T) (*Collector, *BootInstant) {
	t.Helper()
	return collectorFixtureWithPolicy(t, testPolicy(t))
}

func collectorFixtureWithPolicy(t *testing.T, policy egress.Snapshot) (*Collector, *BootInstant) {
	t.Helper()
	now := BootInstant(time.Hour)
	controller := newTestController(t, policy, func(context.Context, string) error { return nil })
	controller.clock.read = func() (BootInstant, error) { return now, nil }
	r, err := NewResolver(controller.policy, nil, nil, controller.clock, func(context.Context, []byte) ([]byte, error) { return nil, Failure("unused_fixture") })
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGuard(controller.agentUID, controller.policy, controller.clock, r, ControllerClient{Identity: controller.identity, Clock: controller.clock}, NewGuardEvents(controller.clock))
	if err != nil {
		t.Fatal(err)
	}
	doh, err := NewDoH([]netip.AddrPort{netip.MustParseAddrPort("1.1.1.1:443")}, "cloudflare-dns.com", nil, controller.clock)
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

func TestCollectorRetainsServiceTrafficSource(t *testing.T) {
	c, now := collectorFixture(t)
	flow := strings.Repeat("f", 32)
	allowed := registration(1, *now, flow)
	allowed.Service = "web"
	denied := GuardEvent{Sequence: 2, BootAt: *now, At: time.Unix(101, 0), Kind: "tls_denied", Name: "blocked.example.com", Service: "worker", Port: 443, Reason: "unapproved_name"}
	c.ingest([]GuardEvent{allowed, denied}, GuardTotals{Sequence: 2}, []EnvoyEvent{proxyEvent(1, *now, flow, "TcpUpstreamConnected", 0, 0, 1)}, EnvoyTotals{Sequence: 1})

	if got := c.flows[flow].row.Service; got != "web" {
		t.Fatalf("allowed service = %q", got)
	}
	if len(c.denials) != 1 || c.denials[0].Service != "worker" {
		t.Fatalf("denied service = %#v", c.denials)
	}
}

func proxyEvent(sequence uint64, now BootInstant, flow, phase string, sent, received, duration uint64) EnvoyEvent {
	return EnvoyEvent{Sequence: sequence, BootAt: now, At: time.Unix(100, 0), FlowID: flow, ConnectionID: "1", Phase: phase,
		Peer: netip.MustParseAddrPort("93.184.216.34:443"), Local: netip.MustParseAddrPort("172.17.0.2:32000"), Sent: &sent, Received: &received, DurationMillis: &duration}
}
func publishFixture(c *Collector, now BootInstant, rows []SocketRow) {
	publishOwnedFixture(c, now, rows, nil)
}
func publishOwnedFixture(c *Collector, now BootInstant, rows []SocketRow, owned []maintenanceSocket) {
	c.publish(KernelSample{Sequence: 1, BootAt: now, At: time.Unix(100, 0), EnforcerReady: true, Counters: &KernelCounters{}}, nil, rows, nil, owned, nil, true)
}

// sampleReleasedFixture publishes rows the way Sample does: the registry's owned connections, then
// every release since the last sample.
func sampleReleasedFixture(c *Collector, now BootInstant, rows []SocketRow) {
	c.publish(KernelSample{Sequence: 1, BootAt: now, At: time.Unix(100, 0), EnforcerReady: true, Counters: &KernelCounters{}}, nil, rows, nil,
		c.doh.sockets.snapshot(), c.doh.sockets.drainReleased(), true)
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
	c.publish(k, nil, nil, nil, nil, nil, true)
	k.Sequence++
	k.Counters = &KernelCounters{DeniedAgent: 1}
	c.publish(k, nil, nil, nil, nil, nil, true)
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
	c.publish(KernelSample{Sequence: 1, BootAt: *now, Counters: &KernelCounters{}}, nil, nil, nil, nil, nil, true)
	if c.snapshot.Availability != "degraded" || c.snapshot.Health.Enforcer.Status != "unavailable" {
		t.Fatal("failed enforcer reported available network")
	}
}

// ipv6Attempt is an agent socket caught connecting to an IPv6 address. The filter records every IPv4
// connection it refuses itself, so a socket sample is the record only for the rest.
func ipv6Attempt(inode uint64, local uint16) SocketRow {
	return SocketRow{UID: 1000, Inode: inode, State: "connecting", Tuple: SocketTuple{Local: netip.AddrPortFrom(netip.MustParseAddr("fd00::2"), local), Peer: netip.MustParseAddrPort("[2001:db8::7]:80")}}
}

func TestBlockedDirectSocketIsSampledPolicyEvidenceNotExternalTraffic(t *testing.T) {
	c, now := collectorFixture(t)
	row := ipv6Attempt(42, 32000)
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

// refusedSample is the controller's sample after the filter refused these agent connections.
func refusedSample(sequence networkview.Count, now BootInstant, refused ...RefusedAttempt) KernelSample {
	return KernelSample{Sequence: sequence, BootAt: now, At: time.Unix(100, 0), EnforcerReady: true,
		Counters: &KernelCounters{DeniedAgent: networkview.Count(len(refused)), Refused: refused}}
}

// A refused connection fails in milliseconds, so no socket sample sees it connecting: the filter's own
// record of the refusal is the evidence. `curl http://example.com` and `curl https://example.com:8443`,
// with only TLS on 443 approved, are both found by the name the run resolved, with the address and
// port; a protected address keeps its own reason.
func TestCollectorRecordsRefusalsTooFastForASocketSample(t *testing.T) {
	c, now := collectorFixture(t)
	c.resolver.cache["example.com"] = cacheEntry{resolution: Resolution{Name: "example.com", Expires: now.Add(time.Minute),
		Addresses: []netip.Addr{netip.MustParseAddr("104.20.23.154")}}}
	plain := RefusedAttempt{Peer: netip.MustParseAddrPort("104.20.23.154:80"), SourcePort: 40001}
	unlisted := RefusedAttempt{Peer: netip.MustParseAddrPort("104.20.23.154:8443"), SourcePort: 40002}
	metadata := RefusedAttempt{Peer: netip.MustParseAddrPort("169.254.169.254:80"), SourcePort: 40003}
	sequence := networkview.Count(0)
	read := func(rows []SocketRow, refused ...RefusedAttempt) []networkview.Denial {
		t.Helper()
		sequence++
		c.publish(refusedSample(sequence, *now, refused...), nil, rows, nil, nil, nil, true)
		return c.Snapshot().Denials
	}
	denials := read(nil, plain, unlisted, metadata)
	if len(denials) != 3 {
		t.Fatalf("recorded %d refusals, want 3: %#v", len(denials), denials)
	}
	for i, want := range []struct {
		peer         string
		port         int
		name, reason string
	}{
		{"104.20.23.154:80", 80, "example.com", "fixed_egress_policy"},
		{"104.20.23.154:8443", 8443, "example.com", "fixed_egress_policy"},
		{"169.254.169.254:80", 80, "", "protected_destination"},
	} {
		d := denials[i]
		if d.Source != "kernel" || d.Kind != "direct_tcp_attempt" || d.Basis != "observed-socket-and-captured-policy" || d.Sequence != 1 || d.Peer != want.peer ||
			d.Port == nil || *d.Port != want.port || d.Name != want.name || d.Reason != want.reason || d.Candidate != nil || d.At.IsZero() {
			t.Errorf("refusal %d = %#v; want %s named %q for %s", i, d, want.peer, want.name, want.reason)
		}
	}
	// An entry stays in the set for its lifetime, so a later read that still lists it is the same
	// attempt; another source port to the same place is another attempt, and so is an entry that
	// expired and came back.
	if got := read(nil, plain, unlisted, metadata); len(got) != 3 {
		t.Fatalf("reading the same set again recorded %d refusals, want 3", len(got))
	}
	if got := read(nil, plain, RefusedAttempt{Peer: plain.Peer, SourcePort: 40004}); len(got) != 4 {
		t.Fatalf("a second attempt at one destination recorded %d refusals, want 4", len(got))
	}
	read(nil)
	if got := read(nil, plain); len(got) != 5 || got[4].ID == got[0].ID {
		t.Fatalf("a refusal that expired and came back = %d refusals (want 5) with a fresh identity", len(got))
	}
	// A blocking connect can still be caught connecting: the kernel's record is the one kept.
	caught := SocketRow{UID: 1000, Inode: 77, State: "connecting", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:40005"), Peer: netip.MustParseAddrPort("104.20.23.154:9443")}}
	got := read([]SocketRow{caught}, plain, RefusedAttempt{Peer: caught.Tuple.Peer, SourcePort: 40005})
	if len(got) != 6 || got[5].Source != "kernel" || got[5].Peer != "104.20.23.154:9443" {
		t.Fatalf("a refused connection caught connecting was recorded %d times: %#v", len(got)-5, got[5:])
	}
	s := c.Snapshot()
	if *s.LiveConnections != 0 || *s.UnknownConnections != 0 || s.Loss.DetailTruncated || s.Loss.Unknown || len(s.Loss.Reasons) != 0 {
		t.Fatalf("recorded refusals became traffic or loss: live=%d unknown=%d loss=%#v", *s.LiveConnections, *s.UnknownConnections, s.Loss)
	}
	if private, _ := json.Marshal(c.Snapshot().Project(false)); strings.Contains(string(private), "104.20.23.154") || strings.Contains(string(private), "example.com") {
		t.Fatal("a recorded refusal leaked its destination through a projection that withholds destinations")
	}
	// A sample older than the last one read lists an older set: it cannot retire newer entries.
	c.publish(refusedSample(sequence-1, *now), nil, nil, nil, nil, nil, true)
	if got := read(nil, plain); len(got) != 6 {
		t.Fatalf("an out-of-order sample re-recorded a refusal: %d", len(got))
	}
}

// A full set records nothing new until entries expire, and the filter still refuses, so the run
// says some blocked attempts may be missing — never how many, and never as a proxy gap.
func TestAFullRefusedSetSaysBlockedAttemptsMayBeMissing(t *testing.T) {
	c, now := collectorFixture(t)
	full := make([]RefusedAttempt, MaxRefusedAttempts)
	for i := range full {
		full[i] = RefusedAttempt{Peer: netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), uint16(1000+i)), SourcePort: 40000}
	}
	c.publish(refusedSample(1, *now, full[:MaxRefusedAttempts-1]...), nil, nil, nil, nil, nil, true)
	if loss := c.Snapshot().Loss; loss.DetailTruncated || loss.Unknown || len(loss.Reasons) != 0 {
		t.Fatalf("a set with room left claimed loss: %#v", loss)
	}
	for sequence, refused := range [][]RefusedAttempt{full, nil} {
		c.publish(refusedSample(networkview.Count(sequence+2), *now, refused...), nil, nil, nil, nil, nil, true)
		loss := c.Snapshot().Loss
		if !loss.DetailTruncated || !loss.Unknown || loss.OmittedDetails != nil || !slices.Equal(loss.Reasons, []string{"refused_attempt_capacity"}) {
			t.Fatalf("a full refused set (read %d) = %#v; want truncated, unknown, uncounted and named", sequence+2, loss)
		}
	}
	if len(c.denials) != MaxRefusedAttempts {
		t.Fatalf("recorded %d refusals from a full set, want %d", len(c.denials), MaxRefusedAttempts)
	}
}

// A direct connection to an address this run's resolver answered for one name is that name on a
// port or transport no rule allows — `curl http://example.com` where only TLS is approved — so the
// refusal carries the name a person asks `coop net blocked` about. An address two names share, or
// one the resolver never answered, stays an address: the record cannot say which name was meant.
func TestBlockedDirectSocketNamesTheHostTheRunResolved(t *testing.T) {
	c, now := collectorFixture(t)
	answer := func(name string, failed bool, addresses ...string) {
		resolution := Resolution{Name: name, Expires: now.Add(time.Minute)}
		for _, address := range addresses {
			resolution.Addresses = append(resolution.Addresses, netip.MustParseAddr(address))
		}
		entry := cacheEntry{resolution: resolution}
		if failed {
			entry.err = Failure("dns_answer_invalid")
		}
		c.resolver.cache[name] = entry
	}
	answer("example.com", false, "104.20.22.154", "104.20.23.154")
	answer("shared-a.example.com", false, "198.51.100.7")
	answer("shared-b.example.com", false, "198.51.100.7")
	answer("broken.example.com", true, "203.0.113.9")
	sequence := networkview.Count(0)
	attempt := func(peer string) networkview.Denial {
		t.Helper()
		sequence++
		c.publish(refusedSample(sequence, *now, RefusedAttempt{Peer: netip.MustParseAddrPort(peer), SourcePort: uint16(40000 + sequence)}), nil, nil, nil, nil, nil, true)
		denials := c.Snapshot().Denials
		return denials[len(denials)-1]
	}
	named := attempt("104.20.23.154:80")
	if named.Name != "example.com" || named.Peer != "104.20.23.154:80" || named.Port == nil || *named.Port != 80 || named.Candidate != nil {
		t.Fatalf("an attempt at an address the run resolved = %#v; want example.com, its observed peer and no draft", named)
	}
	for _, peer := range []string{"198.51.100.7:80", "203.0.113.9:80", "192.0.2.1:80"} {
		if got := attempt(peer); got.Name != "" {
			t.Errorf("%s was attributed to %q", peer, got.Name)
		}
	}
	if private, _ := json.Marshal(c.Snapshot().Project(false)); strings.Contains(string(private), "example.com") {
		t.Fatal("an attributed name leaked through a projection that withholds destinations")
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
			c.publish(k, nil, []SocketRow{row}, nil, nil, nil, true)
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
	row := ipv6Attempt(42, 32000)
	publishFixture(c, *now, []SocketRow{row})
	c.publish(KernelSample{Sequence: 1, BootAt: *now, Counters: &KernelCounters{}, EnforcerReady: true}, nil, nil, Failure("socket_inventory_unavailable"), nil, nil, true)
	publishFixture(c, *now, []SocketRow{row})
	if len(c.denials) != 1 || !c.inventoryGap {
		t.Fatal("failed inventory erased identity or gap evidence")
	}
}

func TestStaleEnforcerCannotClassifyDirectSocketAsBlocked(t *testing.T) {
	c, now := collectorFixture(t)
	row := SocketRow{UID: 1000, Inode: 42, State: "connecting", Tuple: SocketTuple{Local: netip.MustParseAddrPort("172.17.0.2:32000"), Peer: netip.MustParseAddrPort("169.254.169.254:80")}}
	c.publish(KernelSample{Sequence: 1, BootAt: now.Add(-4 * time.Second), Counters: &KernelCounters{}, EnforcerReady: true}, nil, []SocketRow{row}, nil, nil, nil, true)
	if len(c.denials) != 0 || *c.snapshot.UnknownConnections != 1 {
		t.Fatal("stale enforcement produced a blocked-socket assertion")
	}
}

func TestSocketDenialStormBoundsDetailWithoutInventingPacketTotals(t *testing.T) {
	c, now := collectorFixture(t)
	var rows []SocketRow
	for i := 0; i < MaxDenialDetails+20; i++ {
		rows = append(rows, ipv6Attempt(uint64(i+1), uint16(32000+i)))
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

func TestCollectorGrantCounterRegressionsRemainLowerBounds(t *testing.T) {
	policy := transportPolicy(t, egress.Rule{To: egress.Destination{CIDR: "10.1.0.0/16"}, Protocol: "tcp", Ports: []int{5432}}, egress.Rule{To: egress.Destination{CIDR: "10.2.0.0/16"}, Protocol: "udp", Ports: []int{5353}})
	a, b := counterFor(t, policy, "tcp", "10.1.0.0/16"), counterFor(t, policy, "udp", "10.2.0.0/16")
	ruleIDs := map[string]string{}
	for _, grant := range policy.Grants {
		ruleIDs[grant.CounterName()] = grant.ID
	}
	for _, test := range []struct {
		name    string
		grants  map[string]GrantCount
		want    GrantCount
		partial bool
	}{
		{"increase and new", map[string]GrantCount{a: {Packets: 11, Bytes: 110}, b: {Packets: 1, Bytes: 2}}, GrantCount{Packets: 11, Bytes: 110}, false},
		{"packets reset", map[string]GrantCount{a: {Packets: 9, Bytes: 120}}, GrantCount{Packets: 10, Bytes: 120}, true},
		{"bytes reset", map[string]GrantCount{a: {Packets: 11, Bytes: 90}}, GrantCount{Packets: 11, Bytes: 100}, true},
		{"missing", nil, GrantCount{Packets: 10, Bytes: 100}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, now := collectorFixtureWithPolicy(t, policy)
			publish := func(sequence networkview.Count, grants map[string]GrantCount) {
				c.publish(KernelSample{Sequence: sequence, BootAt: *now, At: time.Unix(100, 0), EnforcerReady: true, Counters: &KernelCounters{Grants: grants}}, nil, nil, nil, nil, nil, true)
			}
			publish(1, map[string]GrantCount{a: {Packets: 10, Bytes: 100}})
			publish(2, test.grants)
			snap := c.Snapshot()
			rows := map[string]GrantCount{}
			for _, row := range snap.AddressGrants {
				rows[row.RuleID] = GrantCount{Packets: row.Packets, Bytes: row.Bytes}
			}
			wantStatus := "exact"
			if test.partial {
				wantStatus = "lower-bound"
			}
			if rows[ruleIDs[a]] != test.want || snap.Coverage.KernelPackets.Status != wantStatus {
				t.Fatalf("retained counters: %+v coverage=%+v", rows, snap.Coverage.KernelPackets)
			}
			if !test.partial && rows[ruleIDs[b]] != test.grants[b] {
				t.Fatalf("new grant lost: %+v", rows)
			}
			publish(3, map[string]GrantCount{a: {Packets: 20, Bytes: 200}})
			if test.partial && c.Snapshot().Coverage.KernelPackets.Status != "lower-bound" {
				t.Fatal("lost counter history became exact")
			}
		})
	}
}
