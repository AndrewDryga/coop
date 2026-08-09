package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
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
	timers   []*fakeWatchdogTimer // phase deadlines only; the attempt ceiling is held apart
	ceiling  *fakeWatchdogTimer
	now      time.Time
	canceled int
}

// newWatchdogHarness builds a watchdog already past the runtime-launch boundary — the state
// every supervision test below starts from. newUnarmedWatchdogHarness keeps the host-setup
// window open instead.
func newWatchdogHarness(d watchdogDeadlines) *watchdogHarness {
	h := newUnarmedWatchdogHarness(d)
	h.armLaunch()
	return h
}

func newUnarmedWatchdogHarness(d watchdogDeadlines) *watchdogHarness {
	h := &watchdogHarness{now: time.Date(2026, 7, 26, 8, 0, 0, 0, time.UTC)}
	h.wd = newProviderWatchdogWith(d, func() { h.canceled++ }, func() time.Time { return h.now },
		func(d time.Duration, fn func()) watchdogTimer {
			t := &fakeWatchdogTimer{d: d, fn: fn}
			h.timers = append(h.timers, t)
			return t
		})
	return h
}

func (h *watchdogHarness) active() *fakeWatchdogTimer { return h.timers[len(h.timers)-1] }

// armLaunch crosses the runtime-launch boundary. armStart arms the attempt ceiling first and the
// start deadline second, so the harness lifts the ceiling out of h.timers: every count below is
// then about PHASE deadlines, and the ceiling is asserted on its own where it belongs.
func (h *watchdogHarness) armLaunch() {
	h.wd.armStart()
	if h.ceiling == nil && len(h.timers) > 0 {
		h.ceiling, h.timers = h.timers[0], h.timers[1:]
	}
}

func testDeadlines() watchdogDeadlines {
	return watchdogDeadlines{start: 10 * time.Minute, idle: 30 * time.Minute, tool: 2 * time.Hour}
}

// Host setup — filesystem projection, sibling services, a slow network probe — runs before
// box.Run reaches the runtime-launch boundary. No clock may run during it, or coop's own slow
// setup gets reported as provider_start_timeout and the loop rotates a healthy target away.
func TestWatchdogRunsNoClockBeforeTheRuntimeLaunch(t *testing.T) {
	h := newUnarmedWatchdogHarness(testDeadlines())
	if len(h.timers) != 0 {
		t.Fatalf("host setup armed %d deadline(s), want none", len(h.timers))
	}
	h.armLaunch()
	if len(h.timers) != 1 || h.active().d != 10*time.Minute {
		t.Fatalf("launch armed %d phase timer(s), active deadline %s; want one 10m start deadline", len(h.timers), h.active().d)
	}
	// The launch also starts the one clock no event can touch.
	if h.ceiling == nil || h.ceiling.d != attemptCeiling(testDeadlines()) {
		t.Fatalf("launch armed ceiling %v, want %s", h.ceiling, attemptCeiling(testDeadlines()))
	}
	// The boundary is crossed once: a second signal must not restart a clock the provider is
	// already running against.
	h.wd.armStart()
	if len(h.timers) != 1 {
		t.Fatalf("a second launch signal armed %d deadlines, want one", len(h.timers))
	}
	h.active().fn()
	if h.wd.timedOut() != outcomeStartTimeout || h.canceled != 1 {
		t.Fatalf("armed start deadline: outcome=%q cancels=%d", h.wd.timedOut(), h.canceled)
	}
}

// A box run that fails before it launches anything (an unreachable daemon, a refused compose
// file) never reaches the boundary, so the attempt ends with no timeout to report.
func TestWatchdogUnlaunchedAttemptReportsNoTimeout(t *testing.T) {
	h := newUnarmedWatchdogHarness(testDeadlines())
	h.wd.stop()
	if h.wd.timedOut() != "" || h.canceled != 0 {
		t.Fatalf("unlaunched attempt: outcome=%q cancels=%d", h.wd.timedOut(), h.canceled)
	}
	// Even a late signal from a torn-down launch path cannot start a clock afterwards.
	h.armLaunch()
	if len(h.timers) != 0 || h.ceiling != nil {
		t.Fatalf("launch signal after stop armed %d deadline(s) and ceiling %v, want none", len(h.timers), h.ceiling)
	}
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

func TestWatchdogLooseEventsAreJudgedByTheirID(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	// A result for a never-seen tool still proves the provider is alive — its start may have been
	// dropped at the open-tool cap — so it arms the idle deadline in place of the start deadline.
	h.wd.toolEnd("unseen")
	if h.active().d != 30*time.Minute {
		t.Fatalf("unseen tool result: deadline %s, want idle 30m", h.active().d)
	}
	// An ID-less lifecycle event is unpairable: counting it as progress would let a stream suspend
	// and resume supervision with events that name nothing. It arms nothing at all.
	armed := len(h.timers)
	h.wd.toolStart("")
	h.wd.toolEnd("")
	if len(h.timers) != armed {
		t.Fatalf("ID-less tool events armed %d deadline(s), want none", len(h.timers)-armed)
	}
	// A duplicate open of a live tool must not reset its cap.
	h.wd.toolStart("x")
	capX := h.active()
	h.wd.toolStart("x")
	if h.active() != capX {
		t.Fatal("duplicate tool start replaced the standing cap")
	}
}

// The stream is the box's stdout, so tool IDs are free to invent. The map they land in is capped,
// the overflow is dropped rather than evicting the tool that carries the absolute cap, and closing
// tools gives the slots back — a bound, not a one-way wedge.
func TestWatchdogCapsForgedOpenTools(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	h.wd.progress()
	h.wd.toolStart("real")
	anchor := h.active()
	for i := range 10_000 {
		h.now = h.now.Add(time.Second) // each forged start is "younger" than the real tool
		h.wd.toolStart(fmt.Sprintf("forged-%d", i))
	}
	if len(h.wd.openTools) != maxOpenTools || len(h.wd.toolOrder) != maxOpenTools {
		t.Fatalf("10k forged tool IDs grew state to %d tools / %d ordered, want %d",
			len(h.wd.openTools), len(h.wd.toolOrder), maxOpenTools)
	}
	if h.active() != anchor {
		t.Fatal("a forged flood re-armed the absolute tool cap")
	}
	if h.wd.toolOrder[0] != "real" {
		t.Fatalf("the flood displaced the oldest open tool with %q", h.wd.toolOrder[0])
	}
	// Cleanup: every tracked tool closes, the state empties, and the idle deadline resumes.
	for _, id := range slices.Clone(h.wd.toolOrder) {
		h.wd.toolEnd(id)
	}
	if len(h.wd.openTools) != 0 || len(h.wd.toolOrder) != 0 {
		t.Fatalf("closed tools left %d tools / %d ordered behind", len(h.wd.openTools), len(h.wd.toolOrder))
	}
	if h.active().d != 30*time.Minute {
		t.Fatalf("deadline after the flood closed = %s, want idle 30m", h.active().d)
	}
	// The freed slots are usable again, and a survivor whose budget is already spent fires
	// instead of silently disabling the cap.
	h.wd.toolStart("after")
	if h.active().d != 2*time.Hour {
		t.Fatalf("post-flood tool cap = %s, want 2h", h.active().d)
	}
}

// No sequence of events — the whole vocabulary of a stream that only wants to stay alive — may
// replace, retire, or extend the attempt ceiling.
func TestWatchdogAttemptCeilingIsNotResettable(t *testing.T) {
	h := newWatchdogHarness(testDeadlines())
	if h.ceiling == nil || h.ceiling.d != 48*time.Hour {
		t.Fatalf("armed ceiling = %v, want 48h (24 × the 2h tool budget)", h.ceiling)
	}
	for i := range 500 {
		id := fmt.Sprintf("t%d", i)
		h.wd.bootstrap()
		h.wd.progress()
		h.wd.toolStart(id)
		h.wd.toolEnd(id)
		h.wd.terminal()
		h.now = h.now.Add(time.Hour)
	}
	if h.ceiling.stopped {
		t.Fatal("a stream event stopped the attempt ceiling")
	}
	if h.wd.ceiling != h.ceiling {
		t.Fatal("a stream event replaced the attempt ceiling")
	}
	if h.wd.timedOut() != "" {
		t.Fatalf("phase deadline fired during the event flood: %q", h.wd.timedOut())
	}
	// It fires under its own generation, which no re-arm can retire, and it is a provider timeout
	// so the loop's timeout policy — rotate, capped at three in a row — owns the outcome.
	h.ceiling.fn()
	if h.wd.timedOut() != outcomeAttemptTimeout || h.canceled != 1 {
		t.Fatalf("ceiling fire: outcome=%q cancels=%d", h.wd.timedOut(), h.canceled)
	}
	if !isProviderTimeout(outcomeAttemptTimeout) {
		t.Fatalf("%q is not classified as a provider timeout", outcomeAttemptTimeout)
	}
	// Sticky, like every other fired deadline: later events and a later phase fire change nothing.
	h.wd.progress()
	h.active().fn()
	if h.canceled != 1 || h.wd.timedOut() != outcomeAttemptTimeout {
		t.Fatalf("ceiling outcome not sticky: cancels=%d outcome=%q", h.canceled, h.wd.timedOut())
	}
}

// The ceiling is derived from the phases it bounds — never configured, and never running when the
// phases it is proportional to are disabled.
func TestAttemptCeilingFollowsTheArmedPhases(t *testing.T) {
	for _, c := range []struct {
		name string
		in   watchdogDeadlines
		want time.Duration
	}{
		{"every phase disabled", watchdogDeadlines{}, 0},
		{"the longest phase drives it", testDeadlines(), 48 * time.Hour},
		{"one armed phase is enough", watchdogDeadlines{start: time.Hour}, 24 * time.Hour},
		{"fixture-short phases stay short", watchdogDeadlines{start: 20 * time.Second, idle: time.Second, tool: 4 * time.Second}, 8 * time.Minute},
	} {
		if got := attemptCeiling(c.in); got != c.want {
			t.Errorf("attemptCeiling(%s) = %s, want %s", c.name, got, c.want)
		}
	}
	// Disabled phases arm no ceiling timer at all, so nothing can fire against a run coop
	// promised not to interrupt.
	h := newWatchdogHarness(watchdogDeadlines{})
	if h.ceiling != nil || len(h.timers) != 0 {
		t.Fatalf("disabled deadlines armed ceiling %v and %d phase timer(s)", h.ceiling, len(h.timers))
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

// The override is an internal knob on a security bound, so its policy is tested against the ARMED
// defaults a later task will ship — the disabled zeros in production would make every case pass by
// accident. It may only shorten supervision, it may not set a value too small to be supervision at
// all, and it may never do either quietly.
func TestWatchdogDeadlineOverridePolicy(t *testing.T) {
	armed := watchdogDeadlines{start: 10 * time.Minute, idle: 30 * time.Minute, tool: 2 * time.Hour}
	cases := []struct {
		name  string
		def   watchdogDeadlines
		raw   string
		want  watchdogDeadlines
		says  []string
		quiet bool
	}{
		{
			name: "shortening an armed default is honored",
			def:  armed, raw: "start=30s,idle=45s,tool=90s",
			want: watchdogDeadlines{start: 30 * time.Second, idle: 45 * time.Second, tool: 90 * time.Second},
			says: []string{"overridden by COOP_PROVIDER_TIMEOUTS", "start=30s idle=45s tool=1m30s"},
		},
		{
			name: "lengthening an armed default is cut back to it",
			def:  armed, raw: "idle=72h,tool=72h",
			want: armed,
			says: []string{"idle=72h0m0s cut to the built-in 30m0s", "may only shorten", "tool=72h0m0s cut to the built-in 2h0m0s"},
		},
		{
			name: "an unsafely tiny value is raised, not honored",
			def:  armed, raw: "start=5ms",
			want: watchdogDeadlines{start: time.Second, idle: 30 * time.Minute, tool: 2 * time.Hour},
			says: []string{"start=5ms raised to 1s", "kills healthy providers"},
		},
		{
			name: "a phase the override never named keeps its disabled default",
			def:  watchdogDeadlines{}, raw: "idle=90s",
			want: watchdogDeadlines{idle: 90 * time.Second},
			says: []string{"start=disabled idle=1m30s tool=disabled"},
		},
		{
			name: "the ceiling is not a phase anyone may set",
			def:  armed, raw: "ceiling=99h",
			want: armed,
			says: []string{"ignored", "unknown phase"},
		},
		{
			name: "an unparsable field keeps every default and says so",
			def:  armed, raw: "start=30s,idle=soon",
			want: armed,
			says: []string{"ignored", "unparsable duration", "idle=soon"},
		},
		{name: "no override says nothing", def: armed, raw: "", want: armed, quiet: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, diagnostics := resolveWatchdogDeadlines(c.def, c.raw)
			if got != c.want {
				t.Errorf("resolveWatchdogDeadlines(%q) = %+v, want %+v", c.raw, got, c.want)
			}
			// The ceiling follows the phases that survived the clamp; it is never parsed.
			if ceiling := attemptCeiling(got); ceiling != attemptCeiling(c.want) {
				t.Errorf("ceiling = %s, want %s", ceiling, attemptCeiling(c.want))
			}
			said := strings.Join(diagnostics, "\n")
			if c.quiet {
				if said != "" {
					t.Errorf("an unset override still said %q", said)
				}
				return
			}
			for _, want := range c.says {
				if !strings.Contains(said, want) {
					t.Errorf("diagnostics %q missing %q", said, want)
				}
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
	w := newProviderWatchdogWith(watchdogDeadlines{}, func() { t.Error("cancelled with no deadline set") },
		time.Now, func(time.Duration, func()) watchdogTimer { armed++; return &fakeWatchdogTimer{} })
	w.armStart()
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
