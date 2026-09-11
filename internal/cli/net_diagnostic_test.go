package cli

import (
	"bytes"
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

const netTestEventID = "0123456789abcdef0123456789abcdef"

// Acceptance: check accepts an HTTPS URL or an exact host and reduces it to
// the hostname and effective port; --run is optional; an address still needs
// its transport and a recorded run. explain takes the host a person
// recognizes, or an exact event id with its run.
func TestDiagnosticArgsAcceptURLsHostsAndOptionalRuns(t *testing.T) {
	opts, err := parseNetDiagnosticArgs("check", []string{"https://Docs.Example.COM:8443/path?x=1", "--json"})
	if err != nil {
		t.Fatalf("parseNetDiagnosticArgs: %v", err)
	}
	if opts.query != "docs.example.com" || opts.policy.Port != 8443 || opts.policy.Protocol != "tls" || opts.run != "" || !opts.json {
		t.Errorf("parsed = %+v", opts)
	}
	if opts, err := parseNetDiagnosticArgs("check", []string{"example.com", "--run", "e644"}); err != nil || opts.policy.Port != 443 || opts.run != "e644" {
		t.Errorf("host with a run = (%+v, %v)", opts, err)
	}
	if opts, err := parseNetDiagnosticArgs("check", []string{"example.com:853"}); err != nil || opts.policy.Port != 853 {
		t.Errorf("host:port = (%+v, %v)", opts, err)
	}
	for _, bad := range [][]string{
		{"http://example.com"}, {"*.example.com"}, {}, {"10.0.0.1", "--protocol", "tcp", "--port", "22"}, {"10.0.0.1", "--run", "abc"},
		{"example.com", "--redact"}, {"example.com", "example.org"},
	} {
		if _, err := parseNetDiagnosticArgs("check", bad); err == nil {
			t.Errorf("check accepted %q", bad)
		}
	}
	if opts, err := parseNetDiagnosticArgs("check", []string{"10.0.0.1", "--protocol", "tcp", "--port", "22", "--run", "abc"}); err != nil || !opts.policy.Address.IsValid() {
		t.Errorf("an address against a run = (%+v, %v)", opts, err)
	}
	if opts, err := parseNetDiagnosticArgs("explain", []string{"https://Registry.Example.com/"}); err != nil || opts.query != "registry.example.com" || opts.run != "" {
		t.Errorf("explain by host = (%+v, %v)", opts, err)
	}
	if _, err := parseNetDiagnosticArgs("explain", []string{netTestEventID}); err == nil {
		t.Error("an event id without its run was accepted")
	}
	if opts, err := parseNetDiagnosticArgs("explain", []string{netTestEventID, "--run", "abc"}); err != nil || opts.query != netTestEventID {
		t.Errorf("explain by event = (%+v, %v)", opts, err)
	}
	if _, err := parseNetDiagnosticArgs("explain", []string{"example.com", "--port", "443"}); err == nil {
		t.Error("explain accepted a check-only flag")
	}
}

// Acceptance: a current check answers for a normal new run — an approved
// project rule, or the provider access a named agent brings — with one verdict
// and one cause; a pending request refuses to answer against a policy that
// cannot launch; nothing is appended after the cause.
func TestCurrentCheckAnswersForANewRun(t *testing.T) {
	rule := func(domain string, ports ...int) egress.Rule {
		return egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: ports}
	}
	bundles := map[string]egress.Bundle{
		"claude": {Provider: "claude", Core: []egress.Rule{rule("api.anthropic.com", 443), rule("platform.claude.com", 443)}},
		"codex":  {Provider: "codex", Core: []egress.Rule{rule("api.openai.com", 443), rule("platform.claude.com", 443)}},
	}
	filtered := box.NetworkPosture{Mode: egress.Filtered, Source: box.PostureFromProject,
		Approval: &networkstate.Approval{Posture: egress.Filtered, Envelope: []egress.Rule{rule("*.example.com", 443, 8443)}}}
	render := func(answer netCheckAnswer) string {
		var b bytes.Buffer
		writeNetCheck(&b, ui.Palette{}, answer.Allowed, answer.Verdict, answer.Cause)
		return b.String()
	}
	cases := []struct {
		name    string
		posture box.NetworkPosture
		host    string
		port    int
		want    string
	}{
		{"approved rule", filtered, "docs.example.com", 8443, "✓ New filtered runs can reach docs.example.com:8443\n  Allowed by an approved project rule.\n"},
		{"wrong port", filtered, "docs.example.com", 853, "✗ No new run can reach docs.example.com:853\n  No approved project rule allows it — add one under box.egress_rules in .agent/project.yaml, then run 'coop net approve'.\n"},
		{"one agent", filtered, "api.anthropic.com", 443, "✓ Claude can reach api.anthropic.com:443\n  Provider access for Claude is included automatically.\n"},
		{"two agents", filtered, "platform.claude.com", 443, "✓ Claude and Codex can reach platform.claude.com:443\n  Their provider access is included automatically.\n"},
		{"open", box.NetworkPosture{Mode: egress.Open}, "anything.example", 443, "✓ New runs can reach anything.example:443\n  This project's network access is unrestricted.\n"},
		{"offline", box.NetworkPosture{Mode: egress.None}, "api.anthropic.com", 443, "✗ New runs cannot reach api.anthropic.com:443\n  This project's runs are offline.\n"},
		{"pending", box.NetworkPosture{Mode: egress.Filtered, Add: []egress.Rule{rule("new.example", 443)}, Pending: &networkstate.PendingApproval{Reason: "this project asks for network access that has not been approved"}}, "api.anthropic.com", 443,
			"✗ No new run can start until this project's network request is approved\n  Review it: coop net approve\n"},
	}
	for _, tc := range cases {
		got := render(netCurrentCheck(tc.posture, tc.host, tc.port, bundles))
		if got != tc.want {
			t.Errorf("%s:\n%s\nwant:\n%s", tc.name, got, tc.want)
		}
		for _, forbidden := range []string{"No connection was made", "Other runs", "fingerprint", "rule_allowed", "unapproved_name"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("%s appended a disclaimer %q:\n%s", tc.name, forbidden, got)
			}
		}
	}
}

// Acceptance: a historical check names the run whose frozen rules answered.
func TestHistoricalCheckNamesTheRunAndStops(t *testing.T) {
	allowed := networkstate.PolicyExplanation{RunID: netTestRun, Domain: "example.com", Protocol: "tls", Port: 443, Allowed: true, Reason: "rule_allowed",
		Message: "a rule in this run allows this name and port; whether the host answered is another question",
		Origins: []networkstate.PolicyOrigin{{Kind: "operator", Name: "allow-domain"}}}
	var b bytes.Buffer
	writeNetHistoricalCheck(&b, ui.Palette{}, allowed)
	if want := "✓ Run e644f07a could reach example.com:443\n  Allowed by your allow-domain flag.\n"; b.String() != want {
		t.Errorf("allowed:\n%s\nwant:\n%s", b.String(), want)
	}
	denied := networkstate.PolicyExplanation{RunID: netTestRun, Domain: "registry.npmjs.org", Protocol: "tls", Port: 443, Reason: "unapproved_name",
		Message: "no rule in this run allows this name — a rule you approve now applies to the next run"}
	b.Reset()
	writeNetHistoricalCheck(&b, ui.Palette{}, denied)
	if want := "✗ Run e644f07a could not reach registry.npmjs.org:443\n  No rule in this run allows this name — a rule you approve now applies to the next run.\n"; b.String() != want {
		t.Errorf("denied:\n%s\nwant:\n%s", b.String(), want)
	}
	b.Reset()
	writeNetHistoricalCheck(&b, ui.Palette{}, networkstate.PolicyExplanation{RunID: netTestRun, Protocol: "tls", Port: 443, Withheld: true, Reason: "unapproved_name", Message: "x"})
	if !strings.Contains(b.String(), "could not reach the withheld destination:443") {
		t.Errorf("withheld destination view:\n%s", b.String())
	}
}

// Acceptance: when the evidence proves hostname, protocol and port, explain
// prints the smallest exact YAML list item ready to paste under
// box.egress_rules, then the approve command; the run and time are named.
func TestExplainRendersTheExactRuleToPaste(t *testing.T) {
	port := 443
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	explanation := netExplanation{RunID: netTestRunTwo, Host: "registry.example.com", Causes: []netExplainCause{{Count: 2,
		Explanation: networkstate.EventExplanation{RunID: netTestRunTwo, CandidateState: "draft_not_approved",
			Message: "no rule in this run allows this name — a rule you approve now applies to the next run",
			Event: networkview.Denial{ID: netTestEventID, Source: "guard", Basis: "observed", Kind: "tls_denied",
				Reason: "unapproved_name", Name: "registry.example.com", Port: &port, At: time.Date(2026, 9, 10, 16, 3, 0, 0, time.UTC),
				Candidate: &networkview.Candidate{ID: "c1", EvidenceID: netTestEventID, AppliesTo: "next_run",
					Rule: egress.Rule{To: egress.Destination{Domain: "registry.example.com"}, Protocol: "tls", Ports: []int{443}}}}}}}}
	var b bytes.Buffer
	writeNetExplanation(&b, ui.Palette{}, now, explanation)
	want := "registry.example.com:443 was blocked\n" +
		"  Run cb375d22 · today 16:03\n" +
		"\n" +
		"No rule in this run allows this name — a rule you approve now applies to the next run. (×2)\n" +
		"\n" +
		"Add this rule under box.egress_rules in .agent/project.yaml:\n" +
		"\n" +
		"  - to: {domain: \"registry.example.com\"}\n" +
		"    protocol: tls\n" +
		"    ports: [443]\n" +
		"\n" +
		"Then run:\n" +
		"  coop net approve\n"
	if b.String() != want {
		t.Errorf("explain:\n%s\nwant:\n%s", b.String(), want)
	}
}

// The no-draft cases mean different things and must not be blurred: a boundary
// no rule can cross, a failure that was no refusal, and evidence too thin to
// name a transport. None fabricates YAML.
func TestExplainNeverFabricatesARuleFromThinEvidence(t *testing.T) {
	render := func(kind, reason string, truncated bool) string {
		var b bytes.Buffer
		writeNetExplanation(&b, ui.Palette{}, time.Now(), netExplanation{RunID: netTestRun, Host: "example.org", Causes: []netExplainCause{{Count: 1,
			Explanation: networkstate.EventExplanation{RunID: netTestRun, CandidateState: "none", Message: "m", DetailTruncated: truncated,
				Event: networkview.Denial{ID: netTestEventID, Kind: kind, Reason: reason, Name: "example.org", At: time.Unix(1, 0).UTC()}}}}})
		return b.String()
	}
	const (
		boundary   = "No rule can allow this: it is a protected or unsupported destination, not a gap in the project's rules."
		notRefused = "This was not blocked by a rule — the connection failed on its own, and another rule would not change that."
		tooThin    = "a blocked DNS lookup does not say whether the destination is TLS, or on which port"
	)
	for _, tc := range []struct{ kind, reason, want string }{
		{"dns_denied", "protected_destination", boundary},
		{"dns_denied", "unsafe_dns_answer", boundary},
		{"tls_denied", "tls_ech_unsupported", boundary},
		{"tls_denied", "unsupported_capability", boundary},
		{"tls_denied", "tls_name_missing", boundary},
		{"tls_denied", "tls_name_invalid", boundary},
		{"tls_denied", "upstream_unreachable", notRefused},
		{"tls_denied", "observation_unavailable", notRefused},
		{"dns_denied", "unapproved_name", tooThin},
	} {
		got := render(tc.kind, tc.reason, false)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s:\n%s\nwant to contain:\n%s", tc.reason, got, tc.want)
		}
		if strings.Contains(got, "- to:") || strings.Contains(got, "coop net approve") {
			t.Errorf("%s fabricated a rule to paste:\n%s", tc.reason, got)
		}
	}
	// A run that kept only part of its detail says so, so a cause that is not
	// here does not read as proof that nothing else was blocked.
	if got := render("dns_denied", "unapproved_name", true); !strings.Contains(got, "\nThis run kept only part of its detail, so other blocked attempts may be missing.\n") {
		t.Errorf("a truncated run does not say what it lost:\n%s", got)
	}
	if got := render("dns_denied", "unapproved_name", false); strings.Contains(got, "only part of its detail") {
		t.Errorf("a complete run claimed missing detail:\n%s", got)
	}
}

// netDenial is one retained refusal of host, in the shape the guard records
// it: an id, its producer sequence and the time it happened.
func netDenial(id string, seq networkview.Count, kind, reason, host string, at time.Time) networkview.Denial {
	return networkview.Denial{ID: strings.Repeat(id, 32/len(id)), Source: "guard", Basis: "observed",
		Sequence: seq, Kind: kind, Reason: reason, Name: host, At: at}
}

// Acceptance: `coop net explain <host>` with no run answers from THIS project's
// most recent run that blocked it, groups that run's repeats into one counted
// cause, keeps a materially different cause separate, and never reaches into
// another project's box. `--run` pins the run instead.
func TestExplainChoosesThisProjectsNewestBlockingRunAndGroupsRepeats(t *testing.T) {
	store := netHostFixture(t)
	project, elsewhere := netFixtureProject(t), netFixtureProject(t)
	base := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	older := netSealRun(t, store, netRecordRun(t, store, project),
		netDenial("a1", 1, "dns_denied", "unapproved_name", "example.org", base))
	newer := netSealRun(t, store, netRecordRun(t, store, project),
		netDenial("b1", 1, "dns_denied", "unapproved_name", "example.org", base.Add(time.Minute)),
		netDenial("b2", 2, "dns_denied", "unapproved_name", "example.org", base.Add(2*time.Minute)),
		netDenial("b3", 3, "dns_denied", "unapproved_name", "example.org", base.Add(3*time.Minute)),
		netDenial("b4", 4, "dns_denied", "protected_destination", "example.org", base.Add(4*time.Minute)),
		netDenial("b5", 5, "dns_denied", "unapproved_name", "other.example", base.Add(5*time.Minute)))
	foreign := netSealRun(t, store, netRecordRun(t, store, elsewhere),
		netDenial("c1", 1, "dns_denied", "unapproved_name", "example.org", base.Add(6*time.Minute)))

	// "Newest" is what the retained records say, so a fixture that recorded them
	// out of order must fail here rather than pick a run at random below.
	if !newer.StartedAt.After(older.StartedAt) || !foreign.StartedAt.After(newer.StartedAt) {
		t.Fatalf("the fixture did not record its runs in order: %v %v %v", older.StartedAt, newer.StartedAt, foreign.StartedAt)
	}
	a := &app{cfg: &config.Config{RepoOverride: project}}
	explain := func(t *testing.T, args ...string) string {
		t.Helper()
		return captureStdout(t, func() {
			if code, err := a.netDiagnostic("explain", args); code != 0 || err != nil {
				t.Fatalf("coop net explain %v = (%d, %v)", args, code, err)
			}
		})
	}
	got := explain(t, "example.org")
	if !strings.Contains(got, "Run "+networkreport.ShortID(newer.ID)+" ") {
		t.Fatalf("explain did not answer from this project's newest blocking run (%s):\n%s", networkreport.ShortID(newer.ID), got)
	}
	for _, wrong := range []string{networkreport.ShortID(foreign.ID), networkreport.ShortID(older.ID)} {
		if strings.Contains(got, wrong) {
			t.Errorf("explain reached run %s:\n%s", wrong, got)
		}
	}
	for _, want := range []string{"example.org · DNS ×3 — ", "example.org · DNS — a protected address — "} {
		if !strings.Contains(got, want) {
			t.Errorf("explain lost the grouping %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "other.example") {
		t.Errorf("explain answered about a host nobody asked for:\n%s", got)
	}

	// --run pins the run, however old it is.
	if got := explain(t, "example.org", "--run", older.ID[:8]); !strings.Contains(got, "Run "+networkreport.ShortID(older.ID)+" ") {
		t.Errorf("--run did not pin the run:\n%s", got)
	}
	// A host this project never had blocked is a note, not an invented cause.
	note := captureStderr(t, func() {
		if code, err := a.netDiagnostic("explain", []string{"never.example"}); code != 0 || err != nil {
			t.Fatalf("explain of an unblocked host = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(note, "nothing blocked never.example in this project's recorded runs") {
		t.Errorf("an unblocked host:\n%s", note)
	}
	// Outside a project there is no scope to guess: the cwd is not a project.
	t.Chdir(t.TempDir())
	outside := &app{cfg: &config.Config{}}
	if code, err := outside.netDiagnostic("explain", []string{"example.org"}); code != 1 || err == nil ||
		!strings.Contains(err.Error(), "searches this project's runs") {
		t.Errorf("explain outside a project = (%d, %v)", code, err)
	}
}

// The rule to paste is the normalized shape of exactly what the evidence
// named — a domain, an address, a range, a Compose service, an ICMP type — and
// never more keys than that destination needs.
func TestNetRuleYAMLIsTheSmallestPastableItem(t *testing.T) {
	for want, rule := range map[string]egress.Rule{
		"  - to: {domain: \"docs.example.com\"}\n    protocol: tls\n    ports: [443, 8443]\n": {
			To: egress.Destination{Domain: "docs.example.com"}, Protocol: "tls", Ports: []int{443, 8443}},
		"  - to: {ip: \"198.51.100.7\"}\n    protocol: tcp\n    ports: [5432]\n": {
			To: egress.Destination{IP: "198.51.100.7"}, Protocol: "tcp", Ports: []int{5432}},
		"  - to: {cidr: \"10.42.9.0/24\"}\n    protocol: udp\n    ports: [123]\n": {
			To: egress.Destination{CIDR: "10.42.9.0/24"}, Protocol: "udp", Ports: []int{123}},
		"  - to: {service: \"db\"}\n    protocol: tcp\n    ports: [5432]\n": {
			To: egress.Destination{Service: "db"}, Protocol: "tcp", Ports: []int{5432}},
		"  - to: {ip: \"10.42.8.12\"}\n    protocol: icmp\n    types: [echo-request]\n": {
			To: egress.Destination{IP: "10.42.8.12"}, Protocol: "icmp", Types: []string{"echo-request"}},
	} {
		if got := netRuleYAML(rule); got != want {
			t.Errorf("netRuleYAML:\n%s\nwant:\n%s", got, want)
		}
	}
}

// Materially different refusals of one host are shown as distinct causes with
// their counts, not silently reduced to one.
func TestExplainShowsDistinctCauses(t *testing.T) {
	port := 443
	explanation := netExplanation{RunID: netTestRun, Host: "example.org", Causes: []netExplainCause{
		{Count: 3, Explanation: networkstate.EventExplanation{Message: "no rule in this run allows this name",
			Event: networkview.Denial{ID: "a", Kind: "dns_denied", Reason: "unapproved_name", Name: "example.org", At: time.Unix(1, 0).UTC()}}},
		{Count: 1, Explanation: networkstate.EventExplanation{Message: "the TLS handshake carried no usable server name, so no domain rule could match it",
			Event: networkview.Denial{ID: "b", Kind: "tls_denied", Reason: "tls_name_missing", Name: "example.org", Port: &port, At: time.Unix(2, 0).UTC()}}},
	}}
	var b bytes.Buffer
	writeNetExplanation(&b, ui.Palette{}, time.Now(), explanation)
	for _, want := range []string{
		"\nexample.org · DNS ×3 — No rule in this run allows this name.\n",
		"\nexample.org:443 · TLS — the connection carried no usable name — The TLS handshake carried no usable server name, so no domain rule could match it.\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}

func TestOriginTextNamesWhereAGrantCameFrom(t *testing.T) {
	cases := map[string]networkstate.PolicyOrigin{
		"your allow-domain flag":                   {Kind: "operator", Name: "allow-domain"},
		"an approved project rule":                 {Kind: "project"},
		"Claude's provider access (bundle 1)":      {Kind: "provider", Provider: "claude", BundleVersion: "1"},
		"the MCP server emisar coop set up for th": {Kind: "mcp", Name: "emisar"},
	}
	for want, origin := range cases {
		if got := netOriginText(origin); !strings.Contains(got, want) {
			t.Errorf("netOriginText(%+v) = %q, want it to contain %q", origin, got, want)
		}
	}
	if got := netCheckOrigin(networkstate.PolicyExplanation{Rule: &networkstate.PolicyRule{}}); got != "a rule that run started with" {
		t.Errorf("netCheckOrigin without origins = %q", got)
	}
}
