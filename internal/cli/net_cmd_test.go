package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkreport"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
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

// netTestCandidate is the one image/runtime pair every fixture run qualifies
// against: what it contains never matters here, only that each run is bound to
// an exact candidate the way a real launch is.
var netTestCandidate = networkstate.CandidateSpec{
	Runtime: networkstate.RuntimeBinding{HostFamily: "darwin", Endpoint: "unix:///fixture.sock", DaemonID: "fixture-daemon",
		OS: "linux", Architecture: "arm64", ServerVersion: "29.4.0", KernelVersion: "fixture"},
	ClientImage: "sha256:" + strings.Repeat("b", 64), GatewayImage: "sha256:" + strings.Repeat("a", 64),
	ClientDefinition: strings.Repeat("c", 64), ClientClosure: strings.Repeat("d", 64), GatewaySource: strings.Repeat("e", 64),
	Libc: "glibc", NodeBase: "node@sha256:" + strings.Repeat("f", 64), GoBase: "golang@sha256:" + strings.Repeat("0", 64),
}

// netHostFixture points every net command at retained state this test owns, and
// hands back the store a launch would write through.
func netHostFixture(t *testing.T) *networkstate.Store {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := box.NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// netFixtureProject is a canonical project directory: the net commands compare
// resolved paths, so a symlinked temp dir must be resolved once here.
func netFixtureProject(t *testing.T) string {
	t.Helper()
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// netDepartSupervisor rewrites one retained run's supervisor identity so the
// kernel reports the launching process as departed — what a host crash, a
// SIGKILL or a reboot leaves behind.
func netDepartSupervisor(t *testing.T, store *networkstate.Store, id string) {
	t.Helper()
	path := filepath.Join(store.Path(), "execution-"+id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	supervisor, _ := record["supervisor"].(map[string]any)
	token, _ := supervisor["start_token"].(string)
	if token == "" {
		t.Fatal("the run recorded no supervisor identity")
	}
	// Same pid, a different start time in the same token format: the kernel
	// proves the process that launched this run is not the one holding that pid.
	supervisor["start_token"] = token + "9"
	updated, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
}

// netRecordRun retains one filtered run of project the way a launch does:
// admitted policy, its own preflight, one execution record. Nothing is
// observed and nothing is sealed, so the run is exactly the interrupted shape
// an inspection has to settle.
func netRecordRun(t *testing.T, store *networkstate.Store, project string) networkstate.Execution {
	t.Helper()
	mode := egress.Filtered
	policy, err := store.Admit(project, networkstate.Admission{InvocationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	smoke, err := store.BeginQualification(netTestCandidate,
		[]networkstate.QualifiedClient{{Provider: "claude", Client: egress.ClientCLI, Version: "2.1.260"}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := smoke.CreateExecution(context.Background(), networkstate.ExecutionSpec{
		Project: project, PolicyFingerprint: policy.Fingerprint, Runtime: "docker",
		DaemonID: netTestCandidate.Runtime.DaemonID, Endpoint: netTestCandidate.Runtime.Endpoint,
		GatewayImage: netTestCandidate.GatewayImage, ClientImage: netTestCandidate.ClientImage})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// netSealRun gives a retained run one proven destination, whatever refusals the
// test needs, and a sealed receipt: the shape every shareable record is made
// from and every explanation is read out of.
func netSealRun(t *testing.T, store *networkstate.Store, record networkstate.Execution, denials ...networkview.Denial) networkstate.Execution {
	t.Helper()
	ctx := context.Background()
	snapshot := record.Snapshot
	snapshot.Sequence, snapshot.Availability, snapshot.Terminal = 1, "available", true
	snapshot.Connections = []networkview.Connection{
		netTestConnection("1fc1b237d962565a1e016bed3716de47", "example.com", "104.20.23.154:443", 773, 5323)}
	snapshot.Denials = denials
	record, err := store.AcceptSnapshot(ctx, record.ID, record.Revision, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.SealExecution(ctx, record.ID, record.Revision, "exited")
	if err != nil {
		t.Fatal(err)
	}
	if record.Receipt == nil {
		t.Fatal("the run was not sealed")
	}
	return record
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
	want := "\n⚠ Traffic to 2 remote addresses was blocked\n" +
		"  raw.githubusercontent.com · DNS ×2\n" +
		"  raw.githubusercontent.com · DNS — a protected address\n" +
		"  203.0.113.5:8443 · direct connection — no rule allows that protocol and port\n" +
		"  3 raw packets were blocked with no remote address recorded\n" +
		"To see why: coop net blocked raw.githubusercontent.com --run e644f07a\n" +
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
	for _, want := range []string{"⚠ Traffic to 3 remote addresses was blocked\n", "  registry.example.com:443 · TLS\n",
		"To see why: coop net blocked registry.example.com --run e644f07a\n",
		"  To allow it: add the rule shown by `coop net blocked`, then run `coop net approve` on the host\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Refused raw packets alone are still the boundary being hit.
	inspection.Observed.Denials = nil
	got = renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	if !strings.Contains(got, "\n⚠ 3 raw packets were blocked with no remote address recorded\n") || strings.Contains(got, "coop net blocked") {
		t.Errorf("raw-only refusals:\n%s", got)
	}
}

// Acceptance: the interactive box's inline result and standalone inspect are
// ONE projection — the same aggregate, the same destinations and the same
// exception body from the same snapshot. Only the heading and the destination
// rows differ: the box says `Networking stats:` and carries each destination's
// own totals, because its reader just watched the run rather than choosing it.
func TestInlineRunViewIsTheStandaloneBody(t *testing.T) {
	port := 443
	inspection := netTestClean()
	inspection.Observed.Denials = []networkview.Denial{
		{ID: "d1", Source: "guard", Kind: "tls_denied", Reason: "unapproved_name", Name: "unapproved.example.org", Port: &port},
		{ID: "d2", Source: "guard", Kind: "tls_denied", Reason: "unapproved_name", Name: "unapproved.example.org", Port: &port},
	}
	standalone := renderNetRun(networkreport.View{ID: netTestRun}, inspection)
	inline := renderNetRun(networkreport.View{ID: netTestRun, Inline: true}, inspection)
	_, standaloneBody, _ := strings.Cut(standalone, "\n")
	_, inlineBody, _ := strings.Cut(inline, "\n")
	if !strings.HasPrefix(inline, "Networking stats:\n") || !strings.HasPrefix(standalone, "Network run "+netTestRun+"\n") {
		t.Fatalf("headings drifted:\n%s\n%s", inline, standalone)
	}
	// Everything from the exception block down is the same body, byte for byte.
	inlineExceptions := inlineBody[strings.Index(inlineBody, "\n⚠"):]
	if standaloneExceptions := standaloneBody[strings.Index(standaloneBody, "\n⚠"):]; inlineExceptions != standaloneExceptions {
		t.Fatalf("the inline exceptions are not the standalone body:\n%s\n%s", inlineExceptions, standaloneExceptions)
	}
	allowed, blocked := strings.Index(inline, "  Allowed   1 connection"), strings.Index(inline, "\n⚠ Traffic to 1 remote address was blocked\n")
	if allowed < 0 || blocked < allowed {
		t.Fatalf("allowed traffic must lead and the refusal follow:\n%s", inline)
	}
	for _, want := range []string{"    example.com:443 · TLS · 773 B sent · 5.3 KB received\n",
		"  unapproved.example.org:443 · TLS ×2\n", "To see why: coop net blocked unapproved.example.org --run e644f07a\n",
		"\nFull details: coop net inspect " + netTestRun + " --json\n"} {
		if !strings.Contains(inline, want) {
			t.Errorf("inline view is missing %q:\n%s", want, inline)
		}
	}
	if strings.Contains(inline, netTestRun+"\n") || strings.Contains(inline, "nothing was refused") || strings.Contains(inline, "\x1b") {
		t.Fatalf("inline view repeats the opaque id, the retired filler, or ANSI off a terminal:\n%s", inline)
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
			networkreport.Cleanup{Blocker: "Docker is unavailable", Remedy: "Start Docker; Coop will retry automatically"}},
		{box.NetworkRecovery{Skipped: "the runtime at unix:///x.sock is a different daemon than the one this run used"},
			networkreport.Cleanup{Blocker: "Docker is connected to a different daemon than this run used", Remedy: "Reconnect to the original Docker daemon"}},
		{box.NetworkRecovery{Failures: []error{errors.New("guard: permission denied")}, Pending: []string{"guard"}},
			networkreport.Cleanup{Blocker: "guard: permission denied", Remedy: "Coop will retry automatically"}},
		{box.NetworkRecovery{Pending: []string{"ipc", "observations"}, PendingVolumes: 2},
			networkreport.Cleanup{Blocker: "2 temporary volumes are still in use", Remedy: "Coop will retry automatically"}},
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
	if got, err := netResolveRun(page, "e644", "coop net inspect"); err != nil || got != netTestRun {
		t.Errorf("unique prefix = (%q, %v)", got, err)
	}
	if got, err := netResolveRun(page, netTestRun, "coop net inspect"); err != nil || got != netTestRun {
		t.Errorf("full id = (%q, %v)", got, err)
	}
	_, err := netResolveRun(page, "cb375d22", "coop net inspect")
	if err == nil || !strings.Contains(err.Error(), "cb375d22a") || !strings.Contains(err.Error(), "cb375d22f") || strings.Contains(err.Error(), netTestRunTwo) {
		t.Errorf("ambiguous prefix = %v, want the two longer prefixes", err)
	}
	if _, err := netResolveRun(page, "0000", "coop net inspect"); err == nil || !strings.Contains(err.Error(), "coop net runs") {
		t.Errorf("unknown prefix = %v", err)
	}
	if _, err := netResolveRun(page, "not-hex", "coop net inspect"); err == nil {
		t.Error("a non-hex reference was accepted")
	}
	page.Incomplete = true
	if _, err := netResolveRun(page, "e644", "coop net inspect"); err == nil || !strings.Contains(err.Error(), "Use a full run ID") {
		t.Errorf("a prefix was trusted against an incomplete listing: %v", err)
	}
}

func TestExplicitNetRunWithoutHistoryNamesTheRequestedRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	a := &app{cfg: &config.Config{}}
	for name, run := range map[string]func() (int, error){
		"inspect": func() (int, error) { return a.cmdNetRun("inspect", []string{"deadbeef"}) },
		"watch":   func() (int, error) { return a.cmdNetRun("watch", []string{"deadbeef"}) },
		"export":  func() (int, error) { return a.cmdNetExport([]string{"deadbeef"}) },
	} {
		t.Run(name, func(t *testing.T) {
			code, err := run()
			if code != 1 || err == nil || !strings.Contains(err.Error(), `No network run matches "deadbeef"`) {
				t.Fatalf("explicit missing run = (%d, %v)", code, err)
			}
		})
	}
}

func TestNetRecoverNeedsNoRuntimeWhenNothingNeedsCleanup(t *testing.T) {
	t.Run("no history", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		assertCleanRecoveryWithoutRuntime(t, nil)
	})
	t.Run("empty history", func(t *testing.T) {
		_ = netHostFixture(t)
		assertCleanRecoveryWithoutRuntime(t, nil)
	})
}

func assertCleanRecoveryWithoutRuntime(t *testing.T, args []string) {
	t.Helper()
	a := &app{cfg: &config.Config{RuntimeName: "definitely-not-a-runtime"}}
	var code int
	var err error
	out := captureStderr(t, func() { code, err = a.cmdNetRecover(args) })
	if code != 0 || err != nil || out != "No interrupted network runs need cleanup.\n" {
		t.Fatalf("clean recovery without a runtime = (%d, %v, %q)", code, err, out)
	}
}

func TestNetRecoverStillRequiresRuntimeForPendingCleanup(t *testing.T) {
	store := netHostFixture(t)
	netRecordRun(t, store, netFixtureProject(t))
	a := &app{cfg: &config.Config{RuntimeName: "definitely-not-a-runtime"}}
	code, err := a.cmdNetRecover(nil)
	if code != -1 || err == nil || !strings.Contains(err.Error(), `runtime "definitely-not-a-runtime" not found`) {
		t.Fatalf("pending recovery without a runtime = (%d, %v)", code, err)
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
	if _, err := a.netSelectRun("watch", page, ""); err == nil || !strings.Contains(err.Error(), "Choose a network run to watch") {
		t.Errorf("two unfinished runs were not refused: %v", err)
	}
	page.Executions = page.Executions[2:]
	if _, err := a.netSelectRun("inspect", page, ""); err == nil || !strings.Contains(err.Error(), "No network run recorded for this project") {
		t.Errorf("another project's run was selected: %v", err)
	}
	if got, err := a.netSelectRun("inspect", page, "ffff"); err != nil || got != "ffffffffffffffffffffffffffffffff" {
		t.Errorf("an explicit prefix must still resolve anywhere: (%q, %v)", got, err)
	}
}

// Acceptance: `coop net runs` is this project's 25 newest runs and a pointer at
// the rest; --all widens how many are listed, never whose they are.
func TestNetRunsDefaultsToTheNewestOfThisProjectAndAllStaysInIt(t *testing.T) {
	store := netHostFixture(t)
	project, elsewhere := netFixtureProject(t), netFixtureProject(t)
	mine := map[string]bool{}
	for range netRecentRuns + 1 {
		mine[networkreport.ShortID(netRecordRun(t, store, project).ID)] = true
	}
	foreign := networkreport.ShortID(netRecordRun(t, store, elsewhere).ID)
	a := &app{cfg: &config.Config{RepoOverride: project}}
	listed := func(t *testing.T, args []string) (string, int) {
		t.Helper()
		out := captureStdout(t, func() {
			if code, err := a.cmdNetRuns(args); code != 0 || err != nil {
				t.Fatalf("coop net runs %v = (%d, %v)", args, code, err)
			}
		})
		if strings.Contains(out, foreign) {
			t.Fatalf("another project's run was listed:\n%s", out)
		}
		rows := 0
		for _, line := range strings.Split(out, "\n") {
			if id, _, found := strings.Cut(strings.TrimPrefix(line, "  "), "  "); found && mine[id] {
				rows++
			}
		}
		return out, rows
	}
	out, rows := listed(t, nil)
	if rows != netRecentRuns {
		t.Fatalf("default listing showed %d runs, want %d:\n%s", rows, netRecentRuns, out)
	}
	if !strings.Contains(out, "\nShowing 25 of 26 · coop net runs --all\n") {
		t.Errorf("the bounded listing does not point at the rest:\n%s", out)
	}
	if out, rows := listed(t, []string{"--all"}); rows != netRecentRuns+1 {
		t.Errorf("--all showed %d of this project's runs:\n%s", rows, out)
	}
}

// Acceptance: inspecting an interrupted run makes coop's own bounded recovery
// attempt first. A runtime it cannot reach leaves the run exactly as pending as
// it found it, names the real blocker, and still renders the retained evidence.
func TestInspectSettlesADeadRunItselfAndKeepsItPendingWhenItCannot(t *testing.T) {
	store := netHostFixture(t)
	project := netFixtureProject(t)
	record := netRecordRun(t, store, project)
	netDepartSupervisor(t, store, record.ID)
	a := &app{cfg: &config.Config{RepoOverride: project}, rt: containerruntime.Runtime{Name: "fixture-runtime"}, rtSet: true}
	out := captureStdout(t, func() {
		if code, err := a.cmdNetRun("inspect", []string{record.ID}); code != 0 || err != nil {
			t.Fatalf("coop net inspect = (%d, %v)", code, err)
		}
	})
	want := "\n⚠ Cleanup incomplete — Docker is unavailable\n" +
		"  Start Docker; Coop will retry automatically\n"
	if !strings.Contains(out, want) {
		t.Fatalf("inspect made no recovery attempt of its own:\n%s\nwant to contain:\n%s", out, want)
	}
	for _, forbidden := range []string{"coop net recover", "\x1b"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("inspect printed %q:\n%s", forbidden, out)
		}
	}
	// The evidence coop did retain is still the report: the run, its allowed
	// block and the same footer every inspection ends with.
	for _, line := range []string{"Network run " + record.ID + "\n", "  Allowed   UNKNOWN — nothing was recorded for this run\n",
		"Full details: coop net inspect " + record.ID + " --json\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("inspect dropped %q from the projection:\n%s", line, out)
		}
	}
	// Nothing was claimed settled: the next start still owes this cleanup.
	again, err := store.Execution(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Receipt != nil {
		t.Errorf("a run coop could not touch was sealed: %+v", again.Receipt)
	}
	page, err := netExecutions()
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Executions) != 1 || !page.Executions[0].CleanupPending {
		t.Errorf("the run stopped being pending after a failed attempt: %+v", page.Executions)
	}
}

// Acceptance: an export is the sealed receipt and nothing else, redacted unless
// the operator asks for the names on their own machine — and a run that has not
// sealed one is refused rather than exported as null.
func TestNetExportWithholdsDestinationsUnlessAsked(t *testing.T) {
	store := netHostFixture(t)
	project := netFixtureProject(t)
	record := netSealRun(t, store, netRecordRun(t, store, project))
	a := &app{cfg: &config.Config{RepoOverride: project}}
	export := func(t *testing.T, args ...string) string {
		t.Helper()
		return captureStdout(t, func() {
			if code, err := a.cmdNetExport(args); code != 0 || err != nil {
				t.Fatalf("coop net export %v = (%d, %v)", args, code, err)
			}
		})
	}
	redacted := export(t, record.ID)
	for _, secret := range []string{"example.com", "104.20.23.154"} {
		if strings.Contains(redacted, secret) {
			t.Errorf("the default export carried %q — a blocked or allowed name can encode a secret:\n%s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, `"digest_scope":"destinations-withheld"`) {
		t.Errorf("the default export does not say what it withheld:\n%s", redacted)
	}
	included := export(t, record.ID, "--include-addresses")
	for _, want := range []string{`"example.com"`, `"104.20.23.154:443"`, `"digest_scope":"destinations-included"`} {
		if !strings.Contains(included, want) {
			t.Errorf("the asked-for export is missing %s:\n%s", want, included)
		}
	}

	// What it writes is the receipt contract, not this host's local inspection.
	var receipt networkview.Receipt
	if err := json.Unmarshal([]byte(included), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ID != record.ID || receipt.Finality != "final" || receipt.Workload != "exited" {
		t.Errorf("exported receipt = %+v, want the sealed record of this run", receipt)
	}
	for _, local := range []string{`"read_at"`, `"freshness"`, `"observed"`, `"revision"`} {
		if strings.Contains(included, local) {
			t.Errorf("the export carried the local inspection field %s:\n%s", local, included)
		}
	}
	// The checksum is a fingerprint of what is in the file, and never claims to
	// be more than that.
	for _, overclaim := range []string{"signed", "signature", "attest", "proof", "tamper"} {
		if strings.Contains(strings.ToLower(included), overclaim) {
			t.Errorf("the export describes its checksum as %q:\n%s", overclaim, included)
		}
	}

	// A run with nothing sealed has no record to share.
	pending := netRecordRun(t, store, project)
	if code, err := a.cmdNetExport([]string{pending.ID}); code != 1 || err == nil || !strings.Contains(err.Error(), "has no final record yet") {
		t.Errorf("exporting an unsealed run = (%d, %v)", code, err)
	}
	for name, args := range map[string][]string{
		"no run":       nil,
		"two runs":     {record.ID, "cb375d22"},
		"unknown flag": {record.ID, "--destinations"},
	} {
		if code, err := a.cmdNetExport(args); code != 2 || err == nil {
			t.Errorf("coop net export with %s = (%d, %v), want a usage refusal", name, code, err)
		}
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
	for _, want := range []string{"Allowed example.com:443 · TLS\n", "Blocked example.org · DNS\n", "Full details: coop net inspect run1 --json\n",
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

// Acceptance: --json is the same stream as NDJSON — one whole inspection per
// change, each on its own line carrying its own freshness and loss, none of the
// human view's prose, and every event bounded on its own.
func TestWatchJSONIsOneBoundedInspectionPerChange(t *testing.T) {
	reads := []networkstate.Inspection{
		{Freshness: networkstate.FreshnessNotObserved, Revision: 1},
		{Freshness: networkstate.FreshnessFresh, Revision: 2, Observed: networkview.Snapshot{Sequence: 1,
			Connections: []networkview.Connection{netTestConnection("c1", "example.com", "104.20.23.154:443", 1, 2)}}},
		{Freshness: networkstate.FreshnessFresh, Revision: 2, Observed: networkview.Snapshot{Sequence: 1,
			Connections: []networkview.Connection{netTestConnection("c1", "example.com", "104.20.23.154:443", 1, 2)}}},
		{Freshness: networkstate.FreshnessTerminal, Revision: 3, Observed: networkview.Snapshot{Sequence: 2, Terminal: true},
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
		out: &out, json: true, id: "run1", palette: ui.Palette{},
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
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("the NDJSON stream does not end its last event:\n%q", out.String())
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	var events []networkstate.Inspection
	for n, line := range lines {
		var event networkstate.Inspection
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line %d is not one whole inspection: %v\n%s", n, err, line)
		}
		if event.Freshness == "" {
			t.Errorf("line %d carries no freshness of its own: %s", n, line)
		}
		if n > 0 && line == lines[n-1] {
			t.Errorf("line %d repeats its predecessor — the stream heartbeats: %s", n, line)
		}
		events = append(events, event)
	}
	if len(events) < 2 || events[0].Freshness != networkstate.FreshnessNotObserved {
		t.Fatalf("the stream does not open with what the run had already done: %+v", events)
	}
	sealed := events[len(events)-1]
	if sealed.Receipt == nil || sealed.Receipt.Completeness != "partial" {
		t.Errorf("the stream does not close on the sealed record: %+v", sealed)
	}
	for _, event := range events[:len(events)-1] {
		if event.Receipt != nil {
			t.Errorf("a receipt was emitted before the run sealed one: %+v", event)
		}
	}
	for _, prose := range []string{"Network run", "● Live", "allowed example.com", "Full details:", "⚠"} {
		if strings.Contains(out.String(), prose) {
			t.Errorf("the NDJSON stream carries the human view's %q:\n%s", prose, out.String())
		}
	}

	// One anomalous read ends the stream instead of writing an unbounded event.
	oversized := networkstate.Inspection{Freshness: networkstate.FreshnessFresh,
		Observed: networkview.Snapshot{Denials: []networkview.Denial{{ID: "d1", Name: strings.Repeat("a", netEventMaxBytes)}}}}
	code, err = runNetWatch(netWatchDeps{
		ctx: context.Background(), tick: make(chan time.Time), now: time.Now, out: &bytes.Buffer{}, json: true,
		id: "run1", palette: ui.Palette{},
		read: func(time.Time) (networkstate.Inspection, error) { return oversized, nil },
	})
	if code != 1 || err == nil {
		t.Errorf("an oversized event = (%d, %v), want the watch to stop", code, err)
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
	lines := netWatchDelta(networkstate.Inspection{}, repeat(4), "e644f07a")
	if len(lines) != 1 || lines[0] != "Blocked example.org · DNS · 4 attempts" {
		t.Errorf("four retries of one name = %q, want one counted line", lines)
	}
	var many []networkview.Denial
	for i := range netWatchDeltaLines + 3 {
		many = append(many, networkview.Denial{ID: string(rune('a' + i)), Kind: "dns_denied", Name: "h" + string(rune('a'+i)) + ".example", Reason: "unapproved_name"})
	}
	lines = netWatchDelta(networkstate.Inspection{}, networkstate.Inspection{Observed: networkview.Snapshot{Denials: many}}, "e644f07a")
	if len(lines) != netWatchDeltaLines+1 || !strings.Contains(lines[len(lines)-1], "coop net inspect") {
		t.Errorf("a refusal storm was not bounded: %q", lines)
	}
	// Already-reported evidence never repeats: replay is idempotent.
	if lines := netWatchDelta(repeat(4), repeat(4), "e644f07a"); lines != nil {
		t.Errorf("unchanged evidence produced %q", lines)
	}
	// New connections are named by destination, coalesced, and only when the
	// workload proved them.
	opened := networkstate.Inspection{Observed: networkview.Snapshot{Connections: []networkview.Connection{
		netTestConnection("c1", "example.com", "104.20.23.154:443", 1, 1), netTestConnection("c2", "example.com", "104.20.23.154:443", 1, 1),
		{ID: "u1", NameSource: "unattributed", Transport: "tcp", Peer: "198.51.100.9:443"}}}}
	if lines := netWatchDelta(networkstate.Inspection{}, opened, "e644f07a"); len(lines) != 1 || lines[0] != "Allowed example.com:443 · TLS · 2 connections" {
		t.Errorf("new connections = %q", lines)
	}
}

// Acceptance: the run listing says what happened in words — a measured zero is
// "no external connections", blocked attempts appear only when nonzero, and
// nothing unmeasured reads as zero.
func TestRunOutcomeSaysWhatHappenedInWords(t *testing.T) {
	if got := netRunOutcome(networkstate.Inspection{}); got != netActivityUnknown {
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
	if got := netRunOutcome(truncated); got != "connection count unknown · at least 2 blocked attempts" {
		t.Errorf("truncated outcome = %q, want an unmeasured count and a lower bound", got)
	}
	raw := observed(0, 0)
	raw.Observed.Counters.DeniedPackets = networkview.Value(5)
	if got := netRunOutcome(raw); got != "no external connections · 5 raw packets blocked" {
		t.Errorf("raw-only outcome = %q", got)
	}
}

func TestNetRuleTextReadsLikeADestination(t *testing.T) {
	for want, rule := range map[string]egress.Rule{
		"(no remote address)":             {},
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
