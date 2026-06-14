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
		name        string
		output      string
		wantLimited bool
		wantReset   time.Time // zero = expect unknown
	}{
		{"claude usage limit with epoch",
			"…working…\nClaude AI usage limit reached|1700000000\n", true, time.Unix(1700000000, 0)},
		{"usage limit with millisecond epoch",
			"Claude AI usage limit reached|1700000000000", true, time.Unix(1700000000, 0)},
		{"retry-after seconds",
			"Error: rate limited. Please retry after 45s.", true, now.Add(45 * time.Second)},
		{"try again in N seconds",
			"overloaded; try again in 30 seconds", true, now.Add(30 * time.Second)},
		{"broad rate-limit keyword, no reset",
			"request failed: rate limit exceeded", true, time.Time{}},
		{"http 429, no reset",
			"HTTP 429 Too Many Requests", true, time.Time{}},
		{"normal success output",
			"flipped [x], committed abc123, done", false, time.Time{}},
		{"unrelated failure",
			"Error: file not found: foo.go", false, time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := detectLimit(c.output, now)
			if got.limited != c.wantLimited {
				t.Fatalf("limited = %v, want %v", got.limited, c.wantLimited)
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
		{"far-future reset clamps to the maximum",
			limitHint{limited: true, resetAt: now.Add(3 * time.Hour)}, 1, limitMaxWait},
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
	fails, waits := 3, 2
	if a, _, _ := decideIteration(0, nil, "done", now, &fails, &waits); a != actContinue {
		t.Errorf("success: action = %d, want actContinue", a)
	}
	if fails != 0 || waits != 0 {
		t.Errorf("success should reset counters, got fails=%d waits=%d", fails, waits)
	}

	// A rate limit bumps only waits and asks to wait (not fail).
	fails, waits = 0, 0
	a, wait, _ := decideIteration(1, nil, "Claude AI usage limit reached|1700000000", now, &fails, &waits)
	if a != actWait || waits != 1 || fails != 0 || wait <= 0 {
		t.Errorf("limit: action=%d wait=%v fails=%d waits=%d, want actWait/>0/0/1", a, wait, fails, waits)
	}

	// A non-limit failure bumps fails and asks to retry.
	fails, waits = 0, 0
	if a, _, _ := decideIteration(1, nil, "Error: boom", now, &fails, &waits); a != actRetry || fails != 1 {
		t.Errorf("failure: action=%d fails=%d, want actRetry/1", a, fails)
	}

	// Consecutive non-limit failures stop at the cap.
	fails, waits = maxLoopFailures-1, 0
	if a, _, _ := decideIteration(1, errors.New("x"), "boom", now, &fails, &waits); a != actStop {
		t.Errorf("at failure cap: action = %d, want actStop", a)
	}

	// Consecutive rate-limit waits stop at the cap.
	fails, waits = 0, maxLimitWaits
	if a, _, _ := decideIteration(1, nil, "rate limit", now, &fails, &waits); a != actStop {
		t.Errorf("at limit cap: action = %d, want actStop", a)
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
