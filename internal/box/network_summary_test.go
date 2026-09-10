package box

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

func denial(id, name, kind string, candidate bool) networkview.Denial {
	d := networkview.Denial{ID: id, Source: "guard", Sequence: 1, Basis: "observed", At: time.Unix(1, 0).UTC(),
		Kind: kind, Reason: "unapproved_name", Name: name}
	if candidate {
		d.Candidate = &networkview.Candidate{ID: id, EvidenceID: id, AppliesTo: "next_run",
			Rule: egress.Rule{To: egress.Destination{Domain: name}, Protocol: "tls", Ports: []int{443}}}
	}
	return d
}

func TestRunReportGroupsRefusalsByDestinationAndBasis(t *testing.T) {
	snapshot := networkview.Snapshot{Denials: []networkview.Denial{
		denial("a1", "example.org", "dns_denied", false),
		denial("a2", "example.org", "dns_denied", false),
		denial("a3", "example.org", "tls_denied", true),
		denial("a4", "other.example", "dns_denied", false),
	}}
	report := networkRunReport("run1", snapshot)
	want := []string{"example.org (dns) ×2", "example.org (tls)", "other.example (dns)"}
	// The count stays a number, so the loop's closing summary can add it up
	// across runs instead of parsing a rendered line.
	if report.Denials[0].Count != 2 || report.Denials[0].Destination != "example.org" || report.Denials[0].Basis != "dns" {
		t.Errorf("grouped denial = %+v, want example.org/dns counted twice", report.Denials[0])
	}
	if len(report.Denials) != len(want) {
		t.Fatalf("denial lines = %q, want %q", report.Denials, want)
	}
	for i, line := range want {
		if got := report.Denials[i].String(); got != line {
			t.Errorf("denial line %d = %q, want %q", i, got, line)
		}
	}
	// The refusal that carries a draft rule is the one worth opening, even
	// though an earlier event was retained first.
	if report.Event != "a3" {
		t.Errorf("explain pointer = %q, want the drafted event a3", report.Event)
	}
	if report.Quiet() {
		t.Error("a run with refusals must not report as quiet")
	}
}

func TestRunReportBoundsDestinationsAndCountsTheRest(t *testing.T) {
	var denials []networkview.Denial
	for i := range maxSummaryDenials + 3 {
		denials = append(denials, denial("id"+strconv.Itoa(i), "host"+strconv.Itoa(i)+".example", "dns_denied", false))
	}
	report := networkRunReport("run1", networkview.Snapshot{Denials: denials})
	if len(report.Denials) != maxSummaryDenials {
		t.Fatalf("printed %d destinations, want the %d-line bound", len(report.Denials), maxSummaryDenials)
	}
	if report.Omitted != 3 {
		t.Errorf("omitted = %d, want 3", report.Omitted)
	}
}

// A metric nobody measured is UNKNOWN. Printing 0 would claim the run sent
// nothing, which is a different statement from "no counter was retained".
func TestRunReportNeverFabricatesZeroTraffic(t *testing.T) {
	report := networkRunReport("run1", networkview.Snapshot{})
	if !strings.Contains(report.Allowed, "UNKNOWN") {
		t.Fatalf("allowed line = %q, want UNKNOWN with no counters", report.Allowed)
	}
	if !report.Quiet() {
		t.Error("a run with no refusals and no alerts must be quiet")
	}
	sent := networkview.Count(12)
	partial := networkRunReport("run1", networkview.Snapshot{Counters: &networkview.Counters{SentBytes: &sent}})
	if !strings.Contains(partial.Allowed, "sent 12 bytes") || !strings.Contains(partial.Allowed, "received UNKNOWN bytes") {
		t.Errorf("allowed line = %q, want the measured value and UNKNOWN for the rest", partial.Allowed)
	}
}

func TestRunReportSummarizesAlerts(t *testing.T) {
	snapshot := networkview.Snapshot{Alerts: []networkview.Alert{{ID: "x", Category: "denial_burst", Severity: "warning",
		State: "open", WindowMillis: 5000, Threshold: networkview.AlertThreshold{Value: 20, Unit: "denials"},
		Facts: networkview.AlertFacts{Reason: "sustained refusals"}}}}
	report := networkRunReport("run1", snapshot)
	if len(report.Alerts) != 1 {
		t.Fatalf("alerts = %q", report.Alerts)
	}
	for _, want := range []string{"denial_burst", "warning", "5000ms", "threshold 20 denials", "sustained refusals"} {
		if !strings.Contains(report.Alerts[0], want) {
			t.Errorf("alert line %q is missing %q", report.Alerts[0], want)
		}
	}
	if report.Quiet() {
		t.Error("an alert alone must break the quiet line")
	}
}

func TestInstructionNoteStatesPolicyAndTheWayToAsk(t *testing.T) {
	policy := egress.Snapshot{Mode: egress.Filtered, Grants: []egress.Grant{
		{ID: "1", Rule: egress.Rule{To: egress.Destination{Domain: "example.com"}, Protocol: "tls", Ports: []int{443}},
			Origins: []egress.Origin{{Kind: "operator", Name: "allow-domain"}}},
		{ID: "2", Rule: egress.Rule{To: egress.Destination{Domain: "api.anthropic.com"}, Protocol: "tls", Ports: []int{443}},
			Origins: []egress.Origin{{Kind: "provider", Provider: "claude"}}},
		{ID: "3", Rule: egress.Rule{To: egress.Destination{Domain: "statsig.anthropic.com"}, Protocol: "tls", Ports: []int{443}},
			Origins: []egress.Origin{{Kind: "provider", Provider: "claude"}}},
	}}
	note := networkInstructionNote(policy)
	for _, want := range []string{"# Network (coop restricted egress)", "example.com tls/443", "claude core endpoints",
		"box.egress_rules", "coop net approve"} {
		if !strings.Contains(note, want) {
			t.Errorf("instruction note is missing %q:\n%s", want, note)
		}
	}
	// A provider's maintained hostnames are the release's business; repeating
	// each one is context every turn pays for.
	if strings.Contains(note, "statsig.anthropic.com") || strings.Contains(note, "api.anthropic.com") {
		t.Errorf("note expands a provider bundle instead of summarizing it:\n%s", note)
	}
	if lines := strings.Count(strings.TrimSpace(note), "\n") + 1; lines > 12 {
		t.Errorf("instruction note is %d lines; it is context every turn pays for:\n%s", lines, note)
	}
}

func TestInstructionNoteIsOnlyForFilteredRuns(t *testing.T) {
	for _, mode := range []egress.Mode{egress.Open, egress.None, ""} {
		if note := networkInstructionNote(egress.Snapshot{Mode: mode}); note != "" {
			t.Errorf("mode %q produced a network note: %q", mode, note)
		}
	}
}

func TestInstructionNoteBoundsTheDestinationList(t *testing.T) {
	var grants []egress.Grant
	for i := range maxNoteGrants + 4 {
		name := "host" + strconv.Itoa(i) + ".example"
		grants = append(grants, egress.Grant{ID: name, Rule: egress.Rule{To: egress.Destination{Domain: name},
			Protocol: "tls", Ports: []int{443}}, Origins: []egress.Origin{{Kind: "project"}}})
	}
	note := networkInstructionNote(egress.Snapshot{Mode: egress.Filtered, Grants: grants})
	if !strings.Contains(note, "and 4 more allowed destination(s)") {
		t.Errorf("note does not bound its list:\n%s", note)
	}
}

// A refused raw packet has no destination to name, so it produces no denial row
// — but a run that hit the boundary must never print "nothing was refused".
func TestRunReportCountsRawRefusalsAsHittingTheBoundary(t *testing.T) {
	denied, protected := networkview.Count(4), networkview.Count(1)
	report := networkRunReport("run1", networkview.Snapshot{Counters: &networkview.Counters{DeniedPackets: &denied, ProtectedPackets: &protected}})
	if report.Quiet() {
		t.Fatal("a run with refused packets reported as quiet")
	}
	if report.RawPackets != 4 {
		t.Errorf("RawPackets = %d, want 4", report.RawPackets)
	}
	for _, want := range []string{"refused packets: 4", "(1 to protected addresses)", "counted, not attributed"} {
		if !strings.Contains(report.Raw, want) {
			t.Errorf("raw line %q is missing %q", report.Raw, want)
		}
	}
	// Zero refused packets is a measurement, not a boundary hit.
	zero := networkview.Count(0)
	quiet := networkRunReport("run1", networkview.Snapshot{Counters: &networkview.Counters{DeniedPackets: &zero, ProtectedPackets: &zero}})
	if !quiet.Quiet() || quiet.RawPackets != 0 {
		t.Error("a measured zero was reported as a refusal")
	}
}
