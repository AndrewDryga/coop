package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/networkview"
	"github.com/AndrewDryga/coop/internal/ui"
)

const netTestEventID = "0123456789abcdef0123456789abcdef"

func TestDiagnosticArgsRequireAQueryAndARun(t *testing.T) {
	opts, err := parseNetDiagnosticArgs("why", []string{"Docs.Example.COM", "--run", "abc", "--json"})
	if err != nil {
		t.Fatalf("parseNetDiagnosticArgs: %v", err)
	}
	if opts.query != "docs.example.com" || opts.run != "abc" || !opts.json {
		t.Errorf("parsed = %+v", opts)
	}
	if _, err := parseNetDiagnosticArgs("why", []string{"example.com"}); err == nil {
		t.Error("why without --run was accepted")
	}
	if _, err := parseNetDiagnosticArgs("why", []string{"--run", "abc"}); err == nil {
		t.Error("why without a domain was accepted")
	}
	// why takes a NAME, explain takes an EVENT: neither accepts the other.
	if _, err := parseNetDiagnosticArgs("why", []string{"https://example.com/x", "--run", "abc"}); err == nil {
		t.Error("why accepted a URL")
	}
	if _, err := parseNetDiagnosticArgs("explain", []string{"example.com", "--run", "abc"}); err == nil {
		t.Error("explain accepted a domain instead of an evidence id")
	}
	if _, err := parseNetDiagnosticArgs("explain", []string{netTestEventID, "--run", "abc", "--redact"}); err == nil {
		t.Error("an unknown diagnostic flag was accepted")
	}
}

func TestWhyIsHypotheticalAndNamesItsProvenance(t *testing.T) {
	result := networkstate.PolicyExplanation{Version: networkview.Version, Kind: "hypothetical", RunID: "run1",
		PolicyFingerprint: "fp", Mode: egress.Filtered, Domain: "example.com", Protocol: "tls", Port: 443,
		Allowed: true, Reason: "rule_allowed", Message: "permitted", Resolution: "not_evaluated",
		Rule:    &networkstate.PolicyTLSRule{To: networkstate.PolicyDomain{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}},
		Origins: []networkstate.PolicyOrigin{{Kind: "operator", Name: "allow-domain"}},
	}
	var b bytes.Buffer
	writeNetWhy(&b, ui.Palette{}, result)
	text := b.String()
	for _, want := range []string{"policy check (hypothetical)", "example.com tls/443", "yes —",
		"granted by you (allow-domain)", "No DNS query, probe or connection was made"} {
		if !strings.Contains(text, want) {
			t.Errorf("why view is missing %q:\n%s", want, text)
		}
	}
}

func TestWhyWithheldDestinationIsNotPrinted(t *testing.T) {
	var b bytes.Buffer
	writeNetWhy(&b, ui.Palette{}, networkstate.PolicyExplanation{RunID: "run1", Protocol: "tls", Port: 443,
		Domain: "", Withheld: true, Reason: "unapproved_name"})
	if !strings.Contains(b.String(), "withheld tls/443") {
		t.Errorf("withheld destination view:\n%s", b.String())
	}
}

func TestExplainRendersTheDraftRuleAsADraft(t *testing.T) {
	port := 443
	result := networkstate.EventExplanation{RunID: "run1", Epoch: "e", PolicyFingerprint: "fp", CandidateState: "draft_not_approved",
		Message: "outside the captured grants",
		Event: networkview.Denial{ID: netTestEventID, Source: "guard", Basis: "observed", Kind: "tls_denied",
			Reason: "unapproved_name", Name: "example.org", Port: &port, At: time.Unix(1, 0).UTC(),
			Candidate: &networkview.Candidate{ID: "c1", EvidenceID: netTestEventID, AppliesTo: "next_run",
				Rule: egress.Rule{To: egress.Destination{Domain: "example.org"}, Protocol: "tls", Ports: []int{443}}}},
	}
	var b bytes.Buffer
	writeNetExplanation(&b, ui.Palette{}, result)
	text := b.String()
	for _, want := range []string{"refused (observed)", "example.org", "tls_denied", "draft, not a grant",
		"egress_rules:", `domain: "example.org"`, "coop net approve", "applies to NEW runs",
		"not that the access is needed", "today's policy was not substituted"} {
		if !strings.Contains(text, want) {
			t.Errorf("explain view is missing %q:\n%s", want, text)
		}
	}
}

// The two no-draft cases mean different things and must not be blurred: a
// boundary no rule can cross, versus evidence too thin to name a transport.
func TestExplainDistinguishesAnImmutableBoundaryFromThinEvidence(t *testing.T) {
	render := func(reason string) string {
		var b bytes.Buffer
		writeNetExplanation(&b, ui.Palette{}, networkstate.EventExplanation{RunID: "r", CandidateState: "none",
			Event: networkview.Denial{ID: netTestEventID, Kind: "dns_denied", Reason: reason, At: time.Unix(1, 0).UTC()}})
		return b.String()
	}
	if !strings.Contains(render("protected_destination"), "No rule can grant this") {
		t.Errorf("a protected destination was not called one:\n%s", render("protected_destination"))
	}
	thin := render("unapproved_name")
	if !strings.Contains(thin, "does not establish a transport or port") {
		t.Errorf("a DNS refusal must not claim TLS/443 was observed:\n%s", thin)
	}
	if strings.Contains(thin, "No rule can grant this") {
		t.Errorf("a policy gap was described as an immutable boundary:\n%s", thin)
	}
	if !strings.Contains(render("upstream_unreachable"), "availability failure") {
		t.Errorf("an availability failure was described as a denial")
	}
}

func TestOriginTextNamesWhereAGrantCameFrom(t *testing.T) {
	cases := map[string]networkstate.PolicyOrigin{
		"granted by you (allow-domain)":              {Kind: "operator", Name: "allow-domain"},
		"requested by the project and approved":      {Kind: "project"},
		"core endpoint for claude":                   {Kind: "provider", Provider: "claude", BundleVersion: "1"},
		"MCP server emisar coop configured for this": {Kind: "mcp", Name: "emisar"},
	}
	for want, origin := range cases {
		if got := netOriginText(origin); !strings.Contains(got, want) {
			t.Errorf("netOriginText(%+v) = %q, want it to contain %q", origin, got, want)
		}
	}
}
