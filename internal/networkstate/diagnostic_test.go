package networkstate

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

func diagnosticFixture(t *testing.T) (*Store, *Evidence, Execution) {
	t.Helper()
	s, old := executionFixture(t)
	mode := egress.Filtered
	policy, err := s.Admit(old.Project, Admission{InvocationMode: &mode, Operator: []egress.Input{{
		Rules: []egress.Rule{{To: egress.Destination{Domain: "api.example.com"}, Protocol: "tls", Ports: []int{443}},
			{To: egress.Destination{Domain: "*.example.net"}, Protocol: "tls", Ports: []int{443}}},
		Origin: egress.Origin{Kind: "operator", Name: "private-origin"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := executionSmoke(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: old.Project, PolicyFingerprint: policy.Fingerprint,
		Runtime: "docker", DaemonID: "fixture-daemon", Endpoint: "unix:///fixture.sock",
		GatewayImage: old.GatewayImage, ClientImage: old.ClientImage})
	if err != nil {
		t.Fatal(err)
	}
	return s, fixtureEvidence(t, s), record
}

func TestNetworkWhyUsesOnlyRetainedPolicyAfterKeyAndProjectLoss(t *testing.T) {
	s, evidence, record := diagnosticFixture(t)
	// A new approval is not the captured run's policy, even when it denies all.
	if err := approve(s, record.Project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Path(), "owner.key")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(record.Project); err != nil {
		t.Fatal(err)
	}
	before := inventory(t, s)
	for _, row := range []struct {
		domain  string
		allowed bool
	}{{"API.EXAMPLE.COM.", true}, {"child.example.net", true}, {"example.net", false}, {"denied.example.org", false}} {
		got, err := evidence.Why(record.ID, tlsQuery(row.domain), true)
		if err != nil || got.Allowed != row.allowed || got.Kind != "hypothetical" || got.Resolution != "not_evaluated" || got.CurrentPolicy != "not_evaluated" || got.Integrity != retainedIntegrity {
			t.Fatalf("wrong captured decision for %s: %+v %v", row.domain, got, err)
		}
		if row.allowed && (got.Rule == nil || len(got.Origins) != 1) {
			t.Fatal("matched captured provenance missing")
		}
	}
	redacted, err := evidence.Why(record.ID, tlsQuery("api.example.com"), false)
	if err != nil || !redacted.Withheld || redacted.Domain != "" || redacted.RuleID != "" || redacted.Rule != nil || len(redacted.Origins) != 0 {
		t.Fatal("hypothetical projection leaked destinations", redacted, err)
	}
	data, _ := json.Marshal(redacted)
	for _, secret := range []string{"api.example.com", "private-origin", record.Project, record.Endpoint} {
		if strings.Contains(string(data), secret) {
			t.Fatal("redacted explanation leaked private data")
		}
	}
	if !reflect.DeepEqual(before, inventory(t, s)) {
		t.Fatal("diagnostics wrote or regenerated state")
	}
}

func TestNetworkWhyRefusesCorruptOrSubstitutedPolicy(t *testing.T) {
	for _, change := range []string{"scope", "fingerprint", "mode", "duplicate-key", "missing"} {
		t.Run(change, func(t *testing.T) {
			s, evidence, record := diagnosticFixture(t)
			path := filepath.Join(s.Path(), "snapshot-"+record.Snapshot.PolicyFingerprint+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var policy egress.Snapshot
			if err := json.Unmarshal(data, &policy); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "scope":
				policy.Scope = strings.Repeat("f", 64)
			case "fingerprint":
				policy.Fingerprint = strings.Repeat("f", 64)
			case "mode":
				policy.Mode = egress.Open
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if change != "missing" {
				data, _ = json.Marshal(policy)
				if change == "duplicate-key" {
					data = append([]byte(`{"version":1,`), data[1:]...)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := evidence.Why(record.ID, tlsQuery("api.example.com"), true); err == nil {
				t.Fatal("substituted policy evidence accepted")
			}
		})
	}
}

func tlsQuery(domain string) PolicyQuery {
	return PolicyQuery{Domain: domain, Protocol: "tls", Port: 443}
}

func TestNetworkExplainPreservesObservedFactsAndDraftBoundary(t *testing.T) {
	s, evidence, record := diagnosticFixture(t)
	port := 443
	id := strings.Repeat("d", 32)
	snapshot := record.Snapshot
	snapshot.Sequence, snapshot.AsOf = 1, time.Now().UTC()
	snapshot.Denials = []networkview.Denial{{ID: id, Source: "guard", Sequence: 1, Basis: "observed", Kind: "tls_denied",
		Reason: "unapproved_name", Name: "denied.example.org", Service: "web", Port: &port, At: snapshot.AsOf,
		Candidate: &networkview.Candidate{ID: strings.Repeat("c", 32), EvidenceID: id, PolicyFingerprint: snapshot.PolicyFingerprint,
			Rule: egress.Rule{To: egress.Destination{Domain: "denied.example.org"}, Protocol: "tls", Ports: []int{443}}, AppliesTo: "next_run"}}}
	if _, err := s.AcceptSnapshot(context.Background(), record.ID, record.Revision, snapshot); err != nil {
		t.Fatal(err)
	}
	got, err := evidence.Explain(record.ID, id, true)
	if err != nil || got.CandidateState != "draft_not_approved" || got.Event.Candidate == nil || got.Event.Service != "web" || got.Kind != "observed" || got.Event.At != snapshot.AsOf || got.PolicyFingerprint != snapshot.PolicyFingerprint {
		t.Fatal("observed explanation lost its binding", got, err)
	}
	redacted, err := evidence.Explain(record.ID, id, false)
	data, _ := json.Marshal(redacted)
	if err != nil || redacted.CandidateState != "withheld" || redacted.Event.Candidate != nil || redacted.Event.Service != "web" || strings.Contains(string(data), "denied.example.org") {
		t.Fatal("redacted explanation leaked a draft", err)
	}
	if _, err := evidence.Explain(record.ID, strings.Repeat("e", 32), true); !errors.Is(err, ErrEventNotRetained) {
		t.Fatal("missing detail invented an explanation", err)
	}
	// Recorded event explanations need neither a current key nor retained policy detail.
	if err := os.Remove(filepath.Join(s.Path(), "snapshot-"+snapshot.PolicyFingerprint+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.Explain(record.ID, id, true); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"dns", "protected", "evidence", "policy", "wildcard", "port"} {
		copy := snapshot.Project(true).Denials[0]
		switch change {
		case "dns":
			copy.Kind, copy.Port = "dns_denied", nil
		case "protected":
			copy.Reason = "protected_destination"
		case "evidence":
			copy.Candidate.EvidenceID = strings.Repeat("e", 32)
		case "policy":
			copy.Candidate.PolicyFingerprint = strings.Repeat("f", 64)
		case "wildcard":
			copy.Candidate.Rule.To.Domain = "*.example.org"
		case "port":
			copy.Candidate.Rule.Ports = []int{8443}
		}
		if validateDiagnosticDenial(copy, snapshot.PolicyFingerprint) == nil {
			t.Fatalf("accepted invalid %s draft", change)
		}
	}
	dns := snapshot.Project(true).Denials[0]
	dns.Kind, dns.Port, dns.Candidate = "dns_denied", nil, nil
	if err := validateDiagnosticDenial(dns, snapshot.PolicyFingerprint); err != nil || dns.Port != nil {
		t.Fatal("DNS-only evidence invented a transport", err)
	}
}

func TestNetworkWhyRawAddressUsesRetainedPolicyAndRedacts(t *testing.T) {
	s, old := executionFixture(t)
	mode := egress.Filtered
	policy, err := s.Admit(old.Project, Admission{InvocationMode: &mode, Operator: []egress.Input{{Rules: []egress.Rule{
		{To: egress.Destination{CIDR: "10.0.0.0/8"}, Protocol: "tcp", Ports: []int{5432}},
		{To: egress.Destination{CIDR: "10.0.0.0/8"}, Protocol: "udp", Ports: []int{5353}},
		{To: egress.Destination{CIDR: "10.0.0.0/8"}, Protocol: "icmp", Types: []string{"echo-request"}},
	}, Origin: egress.Origin{Kind: "operator", Name: "private-raw-origin"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var records []Execution
	for _, protected := range [][]netip.Prefix{nil, {netip.MustParsePrefix("10.42.0.0/16")}} {
		record, err := executionSmoke(t, s).CreateExecution(context.Background(), ExecutionSpec{Project: old.Project, PolicyFingerprint: policy.Fingerprint, Runtime: "docker", DaemonID: "fixture-daemon", Endpoint: "unix:///fixture.sock", GatewayImage: old.GatewayImage, ClientImage: old.ClientImage, Protected: protected})
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	evidence := fixtureEvidence(t, s)
	if err := approve(s, old.Project, egress.None, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Path(), "owner.key")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(old.Project); err != nil {
		t.Fatal(err)
	}
	before := inventory(t, s)
	for _, record := range records {
		scope := "not-retained"
		if len(record.Protected) != 0 {
			scope = "run-host-inventory"
		}
		for _, row := range []struct {
			address, protocol string
			port              int
			allowed           bool
			reason            string
		}{
			{"10.1.2.3", "tcp", 5432, true, "rule_allowed"}, {"10.1.2.3", "udp", 5353, true, "rule_allowed"}, {"10.1.2.3", "icmp", 0, true, "rule_allowed"},
			{"10.1.2.3", "tcp", 5433, false, "protocol_not_allowed"}, {"127.0.0.1", "tcp", 5432, false, "protected_destination"},
		} {
			q := PolicyQuery{Address: netip.MustParseAddr(row.address), Protocol: row.protocol, Port: row.port}
			got, err := evidence.Why(record.ID, q, true)
			if err != nil || got.Allowed != row.allowed || got.Reason != row.reason || got.Peer != row.address || got.ProtectedScope != scope || got.Protocol != row.protocol || got.Port != row.port || got.PolicyFingerprint != policy.Fingerprint || got.CurrentPolicy != "not_evaluated" || got.Integrity != retainedIntegrity {
				t.Fatalf("raw decision: %+v %v", got, err)
			}
			if row.allowed && (got.Rule == nil || got.Rule.To.CIDR != "10.0.0.0/8" || got.Rule.Protocol != row.protocol || len(got.Origins) != 1 || got.Origins[0].Name != "private-raw-origin") {
				t.Fatalf("missing provenance: %+v", got)
			}
			redacted, err := evidence.Why(record.ID, q, false)
			if err != nil || !redacted.Withheld || redacted.Peer != "" || redacted.Domain != "" || redacted.RuleID != "" || redacted.Rule != nil || len(redacted.Origins) != 0 || redacted.Allowed != got.Allowed || redacted.Reason != got.Reason {
				t.Fatalf("raw disclosure: %+v %v", redacted, err)
			}
		}
		got, err := evidence.Why(record.ID, PolicyQuery{Address: netip.MustParseAddr("10.42.1.1"), Protocol: "tcp", Port: 5432}, true)
		if err != nil || got.Allowed != (len(record.Protected) == 0) || len(record.Protected) != 0 && got.Reason != "protected_destination" {
			t.Fatalf("retained host protection: %+v %v", got, err)
		}
	}
	if !reflect.DeepEqual(before, inventory(t, s)) {
		t.Fatal("diagnostics wrote state")
	}
}

func TestPolicyQueryRejectsAmbiguousRawAddress(t *testing.T) {
	for _, query := range []PolicyQuery{
		{Domain: "example.com", Address: netip.MustParseAddr("10.1.2.3"), Protocol: "tcp", Port: 22},
		{Address: netip.MustParseAddr("10.1.2.3"), Protocol: "unknown", Port: 22},
		{Address: netip.MustParseAddr("10.1.2.3"), Protocol: "icmp", Port: 22},
		{Address: netip.MustParseAddr("::ffff:10.1.2.3"), Protocol: "tcp", Port: 22},
		{Address: netip.MustParseAddr("fe80::1%en0"), Protocol: "tcp", Port: 22},
		{Address: netip.MustParseAddr("10.1.2.3"), Protocol: "tcp", Port: 0},
		{Address: netip.MustParseAddr("10.1.2.3"), Protocol: "udp", Port: 65536},
	} {
		if err := query.Validate(); err == nil {
			t.Errorf("accepted query: %+v", query)
		}
	}
}
