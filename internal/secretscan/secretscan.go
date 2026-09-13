package secretscan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// SecretFinding is one likely secret found in a file's content.
type SecretFinding struct {
	Line        int    // 1-based line number
	Kind        string // what matched, e.g. "OpenAI API key"
	Detector    string // the stable detector id this finding's identity is built on
	Key         string // for an assignment finding: the key name, never its value
	Fingerprint string // fp-v1:<sha256>, or "" when the scan was given no path to bind it to
}

// Detector ids are the stable half of a finding's identity: a .coopsecretsignore entry written
// today has to keep matching the same finding after a label is reworded or a pattern is tightened,
// so these strings are a compatibility surface. Rename one and every saved exception for it
// silently stops applying. Add ids; never repurpose them.
const (
	DetectorPrivateKey        = "private_key"
	DetectorAWSAccessKeyID    = "aws_access_key_id"
	DetectorAnthropicAPIKey   = "anthropic_api_key"
	DetectorOpenAIAPIKey      = "openai_api_key"
	DetectorGitHubToken       = "github_token"
	DetectorGitHubPAT         = "github_fine_grained_token"
	DetectorSlackToken        = "slack_token"
	DetectorGoogleAPIKey      = "google_api_key"
	DetectorStripeKey         = "stripe_key"
	DetectorJWT               = "jwt"
	DetectorURLPassword       = "url_password"
	DetectorAssignedHighEntro = "assigned_high_entropy"
)

// secretPatterns are high-signal provider token shapes — precise enough to flag with
// low false positives. Filename-based shadowing (SecretGlobs / .coopignore) catches
// secret-*looking paths*; this catches a real token sitting in an ordinary file.
var secretPatterns = []struct {
	detector string
	kind     string
	label    string
	re       *regexp.Regexp
}{
	{DetectorPrivateKey, "private key", "Private key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{DetectorAWSAccessKeyID, "AWS access key id", "AWS access key ID", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{DetectorAnthropicAPIKey, "Anthropic API key", "Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{DetectorOpenAIAPIKey, "OpenAI API key", "OpenAI API key", regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}`)},
	{DetectorGitHubToken, "GitHub token", "GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{DetectorGitHubPAT, "GitHub fine-grained token", "GitHub fine-grained token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}`)},
	{DetectorSlackToken, "Slack token", "Slack token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`)},
	{DetectorGoogleAPIKey, "Google API key", "Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{DetectorStripeKey, "Stripe key", "Stripe key", regexp.MustCompile(`\b[sr]k_live_[0-9a-zA-Z]{24,}\b`)},
	// A 3-part JWT: two dot-joined base64url segments each starting with eyJ ({" encoded)
	// plus a signature. Matched precisely here because the entropy path can never catch one —
	// a JWT's dotted shape parses as a code reference (codeRefRe) and gets skipped.
	{DetectorJWT, "JWT", "JWT", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*`)},
}

// detectorLabels is what a person reads for each detector. The label is presentation and may be
// reworded; the id above may not.
var detectorLabels = map[string]string{
	DetectorURLPassword: "Password in a connection URL",
}

func init() {
	for _, p := range secretPatterns {
		detectorLabels[p.detector] = p.label
	}
}

// Label is the finding's human name. An assignment names the KEY it was found under and never
// the value: the whole point of the report is that the value stays where it is.
func (f SecretFinding) Label() string {
	if f.Detector == DetectorAssignedHighEntro {
		return "Possible secret assigned to " + f.Key
	}
	if label, ok := detectorLabels[f.Detector]; ok {
		return label
	}
	return f.Kind
}

// secretAssignRe matches an assignment whose KEY name ENDS in a credential word —
// password, secret, token, api_key, access_key, client_secret, auth_token, master_key,
// encryption_key, secret_key_base, credentials (with an optional encoding suffix like
// _b64/_value) — capturing the key (1) and a long value (2) for an entropy check. Anchoring
// the word at the END of the key (not anywhere in it) is what keeps config keys that merely
// contain "auth"/"token" from matching — authenticator, auth_proxy_headers, allocate_tokens,
// token_url — a big FP source. Specific *_key names (master/encryption + Rails secret_key_base)
// are listed explicitly rather than a bare "_key", which would flag public_key/primary_key/etc.
var secretAssignRe = regexp.MustCompile(`(?i)([\w-]*(?:password|passwd|secret[_-]?key[_-]?base|secret[_-]?key|master[_-]?key|encryption[_-]?key|access[_-]?key|api[_-]?key|private[_-]?key|client[_-]?secret|auth[_-]?token|authorization|secret|token|credentials?)(?:[_-](?:b64|base64|encoded|value|json|pem))?)\s*[:=]\s*["']?([^\s"']{20,})`)

// entropyThreshold flags a value as likely-random above this many bits/char (Shannon).
// Real base64/hex tokens sit ~3.5–6; English/placeholder text sits lower.
const entropyThreshold = 3.5

// maxEntropyLineSlack caps how much of a line may sit OUTSIDE the matched assignment before
// the entropy heuristic trusts it. Minified/generated output puts a whole program on one line,
// so a high-entropy `token:"…"` there is a build artifact drowning in kilobytes of surrounding
// code — while a hand-written line is essentially just the assignment. Keying on the slack, not
// the line length, means a multi-KB base64 credential blob (its line is all value) still fires;
// a secret pasted next to minified code sits on its own short line, so it fires too; and the
// precise provider patterns scan every line regardless.
const maxEntropyLineSlack = 2048

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
	case looksLikeNamedIdent(v):
		return true // a named thing, not a token: secrets/app/github-token, my-runner-enrollment-key
	default:
		return false
	}
}

// looksLikeNamedIdent reports whether a value is a human-authored NAME — a secret-manager path
// (secrets/desktop-release-manager/github-token), a resource id (emisar-gcp-runner-tfe-token) —
// rather than a literal credential. bareIdentRe covers only snake_case with no slashes, so these
// two extremely common Terraform shapes were flagged as secrets; naming where a secret LIVES is
// not leaking it.
//
// Two conditions, both required, chosen so they cannot hide a real credential:
//
//   - every segment is all-lowercase alphanumeric, separated by - _ . or / — a base64 token is
//     mixed-case (an all-lowercase base64 run of 20+ chars is ~1e-8 likely), so this excludes it.
//   - at least one letter is OUTSIDE the hex alphabet. This is what keeps a UUID firing: a UUID
//     is lowercase and dash-separated and would otherwise match, but it is pure [0-9a-f-], and
//     real credentials ARE UUIDs (see TestScanSecretsUUIDValueStillFlagged). A hex digest has no
//     separators and no non-hex letter either way.
func looksLikeNamedIdent(v string) bool {
	if !namedIdentRe.MatchString(v) {
		return false
	}
	return strings.ContainsAny(v, "ghijklmnopqrstuvwxyz")
}

// namedIdentRe matches lowercase alphanumeric segments joined by - _ . or /, with at least one
// separator (a single bare word is left to the entropy check).
var namedIdentRe = regexp.MustCompile(`^[a-z0-9]+([._\-/][a-z0-9]+)+$`)

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
var placeholderRe = regexp.MustCompile(`(?i)(example|placeholder|redacted|change[-_]?me|replace[-_]?(me)?\b|x{6,})|\b(password|passwd|secret|fake|dummy|sample)\b|your[-_]`)

// commentRe matches a line whose content is a comment (#, ;, //). Comments hold examples
// and placeholders, not live secrets — so the fuzzy entropy heuristic skips them. (The
// precise provider patterns still scan every line, comments included.)
var commentRe = regexp.MustCompile(`^\s*(#|;|//)`)

// urlCredRe matches a connection string with an inline password — scheme://user:PASSWORD@host
// (postgres/redis/amqp/mongodb…). The password leaks regardless of the key name (DATABASE_URL=,
// REDIS=, a bare URL), so this is checked directly, not via the secret-named-key entropy path
// (which would skip it: the key isn't a credential word, and looksLikeURLOrPath bails on "://").
var urlCredRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:([^/\s:@]{6,})@`)

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
// The findings carry no fingerprint — identity needs the file's path, which ScanFile supplies.
func ScanSecrets(content string) []SecretFinding { return ScanFile("", content) }

// ScanFile is ScanSecrets for a file whose findings must be nameable across runs: each finding
// also carries the stable fingerprint a .coopsecretsignore entry refers to. path is the
// repository-relative, slash-separated path — nothing machine-specific goes into an id, so the
// same file in a second checkout produces the same ids and one exception file travels with the
// project. The matched credential text is hashed and dropped; it is never stored on a finding,
// so no caller can print or serialize it by accident.
func ScanFile(path, content string) []SecretFinding {
	var out []SecretFinding
	seen := map[string]bool{}
	add := func(f SecretFinding, material string) {
		if path != "" {
			f.Fingerprint = Fingerprint(path, f.Detector, material)
		}
		// One line repeating one exact value under one detector is one finding: a second copy
		// would print the same row twice and be covered by the same exception anyway.
		identity := f.Fingerprint
		if identity == "" {
			identity = material
		}
		key := fmt.Sprintf("%d\x00%s\x00%s", f.Line, f.Detector, identity)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, f)
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		n := i + 1
		precise := map[string]bool{}
		for _, p := range secretPatterns {
			// EVERY match on the line, not just the first: two tokens side by side are two
			// independent findings, and ignoring one must never hide the other.
			for _, tok := range p.re.FindAllString(line, -1) {
				// Skip a token that is an obvious example/placeholder (AKIA…EXAMPLE) — a real
				// provider token never contains "example"/"secret"/etc.
				if placeholderRe.MatchString(tok) {
					continue
				}
				material := tok
				if p.detector == DetectorPrivateKey {
					material = privateKeyBlock(lines, i, content)
				}
				add(SecretFinding{Line: n, Kind: p.kind, Detector: p.detector}, material)
				precise[tok] = true
			}
		}
		// A password embedded in a connection-string URL (postgres://user:pw@host) — flagged when
		// the password looks real (long enough, not a placeholder or a ${VAR}/code reference).
		for _, m := range urlCredRe.FindAllStringSubmatch(line, -1) {
			if !precise[m[1]] && !placeholderRe.MatchString(m[1]) && !looksLikeCodeRef(m[1]) {
				add(SecretFinding{Line: n, Kind: "password in a connection-string URL", Detector: DetectorURLPassword}, m[1])
			}
		}
		// The fuzzy entropy heuristic only fires on a plausible literal credential: not the
		// same value a precise pattern already flagged, not a comment, not a match drowning
		// in a minified/generated line, and not a code reference, URL, or filesystem path.
		if !commentRe.MatchString(line) {
			if m := secretAssignRe.FindStringSubmatch(line); m != nil &&
				len(line)-len(m[0]) <= maxEntropyLineSlack &&
				!precise[m[2]] && !looksLikeCodeRef(m[2]) && !looksLikeURLOrPath(m[2]) && !placeholderRe.MatchString(m[2]) &&
				shannonEntropy(m[2]) >= entropyThreshold {
				add(SecretFinding{Line: n, Kind: "high-entropy value assigned to '" + m[1] + "'",
					Detector: DetectorAssignedHighEntro, Key: m[1]}, m[2])
			}
		}
	}
	return out
}

// privateKeyBlock is the material a private-key finding is identified by: the whole armoured
// block, BEGIN through END. Every key in a file shares the BEGIN line, so hashing the marker
// would give two different keys one id and let an exception for the first hide the second.
// An unterminated block falls back to the whole file — conservative in the safe direction: the
// id then changes whenever anything else in the file changes, which re-reports rather than hides.
func privateKeyBlock(lines []string, start int, content string) string {
	for j := start; j < len(lines); j++ {
		if strings.Contains(lines[j], "-----END") && strings.Contains(lines[j], "PRIVATE KEY-----") {
			return strings.Join(lines[start:j+1], "\n")
		}
	}
	return content
}

// FingerprintVersion prefixes every id, so a future change to what identity covers can be
// introduced without silently re-interpreting the entries already written by hand.
const FingerprintVersion = "fp-v1"

// Fingerprint is a finding's portable identity: the repository-relative path, the stable detector
// id, and the complete matched material, hashed together under a version tag. The line number is
// deliberately absent — inserting a line above a finding must not invalidate the exception for it
// — and so is anything about this machine, so the file can be committed and shared.
//
// It is a checksum, not encryption: it identifies a finding, it does NOT make the credential it
// was computed from safe to publish. Lengths are encoded before each part so no combination of
// path and material can be read two ways.
func Fingerprint(path, detector, material string) string {
	h := sha256.New()
	for _, part := range []string{FingerprintVersion, path, detector, material} {
		fmt.Fprintf(h, "%d\n", len(part))
		h.Write([]byte(part))
	}
	return FingerprintVersion + ":" + hex.EncodeToString(h.Sum(nil))
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
