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

// Wait bounds for a rate-limit pause.
const (
	limitBuffer  = 5 * time.Second  // grace past a known reset, for clock skew
	limitMinWait = 10 * time.Second // never busy-spin
	limitMaxWait = time.Hour        // never sleep absurdly long on a bad parse
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
		until = ", until " + resetAt.Local().Format("15:04 MST")
	}
	ui.Info("model rate limited — waiting %s%s, then continuing", wait, until)
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= time.Minute {
			if remaining > 0 {
				time.Sleep(remaining)
			}
			return
		}
		time.Sleep(time.Minute)
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
	// maxLoopFailures is how many consecutive non-rate-limit iteration failures
	// the loop tolerates before giving up (e.g. a wedged image or broken repo).
	maxLoopFailures = 5
	// maxLimitWaits is how many consecutive rate-limit pauses to ride out before
	// giving up — a backstop against a misfiring detector or a suspended account,
	// set far above the handful of resets a real long run hits.
	maxLimitWaits = 100
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
