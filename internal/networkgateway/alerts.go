package networkgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

const (
	DetectorVersion     = 1
	MaxAlertSamples     = 602 // two five-minute windows at the declared one-second cadence
	alertUpdateInterval = 30 * time.Second
	alertQuietWindow    = time.Minute
	alertLongWindow     = 5 * time.Minute
	alertHealthQuiet    = 3 * time.Second
	alertMetricCount    = 6
	alertBehaviorCount  = 7
	alertCategoryCount  = alertBehaviorCount + 4 // health never competes for detail slots
)

const (
	metricProtected = iota
	metricDNS
	metricTLS
	metricPackets
	metricConnections
	metricSent
)

var alertRules = [...]struct {
	category, unit string
	metric         int
	threshold      uint64
	window         time.Duration
}{
	{"protected_destination", "protected_packets", metricProtected, 1, time.Minute},
	{"denial_burst_dns", "denied_dns_queries", metricDNS, 20, time.Minute},
	{"denial_burst_tls", "denied_tls_connections", metricTLS, 10, time.Minute},
	{"denial_burst_packets", "denied_packets", metricPackets, 100, time.Minute},
	{"new_connections", "established_connections", metricConnections, 50, time.Minute},
	{"outbound_volume", "proxy_sent_bytes", metricSent, 64 << 20, alertLongWindow},
	{"outbound_rate_rise", "times_baseline_rate", metricSent, 4, alertLongWindow},
}

type alertSample struct {
	at     BootInstant
	values [alertMetricCount]uint64
	valid  [alertMetricCount]bool
}

type alertSlot struct {
	row, latest                   networkview.Alert
	emitted, badSince, quietSince BootInstant
	readySeen                     bool
}

// Fixed categories and sample/evidence caps bound hostile traffic cardinality.
// This observer has no I/O, grants, provider-progress or workload-control hooks.
// Snapshots retain each category's latest open/resolved revision; consumers must
// replace cumulative state by identity/sequence, not total replayed updates.
type alertDetector struct {
	identity                           Identity
	started                            BootInstant
	history                            []alertSample
	slots                              [alertCategoryCount]alertSlot
	lastSnapshot, sequence, suppressed networkview.Count
	saturated                          bool
}

func alertInput(now BootInstant, s *networkview.Snapshot) alertSample {
	result := alertSample{at: now}
	if s.Counters == nil {
		return result
	}
	counts := s.Counters
	values := [...]*networkview.Count{counts.ProtectedPackets, counts.DeniedDNSQueries, counts.DeniedTLS, counts.DeniedPackets, counts.Connections, counts.SentBytes}
	coverage := [...]networkview.MetricCoverage{s.Coverage.KernelPackets, s.Coverage.GuardDenials, s.Coverage.GuardDenials, s.Coverage.KernelPackets, s.Coverage.Connections, s.Coverage.ProxyBytes}
	for i, value := range values {
		if value != nil {
			result.values[i], result.valid[i] = uint64(*value), coverage[i].Status == "exact"
		}
	}
	return result
}

// window returns measured counter differences, never interpolated counts.
// Its actual span is reported; full windows tolerate only the sampling cadence.
func (d *alertDetector) window(end int, duration time.Duration, metric int, full bool) (uint64, time.Duration, int, bool) {
	if end <= 0 || end >= len(d.history) {
		return 0, 0, 0, false
	}
	last := d.history[end]
	start := end
	for start > 0 && last.at.Sub(d.history[start-1].at) <= duration {
		start--
	}
	span := last.at.Sub(d.history[start].at)
	if span <= 0 || full && span < duration-ObservationStaleAfter {
		return 0, 0, 0, false
	}
	for i := start; i <= end; i++ {
		if !d.history[i].valid[metric] {
			return 0, 0, 0, false
		}
		if i > start {
			before, after := d.history[i-1], d.history[i]
			if !after.at.After(before.at) || after.at.Sub(before.at) > ObservationStaleAfter || after.values[metric] < before.values[metric] {
				return 0, 0, 0, false
			}
		}
	}
	return last.values[metric] - d.history[start].values[metric], span, start, true
}

func (d *alertDetector) observe(now BootInstant, s *networkview.Snapshot) {
	if s.Sequence <= d.lastSnapshot {
		d.project(s)
		return
	}
	d.lastSnapshot = s.Sequence
	continuous := now.Valid() && (len(d.history) == 0 || now.After(d.history[len(d.history)-1].at))
	if !continuous {
		d.history = nil
		for i := range d.slots {
			d.slots[i].quietSince = 0
			d.slots[i].badSince = 0
		}
		d.finish(now, s)
		d.project(s)
		return
	} else {
		if len(d.history) > 0 && now.Sub(d.history[len(d.history)-1].at) > ObservationStaleAfter {
			for i := range d.slots {
				d.slots[i].quietSince, d.slots[i].badSince = 0, 0
			}
		}
		d.history = append(d.history, alertInput(now, s))
		if len(d.history) > MaxAlertSamples {
			d.history = append(d.history[:0], d.history[len(d.history)-MaxAlertSamples:]...)
		}
	}
	for i, rule := range alertRules {
		value, span, start, valid := d.window(len(d.history)-1, rule.window, rule.metric, i == alertBehaviorCount-1)
		if !valid {
			d.slots[i].quietSince = 0
			continue // unknown is not a quiet recovery window
		}
		row := networkview.Alert{Version: DetectorVersion, Category: rule.category, Severity: "warning", WindowMillis: networkview.Count(span.Milliseconds()),
			Threshold: networkview.AlertThreshold{Value: networkview.Count(rule.threshold), Unit: rule.unit}}
		switch rule.metric {
		case metricProtected, metricPackets:
			row.Facts.Packets = networkview.Count(value)
		case metricDNS:
			row.Facts.DNSQueries = networkview.Count(value)
		case metricTLS:
			row.Facts.DeniedTLS = networkview.Count(value)
		case metricConnections:
			row.Facts.Connections = networkview.Count(value)
		case metricSent:
			row.Facts.SentBytes = networkview.Count(value)
		}
		condition := value >= rule.threshold
		if i == alertBehaviorCount-1 {
			baseline, baselineSpan, _, ok := d.window(start, alertLongWindow, metricSent, true)
			if !ok {
				d.slots[i].quietSince = 0
				continue
			}
			row.Threshold.Baseline = networkview.Value(baseline)
			row.Threshold.BaselineWindowMillis = networkview.Count(baselineSpan.Milliseconds())
			condition = value >= 64<<20 && baseline >= 16<<20 && float64(value)/span.Seconds() >= 4*float64(baseline)/baselineSpan.Seconds()
		}
		// A cumulative counter window is not proof that any particular retained
		// denial caused it. Reference this source snapshot, not guessed peers.
		row.EvidenceIDs = []string{"snapshot:" + strconv.FormatUint(uint64(s.Sequence), 10)}
		d.update(i, now, s.AsOf, condition, row, alertQuietWindow)
	}
	health := [...]networkview.Health{s.Health.Enforcer, s.Health.Gateway, s.Health.Resolver, s.Health.Collector}
	names := [...]string{"health_enforcer", "health_gateway", "health_resolver", "health_collector"}
	for i, layer := range health {
		if s.Terminal {
			continue // normal teardown is not a newly failing dependency
		}
		slot := &d.slots[alertBehaviorCount+i]
		if layer.Status == "ready" {
			slot.readySeen, slot.badSince = true, 0
		} else if !slot.badSince.Valid() {
			slot.badSince = now
		}
		if !now.Valid() || !slot.readySeen && now.Sub(d.started) < GuardStartTimeout {
			continue // normal pre-forwarding initialization is not an incident
		}
		delay := alertHealthQuiet
		severity := "warning"
		if i == 0 {
			delay, severity = 0, "critical"
		}
		bad := layer.Status != "ready" && now.Sub(slot.badSince) >= delay
		row := networkview.Alert{Version: DetectorVersion, Category: names[i], Severity: severity, WindowMillis: networkview.Count(max(time.Millisecond, delay).Milliseconds()),
			Threshold: networkview.AlertThreshold{Value: networkview.Count(delay.Milliseconds()), Unit: "unhealthy_milliseconds"}, Facts: networkview.AlertFacts{HealthStatus: layer.Status, Reason: layer.Reason}}
		row.EvidenceIDs = []string{"snapshot:" + strconv.FormatUint(uint64(s.Sequence), 10)}
		d.update(alertBehaviorCount+i, now, s.AsOf, bad, row, alertHealthQuiet)
	}
	d.finish(now, s)
	d.project(s)
}

func (d *alertDetector) finish(now BootInstant, s *networkview.Snapshot) {
	if s.Terminal {
		for i := range d.slots {
			slot := &d.slots[i]
			if slot.row.ID == "" || slot.row.Terminal {
				continue
			}
			row := slot.latest
			row.Terminal = true
			d.emit(slot, now, row)
		}
	}
}

func (d *alertDetector) update(index int, now BootInstant, at time.Time, condition bool, row networkview.Alert, quiet time.Duration) {
	slot := &d.slots[index]
	if slot.row.Terminal {
		return
	}
	if condition {
		slot.quietSince = 0
		row.State, row.LastSeen = "open", at
		row.FirstSeen = slot.latest.FirstSeen
		if slot.latest.State != "open" {
			row.FirstSeen = at
		}
	} else {
		if slot.latest.State == "" || slot.latest.State == "resolved" && slot.row.State == "resolved" {
			return
		}
		if slot.latest.State == "open" {
			if !slot.quietSince.Valid() {
				slot.quietSince = now
			}
			if now.Sub(slot.quietSince) < quiet {
				return
			}
		}
		row = slot.latest
		row.State = "resolved"
	}
	slot.latest = row
	if slot.row.ID != "" && now.Sub(slot.emitted) < alertUpdateInterval {
		if !networkview.Add(&d.suppressed, 1) {
			d.saturated = true
		}
		return
	}
	d.emit(slot, now, row)
}

func (d *alertDetector) emit(slot *alertSlot, now BootInstant, row networkview.Alert) {
	if !networkview.Add(&d.sequence, 1) {
		d.saturated = true
		return // never publish changed evidence under a repeated saturated cursor
	}
	digest := sha256.Sum256([]byte("coop-network-alert-v1\x00" + d.identity.RunID + "\x00" + d.identity.Epoch + "\x00" + row.Category))
	row.ID, row.Sequence = hex.EncodeToString(digest[:16]), d.sequence
	slot.row, slot.emitted = row, now
}

func (d *alertDetector) project(s *networkview.Snapshot) {
	s.Alerts = nil
	for _, slot := range d.slots {
		if slot.row.ID != "" {
			row := slot.row
			row.EvidenceIDs = slices.Clone(row.EvidenceIDs)
			if row.Threshold.Baseline != nil {
				row.Threshold.Baseline = networkview.Value(uint64(*row.Threshold.Baseline))
			}
			s.Alerts = append(s.Alerts, row)
		}
	}
	s.Loss.SuppressedAlerts = d.suppressed
	if d.saturated {
		s.Loss.Unknown = true
		s.Loss.Reasons = append(s.Loss.Reasons, "alert_sequence_or_suppression_saturated")
	}
}
