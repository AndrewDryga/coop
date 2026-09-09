package networkgateway

import (
	"reflect"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/networkview"
)

func alertFixture(sequence int, now BootInstant) networkview.Snapshot {
	return networkview.Snapshot{Sequence: networkview.Count(sequence), AsOf: time.Unix(0, int64(now)),
		Coverage: networkview.Coverage{ProxyBytes: exactCoverage(), Connections: exactCoverage(), GuardDenials: exactCoverage(), KernelPackets: exactCoverage()},
		Counters: &networkview.Counters{SentBytes: networkview.Value(0), Connections: networkview.Value(0), DeniedDNSQueries: networkview.Value(0),
			DeniedTLS: networkview.Value(0), DeniedPackets: networkview.Value(0), ProtectedPackets: networkview.Value(0)},
		Health: networkview.HealthLayers{Enforcer: networkview.Health{Status: "ready"}, Gateway: networkview.Health{Status: "ready"},
			Resolver: networkview.Health{Status: "ready"}, Collector: networkview.Health{Status: "ready"}}}
}

func alertByCategory(s networkview.Snapshot, category string) *networkview.Alert {
	for _, row := range s.Alerts {
		if row.Category == category {
			return &row
		}
	}
	return nil
}

func TestAlertsThresholdsReportActualWindowAndDoNotMixUnits(t *testing.T) {
	for i, rule := range alertRules[:alertBehaviorCount-1] {
		t.Run(rule.category, func(t *testing.T) {
			now := testBootNow()
			d := alertDetector{started: now}
			s := alertFixture(1, now)
			d.observe(now, &s)
			now = now.Add(time.Second)
			s = alertFixture(2, now)
			values := [...]*networkview.Count{s.Counters.ProtectedPackets, s.Counters.DeniedDNSQueries, s.Counters.DeniedTLS, s.Counters.DeniedPackets, s.Counters.Connections, s.Counters.SentBytes}
			*values[i] = networkview.Count(rule.threshold)
			d.observe(now, &s)
			row := alertByCategory(s, rule.category)
			if row == nil || len(s.Alerts) != 1 || row.WindowMillis != 1000 || row.Threshold.Value != networkview.Count(rule.threshold) || row.Threshold.Unit != rule.unit || row.State != "open" {
				t.Fatalf("threshold lost unit/window or invented another category: %+v", s.Alerts)
			}
			before := s.Project(false)
			d.observe(now.Add(time.Second), &s)
			if !reflect.DeepEqual(before.Alerts, s.Alerts) {
				t.Fatal("replayed snapshot changed revision or facts")
			}
		})
	}
}

func TestAlertsCoalescedFactsSurviveResolutionAndTerminalCapture(t *testing.T) {
	now := testBootNow()
	d := alertDetector{}
	at := time.Unix(100, 0)
	row := networkview.Alert{Category: "fixture", Facts: networkview.AlertFacts{Packets: 1}}
	d.update(0, now, at, true, row, time.Second)
	first := d.slots[0].row
	row.Facts.Packets = 9
	d.update(0, now.Add(time.Second), at.Add(time.Second), true, row, time.Second)
	if !reflect.DeepEqual(d.slots[0].row, first) || d.slots[0].latest.Facts.Packets != 9 || d.suppressed != 1 {
		t.Fatal("coalescing changed published evidence under the same cursor or discarded latest facts")
	}
	d.update(0, now.Add(2*time.Second), at.Add(2*time.Second), false, row, time.Second)
	d.update(0, now.Add(3*time.Second), at.Add(3*time.Second), false, row, time.Second)
	d.update(0, now.Add(30*time.Second), at.Add(30*time.Second), false, row, time.Second)
	resolved := d.slots[0].row
	if resolved.State != "resolved" || resolved.Sequence != first.Sequence+1 || resolved.Facts.Packets != 9 || resolved.LastSeen != at.Add(time.Second) || resolved.ID != first.ID {
		t.Fatal("coalesced resolution lost last actual observation or replay identity")
	}
	d.update(1, now, at, true, networkview.Alert{Category: "health_enforcer"}, time.Second)
	s := alertFixture(1, now.Add(31*time.Second))
	s.Terminal = true
	d.observe(now.Add(31*time.Second), &s)
	for _, alert := range s.Alerts {
		if !alert.Terminal || alert.Category == "health_enforcer" && alert.State != "open" {
			t.Fatal("terminal capture invented recovery or omitted finality")
		}
	}
	projected := s.Project(false)
	if !reflect.DeepEqual(s.Alerts, projected.Alerts) {
		t.Fatal("public projection lost alert terminal state")
	}
}

func TestAlertsUnknownCoverageGapAndCounterResetNeverCreateBurst(t *testing.T) {
	for _, scenario := range []string{"coverage", "gap", "reset", "clock"} {
		t.Run(scenario, func(t *testing.T) {
			now := testBootNow()
			d := alertDetector{started: now}
			s := alertFixture(1, now)
			*s.Counters.DeniedDNSQueries = 1000
			d.observe(now, &s)
			now = now.Add(time.Second)
			s = alertFixture(2, now)
			*s.Counters.DeniedDNSQueries = 2000
			switch scenario {
			case "coverage":
				s.Coverage.GuardDenials = partialCoverage("fixture_gap")
			case "gap":
				now = now.Add(ObservationStaleAfter)
			case "reset":
				*s.Counters.DeniedDNSQueries = 0
			case "clock":
				now = 0
			}
			d.observe(now, &s)
			if len(s.Alerts) != 0 {
				t.Fatal("uncertain interval fabricated a burst")
			}
		})
	}
}

func TestAlertHealthStartupDebounceRecoveryAndTerminal(t *testing.T) {
	now := testBootNow()
	d := alertDetector{started: now}
	for second := 0; second <= 65; second++ {
		tick := now.Add(time.Duration(second) * time.Second)
		s := alertFixture(second+1, tick)
		if second < 34 {
			s.Health.Collector = networkview.Health{Status: "unknown", Reason: "observation_gap"}
		}
		d.observe(tick, &s)
		row := alertByCategory(s, "health_collector")
		if second < 30 && row != nil {
			t.Fatal("startup became health incident")
		}
		if second == 30 && (row == nil || row.State != "open") {
			t.Fatal("sustained outage not reported after startup")
		}
		if second == 60 && (row == nil || row.State != "resolved") {
			t.Fatal("rate-limited recovery was never emitted")
		}
	}
}

func TestAlertsRateRiseRequiresTwoCompleteMeasuredWindowsAndBoundedHistory(t *testing.T) {
	now := testBootNow()
	d := alertDetector{started: now}
	var sent uint64
	for second := 0; second <= 900; second++ {
		if second > 0 {
			sent += 1 << 20
			if second > 300 {
				sent += 7 << 20
			}
		}
		tick := now.Add(time.Duration(second) * time.Second)
		s := alertFixture(second+1, tick)
		*s.Counters.SentBytes = networkview.Count(sent)
		d.observe(tick, &s)
		row := alertByCategory(s, "outbound_rate_rise")
		if second < 594 && row != nil {
			t.Fatal("rate anomaly invented a complete baseline")
		}
		if second == 600 && (row == nil || row.Threshold.Baseline == nil || row.Threshold.BaselineWindowMillis < 297000 || row.WindowMillis < 297000) {
			t.Fatal("measured rate rise missing baseline/window")
		}
		if len(d.history) > MaxAlertSamples || len(s.Alerts) > alertCategoryCount {
			t.Fatal("detector state grew with traffic history")
		}
	}
}

func TestAlertsTerminalFlushPreservesThrottledFactsEvenWithoutClock(t *testing.T) {
	now := testBootNow()
	d := alertDetector{}
	row := networkview.Alert{Category: "fixture", Facts: networkview.AlertFacts{Packets: 1}}
	d.update(0, now, time.Unix(100, 0), true, row, time.Second)
	row.Facts.Packets = 9
	d.update(0, now.Add(time.Second), time.Unix(101, 0), true, row, time.Second)
	s := alertFixture(1, 0)
	s.Terminal = true
	d.observe(0, &s)
	if len(s.Alerts) != 1 || !s.Alerts[0].Terminal || s.Alerts[0].Facts.Packets != 9 || s.Alerts[0].Sequence != 2 {
		t.Fatal("invalid final clock erased terminal or coalesced alert evidence")
	}
	before := s.Project(false)
	s.Sequence++
	*s.Counters.ProtectedPackets = 999
	d.observe(now.Add(time.Minute), &s)
	if !reflect.DeepEqual(before.Alerts, s.Alerts) {
		t.Fatal("later sample mutated a terminal alert")
	}
}

func TestAlertHealthGapRestartsObservedFailureDuration(t *testing.T) {
	now := testBootNow()
	d := alertDetector{started: now}
	s := alertFixture(1, now)
	d.observe(now, &s)
	for i, seconds := range []int{1, 10, 12, 13} {
		tick := now.Add(time.Duration(seconds) * time.Second)
		s = alertFixture(i+2, tick)
		s.Health.Gateway = networkview.Health{Status: "unknown", Reason: "gateway_not_ready"}
		d.observe(tick, &s)
		row := alertByCategory(s, "health_gateway")
		if seconds < 13 && row != nil {
			t.Fatal("unobserved gap counted as continuously failing gateway")
		}
		if seconds == 13 && row == nil {
			t.Fatal("fresh observed failure duration never alerted")
		}
	}
}

func TestAlertUnknownCoverageNeverResolvesAnOpenBurst(t *testing.T) {
	now := testBootNow()
	d := alertDetector{started: now}
	s := alertFixture(1, now)
	d.observe(now, &s)
	for second := 1; second <= 180; second++ {
		tick := now.Add(time.Duration(second) * time.Second)
		s = alertFixture(second+1, tick)
		*s.Counters.DeniedDNSQueries = 100
		if second > 1 {
			s.Coverage.GuardDenials = missingCoverage("source_gap")
		}
		d.observe(tick, &s)
		if row := alertByCategory(s, "denial_burst_dns"); row == nil || row.State != "open" {
			t.Fatal("unknown coverage was called quiet recovery")
		}
	}
}

func TestAlertCursorSaturationCannotPublishChangedFactsUnderSameSequence(t *testing.T) {
	now := testBootNow()
	d := alertDetector{sequence: networkview.Count(^uint64(0) - 1)}
	row := networkview.Alert{Category: "fixture", Facts: networkview.AlertFacts{Packets: 1}}
	d.update(0, now, time.Unix(100, 0), true, row, time.Second)
	first := d.slots[0].row
	row.Facts.Packets = 2
	d.update(0, now.Add(time.Minute), time.Unix(160, 0), true, row, time.Second)
	s := networkview.Snapshot{}
	d.project(&s)
	if !s.Loss.Unknown || !reflect.DeepEqual(first, s.Alerts[0]) {
		t.Fatal("saturated sequence changed evidence under a reused cursor")
	}
}
