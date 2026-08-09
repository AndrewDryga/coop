package ladder

import (
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
		// (which would make LimitWait clamp to the 10s minimum — a busy retry against a real limit).
		{"absurd retry-after hours saturates",
			"Please retry after 9999999 hours.", true, false, now.Add(LimitMaxWait)},
		{"broad rate-limit keyword, no reset",
			"request failed: rate limit exceeded", true, false, time.Time{}},
		{"codex model-at-capacity notice",
			"Selected model is at capacity. Please try a different model.", true, false, time.Time{}},
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
			got := DetectLimit(c.output, now)
			if got.Limited != c.wantLimited {
				t.Fatalf("Limited = %v, want %v", got.Limited, c.wantLimited)
			}
			if got.OutputLimited != c.wantOutputLimited {
				t.Fatalf("OutputLimited = %v, want %v", got.OutputLimited, c.wantOutputLimited)
			}
			if c.wantReset.IsZero() {
				if !got.ResetAt.IsZero() {
					t.Errorf("ResetAt = %v, want zero", got.ResetAt)
				}
			} else if !got.ResetAt.Equal(c.wantReset) {
				t.Errorf("ResetAt = %v, want %v", got.ResetAt, c.wantReset)
			}
		})
	}
}

func TestDetectIterationLimitRejectsNarration(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	for _, output := range []string{
		"fixture task text discusses rate limit handling but this is an ordinary failure",
		"fixture task says error: rate limited while reviewing an ordinary failure",
		"review output limit handling before retrying the test",
		"review authentication required handling before retrying the test",
		`{"type":not-json,"message":"rate limit handling"}`,
	} {
		if got := DetectIterationLimit(output, now); got.Limited {
			t.Errorf("DetectIterationLimit(%q) = %+v, want ordinary failure", output, got)
		}
	}
	for _, output := range []string{
		"usage limit reached\nretry-after: 3600",
		"HTTP 429 Too Many Requests",
		"Output Limit Reached: maximum output length",
	} {
		if got := DetectIterationLimit(output, now); !got.Limited {
			t.Errorf("DetectIterationLimit(%q) = %+v, want limit", output, got)
		}
	}
}

// LimitNotice is the stream decoder's narrower dedupe question: the notice prose itself, not
// every output a limit could be inferred from.
func TestLimitNotice(t *testing.T) {
	for _, s := range []string{
		"You've hit your weekly limit · resets Jun 18, 8pm (UTC)",
		"Claude AI usage limit reached|1700000000",
	} {
		if !LimitNotice(s) {
			t.Errorf("LimitNotice(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"HTTP 429 Too Many Requests", "flipped [x], committed abc123, done"} {
		if LimitNotice(s) {
			t.Errorf("LimitNotice(%q) = true, want false — only the notice prose dedupes", s)
		}
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
			got := ParseResetTime(c.in, c.now)
			if c.want.IsZero() {
				if !got.IsZero() {
					t.Errorf("ParseResetTime = %v, want zero", got)
				}
			} else if !got.Equal(c.want) {
				t.Errorf("ParseResetTime = %v, want %v", got, c.want)
			}
		})
	}
}

func TestLimitWait(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)
	cases := []struct {
		name    string
		hint    LimitHint
		attempt int
		want    time.Duration
	}{
		{"known reset waits until then plus buffer",
			LimitHint{Limited: true, ResetAt: now.Add(10 * time.Minute)}, 1, 10*time.Minute + limitBuffer},
		{"past reset clamps to the minimum",
			LimitHint{Limited: true, ResetAt: now.Add(-time.Hour)}, 1, limitMinWait},
		{"a multi-hour reset is honored, not clamped to an hour",
			LimitHint{Limited: true, ResetAt: now.Add(3 * time.Hour)}, 1, 3*time.Hour + limitBuffer},
		{"a multi-day weekly reset is honored",
			LimitHint{Limited: true, ResetAt: now.Add(48 * time.Hour)}, 1, 48*time.Hour + limitBuffer},
		{"an absurd far-future reset clamps to the ceiling",
			LimitHint{Limited: true, ResetAt: now.Add(30 * 24 * time.Hour)}, 1, LimitMaxWait},
		{"unknown reset backs off: attempt 1 → 1m", LimitHint{Limited: true}, 1, time.Minute},
		{"unknown reset backs off: attempt 3 → 4m", LimitHint{Limited: true}, 3, 4 * time.Minute},
		{"unknown reset backs off: capped at 32m", LimitHint{Limited: true}, 99, 32 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LimitWait(c.hint, c.attempt, now); got != c.want {
				t.Errorf("LimitWait = %v, want %v", got, c.want)
			}
		})
	}
}
