package cli

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// limitHint is what an iteration's output told us about a model rate/usage limit or output/token exhaustion.
type limitHint struct {
	limited       bool      // the model is rate- or usage-limited
	outputLimited bool      // the model reached its maximum output length limit
	resetAt       time.Time // when it resets (zero = unknown)
}

var (
	// Claude prints this in headless mode when a subscription limit is hit:
	// "Claude AI usage limit reached|<unix_epoch>" — the epoch is the reset time.
	usageLimitRe = regexp.MustCompile(`(?i)usage limit reached\s*\|\s*(\d{9,})`)
	// Newer human-readable subscription notice (also seen in headless output and ACP errors):
	// "You've hit your weekly limit · resets Jun 18, 8pm (UTC)" and "You've reached your Fable 5
	// limit. Run /usage-credits …". The verb (hit/reached) and the descriptor between "your" and
	// "limit" — a window ("weekly") or a model name ("Fable 5", "Opus 4.8") — both vary, and the
	// "resets …" clause may be absent; allow up to a few descriptor words.
	hitLimitRe = regexp.MustCompile(`(?i)(?:hit|reached) your (?:[\w.-]+ ){0,3}limit`)
	// The "resets <when>" or "try again at <when>" clause that follows it, when present.
	resetsRe = regexp.MustCompile(`(?i)(?:resets?|try again at)\s+([^\n·]+)`)
	// A trailing timezone in parens at the end of that clause, e.g. "(UTC)".
	tzParenRe = regexp.MustCompile(`\(([A-Za-z]{2,5})\)\s*$`)
	// API-style hints carrying a delay: "retry-after: 30" (bare = seconds), "retry after 30s",
	// "try again in 5 minutes", "retry after 2 hours". The unit is optional and scaled by its
	// first letter (m→minutes, h→hours, else seconds) in the caller.
	retryAfterRe = regexp.MustCompile(`(?i)(?:retry[ -]?after|try again in)[^\d]{0,8}(\d{1,7})\s*([a-z]+)?`)
	// Output/token exhaustion is recoverable by immediately asking the same model to continue; it is
	// not a provider rate limit, so it must not rotate credentials or sleep until a reset.
	outputLimitRe = regexp.MustCompile(`(?i)(?:output limit|max(?:imum)? output length|max(?:imum)?[_ -]?output[_ -]?tokens?|output length limit|finish[_ ]?reason["'\s:=]+(?:length|max[_ -]?tokens?))`)
	// Broad markers with no parseable reset — a limit we should back off from. "at capacity"
	// is codex's model-overload notice ("Selected model is at capacity. Please try a different
	// model.") — a transient provider-side limit handled like an overload: rotate to the next
	// rung (a different model heeds its own advice), or back off until it clears.
	limitMarkers = []string{
		"usage limit", "rate limit", "rate-limit", "rate limited",
		"ratelimited", "overloaded", "at capacity", "resource exhausted",
		"quota exceeded", "quota limit", "exceeded quota", "insufficient quota",
		"usagelimit", "usagelimitexceeded",
	}
	// The HTTP 429 status, matched with word boundaries. A bare Contains(lower, "429") also
	// matched an unrelated number an agent happened to print — a line count ("1429 files"), a
	// byte offset, a hash fragment ("…429…") — turning an ordinary failed iteration into a
	// rate-limit wait. \b429\b fires only on the standalone status a real 429 is reported as.
	status429Re = regexp.MustCompile(`\b429\b`)
)

// detectLimit inspects an iteration's captured output for a model rate/usage
// limit and, when present, when it resets. `now` anchors relative hints like
// "retry after N". Precise signals (the usage-limit epoch, an explicit retry
// delay) win over the broad keyword fallback.
func detectLimit(output string, now time.Time) limitHint {
	if m := usageLimitRe.FindStringSubmatch(output); m != nil {
		if epoch, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if epoch > 1e12 { // tolerate a millisecond epoch
				epoch /= 1000
			}
			return limitHint{limited: true, resetAt: time.Unix(epoch, 0)}
		}
		return limitHint{limited: true}
	}
	if outputLimitRe.MatchString(output) {
		return limitHint{limited: true, outputLimited: true}
	}
	// "You've hit your weekly limit · resets Jun 18, 8pm (UTC)" — parse the stated
	// reset so the loop sleeps until then rather than backing off into the wall.
	if hitLimitRe.MatchString(output) {
		return limitHint{limited: true, resetAt: parseResetTime(output, now)}
	}
	lower := strings.ToLower(output)
	if m := retryAfterRe.FindStringSubmatch(lower); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			unit, isTime := time.Second, true // a bare number (HTTP Retry-After) is seconds
			if len(m[2]) > 0 {
				switch m[2][0] {
				case 's':
					unit = time.Second
				case 'm':
					unit = time.Minute
				case 'h':
					unit = time.Hour
				default:
					// A non-time unit ("retry after 3 attempts", "try again in 2 ways") is ordinary
					// prose, not a limit — don't treat it as one.
					isTime = false
				}
			}
			if isTime {
				dur := time.Duration(n) * unit
				if dur < 0 { // overflow on an absurd count (millions of hours) — saturate; limitWait caps it
					dur = limitMaxWait
				}
				return limitHint{limited: true, resetAt: now.Add(dur)}
			}
		}
	}
	for _, mark := range limitMarkers {
		if strings.Contains(lower, mark) {
			return limitHint{limited: true}
		}
	}
	if status429Re.MatchString(lower) {
		return limitHint{limited: true}
	}
	return limitHint{}
}

// parseResetTime reads the "resets <when>" clause of a subscription-limit notice
// — "resets Jun 18, 8pm (UTC)" or a bare "resets 11am" — into an absolute time.
// `now` supplies the missing year (and the date, for a time-only reset). A zero
// return means "not stated / unrecognized" — the caller then backs off instead.
func parseResetTime(output string, now time.Time) time.Time {
	m := resetsRe.FindStringSubmatch(output)
	if m == nil {
		return time.Time{}
	}
	s := strings.ToLower(strings.TrimSpace(m[1]))
	// Strip spaces before AM/PM so "3:04 PM" becomes "3:04PM" matching Go's "3:04pm" layout
	s = regexp.MustCompile(`(?i)\s+(am|pm)\b`).ReplaceAllString(s, "$1")
	loc := time.Local
	if tz := tzParenRe.FindStringSubmatch(s); tz != nil {
		switch strings.ToUpper(tz[1]) {
		case "UTC", "GMT", "Z":
			loc = time.UTC
		default:
			// A stated but unrecognized zone (PST, ET, CET, …): don't silently reinterpret the
			// time in the HOST's zone — on a UTC server that can be hours early, waking the loop
			// before the real reset so it re-hits the limit. Fall back to backoff instead.
			return time.Time{}
		}
		s = strings.TrimSpace(s[:len(s)-len(tz[0])])
	}
	s = strings.TrimRight(s, " .,")
	// Date + time: "Jun 18, 8pm" / "Jun 18, 8:30pm" (comma optional). The layout
	// carries no year, so rebuild with now's year and roll forward past a stale
	// month (a December notice that resets in January).
	for _, lay := range []string{"Jan 2, 3:04pm", "Jan 2, 3pm", "Jan 2 3:04pm", "Jan 2 3pm"} {
		if t, err := time.ParseInLocation(lay, s, loc); err == nil {
			r := time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if r.Before(now.Add(-24 * time.Hour)) {
				r = r.AddDate(1, 0, 0)
			}
			return r
		}
	}
	// Time only: "11am" / "8:30pm" — the next time that clock reading comes round.
	for _, lay := range []string{"3:04pm", "3pm"} {
		if t, err := time.ParseInLocation(lay, s, loc); err == nil {
			r := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
			if !r.After(now) {
				r = r.AddDate(0, 0, 1)
			}
			return r
		}
	}
	return time.Time{}
}

// Wait bounds for a rate-limit pause.
const (
	limitBuffer  = 5 * time.Second    // grace past a known reset, for clock skew
	limitMinWait = 10 * time.Second   // never busy-spin
	limitMaxWait = 8 * 24 * time.Hour // spans the longest window (weekly), still bounds a bad parse
)

// limitWait computes how long to pause before retrying after a rate limit. With
// a known reset it waits until then (plus a small buffer); otherwise it backs
// off exponentially by attempt (1m, 2m, 4m … capped). The result is clamped to
// [limitMinWait, limitMaxWait].
func limitWait(hint limitHint, attempt int, now time.Time) time.Duration {
	var d time.Duration
	if !hint.resetAt.IsZero() {
		d = hint.resetAt.Sub(now) + limitBuffer
	} else {
		shift := attempt - 1
		if shift < 0 {
			shift = 0
		}
		if shift > 5 {
			shift = 5
		}
		d = time.Minute << uint(shift)
	}
	if d < limitMinWait {
		d = limitMinWait
	}
	if d > limitMaxWait {
		d = limitMaxWait
	}
	return d
}

// sleepOrWake waits up to d, returning early with false if wake fires (or is closed) — so the
// loop's pauses end promptly when a stop is requested, now that the loop catches SIGINT instead
// of dying on it. A nil wake never fires; true means it slept the full d.
func sleepOrWake(d time.Duration, wake <-chan struct{}) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-wake:
		return false
	}
}

// limitTickCap bounds each sleep segment of a rate-limit wait. Go timers run on the MONOTONIC
// clock, which freezes while a laptop is suspended (macOS lid closed) and does not fire during
// sleep — so a single long timer resumes on wake still owing the pre-suspend remainder and
// over-waits past the real reset by roughly the closed duration. Waking at least this often
// re-derives the remaining time from the WALL clock, so reopening the lid past the reset ends the
// wait within one tick instead of counting leftover awake-time.
const limitTickCap = time.Minute

// waitUntilWall blocks until `deadline` passes on the WALL clock, or until stop fires. It strips
// the monotonic reading from deadline and re-compares against nowFn() each cycle, sleeping at most
// tickCap between checks, so a system-suspend gap can't inflate the wait: a frozen monotonic timer
// resumes and re-evaluates against the real clock, returning promptly once the deadline is past.
// onSegment, when non-nil, is called with the wall-clock remaining after each FULL tickCap segment
// (not the final partial one) for progress narration. Returns true if it waited to the deadline,
// false if stop fired first. nowFn defaults to time.Now when nil; it is injectable for tests.
func waitUntilWall(deadline time.Time, tickCap time.Duration, nowFn func() time.Time, stop <-chan struct{}, onSegment func(remaining time.Duration)) bool {
	if nowFn == nil {
		nowFn = time.Now
	}
	deadline = deadline.Round(0) // drop the monotonic reading so Sub uses the wall clock
	for {
		remaining := deadline.Sub(nowFn())
		if remaining <= 0 {
			return true
		}
		seg, capped := remaining, false
		if seg > tickCap {
			seg, capped = tickCap, true
		}
		if !sleepOrWake(seg, stop) {
			return false // stop requested — bail out of the wait
		}
		if capped && onSegment != nil {
			onSegment(deadline.Sub(nowFn()))
		}
	}
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
	// Anchor the wait to a WALL-clock deadline (start + wait, monotonic stripped by waitUntilWall)
	// and re-check it on a short cadence, so a suspend that freezes the monotonic clock can't
	// inflate the wait past the real reset. Narration stays on the ~20-tick cadence via a wall-clock
	// elapsed check, independent of the shorter re-check ticks.
	waitUntilWall(start.Add(wait), limitTickCap, nowFn, wake, func(remaining time.Duration) {
		if t := nowFn(); t.Sub(last) >= narrate {
			last = t
			ui.Info("  …%s remaining", remaining.Round(time.Minute))
		}
	})
}

// loopAction is what loop() should do after one iteration.
type loopAction int

const (
	actContinue loopAction = iota // success — advance to the next item
	actWait                       // rate/usage limited — pause, then retry this item
	actRetry                      // other failure — short backoff, then retry this item
	actRetryNow                   // output/token limit — immediately resume this item
	actStop                       // a cap tripped — give up
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
func decideIteration(code int, err error, out string, now time.Time, fails, waits, retries *int) (action loopAction, wait time.Duration, resetAt time.Time) {
	if err == nil && code == 0 {
		*fails, *waits, *retries = 0, 0, 0
		return actContinue, 0, time.Time{}
	}
	if hint := detectLimit(out, now); hint.limited {
		if hint.outputLimited {
			// An output limit is neither a failure nor a rate wait; only a consecutive RUN of
			// them is a problem. Cap it so a wedged iteration can't respawn the box forever.
			if *retries++; *retries > maxOutputRetries {
				*retries = 0
				return actStop, 0, time.Time{}
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
		return actWait, limitWait(hint, *waits, now), hint.resetAt
	}
	*retries = 0 // a plain failure breaks any output-limit run
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
