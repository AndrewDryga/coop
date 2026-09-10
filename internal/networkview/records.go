// Package networkview is the versioned evidence model shared by gateway
// collection, private host records and explicit owner/worker projections.
// Observations are facts, never authority or provider-progress signals.
package networkview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

const Version = 1

// Count is a JSON unsigned decimal string, so JavaScript cannot round values
// above 2^53. A missing metric is represented by a nil pointer, not Count(0).
type Count uint64

func (c Count) MarshalJSON() ([]byte, error) { return json.Marshal(strconv.FormatUint(uint64(c), 10)) }
func (c *Count) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil || value == "" || (len(value) > 1 && value[0] == '0') {
		return errors.New("network counter must be an unsigned decimal string")
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return errors.New("invalid network counter")
	}
	*c = Count(n)
	return nil
}

func Value(n uint64) *Count { value := Count(n); return &value }

// Add saturates and reports loss of exactness; counter wrap must never invent a
// negative delta, low total or an upload-rate spike on the next observation.
func Add(current *Count, delta uint64) bool {
	if ^uint64(0)-uint64(*current) < delta {
		*current = Count(^uint64(0))
		return false
	}
	*current += Count(delta)
	return true
}

type Health struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type HealthLayers struct {
	Enforcer  Health `json:"enforcer"`
	Gateway   Health `json:"gateway"`
	Resolver  Health `json:"resolver"`
	Collector Health `json:"collector"`
}

type Counters struct {
	SentBytes                *Count `json:"sent_bytes"`
	ReceivedBytes            *Count `json:"received_bytes"`
	Connections              *Count `json:"connections"`
	UpstreamFailures         *Count `json:"upstream_failures"`
	DeniedPackets            *Count `json:"denied_packets"`
	ProtectedPackets         *Count `json:"protected_packets"`
	DeniedDNSQueries         *Count `json:"denied_dns_queries"`
	DeniedTLS                *Count `json:"denied_tls_connections"`
	MaintenanceQueries       *Count `json:"maintenance_queries"`
	MaintenanceFailures      *Count `json:"maintenance_failures"`
	IngressDenials           *Count `json:"ingress_denied_packets"`
	MaintenanceSentBytes     *Count `json:"maintenance_sent_bytes"`
	MaintenanceReceivedBytes *Count `json:"maintenance_received_bytes"`
}

// AddressGrantObservation is what one raw tcp/udp/icmp grant actually passed.
// A refusal is deliberately absent: refused raw packets are counted in
// DeniedPackets/ProtectedPackets and never attributed to a destination, because
// the packet filter drops them without recording where they were headed.
type AddressGrantObservation struct {
	RuleID  string `json:"rule_id"`
	Packets Count  `json:"packets"`
	Bytes   Count  `json:"bytes"`
}

// Coverage describes each measurement source independently. A retained total
// can be a lower bound even while a different source remains exact.
type MetricCoverage struct {
	Status string `json:"status"` // exact, lower-bound, or unavailable
	Reason string `json:"reason,omitempty"`
}

type Coverage struct {
	ProxyBytes          MetricCoverage `json:"proxy_bytes"`
	Connections         MetricCoverage `json:"connections"`
	UpstreamFailures    MetricCoverage `json:"upstream_failures"`
	KernelPackets       MetricCoverage `json:"kernel_packets"`
	GuardDenials        MetricCoverage `json:"guard_denials"`
	MaintenanceQueries  MetricCoverage `json:"maintenance_queries"`
	MaintenanceBytes    MetricCoverage `json:"maintenance_bytes"`
	SocketInventory     MetricCoverage `json:"socket_inventory"`
	BoundaryAttribution MetricCoverage `json:"boundary_attribution"`
}

type Source struct {
	ID          string     `json:"id"`
	Sequence    Count      `json:"sequence"`
	ObservedAt  *time.Time `json:"observed_at"`
	LastEventAt *time.Time `json:"last_event_at"`
	Status      string     `json:"status"`
	Lost        Count      `json:"lost_records"`
	Unknown     bool       `json:"unknown_loss"`
	Reason      string     `json:"reason,omitempty"`
}

type Loss struct {
	Records          Count    `json:"records"`
	Unknown          bool     `json:"unknown"`
	Reasons          []string `json:"reasons,omitempty"`
	DetailTruncated  bool     `json:"detail_truncated"`
	OmittedDetails   *Count   `json:"omitted_details"`
	SuppressedAlerts Count    `json:"suppressed_alerts"`
}

type Rate struct {
	SentPerSecond     float64 `json:"sent_bytes_per_second"`
	ReceivedPerSecond float64 `json:"received_bytes_per_second"`
	WindowMillis      Count   `json:"window_ms"`
	MaxWindowMillis   Count   `json:"max_window_ms,omitempty"`
	Measurement       string  `json:"measurement,omitempty"`
}

type Connection struct {
	ID            string     `json:"id"`
	DestinationID string     `json:"destination_id"`
	State         string     `json:"state"`
	Reason        string     `json:"reason,omitempty"`
	Transport     string     `json:"transport"`
	Name          string     `json:"name,omitempty"`
	NameSource    string     `json:"name_source,omitempty"`
	Peer          string     `json:"peer,omitempty"`
	RuleID        string     `json:"rule_id,omitempty"`
	StartedAt     *time.Time `json:"started_at"`
	ObservedAt    time.Time  `json:"observed_at"`
	SentBytes     *Count     `json:"sent_bytes"`
	ReceivedBytes *Count     `json:"received_bytes"`
	Rate          *Rate      `json:"rate,omitempty"`
	ConnectMillis *Count     `json:"connect_ms,omitempty"`
	Partial       bool       `json:"partial"`
}

type Candidate struct {
	ID                string      `json:"id"`
	EvidenceID        string      `json:"evidence_id"`
	PolicyFingerprint string      `json:"policy_fingerprint"`
	Rule              egress.Rule `json:"rule"`
	AppliesTo         string      `json:"applies_to"`
}

type Denial struct {
	ID            string     `json:"id"`
	Source        string     `json:"source"`
	Sequence      Count      `json:"source_sequence"`
	Basis         string     `json:"basis"`
	DestinationID string     `json:"destination_id,omitempty"`
	At            time.Time  `json:"at"`
	Kind          string     `json:"kind"`
	Reason        string     `json:"reason"`
	Name          string     `json:"name,omitempty"`
	Peer          string     `json:"peer,omitempty"`
	Port          *int       `json:"port,omitempty"`
	Candidate     *Candidate `json:"candidate,omitempty"`
	Withheld      bool       `json:"destination_withheld,omitempty"`
}

type AlertFacts struct {
	Packets      Count  `json:"packets"`
	DNSQueries   Count  `json:"dns_queries"`
	DeniedTLS    Count  `json:"denied_tls_connections"`
	Connections  Count  `json:"connections"`
	SentBytes    Count  `json:"sent_bytes"`
	HealthStatus string `json:"health_status,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type Alert struct {
	ID           string         `json:"id"`
	Sequence     Count          `json:"source_sequence"`
	Version      int            `json:"detector_version"`
	Category     string         `json:"category"`
	Severity     string         `json:"severity"`
	State        string         `json:"state"`
	Terminal     bool           `json:"terminal"`
	FirstSeen    time.Time      `json:"first_seen"`
	LastSeen     time.Time      `json:"last_seen"`
	WindowMillis Count          `json:"window_ms"`
	Threshold    AlertThreshold `json:"threshold"`
	Facts        AlertFacts     `json:"facts"`
	EvidenceIDs  []string       `json:"evidence_ids,omitempty"`
}

type AlertThreshold struct {
	Value                Count  `json:"value"`
	Unit                 string `json:"unit"`
	Baseline             *Count `json:"baseline,omitempty"`
	BaselineWindowMillis Count  `json:"baseline_window_ms,omitempty"`
}

type Snapshot struct {
	Version              int          `json:"version"`
	RunID                string       `json:"run_id"`
	Epoch                string       `json:"gateway_epoch"`
	PolicyFingerprint    string       `json:"policy_fingerprint"`
	Mode                 egress.Mode  `json:"mode"`
	Sequence             Count        `json:"sequence"`
	Terminal             bool         `json:"terminal"`
	AsOf                 time.Time    `json:"as_of"`
	ElapsedMillis        Count        `json:"elapsed_ms"`
	Availability         string       `json:"availability"`
	Scope                string       `json:"scope"`
	Health               HealthLayers `json:"health"`
	Coverage             Coverage     `json:"coverage"`
	Counters             *Counters    `json:"counters"`
	Rate                 *Rate        `json:"rate,omitempty"`
	LiveConnections      *Count       `json:"live_connections"`
	UnknownConnections   *Count       `json:"unknown_connections"`
	PendingConnections   Count        `json:"pending_connections"`
	KernelClosingSockets *Count       `json:"kernel_closing_sockets"`
	StaleConnections     Count        `json:"stale_connections"`
	Connections          []Connection `json:"connections"`
	// AddressGrants shares KernelPackets coverage: it is the same kernel sample.
	AddressGrants []AddressGrantObservation `json:"address_grants,omitempty"`
	Denials       []Denial                  `json:"denials"`
	Alerts        []Alert                   `json:"alerts"`
	Loss          Loss                      `json:"loss"`
	Sources       []Source                  `json:"sources"`
	Projection    string                    `json:"projection"`
}

type Receipt struct {
	Version          int        `json:"version"`
	ID               string     `json:"id"`
	Snapshot         Snapshot   `json:"network"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	Finality         string     `json:"finality"`
	Completeness     string     `json:"completeness"`
	Workload         string     `json:"workload_outcome"`
	Cleanup          string     `json:"cleanup_outcome"`
	Runtime          string     `json:"runtime"`
	GatewayImage     string     `json:"gateway_image"`
	SessionID        string     `json:"session_id,omitempty"`
	AttemptID        string     `json:"attempt_id,omitempty"`
	AuthorityDigest  string     `json:"authority_digest"`
	CollectorVersion string     `json:"collector_version"`
	BundleReferences []string   `json:"bundle_references"`
	Digest           string     `json:"digest"`
	DigestScope      string     `json:"digest_scope"`
}

// Project constructs a separate outbound value. In particular, private receipt
// digests and concrete candidates must never accompany a redacted projection.
func (s Snapshot) Project(exportDestinations bool) Snapshot {
	// This is deliberately an allowlist, not a serialize-then-delete projection:
	// adding an owner-private field must not silently add it to worker replies.
	out := Snapshot{Version: s.Version, RunID: s.RunID, Epoch: s.Epoch, PolicyFingerprint: s.PolicyFingerprint,
		Mode: s.Mode, Sequence: s.Sequence, AsOf: s.AsOf, ElapsedMillis: s.ElapsedMillis,
		Availability: s.Availability, Scope: s.Scope, Health: s.Health, Coverage: s.Coverage, Rate: clone(s.Rate), Terminal: s.Terminal,
		LiveConnections: clone(s.LiveConnections), UnknownConnections: clone(s.UnknownConnections), PendingConnections: s.PendingConnections,
		KernelClosingSockets: clone(s.KernelClosingSockets), StaleConnections: s.StaleConnections, Projection: "destinations-withheld",
		Loss: Loss{Records: s.Loss.Records, Unknown: s.Loss.Unknown, Reasons: slices.Clone(s.Loss.Reasons),
			DetailTruncated: s.Loss.DetailTruncated, OmittedDetails: clone(s.Loss.OmittedDetails), SuppressedAlerts: s.Loss.SuppressedAlerts}}
	if s.Counters != nil {
		c := s.Counters
		out.Counters = &Counters{SentBytes: clone(c.SentBytes), ReceivedBytes: clone(c.ReceivedBytes),
			Connections: clone(c.Connections), UpstreamFailures: clone(c.UpstreamFailures), DeniedPackets: clone(c.DeniedPackets),
			ProtectedPackets: clone(c.ProtectedPackets), DeniedDNSQueries: clone(c.DeniedDNSQueries), DeniedTLS: clone(c.DeniedTLS),
			MaintenanceQueries: clone(c.MaintenanceQueries), MaintenanceFailures: clone(c.MaintenanceFailures), IngressDenials: clone(c.IngressDenials),
			MaintenanceSentBytes: clone(c.MaintenanceSentBytes), MaintenanceReceivedBytes: clone(c.MaintenanceReceivedBytes)}
	}
	if exportDestinations {
		out.Projection = "destinations-included"
		// A grant ID names a private destination as surely as its address does.
		out.AddressGrants = slices.Clone(s.AddressGrants)
	}
	for _, c := range s.Connections {
		row := Connection{ID: c.ID, DestinationID: c.DestinationID, State: c.State, Reason: c.Reason, Transport: c.Transport,
			NameSource: c.NameSource, StartedAt: clone(c.StartedAt), ObservedAt: c.ObservedAt, SentBytes: clone(c.SentBytes),
			ReceivedBytes: clone(c.ReceivedBytes), Rate: clone(c.Rate), ConnectMillis: clone(c.ConnectMillis), Partial: c.Partial}
		if exportDestinations {
			row.Name, row.Peer, row.RuleID = c.Name, c.Peer, c.RuleID
		}
		out.Connections = append(out.Connections, row)
	}
	for _, d := range s.Denials {
		row := Denial{ID: d.ID, Source: d.Source, Sequence: d.Sequence, Basis: d.Basis, DestinationID: d.DestinationID,
			At: d.At, Kind: d.Kind, Reason: d.Reason, Port: clone(d.Port), Withheld: !exportDestinations}
		if exportDestinations {
			row.Name = d.Name
			row.Peer = d.Peer
			if c := d.Candidate; c != nil {
				r := c.Rule
				row.Candidate = &Candidate{ID: c.ID, EvidenceID: c.EvidenceID, PolicyFingerprint: c.PolicyFingerprint,
					AppliesTo: c.AppliesTo, Rule: egress.Rule{To: egress.Destination{Domain: r.To.Domain, IP: r.To.IP, CIDR: r.To.CIDR,
						Provider: r.To.Provider, Features: slices.Clone(r.To.Features)}, Protocol: r.Protocol,
						Ports: slices.Clone(r.Ports), Types: slices.Clone(r.Types), Codes: slices.Clone(r.Codes)}}
			}
		}
		out.Denials = append(out.Denials, row)
	}
	for _, a := range s.Alerts {
		out.Alerts = append(out.Alerts, Alert{ID: a.ID, Sequence: a.Sequence, Version: a.Version, Category: a.Category, Severity: a.Severity,
			State: a.State, Terminal: a.Terminal, FirstSeen: a.FirstSeen, LastSeen: a.LastSeen, WindowMillis: a.WindowMillis,
			Threshold: AlertThreshold{Value: a.Threshold.Value, Unit: a.Threshold.Unit, Baseline: clone(a.Threshold.Baseline), BaselineWindowMillis: a.Threshold.BaselineWindowMillis}, Facts: a.Facts, EvidenceIDs: slices.Clone(a.EvidenceIDs)})
	}
	for _, source := range s.Sources {
		out.Sources = append(out.Sources, Source{ID: source.ID, Sequence: source.Sequence, ObservedAt: clone(source.ObservedAt), LastEventAt: clone(source.LastEventAt),
			Status: source.Status, Lost: source.Lost, Unknown: source.Unknown, Reason: source.Reason})
	}
	return out
}

func (r Receipt) Project(exportDestinations bool) (Receipt, error) {
	out := Receipt{Version: r.Version, ID: r.ID, Snapshot: r.Snapshot.Project(exportDestinations), StartedAt: r.StartedAt,
		EndedAt: clone(r.EndedAt), Finality: r.Finality, Completeness: r.Completeness, Workload: r.Workload, Cleanup: r.Cleanup,
		Runtime: r.Runtime, GatewayImage: r.GatewayImage, SessionID: r.SessionID, AttemptID: r.AttemptID,
		AuthorityDigest: r.AuthorityDigest, CollectorVersion: r.CollectorVersion, BundleReferences: slices.Clone(r.BundleReferences)}
	out.DigestScope = out.Snapshot.Projection
	err := out.SealDigest()
	return out, err
}

// SealDigest fingerprints this projection's content. It is not a signature,
// attestation or claim that partial observations are complete.
func (r *Receipt) SealDigest() error {
	r.Digest = ""
	if !validRate(r.Snapshot.Rate) {
		return errors.New("network receipt contains invalid rate")
	}
	for _, c := range r.Snapshot.Connections {
		if !validRate(c.Rate) {
			return errors.New("network receipt contains invalid rate")
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		return errors.New("network receipt contains invalid evidence")
	}
	digest := sha256.Sum256(data)
	r.Digest = hex.EncodeToString(digest[:])
	return nil
}

func validRate(rate *Rate) bool {
	return rate == nil || (rate.WindowMillis > 0 && rate.SentPerSecond >= 0 && rate.ReceivedPerSecond >= 0 &&
		!math.IsNaN(rate.SentPerSecond) && !math.IsNaN(rate.ReceivedPerSecond) &&
		!math.IsInf(rate.SentPerSecond, 0) && !math.IsInf(rate.ReceivedPerSecond, 0))
}

func clone[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
