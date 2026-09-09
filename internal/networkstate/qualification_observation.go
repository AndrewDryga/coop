package networkstate

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/networkview"
)

const MaxQualificationEvidenceBytes = 2 << 20

// QualificationObservation is derived by storage from the sealed receipt, never
// accepted as a caller's replacement for measured coverage. It retains the
// distinction between exact source totals and sampled connection attribution.
type QualificationObservation struct {
	Scope          string               `json:"scope"`
	Coverage       networkview.Coverage `json:"coverage"`
	Loss           networkview.Loss     `json:"loss"`
	HistoryReasons []string             `json:"history_reasons"`
}

// QualificationEvidence retains the exact sealed terminal frame plus bounded
// facts from the trusted host harness (for example independent transfer meters).
// It is private evidence, not a caller-supplied success or permission assertion.
type QualificationEvidence struct {
	Version  int                  `json:"version"`
	Snapshot networkview.Snapshot `json:"network"`
	Facts    map[string]string    `json:"host_facts"`
}

func validateQualificationEvidence(data []byte, receipt *networkview.Receipt) error {
	if len(data) == 0 || len(data) > MaxQualificationEvidenceBytes || receipt == nil {
		return errors.New("qualification requires bounded evidence from its sealed receipt")
	}
	var evidence QualificationEvidence
	if strictJSON(data, &evidence) != nil || evidence.Version != 1 || !equalJSON(evidence.Snapshot, receipt.Snapshot) ||
		len(evidence.Facts) == 0 || len(evidence.Facts) > 128 {
		return errors.New("qualification evidence differs from its sealed terminal observation")
	}
	remaining := 64 << 10
	for name, value := range evidence.Facts {
		if name == "" || len(name) > 128 || len(value) > 8<<10 || !utf8.ValidString(name) || !utf8.ValidString(value) || strings.ContainsAny(name, "\x00\r\n") || strings.ContainsRune(value, '\x00') {
			return errors.New("invalid qualification host evidence fact")
		}
		remaining -= len(name) + len(value)
		if remaining < 0 {
			return errors.New("qualification host evidence facts exceed their byte limit")
		}
	}
	return nil
}

func qualificationObservation(receipt *networkview.Receipt) (*QualificationObservation, error) {
	if receipt == nil {
		return nil, errors.New("qualification observation requires a sealed receipt")
	}
	// Project deep-copies Loss pointers/slices. The observation must not alias a
	// mutable caller receipt or reuse the pre-seal Execution.Snapshot.
	snapshot := receipt.Snapshot.Project(true)
	observation := &QualificationObservation{Scope: snapshot.Scope, Coverage: snapshot.Coverage, Loss: snapshot.Loss, HistoryReasons: []string{}}
	for _, row := range snapshot.Connections {
		if row.NameSource == "unattributed-history" {
			observation.HistoryReasons = append(observation.HistoryReasons, row.Reason)
		}
	}
	slices.Sort(observation.Loss.Reasons)
	observation.Loss.Reasons = slices.Compact(observation.Loss.Reasons)
	slices.Sort(observation.HistoryReasons)
	observation.HistoryReasons = slices.Compact(observation.HistoryReasons)
	if err := validateQualificationObservation(observation); err != nil {
		return nil, err
	}
	return observation, nil
}

func validateQualificationObservation(observation *QualificationObservation) error {
	if observation == nil || observation.Scope == "" || len(observation.Scope) > 128 || !utf8.ValidString(observation.Scope) ||
		len(observation.Loss.Reasons) > 32 || len(observation.HistoryReasons) > 32 {
		return errors.New("invalid qualification observation summary")
	}
	for _, reasons := range [][]string{observation.Loss.Reasons, observation.HistoryReasons} {
		for i, reason := range reasons {
			if !safeRecordToken(reason, 128) || i > 0 && reasons[i-1] >= reason {
				return errors.New("invalid qualification observation reason set")
			}
		}
	}
	c := observation.Coverage
	for _, metric := range []networkview.MetricCoverage{c.ProxyBytes, c.Connections, c.UpstreamFailures, c.KernelPackets, c.GuardDenials,
		c.MaintenanceQueries, c.MaintenanceBytes, c.SocketInventory, c.BoundaryAttribution} {
		// A failure before the first observation has no measurements. Preserve
		// that absence; functional cases still require observed exact coverage.
		if observation.Scope == "not-observed" && metric == (networkview.MetricCoverage{}) {
			continue
		}
		if !slices.Contains([]string{"exact", "lower-bound", "unavailable"}, metric.Status) || len(metric.Reason) > 128 ||
			metric.Reason != "" && !safeRecordToken(metric.Reason, 128) {
			return errors.New("invalid qualification metric coverage")
		}
	}
	return nil
}

// Functional qualification permits only the documented sampled-history gap.
// It never relabels that receipt complete or excuses another anomaly alongside it.
func validateFunctionalQualification(r Execution) error {
	if r.Receipt == nil {
		return errors.New("functional qualification requires a sealed receipt")
	}
	s := r.Receipt.Snapshot
	fail := errors.New("functional qualification has incomplete sources or inconsistent sampled attribution")
	if r.Receipt.Finality != "final" || r.Receipt.EndedAt == nil || !s.Terminal || !r.ObserverAfterWorkload ||
		s.Scope != "proxy-streams-and-sampled-tcp-sockets" || s.Sequence == 0 || s.AsOf.IsZero() ||
		s.PendingConnections != 0 || s.StaleConnections != 0 || s.UnknownConnections == nil || *s.UnknownConnections != 0 ||
		s.LiveConnections == nil || s.KernelClosingSockets == nil || s.Loss.Records != 0 || s.Loss.DetailTruncated ||
		s.Loss.OmittedDetails == nil || *s.Loss.OmittedDetails != 0 || s.Loss.SuppressedAlerts != 0 {
		return fail
	}
	c := s.Coverage
	for _, metric := range []networkview.MetricCoverage{c.ProxyBytes, c.Connections, c.UpstreamFailures, c.KernelPackets, c.GuardDenials, c.MaintenanceQueries, c.MaintenanceBytes, c.SocketInventory} {
		if metric.Status != "exact" || metric.Reason != "" {
			return fail
		}
	}
	if s.Counters == nil {
		return fail
	}
	n := s.Counters
	for _, counter := range []*networkview.Count{n.SentBytes, n.ReceivedBytes, n.Connections, n.UpstreamFailures, n.DeniedPackets, n.ProtectedPackets,
		n.DeniedDNSQueries, n.DeniedTLS, n.MaintenanceQueries, n.MaintenanceFailures, n.IngressDenials, n.MaintenanceSentBytes, n.MaintenanceReceivedBytes} {
		if counter == nil {
			return fail
		}
	}
	if s.Health.Enforcer.Status != "ready" || s.Health.Enforcer.Reason != "" || s.Health.Gateway.Status != "stopped" || s.Health.Gateway.Reason != "" ||
		s.Health.Resolver.Status != "ready" || s.Health.Resolver.Reason != "" || len(s.Sources) != 4 {
		return fail
	}
	expected := map[string]networkview.Health{"guard": {Status: "live"}, "proxy": {Status: "stopped", Reason: "drained"},
		"kernel": {Status: "live"}, "socket-inventory": {Status: "sampled", Reason: "sampled-not-packet-correlated"}}
	for _, source := range s.Sources {
		want, present := expected[source.ID]
		if !present || source.Status != want.Status || source.Reason != want.Reason || source.ObservedAt == nil || source.ObservedAt.IsZero() ||
			source.ObservedAt.After(s.AsOf) || s.AsOf.Sub(*source.ObservedAt) > 3*time.Second || source.Lost != 0 || source.Unknown {
			return fail
		}
		delete(expected, source.ID)
	}
	history := 0
	for _, row := range s.Connections {
		if row.NameSource == "unattributed-history" {
			if !slices.Contains([]string{"socket_join_expired", "socket_join_terminal"}, row.Reason) || !row.Partial || row.State != "unknown" ||
				row.StartedAt != nil || row.SentBytes != nil || row.ReceivedBytes != nil || row.Rate != nil || row.ConnectMillis != nil {
				return fail
			}
			history++
		} else if row.Partial && !(row.NameSource == "socket-inventory" && row.State == "kernel-closing" && row.Reason == "kernel_closing_remnant" &&
			row.StartedAt == nil && row.SentBytes == nil && row.ReceivedBytes == nil && row.Rate == nil && row.ConnectMillis == nil) {
			return fail
		}
	}
	if history == 0 {
		if r.Receipt.Completeness != "complete" || s.Loss.Unknown || len(s.Loss.Reasons) != 0 || c.BoundaryAttribution.Status != "exact" || c.BoundaryAttribution.Reason != "" ||
			s.Availability != "available" || s.Health.Collector.Status != "ready" || s.Health.Collector.Reason != "" {
			return fail
		}
		return nil
	}
	if r.TrialCase == "observation-baseline" || r.Receipt.Completeness != "partial" || !s.Loss.Unknown ||
		!slices.Equal(s.Loss.Reasons, []string{"unattributed_socket"}) || c.BoundaryAttribution.Status != "lower-bound" || c.BoundaryAttribution.Reason != "unattributed_socket" ||
		s.Availability != "degraded" || s.Health.Collector.Status != "degraded" || s.Health.Collector.Reason != "observation_gap" {
		return fail
	}
	return nil
}

func qualificationEvidenceBytes(snapshot networkview.Snapshot, facts map[string]string) ([]byte, error) {
	return json.Marshal(QualificationEvidence{Version: 1, Snapshot: snapshot, Facts: facts})
}
