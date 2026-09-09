package networkview

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"time"

	"github.com/AndrewDryga/coop/internal/egress"
)

// MaxSessionRunReferences bounds one receipt, not the number of valid session
// attempts. All owned runs contribute to totals even when references truncate.
const MaxSessionRunReferences = 2048

// SessionNetworkIdentity comes from captured session authority and the durable
// owned-run registry, never from a read request or untrusted runtime labels.
type SessionNetworkIdentity struct {
	ID, PolicyFingerprint, AuthorityDigest string
	Mode                                   egress.Mode
	StartedAt                              time.Time
	ClosedAt                               *time.Time
	RunsComplete                           bool
}

// RunObservation carries the execution revision as well as its cumulative
// snapshot sequence: sealing a receipt need not create another traffic sample.
type RunObservation struct {
	Revision Count
	Receipt  Receipt
}

type RunReceiptReference struct {
	RunID         string    `json:"run_id"`
	Epoch         string    `json:"gateway_epoch"`
	Sequence      Count     `json:"sequence"`
	AsOf          time.Time `json:"as_of"`
	Finality      string    `json:"finality"`
	Completeness  string    `json:"completeness"`
	ReceiptDigest string    `json:"receipt_digest"`
}

// SessionReceipt is a projection, not authority or a packet archive. Its run
// references contain only digests of the same disclosure projection as itself.
type SessionReceipt struct {
	Version           int                   `json:"version"`
	SessionID         string                `json:"session_id"`
	PolicyFingerprint string                `json:"policy_fingerprint"`
	AuthorityDigest   string                `json:"authority_digest"`
	Mode              egress.Mode           `json:"mode"`
	StartedAt         time.Time             `json:"started_at"`
	ClosedAt          *time.Time            `json:"closed_at,omitempty"`
	Finality          string                `json:"finality"`
	Completeness      string                `json:"completeness"`
	Scope             string                `json:"scope"`
	Counters          *Counters             `json:"counters"`
	Coverage          Coverage              `json:"coverage"`
	Loss              Loss                  `json:"loss"`
	Runs              []RunReceiptReference `json:"runs"`
	RunCount          Count                 `json:"run_count"`
	OmittedReferences Count                 `json:"omitted_run_references"`
	Projection        string                `json:"projection"`
	Digest            string                `json:"digest"`
	DigestScope       string                `json:"digest_scope"`
}

// AggregateSessionNetwork reduces an owner-store stream ordered by run ID,
// epoch and revision. It retains only the current run and bounded references,
// not the whole session history. Duplicate cumulative deliveries cannot add
// bytes; missing metrics remain missing or explicitly lower-bound, never zero.
func AggregateSessionNetwork(identity SessionNetworkIdentity, observations iter.Seq2[RunObservation, error], exportDestinations bool) (SessionReceipt, error) {
	fail := func() (SessionReceipt, error) {
		return SessionReceipt{}, errors.New("invalid or conflicting session network observations")
	}
	if observations == nil || identity.ID == "" || identity.StartedAt.IsZero() || identity.AuthorityDigest == "" || identity.PolicyFingerprint == "" ||
		identity.ClosedAt != nil && identity.ClosedAt.IsZero() {
		return fail()
	}
	if _, err := egress.ParseMode(string(identity.Mode)); err != nil {
		return fail()
	}
	out := SessionReceipt{Version: Version, SessionID: identity.ID, PolicyFingerprint: identity.PolicyFingerprint,
		AuthorityDigest: identity.AuthorityDigest, Mode: identity.Mode, StartedAt: identity.StartedAt.UTC(), ClosedAt: clone(identity.ClosedAt),
		Finality: "provisional", Completeness: "unknown", Scope: "not-observed", Projection: "destinations-withheld"}
	if exportDestinations {
		out.Projection = "destinations-included"
	}
	if out.ClosedAt != nil {
		*out.ClosedAt = out.ClosedAt.UTC()
	}
	allFinal, allComplete, observed := identity.RunsComplete, identity.RunsComplete, false
	omissionsUnknown := false
	if !identity.RunsComplete {
		out.Loss.Unknown = true
		out.Loss.Reasons = []string{"owned_run_set_incomplete"}
	}
	for observation, streamErr := range latestSessionRuns(identity, observations) {
		if streamErr != nil {
			return SessionReceipt{}, streamErr
		}
		r, err := observation.Receipt.Project(exportDestinations)
		if err != nil {
			return fail()
		}
		s := r.Snapshot
		scope := s.Scope
		if scope != "not-observed" && scope != "proxy-streams-and-sampled-tcp-sockets" {
			scope = "unavailable"
		}
		if out.RunCount == 0 {
			out.Scope = scope
		} else if out.Scope != scope {
			out.Scope = "mixed"
		}
		if len(out.Runs) < MaxSessionRunReferences {
			out.Runs = append(out.Runs, RunReceiptReference{RunID: r.ID, Epoch: s.Epoch,
				Sequence: s.Sequence, AsOf: s.AsOf, Finality: r.Finality, Completeness: r.Completeness, ReceiptDigest: r.Digest})
		} else {
			out.OmittedReferences++
		}
		allFinal = allFinal && r.Finality == "final"
		allComplete = allComplete && r.Completeness == "complete"
		if s.Counters != nil {
			if out.Counters == nil {
				out.Counters = &Counters{}
			}
		}
		coverage := s.Coverage
		if !addSessionCounters(out.Counters, s.Counters, &coverage) {
			out.Loss.Unknown = true
			addSessionLossReason(&out.Loss, "counter_overflow")
		}
		mergeSessionCoverage(&out.Coverage, coverage, out.RunCount != 0)
		out.RunCount++
		observed = observed || s.Sequence != 0
		recordsExact := Add(&out.Loss.Records, uint64(s.Loss.Records))
		alertsExact := Add(&out.Loss.SuppressedAlerts, uint64(s.Loss.SuppressedAlerts))
		if !recordsExact || !alertsExact {
			out.Loss.Unknown = true
			addSessionLossReason(&out.Loss, "counter_overflow")
		}
		out.Loss.Unknown = out.Loss.Unknown || s.Loss.Unknown
		out.Loss.DetailTruncated = out.Loss.DetailTruncated || s.Loss.DetailTruncated
		if s.Loss.Records != 0 || s.Loss.Unknown {
			addSessionLossReason(&out.Loss, "run_observation_loss")
		}
		omissionsUnknown = omissionsUnknown || s.Loss.DetailTruncated && s.Loss.OmittedDetails == nil
		if s.Loss.OmittedDetails != nil && !omissionsUnknown {
			if out.Loss.OmittedDetails == nil {
				out.Loss.OmittedDetails = Value(0)
			}
			if !Add(out.Loss.OmittedDetails, uint64(*s.Loss.OmittedDetails)) {
				out.Loss.Unknown = true
				addSessionLossReason(&out.Loss, "counter_overflow")
				omissionsUnknown = true
			}
		}
		if omissionsUnknown {
			out.Loss.OmittedDetails = nil
		}
	}
	if !observed {
		out.Scope = "not-observed"
	}
	if out.RunCount == 0 {
		mergeSessionCoverage(&out.Coverage, Coverage{}, false)
	}
	if !identity.RunsComplete {
		mergeSessionCoverage(&out.Coverage, Coverage{}, true)
	}
	if identity.ClosedAt != nil && allFinal {
		out.Finality = "final"
	}
	if observed {
		out.Completeness = "partial"
		if allComplete && sessionCoverageExact(out.Coverage) && !out.Loss.Unknown && out.Loss.Records == 0 && !out.Loss.DetailTruncated {
			out.Completeness = "complete"
		}
	}
	slices.Sort(out.Loss.Reasons)
	out.Loss.Reasons = slices.Compact(out.Loss.Reasons)
	out.DigestScope = out.Projection
	data, err := json.Marshal(out)
	if err != nil {
		return fail()
	}
	digest := sha256.Sum256(data)
	out.Digest = hex.EncodeToString(digest[:])
	return out, nil
}

func latestSessionRuns(identity SessionNetworkIdentity, observations iter.Seq2[RunObservation, error]) iter.Seq2[RunObservation, error] {
	return func(yield func(RunObservation, error) bool) {
		var previous RunObservation
		fail := func() { yield(RunObservation{}, errors.New("invalid or conflicting session network observation")) }
		for current, err := range observations {
			if err != nil {
				yield(RunObservation{}, err)
				return
			}
			r := current.Receipt
			if current.Revision == 0 || r.Version != Version || r.Snapshot.Version != Version || r.ID == "" || r.ID != r.Snapshot.RunID || r.Snapshot.Epoch == "" ||
				r.SessionID != identity.ID || r.AuthorityDigest != identity.AuthorityDigest || r.Snapshot.PolicyFingerprint != identity.PolicyFingerprint ||
				r.Snapshot.Mode != identity.Mode || r.StartedAt.IsZero() ||
				(r.Finality != "final" && r.Finality != "provisional") || r.Finality == "final" && r.EndedAt == nil ||
				!slices.Contains([]string{"complete", "partial", "unknown"}, r.Completeness) {
				fail()
				return
			}
			if previous.Revision != 0 {
				order := cmp.Compare(r.ID, previous.Receipt.ID)
				if order == 0 {
					order = cmp.Compare(r.Snapshot.Epoch, previous.Receipt.Snapshot.Epoch)
				}
				if order < 0 {
					fail()
					return
				}
				if order == 0 {
					if !validSessionObservationAdvance(previous, current) {
						fail()
						return
					}
				} else if !yield(previous, nil) {
					return
				}
			}
			previous = current
		}
		if previous.Revision != 0 {
			yield(previous, nil)
		}
	}
}

func validSessionObservationAdvance(previous, current RunObservation) bool {
	if current.Revision < previous.Revision || current.Receipt.Snapshot.Sequence < previous.Receipt.Snapshot.Sequence {
		return false
	}
	a, b := previous.Receipt, current.Receipt
	a.Digest, b.Digest = "", ""
	// Provided hashes never decide equivalence and never become outbound references.
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	if aErr != nil || bErr != nil {
		return false
	}
	if bytes.Equal(aJSON, bJSON) {
		return true
	}
	if current.Revision == previous.Revision || a.Finality == "final" {
		return false
	}
	if a.Snapshot.Sequence == b.Snapshot.Sequence {
		priorSnapshot, priorErr := json.Marshal(a.Snapshot)
		nextSnapshot, nextErr := json.Marshal(b.Snapshot)
		if priorErr != nil || nextErr != nil {
			return false
		}
		if bytes.Equal(priorSnapshot, nextSnapshot) {
			return true
		}
		// Host sealing can record observation loss without a new producer sample.
		// Another provisional update at that sequence cannot rewrite the sample.
		priorCounters, priorErr := json.Marshal(a.Snapshot.Counters)
		nextCounters, nextErr := json.Marshal(b.Snapshot.Counters)
		return priorErr == nil && nextErr == nil && bytes.Equal(priorCounters, nextCounters) &&
			a.Snapshot.AsOf.Equal(b.Snapshot.AsOf) && a.Snapshot.ElapsedMillis == b.Snapshot.ElapsedMillis &&
			a.Snapshot.Scope == b.Snapshot.Scope && a.Finality == "provisional" && b.Finality == "final"
	}
	return true
}

func sessionCoverageExact(c Coverage) bool {
	for _, metric := range []MetricCoverage{c.ProxyBytes, c.Connections, c.UpstreamFailures, c.KernelPackets,
		c.GuardDenials, c.MaintenanceQueries, c.MaintenanceBytes, c.SocketInventory, c.BoundaryAttribution} {
		if metric.Status != "exact" {
			return false
		}
	}
	return true
}

func addSessionLossReason(loss *Loss, reason string) {
	if !slices.Contains(loss.Reasons, reason) {
		loss.Reasons = append(loss.Reasons, reason)
	}
}

func addSessionCounters(dst, source *Counters, coverage *Coverage) bool {
	if dst == nil {
		dst = &Counters{}
	}
	if source == nil {
		source = &Counters{}
	}
	exact := true
	type counterPair struct {
		dst    **Count
		source *Count
	}
	for _, group := range []struct {
		coverage *MetricCoverage
		pairs    []counterPair
	}{
		{&coverage.ProxyBytes, []counterPair{{&dst.SentBytes, source.SentBytes}, {&dst.ReceivedBytes, source.ReceivedBytes}}},
		{&coverage.Connections, []counterPair{{&dst.Connections, source.Connections}}},
		{&coverage.UpstreamFailures, []counterPair{{&dst.UpstreamFailures, source.UpstreamFailures}}},
		{&coverage.KernelPackets, []counterPair{{&dst.DeniedPackets, source.DeniedPackets}, {&dst.ProtectedPackets, source.ProtectedPackets}, {&dst.IngressDenials, source.IngressDenials}}},
		{&coverage.GuardDenials, []counterPair{{&dst.DeniedDNSQueries, source.DeniedDNSQueries}, {&dst.DeniedTLS, source.DeniedTLS}}},
		{&coverage.MaintenanceQueries, []counterPair{{&dst.MaintenanceQueries, source.MaintenanceQueries}, {&dst.MaintenanceFailures, source.MaintenanceFailures}}},
		{&coverage.MaintenanceBytes, []counterPair{{&dst.MaintenanceSentBytes, source.MaintenanceSentBytes}, {&dst.MaintenanceReceivedBytes, source.MaintenanceReceivedBytes}}},
	} {
		known, overflow := 0, false
		for _, pair := range group.pairs {
			if pair.source == nil {
				continue
			}
			known++
			if *pair.dst == nil {
				*pair.dst = Value(0)
			}
			if !Add(*pair.dst, uint64(*pair.source)) {
				exact, overflow = false, true
			}
		}
		if known == 0 {
			*group.coverage = MetricCoverage{Status: "unavailable"}
		} else if group.coverage.Status == "exact" && (known != len(group.pairs) || overflow) {
			*group.coverage = MetricCoverage{Status: "lower-bound"}
		}
	}
	return exact
}

func mergeSessionCoverage(dst *Coverage, source Coverage, initialized bool) {
	for _, pair := range [][2]*MetricCoverage{
		{&dst.ProxyBytes, &source.ProxyBytes}, {&dst.Connections, &source.Connections},
		{&dst.UpstreamFailures, &source.UpstreamFailures}, {&dst.KernelPackets, &source.KernelPackets},
		{&dst.GuardDenials, &source.GuardDenials}, {&dst.MaintenanceQueries, &source.MaintenanceQueries},
		{&dst.MaintenanceBytes, &source.MaintenanceBytes}, {&dst.SocketInventory, &source.SocketInventory},
		{&dst.BoundaryAttribution, &source.BoundaryAttribution},
	} {
		status := pair[1].Status
		if status != "exact" && status != "lower-bound" {
			status = "unavailable"
		}
		if initialized {
			switch {
			case pair[0].Status == "exact" && status == "exact":
			case pair[0].Status == "unavailable" && status == "unavailable":
			default:
				status = "lower-bound"
			}
		}
		*pair[0] = MetricCoverage{Status: status}
		if status != "exact" {
			pair[0].Reason = "run_measurement_incomplete"
		}
	}
}
