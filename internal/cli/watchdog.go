package cli

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// The provider-attempt watchdog bounds SILENCE, not work: a built-in loop/review/preflight
// attempt must show its first model action within the start deadline, keep producing
// adapter-recognized activity within the idle deadline, and finish its oldest open foreground
// tool within the tool deadline. Only semantic stream events (streamActivity) feed it — never
// process names, CPU, lease heartbeats, redraws, or raw bytes — so a wedged provider is killed
// while long reasoning and a slow foreground gate survive.
// providerIdleDeadline must stay comfortably ABOVE the box's descendant-drain wait
// (COOP_DESCENDANT_TIMEOUT in internal/box/image.go, 10m). A draining box emits no stream events,
// so the two clocks run from the same instant: if the drain could reach this deadline, every box
// held open by a leaked descendant would be killed as a wedged provider instead of surfacing as a
// descendant handoff, and the drain's own exit codes would never be observed.
const (
	providerStartDeadline = 10 * time.Minute
	providerIdleDeadline  = 30 * time.Minute
	providerToolDeadline  = 2 * time.Hour
)

// Provider-attempt timeout outcomes, recorded verbatim in stage telemetry and handled by the
// loop's dedicated timeout policy: rotate to the next usable rung without cooling, capped at
// maxProviderTimeouts consecutive timeouts per stage.
const (
	outcomeStartTimeout = "provider_start_timeout"
	outcomeIdleTimeout  = "provider_idle_timeout"
	outcomeToolTimeout  = "provider_tool_timeout"
)

// maxProviderTimeouts caps CONSECUTIVE provider-attempt timeouts. Timeouts keep their own
// counter so they never consume the ordinary failure, stall, output, or rate-limit budgets.
const maxProviderTimeouts = 3

func isProviderTimeout(outcome string) bool {
	switch outcome {
	case outcomeStartTimeout, outcomeIdleTimeout, outcomeToolTimeout:
		return true
	}
	return false
}

type watchdogDeadlines struct {
	start, idle, tool time.Duration
}

// watchdogDeadlinesFor resolves the fixed conservative defaults, honoring the internal
// COOP_PROVIDER_TIMEOUTS override ("start=2s,idle=3s,tool=6s"). The override exists so
// deterministic fixture tests can shorten the deadlines — it is not a user knob, and any
// malformed field keeps every default rather than half-applying.
func watchdogDeadlinesFor(cfg *config.Config) watchdogDeadlines {
	defaults := watchdogDeadlines{
		start: providerStartDeadline, idle: providerIdleDeadline, tool: providerToolDeadline,
	}
	raw := strings.TrimSpace(cfg.ProviderTimeouts)
	if raw == "" {
		return defaults
	}
	parsed := defaults
	for _, field := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			return defaults
		}
		dur, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || dur <= 0 {
			return defaults
		}
		switch strings.TrimSpace(key) {
		case "start":
			parsed.start = dur
		case "idle":
			parsed.idle = dur
		case "tool":
			parsed.tool = dur
		default:
			return defaults
		}
	}
	return parsed
}

// watchdogTimer is the one armable deadline the watchdog holds — time.AfterFunc in
// production, a hand-fired fake in tests.
type watchdogTimer interface{ Stop() bool }

// providerWatchdog supervises one provider attempt. It is the decoder's streamActivity sink:
// each recognized event re-arms the single deadline for the current phase (start → idle, with
// idle suspended while foreground tools are open and the oldest open tool absolutely capped).
// When a deadline fires it records the timeout outcome once and cancels the child box context;
// stop() ends supervision the moment box.Run returns, so nothing can fire afterwards.
type providerWatchdog struct {
	mu       sync.Mutex
	deadline watchdogDeadlines
	cancel   func()
	now      func() time.Time
	after    func(time.Duration, func()) watchdogTimer

	timer     watchdogTimer
	gen       uint64
	openTools map[string]time.Time
	toolOrder []string
	stopped   bool
	fired     string
}

func newProviderWatchdog(deadline watchdogDeadlines, cancel func()) *providerWatchdog {
	return startProviderWatchdog(deadline, cancel, time.Now, func(d time.Duration, fn func()) watchdogTimer {
		return time.AfterFunc(d, fn)
	})
}

// startProviderWatchdog arms the start deadline immediately; the clock and timer factory are
// injected so unit tests drive every transition deterministically.
func startProviderWatchdog(deadline watchdogDeadlines, cancel func(), now func() time.Time, after func(time.Duration, func()) watchdogTimer) *providerWatchdog {
	w := &providerWatchdog{
		deadline: deadline, cancel: cancel, now: now, after: after,
		openTools: map[string]time.Time{},
	}
	w.mu.Lock()
	w.arm(deadline.start, outcomeStartTimeout)
	w.mu.Unlock()
	return w
}

// arm replaces the active deadline. The caller holds mu; the generation guard makes a
// concurrently-firing stale timer a no-op.
func (w *providerWatchdog) arm(d time.Duration, outcome string) {
	if w.stopped || w.fired != "" {
		return
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	w.gen++
	gen := w.gen
	w.timer = w.after(d, func() { w.fire(gen, outcome) })
}

func (w *providerWatchdog) fire(gen uint64, outcome string) {
	w.mu.Lock()
	if w.stopped || w.fired != "" || gen != w.gen {
		w.mu.Unlock()
		return
	}
	w.fired = outcome
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
}

// timedOut reports the recorded timeout outcome, or "" when no deadline fired.
func (w *providerWatchdog) timedOut() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// bootstrap proves the provider CLI launched, not that the model acted — the start deadline
// deliberately stands until the first model action.
func (w *providerWatchdog) bootstrap() {}

// progress is any adapter-recognized model action: assistant text, reasoning, or a recognized
// stream item. It arms (or re-arms) the idle deadline unless a foreground tool suspends it.
func (w *providerWatchdog) progress() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.openTools) == 0 {
		w.arm(w.deadline.idle, outcomeIdleTimeout)
	}
}

// toolStart opens a foreground tool: the ordinary idle deadline is suspended while any tool is
// open, and the OLDEST open tool is capped absolutely so a wedged child cannot hold the
// attempt forever. An ID-less start is indistinguishable from plain progress.
func (w *providerWatchdog) toolStart(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if id == "" {
		if len(w.openTools) == 0 {
			w.arm(w.deadline.idle, outcomeIdleTimeout)
		}
		return
	}
	if _, open := w.openTools[id]; open {
		return
	}
	w.openTools[id] = w.now()
	w.toolOrder = append(w.toolOrder, id)
	if len(w.openTools) == 1 {
		w.arm(w.deadline.tool, outcomeToolTimeout)
	}
}

// toolEnd closes an open tool. When the last tool closes the idle deadline resumes from now;
// when the oldest closes the cap re-anchors to the next-oldest survivor. A result for a tool
// that was never seen still proves the provider is alive, so it counts as progress.
func (w *providerWatchdog) toolEnd(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, open := w.openTools[id]; !open {
		if len(w.openTools) == 0 {
			w.arm(w.deadline.idle, outcomeIdleTimeout)
		}
		return
	}
	delete(w.openTools, id)
	i := slices.Index(w.toolOrder, id)
	wasOldest := i == 0
	w.toolOrder = slices.Delete(w.toolOrder, i, i+1)
	if len(w.openTools) == 0 {
		w.arm(w.deadline.idle, outcomeIdleTimeout)
		return
	}
	if wasOldest {
		w.arm(w.openTools[w.toolOrder[0]].Add(w.deadline.tool).Sub(w.now()), outcomeToolTimeout)
	}
}

// terminal is the provider's closing event (result / turn.completed / end). It counts as
// progress so a completion that lands just before a deadline is never misread as silence;
// box.Run returning stops the watchdog for good.
func (w *providerWatchdog) terminal() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.openTools) == 0 {
		w.arm(w.deadline.idle, outcomeIdleTimeout)
	}
}
