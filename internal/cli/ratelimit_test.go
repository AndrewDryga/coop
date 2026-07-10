package cli

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDetectLimit(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	cases := []struct {
		name              string
		output            string
		wantLimited       bool
		wantOutputLimited bool
		wantReset         time.Time // zero = expect unknown
	}{
		{"claude usage limit with epoch",
			"…working…\nClaude AI usage limit reached|1700000000\n", true, false, time.Unix(1700000000, 0)},
		{"usage limit with millisecond epoch",
			"Claude AI usage limit reached|1700000000000", true, false, time.Unix(1700000000, 0)},
		{"retry-after seconds",
			"Error: rate limited. Please retry after 45s.", true, false, now.Add(45 * time.Second)},
		{"try again in N seconds",
			"overloaded; try again in 30 seconds", true, false, now.Add(30 * time.Second)},
		{"retry after N minutes",
			"rate limited; try again in 5 minutes", true, false, now.Add(5 * time.Minute)},
		{"retry after N hours",
			"Please retry after 2 hours.", true, false, now.Add(2 * time.Hour)},
		{"bare http retry-after (seconds)",
			"429; retry-after: 30", true, false, now.Add(30 * time.Second)},
		// A non-time unit ("attempts", "ways") is ordinary prose, not a retry-after — don't trip.
		{"non-time unit (attempts) is not a limit",
			"I'll retry after 3 attempts to fix the test", false, false, time.Time{}},
		{"non-time unit (ways) is not a limit",
			"let me try again in 2 ways", false, false, time.Time{}},
		// An absurd hours value overflows int64; it must saturate to a long wait, not flip negative
		// (which would make limitWait clamp to the 10s minimum — a busy retry against a real limit).
		{"absurd retry-after hours saturates",
			"Please retry after 9999999 hours.", true, false, now.Add(limitMaxWait)},
		{"broad rate-limit keyword, no reset",
			"request failed: rate limit exceeded", true, false, time.Time{}},
		{"http 429, no reset",
			"HTTP 429 Too Many Requests", true, false, time.Time{}},
		{"weekly subscription limit with stated reset",
			"coop: shadowed 4 secret path(s)\nYou've hit your weekly limit · resets Oct 18, 8pm (UTC)\n",
			true, false, time.Date(now.Year(), time.October, 18, 20, 0, 0, 0, time.UTC)},
		{"subscription limit with no reset clause",
			"You've hit your weekly limit.", true, false, time.Time{}},
		{"normal success output",
			"flipped [x], committed abc123, done", false, false, time.Time{}},
		{"unrelated failure",
			"Error: file not found: foo.go", false, false, time.Time{}},
		{"429 inside a larger number is not a limit",
			"build failed: 1429 files scanned, exit 1", false, false, time.Time{}},
		{"quota in an unrelated type name is not a limit",
			"generated PolicyRuleQuotaResponse type", false, false, time.Time{}},
		{"codexErrorInfo field name alone is not a limit",
			`{"error":{"message":"provider failed","codexErrorInfo":"internalServerError"}}`, false, false, time.Time{}},
		{"gemini output limit reached",
			"Output Limit Reached\nThe model stopped because it reached its maximum output length.", true, true, time.Time{}},
		{"gemini finish reason max tokens",
			`{"finishReason":"MAX_TOKENS"}`, true, true, time.Time{}},
		{"codex finish reason length",
			`{"finish_reason":"length"}`, true, true, time.Time{}},
		{"token-per-minute rate limit is not an output limit",
			"rate limit exceeded for max_tokens_per_minute quota", true, false, time.Time{}},
		{"codex usage limit with absolute reset time",
			"You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 1:26 AM.",
			true, false, time.Date(now.Year(), now.Month(), now.Day()+1, 1, 26, 0, 0, time.Local)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectLimit(c.output, now)
			if got.limited != c.wantLimited {
				t.Fatalf("limited = %v, want %v", got.limited, c.wantLimited)
			}
			if got.outputLimited != c.wantOutputLimited {
				t.Fatalf("outputLimited = %v, want %v", got.outputLimited, c.wantOutputLimited)
			}
			if c.wantReset.IsZero() {
				if !got.resetAt.IsZero() {
					t.Errorf("resetAt = %v, want zero", got.resetAt)
				}
			} else if !got.resetAt.Equal(c.wantReset) {
				t.Errorf("resetAt = %v, want %v", got.resetAt, c.wantReset)
			}
		})
	}
}

func TestParseResetTime(t *testing.T) {
	base := time.Date(2026, time.June, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		in   string
		want time.Time // zero = expect no parse
	}{
		{"date and time in utc",
			base, "You've hit your weekly limit · resets Jun 18, 8pm (UTC)",
			time.Date(2026, time.June, 18, 20, 0, 0, 0, time.UTC)},
		{"date and time with minutes",
			base, "resets Jun 18, 8:30pm (UTC)",
			time.Date(2026, time.June, 18, 20, 30, 0, 0, time.UTC)},
		{"time only, later today",
			base, "resets 5pm (UTC)",
			time.Date(2026, time.June, 16, 17, 0, 0, 0, time.UTC)},
		{"time only, already past, rolls to tomorrow",
			base, "resets 9am (UTC)",
			time.Date(2026, time.June, 17, 9, 0, 0, 0, time.UTC)},
		{"december notice resetting in january rolls the year",
			time.Date(2026, time.December, 30, 12, 0, 0, 0, time.UTC), "resets Jan 2, 8am (UTC)",
			time.Date(2027, time.January, 2, 8, 0, 0, 0, time.UTC)},
		{"no resets clause", base, "You've hit your weekly limit.", time.Time{}},
		{"unparseable when", base, "resets soon, hang tight", time.Time{}},
		{"unrecognized tz falls back to backoff", base, "resets Jun 18, 8pm (PST)", time.Time{}},
		{"try again at am time",
			base, "try again at 1:26 AM",
			time.Date(2026, time.June, 17, 1, 26, 0, 0, time.Local)},
		{"try again at pm time with space",
			base, "try again at 8:30 PM",
			time.Date(2026, time.June, 16, 20, 30, 0, 0, time.Local)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseResetTime(c.in, c.now)
			if c.want.IsZero() {
				if !got.IsZero() {
					t.Errorf("parseResetTime = %v, want zero", got)
				}
			} else if !got.Equal(c.want) {
				t.Errorf("parseResetTime = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLimitWait(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	cases := []struct {
		name    string
		hint    limitHint
		attempt int
		want    time.Duration
	}{
		{"known reset waits until then plus buffer",
			limitHint{limited: true, resetAt: now.Add(10 * time.Minute)}, 1, 10*time.Minute + limitBuffer},
		{"past reset clamps to the minimum",
			limitHint{limited: true, resetAt: now.Add(-time.Hour)}, 1, limitMinWait},
		{"a multi-hour reset is honored, not clamped to an hour",
			limitHint{limited: true, resetAt: now.Add(3 * time.Hour)}, 1, 3*time.Hour + limitBuffer},
		{"a multi-day weekly reset is honored",
			limitHint{limited: true, resetAt: now.Add(48 * time.Hour)}, 1, 48*time.Hour + limitBuffer},
		{"an absurd far-future reset clamps to the ceiling",
			limitHint{limited: true, resetAt: now.Add(30 * 24 * time.Hour)}, 1, limitMaxWait},
		{"unknown reset backs off: attempt 1 → 1m", limitHint{limited: true}, 1, time.Minute},
		{"unknown reset backs off: attempt 3 → 4m", limitHint{limited: true}, 3, 4 * time.Minute},
		{"unknown reset backs off: capped at 32m", limitHint{limited: true}, 99, 32 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := limitWait(c.hint, c.attempt, now); got != c.want {
				t.Errorf("limitWait = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecideIteration(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)

	// Success resets both counters and advances.
	fails, waits, retries := 3, 2, 0
	if a, _, _ := decideIteration(0, nil, "done", now, &fails, &waits, &retries); a != actContinue {
		t.Errorf("success: action = %d, want actContinue", a)
	}
	if fails != 0 || waits != 0 {
		t.Errorf("success should reset counters, got fails=%d waits=%d", fails, waits)
	}

	// A rate limit bumps only waits and asks to wait (not fail).
	fails, waits = 0, 0
	a, wait, _ := decideIteration(1, nil, "Claude AI usage limit reached|1700000000", now, &fails, &waits, &retries)
	if a != actWait || waits != 1 || fails != 0 || wait <= 0 {
		t.Errorf("limit: action=%d wait=%v fails=%d waits=%d, want actWait/>0/0/1", a, wait, fails, waits)
	}

	// The newer human-readable weekly limit is a wait, not a failure.
	fails, waits = 0, 0
	if a, _, _ := decideIteration(1, nil, "You've hit your weekly limit · resets Jun 18, 8pm (UTC)", now, &fails, &waits, &retries); a != actWait || waits != 1 || fails != 0 {
		t.Errorf("weekly limit: action=%d waits=%d fails=%d, want actWait/1/0", a, waits, fails)
	}

	// A non-limit failure bumps fails and asks to retry.
	fails, waits = 0, 0
	if a, _, _ := decideIteration(1, nil, "Error: boom", now, &fails, &waits, &retries); a != actRetry || fails != 1 {
		t.Errorf("failure: action=%d fails=%d, want actRetry/1", a, fails)
	}

	// Consecutive non-limit failures stop at the cap.
	fails, waits = maxLoopFailures-1, 0
	if a, _, _ := decideIteration(1, errors.New("x"), "boom", now, &fails, &waits, &retries); a != actStop {
		t.Errorf("at failure cap: action = %d, want actStop", a)
	}

	// Consecutive rate-limit waits stop at the cap.
	fails, waits = 0, maxLimitWaits
	if a, _, _ := decideIteration(1, nil, "rate limit", now, &fails, &waits, &retries); a != actStop {
		t.Errorf("at limit cap: action = %d, want actStop", a)
	}

	// A SINGLE output limit resumes immediately (the fast path) and leaves fails/waits untouched.
	fails, waits, retries = 2, 3, 0
	if a, wait, _ := decideIteration(1, nil, "Output Limit Reached: maximum output length", now, &fails, &waits, &retries); a != actRetryNow || wait != 0 || fails != 2 || waits != 3 || retries != 1 {
		t.Errorf("output limit: action=%d wait=%v fails=%d waits=%d retries=%d, want actRetryNow/0/2/3/1", a, wait, fails, waits, retries)
	}
}

// TestDecideIterationOutputLimitCapped: the output-limit path used to return actRetryNow forever,
// respawning the box and burning quota with no give-up (introduced by eb36c66). Now a consecutive
// RUN of output limits backs off after the first and stops at the cap, while a single one still
// resumes at once and an intervening different outcome resets the run.
func TestDecideIterationOutputLimitCapped(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	out := "finish_reason: length"
	var fails, waits, retries int

	// First hit: immediate resume. Subsequent consecutive hits: a short backoff, until the cap.
	for i := 1; i <= maxOutputRetries; i++ {
		a, wait, _ := decideIteration(1, nil, out, now, &fails, &waits, &retries)
		if a != actRetryNow {
			t.Fatalf("hit %d: action=%d, want actRetryNow", i, a)
		}
		wantWait := outputRetryBackoff
		if i == 1 {
			wantWait = 0
		}
		if wait != wantWait {
			t.Errorf("hit %d: wait=%v, want %v", i, wait, wantWait)
		}
	}
	// One past the cap: give up instead of resuming forever, and reset the counter.
	if a, _, _ := decideIteration(1, nil, out, now, &fails, &waits, &retries); a != actStop {
		t.Fatalf("past the output-limit cap: action=%d, want actStop", a)
	}
	if retries != 0 {
		t.Errorf("the counter should reset when the cap trips, got %d", retries)
	}

	// A single output limit followed by a NON-output outcome resets the run: a fresh run gets the
	// full budget again, so a lone hit here and there never accumulates toward the cap.
	fails, waits, retries = 0, 0, 0
	decideIteration(1, nil, out, now, &fails, &waits, &retries) // retries -> 1
	decideIteration(1, nil, "Error: boom", now, &fails, &waits, &retries)
	if retries != 0 {
		t.Errorf("a non-output failure must reset the output-limit run, got retries=%d", retries)
	}
	if a, _, _ := decideIteration(1, nil, out, now, &fails, &waits, &retries); a != actRetryNow || retries != 1 {
		t.Errorf("after a reset, a fresh output limit resumes at once: action=%d retries=%d", a, retries)
	}
}

// TestProgressStall: the loop's stall guard resets when a task completes (Done advances) and
// stops only after maxStalls consecutive iterations complete nothing.
func TestProgressStall(t *testing.T) {
	// Done advanced → baseline moves up, stalls reset, never stop.
	if b, s, stop := progressStall(3, 2, 4); b != 3 || s != 0 || stop {
		t.Errorf("progress: got (%d,%d,%v), want (3,0,false)", b, s, stop)
	}
	// No progress → baseline held, stalls increment, no stop below the cap.
	if b, s, stop := progressStall(2, 2, 0); b != 2 || s != 1 || stop {
		t.Errorf("first stall: got (%d,%d,%v), want (2,1,false)", b, s, stop)
	}
	// maxStalls consecutive no-progress iterations → stop.
	if _, s, stop := progressStall(2, 2, maxStalls-1); s != maxStalls || !stop {
		t.Errorf("at cap: got stalls=%d stop=%v, want %d/true", s, stop, maxStalls)
	}
	// A completion resets the counter even at the cap.
	if _, s, stop := progressStall(5, 2, maxStalls-1); s != 0 || stop {
		t.Errorf("recovery: got stalls=%d stop=%v, want 0/false", s, stop)
	}
	// The loop feeds the SETTLED count (done+blocked), so blocking a one-way door (done flat,
	// blocked up → settled changes) is progress and must reset the stall, even at the cap.
	if b, s, stop := progressStall(1, 0, maxStalls-1); b != 1 || s != 0 || stop {
		t.Errorf("a settled change (e.g. a block) must reset the stall: got (%d,%d,%v), want (1,0,false)", b, s, stop)
	}
	// A Done DECREASE (an audit reopened an [x], or a torn read) is movement, not a stall — it
	// re-baselines and resets, so it can't falsely trip the cap on the next iteration.
	if b, s, stop := progressStall(3, 5, maxStalls-1); b != 3 || s != 0 || stop {
		t.Errorf("decrease: got (%d,%d,%v), want (3,0,false)", b, s, stop)
	}
}

func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 10}
	w.Write([]byte("12345"))
	w.Write([]byte("67890ABCDE"))
	if got := w.String(); got != "67890ABCDE" {
		t.Errorf("tail = %q, want last 10 bytes %q", got, "67890ABCDE")
	}

	// Safe under the concurrent stdout/stderr copy goroutines os/exec uses.
	cw := &tailWriter{max: 1 << 12}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				cw.Write([]byte(strings.Repeat("x", 8)))
			}
		}()
	}
	wg.Wait()
	if len(cw.String()) > cw.max {
		t.Errorf("tail grew past max: %d > %d", len(cw.String()), cw.max)
	}
}
