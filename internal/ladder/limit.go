package ladder

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
)

// LimitHint is what an iteration's output told us about a model rate/usage limit or output/token exhaustion.
type LimitHint struct {
	Limited       bool      // the model is rate- or usage-limited
	OutputLimited bool      // the model reached its maximum output length limit
	ResetAt       time.Time // when it resets (zero = unknown)
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
	// iterationOutputLimitRe excludes generic prose such as "review output limit handling". The
	// loop sees assistant narration as well as terminal diagnostics, so only explicit exhaustion
	// forms may trigger a same-target resume.
	iterationOutputLimitRe = regexp.MustCompile(`(?i)^(?:error:\s*)?(?:output limit reached(?:[: ].*)?|max(?:imum)? output length(?: reached)?|(?:max(?:imum)?[_ -]?output[_ -]?tokens?)(?:["'\s:=]+(?:reached|exceeded|true))|.*finish[_ ]?reason["'\s:=]+(?:length|max[_ -]?tokens?).*|.*finishreason["'\s:=]+max_tokens.*)[.!]?$`)
)

// DetectLimit inspects an iteration's captured output for a model rate/usage
// limit and, when present, when it resets. `now` anchors relative hints like
// "retry after N". Precise signals (the usage-limit epoch, an explicit retry
// delay) win over the broad keyword fallback.
func DetectLimit(output string, now time.Time) LimitHint {
	if m := usageLimitRe.FindStringSubmatch(output); m != nil {
		if epoch, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if epoch > 1e12 { // tolerate a millisecond epoch
				epoch /= 1000
			}
			return LimitHint{Limited: true, ResetAt: time.Unix(epoch, 0)}
		}
		return LimitHint{Limited: true}
	}
	if outputLimitRe.MatchString(output) {
		return LimitHint{Limited: true, OutputLimited: true}
	}
	// "You've hit your weekly limit · resets Jun 18, 8pm (UTC)" — parse the stated
	// reset so the loop sleeps until then rather than backing off into the wall.
	if hitLimitRe.MatchString(output) {
		return LimitHint{Limited: true, ResetAt: ParseResetTime(output, now)}
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
				if dur < 0 { // overflow on an absurd count (millions of hours) — saturate; LimitWait caps it
					dur = LimitMaxWait
				}
				return LimitHint{Limited: true, ResetAt: now.Add(dur)}
			}
		}
	}
	if agents.CLIRateLimited(lower) {
		return LimitHint{Limited: true}
	}
	return LimitHint{}
}

// DetectIterationLimit is the stricter failed-process boundary. Structured provider output can
// contain ordinary assistant prose, so broad keywords are insufficient evidence for rotation.
func DetectIterationLimit(output string, now time.Time) LimitHint {
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if iterationOutputLimitRe.MatchString(line) {
			return LimitHint{Limited: true, OutputLimited: true}
		}
		lower := strings.ToLower(line)
		proved := usageLimitRe.MatchString(line) ||
			(hitLimitRe.MatchString(line) && (strings.HasPrefix(lower, "you've hit ") || strings.HasPrefix(lower, "you've reached "))) ||
			(agents.WrapperRateLimited(line) && iterationDiagnosticPrefix(lower))
		if proved {
			hint := DetectLimit(output, now)
			hint.Limited = true
			hint.OutputLimited = false
			return hint
		}
	}
	return LimitHint{}
}

func iterationDiagnosticPrefix(line string) bool {
	for _, prefix := range []string{
		"error:", "fatal:", "http ", "http:", "{", "[", "usage limit", "rate limit",
		"rate-limit", "you are rate limited", "request was rate limited", "server overloaded",
		"service overloaded", "selected model is at capacity", "resource exhausted",
		"resource_exhausted", "quota exceeded", "exceeded quota", "insufficient quota",
		"usagelimit", "ratelimited", "too many requests",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// LimitNotice reports whether a piece of provider prose IS the subscription-limit notice — the
// narrower question a stream decoder asks to avoid printing the same notice twice once it has
// already rendered the limit badge. DetectLimit answers the broader "was this limited".
func LimitNotice(s string) bool {
	return hitLimitRe.MatchString(s) || strings.Contains(strings.ToLower(s), "usage limit reached")
}

// ParseResetTime reads the "resets <when>" clause of a subscription-limit notice
// — "resets Jun 18, 8pm (UTC)" or a bare "resets 11am" — into an absolute time.
// `now` supplies the missing year (and the date, for a time-only reset). A zero
// return means "not stated / unrecognized" — the caller then backs off instead.
func ParseResetTime(output string, now time.Time) time.Time {
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
	LimitMaxWait = 8 * 24 * time.Hour // spans the longest window (weekly), still bounds a bad parse
)

// LimitWait computes how long to pause before retrying after a rate limit. With
// a known reset it waits until then (plus a small buffer); otherwise it backs
// off exponentially by attempt (1m, 2m, 4m … capped). The result is clamped to
// [limitMinWait, LimitMaxWait].
func LimitWait(hint LimitHint, attempt int, now time.Time) time.Duration {
	var d time.Duration
	if !hint.ResetAt.IsZero() {
		d = hint.ResetAt.Sub(now) + limitBuffer
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
	if d > LimitMaxWait {
		d = LimitMaxWait
	}
	return d
}
