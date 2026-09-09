package networkstate

import (
	"errors"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

// Freshness describes how recent the retained observation is. None of these
// words is a claim that a workload is running, exited or reachable.
const (
	FreshnessFresh       = "fresh"
	FreshnessStale       = "stale"
	FreshnessNotObserved = "not-observed"
	FreshnessTerminal    = "terminal"
)

// ObservationMaxAge bounds how old the last observation may be before its
// point-in-time metrics stop being reported as current measurements.
const ObservationMaxAge = 3 * time.Second

// CurrentNetwork carries measurements of the last observation only, never a
// cumulative total. A nil CurrentNetwork means unknown, never "no traffic", and
// a nil field inside it stays nil: a missing metric is not a measured zero.
type CurrentNetwork struct {
	Rate                 *networkview.Rate  `json:"rate"`
	LiveConnections      *networkview.Count `json:"live_connections"`
	UnknownConnections   *networkview.Count `json:"unknown_connections"`
	PendingConnections   *networkview.Count `json:"pending_connections"`
	KernelClosingSockets *networkview.Count `json:"kernel_closing_sockets"`
}

// Inspection is the shared read-only view of one execution's retained evidence.
// It is deliberately built from projections only: the owner-private execution
// record — project path, endpoint, daemon and supervisor identity, resource and
// artifact custody — has no field here and must never gain one.
type Inspection struct {
	// Built from the same retained read, but not an additional wire field.
	AggregateObservation networkview.RunObservation `json:"-"`
	Version              int                        `json:"version"`
	Revision             networkview.Count          `json:"revision"`
	Observed             networkview.Snapshot       `json:"observed"`
	Receipt              *networkview.Receipt       `json:"receipt,omitempty"`
	Freshness            string                     `json:"freshness"`
	ReadAt               time.Time                  `json:"read_at"`
	Current              *CurrentNetwork            `json:"current"`
	Cleanup              string                     `json:"cleanup_outcome"`
}

// Inspect projects retained evidence at the caller's read time. It reads no
// runtime, DNS, configuration or credential, and publishes nothing: retained
// snapshots, totals and sealed digests are left exactly as they were stored.
func (e *Evidence) Inspect(id string, now time.Time, exportDestinations bool) (Inspection, error) {
	if now.IsZero() {
		return Inspection{}, errors.New("network inspection requires the reader's observation time")
	}
	record, err := e.Execution(id)
	if err != nil {
		return Inspection{}, err
	}
	return inspect(record, now, exportDestinations)
}

func inspect(record Execution, now time.Time, exportDestinations bool) (Inspection, error) {
	observation, err := aggregateObservation(record, exportDestinations)
	if err != nil {
		return Inspection{}, err
	}
	out := Inspection{Version: networkview.Version, Revision: record.Revision, Observed: record.Snapshot.Project(exportDestinations),
		AggregateObservation: observation, ReadAt: now.UTC(), Cleanup: inspectedCleanup(record)}
	if record.Receipt != nil {
		receipt, err := record.Receipt.Project(exportDestinations)
		if err != nil {
			return Inspection{}, errors.New("retained network evidence cannot be projected")
		}
		out.Receipt = &receipt
	}
	out.Freshness = observationFreshness(record.Snapshot, record.Receipt != nil, out.ReadAt)
	if out.Freshness == FreshnessFresh {
		out.Current = currentNetwork(record.Snapshot)
	}
	return out, nil
}

// Provisional aggregates use retained execution identity, not fabricated run
// start/ownership inferred from a snapshot. Ordinary Receipt stays nil until
// sealed so existing inspect/receipt callers keep their finality contract.
func aggregateObservation(record Execution, exportDestinations bool) (networkview.RunObservation, error) {
	receipt := networkview.Receipt{Version: networkview.Version, ID: record.ID, Snapshot: record.Snapshot,
		StartedAt: record.StartedAt, Finality: "provisional", Completeness: provisionalCompleteness(record.Snapshot),
		Cleanup: inspectedCleanup(record), Runtime: record.Runtime, GatewayImage: record.GatewayImage,
		SessionID: record.SessionID, AttemptID: record.AttemptID, AuthorityDigest: record.AuthorityDigest,
		CollectorVersion: "gateway-v1", BundleReferences: record.BundleReferences}
	if record.Purpose == SessionUnobservedPurpose {
		receipt.CollectorVersion = ""
	}
	if record.Receipt != nil {
		receipt = *record.Receipt
	}
	projected, err := receipt.Project(exportDestinations)
	if err != nil {
		return networkview.RunObservation{}, errors.New("retained network evidence cannot be projected")
	}
	return networkview.RunObservation{Revision: record.Revision, Receipt: projected}, nil
}

func provisionalCompleteness(snapshot networkview.Snapshot) string {
	if snapshot.Sequence == 0 {
		return "unknown"
	}
	if snapshot.Loss.Unknown || snapshot.Loss.Records != 0 || snapshot.Loss.DetailTruncated {
		return "partial"
	}
	for _, coverage := range []networkview.MetricCoverage{snapshot.Coverage.ProxyBytes, snapshot.Coverage.Connections,
		snapshot.Coverage.UpstreamFailures, snapshot.Coverage.KernelPackets, snapshot.Coverage.GuardDenials,
		snapshot.Coverage.MaintenanceQueries, snapshot.Coverage.MaintenanceBytes, snapshot.Coverage.SocketInventory, snapshot.Coverage.BoundaryAttribution} {
		if coverage.Status != "exact" {
			return "partial"
		}
	}
	return "complete"
}

// A terminal epoch or a sealed receipt ends observation, so aging it would
// invent a heartbeat that nothing is expected to send. A future or too-old
// observation is stale: its point-in-time metrics are unknown, not zero.
func observationFreshness(snapshot networkview.Snapshot, final bool, now time.Time) string {
	if final || snapshot.Terminal {
		return FreshnessTerminal
	}
	if snapshot.Sequence == 0 || snapshot.AsOf.IsZero() {
		return FreshnessNotObserved
	}
	age := now.Sub(snapshot.AsOf)
	if age < 0 || age > ObservationMaxAge {
		return FreshnessStale
	}
	return FreshnessFresh
}

func currentNetwork(snapshot networkview.Snapshot) *CurrentNetwork {
	pending := snapshot.PendingConnections
	return &CurrentNetwork{Rate: copyOf(snapshot.Rate), LiveConnections: copyOf(snapshot.LiveConnections),
		UnknownConnections: copyOf(snapshot.UnknownConnections), PendingConnections: &pending,
		KernelClosingSockets: copyOf(snapshot.KernelClosingSockets)}
}

// Cleanup repeats the receipt's rule instead of reading the sealed outcome: a
// resource or private artifact is complete only where custody confirmed its
// absence. Cleanup that finishes after sealing shows here; the immutable
// receipt keeps the outcome it was sealed with.
func inspectedCleanup(record Execution) string {
	if record.Purpose == SessionUnobservedPurpose {
		if !record.SessionWorkloadGone || record.RunFiles.State != "gone" {
			return "pending"
		}
		return "complete"
	}
	for _, resource := range record.Resources {
		if resource.State != "gone" {
			return "pending"
		}
	}
	if record.LaunchConfig.State != "gone" || record.RunFiles.State != "gone" {
		return "pending"
	}
	return "complete"
}

func copyOf[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
