package loop

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The provider-attempt watchdog bounds SILENCE, not work: a built-in loop/review/preflight
// attempt must show its first model action within the start deadline OF THE RUNTIME LAUNCH,
// keep producing adapter-recognized activity within the idle deadline, and finish its oldest
// open foreground tool within the tool deadline. Only semantic stream events (streamActivity)
// feed it — never process names, CPU, lease heartbeats, redraws, or raw bytes — so a wedged
// provider is killed while long reasoning and a slow foreground gate survive.
// WHICH of those phases a provider actually runs follows the capability its adapter DECLARES about
// its own stream (watchdogPolicy below): a schema with no tool lifecycle can never suspend the idle
// deadline for a gate, so it is supervised by one conservative post-progress fallback instead.
// A phase set to 0 is disabled — no timer at all — and with every phase disabled the attempt
// ceiling below goes with them; the shipped values are all armed.
//
// These clocks cannot tell LONG from WEDGED — they only measure silence — and killing a provider
// that is genuinely working is expensive twice over: the answer is lost, and the loop then restarts
// the task, re-reads its whole context and re-pays for it. Measured: a bounded consult died with
// "exceeded the wrapper cap and terminated without a usable answer" on a security review the lead
// had asked for, and a delegate was killed ten minutes in, after which the lead wrote the diff
// itself. That cost is why these are SILENCE budgets no honest attempt reaches rather than
// service-level targets — ten minutes to the first model action, half an hour between two of them,
// two hours on one foreground tool — and why what re-arms them had to become semantic, launch-
// anchored, and per-adapter first.
//
// They are armed anyway, because the alternative is unbounded: every retry budget in the loop
// (maxLoopFailures, maxStalls, maxProviderTimeouts) counts outcomes that already HAPPENED, so an
// attempt that never finishes is bounded by nothing at all — one wedged provider CLI holds its
// iteration, its task lease, and its credential until a human notices the overnight drain stopped.
// A false kill costs one restarted task; no kill at all costs the whole unattended run.
//
// providerIdleDeadline must stay comfortably ABOVE the box's descendant-drain wait
// (COOP_DESCENDANT_TIMEOUT in internal/box/image.go, 15m). A draining box emits no stream events,
// so the two clocks run from the same instant: if the drain could reach this deadline, every box
// held open by a leaked descendant would be killed as a wedged provider instead of surfacing as a
// descendant handoff, and the drain's own exit codes would never be observed.
//
// The retry CAPS (maxLoopFailures, maxProviderTimeouts) are untouched: they count outcomes that
// already happened rather than interrupting work in flight.
const (
	providerStartDeadline = 10 * time.Minute
	providerIdleDeadline  = 30 * time.Minute
	providerToolDeadline  = 2 * time.Hour
)

// providerSilenceFallbackMultiple supervises a provider whose stream declares NO tool lifecycle
// (agents.ToolLifecycleAbsent — grok, probed at v0.2.101, which emits only thought/text/end).
// Nothing in such a stream says "a foreground gate is running", so the idle deadline cannot be
// suspended for one, and applying it as-is would kill a legitimate 40-minute `make check` at the
// 30-minute mark — the exact thing coop promises not to do. That provider instead gets ONE
// post-progress deadline at this multiple of the idle budget: 30m idle → a 2h fallback, long
// enough that no honest foreground gate reaches it, short enough that a silent attempt is still
// bounded well inside the ceiling above.
//
// It is DERIVED from the idle budget rather than a 2h constant for the same reason the ceiling is:
// the override may only shorten supervision, and shortening idle has to shorten this with it (a
// fixture supervising in seconds keeps the ratio, so the policy is testable). A disabled idle
// deadline multiplies to a disabled fallback, so a phase turned off stays off.
const providerSilenceFallbackMultiple = 4

// Provider-attempt timeout outcomes, recorded verbatim in stage telemetry and handled by the
// loop's dedicated timeout policy: rotate to the next usable rung without cooling, capped at
// maxProviderTimeouts consecutive timeouts per stage.
const (
	outcomeStartTimeout   = "provider_start_timeout"
	outcomeIdleTimeout    = "provider_idle_timeout"
	outcomeToolTimeout    = "provider_tool_timeout"
	outcomeAttemptTimeout = "provider_attempt_timeout"
)

// THE TRUST BOUNDARY: every phase deadline above is re-armed by events the BOX writes to its own
// stdout — the box that holds the credential and runs the agent. Semantic validation (streamjson.go)
// raises the bar to well-formed, content-bearing events, but it cannot make the stream honest: a
// compromised or buggy box can print one valid-looking event per second forever and hold its
// attempt — and the loop slot, the task lease, and the credential — indefinitely.
//
// The attempt ceiling is the one clock no event can touch: armed once at the runtime launch,
// never re-armed, never derived from event math. 24× the LONGEST phase budget in force, because an
// attempt that has burned two dozen of its own worst-case silences without finishing is not doing
// legitimate work, while a real iteration never comes close (they run in minutes; the shipped phase
// values put the ceiling at 48 hours).
//
// It is DERIVED, not configured: nothing — no env var, no conf file, no event — can lengthen it,
// and being proportional it follows whatever supervision the phases actually run, shortening with a
// clamped override and disappearing entirely if every phase is turned off.
const providerAttemptCeilingMultiple = 24

// attemptCeiling is the absolute wall-clock bound for one provider attempt, or 0 when every phase
// deadline is disabled.
func attemptCeiling(d watchdogDeadlines) time.Duration {
	longest := max(d.start, d.idle, d.tool)
	if longest <= 0 {
		return 0
	}
	return longest * providerAttemptCeilingMultiple
}

// maxOpenTools caps the foreground tools one attempt may hold open. Tool IDs come off the box's
// stdout, so unique ones are free to forge and an uncapped map is host memory the box controls.
// Far beyond any real provider's concurrency (a parallel Claude turn calls a handful of tools), so
// a genuine run never reaches it.
const maxOpenTools = 64

// maxProviderTimeouts caps CONSECUTIVE provider-attempt timeouts. Timeouts keep their own
// counter so they never consume the ordinary failure, stall, output, or rate-limit budgets.
const maxProviderTimeouts = 3

func isProviderTimeout(outcome string) bool {
	switch outcome {
	case outcomeStartTimeout, outcomeIdleTimeout, outcomeToolTimeout, outcomeAttemptTimeout:
		return true
	}
	return false
}

type watchdogDeadlines struct {
	start, idle, tool time.Duration
}

// watchdogPolicy is the complete supervision one attempt runs under: the resolved phase deadlines,
// plus whether the provider's stream is DECLARED to carry the foreground tool lifecycle two of
// those phases assume. The capability is read off the adapter registry (agents.StreamSpec), never
// inferred from process names, CPU, or the events seen so far — a stream that has not opened a tool
// yet looks exactly like one whose schema has no tools at all.
type watchdogPolicy struct {
	watchdogDeadlines
	toolLifecycle bool
}

// watchdogPolicyFor is the supervision one attempt against `agent` runs under: coop's defaults,
// narrowed by the internal override, then adapted to that adapter's declared stream capability.
func watchdogPolicyFor(cfg *config.Config, agent string) watchdogPolicy {
	adapter, ok := agents.Get(agent)
	return providerWatchdogPolicy(watchdogDeadlinesFor(cfg), ok && adapter.Stream().TracksTools())
}

// providerWatchdogPolicy adapts resolved deadlines to what the provider's stream can prove.
//
// WITH a tool lifecycle (claude, codex, gemini) nothing changes: post-progress silence is bounded
// by the idle deadline, suspended while a tool is open, and the oldest open tool carries the
// absolute tool cap.
//
// WITHOUT one (grok) those two phases have no events to run on. The idle deadline can never be
// suspended for a foreground gate, so it would kill legitimate work; the tool cap can never be
// armed at all, so keeping it would only inflate the attempt ceiling with a budget nothing can
// reach. Post-progress silence is therefore bounded once, at providerSilenceFallbackMultiple ×
// idle, and the tool phase is disabled — an honest description of a stream that reports no tools.
func providerWatchdogPolicy(d watchdogDeadlines, toolLifecycle bool) watchdogPolicy {
	if toolLifecycle {
		return watchdogPolicy{watchdogDeadlines: d, toolLifecycle: true}
	}
	// A disabled idle deadline multiplies to a disabled fallback, which is the shipped default. The
	// comparison also keeps an absurd (multi-decade) budget from overflowing into a negative
	// duration that arm would read as "this phase is off".
	if fallback := d.idle * providerSilenceFallbackMultiple; fallback > d.idle {
		d.idle = fallback
	}
	d.tool = 0
	return watchdogPolicy{watchdogDeadlines: d}
}

// minWatchdogDeadline floors every overridden deadline. Under a second a deadline stops being
// supervision and becomes a coin flip — one slow container read, a GC pause, or the CLI's own
// startup outruns it and a healthy provider dies at launch. The override is an internal knob, so a
// value that small is a mistake rather than a preference: it is raised, and said out loud.
const minWatchdogDeadline = time.Second

// watchdogOverrideAnnounced keeps the override's diagnostics to one telling per process. The knob
// is per-config, not per-attempt, so repeating it on every iteration would be noise — but saying
// it zero times is how a run ends up supervised by numbers nobody chose.
var watchdogOverrideAnnounced sync.Once

// watchdogDeadlinesFor resolves the deadlines one attempt is supervised by: the fixed conservative
// defaults, narrowed by the internal COOP_PROVIDER_TIMEOUTS override, and announced on stderr when
// the override changes anything.
func watchdogDeadlinesFor(cfg *config.Config) watchdogDeadlines {
	defaults := watchdogDeadlines{
		start: providerStartDeadline, idle: providerIdleDeadline, tool: providerToolDeadline,
	}
	deadlines, diagnostics := resolveWatchdogDeadlines(defaults, cfg.ProviderTimeouts)
	if len(diagnostics) > 0 {
		watchdogOverrideAnnounced.Do(func() {
			for _, diagnostic := range diagnostics {
				ui.Warn("%s", diagnostic)
			}
		})
	}
	return deadlines
}

// resolveWatchdogDeadlines applies the internal COOP_PROVIDER_TIMEOUTS override
// ("start=2s,idle=3s,tool=6s") to the given defaults and returns the deadlines to supervise with
// plus the diagnostics the caller must print. It takes its defaults as an argument so the clamp
// policy is tested against fixed values instead of whatever the shipped constants happen to be.
//
// The override exists so deterministic fixture tests can supervise in seconds where production
// wants minutes. It is therefore allowed to SHORTEN supervision and nothing else: an override that
// could lengthen a deadline would be a supported way to disable the bound coop advertises,
// reachable by anything that can set an environment variable or write the conf file — including the
// box, if it ever reached one. A disabled default (0) counts as infinite, so any finite value
// shortens it. A malformed field keeps every default rather than half-applying, and every
// deviation — clamp or rejection — is named on stderr, because a supervision bound the operator
// did not choose and cannot see is worse than none.
func resolveWatchdogDeadlines(defaults watchdogDeadlines, raw string) (watchdogDeadlines, []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaults, nil
	}
	parsed := defaults
	named := map[string]bool{}
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return defaults, []string{rejectedOverride(field, "expected <phase>=<duration>")}
		}
		dur, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return defaults, []string{rejectedOverride(field, "unparsable duration")}
		}
		if dur <= 0 {
			return defaults, []string{rejectedOverride(field, "a deadline must be positive")}
		}
		key = strings.TrimSpace(key)
		switch key {
		case "start":
			parsed.start = dur
		case "idle":
			parsed.idle = dur
		case "tool":
			parsed.tool = dur
		default:
			return defaults, []string{rejectedOverride(field, "unknown phase")}
		}
		named[key] = true
	}
	// Only a phase the override actually named is clamped. An untouched default is coop's own
	// value and stays exactly as shipped — including the disabled 0 that the floor would otherwise
	// "raise" into a live deadline nobody asked for.
	var diagnostics []string
	clamp := func(phase string, value, def time.Duration) time.Duration {
		if !named[phase] {
			return value
		}
		value, diagnostics = clampOverriddenDeadline(phase, value, def, diagnostics)
		return value
	}
	parsed.start = clamp("start", parsed.start, defaults.start)
	parsed.idle = clamp("idle", parsed.idle, defaults.idle)
	parsed.tool = clamp("tool", parsed.tool, defaults.tool)
	// The overridden budgets are what a provider WITH tool lifecycle runs under; one without is
	// supervised by the derived fallback instead, so its numbers are named here too rather than
	// leaving an operator to reconcile a grok attempt that outlives the idle deadline they can see.
	fallback := providerWatchdogPolicy(parsed, false)
	announced := fmt.Sprintf("provider watchdog deadlines overridden by COOP_PROVIDER_TIMEOUTS (internal knob; it may only shorten supervision): start=%s idle=%s tool=%s attempt ceiling=%s (no tool lifecycle: silence %s, no tool cap, ceiling %s)",
		overrideDuration(parsed.start), overrideDuration(parsed.idle), overrideDuration(parsed.tool), overrideDuration(attemptCeiling(parsed)),
		overrideDuration(fallback.idle), overrideDuration(attemptCeiling(fallback.watchdogDeadlines)))
	return parsed, append([]string{announced}, diagnostics...)
}

// clampOverriddenDeadline enforces the two bounds an override may not cross, floor first so the
// "never longer than the default" half always wins the tie.
func clampOverriddenDeadline(phase string, value, def time.Duration, diagnostics []string) (time.Duration, []string) {
	if value < minWatchdogDeadline {
		diagnostics = append(diagnostics, fmt.Sprintf("COOP_PROVIDER_TIMEOUTS %s=%s raised to %s — a shorter deadline kills healthy providers instead of supervising them",
			phase, value, minWatchdogDeadline))
		value = minWatchdogDeadline
	}
	if def > 0 && value > def {
		diagnostics = append(diagnostics, fmt.Sprintf("COOP_PROVIDER_TIMEOUTS %s=%s cut to the built-in %s — the override may only shorten supervision",
			phase, value, def))
		value = def
	}
	return value, diagnostics
}

func rejectedOverride(field, why string) string {
	return fmt.Sprintf("COOP_PROVIDER_TIMEOUTS ignored (%q: %s) — supervising with the built-in deadlines", field, why)
}

// overrideDuration renders a deadline for the announcement, naming 0 as what it means.
func overrideDuration(d time.Duration) string {
	if d <= 0 {
		return "disabled"
	}
	return d.String()
}

// watchdogTimer is an armed deadline the watchdog holds — the current phase's, or the attempt
// ceiling's. time.AfterFunc in production, a hand-fired fake in tests.
type watchdogTimer interface{ Stop() bool }

// providerWatchdog supervises one provider attempt under one watchdogPolicy. It is the decoder's
// streamActivity sink: each recognized event re-arms the single deadline for the current phase
// (start → idle, with idle suspended while foreground tools are open and the oldest open tool
// absolutely capped — for a provider that declares no tool lifecycle, start → the silence fallback,
// and nothing suspends it).
// Alongside it runs the one deadline events cannot reach, the attempt ceiling. When either fires
// it records the timeout outcome once and cancels the child box context; stop() ends supervision
// the moment box.Run returns, so nothing can fire afterwards.
//
// It is born UNARMED: no clock runs until armStart, which box.Run fires at the runtime-launch
// boundary. Host setup before that boundary is coop's own work, not provider silence.
type providerWatchdog struct {
	mu     sync.Mutex
	policy watchdogPolicy
	cancel func()
	now    func() time.Time
	after  func(time.Duration, func()) watchdogTimer

	timer      watchdogTimer
	ceiling    watchdogTimer
	gen        uint64
	openTools  map[string]time.Time
	toolOrder  []string
	startArmed bool
	stopped    bool
	fired      string
	// armedAt/launchedAt are what the fired clock was measuring FROM: the instant the active phase
	// deadline started running, and the runtime-launch boundary the ceiling runs from. Kept so a
	// kill can report the silence it observed and not just the name of the deadline that ran out.
	armedAt    time.Time
	launchedAt time.Time
	firedAfter time.Duration
}

// ceilingGen is the generation the attempt ceiling fires under. arm increments gen before every
// phase timer, so a phase deadline is never generation 0 — which makes 0 the one generation no
// re-arm can retire, exactly what a non-resettable ceiling needs.
const ceilingGen uint64 = 0

func newProviderWatchdog(policy watchdogPolicy, cancel func()) *providerWatchdog {
	return newProviderWatchdogWith(policy, cancel, time.Now, func(d time.Duration, fn func()) watchdogTimer {
		return time.AfterFunc(d, fn)
	})
}

// newProviderWatchdogWith builds the watchdog with no deadline running; the clock and timer
// factory are injected so unit tests drive every transition deterministically.
func newProviderWatchdogWith(policy watchdogPolicy, cancel func(), now func() time.Time, after func(time.Duration, func()) watchdogTimer) *providerWatchdog {
	return &providerWatchdog{
		policy: policy, cancel: cancel, now: now, after: after,
		openTools: map[string]time.Time{},
	}
}

// armStart starts the start deadline at the runtime-launch boundary (box.Run's OnRuntimeLaunch),
// so filesystem projection, sibling services, or a slow network probe can never be reported as
// provider_start_timeout — and the deadline only ever cancels a context the launch is watching.
// Exactly one arming per attempt: a second call would restart a clock the provider is already
// running against, and the launch boundary is crossed once.
func (w *providerWatchdog) armStart() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.startArmed || w.stopped || w.fired != "" {
		return
	}
	w.startArmed = true
	w.launchedAt = w.now()
	// The launch boundary is also where the attempt ceiling starts, and the only place it is ever
	// touched: it is deliberately NOT routed through arm, so no event, phase change, or re-arm can
	// replace or extend it. Only stop() — box.Run returning — retires it.
	if ceiling := attemptCeiling(w.policy.watchdogDeadlines); ceiling > 0 {
		w.ceiling = w.after(ceiling, func() { w.fire(ceilingGen, outcomeAttemptTimeout) })
	}
	w.arm(w.policy.start, outcomeStartTimeout)
}

// arm replaces the active PHASE deadline — never the attempt ceiling, which is armed once by
// armStart and routed nowhere near here. The caller holds mu; the generation guard makes a
// concurrently-firing stale timer a no-op.
func (w *providerWatchdog) arm(d time.Duration, outcome string) {
	if w.stopped || w.fired != "" {
		return
	}
	// A non-positive deadline is disabled: create no timer at all, so nothing can fire against a
	// phase nobody is supervising. The shipped deadlines are all armed, so this is the tool cap of a
	// provider that declares no tool lifecycle rather than a default.
	if d <= 0 {
		if w.timer != nil {
			w.timer.Stop()
			w.timer = nil
		}
		w.gen++
		return
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	w.gen++
	gen := w.gen
	w.armedAt = w.now()
	w.timer = w.after(d, func() { w.fire(gen, outcome) })
}

func (w *providerWatchdog) fire(gen uint64, outcome string) {
	w.mu.Lock()
	// The ceiling (gen 0) belongs to no phase, so it skips the generation guard: every other timer
	// is stale the moment activity re-arms its phase, and the ceiling is stale only when the
	// attempt is over.
	if w.stopped || w.fired != "" || (gen != ceilingGen && gen != w.gen) {
		w.mu.Unlock()
		return
	}
	w.fired = outcome
	w.firedAfter = w.observed(outcome)
	cancel := w.cancel
	w.mu.Unlock()
	// Cancel outside the lock: the context teardown may run arbitrary callbacks.
	if cancel != nil {
		cancel()
	}
}

// stop ends supervision; called immediately after box.Run returns so a late timer cannot
// cancel anything or misreport a completed attempt as timed out.
func (w *providerWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	if w.ceiling != nil {
		w.ceiling.Stop()
		w.ceiling = nil
	}
}

// timedOut reports the recorded timeout outcome, or "" when no deadline fired.
func (w *providerWatchdog) timedOut() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// observed is how long the clock that just fired had actually been watching: the silence since the
// phase deadline was armed, the age of the oldest open tool for the absolute cap, and the whole
// attempt for the ceiling. The caller holds mu.
func (w *providerWatchdog) observed(outcome string) time.Duration {
	switch {
	case outcome == outcomeAttemptTimeout:
		return w.now().Sub(w.launchedAt)
	case outcome == outcomeToolTimeout && len(w.toolOrder) > 0:
		// Not since the cap was armed: a re-anchored cap runs for the SURVIVOR's remaining budget,
		// and what an operator needs is how long that tool has been open.
		return w.now().Sub(w.openTools[w.toolOrder[0]])
	default:
		return w.now().Sub(w.armedAt)
	}
}

// timeoutDiagnostic explains the kill in one clause: which clock ran out, what it was watching, for
// how long, and the budget it crossed. The outcome name alone names the phase but never the number,
// and "the provider timed out" is not something an operator can act on — the observed silence is
// what tells them whether the provider wedged or the deadline is too tight for this repo. "" when
// no deadline fired. Durations are rounded to the second: the floor on any deadline is 1s, so
// sub-second precision here is noise.
func (w *providerWatchdog) timeoutDiagnostic() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	observed := w.firedAfter.Round(time.Second)
	switch w.fired {
	case outcomeStartTimeout:
		return fmt.Sprintf("no first model action for %s (start deadline %s)", observed, w.policy.start)
	case outcomeIdleTimeout:
		return fmt.Sprintf("no recognized provider activity for %s (idle deadline %s)", observed, w.policy.idle)
	case outcomeToolTimeout:
		return fmt.Sprintf("its oldest foreground tool held %s (tool cap %s)", observed, w.policy.tool)
	case outcomeAttemptTimeout:
		return fmt.Sprintf("the attempt ran %s without finishing (ceiling %s)", observed, attemptCeiling(w.policy.watchdogDeadlines))
	}
	return ""
}

// bootstrap proves the provider CLI launched, not that the model acted — the start deadline
// deliberately stands until the first model action.
func (w *providerWatchdog) bootstrap() {}

// progress is any adapter-recognized model action: assistant text, reasoning, or a recognized
// stream item. It arms (or re-arms) the idle deadline unless a foreground tool suspends it — for a
// provider with no declared tool lifecycle that deadline is the conservative silence fallback,
// which is the only phase such a stream can ever run.
func (w *providerWatchdog) progress() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.openTools) == 0 {
		w.arm(w.policy.idle, outcomeIdleTimeout)
	}
}

// toolStart opens a foreground tool: the ordinary idle deadline is suspended while any tool is
// open, and the OLDEST open tool is capped absolutely so a wedged child cannot hold the
// attempt forever.
//
// Three things fail closed here. A tool from a provider that DECLARED no tool lifecycle is not a
// tool: the declaration is the authority, so an event its schema does not have cannot suspend the
// conservative fallback that provider is supervised by (nothing could then resume it — the
// matching end does not exist either). An ID-less start proves nothing this watchdog can act on —
// it can never be paired with an end, so it would suspend idle with no way to resume — and the
// decoders already reject it, so it is dropped rather than counted as progress. Past maxOpenTools
// the overflow is dropped too, and deliberately NOT by evicting the oldest: evicting re-anchors the
// absolute cap to a younger tool, which would sell the box an endless extension for one forged
// start per interval. Dropping costs nothing — idle stays suspended while any tool is open, and
// the oldest survivor keeps carrying the cap.
func (w *providerWatchdog) toolStart(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id == "" || !w.policy.toolLifecycle {
		return
	}
	if _, open := w.openTools[id]; open {
		return
	}
	if len(w.openTools) >= maxOpenTools {
		return
	}
	w.openTools[id] = w.now()
	w.toolOrder = append(w.toolOrder, id)
	if len(w.openTools) == 1 {
		w.arm(w.policy.tool, outcomeToolTimeout)
	}
}

// toolEnd closes an open tool. When the last tool closes the idle deadline resumes from now;
// when the oldest closes the cap re-anchors to the next-oldest survivor. A result for a tool that
// was never seen — a start we dropped at the cap, or one from before this stream — still proves
// the provider is alive, so it counts as progress; with other tools still open it changes nothing.
// A stream that declared no tool lifecycle has no ends to close either, and its events are ignored
// here for the same reason toolStart ignores them: the declaration is the authority.
func (w *providerWatchdog) toolEnd(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id == "" || !w.policy.toolLifecycle {
		return
	}
	if _, open := w.openTools[id]; !open {
		if len(w.openTools) == 0 {
			w.arm(w.policy.idle, outcomeIdleTimeout)
		}
		return
	}
	delete(w.openTools, id)
	i := slices.Index(w.toolOrder, id)
	wasOldest := i == 0
	w.toolOrder = slices.Delete(w.toolOrder, i, i+1)
	if len(w.openTools) == 0 {
		w.arm(w.policy.idle, outcomeIdleTimeout)
		return
	}
	if wasOldest {
		w.armToolCap()
	}
}

// armToolCap re-anchors the absolute cap on the oldest surviving open tool, for whatever is left
// of its budget. A survivor whose budget is already spent gets the smallest positive deadline
// rather than none: arm reads a non-positive duration as "this phase is disabled", which is right
// for a deadline the operator set to 0 and exactly wrong for a budget that ran out. The caller
// holds mu.
func (w *providerWatchdog) armToolCap() {
	if w.policy.tool <= 0 {
		w.arm(0, outcomeToolTimeout) // disabled phase: no clock at all
		return
	}
	remaining := w.openTools[w.toolOrder[0]].Add(w.policy.tool).Sub(w.now())
	w.arm(max(remaining, time.Nanosecond), outcomeToolTimeout)
}

// terminal is the provider's closing event (result / turn.completed / end). It counts as
// progress so a completion that lands just before a deadline is never misread as silence;
// box.Run returning stops the watchdog for good.
func (w *providerWatchdog) terminal() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.openTools) == 0 {
		w.arm(w.policy.idle, outcomeIdleTimeout)
	}
}
