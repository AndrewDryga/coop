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
var ErrEventNotRetained = errors.New("that refusal is no longer recorded for this run — 'coop net inspect <run>' lists the ones that are (event_not_retained)")

// PolicyExplanation is hypothetical admission, not an observed connection or
// launch capability. The owner-private policy itself never crosses this API.
type PolicyExplanation struct {
	Version           int         `json:"version"`
	Kind              string      `json:"kind"`
	RunID             string      `json:"run_id"`
	PolicyFingerprint string      `json:"policy_fingerprint"`
	Integrity         string      `json:"integrity"`
	Mode              egress.Mode `json:"mode"`
	Domain            string      `json:"domain,omitempty"`
	Peer              string      `json:"peer,omitempty"`
	Protocol          string      `json:"protocol"`
	Port              int         `json:"port,omitempty"`
	// ProtectedScope says whether this run's host address inventory was
	// available to the check. "not-retained" means the permanent denials were
	// evaluated from the fixed ranges alone, so this verdict is narrower.
	ProtectedScope string         `json:"protected_scope,omitempty"`
	Allowed        bool           `json:"allowed_by_policy"`
	Reason         string         `json:"reason"`
	Message        string         `json:"message"`
	Resolution     string         `json:"resolution"`
	CurrentPolicy  string         `json:"current_policy"`
	RuleID         string         `json:"rule_id,omitempty"`
	Rule           *PolicyRule    `json:"rule,omitempty"`
	Origins        []PolicyOrigin `json:"origins,omitempty"`
	Withheld       bool           `json:"destination_withheld"`
}

// PolicyQuery is the hypothetical this check evaluates. Exactly one of Domain
// or Address is set. A domain is TLS on the port asked about, 443 unless the
// operator names another. An address needs an explicit transport: a bare IP
// does not imply TLS, and guessing one would answer a question nobody asked.
type PolicyQuery struct {
	Domain   string
	Address  netip.Addr
	Protocol string
	Port     int
}

// Validate is the one place a hypothetical is checked, so the CLI refuses a
// nonsense transport before opening any evidence.
func (q PolicyQuery) Validate() error {
	invalid := errors.New("coop net why takes one exact domain (checked as TLS on 443, unless --port says otherwise), or one IPv4 address with --protocol tcp|udp --port <n>, or --icmp")
	switch {
	case q.Domain != "" && q.Address.IsValid():
		return invalid
	case q.Domain != "":
		if q.Protocol != "tls" || q.Port < 1 || q.Port > 65535 {
			return invalid
		}
	case !q.Address.IsValid() || q.Address.Is4In6() || q.Address.Zone() != "":
		return invalid
	case q.Protocol == "icmp":
		if q.Port != 0 || !q.Address.Is4() {
			return invalid
		}
	case q.Protocol == "tcp" || q.Protocol == "udp":
		if q.Port < 1 || q.Port > 65535 {
			return invalid
		}
	default:
		return invalid
	}
	return nil
}

// Diagnostic-owned projections deliberately do not embed authority types.
// Extending a private rule or origin must not silently extend public output.
type PolicyRule struct {
	To       PolicyDestination `json:"to"`
	Protocol string            `json:"protocol"`
	Ports    []int             `json:"ports,omitempty"`
	Types    []string          `json:"types,omitempty"`
}

type PolicyDestination struct {
	Domain  string `json:"domain,omitempty"`
	CIDR    string `json:"cidr,omitempty"`
	Service string `json:"service,omitempty"`
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

func (e *Evidence) Why(runID string, query PolicyQuery, exportDestinations bool) (PolicyExplanation, error) {
	if err := query.Validate(); err != nil {
		return PolicyExplanation{}, err
	}
	name := ""
	if query.Domain != "" {
		var err error
		if name, err = egress.NormalizeDomain(query.Domain, false); err != nil {
			return PolicyExplanation{}, errors.New("coop net why takes one exact domain, checked as TLS on the port you asked about")
		}
	}
	record, err := e.Execution(runID)
	if err != nil {
		return PolicyExplanation{}, err
	}
	policy, err := e.recordedPolicy(record)
	if err != nil {
		return PolicyExplanation{}, err
	}
	decision := policy.Domain(name, query.Port)
	if name == "" {
		// Echo-request is the one qualified ICMP message, so the hypothetical
		// uses exactly it rather than inventing a type the grammar would refuse.
		kind := 0
		if query.Protocol == "icmp" {
			kind = 8
		}
		decision = policy.Address(query.Address, query.Protocol, query.Port, kind, 0, record.Protected)
	}
	out := PolicyExplanation{Version: networkview.Version, Kind: "hypothetical", RunID: record.ID,
		PolicyFingerprint: policy.Fingerprint, Integrity: retainedIntegrity, Mode: policy.Mode,
		Protocol: query.Protocol, Port: query.Port, Allowed: decision.Allowed, Reason: decision.Reason,
		Message: diagnosticReason(decision.Reason), Resolution: "not_evaluated", CurrentPolicy: "not_evaluated", Withheld: !exportDestinations}
	if name == "" && decision.Reason == "rule_allowed" {
		out.Message = "a rule in this run allows this address, transport and port; nothing was sent, so this says nothing about whether it was up"
	}
	if name == "" {
		out.ProtectedScope = "run-host-inventory"
		if len(record.Protected) == 0 {
			out.ProtectedScope = "not-retained"
		}
	}
	if !exportDestinations {
		return out, nil
	}
	out.RuleID = decision.RuleID
	if name != "" {
		out.Domain = name
	} else {
		out.Peer = query.Address.String()
	}
	for _, grant := range policy.Grants {
		if grant.ID != decision.RuleID {
			continue
		}
		r := grant.Rule
		out.Rule = &PolicyRule{To: PolicyDestination{Domain: r.To.Domain, CIDR: r.To.CIDR, Service: r.To.Service},
			Protocol: r.Protocol, Ports: slices.Clone(r.Ports), Types: slices.Clone(r.Types)}
		for _, origin := range grant.Origins {
			out.Origins = append(out.Origins, PolicyOrigin{Kind: origin.Kind, Name: origin.Name, Version: origin.Version,
				Feature: origin.Feature, Provider: origin.Provider, Client: origin.Client, Backend: origin.Backend,
				AuthMode: origin.AuthMode, BundleVersion: origin.BundleVersion})
		}
		break
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
			// The port is the kernel's redirect record, so a refused attempt the
			// capture chain delivered has one — and an attempt whose record could
			// not be read has none at all rather than an invented 443.
			if d.Port != nil && (*d.Port < 1 || *d.Port > 65535) {
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
		if d.Port == nil {
			return bad // a draft names the observed port, so it needs one
		}
		expected := egress.Rule{To: egress.Destination{Domain: d.Name}, Protocol: "tls", Ports: []int{*d.Port}}
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
		return "this run was not filtered, so everything was reachable"
	case "rule_allowed":
		return "a rule in this run allows this name and port; whether the host answered is another question"
	case "unapproved_name":
		return "no rule in this run allows this name — a rule you approve now applies to the next run"
	case "protected_destination", "unsafe_dns_answer":
		return "this destination is protected, or the DNS answer pointed somewhere unsafe — no rule can allow it"
	case "protocol_not_allowed", "port_not_allowed", "fixed_egress_policy":
		return "no rule in this run allows that protocol and port"
	case "tls_name_missing", "tls_name_invalid":
		return "the TLS handshake carried no usable server name, so no domain rule could match it"
	case "tls_ech_unsupported":
		return "Encrypted ClientHello hides the server name, so a filtered box cannot allow this connection"
	case "tls_malformed", "tls_hello_too_large", "tls_inspection_timeout", "tls_inspection_unavailable":
		return "the TLS handshake could not be read safely — this is not a missing domain rule"
	case "dns_name_invalid", "dns_query_invalid", "dns_answer_invalid", "dns_cname_limit", "dns_answer_limit", "dns_upstream_invalid":
		return "the DNS answer failed validation, and it names no port or protocol to allow"
	case "dns_unavailable", "dns_no_address", "dns_ttl_expired", "dns_capacity_exceeded":
		return "the resolver could not give a usable address — fix the resolver, a rule will not help"
	case "gateway_connection_capacity", "gateway_lease_capacity", "gateway_lease_refused", "gateway_unavailable", "enforcement_unavailable", "clock_unavailable", "observation_unavailable", "upstream_unreachable", "unsupported_capability":
		return "the gateway could not check or record this — it is not a missing rule"
	default:
		return "this coop cannot explain that reason code"
	}
}
