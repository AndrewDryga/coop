package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
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
// window open instead; newPolicyWatchdogHarness supervises a provider whose declared capability
// is not the default one.
func newWatchdogHarness(d watchdogDeadlines) *watchdogHarness {
	return newPolicyWatchdogHarness(toolLifecyclePolicy(d))
}

func newUnarmedWatchdogHarness(d watchdogDeadlines) *watchdogHarness {
	return newUnarmedPolicyWatchdogHarness(toolLifecyclePolicy(d))
}

func newPolicyWatchdogHarness(p watchdogPolicy) *watchdogHarness {
	h := newUnarmedPolicyWatchdogHarness(p)
	h.armLaunch()
	return h
}

func newUnarmedPolicyWatchdogHarness(p watchdogPolicy) *watchdogHarness {
	h := &watchdogHarness{now: time.Date(2026, 7, 26, 8, 0, 0, 0, time.UTC)}
	h.wd = newProviderWatchdogWith(p, func() { h.canceled++ }, func() time.Time { return h.now },
		func(d time.Duration, fn func()) watchdogTimer {
			t := &fakeWatchdogTimer{d: d, fn: fn}
			h.timers = append(h.timers, t)
			return t
		})
	return h
}

// toolLifecyclePolicy is the supervision claude, codex, and gemini run under — a stream that
// reports foreground tool start and end under its own ids — which is what every test below means
// unless it says otherwise.
func toolLifecyclePolicy(d watchdogDeadlines) watchdogPolicy {
	return watchdogPolicy{watchdogDeadlines: d, toolLifecycle: true}
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

// The phases a provider runs follow the capability its ADAPTER declares, not what its stream has
// happened to emit so far. A stream with no tool lifecycle cannot suspend the idle deadline for a
// foreground gate, so it trades that deadline for one conservative post-progress fallback and
// keeps no tool cap it could never arm.
func TestProviderWatchdogPolicyFollowsTheDeclaredCapability(t *testing.T) {
	armed := testDeadlines() // 10m start / 30m idle / 2h tool
	for _, c := range []struct {
		name          string
		in            watchdogDeadlines
		toolLifecycle bool
		want          watchdogPolicy
		ceiling       time.Duration
	}{
		{
			name: "a stream that reports tools is supervised exactly as shipped",
			in:   armed, toolLifecycle: true,
			want:    watchdogPolicy{watchdogDeadlines: armed, toolLifecycle: true},
			ceiling: 48 * time.Hour,
		},
		{
			name: "a stream that reports none trades idle for the fallback and drops the tool cap",
			in:   armed,
			want: watchdogPolicy{watchdogDeadlines: watchdogDeadlines{
				start: 10 * time.Minute, idle: 2 * time.Hour,
			}},
			ceiling: 48 * time.Hour,
		},
		{
			name: "disabled deadlines stay disabled — the fallback is inert by default",
			in:   watchdogDeadlines{},
			want: watchdogPolicy{}, ceiling: 0,
		},
		{
			name: "a fixture's short deadlines keep the ratio, so the policy stays testable",
			in:   watchdogDeadlines{start: 20 * time.Second, idle: time.Second, tool: 4 * time.Second},
			want: watchdogPolicy{watchdogDeadlines: watchdogDeadlines{
				start: 20 * time.Second, idle: 4 * time.Second,
			}},
			ceiling: 8 * time.Minute,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := providerWatchdogPolicy(c.in, c.toolLifecycle)
			if got != c.want {
				t.Errorf("providerWatchdogPolicy(%+v, %v) = %+v, want %+v", c.in, c.toolLifecycle, got, c.want)
			}
			// The ceiling stays the OUTERMOST bound whichever policy applies: the fallback is one of
			// the phases it is derived from, never something that outruns it.
			ceiling := attemptCeiling(got.watchdogDeadlines)
			if ceiling != c.ceiling {
				t.Errorf("ceiling = %s, want %s", ceiling, c.ceiling)
			}
			if got.idle > 0 && ceiling <= got.idle {
				t.Errorf("ceiling %s does not outlast the post-progress deadline %s", ceiling, got.idle)
			}
		})
	}
}

// The declaration reaches the watchdog through the registry, so the policy an attempt runs under is
// decided by the provider it is running — never by anything coop measures about the process.
func TestWatchdogPolicyForReadsTheAdapterDeclaration(t *testing.T) {
	cfg := &config.Config{ProviderTimeouts: "start=10s,idle=20s,tool=30s"}
	for _, agent := range []string{"claude", "codex", "gemini"} {
		want := watchdogPolicy{watchdogDeadlines{start: 10 * time.Second, idle: 20 * time.Second, tool: 30 * time.Second}, true}
		if got := watchdogPolicyFor(cfg, agent); got != want {
			t.Errorf("%s policy = %+v, want %+v", agent, got, want)
		}
	}
	// Grok's stream carries no tool lifecycle (probed at v0.2.101): 4 × idle, no tool cap.
	want := watchdogPolicy{watchdogDeadlines: watchdogDeadlines{start: 10 * time.Second, idle: 80 * time.Second}}
	if got := watchdogPolicyFor(cfg, "grok"); got != want {
		t.Errorf("grok policy = %+v, want %+v", got, want)
	}
	// An agent no adapter answers for gets the conservative side, not the trusting one.
	if got := watchdogPolicyFor(cfg, "not-a-registered-agent"); got.toolLifecycle {
		t.Errorf("unknown agent policy = %+v, want no assumed tool lifecycle", got)
	}
}

// A grok foreground gate is INVISIBLE in its stream — no start, no end, no id — so it is
// indistinguishable from silence. Killing it at the ordinary idle deadline is exactly the thing
// coop promises not to do, and letting it run forever is the thing the watchdog exists to prevent.
// The fallback is the deliberate middle: nothing suspends it, but it is long enough that no honest
// gate reaches it.
func TestWatchdogWithoutToolLifecycleBoundsSilenceNotWork(t *testing.T) {
	h := newPolicyWatchdogHarness(providerWatchdogPolicy(testDeadlines(), false))
	if h.active().d != 10*time.Minute {
		t.Fatalf("start deadline = %s, want 10m", h.active().d)
	}
	h.wd.progress()
	if h.active().d != 2*time.Hour {
		t.Fatalf("post-progress deadline = %s, want the 2h fallback (4 × the 30m idle budget)", h.active().d)
	}
	// Tool events are not in this stream's schema, so the declaration — not the event — decides:
	// one forged start must not suspend the fallback, because no end could ever resume it.
	armed := len(h.timers)
	h.wd.toolStart("forged")
	h.wd.toolEnd("forged")
	if len(h.timers) != armed || len(h.wd.openTools) != 0 {
		t.Fatalf("tool events on a no-lifecycle stream armed %d deadline(s) and opened %d tool(s)",
			len(h.timers)-armed, len(h.wd.openTools))
	}
	// Progress keeps re-arming it, and silence past it still ends the attempt: bounded, just
	// conservatively, and reported as the post-progress silence it is.
	h.wd.progress()
	h.active().fn()
	if h.wd.timedOut() != outcomeIdleTimeout || h.canceled != 1 {
		t.Fatalf("fallback silence: outcome=%q cancels=%d", h.wd.timedOut(), h.canceled)
	}
	if h.ceiling == nil || h.ceiling.d != 48*time.Hour {
		t.Fatalf("ceiling = %v, want 48h (24 × the fallback it bounds)", h.ceiling)
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
			// The announcement names the derived no-tool-lifecycle bound too: the visible idle
			// deadline is not what every provider actually runs under.
			says: []string{"overridden by COOP_PROVIDER_TIMEOUTS", "start=30s idle=45s tool=1m30s",
				"no tool lifecycle: silence 3m0s, no tool cap, ceiling 1h12m0s"},
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

// The shipped deadlines are ARMED, because nothing else bounds an attempt that never finishes:
// every retry budget in the loop counts outcomes that already happened, so an unattended drain
// meeting one wedged provider CLI waits for a human. They are also SILENCE budgets rather than
// service-level targets — 73d2634 zeroed them after measured false kills, and what makes them safe
// to run again is the launch-anchored arming boundary, semantically validated activity, the
// per-adapter policy, and the non-resettable ceiling. Tightening a number here re-opens that trade;
// it is not a tuning knob.
func TestShippedProviderDeadlinesAreArmed(t *testing.T) {
	shipped := watchdogDeadlines{start: providerStartDeadline, idle: providerIdleDeadline, tool: providerToolDeadline}
	want := watchdogDeadlines{start: 10 * time.Minute, idle: 30 * time.Minute, tool: 2 * time.Hour}
	if shipped != want {
		t.Fatalf("shipped provider deadlines = %+v, want %+v", shipped, want)
	}
	// A draining box emits no stream events, so the descendant drain and the idle deadline run from
	// the same instant. Were the drain able to reach the deadline, a box held open by a leaked
	// descendant would die as a wedged provider — losing the handoff AND the drain's own exit codes
	// — so the two budgets are checked against each other rather than kept in step by a comment.
	dm := regexp.MustCompile(`COOP_DESCENDANT_TIMEOUT:-(\d+)`).FindStringSubmatch(box.BaseDockerfile())
	if dm == nil {
		t.Fatal("could not find the descendant drain default in the generated box definition")
	}
	seconds, err := strconv.Atoi(dm[1])
	if err != nil {
		t.Fatal(err)
	}
	if drain := time.Duration(seconds) * time.Second; shipped.idle < 2*drain {
		t.Errorf("idle deadline %s does not comfortably outlast the %s descendant drain", shipped.idle, drain)
	}
	// Armed phases mean an armed ceiling, and the fallback a no-tool-lifecycle provider runs under
	// stays well inside it.
	if got, want := attemptCeiling(shipped), 48*time.Hour; got != want {
		t.Errorf("shipped attempt ceiling = %s, want %s", got, want)
	}
	if got, want := providerWatchdogPolicy(shipped, false).idle, 2*time.Hour; got != want {
		t.Errorf("shipped no-tool-lifecycle silence fallback = %s, want %s", got, want)
	}
}

// The consult wrapper is a DIFFERENT clock and stays unlimited: a peer's answer is the whole point
// of the call, and the false kills that zeroed every bound in 73d2634 were consults. A consult also
// runs as a child of a supervised attempt, so the attempt's tool cap and ceiling already bound it
// from outside — it needs no guess of its own.
func TestConsultWrapperDoesNotBoundAWorkingPeer(t *testing.T) {
	cm := regexp.MustCompile(`COOP_CONSULT_TIMEOUT:-(\d+)`).FindStringSubmatch(fusion.ConsultWrapper())
	if cm == nil {
		t.Fatal("could not find the consult timeout default in the generated wrapper")
	}
	if cm[1] != "0" {
		t.Errorf("consult timeout defaults to %ss, want 0 (unlimited)", cm[1])
	}
}

// 0 still means "no timer at all" — the mechanism the derived fallback (which turns the tool phase
// off) and the ceiling both rest on. No longer reachable from the shipped defaults, still
// load-bearing wherever a policy disables a phase, and a stale timer left running would fire.
func TestDisabledDeadlineArmsNoTimer(t *testing.T) {
	var armed int
	w := newProviderWatchdogWith(toolLifecyclePolicy(watchdogDeadlines{}), func() { t.Error("cancelled with no deadline set") },
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

// A kill has to report what it SAW: which clock ran out, what that clock was watching, and for how
// long. The outcome name carries the phase but never the number, and the number is what tells an
// operator whether the provider wedged or the budget is too tight for this repo.
func TestWatchdogTimeoutDiagnosticNamesTheClockAndTheSilence(t *testing.T) {
	for _, c := range []struct {
		name string
		fire func(*watchdogHarness)
		want string
	}{
		{
			name: "start",
			fire: func(h *watchdogHarness) {
				h.now = h.now.Add(10*time.Minute + 400*time.Millisecond)
				h.active().fn()
			},
			want: "no first model action for 10m0s (start deadline 10m0s)",
		},
		{
			name: "idle",
			fire: func(h *watchdogHarness) {
				h.wd.progress()
				h.now = h.now.Add(30*time.Minute + time.Second)
				h.active().fn()
			},
			want: "no recognized provider activity for 30m1s (idle deadline 30m0s)",
		},
		{
			// The clause measures the surviving TOOL's age, not the time since its cap was
			// re-anchored: anchoring on the re-arm would report 1h50m for the same fire.
			name: "tool",
			fire: func(h *watchdogHarness) {
				h.wd.progress()
				h.wd.toolStart("a")
				h.now = h.now.Add(30 * time.Minute)
				h.wd.toolStart("b")
				h.now = h.now.Add(10 * time.Minute)
				h.wd.toolEnd("a")
				h.now = h.now.Add(time.Hour + 50*time.Minute)
				h.active().fn()
			},
			want: "its oldest foreground tool held 2h0m0s (tool cap 2h0m0s)",
		},
		{
			name: "ceiling",
			fire: func(h *watchdogHarness) {
				h.wd.progress()
				h.now = h.now.Add(48 * time.Hour)
				h.ceiling.fn()
			},
			want: "the attempt ran 48h0m0s without finishing (ceiling 48h0m0s)",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newWatchdogHarness(testDeadlines())
			if said := h.wd.timeoutDiagnostic(); said != "" {
				t.Fatalf("a live attempt already reported %q", said)
			}
			c.fire(h)
			if said := h.wd.timeoutDiagnostic(); said != c.want {
				t.Errorf("timeoutDiagnostic() = %q, want %q", said, c.want)
			}
		})
	}
}
