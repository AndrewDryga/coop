package networkgateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/networkview"
)

func TestCandidateNeedsExactObservedTLSNameAndPort(t *testing.T) {
	c, _ := collectorFixture(t)
	port := 443
	valid := networkview.Denial{ID: "evidence", Source: "guard", Kind: "tls_denied", Reason: "unapproved_name", Name: "blocked.example.net", Port: &port}
	for _, scenario := range []string{"valid", "dns", "socket", "protected", "unsafe", "ech", "missing-name", "wildcard", "noncanonical", "control", "url", "missing-port", "other-port", "missing-evidence", "already-allowed"} {
		t.Run(scenario, func(t *testing.T) {
			d := valid
			switch scenario {
			case "dns":
				d.Kind = "dns_denied"
			case "socket":
				d.Source = "socket-inventory"
			case "protected":
				d.Reason = "protected_destination"
			case "unsafe":
				d.Reason = "unsafe_dns_answer"
			case "ech":
				d.Reason = "tls_ech_unsupported"
			case "missing-name":
				d.Name = ""
			case "wildcard":
				d.Name = "*.example.com"
			case "noncanonical":
				d.Name = "API.Example.COM."
			case "control":
				d.Name = "\x1b[0m.example.com"
			case "url":
				d.Name = "https://api.example.com"
			case "missing-port":
				d.Port = nil
			case "other-port":
				other := 8443
				d.Port = &other
			case "missing-evidence":
				d.ID = ""
			case "already-allowed":
				d.Name = "api.example.com"
			}
			candidate := c.candidate(d)
			if (candidate != nil) != (scenario == "valid") {
				t.Fatalf("wrong suggestion for %s: %+v", scenario, candidate)
			}
			if candidate != nil && (candidate.Rule.To.Domain != d.Name || candidate.Rule.Protocol != "tls" || len(candidate.Rule.Ports) != 1 || candidate.Rule.Ports[0] != 443 || candidate.AppliesTo != "next_run") {
				t.Fatal("suggestion widened observed evidence or applied to live run")
			}
		})
	}
	first := c.candidate(valid)
	if c.candidate(valid).ID != first.ID {
		t.Fatal("same candidate evidence changed identity")
	}
	valid.ID = "different-evidence"
	if c.candidate(valid).ID == first.ID {
		t.Fatal("candidate not bound to evidence")
	}
	valid.ID = "evidence"
	c.identity.PolicyFingerprint = strings.Repeat("f", 64)
	if c.candidate(valid).ID == first.ID {
		t.Fatal("candidate not bound to captured authority")
	}
}

func TestCandidateIngestIsBoundedPrivateAndNeverChangesPolicy(t *testing.T) {
	c, now := collectorFixture(t)
	for i := range MaxDenialDetails + 1 {
		e := GuardEvent{Sequence: uint64(i + 1), BootAt: *now, Kind: "tls_denied", Reason: "unapproved_name", Name: "blocked.example.net", Port: 443}
		c.ingest([]GuardEvent{e}, GuardTotals{Sequence: e.Sequence, DeniedTLS: e.Sequence}, nil, EnvoyTotals{})
	}
	publishFixture(c, *now, nil)
	s := c.Snapshot()
	if len(s.Denials) != MaxDenialDetails || s.Denials[0].Candidate == nil || !s.Loss.DetailTruncated {
		t.Fatal("candidate retention escaped denial bounds or candidate was missing")
	}
	if c.resolver.policy.Domain("blocked.example.net", 443).Allowed {
		t.Fatal("candidate changed live policy")
	}
	private := s.Project(false)
	encoded, _ := json.Marshal(private)
	if strings.Contains(string(encoded), "blocked.example.net") || strings.Contains(string(encoded), "candidate") || !private.Denials[0].Withheld {
		t.Fatal("candidate disclosed private destination remotely")
	}
	s.Denials[0].Candidate.Rule.Ports[0] = 1
	if c.Snapshot().Denials[0].Candidate.Rule.Ports[0] != 443 {
		t.Fatal("caller mutated retained candidate")
	}
}
