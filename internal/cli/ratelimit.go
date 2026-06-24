package cli

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// limitHint is what an iteration's output told us about a model rate/usage limit.
type limitHint struct {
	limited bool      // the model is rate- or usage-limited
	resetAt time.Time // when it resets (zero = unknown)
}

var (
	// Claude prints this in headless mode when a subscription limit is hit:
	// "Claude AI usage limit reached|<unix_epoch>" — the epoch is the reset time.
	usageLimitRe = regexp.MustCompile(`(?i)usage limit reached\s*\|\s*(\d{9,})`)
	// Newer human-readable subscription notice (also seen in headless output):
	// "You've hit your weekly limit · resets Jun 18, 8pm (UTC)". The window word
	// ("weekly", "5-hour", …) varies and the "resets …" clause may be absent.
	hitLimitRe = regexp.MustCompile(`(?i)hit your (?:[\w-]+ )?limit`)
	// The "resets <when>" clause that follows it, when present.
	resetsRe = regexp.MustCompile(`(?i)resets?\s+([^\n·]+)`)
	// A trailing timezone in parens at the end of that clause, e.g. "(UTC)".
	tzParenRe = regexp.MustCompile(`\(([A-Za-z]{2,5})\)\s*$`)
	// API-style hints carrying a delay: "retry after 30", "retry-after: 30s",
	// "try again in 30 seconds".
	retryAfterRe = regexp.MustCompile(`(?i)(?:retry[ -]?after|try again in)[^\d]{0,8}(\d{1,7})\s*(?:s\b|sec|second)`)
	// Broad markers with no parseable reset — a limit we should back off from.
	limitMarkers = []string{
		"usage limit", "rate limit", "rate-limit", "rate limited",
		"ratelimited", "overloaded", "quota", "429",
	}
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
	// "You've hit your weekly limit · resets Jun 18, 8pm (UTC)" — parse the stated
	// reset so the loop sleeps until then rather than backing off into the wall.
	if hitLimitRe.MatchString(output) {
		return limitHint{limited: true, resetAt: parseResetTime(output, now)}
	}
	lower := strings.ToLower(output)
	if m := retryAfterRe.FindStringSubmatch(lower); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return limitHint{limited: true, resetAt: now.Add(time.Duration(n) * time.Second)}
		}
	}
	for _, mark := range limitMarkers {
		if strings.Contains(lower, mark) {
			return limitHint{limited: true}
		}
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
	s := strings.TrimSpace(m[1])
	loc := time.Local
	if tz := tzParenRe.FindStringSubmatch(s); tz != nil {
		switch strings.ToUpper(tz[1]) {
		case "UTC", "GMT", "Z":
			loc = time.UTC
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

// sleepForLimit pauses for the rate limit, narrating so a long wait visibly
// stays alive (and so an unattended log shows why nothing is happening). It is
// interruptible with Ctrl-C like the rest of the loop.
func sleepForLimit(wait time.Duration, resetAt time.Time) {
	wait = wait.Round(time.Second)
	if wait <= 0 {
		return
	}
	until := ""
	if !resetAt.IsZero() {
		until = ", until " + resetAt.Local().Format("Mon 15:04 MST")
	}
	ui.Info("model rate limited — waiting %s%s, then continuing", wait, until)
	deadline := time.Now().Add(wait)
	// ~20 progress ticks regardless of total, so a multi-day wait doesn't spam
	// the log (and a short one still reports more than once).
	tick := wait / 20
	if tick < time.Minute {
		tick = time.Minute
	} else if tick > time.Hour {
		tick = time.Hour
	}
	for {
		remaining := time.Until(deadline)
		if remaining <= tick {
			if remaining > 0 {
				time.Sleep(remaining)
			}
			return
		}
		time.Sleep(tick)
		ui.Info("  …%s remaining", time.Until(deadline).Round(time.Minute))
	}
}

// loopAction is what loop() should do after one iteration.
type loopAction int

const (
	actContinue loopAction = iota // success — advance to the next item
	actWait                       // rate/usage limited — pause, then retry this item
	actRetry                      // other failure — short backoff, then retry this item
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
	// maxStalls is how many consecutive work iterations may complete no task before the
	// loop gives up — a backstop against an in-progress ([w]) task the agent keeps
	// continuing but can't finish, which would otherwise spin forever.
	maxStalls = 5
)

// decideIteration interprets one iteration's result, updates the failure/wait
// counters in place, and returns the action loop() should take (with the pause
// and reset time for actWait). Keeping the cap-and-counter logic here, pure and
// unit-tested, separates it from the container run and the actual sleeps.
func decideIteration(code int, err error, out string, now time.Time, fails, waits *int) (action loopAction, wait time.Duration, resetAt time.Time) {
	if err == nil && code == 0 {
		*fails, *waits = 0, 0
		return actContinue, 0, time.Time{}
	}
	if hint := detectLimit(out, now); hint.limited {
		if *waits++; *waits > maxLimitWaits {
			return actStop, 0, time.Time{}
		}
		return actWait, limitWait(hint, *waits, now), hint.resetAt
	}
	if *fails++; *fails >= maxLoopFailures {
		return actStop, 0, time.Time{}
	}
	return actRetry, 0, time.Time{}
}

// progressStall tracks whether the loop is still completing tasks. Given the queue's Done count
// after a work iteration, the running baseline, and the stall counter, it resets the counter when
// Done CHANGES (a task finished, or an audit reopened one / a torn read undercounted — either way the
// queue moved) and bumps it only when Done is unchanged; it reports stop once maxStalls iterations
// pass with no movement — the signal that the active task (often a continued [w]) can't be finished.
// Keying on "changed" (not "advanced") means a Done dip-then-recover isn't a false stall.
func progressStall(done, baseline, stalls int) (newBaseline, newStalls int, stop bool) {
	if done != baseline {
		return done, 0, false
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
