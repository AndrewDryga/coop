package workerproto

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SessionEvidenceVersion is the version of the evidence object itself, separate from the poll's
// protocol version: the inspection contract can grow long before the whole worker contract does,
// and a control plane that only understands version 1 has to be able to say so.
const SessionEvidenceVersion = 1

// Bounds on the exported evidence. The daemon's own retained rings are larger; the export keeps a
// bounded head of each list and counts what it dropped, so a control plane always learns that a
// list is incomplete rather than reading a short list as the whole story.
const (
	MaxSessionEvidenceRules       = 256
	MaxSessionEvidenceDenials     = 64
	MaxSessionEvidenceConnections = 64
	MaxSessionEvidenceAlerts      = 32
	MaxSessionEvidenceSources     = 16
	MaxSessionEvidenceRunRefs     = 64
	MaxSessionEvidenceChecklist   = 64
	MaxSessionEvidenceTaskFiles   = MaxWorkspaceCheckpointTaskFiles
	MaxSessionEvidenceNoteBytes   = 8 << 10
	MaxSessionEvidenceTextBytes   = 512
	MaxSessionEvidenceTitleBytes  = 256
	MaxSessionEvidenceLabelBytes  = 1024
)

const (
	NetworkModeOpen     = "open"
	NetworkModeNone     = "none"
	NetworkModeFiltered = "filtered"

	NetworkProjectionWithheld = "destinations-withheld"
	NetworkProjectionIncluded = "destinations-included"

	EvidenceStatusCaptured    = "captured"
	EvidenceStatusNotFiltered = "not_filtered"
	EvidenceStatusUnavailable = "unavailable"
	EvidenceStatusObserved    = "observed"
	EvidenceStatusNoRun       = "no_run"
	EvidenceStatusAvailable   = "available"
	EvidenceStatusBound       = "bound"
	EvidenceStatusUnbound     = "unbound"
	EvidenceStatusAbsent      = "absent"
	EvidenceStatusWithheld    = "withheld"
)

var (
	networkModes           = []string{NetworkModeOpen, NetworkModeNone, NetworkModeFiltered}
	networkProjections     = []string{NetworkProjectionWithheld, NetworkProjectionIncluded}
	networkAccessStatuses  = []string{EvidenceStatusCaptured, EvidenceStatusNotFiltered, EvidenceStatusUnavailable}
	networkObservedStatus  = []string{EvidenceStatusObserved, EvidenceStatusNoRun, EvidenceStatusNotFiltered, EvidenceStatusUnavailable}
	networkReceiptStatuses = []string{EvidenceStatusAvailable, EvidenceStatusNotFiltered, EvidenceStatusUnavailable}
	networkFreshness       = []string{"fresh", "stale", "not-observed", "terminal"}
	networkFinalities      = []string{"provisional", "final"}
	networkCompleteness    = []string{"complete", "partial", "unknown"}
	networkCoverageStatus  = []string{"exact", "lower-bound", "unavailable"}
	networkDenialBases     = []string{"dns", "tls", "socket", "admission", "unknown"}
	taskStatuses           = []string{EvidenceStatusBound, EvidenceStatusUnbound, EvidenceStatusUnavailable}
	taskNoteStatuses       = []string{EvidenceStatusCaptured, EvidenceStatusAbsent, EvidenceStatusWithheld}
	sessionEvidenceStates  = []string{"open", "exhausted", "closed", "discarded"}
	evidenceCounterPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,19})$`)
)

// SessionEvidence is the daemon's own account of one session for a control plane's inspection
// page: the network posture the session was admitted under and what its runs were observed doing,
// and the host-approved task bound into its workspace as the task folder currently stands.
//
// Every section states its own availability. A section that could not be read says so with a
// reason; it never collapses into an empty list or a zero counter, because "nothing observed" and
// "could not observe" lead an operator to opposite conclusions. The object never carries a
// credential, a host path, a packet body, or — unless the session policy opted into destination
// export — a destination name.
type SessionEvidence struct {
	Version    int             `json:"version"`
	CapturedAt time.Time       `json:"captured_at"`
	SessionID  string          `json:"session_id"`
	Revision   int64           `json:"revision"`
	State      string          `json:"state"`
	Network    NetworkEvidence `json:"network"`
	Task       TaskEvidence    `json:"task"`
}

// NetworkEvidence is the frozen posture plus the three reads behind it. Mode and fingerprint come
// from the immutable session row, so they are always known; the rest is retained evidence that
// may be unreadable on a given host without the posture itself becoming unknown.
type NetworkEvidence struct {
	Mode        string             `json:"mode"`
	Fingerprint *string            `json:"fingerprint"`
	Access      NetworkAccess      `json:"access"`
	Observation NetworkObservation `json:"observation"`
	Receipt     NetworkReceipt     `json:"receipt"`
}

// NetworkAccess is what the captured policy lets the session reach, as far as it is disclosed.
// Requested is what a human wrote; Effective adds the provider and MCP grants derived from the
// authorized selection. Both stay empty under a withheld projection: a rule text names a
// destination as surely as a connection row does.
type NetworkAccess struct {
	Status        string   `json:"status"`
	Reason        *string  `json:"reason"`
	Qualification *string  `json:"qualification"`
	Projection    *string  `json:"projection"`
	Requested     []string `json:"requested"`
	Effective     []string `json:"effective"`
}

// NetworkObservation is the newest run's retained observation. Freshness is the collector's own
// word for how old the sample is; none of these words is a claim that the workload is running.
type NetworkObservation struct {
	Status             string              `json:"status"`
	Reason             *string             `json:"reason"`
	Freshness          *string             `json:"freshness"`
	RunID              *string             `json:"run_id"`
	AttemptID          *string             `json:"attempt_id"`
	GatewayEpoch       *string             `json:"gateway_epoch"`
	Sequence           *string             `json:"sequence"`
	AsOf               *time.Time          `json:"as_of"`
	Availability       *string             `json:"availability"`
	Scope              *string             `json:"scope"`
	Sealed             bool                `json:"sealed"`
	CleanupOutcome     *string             `json:"cleanup_outcome"`
	Projection         *string             `json:"projection"`
	Health             *NetworkHealth      `json:"health"`
	Coverage           *NetworkCoverage    `json:"coverage"`
	Counters           *NetworkCounters    `json:"counters"`
	Loss               *NetworkLoss        `json:"loss"`
	Sources            []NetworkSource     `json:"sources"`
	Denials            []NetworkDenial     `json:"denials"`
	OmittedDenials     int                 `json:"omitted_denials"`
	Connections        []NetworkConnection `json:"connections"`
	OmittedConnections int                 `json:"omitted_connections"`
	Alerts             []NetworkAlert      `json:"alerts"`
	OmittedAlerts      int                 `json:"omitted_alerts"`
}

type NetworkLayerHealth struct {
	Status string  `json:"status"`
	Reason *string `json:"reason"`
}

// NetworkHealth reports each enforcement layer separately. A configured policy is not an enforced
// one until the enforcer layer says so, and a dead collector is not a broken enforcer.
type NetworkHealth struct {
	Enforcer  NetworkLayerHealth `json:"enforcer"`
	Gateway   NetworkLayerHealth `json:"gateway"`
	Resolver  NetworkLayerHealth `json:"resolver"`
	Collector NetworkLayerHealth `json:"collector"`
}

type NetworkMetricCoverage struct {
	Status string  `json:"status"`
	Reason *string `json:"reason"`
}

// NetworkCoverage describes each measurement source independently: a retained total can be a
// lower bound while a different source stays exact.
type NetworkCoverage struct {
	ProxyBytes          NetworkMetricCoverage `json:"proxy_bytes"`
	Connections         NetworkMetricCoverage `json:"connections"`
	UpstreamFailures    NetworkMetricCoverage `json:"upstream_failures"`
	KernelPackets       NetworkMetricCoverage `json:"kernel_packets"`
	GuardDenials        NetworkMetricCoverage `json:"guard_denials"`
	MaintenanceQueries  NetworkMetricCoverage `json:"maintenance_queries"`
	MaintenanceBytes    NetworkMetricCoverage `json:"maintenance_bytes"`
	SocketInventory     NetworkMetricCoverage `json:"socket_inventory"`
	BoundaryAttribution NetworkMetricCoverage `json:"boundary_attribution"`
}

// NetworkCounters are unsigned decimal STRINGS, exactly as the collector records them, so a value
// above 2^53 survives a JavaScript client. A null counter is a metric nobody measured, never 0.
type NetworkCounters struct {
	SentBytes                *string `json:"sent_bytes"`
	ReceivedBytes            *string `json:"received_bytes"`
	Connections              *string `json:"connections"`
	UpstreamFailures         *string `json:"upstream_failures"`
	DeniedPackets            *string `json:"denied_packets"`
	ProtectedPackets         *string `json:"protected_packets"`
	DeniedDNSQueries         *string `json:"denied_dns_queries"`
	DeniedTLSConnections     *string `json:"denied_tls_connections"`
	MaintenanceQueries       *string `json:"maintenance_queries"`
	MaintenanceFailures      *string `json:"maintenance_failures"`
	IngressDeniedPackets     *string `json:"ingress_denied_packets"`
	MaintenanceSentBytes     *string `json:"maintenance_sent_bytes"`
	MaintenanceReceivedBytes *string `json:"maintenance_received_bytes"`
}

type NetworkLoss struct {
	Records          string   `json:"records"`
	Unknown          bool     `json:"unknown"`
	Reasons          []string `json:"reasons"`
	DetailTruncated  bool     `json:"detail_truncated"`
	OmittedDetails   *string  `json:"omitted_details"`
	SuppressedAlerts string   `json:"suppressed_alerts"`
}

type NetworkSource struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	Sequence    string     `json:"sequence"`
	ObservedAt  *time.Time `json:"observed_at"`
	LastEventAt *time.Time `json:"last_event_at"`
	LostRecords string     `json:"lost_records"`
	UnknownLoss bool       `json:"unknown_loss"`
	Reason      *string    `json:"reason"`
}

// NetworkDenial is one refusal the boundary recorded. Destination is present only under an
// included projection; DestinationWithheld says the name existed and was not exported, which is a
// different fact from a refusal that observed no name at all.
type NetworkDenial struct {
	ID                  string    `json:"id"`
	At                  time.Time `json:"at"`
	Kind                string    `json:"kind"`
	Basis               string    `json:"basis"`
	Reason              string    `json:"reason"`
	Source              string    `json:"source"`
	SourceSequence      string    `json:"source_sequence"`
	Destination         *string   `json:"destination"`
	DestinationWithheld bool      `json:"destination_withheld"`
	Port                *int      `json:"port"`
	// SourcePort is the client's own port for a refusal Coop's gateway made against
	// something addressed to IT — a connection dialed straight at a listener, a
	// message its DNS listener could not read as a query. Those name no destination,
	// so this is the only handle on which program in the box made the attempt. It is
	// a SOURCE: never where the attempt was going, and never rendered as one.
	SourcePort *int `json:"source_port,omitempty"`
}

type NetworkConnection struct {
	ID                  string     `json:"id"`
	State               string     `json:"state"`
	Reason              *string    `json:"reason"`
	Transport           string     `json:"transport"`
	Destination         *string    `json:"destination"`
	DestinationWithheld bool       `json:"destination_withheld"`
	RuleID              *string    `json:"rule_id"`
	StartedAt           *time.Time `json:"started_at"`
	ObservedAt          time.Time  `json:"observed_at"`
	SentBytes           *string    `json:"sent_bytes"`
	ReceivedBytes       *string    `json:"received_bytes"`
	Partial             bool       `json:"partial"`
}

type NetworkAlert struct {
	ID           string    `json:"id"`
	Category     string    `json:"category"`
	Severity     string    `json:"severity"`
	State        string    `json:"state"`
	Terminal     bool      `json:"terminal"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	Reason       *string   `json:"reason"`
	HealthStatus *string   `json:"health_status"`
}

// NetworkReceipt is the session-wide aggregate across every run the session owned, independent of
// container garbage collection. Finality and completeness are separate: a closed session's
// receipt can be final and still honestly partial. Run references are a bounded head of the
// daemon's own bounded list; OmittedRunReferences counts everything not listed.
type NetworkReceipt struct {
	Status               string                `json:"status"`
	Reason               *string               `json:"reason"`
	PolicyFingerprint    *string               `json:"policy_fingerprint"`
	AuthorityDigest      *string               `json:"authority_digest"`
	StartedAt            *time.Time            `json:"started_at"`
	ClosedAt             *time.Time            `json:"closed_at"`
	Finality             *string               `json:"finality"`
	Completeness         *string               `json:"completeness"`
	Scope                *string               `json:"scope"`
	Counters             *NetworkCounters      `json:"counters"`
	Coverage             *NetworkCoverage      `json:"coverage"`
	Loss                 *NetworkLoss          `json:"loss"`
	Runs                 []NetworkRunReference `json:"runs"`
	RunCount             *string               `json:"run_count"`
	OmittedRunReferences *string               `json:"omitted_run_references"`
	Projection           *string               `json:"projection"`
	ReceiptDigest        *string               `json:"receipt_digest"`
	DigestScope          *string               `json:"digest_scope"`
}

type NetworkRunReference struct {
	RunID         string    `json:"run_id"`
	GatewayEpoch  string    `json:"gateway_epoch"`
	Sequence      string    `json:"sequence"`
	AsOf          time.Time `json:"as_of"`
	Finality      string    `json:"finality"`
	Completeness  string    `json:"completeness"`
	ReceiptDigest string    `json:"receipt_digest"`
}

// TaskEvidence is the host-approved task bound into the session workspace. The identities are the
// immutable binding on the session row; the snapshot is the task folder as it stands at capture,
// which is the only history a worker can honestly offer — it keeps no transition ledger, so a
// control plane records successive snapshots rather than asking for one.
type TaskEvidence struct {
	Status      string        `json:"status"`
	Reason      *string       `json:"reason"`
	QueueID     *string       `json:"queue_id"`
	TaskID      *string       `json:"task_id"`
	ID          *string       `json:"id"`
	OfferRef    *string       `json:"offer_ref"`
	DraftSHA256 *string       `json:"draft_sha256"`
	Snapshot    *TaskSnapshot `json:"snapshot"`
}

// TaskSnapshot is the task folder at capture. StateSHA256 is the same digest a workspace checkpoint
// records for its task projection, so a checkpoint and a snapshot of the same folder state agree
// exactly and a control plane can link the two without guessing.
type TaskSnapshot struct {
	State       string              `json:"state"`
	StateSHA256 string              `json:"state_sha256"`
	Title       string              `json:"title"`
	Checklist   []TaskChecklistItem `json:"checklist"`
	Files       []TaskFile          `json:"files"`
	StateNote   TaskNote            `json:"state_note"`
	HasDecision bool                `json:"has_decision"`
}

type TaskChecklistItem struct {
	Label   string `json:"label"`
	Checked bool   `json:"checked"`
}

type TaskFile struct {
	Path     string `json:"path"`
	ByteSize int64  `json:"byte_size"`
	SHA256   string `json:"sha256"`
}

// TaskNote is the agent-written state.md. It is bounded, and withheld whole when it looks like it
// carries a secret: a resume note is useful context, not worth a leaked token.
type TaskNote struct {
	Status    string  `json:"status"`
	Text      *string `json:"text"`
	Truncated bool    `json:"truncated"`
	Reason    *string `json:"reason"`
}

// DecodeSessionEvidence decodes and validates one exported evidence object strictly: unknown
// fields, trailing data and an oversized document are refused before any field is trusted.
func DecodeSessionEvidence(document []byte) (SessionEvidence, error) {
	var evidence SessionEvidence
	if err := decodeStrict(document, &evidence); err != nil {
		return SessionEvidence{}, fmt.Errorf("invalid session evidence: %w", err)
	}
	if err := evidence.Validate(); err != nil {
		return SessionEvidence{}, err
	}
	return evidence, nil
}

// Validate confirms one evidence object before the daemon publishes it or a connector forwards
// it. Every section's optional fields must match its status: a section that says it captured
// nothing may not carry data, and one that says it observed a run must name the run.
func (e SessionEvidence) Validate() error {
	if e.Version != SessionEvidenceVersion {
		return fmt.Errorf("unsupported session evidence version %d", e.Version)
	}
	if e.CapturedAt.IsZero() {
		return errors.New("session evidence capture time is required")
	}
	if err := reference(e.SessionID, 1024, "session evidence session id"); err != nil {
		return err
	}
	if e.Revision <= 0 {
		return errors.New("session evidence revision must be positive")
	}
	if !slices.Contains(sessionEvidenceStates, e.State) {
		return errors.New("invalid session evidence state")
	}
	if err := e.Network.validate(); err != nil {
		return err
	}
	return e.Task.validate()
}

func (n NetworkEvidence) validate() error {
	if !slices.Contains(networkModes, n.Mode) {
		return errors.New("invalid session evidence network mode")
	}
	filtered := n.Mode == NetworkModeFiltered
	if filtered != (n.Fingerprint != nil) {
		return errors.New("network fingerprint must accompany exactly a filtered posture")
	}
	if n.Fingerprint != nil && !digestPattern.MatchString(*n.Fingerprint) {
		return errors.New("network fingerprint must be a lowercase SHA-256 digest")
	}
	if err := n.Access.validate(filtered); err != nil {
		return err
	}
	if err := n.Observation.validate(filtered); err != nil {
		return err
	}
	return n.Receipt.validate(filtered)
}

func (a NetworkAccess) validate(filtered bool) error {
	if !slices.Contains(networkAccessStatuses, a.Status) {
		return errors.New("invalid network access status")
	}
	if (a.Status == EvidenceStatusNotFiltered) == filtered {
		return errors.New("network access status does not match the session posture")
	}
	if err := optionalReason(a.Reason, a.Status == EvidenceStatusUnavailable, "network access reason"); err != nil {
		return err
	}
	if a.Qualification != nil && reference(*a.Qualification, 256, "network qualification") != nil {
		return errors.New("invalid network qualification reference")
	}
	if a.Requested == nil || a.Effective == nil || len(a.Requested) > MaxSessionEvidenceRules || len(a.Effective) > MaxSessionEvidenceRules {
		return errors.New("network rule lists must be present and bounded")
	}
	if a.Status != EvidenceStatusCaptured {
		if a.Projection != nil || len(a.Requested) != 0 || len(a.Effective) != 0 || a.Qualification != nil {
			return errors.New("an uncaptured network access carries no policy detail")
		}
		return nil
	}
	if a.Projection == nil || !slices.Contains(networkProjections, *a.Projection) {
		return errors.New("captured network access requires its projection")
	}
	if *a.Projection == NetworkProjectionWithheld && (len(a.Requested) != 0 || len(a.Effective) != 0) {
		return errors.New("a withheld projection cannot name rules")
	}
	for _, rules := range [][]string{a.Requested, a.Effective} {
		for _, rule := range rules {
			if !boundedText(rule, MaxSessionEvidenceTextBytes) {
				return errors.New("invalid network rule text")
			}
		}
	}
	return nil
}

func (o NetworkObservation) validate(filtered bool) error {
	if !slices.Contains(networkObservedStatus, o.Status) {
		return errors.New("invalid network observation status")
	}
	if (o.Status == EvidenceStatusNotFiltered) == filtered {
		return errors.New("network observation status does not match the session posture")
	}
	if err := optionalReason(o.Reason, o.Status == EvidenceStatusUnavailable, "network observation reason"); err != nil {
		return err
	}
	if o.Sources == nil || o.Denials == nil || o.Connections == nil || o.Alerts == nil {
		return errors.New("network observation lists must be present")
	}
	if o.OmittedDenials < 0 || o.OmittedConnections < 0 || o.OmittedAlerts < 0 {
		return errors.New("network observation omission counts cannot be negative")
	}
	if o.Status != EvidenceStatusObserved {
		if o.Freshness != nil || o.RunID != nil || o.AttemptID != nil || o.GatewayEpoch != nil || o.Sequence != nil ||
			o.AsOf != nil || o.Availability != nil || o.Scope != nil || o.Sealed || o.CleanupOutcome != nil ||
			o.Projection != nil || o.Health != nil || o.Coverage != nil || o.Counters != nil || o.Loss != nil ||
			len(o.Sources) != 0 || len(o.Denials) != 0 || len(o.Connections) != 0 || len(o.Alerts) != 0 ||
			o.OmittedDenials != 0 || o.OmittedConnections != 0 || o.OmittedAlerts != 0 {
			return errors.New("an unobserved network run carries no observation detail")
		}
		return nil
	}
	if o.Freshness == nil || !slices.Contains(networkFreshness, *o.Freshness) {
		return errors.New("observed network run requires its freshness")
	}
	if o.RunID == nil || reference(*o.RunID, 256, "network run id") != nil {
		return errors.New("observed network run requires its run id")
	}
	for name, value := range map[string]*string{"attempt id": o.AttemptID, "gateway epoch": o.GatewayEpoch} {
		if value != nil && reference(*value, 256, "network "+name) != nil {
			return fmt.Errorf("invalid network %s", name)
		}
	}
	if o.Sequence == nil || !unsignedCounter(*o.Sequence) {
		return errors.New("observed network run requires its sequence")
	}
	if o.AsOf == nil || o.AsOf.IsZero() {
		return errors.New("observed network run requires its observation time")
	}
	for name, value := range map[string]*string{"availability": o.Availability, "scope": o.Scope, "cleanup outcome": o.CleanupOutcome} {
		if value != nil && !boundedText(*value, MaxSessionEvidenceTextBytes) {
			return fmt.Errorf("invalid network observation %s", name)
		}
	}
	if o.Projection == nil || !slices.Contains(networkProjections, *o.Projection) {
		return errors.New("observed network run requires its projection")
	}
	included := *o.Projection == NetworkProjectionIncluded
	if o.Health != nil {
		if err := o.Health.validate(); err != nil {
			return err
		}
	}
	if o.Coverage != nil {
		if err := o.Coverage.validate(); err != nil {
			return err
		}
	}
	if o.Counters != nil {
		if err := o.Counters.validate(); err != nil {
			return err
		}
	}
	if o.Loss != nil {
		if err := o.Loss.validate(); err != nil {
			return err
		}
	}
	if len(o.Sources) > MaxSessionEvidenceSources || len(o.Denials) > MaxSessionEvidenceDenials ||
		len(o.Connections) > MaxSessionEvidenceConnections || len(o.Alerts) > MaxSessionEvidenceAlerts {
		return errors.New("network observation lists exceed their bounds")
	}
	for _, source := range o.Sources {
		if err := source.validate(); err != nil {
			return err
		}
	}
	for _, denial := range o.Denials {
		if err := denial.validate(included); err != nil {
			return err
		}
	}
	for _, connection := range o.Connections {
		if err := connection.validate(included); err != nil {
			return err
		}
	}
	for _, alert := range o.Alerts {
		if err := alert.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (h NetworkHealth) validate() error {
	for _, layer := range []NetworkLayerHealth{h.Enforcer, h.Gateway, h.Resolver, h.Collector} {
		if !boundedText(layer.Status, MaxSessionEvidenceTextBytes) {
			return errors.New("invalid network health status")
		}
		if err := optionalReason(layer.Reason, false, "network health reason"); err != nil {
			return err
		}
	}
	return nil
}

func (c NetworkCoverage) validate() error {
	for _, metric := range []NetworkMetricCoverage{c.ProxyBytes, c.Connections, c.UpstreamFailures, c.KernelPackets,
		c.GuardDenials, c.MaintenanceQueries, c.MaintenanceBytes, c.SocketInventory, c.BoundaryAttribution} {
		if !slices.Contains(networkCoverageStatus, metric.Status) {
			return errors.New("invalid network coverage status")
		}
		if err := optionalReason(metric.Reason, false, "network coverage reason"); err != nil {
			return err
		}
	}
	return nil
}

func (c NetworkCounters) validate() error {
	for _, value := range []*string{c.SentBytes, c.ReceivedBytes, c.Connections, c.UpstreamFailures, c.DeniedPackets,
		c.ProtectedPackets, c.DeniedDNSQueries, c.DeniedTLSConnections, c.MaintenanceQueries, c.MaintenanceFailures,
		c.IngressDeniedPackets, c.MaintenanceSentBytes, c.MaintenanceReceivedBytes} {
		if value != nil && !unsignedCounter(*value) {
			return errors.New("network counters must be unsigned decimal strings")
		}
	}
	return nil
}

func (l NetworkLoss) validate() error {
	if !unsignedCounter(l.Records) || !unsignedCounter(l.SuppressedAlerts) {
		return errors.New("network loss counters must be unsigned decimal strings")
	}
	if l.OmittedDetails != nil && !unsignedCounter(*l.OmittedDetails) {
		return errors.New("network omitted detail count must be an unsigned decimal string")
	}
	if l.Reasons == nil || len(l.Reasons) > 32 {
		return errors.New("network loss reasons must be present and bounded")
	}
	for _, reason := range l.Reasons {
		if !boundedText(reason, MaxSessionEvidenceTextBytes) {
			return errors.New("invalid network loss reason")
		}
	}
	return nil
}

func (s NetworkSource) validate() error {
	if reference(s.ID, 256, "network source id") != nil || !boundedText(s.Status, MaxSessionEvidenceTextBytes) ||
		!unsignedCounter(s.Sequence) || !unsignedCounter(s.LostRecords) {
		return errors.New("invalid network source")
	}
	for _, at := range []*time.Time{s.ObservedAt, s.LastEventAt} {
		if at != nil && at.IsZero() {
			return errors.New("invalid network source time")
		}
	}
	return optionalReason(s.Reason, false, "network source reason")
}

func (d NetworkDenial) validate(included bool) error {
	if reference(d.ID, 256, "network denial id") != nil || d.At.IsZero() ||
		!boundedText(d.Kind, MaxSessionEvidenceTextBytes) || !slices.Contains(networkDenialBases, d.Basis) ||
		!boundedText(d.Reason, MaxSessionEvidenceTextBytes) || !boundedText(d.Source, MaxSessionEvidenceTextBytes) ||
		!unsignedCounter(d.SourceSequence) {
		return errors.New("invalid network denial")
	}
	if d.Port != nil && (*d.Port < 0 || *d.Port > 65535) {
		return errors.New("invalid network denial port")
	}
	return destinationDisclosure(d.Destination, d.DestinationWithheld, included, "denial")
}

func (c NetworkConnection) validate(included bool) error {
	if reference(c.ID, 256, "network connection id") != nil || !boundedText(c.State, MaxSessionEvidenceTextBytes) ||
		!boundedText(c.Transport, MaxSessionEvidenceTextBytes) || c.ObservedAt.IsZero() {
		return errors.New("invalid network connection")
	}
	if err := optionalReason(c.Reason, false, "network connection reason"); err != nil {
		return err
	}
	if c.StartedAt != nil && c.StartedAt.IsZero() {
		return errors.New("invalid network connection start time")
	}
	for _, value := range []*string{c.SentBytes, c.ReceivedBytes} {
		if value != nil && !unsignedCounter(*value) {
			return errors.New("network connection bytes must be unsigned decimal strings")
		}
	}
	if c.RuleID != nil && (!included || reference(*c.RuleID, 256, "network rule id") != nil) {
		return errors.New("a network rule id is only exported under an included projection")
	}
	return destinationDisclosure(c.Destination, c.DestinationWithheld, included, "connection")
}

func (a NetworkAlert) validate() error {
	if reference(a.ID, 256, "network alert id") != nil || !boundedText(a.Category, MaxSessionEvidenceTextBytes) ||
		!boundedText(a.Severity, MaxSessionEvidenceTextBytes) || !boundedText(a.State, MaxSessionEvidenceTextBytes) ||
		a.FirstSeen.IsZero() || a.LastSeen.IsZero() || a.LastSeen.Before(a.FirstSeen) {
		return errors.New("invalid network alert")
	}
	if err := optionalReason(a.Reason, false, "network alert reason"); err != nil {
		return err
	}
	return optionalReason(a.HealthStatus, false, "network alert health status")
}

func (r NetworkReceipt) validate(filtered bool) error {
	if !slices.Contains(networkReceiptStatuses, r.Status) {
		return errors.New("invalid network receipt status")
	}
	if (r.Status == EvidenceStatusNotFiltered) == filtered {
		return errors.New("network receipt status does not match the session posture")
	}
	if err := optionalReason(r.Reason, r.Status == EvidenceStatusUnavailable, "network receipt reason"); err != nil {
		return err
	}
	if r.Runs == nil {
		return errors.New("network receipt run list must be present")
	}
	if r.Status != EvidenceStatusAvailable {
		if r.PolicyFingerprint != nil || r.AuthorityDigest != nil || r.StartedAt != nil || r.ClosedAt != nil ||
			r.Finality != nil || r.Completeness != nil || r.Scope != nil || r.Counters != nil || r.Coverage != nil ||
			r.Loss != nil || len(r.Runs) != 0 || r.RunCount != nil || r.OmittedRunReferences != nil ||
			r.Projection != nil || r.ReceiptDigest != nil || r.DigestScope != nil {
			return errors.New("an unavailable network receipt carries no aggregate")
		}
		return nil
	}
	if r.PolicyFingerprint == nil || !digestPattern.MatchString(*r.PolicyFingerprint) ||
		r.AuthorityDigest == nil || !digestPattern.MatchString(*r.AuthorityDigest) ||
		r.ReceiptDigest == nil || !digestPattern.MatchString(*r.ReceiptDigest) {
		return errors.New("network receipt requires its policy, authority and receipt digests")
	}
	if r.StartedAt == nil || r.StartedAt.IsZero() || (r.ClosedAt != nil && r.ClosedAt.IsZero()) {
		return errors.New("network receipt requires its session start time")
	}
	if r.Finality == nil || !slices.Contains(networkFinalities, *r.Finality) ||
		r.Completeness == nil || !slices.Contains(networkCompleteness, *r.Completeness) {
		return errors.New("network receipt requires finality and completeness")
	}
	if *r.Finality == "final" && r.ClosedAt == nil {
		return errors.New("a final network receipt requires the session close time")
	}
	if r.Scope == nil || !boundedText(*r.Scope, MaxSessionEvidenceTextBytes) {
		return errors.New("network receipt requires its scope")
	}
	if r.Counters != nil {
		if err := r.Counters.validate(); err != nil {
			return err
		}
	}
	if r.Coverage == nil {
		return errors.New("network receipt requires its coverage")
	}
	if err := r.Coverage.validate(); err != nil {
		return err
	}
	if r.Loss == nil {
		return errors.New("network receipt requires its loss account")
	}
	if err := r.Loss.validate(); err != nil {
		return err
	}
	if len(r.Runs) > MaxSessionEvidenceRunRefs {
		return errors.New("network receipt run references exceed their bound")
	}
	for _, run := range r.Runs {
		if reference(run.RunID, 256, "network run id") != nil || reference(run.GatewayEpoch, 256, "network gateway epoch") != nil ||
			!unsignedCounter(run.Sequence) || run.AsOf.IsZero() ||
			!slices.Contains(networkFinalities, run.Finality) || !slices.Contains(networkCompleteness, run.Completeness) ||
			!digestPattern.MatchString(run.ReceiptDigest) {
			return errors.New("invalid network run reference")
		}
	}
	if r.RunCount == nil || !unsignedCounter(*r.RunCount) ||
		r.OmittedRunReferences == nil || !unsignedCounter(*r.OmittedRunReferences) {
		return errors.New("network receipt requires its run counts")
	}
	if r.Projection == nil || !slices.Contains(networkProjections, *r.Projection) ||
		r.DigestScope == nil || *r.DigestScope != *r.Projection {
		return errors.New("network receipt requires its projection and matching digest scope")
	}
	return nil
}

func (t TaskEvidence) validate() error {
	if !slices.Contains(taskStatuses, t.Status) {
		return errors.New("invalid task evidence status")
	}
	if err := optionalReason(t.Reason, t.Status == EvidenceStatusUnavailable, "task evidence reason"); err != nil {
		return err
	}
	identities := []*string{t.QueueID, t.TaskID, t.ID, t.OfferRef, t.DraftSHA256}
	if t.Status == EvidenceStatusUnbound {
		for _, identity := range identities {
			if identity != nil {
				return errors.New("an unbound task carries no identity")
			}
		}
		if t.Snapshot != nil {
			return errors.New("an unbound task carries no snapshot")
		}
		return nil
	}
	for _, identity := range identities {
		if identity == nil {
			return errors.New("a bound task requires its complete identity")
		}
	}
	if !workspaceCheckpointIdentityPattern.MatchString(*t.QueueID) || !workspaceCheckpointIdentityPattern.MatchString(*t.TaskID) ||
		reference(*t.ID, 256, "task id") != nil || reference(*t.OfferRef, 256, "task offer ref") != nil ||
		!digestPattern.MatchString(*t.DraftSHA256) {
		return errors.New("invalid task binding identity")
	}
	if (t.Status == EvidenceStatusBound) != (t.Snapshot != nil) {
		return errors.New("a task snapshot accompanies exactly a bound task")
	}
	if t.Snapshot != nil {
		return t.Snapshot.validate()
	}
	return nil
}

func (s TaskSnapshot) validate() error {
	if !slices.Contains(workspaceCheckpointTaskStates, s.State) || !digestPattern.MatchString(s.StateSHA256) ||
		!boundedText(s.Title, MaxSessionEvidenceTitleBytes) {
		return errors.New("invalid task snapshot")
	}
	if s.Checklist == nil || len(s.Checklist) > MaxSessionEvidenceChecklist {
		return errors.New("task checklist must be present and bounded")
	}
	for _, item := range s.Checklist {
		if !boundedText(item.Label, MaxSessionEvidenceLabelBytes) {
			return errors.New("invalid task checklist label")
		}
	}
	if len(s.Files) < 1 || len(s.Files) > MaxSessionEvidenceTaskFiles {
		return errors.New("task files must name at least the task document and stay bounded")
	}
	for _, file := range s.Files {
		if !boundedText(file.Path, MaxWorkspaceCheckpointPathBytes) || strings.Contains(file.Path, "\x00") ||
			file.ByteSize < 0 || !digestPattern.MatchString(file.SHA256) {
			return errors.New("invalid task file entry")
		}
	}
	return s.StateNote.validate()
}

func (n TaskNote) validate() error {
	if !slices.Contains(taskNoteStatuses, n.Status) {
		return errors.New("invalid task note status")
	}
	if err := optionalReason(n.Reason, n.Status == EvidenceStatusWithheld, "task note reason"); err != nil {
		return err
	}
	if (n.Status == EvidenceStatusCaptured) != (n.Text != nil) {
		return errors.New("a task note text accompanies exactly a captured note")
	}
	if n.Text != nil && (len(*n.Text) > MaxSessionEvidenceNoteBytes || !utf8.ValidString(*n.Text) || strings.ContainsRune(*n.Text, 0)) {
		return errors.New("task note text is outside its bound")
	}
	if n.Truncated && n.Text == nil {
		return errors.New("only a captured task note can be truncated")
	}
	return nil
}

func destinationDisclosure(destination *string, withheld, included bool, scope string) error {
	if withheld && included {
		return fmt.Errorf("an included projection cannot withhold a %s destination", scope)
	}
	if destination != nil && (withheld || !included || !boundedText(*destination, MaxSessionEvidenceTextBytes)) {
		return fmt.Errorf("a %s destination is only exported under an included projection", scope)
	}
	return nil
}

func optionalReason(reason *string, required bool, field string) error {
	if reason == nil {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	if !boundedText(*reason, MaxSessionEvidenceTextBytes) {
		return fmt.Errorf("invalid %s", field)
	}
	return nil
}

// unsignedCounter accepts exactly the collector's own encoding: an unsigned decimal string with no
// sign, no leading zero and a value that fits in 64 bits, so "18446744073709551616" is refused
// rather than silently wrapped.
func unsignedCounter(value string) bool {
	if !evidenceCounterPattern.MatchString(value) {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func boundedText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) &&
		!strings.ContainsRune(value, 0) && strings.TrimSpace(value) != ""
}
