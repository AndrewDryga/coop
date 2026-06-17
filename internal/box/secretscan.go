package box

import (
	"math"
	"regexp"
	"strings"
)

// SecretFinding is one likely secret found in a file's content.
type SecretFinding struct {
	Line int    // 1-based line number
	Kind string // what matched, e.g. "OpenAI API key"
}

// secretPatterns are high-signal provider token shapes — precise enough to flag with
// low false positives. Filename-based shadowing (SecretGlobs / .coopignore) catches
// secret-*looking paths*; this catches a real token sitting in an ordinary file.
var secretPatterns = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{"AWS access key id", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"OpenAI API key", regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"Stripe key", regexp.MustCompile(`\b[sr]k_live_[0-9a-zA-Z]{24,}\b`)},
}

// secretAssignRe matches an assignment whose KEY name ENDS in a credential word —
// password, secret, token, api_key, access_key, client_secret, auth_token, credentials
// (with an optional encoding suffix like _b64/_value) — capturing the key (1) and a long
// value (2) for an entropy check. Anchoring the word at the END of the key (not anywhere
// in it) is what keeps config keys that merely contain "auth"/"token" from matching —
// authenticator, auth_proxy_headers, allocate_tokens, token_url — a big FP source.
var secretAssignRe = regexp.MustCompile(`(?i)([\w-]*(?:password|passwd|secret[_-]?key|access[_-]?key|api[_-]?key|private[_-]?key|client[_-]?secret|auth[_-]?token|authorization|secret|token|credentials?)(?:[_-](?:b64|base64|encoded|value|json|pem))?)\s*[:=]\s*["']?([^\s"']{20,})`)

// entropyThreshold flags a value as likely-random above this many bits/char (Shannon).
// Real base64/hex tokens sit ~3.5–6; English/placeholder text sits lower.
const entropyThreshold = 3.5

// codeRefRe matches a dotted identifier path — var.x, data.y.z, process.env.API_KEY,
// google_storage_hmac_key.s3.access_id — i.e. a code reference, not a literal secret.
var codeRefRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$-]*(\.[A-Za-z_$][A-Za-z0-9_$-]*)+$`)

// looksLikeCodeRef reports whether a value is a code expression — a variable/config
// reference, an interpolation, a shell substitution, a call, or an array — rather than a
// literal secret, so the entropy heuristic doesn't flag innocent code like
// `api_key = var.databricks_api_key` or `token = $(get-token …)`. (The value is captured
// up to the first space/quote, so a call may arrive without its closing paren.)
func looksLikeCodeRef(v string) bool {
	v = strings.TrimRight(v, ",;)]}|") // drop trailing code punctuation: x, x; f(x), #{x}, [x], ~S|x|
	switch {
	case strings.ContainsAny(v, "$([{<@"):
		return true // var/call/index/interp/generic/annotation: $X, ${x}, $(cmd, f(x, [ref], {x}, #{x}, T<U>, @attr
	case strings.Contains(v, "::"):
		return true // a namespace path: snownet::Credentials, Foo::Bar (Rust/Ruby/C++/PHP)
	case strings.Contains(v, "{{"):
		return true // a Go/Helm template: {{ .Secret }}
	case codeRefRe.MatchString(v):
		return true // a dotted reference: var.x, google_storage_hmac_key.s3.access_id
	case bareIdentRe.MatchString(v):
		return true // a bare snake_case variable: gateway_group_token, braintree_private_key
	default:
		return false
	}
}

// bareIdentRe matches an all-lowercase snake_case variable (gateway_group_token) or an
// all-uppercase SCREAMING_SNAKE constant (PUBLIC_ACCESS_TOKEN) — a reference, not a literal
// secret. It deliberately does NOT match mixed-case joined words, so a real token that uses
// underscores as separators (sk_test_b3iJGZ3i…) still gets flagged.
var bareIdentRe = regexp.MustCompile(`^([a-z][a-z0-9]*(_[a-z0-9]+)+|[A-Z][A-Z0-9]*(_[A-Z0-9]+)+)$`)

// placeholderRe matches a value that isn't a live credential: a placeholder/redaction
// marker (your-…, changeme, placeholder, redacted, xxxxxx) or example/fixture vocabulary a
// random token never contains — including the credential words themselves, since a real
// secret value isn't the literal word "password"/"secret". Applied to both the entropy
// value and the matched provider token, so AKIA…EXAMPLE, password = "very-long-password-1"
// and payment_method_token = "fake-payment-method-token" are all skipped.
var placeholderRe = regexp.MustCompile(`(?i)(example|placeholder|redacted|change[-_]?me|x{6,})|\b(password|passwd|secret|fake|dummy|sample)\b|your[-_]`)

// commentRe matches a line whose content is a comment (#, ;, //). Comments hold examples
// and placeholders, not live secrets — so the fuzzy entropy heuristic skips them. (The
// precise provider patterns still scan every line, comments included.)
var commentRe = regexp.MustCompile(`^\s*(#|;|//)`)

// looksLikeURLOrPath reports whether a value is a URL or filesystem path rather than a
// literal secret — e.g. token_url = https://…/oauth, or CREDENTIALS = /secrets/x.json.
func looksLikeURLOrPath(v string) bool {
	if strings.Contains(v, "://") {
		return true
	}
	return strings.HasPrefix(v, "/") || strings.HasPrefix(v, "./") ||
		strings.HasPrefix(v, "../") || strings.HasPrefix(v, "~/")
}

// ScanSecrets reports likely secrets in content: the provider patterns on every line,
// plus a conservative entropy check (a long, high-entropy value assigned to a
// secret-named key). It is pure; callers skip binary/oversized blobs before calling.
func ScanSecrets(content string) []SecretFinding {
	var out []SecretFinding
	for i, line := range strings.Split(content, "\n") {
		n := i + 1
		matched := false
		for _, p := range secretPatterns {
			// Skip a token that is an obvious example/placeholder (AKIA…EXAMPLE) — a real
			// provider token never contains "example"/"secret"/etc.
			if tok := p.re.FindString(line); tok != "" && !placeholderRe.MatchString(tok) {
				out = append(out, SecretFinding{n, p.kind})
				matched = true
			}
		}
		// The fuzzy entropy heuristic only fires on a plausible literal credential: not a
		// line a pattern already flagged, not a comment, and a value that isn't a code
		// reference, URL, or filesystem path.
		if !matched && !commentRe.MatchString(line) {
			if m := secretAssignRe.FindStringSubmatch(line); m != nil &&
				!looksLikeCodeRef(m[2]) && !looksLikeURLOrPath(m[2]) && !placeholderRe.MatchString(m[2]) &&
				shannonEntropy(m[2]) >= entropyThreshold {
				out = append(out, SecretFinding{n, "high-entropy value assigned to '" + m[1] + "'"})
			}
		}
	}
	return out
}

// shannonEntropy returns the per-character Shannon entropy (bits) of s.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]float64
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var e float64
	for _, c := range freq {
		if c > 0 {
			p := c / n
			e -= p * math.Log2(p)
		}
	}
	return e
}
