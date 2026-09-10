package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkreport"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	netTestRun    = "e644f07ab14f67cd3f5af72c89295488"
	netTestRunTwo = "cb375d22a1be564428a533a0a838e32a"
)

// netTestConnection is one proven workload connection the way the collector
// records it: named from the ClientHello, with the peer it resolved to.
func netTestConnection(id, name, peer string, sent, received uint64) networkview.Connection {
	return networkview.Connection{ID: id, State: "closed", Reason: "normal_close", Transport: "tls", Name: name,
		NameSource: "sni", Peer: peer, RuleID: "5f71e4ddc97da77473ce17091c856a0d",
		SentBytes: networkview.Value(sent), ReceivedBytes: networkview.Value(received)}
}

// netTestClean is the retained evidence of the reference clean run: one
// connection to example.com, exact coverage, a sealed complete receipt — and
// the historical kernel-closing remnant of coop's own resolver socket, which
// must not appear as workload traffic.
func netTestClean() networkstate.Inspection {
	exact := networkview.MetricCoverage{Status: "exact"}
	snapshot := networkview.Snapshot{Version: 1, RunID: netTestRun, Mode: egress.Filtered, Sequence: 7, Terminal: true,
		AsOf: time.Date(2026, 9, 10, 14, 49, 38, 0, time.UTC), Availability: "available", Scope: "proxy-streams-and-sampled-tcp-sockets",
		PolicyFingerprint: "48554b548acfd6df97395b6095b096fbef8af35a0f05c177f431864427faf7ab", Epoch: "e01c32e95e63de87bb6b87ee8a944d92",
		Health: networkview.HealthLayers{Enforcer: networkview.Health{Status: "ready"}, Gateway: networkview.Health{Status: "stopped"},
			Resolver: networkview.Health{Status: "ready", Reason: "last_lookup_succeeded"}, Collector: networkview.Health{Status: "ready"}},
		Coverage: networkview.Coverage{ProxyBytes: exact, Connections: exact, UpstreamFailures: exact, KernelPackets: exact, GuardDenials: exact,
			MaintenanceQueries: exact, MaintenanceBytes: exact, SocketInventory: exact, BoundaryAttribution: exact},
		Counters: &networkview.Counters{SentBytes: networkview.Value(773), ReceivedBytes: networkview.Value(5323), Connections: networkview.Value(1),
			UpstreamFailures: networkview.Value(0), DeniedPackets: networkview.Value(0), ProtectedPackets: networkview.Value(0),
			DeniedDNSQueries: networkview.Value(0), DeniedTLS: networkview.Value(0), MaintenanceQueries: networkview.Value(1),
			MaintenanceSentBytes: networkview.Value(1920), MaintenanceReceivedBytes: networkview.Value(4346)},
		Connections: []networkview.Connection{
			netTestConnection("1fc1b237d962565a1e016bed3716de47", "example.com", "104.20.23.154:443", 773, 5323),
			{ID: "a2dbb4553f8e958d1427c6e4a18b418d", State: "kernel-closing", Reason: "kernel_closing_remnant", Transport: "tcp",
				NameSource: "socket-inventory", Peer: "1.1.1.1:443", Partial: true},
		},
		Loss: networkview.Loss{OmittedDetails: networkview.Value(0)}, Projection: "destinations-included"}
	receipt := networkview.Receipt{Version: 1, ID: netTestRun, Snapshot: snapshot, Finality: "final", Completeness: "complete",
		Workload: "exited", Cleanup: "complete", Digest: "abc", DigestScope: "destinations-included"}
	return networkstate.Inspection{Version: 1, Observed: snapshot, Receipt: &receipt, Freshness: networkstate.FreshnessTerminal,
		ReadAt: time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC), Cleanup: "complete"}
}

func renderNetRun(view networkreport.View, inspection networkstate.Inspection) string {
	var b bytes.Buffer
	networkreport.WriteRun(&b, ui.Palette{}, view, inspection)
	return b.String()
}

// Acceptance: a complete, clean, terminal run prints only the title, the
// allowed aggregate, every proven destination beneath it and the exact footer.
// No glyph, no status, no zero, no healthy or invariant fact — and no ESC byte
// when the palette is off.
func TestInspectCleanRunIsDestinationFirstAndSilentAboutHealth(t *testing.T) {
	got := renderNetRun(networkreport.View{ID: netTestRun}, netTestClean())
	want := "Network run " + netTestRun + "\n" +
		"\n" +
		"  Allowed   1 connection · 773 B sent · 5.3 KB received\n" +
		"    example.com:443 · TLS\n" +
		"      104.20.23.154:443 · 1 connection · 773 B sent · 5.3 KB received\n" +
		"\n" +
		"Full details: coop net inspect " + netTestRun + " --json\n"
	if got != want {
		t.Fatalf("clean run rendered:\n%s\nwant:\n%s", got, want)
	}
	for _, forbidden := range []string{"Egress", "filtered", "Gateway", "epoch", "Observed", "Read at", "Now", "UNKNOWN",
		"Refused", "Alerts", "none", "Health", "ready", "stopped", "Cleanup", "complete", "Receipt", "final", "✓", "⚠",
		"1.1.1.1", "Other observed", "everything recorded", "\x1b"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("clean run printed %q — a healthy fact is not a line:\n%s", forbidden, got)
		}
	}
}

// Acceptance: repeated connections to one host and peer aggregate into a count
// and exact totals; one name behind two peers keeps both; hosts sort
// deterministically whatever order the evidence arrived in.
func TestInspectGroupsRepeatsKeepsPeersAndSortsDeterministically(t *testing.T) {
	inspection := netTestClean()
	inspection.Observed.Connections = []networkview.Connection{
		netTestConnection("c1", "api.anthropic.com", "160.79.104.10:443", 100, 1000),
		netTestConnection("c2", "zeta.example", "203.0.113.9:8443", 5, 6),
		netTestConnection("c3", "api.anthropic.com", "160.79.104.10:443", 200, 2000),
		netTestConnection("c4", "api.anthropic.com", "160.79.104.11:443", 1, 2),
		netTestConnection("c5", "example.com", "104.20.23.154:443", 773, 5323),
		netTestConnection("c6", "example.com.au", "104.20.23.155:443", 7, 8),
	}
	inspection.Observed.Counters.Connections = networkview.Value(6)
	got := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	body := strings.SplitN(got, "\n\n", 3)[1]
	want := "  Allowed   6 connections · 773 B sent · 5.3 KB received\n" +
		"    api.anthropic.com:443 · TLS\n" +
		"      160.79.104.10:443 · 2 connections · 300 B sent · 3.0 KB received\n" +
		"      160.79.104.11:443 · 1 connection · 1 B sent · 2 B received\n" +
		"    example.com:443 · TLS\n" +
		"      104.20.23.154:443 · 1 connection · 773 B sent · 5.3 KB received\n" +
		"    example.com.au:443 · TLS\n" +
		"      104.20.23.155:443 · 1 connection · 7 B sent · 8 B received\n" +
		"    zeta.example:8443 · TLS\n" +
		"      203.0.113.9:8443 · 1 connection · 5 B sent · 6 B received"
	if body != want {
		t.Fatalf("grouped destinations:\n%s\nwant:\n%s", body, want)
	}
}

// A policy that allowed a destination is not a host that answered: attempts
// the peer never accepted are counted as failed, with the reason, and never
// as connections; an attempt still being made is "connecting". Neither
// inflates the aggregate, which counts established connections only.
func TestInspectSeparatesFailedAndInFlightAttemptsFromConnections(t *testing.T) {
	inspection := netTestClean()
	failed := netTestConnection("f1", "api.example.com", "203.0.113.5:443", 0, 0)
	failed.State, failed.Reason, failed.SentBytes, failed.ReceivedBytes = "failed", "upstream_connection_failed", nil, nil
	failedAgain := failed
	failedAgain.ID = "f2"
	connecting := netTestConnection("c9", "example.com", "104.20.23.154:443", 0, 0)
	connecting.State, connecting.Reason, connecting.SentBytes, connecting.ReceivedBytes = "connecting", "", nil, nil
	inspection.Observed.Connections = append(inspection.Observed.Connections, failed, failedAgain, connecting)
	got := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	want := "  Allowed   1 connection · 773 B sent · 5.3 KB received\n" +
		"    api.example.com:443 · TLS\n" +
		"      203.0.113.5:443 · 2 attempts failed — the destination did not answer\n" +
		"    example.com:443 · TLS\n" +
		"      104.20.23.154:443 · 1 connection · 773 B sent · 5.3 KB received · 1 connecting\n"
	if !strings.Contains(got, want) {
		t.Errorf("failed and in-flight attempts:\n%s\nwant to contain:\n%s", got, want)
	}
	if strings.Contains(got, "3 connections") || strings.Contains(got, "0 B sent") {
		t.Errorf("a failed attempt was counted as a connection:\n%s", got)
	}
}

// Acceptance: unmeasured values stay UNKNOWN. A nil member makes its group's
// total UNKNOWN rather than a precise number that counted it as zero, a
// saturated counter stays a lower bound, and missing counters never become a
// zero aggregate.
func TestInspectNeverInventsATotal(t *testing.T) {
	inspection := netTestClean()
	unknown := netTestConnection("c2", "example.com", "104.20.23.154:443", 0, 0)
	unknown.SentBytes, unknown.ReceivedBytes = nil, nil
	saturated := netTestConnection("c3", "big.example", "198.51.100.7:443", ^uint64(0), 1)
	inspection.Observed.Connections = []networkview.Connection{inspection.Observed.Connections[0], unknown, saturated}
	got := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	for _, want := range []string{
		"      198.51.100.7:443 · 1 connection · ≥ 18446744.1 TB sent · 1 B received\n",
		"      104.20.23.154:443 · 2 connections · UNKNOWN sent · UNKNOWN received\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	inspection.Observed.Counters = nil
	got = renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	if !strings.Contains(got, "  Allowed   UNKNOWN — no totals were recorded for this run\n") || strings.Contains(got, "0 B sent") {
		t.Errorf("missing counters were rendered as a number:\n%s", got)
	}
	empty := networkstate.Inspection{Freshness: networkstate.FreshnessNotObserved}
	got = renderNetRun(networkreport.View{ID: "run1"}, empty)
	if !strings.Contains(got, "  Allowed   UNKNOWN — nothing was recorded for this run\n") || !strings.Contains(got, "Full details: coop net inspect run1 --json") {
		t.Errorf("an unobserved run:\n%s", got)
	}
}

// Acceptance: maintenance, socket-inventory remnants and unattributed sockets
// never inflate or appear beneath Allowed. Coop's own resolver and an ownerless
// closing kernel block are explained internals and cost no line; a socket that
// genuinely could not be attributed is shown as such, with its reason, and
// never as allowed traffic.
func TestInspectKeepsNonWorkloadRowsOutOfAllowed(t *testing.T) {
	inspection := netTestClean()
	inspection.Observed.Connections = append(inspection.Observed.Connections,
		networkview.Connection{ID: "m1", State: "open", Transport: "tcp", NameSource: "trusted-maintenance", Peer: "1.1.1.1:443",
			SentBytes: networkview.Value(1920), ReceivedBytes: networkview.Value(4346)},
		networkview.Connection{ID: "u1", State: "unknown", Reason: "unattributed_socket", Transport: "tcp", NameSource: "unattributed", Peer: "198.51.100.9:443", Partial: true},
		networkview.Connection{ID: "u2", State: "unknown", Reason: "unattributed_socket", Transport: "tcp", NameSource: "unattributed-history", Peer: "198.51.100.9:443", Partial: true},
	)
	got := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	want := "  Allowed   1 connection · 773 B sent · 5.3 KB received\n" +
		"    example.com:443 · TLS\n" +
		"      104.20.23.154:443 · 1 connection · 773 B sent · 5.3 KB received\n" +
		"\n" +
		"  Other observed endpoints\n" +
		"    198.51.100.9:443 · TCP ×2 · never matched to a connection\n" +
		"\nFull details:"
	if !strings.Contains(got, want) {
		t.Fatalf("non-workload rows:\n%s\nwant to contain:\n%s", got, want)
	}
	if strings.Contains(got, "1.1.1.1") || strings.Contains(got, "1.9 KB") {
		t.Errorf("coop's own resolver traffic surfaced as an endpoint or a total:\n%s", got)
	}
}

// Acceptance: raw grant traffic is per-rule kernel accounting, shown apart from
// destinations and only when a grant carried something — no host is invented
// inside a CIDR.
func TestInspectShowsRawTrafficPerRuleWithoutInventingHosts(t *testing.T) {
	inspection := netTestClean()
	inspection.Observed.AddressGrants = []networkview.AddressGrantObservation{
		{RuleID: "aaaa1111bbbb2222cccc3333dddd4444", Packets: 12, Bytes: 9000},
		{RuleID: "eeee5555ffff6666aaaa7777bbbb8888"},
	}
	got := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	want := "\n  Raw traffic — counted per rule, no destination is recorded\n" +
		"    rule aaaa1111 · 12 packets · 9.0 KB\n" +
		"\nFull details:"
	if !strings.Contains(got, want) {
		t.Errorf("raw traffic block:\n%s\nwant to contain:\n%s", got, want)
	}
	if strings.Contains(got, "eeee5555") {
		t.Errorf("a grant that carried nothing got a row:\n%s", got)
	}
	inspection.Observed.AddressGrants = nil
	if got := renderNetRun(networkreport.View{ID: netTestRun}, inspection); strings.Contains(got, "Raw traffic") {
		t.Errorf("a run with no raw grants printed the raw block:\n%s", got)
	}
}

// Acceptance: zero allowed traffic is still a result, said in words, with no
// manufactured destination.
func TestInspectZeroTrafficIsStillAResult(t *testing.T) {
	inspection := netTestClean()
	inspection.Observed.Connections = nil
	inspection.Observed.Counters = &networkview.Counters{Connections: networkview.Value(0), SentBytes: networkview.Value(0), ReceivedBytes: networkview.Value(0), DeniedPackets: networkview.Value(0)}
	got := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	want := "Network run " + netTestRun + "\n\n  Allowed   no external connections\n\nFull details: coop net inspect " + netTestRun + " --json\n"
	if got != want {
		t.Errorf("zero-traffic run:\n%s\nwant:\n%s", got, want)
	}
}

// Acceptance: refusals appear only when present, coalesced by destination,
// boundary and reason, with the real reason; a named refusal keeps the
// hostname-based explain action with the short run id; approval guidance is
// offered only when the evidence itself proves a rule; raw refused packets
// stay a count with no destination.
func TestInspectRefusalsCoalesceAndKeepTheExplainAction(t *testing.T) {
	port := 443
	inspection := netTestClean()
	inspection.Observed.Denials = []networkview.Denial{
		{ID: "d1", Source: "guard", Kind: "dns_denied", Reason: "unapproved_name", Name: "raw.githubusercontent.com"},
		{ID: "d2", Source: "guard", Kind: "dns_denied", Reason: "unapproved_name", Name: "raw.githubusercontent.com"},
		{ID: "d3", Source: "guard", Kind: "dns_denied", Reason: "unsafe_dns_answer", Name: "raw.githubusercontent.com"},
		{ID: "d4", Source: "socket-inventory", Kind: "direct_tcp_attempt", Reason: "fixed_egress_policy", Peer: "203.0.113.5:8443", Port: &port},
	}
	inspection.Observed.Counters.DeniedPackets = networkview.Value(3)
	got := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	want := "\n⚠ 2 destinations were blocked\n" +
		"  raw.githubusercontent.com · DNS ×2\n" +
		"  raw.githubusercontent.com · DNS — a protected address\n" +
		"  203.0.113.5:8443 · direct connection — no rule allows that protocol and port\n" +
		"  3 raw packets were blocked with no destination recorded\n" +
		"  coop net explain raw.githubusercontent.com --run e644f07a   # why\n" +
		"\nFull details:"
	if !strings.Contains(got, want) {
		t.Fatalf("refusals:\n%s\nwant to contain:\n%s", got, want)
	}
	if strings.Contains(got, "coop net approve") {
		t.Errorf("DNS evidence proves no rule, yet approval guidance was offered:\n%s", got)
	}
	// A TLS refusal that carries an exact candidate earns the guidance, and the
	// explain action prefers that host: it is the one a human can act on.
	inspection.Observed.Denials = append(inspection.Observed.Denials, networkview.Denial{ID: "d5", Source: "guard", Kind: "tls_denied",
		Reason: "unapproved_name", Name: "registry.example.com", Port: &port,
		Candidate: &networkview.Candidate{ID: "c1", EvidenceID: "d5", AppliesTo: "next_run",
			Rule: egress.Rule{To: egress.Destination{Domain: "registry.example.com"}, Protocol: "tls", Ports: []int{443}}}})
	got = renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	for _, want := range []string{"⚠ 3 destinations were blocked\n", "  registry.example.com:443 · TLS\n",
		"  coop net explain registry.example.com --run e644f07a   # why\n",
		"  To allow it: add the rule shown by 'coop net explain', then run 'coop net approve'\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Refused raw packets alone are still the boundary being hit.
	inspection.Observed.Denials = nil
	got = renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	if !strings.Contains(got, "\n⚠ 3 raw packets were blocked with no destination recorded\n") || strings.Contains(got, "coop net explain") {
		t.Errorf("raw-only refusals:\n%s", got)
	}
}

// Acceptance: the interactive box's inline result and standalone inspect are
// ONE projection — the same traffic and exception body from the same
// snapshot, differing only by coop's anchor on the heading. Allowed traffic
// leads, the refusal rows follow with the hostname-based explain action, and
// the footer is the same exact line.
func TestInlineRunViewIsTheStandaloneBodyUnderCoopsAnchor(t *testing.T) {
	port := 443
	inspection := netTestClean()
	inspection.Observed.Denials = []networkview.Denial{
		{ID: "d1", Source: "guard", Kind: "tls_denied", Reason: "unapproved_name", Name: "unapproved.example.org", Port: &port},
		{ID: "d2", Source: "guard", Kind: "tls_denied", Reason: "unapproved_name", Name: "unapproved.example.org", Port: &port},
	}
	standalone := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	inline := renderNetRun(networkreport.View{ID: netTestRun, Prefix: "coop: "}, inspection)
	if inline != "coop: "+standalone {
		t.Fatalf("inline view:\n%s\nis not the standalone body under coop's anchor:\n%s", inline, standalone)
	}
	allowed, blocked := strings.Index(inline, "  Allowed   1 connection"), strings.Index(inline, "\n⚠ 1 destination was blocked\n")
	if allowed < 0 || blocked < allowed {
		t.Fatalf("allowed traffic must lead and the refusal follow:\n%s", inline)
	}
	for _, want := range []string{"coop: Network run " + netTestRun + "\n", "    example.com:443 · TLS\n",
		"  unapproved.example.org:443 · TLS ×2\n", "  coop net explain unapproved.example.org --run e644f07a   # why\n",
		"\nFull details: coop net inspect " + netTestRun + " --json\n"} {
		if !strings.Contains(inline, want) {
			t.Errorf("inline view is missing %q:\n%s", want, inline)
		}
	}
	if strings.Contains(inline, "coop net approve") || strings.Contains(inline, "nothing was refused") || strings.Contains(inline, "\x1b") {
		t.Fatalf("inline view offers unproven approval guidance, the retired filler, or ANSI off a terminal:\n%s", inline)
	}
}

// Acceptance: live, unobserved, stale, partial, lossy, alerting and unhealthy
// runs each print only their applicable exception, in plain language; a
// terminal run's stopped gateway is never a warning.
func TestInspectLifecycleExceptionsAppearOnlyWhenPresent(t *testing.T) {
	live := netTestClean()
	live.Receipt, live.Observed.Terminal, live.Freshness, live.Cleanup = nil, false, networkstate.FreshnessFresh, "pending"
	live.Observed.Health.Gateway = networkview.Health{Status: "ready"}
	got := renderNetRun(networkreport.View{ID: netTestRun}, live)
	if !strings.HasPrefix(got, "Network run "+netTestRun+"\n● Live — totals are still changing\n\n  Allowed") {
		t.Errorf("a live run lacks its one context line:\n%s", got)
	}
	if strings.Contains(got, "⚠") {
		t.Errorf("a live run's expected state was reported as an exception:\n%s", got)
	}

	stale := live
	stale.Freshness = networkstate.FreshnessStale
	got = renderNetRun(networkreport.View{ID: netTestRun}, stale)
	if !strings.Contains(got, "\n⚠ Some network activity may be missing — no final record was sealed — the last observation was at 2026-09-10T14:49:38Z\n") || strings.Contains(got, "● Live") {
		t.Errorf("a stale unsealed run:\n%s", got)
	}
	// The same evidence while its supervisor is still running is a run in
	// progress, not a lost one: coop's recovery attempt learned that.
	owned := networkreport.View{ID: netTestRun, Cleanup: networkreport.Cleanup{Expected: true}}
	got = renderNetRun(owned, stale)
	if !strings.Contains(got, "\n● Live — last observed today 14:49\n") || strings.Contains(got, "⚠") {
		t.Errorf("a stale run with a live supervisor:\n%s", got)
	}
	starting := stale
	starting.Freshness, starting.Observed.Sequence = networkstate.FreshnessNotObserved, 0
	got = renderNetRun(owned, starting)
	if !strings.Contains(got, "\n● Live — nothing observed yet\n") || strings.Contains(got, "⚠") {
		t.Errorf("a starting run with a live supervisor:\n%s", got)
	}

	partial := netTestClean()
	partial.Receipt.Completeness = "partial"
	got = renderNetRun(networkreport.View{ID: netTestRun}, partial)
	if !strings.Contains(got, "\n⚠ Some network activity may be missing — the record was sealed with partial evidence\n") {
		t.Errorf("a partial receipt:\n%s", got)
	}

	lossy := netTestClean()
	lossy.Observed.Loss = networkview.Loss{Records: 2, DetailTruncated: true, OmittedDetails: networkview.Value(40), Reasons: []string{"kernel_terminal_sample_unavailable"}}
	lossy.Observed.Coverage.BoundaryAttribution = networkview.MetricCoverage{Status: "lower-bound", Reason: "unattributed_socket"}
	got = renderNetRun(networkreport.View{ID: netTestRun}, lossy)
	want := "\n⚠ Some network activity may be missing\n" +
		"  2 observation records were lost\n" +
		"  only part of the connection and blocked-attempt detail was kept (40 details dropped)\n" +
		"  the final observation was not recorded\n" +
		"  connection ownership are a lower bound — a socket could not be matched to a connection\n"
	if !strings.Contains(got, want) {
		t.Errorf("evidence gaps:\n%s\nwant to contain:\n%s", got, want)
	}

	alerting := netTestClean()
	alerting.Observed.Alerts = []networkview.Alert{{ID: "a1", Category: "denial_burst_dns", Severity: "warning", State: "active", WindowMillis: 60000,
		Threshold: networkview.AlertThreshold{Value: 20, Unit: "denied_dns_queries"}, Facts: networkview.AlertFacts{DNSQueries: 24}}}
	alerting.Observed.Loss.SuppressedAlerts = 1
	got = renderNetRun(networkreport.View{ID: netTestRun}, alerting)
	want = "\n⚠ 1 alert was raised\n" +
		"  a burst of blocked DNS lookups (warning) — 24 in 1m, over a threshold of 20\n" +
		"  1 more alert exceeded this run's alert budget and was not recorded\n"
	if !strings.Contains(got, want) {
		t.Errorf("alerts:\n%s\nwant to contain:\n%s", got, want)
	}

	unhealthy := live
	unhealthy.Observed.Health.Enforcer = networkview.Health{Status: "unknown", Reason: "unexpected_agent_connection"}
	got = renderNetRun(networkreport.View{ID: netTestRun}, unhealthy)
	if !strings.Contains(got, "\n⚠ The packet filter was unknown — the agent opened a connection outside the gateway's capture\n") {
		t.Errorf("an unhealthy enforcer:\n%s", got)
	}
	if got := renderNetRun(networkreport.View{ID: netTestRun}, netTestClean()); strings.Contains(got, "gateway") {
		t.Errorf("a terminal run's stopped gateway was reported:\n%s", got)
	}
}

// Acceptance: cleanup is never a success line; it is not an issue on a live
// run or while the supervisor still owns it; after coop's own recovery attempt
// a remaining problem names the external blocker with an exactly two-space
// continuation, and never tells the operator to run recovery.
func TestInspectCleanupIsReportedOnlyWhenStillOwed(t *testing.T) {
	clean := netTestClean()
	if got := renderNetRun(networkreport.View{ID: netTestRun}, clean); strings.Contains(got, "leanup") {
		t.Errorf("finished cleanup got a line:\n%s", got)
	}
	live := clean
	live.Receipt, live.Observed.Terminal, live.Freshness, live.Cleanup = nil, false, networkstate.FreshnessFresh, "pending"
	if got := renderNetRun(networkreport.View{ID: netTestRun}, live); strings.Contains(got, "leanup") {
		t.Errorf("a live run's pending cleanup got a warning:\n%s", got)
	}
	owed := clean
	owed.Cleanup = "pending"
	got := renderNetRun(networkreport.View{ID: netTestRun, Cleanup: networkreport.Cleanup{Blocker: "Docker is unavailable", Remedy: "Start Docker; Coop will retry automatically"}}, owed)
	want := "\n⚠ Cleanup incomplete — Docker is unavailable\n  Start Docker; Coop will retry automatically\n\nFull details:"
	if !strings.Contains(got, want) {
		t.Errorf("unresolved cleanup:\n%s\nwant to contain:\n%s", got, want)
	}
	if strings.Contains(got, "coop net recover") {
		t.Errorf("the operator was told to run routine recovery:\n%s", got)
	}
	if got := renderNetRun(networkreport.View{ID: netTestRun, Cleanup: networkreport.Cleanup{Expected: true}}, owed); strings.Contains(got, "leanup") {
		t.Errorf("a supervisor still cleaning up was reported as incomplete:\n%s", got)
	}
	// A view that made no attempt of its own (the watch's sealed projection)
	// still names who retries, never a bare headline.
	if got := renderNetRun(networkreport.View{ID: netTestRun}, owed); !strings.Contains(got, "\n⚠ Cleanup incomplete\n  Coop will retry automatically\n") {
		t.Errorf("cleanup owed with no attempt of this view's own:\n%s", got)
	}
	// The recovery result maps onto those terms: a live supervisor is expected,
	// a skipped daemon names itself, a failed removal keeps its cause.
	for _, tc := range []struct {
		result box.NetworkRecovery
		want   networkreport.Cleanup
	}{
		{box.NetworkRecovery{Live: true, Skipped: "its supervisor (pid 7) is still running or its identity is uncertain"}, networkreport.Cleanup{Expected: true}},
		{box.NetworkRecovery{Skipped: "the runtime it ran on is unavailable: Cannot connect to the Docker daemon"},
			networkreport.Cleanup{Blocker: "the runtime it ran on is unavailable: Cannot connect to the Docker daemon", Remedy: "Coop will retry automatically once that Docker daemon is back"}},
		{box.NetworkRecovery{Failures: []error{errors.New("guard: permission denied")}, Pending: []string{"guard"}},
			networkreport.Cleanup{Blocker: "guard: permission denied", Remedy: "Coop will retry automatically"}},
		{box.NetworkRecovery{Pending: []string{"ipc", "observations"}},
			networkreport.Cleanup{Blocker: "still to remove: ipc, observations", Remedy: "Coop will retry automatically"}},
		{box.NetworkRecovery{Removed: []string{"agent"}, Sealed: true}, networkreport.Cleanup{}},
	} {
		if got := netCleanupOf(tc.result); got != tc.want {
			t.Errorf("netCleanupOf(%+v) = %+v, want %+v", tc.result, got, tc.want)
		}
	}
}

func TestNetRunsFlagsRejectAnythingElse(t *testing.T) {
	opts, err := parseNetRunsFlags([]string{"--all", "--json"})
	if err != nil || !opts.all || !opts.json {
		t.Fatalf("parseNetRunsFlags = (%+v, %v)", opts, err)
	}
	if _, err := parseNetRunsFlags([]string{"--project", "/tmp"}); err == nil {
		t.Error("an unknown runs flag was accepted")
	}
	if _, err := parseNetRunsFlags([]string{"--all", "--all-projects"}); err == nil {
		t.Error("two scopes were accepted at once")
	}
}

func TestNetRunArgsTakeOneOptionalRun(t *testing.T) {
	opts, err := parseNetRunArgs("inspect", []string{"abc", "--json"})
	if err != nil || opts.id != "abc" || !opts.json {
		t.Fatalf("parseNetRunArgs = (%+v, %v)", opts, err)
	}
	// No run means the newest one recorded for this project, resolved later.
	if opts, err := parseNetRunArgs("inspect", []string{"--json"}); err != nil || opts.id != "" {
		t.Errorf("inspect without a run = (%+v, %v)", opts, err)
	}
	if _, err := parseNetRunArgs("inspect", []string{"a", "b"}); err == nil {
		t.Error("inspect with two runs was accepted")
	}
	if _, err := parseNetRunArgs("inspect", []string{"abc", "--destinations"}); err == nil {
		t.Error("inspect accepted an unknown flag")
	}
}

// Acceptance: every run command resolves a unique short prefix; an ambiguous
// one fails with the prefixes that would settle it, never a guess; a full id
// passes through; a listing that could not be read whole trusts no prefix.
func TestNetResolveRunAcceptsUniquePrefixesAndNeverGuesses(t *testing.T) {
	page := networkstate.ExecutionPage{Executions: []networkstate.ExecutionSummary{
		{ID: netTestRun}, {ID: netTestRunTwo}, {ID: "cb375d22ffffffffffffffffffffffff"}}}
	if got, err := netResolveRun(page, "e644"); err != nil || got != netTestRun {
		t.Errorf("unique prefix = (%q, %v)", got, err)
	}
	if got, err := netResolveRun(page, netTestRun); err != nil || got != netTestRun {
		t.Errorf("full id = (%q, %v)", got, err)
	}
	_, err := netResolveRun(page, "cb375d22")
	if err == nil || !strings.Contains(err.Error(), "cb375d22a") || !strings.Contains(err.Error(), "cb375d22f") || strings.Contains(err.Error(), netTestRunTwo) {
		t.Errorf("ambiguous prefix = %v, want the two longer prefixes", err)
	}
	if _, err := netResolveRun(page, "0000"); err == nil || !strings.Contains(err.Error(), "coop net runs") {
		t.Errorf("unknown prefix = %v", err)
	}
	if _, err := netResolveRun(page, "not-hex"); err == nil {
		t.Error("a non-hex reference was accepted")
	}
	page.Incomplete = true
	if _, err := netResolveRun(page, "e644"); err == nil || !strings.Contains(err.Error(), "full run id") {
		t.Errorf("a prefix was trusted against an incomplete listing: %v", err)
	}
}

// Acceptance: inspect without a run selects the newest recorded run of the
// current project only; watch selects the sole active one and refuses to pick
// among several.
func TestNetSelectRunDefaultsToThisProjectsNewestOrSoleActive(t *testing.T) {
	project := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: project}}
	page := networkstate.ExecutionPage{Executions: []networkstate.ExecutionSummary{
		{ID: netTestRunTwo, Project: project, StartedAt: time.Unix(20, 0), Final: false},
		{ID: netTestRun, Project: project, StartedAt: time.Unix(10, 0), Final: true},
		{ID: "ffffffffffffffffffffffffffffffff", Project: "/elsewhere", StartedAt: time.Unix(30, 0), Final: false}}}
	if got, err := a.netSelectRun("inspect", page, ""); err != nil || got != netTestRunTwo {
		t.Errorf("inspect default = (%q, %v), want this project's newest", got, err)
	}
	if got, err := a.netSelectRun("watch", page, ""); err != nil || got != netTestRunTwo {
		t.Errorf("watch default = (%q, %v), want the sole active run", got, err)
	}
	page.Executions[1].Final = false
	if _, err := a.netSelectRun("watch", page, ""); err == nil || !strings.Contains(err.Error(), "2 runs are active") {
		t.Errorf("two active runs were not refused: %v", err)
	}
	page.Executions = page.Executions[2:]
	if _, err := a.netSelectRun("inspect", page, ""); err == nil || !strings.Contains(err.Error(), "this project") {
		t.Errorf("another project's run was selected: %v", err)
	}
	if got, err := a.netSelectRun("inspect", page, "ffff"); err != nil || got != "ffffffffffffffffffffffffffffffff" {
		t.Errorf("an explicit prefix must still resolve anywhere: (%q, %v)", got, err)
	}
}

func TestNetListingDTOPublishesOnlyItsAllowlist(t *testing.T) {
	runs := []networkstate.ExecutionSummary{{ID: "a", Epoch: "e", Project: "/private/host/secret-project",
		StartedAt: time.Unix(1, 0).UTC(), Final: true}}
	dto := netListingDTO(runs, networkstate.ExecutionPage{Unreadable: 2, Incomplete: true})
	var buf bytes.Buffer
	if err := netWriteJSON(&buf, dto); err != nil {
		t.Fatal(err)
	}
	data := buf.String()
	// The retained record carries the owner's project path. Publishing it by
	// growing into a serialized type is exactly what the allowlist prevents.
	if strings.Contains(data, "secret-project") {
		t.Errorf("listing DTO leaked the project path: %s", data)
	}
	for _, want := range []string{`"id":"a"`, `"final":true`, `"unreadable":2`, `"incomplete":true`} {
		if !strings.Contains(data, want) {
			t.Errorf("listing DTO is missing %s: %s", want, data)
		}
	}
}

func TestNetFilterProjectMatchesThroughSymlinks(t *testing.T) {
	runs := []networkstate.ExecutionSummary{{ID: "a", Project: "/private/tmp/x"}, {ID: "b", Project: "/other"}}
	got := netFilterProject(runs, "/private/tmp/x")
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("netFilterProject = %+v", got)
	}
	if got := netFilterProject(runs, "/nothing/here"); got != nil {
		t.Errorf("unrelated project matched: %+v", got)
	}
}

// A watch opens with one heading and what the run already did, appends only
// what is new, and closes with the shared projection exactly once. It never
// repaints and never prints a heartbeat.
func TestWatchAppendsNewEventsAndRendersTheFinalProjectionOnce(t *testing.T) {
	reads := []networkstate.Inspection{
		{Freshness: networkstate.FreshnessNotObserved, Revision: 1},
		{Freshness: networkstate.FreshnessFresh, Revision: 2, Observed: networkview.Snapshot{Sequence: 1,
			Connections: []networkview.Connection{netTestConnection("c1", "example.com", "104.20.23.154:443", 1, 2)},
			Denials:     []networkview.Denial{{ID: "d1", Kind: "dns_denied", Name: "example.org", Reason: "unapproved_name"}}}},
		{Freshness: networkstate.FreshnessFresh, Revision: 3, Observed: networkview.Snapshot{Sequence: 2,
			Connections: []networkview.Connection{netTestConnection("c1", "example.com", "104.20.23.154:443", 1, 2)},
			Denials:     []networkview.Denial{{ID: "d1", Kind: "dns_denied", Name: "example.org", Reason: "unapproved_name"}}}},
		{Freshness: networkstate.FreshnessTerminal, Revision: 4, Observed: networkview.Snapshot{Sequence: 3, Terminal: true},
			Receipt: &networkview.Receipt{ID: "run1", Finality: "final", Completeness: "partial"}},
	}
	var out bytes.Buffer
	tick := make(chan time.Time, len(reads))
	for range reads {
		tick <- time.Unix(0, 0)
	}
	i := 0
	code, err := runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: tick, now: func() time.Time { return time.Unix(int64(i), 0) },
		out: &out, id: "run1", palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) {
			value := reads[i]
			if i < len(reads)-1 {
				i++
			}
			return value, nil
		},
	})
	if code != 0 || err != nil {
		t.Fatalf("runNetWatch = (%d, %v)", code, err)
	}
	text := out.String()
	if headings := strings.Count(text, "Network run run1"); headings != 2 {
		t.Errorf("printed %d headings, want the opening one and the final projection:\n%s", headings, text)
	}
	for _, want := range []string{"allowed example.com:443 · TLS\n", "blocked example.org · DNS\n", "Full details: coop net inspect run1 --json\n",
		"⚠ Some network activity may be missing — the record was sealed with partial evidence\n"} {
		if strings.Count(text, want) != 1 {
			t.Errorf("want exactly one %q in:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{"so far", "fresh,", "not sealed yet"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the stream printed a status ledger %q:\n%s", forbidden, text)
		}
	}
}

func TestWatchExitsImmediatelyOnAnAlreadySealedRun(t *testing.T) {
	var out bytes.Buffer
	code, err := runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: make(chan time.Time), now: time.Now, out: &out, id: "run1", palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) {
			return networkstate.Inspection{Receipt: &networkview.Receipt{ID: "run1", Finality: "final", Completeness: "complete"}}, nil
		},
	})
	if code != 0 || err != nil {
		t.Fatalf("runNetWatch = (%d, %v)", code, err)
	}
	if strings.Count(out.String(), "Network run run1") != 1 || strings.Contains(out.String(), "● Live") {
		t.Errorf("a sealed run printed more than its one projection:\n%s", out.String())
	}
}

func TestWatchReportsAReadFailureInsteadOfLoopingOnIt(t *testing.T) {
	code, err := runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: make(chan time.Time), now: time.Now, out: &bytes.Buffer{}, id: "gone",
		palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) {
			return networkstate.Inspection{}, networkstate.ErrEvidenceUnavailable
		},
	})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "coop net runs") {
		t.Fatalf("runNetWatch = (%d, %v), want a failure naming the listing", code, err)
	}
}

func TestWatchStopsWhenTheReaderCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, err := runNetWatch(netWatchDeps{
		ctx: ctx, tick: make(chan time.Time), now: time.Now, out: &bytes.Buffer{}, id: "run1", palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) { return networkstate.Inspection{}, nil },
	})
	if code != 0 || err != nil {
		t.Fatalf("cancelled watch = (%d, %v)", code, err)
	}
}

func TestWatchDeltaCoalescesRepeatsAndBoundsItself(t *testing.T) {
	repeat := func(n int) networkstate.Inspection {
		var denials []networkview.Denial
		for i := range n {
			denials = append(denials, networkview.Denial{ID: string(rune('a' + i)), Kind: "dns_denied", Name: "example.org", Reason: "unapproved_name"})
		}
		return networkstate.Inspection{Observed: networkview.Snapshot{Denials: denials}}
	}
	lines := netWatchDelta(networkstate.Inspection{}, repeat(4))
	if len(lines) != 1 || lines[0] != "blocked example.org · DNS ×4" {
		t.Errorf("four retries of one name = %q, want one counted line", lines)
	}
	var many []networkview.Denial
	for i := range netWatchDeltaLines + 3 {
		many = append(many, networkview.Denial{ID: string(rune('a' + i)), Kind: "dns_denied", Name: "h" + string(rune('a'+i)) + ".example", Reason: "unapproved_name"})
	}
	lines = netWatchDelta(networkstate.Inspection{}, networkstate.Inspection{Observed: networkview.Snapshot{Denials: many}})
	if len(lines) != netWatchDeltaLines+1 || !strings.Contains(lines[len(lines)-1], "coop net inspect") {
		t.Errorf("a refusal storm was not bounded: %q", lines)
	}
	// Already-reported evidence never repeats: replay is idempotent.
	if lines := netWatchDelta(repeat(4), repeat(4)); lines != nil {
		t.Errorf("unchanged evidence produced %q", lines)
	}
	// New connections are named by destination, coalesced, and only when the
	// workload proved them.
	opened := networkstate.Inspection{Observed: networkview.Snapshot{Connections: []networkview.Connection{
		netTestConnection("c1", "example.com", "104.20.23.154:443", 1, 1), netTestConnection("c2", "example.com", "104.20.23.154:443", 1, 1),
		{ID: "u1", NameSource: "unattributed", Transport: "tcp", Peer: "198.51.100.9:443"}}}}
	if lines := netWatchDelta(networkstate.Inspection{}, opened); len(lines) != 1 || lines[0] != "allowed example.com:443 · TLS ×2" {
		t.Errorf("new connections = %q", lines)
	}
}

// Acceptance: the run listing says what happened in words — a measured zero is
// "no external connections", blocked attempts appear only when nonzero, and
// nothing unmeasured reads as zero.
func TestRunOutcomeSaysWhatHappenedInWords(t *testing.T) {
	if got := netRunOutcome(networkstate.Inspection{}); got != "nothing observed" {
		t.Errorf("unobserved run = %q", got)
	}
	observed := func(connections uint64, blocked int) networkstate.Inspection {
		var denials []networkview.Denial
		for i := range blocked {
			denials = append(denials, networkview.Denial{ID: string(rune('a' + i))})
		}
		return networkstate.Inspection{Observed: networkview.Snapshot{Sequence: 1,
			Counters: &networkview.Counters{Connections: networkview.Value(connections)}, Denials: denials}}
	}
	for want, inspection := range map[string]networkstate.Inspection{
		"13 connections · 8 blocked": observed(13, 8), "1 connection": observed(1, 0), "no external connections": observed(0, 0),
	} {
		if got := netRunOutcome(inspection); got != want {
			t.Errorf("outcome = %q, want %q", got, want)
		}
	}
	truncated := observed(3, 2)
	truncated.Observed.Counters = nil
	truncated.Observed.Loss.DetailTruncated = true
	if got := netRunOutcome(truncated); got != "UNKNOWN connections · 2+ blocked" {
		t.Errorf("truncated outcome = %q, want UNKNOWN and a lower bound", got)
	}
	raw := observed(0, 0)
	raw.Observed.Counters.DeniedPackets = networkview.Value(5)
	if got := netRunOutcome(raw); got != "no external connections · 5 raw packets blocked" {
		t.Errorf("raw-only outcome = %q", got)
	}
}

// Acceptance: `coop net runs` shows five recent runs with short ids and a
// pointer to the rest, no success total; --all-projects labels every project
// by name and canonical path.
func TestNetRunsViewIsBoundedShortAndLabeled(t *testing.T) {
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	runs := []netRun{
		{ExecutionSummary: networkstate.ExecutionSummary{ID: netTestRunTwo, Project: "/Users/me/coop", StartedAt: now.Add(-2 * time.Hour), Final: true}, Outcome: "13 connections · 8 blocked"},
		{ExecutionSummary: networkstate.ExecutionSummary{ID: netTestRun, Project: "/Users/me/coop", StartedAt: now.Add(-26 * time.Hour), Final: true}, Outcome: "1 connection"},
		{ExecutionSummary: networkstate.ExecutionSummary{ID: "30d7a6cd5dabf6dacc3be65790c23c28", Project: "/Users/me/coop", StartedAt: now.Add(-72 * time.Hour), CleanupPending: true}, Outcome: "no external connections"},
	}
	var b bytes.Buffer
	writeNetRuns(&b, ui.Palette{}, now, "/Users/me/coop", runs, 33, netRunsOptions{})
	want := "Recent network runs for coop\n\n" +
		"  cb375d22  today 16:00        13 connections · 8 blocked\n" +
		"  e644f07a  yesterday 16:00    1 connection\n" +
		"  30d7a6cd  2026-09-07 18:00   no external connections · no receipt yet · cleanup pending\n" +
		"\nShowing 3 of 33 · coop net runs --all\n"
	if b.String() != want {
		t.Errorf("runs view:\n%s\nwant:\n%s", b.String(), want)
	}
	var all bytes.Buffer
	runs[2].Project = "/Users/me/other"
	writeNetRuns(&all, ui.Palette{}, now, "", runs, 3, netRunsOptions{allProjects: true})
	for _, want := range []string{"Network runs on this host\n", "\ncoop\n  /Users/me/coop\n  cb375d22  ", "\nother\n  /Users/me/other\n  30d7a6cd  "} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("all-projects view is missing %q:\n%s", want, all.String())
		}
	}
	if strings.Contains(all.String(), "Showing") || strings.Contains(all.String(), "recorded for") {
		t.Errorf("the all-projects view carries a footer:\n%s", all.String())
	}
	var empty bytes.Buffer
	writeNetRuns(&empty, ui.Palette{}, now, "/Users/me/coop", nil, 0, netRunsOptions{})
	if !strings.Contains(empty.String(), "no filtered run has been recorded for this project yet") {
		t.Errorf("empty listing:\n%s", empty.String())
	}
}

// Acceptance: bare `coop net` states the mode and its real cause, names
// provider access in one example, lists approved project access only when
// any exists, shows the request diff only when pending, and never prints a
// healthy setup record or run history.
func TestPostureViewAnswersWhatANewRunCanReach(t *testing.T) {
	rule := func(domain string) egress.Rule {
		return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}
	}
	approved := box.NetworkPosture{Project: "/private/tmp/coop", Mode: egress.Filtered, Source: box.PostureFromProject,
		Approval:  &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{rule("github.com"), rule("registry.npmjs.org")}},
		Requested: []egress.Rule{rule("github.com"), rule("registry.npmjs.org")}, RequestedMode: egress.Filtered}
	var b bytes.Buffer
	writeNetPosture(&b, ui.Palette{}, approved)
	want := "Network access for coop\n  /private/tmp/coop\n\n" +
		"New runs use filtered internet access because .agent/project.yaml says so.\n" +
		"When you start an agent, it can reach its provider — for example, Claude can reach Anthropic.\n" +
		"Everything else is blocked.\n\n" +
		"This project can also reach:\n  github.com:443 · TLS\n  registry.npmjs.org:443 · TLS\n"
	if b.String() != want {
		t.Errorf("approved posture:\n%s\nwant:\n%s", b.String(), want)
	}

	pending := approved
	pending.Source = box.PostureFromApproval
	pending.Requested = []egress.Rule{rule("github.com"), rule("api.example.com")}
	pending.Add, pending.Remove = []egress.Rule{rule("api.example.com")}, []egress.Rule{rule("registry.npmjs.org")}
	pending.Pending = &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}
	b.Reset()
	writeNetPosture(&b, ui.Palette{}, pending)
	want = "New runs use filtered internet access because that is what was approved for this project.\n" +
		"When you start an agent, it can reach its provider — for example, Claude can reach Anthropic.\n" +
		"Everything else is blocked.\n\n" +
		"⚠ New runs cannot start until this project's network request is approved\n" +
		"  Access:\n" +
		"    + api.example.com:443 · TLS      new request\n" +
		"      github.com:443 · TLS           already approved\n" +
		"    - registry.npmjs.org:443 · TLS   no longer requested\n" +
		"  Review it: coop net approve\n"
	if !strings.HasSuffix(b.String(), want) {
		t.Errorf("pending posture:\n%s\nwant to end with:\n%s", b.String(), want)
	}
	if strings.Contains(b.String(), "can also reach") {
		t.Errorf("a stale approved list was presented as ready beside a pending request:\n%s", b.String())
	}

	// A launch can refuse for something no rule diff shows; that reason is the
	// pending line's detail.
	replaced := approved
	replaced.Pending = &networkstate.PendingApproval{Reason: "the project directory at /private/tmp/coop was replaced since it was approved"}
	b.Reset()
	writeNetPosture(&b, ui.Palette{}, replaced)
	// The baseline is still shown: it is what the review re-binds to the directory that is there now.
	if !strings.HasSuffix(b.String(), "⚠ New runs cannot start until this project's network request is approved\n"+
		"  the project directory at /private/tmp/coop was replaced since it was approved\n"+
		"  Access:\n      github.com:443 · TLS           already approved\n      registry.npmjs.org:443 · TLS   already approved\n"+
		"  Review it: coop net approve\n") {
		t.Errorf("replaced directory:\n%s", b.String())
	}

	// A mode change is the other thing no rule diff shows: an unapproved open
	// request is named as the escalation it is, with the rules it drops.
	widening := approved
	widening.Source, widening.RequestedMode, widening.Requested = box.PostureFromApproval, egress.Open, nil
	widening.Add, widening.Remove = nil, approved.Approval.Envelope
	widening.Pending = &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}
	b.Reset()
	writeNetPosture(&b, ui.Palette{}, widening)
	want = "⚠ New runs cannot start until this project's network request is approved\n" +
		"  Internet access changes from filtered to unrestricted.\n" +
		"  ⚠ Nothing will be blocked — an agent can reach any destination\n" +
		"  Access:\n" +
		"    - github.com:443 · TLS           no longer requested\n" +
		"    - registry.npmjs.org:443 · TLS   no longer requested\n" +
		"  Review it: coop net approve\n"
	if !strings.HasSuffix(b.String(), want) {
		t.Errorf("widening posture:\n%s\nwant to end with:\n%s", b.String(), want)
	}

	for _, forbidden := range []string{"This host", "set up", "Recent runs", "remembered", "Egress", "Rules none", "destinations of its own"} {
		if strings.Contains(b.String(), forbidden) {
			t.Errorf("posture printed %q:\n%s", forbidden, b.String())
		}
	}

	// A never-approved file asking for open access: the mode is the whole change.
	freshOpen := box.NetworkPosture{Project: "/p", Mode: egress.Open, Source: box.PostureFromProject, RequestedMode: egress.Open,
		Pending: &networkstate.PendingApproval{Reason: "this project asks for unrestricted internet access, which has not been approved"}}
	b.Reset()
	writeNetPosture(&b, ui.Palette{}, freshOpen)
	if !strings.HasSuffix(b.String(), "⚠ New runs cannot start until this project's network request is approved\n"+
		"  Internet access will be unrestricted.\n  ⚠ Nothing will be blocked — an agent can reach any destination\n  Review it: coop net approve\n") {
		t.Errorf("fresh open request:\n%s", b.String())
	}

	open := box.NetworkPosture{Project: "/p", Mode: egress.Open, Source: box.PostureFromDefault}
	b.Reset()
	writeNetPosture(&b, ui.Palette{}, open)
	if !strings.Contains(b.String(), "New runs have unrestricted internet access because that is coop's default.\n") || strings.Contains(b.String(), "provider") {
		t.Errorf("open posture:\n%s", b.String())
	}
	offline := box.NetworkPosture{Project: "/p", Mode: egress.None, Source: box.PostureFromHost}
	b.Reset()
	writeNetPosture(&b, ui.Palette{}, offline)
	if !strings.Contains(b.String(), "New runs are offline because COOP_EGRESS says so.\nNo external destination is reachable, so an agent cannot reach its provider.\n") {
		t.Errorf("offline posture:\n%s", b.String())
	}
	// A fresh project is the common case: filtered, nothing approved, nothing
	// pending — and host setup is a launch's own business, so nothing else.
	fresh := box.NetworkPosture{Project: "/p", Mode: egress.Filtered, Source: box.PostureFromProject, RequestedMode: egress.Filtered}
	b.Reset()
	writeNetPosture(&b, ui.Palette{}, fresh)
	if !strings.HasSuffix(b.String(), "Everything else is blocked.\n") || strings.Contains(b.String(), "coop net") {
		t.Errorf("fresh project:\n%s", b.String())
	}
}

func TestNetRuleTextReadsLikeADestination(t *testing.T) {
	for want, rule := range map[string]egress.Rule{
		"docs.example.com:443,8443 · TLS": {To: egress.Destination{Domain: "docs.example.com"}, Protocol: "tls", Ports: []int{443, 8443}},
		"10.42.9.0/24:123 · UDP":          {To: egress.Destination{CIDR: "10.42.9.0/24"}, Protocol: "udp", Ports: []int{123}},
		"10.42.8.12 · icmp echo-request":  {To: egress.Destination{IP: "10.42.8.12"}, Protocol: "icmp", Types: []string{"echo-request"}},
		"service db:5432 · TCP":           {To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}},
	} {
		if got := netRuleText(rule); got != want {
			t.Errorf("netRuleText = %q, want %q", got, want)
		}
	}
}

func TestNetWhenReadsLikeAClock(t *testing.T) {
	now := time.Date(2026, 9, 10, 18, 5, 0, 0, time.UTC)
	for want, at := range map[string]time.Time{
		"today 16:03":      time.Date(2026, 9, 10, 16, 3, 0, 0, time.UTC),
		"yesterday 23:59":  time.Date(2026, 9, 9, 23, 59, 0, 0, time.UTC),
		"2026-09-08 14:49": time.Date(2026, 9, 8, 14, 49, 0, 0, time.UTC),
	} {
		if got := networkreport.When(now, at); got != want {
			t.Errorf("netWhen = %q, want %q", got, want)
		}
	}
}

func TestNetProjectRefusesToGuessOutsideAProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := netProject(""); err == nil || !strings.Contains(err.Error(), "coop net runs --all-projects") {
		t.Fatalf("netProject outside a project = %v, want a pointer at the explicit scope", err)
	}
	if got, err := netProject(dir); err != nil || got != dir {
		t.Errorf("netProject(%q) = (%q, %v)", dir, got, err)
	}
}

func TestNetRunErrPointsAtTheListing(t *testing.T) {
	err := netRunErr("abc", networkstate.ErrEvidenceUnavailable)
	if !strings.Contains(err.Error(), "coop net runs") || !strings.Contains(err.Error(), `"abc"`) {
		t.Errorf("netRunErr = %v", err)
	}
	if err := netRunErr("abc", errors.New("boom")); !strings.Contains(err.Error(), "boom") {
		t.Errorf("netRunErr dropped the cause: %v", err)
	}
}

func TestNetEventBoundRefusesAnOversizedView(t *testing.T) {
	if err := netEventBound(netEventMaxBytes + 1); err == nil {
		t.Error("an oversized view was accepted")
	}
	if err := netEventBound(10); err != nil {
		t.Errorf("netEventBound(10) = %v", err)
	}
}
