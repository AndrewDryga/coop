package networkstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"

	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkview"
)

const retainedIntegrity = "retained-evidence-only"

// Absence from a bounded detail ring does not prove that this particular event
// existed or expired. Keep that distinction in both human and machine output.
var ErrEventNotRetained = errors.New("event_not_retained: no matching event remains in this run's bounded evidence")

// PolicyExplanation is hypothetical admission, not an observed connection or
// launch capability. The owner-private policy itself never crosses this API.
type PolicyExplanation struct {
	Version           int            `json:"version"`
	Kind              string         `json:"kind"`
	RunID             string         `json:"run_id"`
	PolicyFingerprint string         `json:"policy_fingerprint"`
	Integrity         string         `json:"integrity"`
	Mode              egress.Mode    `json:"mode"`
	Domain            string         `json:"domain,omitempty"`
	Protocol          string         `json:"protocol"`
	Port              int            `json:"port"`
	Allowed           bool           `json:"allowed_by_policy"`
	Reason            string         `json:"reason"`
	Message           string         `json:"message"`
	Resolution        string         `json:"resolution"`
	CurrentPolicy     string         `json:"current_policy"`
	RuleID            string         `json:"rule_id,omitempty"`
	Rule              *PolicyTLSRule `json:"rule,omitempty"`
	Origins           []PolicyOrigin `json:"origins,omitempty"`
	Withheld          bool           `json:"destination_withheld"`
}

// Diagnostic-owned projections deliberately do not embed authority types.
// Extending a private rule or origin must not silently extend public output.
type PolicyTLSRule struct {
	To       PolicyDomain `json:"to"`
	Protocol string       `json:"protocol"`
	Ports    []int        `json:"ports"`
}

type PolicyDomain struct {
	Domain string `json:"domain"`
}

type PolicyOrigin struct {
	Kind          string        `json:"kind"`
	Name          string        `json:"name,omitempty"`
	Version       string        `json:"version,omitempty"`
	Feature       string        `json:"feature,omitempty"`
	Provider      string        `json:"provider,omitempty"`
	Client        egress.Client `json:"client,omitempty"`
	Backend       string        `json:"backend,omitempty"`
	AuthMode      string        `json:"auth_mode,omitempty"`
	BundleVersion string        `json:"bundle_version,omitempty"`
}

type EventExplanation struct {
	Version           int                `json:"version"`
	Kind              string             `json:"kind"`
	RunID             string             `json:"run_id"`
	Epoch             string             `json:"gateway_epoch"`
	PolicyFingerprint string             `json:"policy_fingerprint"`
	Integrity         string             `json:"integrity"`
	Event             networkview.Denial `json:"event"`
	Message           string             `json:"message"`
	CurrentPolicy     string             `json:"current_policy"`
	CandidateState    string             `json:"candidate_state"`
	DetailTruncated   bool               `json:"detail_truncated"`
}

func (e *Evidence) Why(runID, domain string, exportDestinations bool) (PolicyExplanation, error) {
	name, err := egress.NormalizeDomain(domain, false)
	if err != nil {
		return PolicyExplanation{}, errors.New("why requires one exact ASCII domain; the hypothetical transport is TLS on port 443")
	}
	record, err := e.Execution(runID)
	if err != nil {
		return PolicyExplanation{}, err
	}
	policy, err := e.recordedPolicy(record)
	if err != nil {
		return PolicyExplanation{}, err
	}
	decision := policy.Domain(name, 443)
	out := PolicyExplanation{Version: networkview.Version, Kind: "hypothetical", RunID: record.ID,
		PolicyFingerprint: policy.Fingerprint, Integrity: retainedIntegrity, Mode: policy.Mode,
		Protocol: "tls", Port: 443, Allowed: decision.Allowed, Reason: decision.Reason,
		Message: diagnosticReason(decision.Reason), Resolution: "not_evaluated", CurrentPolicy: "not_evaluated", Withheld: !exportDestinations}
	if exportDestinations {
		out.Domain, out.RuleID = name, decision.RuleID
		for _, grant := range policy.Grants {
			if grant.ID == decision.RuleID {
				out.Rule = &PolicyTLSRule{To: PolicyDomain{Domain: grant.Rule.To.Domain}, Protocol: grant.Rule.Protocol, Ports: slices.Clone(grant.Rule.Ports)}
				for _, origin := range grant.Origins {
					out.Origins = append(out.Origins, PolicyOrigin{Kind: origin.Kind, Name: origin.Name, Version: origin.Version,
						Feature: origin.Feature, Provider: origin.Provider, Client: origin.Client, Backend: origin.Backend,
						AuthMode: origin.AuthMode, BundleVersion: origin.BundleVersion})
				}
				break
			}
		}
	}
	return out, nil
}

// This private reader is deliberately not Store.LoadSnapshot: it cannot recover
// a key, authenticate authority, recapture policy, or hand a launcher a permit.
func (e *Evidence) recordedPolicy(record Execution) (egress.Snapshot, error) {
	bad := errors.New("captured policy evidence is unavailable or invalid; no current policy was substituted")
	data, err := e.files.read("snapshot-"+record.Snapshot.PolicyFingerprint+".json", maxPrivateRecordBytes)
	if err != nil {
		return egress.Snapshot{}, bad
	}
	var policy egress.Snapshot
	if strictJSON(data, &policy) != nil || policy.ValidateRecorded() != nil ||
		policy.Fingerprint != record.Snapshot.PolicyFingerprint || policy.Scope != record.Scope || policy.Mode != record.Snapshot.Mode {
		return egress.Snapshot{}, bad
	}
	canonical, err := json.Marshal(policy)
	if err != nil || !bytes.Equal(data, canonical) {
		return egress.Snapshot{}, bad
	}
	return policy, nil
}

func (e *Evidence) Explain(runID, eventID string, exportDestinations bool) (EventExplanation, error) {
	if !lowerHex(eventID, 32) {
		return EventExplanation{}, errors.New("invalid network evidence id")
	}
	record, err := e.Execution(runID)
	if err != nil {
		return EventExplanation{}, err
	}
	var found *networkview.Denial
	for _, denial := range record.Snapshot.Denials {
		if denial.ID == eventID {
			if found != nil {
				return EventExplanation{}, errors.New("ambiguous retained network evidence id")
			}
			found = &denial
		}
	}
	if found == nil {
		return EventExplanation{}, ErrEventNotRetained
	}
	if err := validateDiagnosticDenial(*found, record.Snapshot.PolicyFingerprint); err != nil {
		return EventExplanation{}, err
	}
	// The shared allowlist owns the destination/candidate privacy projection.
	projected := (networkview.Snapshot{Denials: []networkview.Denial{*found}}).Project(exportDestinations).Denials[0]
	message := diagnosticReason(projected.Reason)
	if message == "This version cannot explain the retained reason code." {
		projected.Reason = "unknown_reason"
	}
	state := "none"
	if found.Candidate != nil {
		state = "draft_not_approved"
		if !exportDestinations {
			state = "withheld"
		}
	}
	return EventExplanation{Version: networkview.Version, Kind: "observed", RunID: record.ID, Epoch: record.Epoch,
		PolicyFingerprint: record.Snapshot.PolicyFingerprint, Integrity: retainedIntegrity, Event: projected,
		Message: message, CurrentPolicy: "not_evaluated", CandidateState: state, DetailTruncated: record.Snapshot.Loss.DetailTruncated}, nil
}

func validateDiagnosticDenial(d networkview.Denial, fingerprint string) error {
	bad := errors.New("invalid retained network decision")
	if d.Sequence == 0 || d.At.IsZero() || d.DestinationID != "" && !lowerHex(d.DestinationID, 32) {
		return bad
	}
	switch d.Source {
	case "guard":
		if d.Basis != "observed" || !slices.Contains([]string{"tls_denied", "dns_denied", "admission_failed"}, d.Kind) {
			return bad
		}
		if d.Kind == "tls_denied" {
			if d.Port == nil || *d.Port != 443 {
				return bad
			}
		} else if d.Port != nil {
			return bad // DNS and availability evidence did not observe a transport port.
		}
	case "socket-inventory":
		if d.Basis != "observed-socket-and-captured-policy" || d.Kind != "direct_tcp_attempt" || d.Port == nil || *d.Port < 1 || *d.Port > 65535 {
			return bad
		}
	default:
		return bad
	}
	if d.Name != "" {
		name, err := egress.NormalizeDomain(d.Name, false)
		if err != nil || name != d.Name {
			return bad
		}
	}
	if d.Peer != "" {
		peer, err := netip.ParseAddrPort(d.Peer)
		if err != nil || peer.String() != d.Peer || d.Port != nil && int(peer.Port()) != *d.Port {
			return bad
		}
	}
	if c := d.Candidate; c != nil {
		expected := egress.Rule{To: egress.Destination{Domain: d.Name}, Protocol: "tls", Ports: []int{443}}
		if d.Source != "guard" || d.Kind != "tls_denied" || d.Reason != "unapproved_name" || d.Name == "" ||
			!lowerHex(c.ID, 32) || c.EvidenceID != d.ID || c.PolicyFingerprint != fingerprint || c.AppliesTo != "next_run" || !equalJSON(c.Rule, expected) {
			return bad
		}
	}
	return nil
}

// Fixed prose never reflects parser errors, DNS payloads or terminal controls.
func diagnosticReason(reason string) string {
	switch reason {
	case "open":
		return "The captured mode is open; this policy check does not establish reachability or isolation."
	case "rule_allowed":
		return "The captured policy permits this TLS name and port; DNS safety and reachability were not tested."
	case "unapproved_name":
		return "The name is outside the captured TLS grants. A reviewed change can apply only to a new run."
	case "protected_destination", "unsafe_dns_answer":
		return "The destination is protected or its DNS answer is unsafe. Adding a name grant cannot bypass this boundary."
	case "protocol_not_allowed", "port_not_allowed", "fixed_egress_policy":
		return "The traffic does not fit the captured transport policy. A sampled socket is not a correlated packet verdict."
	case "tls_name_missing", "tls_name_invalid":
		return "The TLS handshake did not provide a usable server name. A domain rule cannot authorize unnamed traffic."
	case "tls_ech_unsupported":
		return "Encrypted ClientHello prevents supported name inspection; this traffic is not supported by the captured enforcer."
	case "tls_malformed", "tls_hello_too_large", "tls_inspection_timeout", "tls_inspection_unavailable":
		return "The guard could not safely inspect a bounded TLS handshake. This is not a missing domain grant."
	case "dns_name_invalid", "dns_query_invalid", "dns_answer_invalid", "dns_cname_limit", "dns_answer_limit", "dns_upstream_invalid":
		return "DNS validation failed. The evidence does not establish a destination transport or port to allow."
	case "dns_unavailable", "dns_no_address", "dns_ttl_expired", "dns_capacity_exceeded":
		return "The resolver could not supply a currently usable address. Repair resolver health rather than adding a grant."
	case "gateway_connection_capacity", "gateway_lease_capacity", "gateway_lease_refused", "gateway_unavailable", "enforcement_unavailable", "clock_unavailable", "observation_unavailable", "upstream_unreachable", "unsupported_capability":
		return "Admission or observation was unavailable. This is not evidence that a broader rule is needed."
	default:
		return "This version cannot explain the retained reason code."
	}
}
