package cli

import (
	"strings"
	"sync"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/ui"
)

// This file is the LOOP ENGINE's half of rate limiting: what one iteration's outcome means
// (classifyIteration), what the loop does about it under its caps (decideIteration), and the
// narrated sleep. The classification itself — is this prose a limit, when does it reset, how long
// to wait — lives in internal/ladder, shared with the ACP control and the sessions API, and so do
// the wall-clock wait mechanics the narration drives (ladder.WaitUntilWall).

func iterationAuthentication(provider, output string) bool {
	agent, ok := agents.Get(provider)
	if !ok {
		return false
	}
	for _, raw := range strings.Split(strings.ToLower(output), "\n") {
		line := strings.TrimSpace(raw)
		for _, signal := range agent.LiveCredentials().AuthSignals {
			signal = strings.ToLower(strings.TrimSpace(signal))
			if signal == "" {
				continue
			}
			if line == signal || strings.HasPrefix(line, signal+".") || strings.HasPrefix(line, signal+":") ||
				((strings.HasPrefix(line, "error:") || strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[")) && strings.Contains(line, signal)) {
				return true
			}
		}
	}
	return false
}

type iterationClassification struct {
	outcome string
	// detail is the explanation the outcome NAME cannot carry — today, the watchdog's account of
	// which deadline fired and the silence it observed. Telemetry keeps recording the bare outcome;
	// this is for the human reading the warning. Empty for outcomes that speak for themselves.
	detail string
	limit  ladder.LimitHint
}

// timeoutDetail renders a provider timeout's observed silence as a trailing clause for the
// operator-facing warnings, or "" when the classification carries none.
func (c iterationClassification) timeoutDetail() string {
	if c.detail == "" {
		return ""
	}
	return " after " + c.detail
}

func classifyIteration(provider string, code int, err error, diagnostic string, stream providerStreamOutcome, now time.Time) iterationClassification {
	if err == nil && code == box.DescendantsDrainedExit {
		return iterationClassification{outcome: "background_drained"}
	}
	if err == nil && code == box.DescendantsTimedOutExit {
		return iterationClassification{outcome: "background_timeout"}
	}
	if err == nil && code == 0 {
		return iterationClassification{outcome: "success"}
	}
	if stream == streamMalformed {
		return iterationClassification{outcome: "malformed_stream"}
	}
	if iterationAuthentication(provider, diagnostic) {
		return iterationClassification{outcome: "authentication"}
	}
	if hint := ladder.DetectIterationLimit(diagnostic, now); hint.Limited {
		if hint.OutputLimited {
			return iterationClassification{outcome: "output_limit", limit: hint}
		}
		return iterationClassification{outcome: "rate_limit", limit: hint}
	}
	return iterationClassification{outcome: "process_failure"}
}

func isBackgroundHandoff(outcome string) bool {
	return outcome == "background_drained" || outcome == "background_timeout"
}

// sleepForLimit pauses for the rate limit, narrating so a long wait visibly
// stays alive (and so an unattended log shows why nothing is happening). It
// returns early when wake fires — the loop's soft-stop path — so a Ctrl-C during
// a long wait takes effect instead of hanging until the reset.
func sleepForLimit(wait time.Duration, resetAt time.Time, wake <-chan struct{}) {
	sleepForLimitAt(wait, resetAt, wake, time.Now)
}

// sleepForLimitAt is sleepForLimit with an injectable clock, so a test can jump the wall clock
// past the reset mid-wait (simulating a laptop suspend) and assert the wait ends promptly.
func sleepForLimitAt(wait time.Duration, resetAt time.Time, wake <-chan struct{}, nowFn func() time.Time) {
	wait = wait.Round(time.Second)
	if wait <= 0 {
		return
	}
	until := ""
	if !resetAt.IsZero() {
		until = ", until " + resetAt.Local().Format("Mon 15:04 MST")
	}
	ui.Info("model rate limited — waiting %s%s, then continuing", wait, until)
	// ~20 progress ticks regardless of total, so a multi-day wait doesn't spam
	// the log (and a short one still reports more than once).
	narrate := wait / 20
	if narrate < time.Minute {
		narrate = time.Minute
	} else if narrate > time.Hour {
		narrate = time.Hour
	}
	start := nowFn()
	last := start
	// Anchor the wait to a WALL-clock deadline (start + wait, monotonic stripped by WaitUntilWall)
	// and re-check it on a short cadence, so a suspend that freezes the monotonic clock can't
	// inflate the wait past the real reset. Narration stays on the ~20-tick cadence via a wall-clock
	// elapsed check, independent of the shorter re-check ticks.
	ladder.WaitUntilWall(start.Add(wait), ladder.LimitTickCap, nowFn, wake, func(remaining time.Duration) {
		if t := nowFn(); t.Sub(last) >= narrate {
			last = t
			ui.Info("  …%s remaining", remaining.Round(time.Minute))
		}
	})
}

// loopAction is what loop() should do after one iteration.
type loopAction int

const (
	actContinue   loopAction = iota // success — advance to the next item
	actWait                         // rate/usage limited — pause, then retry this item
	actRetry                        // other failure — short backoff, then retry this item
	actRetryNow                     // output/token limit — immediately resume this item
	actStop                         // a cap tripped — give up
	actAuthStop                     // provider authentication failed — stop with login recovery
	actOutputStop                   // repeated output exhaustion hit its independent cap
)

const (
	// maxLoopFailures is how many non-rate-limit iteration failures the loop tolerates before
	// giving up (e.g. a wedged image or broken repo). Counted since the last successful iteration;
	// a rate-limit wait in between doesn't reset it (the build is still failing), so the failures
	// aren't necessarily back-to-back.
	maxLoopFailures = 5
	// maxLimitWaits is how many consecutive rate-limit pauses to ride out before
	// giving up — a backstop against a misfiring detector or a suspended account,
	// set far above the handful of resets a real long run hits.
	maxLimitWaits = 100
	// maxOutputRetries caps CONSECUTIVE output/token-limit resumes. One is the common
	// case (a turn that ran long — resume and it finishes); an UNBROKEN run means the
	// same iteration keeps maxing out with no progress (a model wedged on output, or a
	// gate whose failing output echoes "finish_reason: length"), so it gives up rather
	// than respawn the box forever. Sized like maxLoopFailures.
	maxOutputRetries = 5
	// maxStalls is how many consecutive work iterations may complete no task before the
	// loop gives up — a backstop against an in_progress/ task the agent keeps
	// continuing but can't finish, which would otherwise spin forever.
	maxStalls = 5
)

// The work↔signoff round cap is .agent/loop.yaml signoff.rounds (default 5):
// after each signoff pass the loop re-drains anything it reopened, so this bounds the
// ping-pong for a task that can't self-heal (signoffRoundOutcome below decides accept / continue /
// cap→block).

// signoffDecision is what the loop does after a signoff pass (see signoffRoundOutcome).
type signoffDecision int

const (
	signoffAccepted   signoffDecision = iota // the signoff reopened nothing — the queue is verified done (exit 0)
	signoffContinue                          // the signoff reopened work and rounds remain — drain again, then sign off again
	signoffCapReached                        // the signoff still reopens at the round cap — block the stuck task for a human (exit 3)
)

// signoffRoundCap scales the work↔signoff round cap with the batch: half the tasks worked this run,
// floored at 3 (a tiny batch still gets a few tries) and ceilinged at max (loop.yaml signoff.rounds),
// so a 100-task overnight batch caps at max instead of ping-ponging one stuck task forever. The
// floor is applied before the ceiling, so a max set BELOW 3 (signoff.rounds: 1, a one-shot signoff)
// still wins. Pure, so the clamp is unit-tested.
func signoffRoundCap(tasks, max int) int {
	cap := tasks / 2
	if cap < 3 {
		cap = 3
	}
	if cap > max {
		cap = max
	}
	return cap
}

// signoffRoundOutcome decides what loop() does after a signoff pass, given the just-finished round
// number (1-based), the cap, and whether the signoff reopened any actionable work (todo+in_progress
// > 0). Nothing reopened → accepted (done). Otherwise continue while rounds remain, else give up and
// block the persistently-reopened task. Pure, so the three convergence paths — accept-immediately,
// reopen-then-accept, never-converge → cap → block — are unit-tested without driving a box.
func signoffRoundOutcome(round, cap int, reopened bool) signoffDecision {
	if !reopened {
		return signoffAccepted
	}
	if round < cap {
		return signoffContinue
	}
	return signoffCapReached
}

// outputRetryBackoff spaces out consecutive output-limit resumes: the first is immediate
// (the fast path for a single long turn), later ones back off, so a misfire can't
// tight-loop box respawns before maxOutputRetries trips.
const outputRetryBackoff = 5 * time.Second

// decideIteration interprets one iteration's result, updates the failure/wait/retry
// counters in place, and returns the action loop() should take (with the pause and
// reset time for actWait, or the backoff for actRetryNow). Output/token limits are a
// separate retry action with their OWN cap, so an unbroken run of them gives up instead
// of resuming forever. Keeping the cap-and-counter logic here, pure and unit-tested,
// separates it from the container run and the actual sleeps. retries counts consecutive
// output-limit resumes; any other outcome resets it.
func decideIteration(classification iterationClassification, now time.Time, fails, waits, retries *int) (action loopAction, wait time.Duration, resetAt time.Time) {
	if classification.outcome == "success" {
		*fails, *waits, *retries = 0, 0, 0
		return actContinue, 0, time.Time{}
	}
	if classification.outcome == "authentication" {
		*retries = 0
		return actAuthStop, 0, time.Time{}
	}
	if hint := classification.limit; hint.Limited {
		if hint.OutputLimited {
			// An output limit is neither a failure nor a rate wait; only a consecutive RUN of
			// them is a problem. Cap it so a wedged iteration can't respawn the box forever.
			if *retries++; *retries > maxOutputRetries {
				return actOutputStop, 0, time.Time{}
			}
			if *retries == 1 {
				return actRetryNow, 0, time.Time{} // fast path: a single long turn resumes at once
			}
			return actRetryNow, outputRetryBackoff, time.Time{}
		}
		*retries = 0 // a rate wait breaks any output-limit run
		if *waits++; *waits > maxLimitWaits {
			return actStop, 0, time.Time{}
		}
		return actWait, ladder.LimitWait(hint, *waits, now), hint.ResetAt
	}
	*retries = 0 // a plain failure breaks any output-limit run
	*waits = 0   // rate-limit pauses are consecutive; an ordinary failure breaks the run
	if *fails++; *fails >= maxLoopFailures {
		return actStop, 0, time.Time{}
	}
	return actRetry, 0, time.Time{}
}

// progressStall tracks whether the loop is still moving tasks OUT of the actionable set. Given the
// queue's settled count (done + blocked) after a work iteration, the running baseline, and the stall
// counter, it resets the counter when that count CHANGES — a task finished OR got parked on a human
// decision, or an audit reopened one / a torn read undercounted: either way the queue moved — and
// bumps it only when nothing settled; it reports stop once maxStalls iterations pass with no movement
// (the active task, often a continued in_progress/ one, can't be finished and isn't being parked
// either). Keying on "changed" (not "advanced") means a dip-then-recover isn't a false stall, and
// counting blocked — not just done — means triaging one-way doors into 50_blocked/ is progress, not
// a "stuck" stop.
func progressStall(settled, baseline, stalls int) (newBaseline, newStalls int, stop bool) {
	if settled != baseline {
		return settled, 0, false
	}
	return baseline, stalls + 1, stalls+1 >= maxStalls
}

// tailWriter keeps the last max bytes written to it, so a long run's output can
// be scanned for a rate-limit notice without buffering all of it. It is safe for
// the concurrent stdout/stderr copy goroutines os/exec uses.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}
