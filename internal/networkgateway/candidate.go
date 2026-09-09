package networkgateway

import (
	"encoding/json"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

// A draft records sufficient observed traffic, not necessity or authorization.
// Only trusted SNI inspection establishes both an exact name and TLS443. DNS,
// packet counters and raw socket attempts cannot supply that missing evidence.
func (c *Collector) candidate(d networkview.Denial) *networkview.Candidate {
	if d.Source != "guard" || d.Kind != "tls_denied" || d.Reason != "unapproved_name" || d.Port == nil || *d.Port != 443 || d.ID == "" {
		return nil
	}
	name, err := egress.NormalizeDomain(d.Name, false)
	if err != nil || name != d.Name {
		return nil
	}
	if decision := c.resolver.policy.Domain(name, 443); decision.Allowed || decision.Reason != "unapproved_name" {
		return nil
	}
	rules, err := egress.NormalizeRules([]egress.Rule{{To: egress.Destination{Domain: name}, Protocol: "tls", Ports: []int{443}}})
	if err != nil {
		return nil
	}
	rule := rules[0]
	encoded, _ := json.Marshal(rule)
	return &networkview.Candidate{ID: c.opaque("candidate", d.ID+"\x00"+c.identity.PolicyFingerprint+"\x00"+string(encoded)),
		EvidenceID: d.ID, PolicyFingerprint: c.identity.PolicyFingerprint, Rule: rule, AppliesTo: "next_run"}
}
