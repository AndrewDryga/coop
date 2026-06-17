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

// secretAssignRe matches an assignment whose KEY name looks secret-bearing, capturing
// the key (1) and a long contiguous value (2) for an entropy check. The key-name gate
// keeps random-looking-but-innocent tokens (hashes, IDs, base64 blobs) from flagging
// unless they're actually assigned to something secret-shaped.
var secretAssignRe = regexp.MustCompile(`(?i)(\w*(?:secret|token|password|passwd|api[_-]?key|access[_-]?key|auth|credential)\w*)\s*[:=]\s*["']?([^\s"']{20,})`)

// entropyThreshold flags a value as likely-random above this many bits/char (Shannon).
// Real base64/hex tokens sit ~3.5–6; English/placeholder text sits lower.
const entropyThreshold = 3.5

// codeRefRe matches a dotted identifier path with a short leading segment — var.x,
// data.y.z, process.env.API_KEY, config.databricks_api_key — i.e. a code reference, not a
// literal secret. The ≤16-char first segment keeps it from matching dotted base64 blobs
// like JWTs, whose segments are long.
var codeRefRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$-]{0,15}(\.[A-Za-z_$][A-Za-z0-9_$-]*)+$`)

// looksLikeCodeRef reports whether a value is a code expression — a variable/config
// reference, a template interpolation, or a function call — rather than a literal secret.
// It keeps the entropy heuristic from flagging innocent code, e.g. the common
// `databricks_api_key = var.blitz_databricks_api_key`.
func looksLikeCodeRef(v string) bool {
	switch {
	case strings.Contains(v, "${"), strings.Contains(v, "{{"):
		return true // interpolation: ${var.x}, {{ .Secret }}
	case strings.IndexByte(v, '(') >= 0 && strings.IndexByte(v, ')') >= 0:
		return true // a call: generateKey(), os.getenv("X")
	case codeRefRe.MatchString(v):
		return true // a dotted reference: var.x, process.env.API_KEY, config.token
	default:
		return false
	}
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
			if p.re.MatchString(line) {
				out = append(out, SecretFinding{n, p.kind})
				matched = true
			}
		}
		// Don't double-report a line a pattern already flagged.
		if !matched {
			if m := secretAssignRe.FindStringSubmatch(line); m != nil && !looksLikeCodeRef(m[2]) && shannonEntropy(m[2]) >= entropyThreshold {
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
