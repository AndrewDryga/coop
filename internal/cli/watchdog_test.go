package cli

import (
	"regexp"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
)

// activityRecorder captures the semantic events a decoder reports, for decoder-side tests.
type activityRecorder struct{ events []string }

func (r *activityRecorder) bootstrap()          { r.events = append(r.events, "bootstrap") }
func (r *activityRecorder) progress()           { r.events = append(r.events, "progress") }
func (r *activityRecorder) toolStart(id string) { r.events = append(r.events, "tool_start:"+id) }
func (r *activityRecorder) toolEnd(id string)   { r.events = append(r.events, "tool_end:"+id) }
func (r *activityRecorder) terminal()           { r.events = append(r.events, "terminal") }

// fakeWatchdogTimer records its deadline and fire callback so tests drive time by hand,
// including firing STALE timers to prove the generation guard.
type fakeWatchdogTimer struct {
	d       time.Duration
	fn      func()
	stopped bool
}

func (t *fakeWatchdogTimer) Stop() bool { t.stopped = true; return true }

type watchdogHarness struct {
	wd       *providerWatchdog
	timers   []*fakeWatchdogTimer
	now      time.Time
	canceled int
}

func newWatchdogHarness(d watchdogDeadlines) *watchdogHarness {
	h := &watchdogHarness{now: time.Date(2026, 7, 26, 8, 0, 0, 0, time.UTC)}
	h.wd = startProviderWatchdog(d, func() { h.canceled++ }, func() time.Time { return h.now },
		func(d time.Duration, fn func()) watchdogTimer {
			t := &fakeWatchdogTimer{d: d, fn: fn}
			h.timers = append(h.timers, t)
			return t
		})
	return h
}

func (h *watchdogHarness) active() *fakeWatchdogTimer { return h.timers[len(h.timers)-1] }

func testDeadlines() watchdogDeadlines {
	return watchdogDeadlines{start: 10 * time.Minute, idle: 30 * time.Minute, tool: 2 * time.Hour}
}

func TestWatchdogStartTimeoutOnBootstrapOnlySilence(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	if h.active().d != 10*time.Minute {
		t.Fatalf("start deadline = %s, want 10m", h.active().d)
	}
	// Bootstrap proves the CLI launched, not that the model acted: no re-arm.
	h.wd.bootstrap()
	if len(h.timers) != 1 {
		t.Fatalf("bootstrap re-armed the deadline: %d timers", len(h.timers))
	}
	h.active().fn()
	if h.wd.timedOut() != outcomeStartTimeout {
		t.Fatalf("timedOut = %q, want %q", h.wd.timedOut(), outcomeStartTimeout)
	}
	if h.canceled != 1 {
		t.Fatalf("cancel calls = %d, want 1", h.canceled)
	}
	// A later fire or event changes nothing once a timeout is recorded.
	h.wd.progress()
	h.active().fn()
	if h.canceled != 1 || h.wd.timedOut() != outcomeStartTimeout {
		t.Fatalf("recorded timeout not sticky: cancels=%d outcome=%q", h.canceled, h.wd.timedOut())
	}
}

func TestWatchdogProgressArmsAndReArmsIdle(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	start := h.active()
	h.wd.progress()
	if !start.stopped {
		t.Fatal("first model action did not stop the start deadline")
	}
	if h.active().d != 30*time.Minute {
		t.Fatalf("idle deadline = %s, want 30m", h.active().d)
	}
	// A stale timer firing after replacement is a no-op (generation guard).
	start.fn()
	if h.canceled != 0 || h.wd.timedOut() != "" {
		t.Fatalf("stale start timer fired: cancels=%d outcome=%q", h.canceled, h.wd.timedOut())
	}
	// Periodic reasoning keeps re-arming idle; silence after the last one fires idle.
	for i := 0; i < 3; i++ {
		h.wd.progress()
	}
	h.active().fn()
	if h.wd.timedOut() != outcomeIdleTimeout || h.canceled != 1 {
		t.Fatalf("idle silence: outcome=%q cancels=%d", h.wd.timedOut(), h.canceled)
	}
}

func TestWatchdogOpenToolSuspendsIdleAndCapsOldest(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	h.wd.progress()
	h.wd.toolStart("a")
	capA := h.active()
	if capA.d != 2*time.Hour {
		t.Fatalf("tool cap = %s, want 2h", capA.d)
	}
	// Overlapping tools keep the OLDEST cap; narration during tools re-arms nothing.
	h.now = h.now.Add(30 * time.Minute)
	h.wd.toolStart("b")
	h.wd.progress()
	if h.active() != capA {
		t.Fatal("a younger tool or mid-tool narration replaced the oldest tool's cap")
	}
	// Closing the oldest re-anchors to the survivor's remaining budget.
	h.now = h.now.Add(10 * time.Minute)
	h.wd.toolEnd("a")
	if got, want := h.active().d, 2*time.Hour-10*time.Minute; got != want {
		t.Fatalf("survivor cap remaining = %s, want %s", got, want)
	}
	// Closing the last tool resumes the ordinary idle deadline.
	h.wd.toolEnd("b")
	if h.active().d != 30*time.Minute {
		t.Fatalf("idle deadline after tools = %s, want 30m", h.active().d)
	}
}

func TestWatchdogToolCapKillsWedgedTool(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	h.wd.progress()
	h.wd.toolStart("gate")
	h.active().fn()
	if h.wd.timedOut() != outcomeToolTimeout || h.canceled != 1 {
		t.Fatalf("tool cap: outcome=%q cancels=%d", h.wd.timedOut(), h.canceled)
	}
}

func TestWatchdogLooseEventsCountAsProgress(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	// A result for a never-seen tool and an ID-less tool start both prove the provider is
	// alive: each arms the idle deadline in place of the start deadline.
	h.wd.toolEnd("unseen")
	if h.active().d != 30*time.Minute {
		t.Fatalf("unseen tool result: deadline %s, want idle 30m", h.active().d)
	}
	h.wd.toolStart("")
	if h.active().d != 30*time.Minute {
		t.Fatalf("id-less tool start: deadline %s, want idle 30m", h.active().d)
	}
	// A duplicate open of a live tool must not reset its cap.
	h.wd.toolStart("x")
	capX := h.active()
	h.wd.toolStart("x")
	if h.active() != capX {
		t.Fatal("duplicate tool start replaced the standing cap")
	}
}

func TestWatchdogTerminalAndStopEndSupervision(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	h.wd.progress()
	h.wd.terminal()
	if h.active().d != 30*time.Minute {
		t.Fatalf("terminal deadline = %s, want idle 30m", h.active().d)
	}
	// A completion racing the deadline: box.Run returned, stop() wins over a pending fire.
	pending := h.active()
	h.wd.stop()
	if !pending.stopped {
		t.Fatal("stop did not stop the pending deadline")
	}
	pending.fn()
	if h.canceled != 0 || h.wd.timedOut() != "" {
		t.Fatalf("fire after stop: cancels=%d outcome=%q", h.canceled, h.wd.timedOut())
	}
	// Nothing re-arms after stop.
	h.wd.progress()
	if h.active() != pending {
		t.Fatal("activity after stop armed a new deadline")
	}
}

func TestWatchdogDeadlinesOverride(t *testing.T) {
	defaults := watchdogDeadlines{start: providerStartDeadline, idle: providerIdleDeadline, tool: providerToolDeadline}
	cases := []struct {
		name string
		raw  string
		want watchdogDeadlines
	}{
		{"empty keeps defaults", "", defaults},
		{"full override", "start=2s,idle=3s,tool=6s", watchdogDeadlines{start: 2 * time.Second, idle: 3 * time.Second, tool: 6 * time.Second}},
		{"partial override keeps other defaults", "idle=90s", watchdogDeadlines{start: providerStartDeadline, idle: 90 * time.Second, tool: providerToolDeadline}},
		{"malformed field keeps every default", "start=2s,idle=soon", defaults},
		{"unknown key keeps every default", "startup=2s", defaults},
		{"non-positive keeps every default", "start=0s", defaults},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{ProviderTimeouts: c.raw}
			if got := watchdogDeadlinesFor(cfg); got != c.want {
				t.Errorf("watchdogDeadlinesFor(%q) = %+v, want %+v", c.raw, got, c.want)
			}
		})
	}
}

func TestIsProviderTimeout(t *testing.T) {
	for _, outcome := range []string{outcomeStartTimeout, outcomeIdleTimeout, outcomeToolTimeout} {
		if !isProviderTimeout(outcome) {
			t.Errorf("isProviderTimeout(%q) = false", outcome)
		}
	}
	for _, outcome := range []string{"success", "process_failure", "rate_limit", "background_timeout", "interrupted", ""} {
		if isProviderTimeout(outcome) {
			t.Errorf("isProviderTimeout(%q) = true", outcome)
		}
	}
}

// No coop clock may interrupt a provider that is producing work. These are the defaults; a run that
// wants a bound sets COOP_CONSULT_TIMEOUT or COOP_PROVIDER_TIMEOUTS explicitly.
//
// This replaced an ordering test (consult < drain < idle) whose whole premise was that all three
// were finite. They are not any more: a clock cannot tell a long review from a wedged one, and
// killing a working peer loses its answer AND costs the task restart that follows.
func TestNoDefaultDeadlineInterruptsAWorkingProvider(t *testing.T) {
	for _, d := range []struct {
		name string
		got  time.Duration
	}{
		{"start", providerStartDeadline},
		{"idle", providerIdleDeadline},
		{"tool", providerToolDeadline},
	} {
		if d.got != 0 {
			t.Errorf("provider %s deadline defaults to %s, want 0 (disabled)", d.name, d.got)
		}
	}
	cm := regexp.MustCompile(`COOP_CONSULT_TIMEOUT:-(\d+)`).FindStringSubmatch(fusion.ConsultWrapper())
	if cm == nil {
		t.Fatal("could not find the consult timeout default in the generated wrapper")
	}
	if cm[1] != "0" {
		t.Errorf("consult timeout defaults to %ss, want 0 (unlimited)", cm[1])
	}
	// A disabled deadline must create no timer at all, or a stale one could still fire.
	var armed int
	w := startProviderWatchdog(watchdogDeadlines{}, func() { t.Error("cancelled with no deadline set") },
		time.Now, func(time.Duration, func()) watchdogTimer { armed++; return &fakeWatchdogTimer{} })
	w.progress()
	w.toolStart("t1")
	w.toolEnd("t1")
	w.progress()
	if armed != 0 {
		t.Errorf("armed %d timer(s) with deadlines disabled, want 0", armed)
	}
	if fired := w.timedOut(); fired != "" {
		t.Errorf("watchdog fired %q with deadlines disabled", fired)
	}
}
