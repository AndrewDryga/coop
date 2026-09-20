package networkstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strings"

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
	invalid := errors.New("coop net check takes one exact domain (checked as TLS on 443, unless --port says otherwise), or one IPv4 address with --protocol tcp|udp --port <n>, or --icmp")
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
			return PolicyExplanation{}, errors.New("coop net check takes one exact domain, checked as TLS on the port you asked about")
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
		Message: DiagnosticReason(decision.Reason), Resolution: "not_evaluated", CurrentPolicy: "not_evaluated", Withheld: !exportDestinations}
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
	message, known := diagnosticMessage(projected.Reason)
	if !known {
		// A code this build has no sentence for keeps its own text in the
		// message and is retagged, so nothing downstream reads it as a reason
		// it understands.
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
	// The client's own port is a source inside the box, not a destination, so it is
	// admitted for any kind — but it is still a port, and a retained one that is not
	// is not evidence.
	if d.SourcePort != nil && (*d.SourcePort < 1 || *d.SourcePort > 65535) {
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

// diagnosticReasons is the ONE mapping from a retained reason code to the
// sentence a person reads. It is DATA, not a renderer: every code the evidence
// can carry has its own exact cause here, so a resolver, transport or recording
// failure is never relabeled as a missing project approval — and nothing has to
// guess a Docker, provider or firewall remedy it cannot prove.
//
// The codes themselves are the machine contract and never change. These
// sentences are human copy and may.
var diagnosticReasons = map[string]string{
	"open":                        "This run allowed unrestricted internet access.",
	"rule_allowed":                "Allowed by a rule this run started with.",
	"unapproved_name":             "This run had no approved rule for this destination.",
	"protected_destination":       "Coop blocks access to this protected destination.",
	"unsafe_dns_answer":           "The hostname resolved to an address Coop blocks for safety.",
	"protocol_not_allowed":        "This run had no approved rule for this protocol.",
	"port_not_allowed":            "This run had no approved rule for this port.",
	"fixed_egress_policy":         "This connection is blocked by the run's network policy.",
	"tls_name_missing":            "The connection did not provide the server name needed to check a domain rule.",
	"tls_name_invalid":            "The connection provided an invalid server name.",
	"tls_ech_unsupported":         "The connection hid its server name, so Coop could not check the domain rule.",
	"tls_malformed":               "The connection's TLS handshake was invalid.",
	"tls_hello_too_large":         "The connection's TLS handshake was too large to check safely.",
	"tls_inspection_timeout":      "Checking the connection's TLS handshake timed out.",
	"tls_inspection_unavailable":  "Coop could not inspect the connection's TLS handshake.",
	"dns_name_invalid":            "The requested hostname was invalid.",
	"dns_query_invalid":           "The DNS request was invalid.",
	"dns_answer_invalid":          "The DNS response was invalid.",
	"dns_cname_limit":             "Resolving this hostname required too many redirects.",
	"dns_answer_limit":            "The DNS response contained too many addresses.",
	"dns_upstream_invalid":        "The upstream DNS response failed validation.",
	"dns_unavailable":             "DNS resolution was unavailable.",
	"dns_no_address":              "DNS returned no usable address for this hostname.",
	"dns_ttl_expired":             "The resolved address expired before the connection could use it.",
	"dns_capacity_exceeded":       "Coop's DNS resolver reached its capacity limit.",
	"gateway_connection_capacity": "Coop's network gateway reached its connection limit.",
	"gateway_lease_capacity":      "Coop's network gateway reached its capacity limit.",
	"gateway_lease_refused":       "Coop's network gateway could not authorize this connection.",
	"gateway_unavailable":         "Coop's network gateway was unavailable.",
	"enforcement_unavailable":     "Network enforcement became unavailable.",
	"clock_unavailable":           "Coop could not verify the time needed to authorize this connection.",
	"observation_unavailable":     "Connection recording was unavailable.",
	"upstream_unreachable":        "The destination could not be reached.",
	"unsupported_capability":      "This connection needs a network capability Coop does not support.",
}

// DiagnosticReason is the human cause for one retained reason code, for a
// renderer that holds a reason without an explanation around it.
func DiagnosticReason(reason string) string {
	message, _ := diagnosticMessage(reason)
	return message
}

// diagnosticMessage returns that sentence and whether this build knows the
// code. An unknown code is shown as itself — terminal-safe, since the reason
// grammar is a fixed lowercase identifier — because a guess would be a worse
// answer than the code.
func diagnosticMessage(reason string) (string, bool) {
	if message, ok := diagnosticReasons[reason]; ok {
		return message, true
	}
	return "This Coop version cannot explain the recorded reason: " + safeReasonCode(reason) + ".", false
}

// safeReasonCode keeps an unrecognized code printable: the retained grammar is
// lowercase letters, digits and underscores, and anything else is replaced
// rather than passed to a terminal.
func safeReasonCode(reason string) string {
	if reason == "" {
		return "(none)"
	}
	var b strings.Builder
	for i, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('?')
		}
		if i >= 63 {
			break
		}
	}
	return b.String()
}
