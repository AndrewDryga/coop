package agent

import (
	"regexp"
	"strings"
)

// cliLimitMarkers are provider CLI phrases that prove a failed command hit a
// rate, usage, quota, or capacity limit. They are shared by the host loop and the
// generated in-box role wrappers; keep prose that could appear in normal task output
// out of this list because a false positive rotates to a different provider.
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
}

var (
	cliSubscriptionLimitRe = regexp.MustCompile(`(?i)(?:hit|reached) your (?:[\w.-]+ ){0,3}limit`)
	cliStatus429Re         = regexp.MustCompile(`\b429\b`)
	wrapperStatus429Re     = regexp.MustCompile(`(?i)(?:http(?: status)?[[:space:]:=]*429|status["'[:space:]:=]+429|code["'[:space:]:=]+429|too many requests)`)
)

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
// wrappers. The literal marker rows come from cliLimitMarkers, so adding a provider
// phrase updates host and in-box classification together.
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
	b.WriteString(`COOP_LIMIT_MARKERS
	grep -Eiq '(hit|reached) your ([[:alnum:]_.-]+ ){0,3}limit|HTTP( status)?[[:space:]:=]*429|status["[:space:]:=]+429|code["[:space:]:=]+429|too many requests' "$limit_file"
}
`)
	return b.String()
}
