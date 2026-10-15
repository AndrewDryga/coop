package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// cliLimitMarkers are provider CLI phrases that prove a failed command hit a
// rate, usage, quota, or capacity limit. Only the host's CLIRateLimited reads them;
// keep prose that could appear in normal task output out of this list because a
// false positive rotates to a different provider.
var cliLimitMarkers = []string{
	"usage limit", "rate limit", "rate-limit", "rate limited",
	"ratelimited", "overloaded", "at capacity", "resource exhausted",
	"resource_exhausted", "quota exceeded", "quota limit", "exceeded quota",
	"insufficient quota", "usagelimit", "usagelimitexceeded",
}

// wrapperLimitMarkers are deliberately stricter than the host detector above.
// Wrapper output can include task prose, so a generic phrase such as "rate limit"
// is not enough evidence to hand the same task to another provider.
var wrapperLimitMarkers = []string{
	"usage limit reached", "rate limit exceeded", "rate-limit exceeded",
	"you are rate limited", "request was rate limited", "error: rate limited", "ratelimited",
	"server overloaded", "service overloaded", "overloaded_error",
	"selected model is at capacity", "resource exhausted", "resource_exhausted",
	"quota exceeded", "exceeded quota",
	"insufficient quota", "usagelimit", "usagelimitexceeded",
	// The pinned Grok client's structured error payload for a 402, its "run out of credits". Its 429
	// carries no status: the client prints only the server's own text, which the status pattern
	// below reads when it says "too many requests".
	`"http_status": 402`,
}

// These patterns are written once, in the POSIX extended syntax both Go and grep -E accept (no
// \w, \b or (?:...)), so the generated role wrappers grep the same text WrapperRateLimited matches.
// The wrappers run grep under LC_ALL=C: the box's UTF-8 locale would widen [[:alnum:]],
// [[:space:]] and -i to Unicode, which Go's ASCII classes do not match.
const (
	subscriptionLimitPattern = `(hit|reached) your ([[:alnum:]_.-]+ ){0,3}limit`
	wrapperStatus429Pattern  = `http( status)?[[:space:]:=]*429|status["'[:space:]:=]+429|code["'[:space:]:=]+429|too many requests`
)

var (
	cliSubscriptionLimitRe = regexp.MustCompile(`(?i)` + subscriptionLimitPattern)
	cliStatus429Re         = regexp.MustCompile(`\b429\b`)
	wrapperStatus429Re     = regexp.MustCompile(`(?i)` + wrapperStatus429Pattern)
)

// SubscriptionLimitHit reports whether text says the account hit a subscription limit — "You've hit
// your weekly limit", "You've reached your Fable 5 limit". The ladder reads the same sentence for its
// reset time, so it uses this rather than a copy of the pattern.
func SubscriptionLimitHit(text string) bool { return cliSubscriptionLimitRe.MatchString(text) }

// CLIRateLimited reports whether provider CLI output carries a broad rate-limit
// marker. Callers must also require a non-zero exit status; successful task output
// is untrusted prose and must never trigger rotation by itself.
func CLIRateLimited(output string) bool {
	if cliSubscriptionLimitRe.MatchString(output) || cliStatus429Re.MatchString(output) {
		return true
	}
	lower := strings.ToLower(output)
	for _, marker := range cliLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// WrapperRateLimited applies the stricter detector used after a failed role command.
func WrapperRateLimited(output string) bool {
	if cliSubscriptionLimitRe.MatchString(output) || wrapperStatus429Re.MatchString(output) {
		return true
	}
	lower := strings.ToLower(output)
	for _, marker := range wrapperLimitMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ShellRateLimitDetector renders the POSIX-shell function used by generated role
// wrappers. Its marker rows come from wrapperLimitMarkers and its grep -E from the
// patterns WrapperRateLimited compiles, so the in-box check matches the Go one.
func ShellRateLimitDetector() string {
	var b strings.Builder
	b.WriteString(`coop_rate_limited() {
	limit_file=$1
	while IFS= read -r marker; do
		grep -Fqi "$marker" "$limit_file" && return 0
	done <<'COOP_LIMIT_MARKERS'
`)
	for _, marker := range wrapperLimitMarkers {
		b.WriteString(marker + "\n")
	}
	// A single-quoted shell word cannot hold a quote: close it, add an escaped one, reopen it.
	pattern := strings.ReplaceAll(subscriptionLimitPattern+"|"+wrapperStatus429Pattern, "'", `'\''`)
	fmt.Fprintf(&b, "COOP_LIMIT_MARKERS\n\tLC_ALL=C grep -Eiq '%s' \"$limit_file\"\n}\n", pattern)
	return b.String()
}
