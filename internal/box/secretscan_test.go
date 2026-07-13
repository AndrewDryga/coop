package box

import (
	"strings"
	"testing"
)

func TestScanSecretsPatterns(t *testing.T) {
	hits := map[string]string{
		"Anthropic API key":         `client = "sk-ant-api03-abcDEF1234567890abcDEF12"`,
		"OpenAI API key":            `OPENAI_KEY=sk-proj-abcDEF1234567890abcDEF1234`,
		"AWS access key id":         `aws_key: AKIA1234567890ABCDEF`,
		"GitHub token":              `token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"`,
		"GitHub fine-grained token": `pat = github_pat_11ABCDEFGHIJKLMNOPQRST_0aBcDeFgHiJkLmNoPqRsTuVwXyZ012345`,
		"private key":               "-----BEGIN OPENSSH PRIVATE KEY-----",
		"Google API key":            `key=AIzaSyA1234567890abcdefghijklmnopqrstuv`,
	}
	for kind, line := range hits {
		f := ScanSecrets(line)
		found := false
		for _, x := range f {
			if x.Kind == kind {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: findings %v don't include the expected kind for %q", kind, f, line)
		}
	}
}

func TestScanSecretsURLCredentials(t *testing.T) {
	// A password embedded in a connection string is flagged regardless of the key name (the key
	// isn't a credential word, so the entropy path never sees it).
	for _, line := range []string{
		`DATABASE_URL=postgres://app:S3cr3tP4ss@db.internal:5432/app`,
		`redis = "redis://default:hunter2pass@cache:6379/0"`,
		`amqp://guest:r4bb1tMQpw@broker//`,
	} {
		if f := ScanSecrets(line); len(f) == 0 {
			t.Errorf("expected a connection-string password finding for %q", line)
		}
	}
	// NOT flagged: a plain URL with no inline password, an interpolated/placeholder password,
	// or one too short to be a real secret.
	for _, line := range []string{
		`token_url = https://api.example.com/oauth/token`,
		`url: redis://cache:6379/0`,
		`DB=postgres://user:${DB_PASSWORD}@host/db`,
		`x = https://user:pass@host`, // "pass" < 6 chars
	} {
		if f := ScanSecrets(line); len(f) != 0 {
			t.Errorf("did not expect a finding for %q, got %v", line, f)
		}
	}
}

func TestScanSecretsEntropy(t *testing.T) {
	// A long, high-entropy value on a secret-named key is flagged — quoted or not.
	if f := ScanSecrets(`api_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sG2eU4iO7p"`); len(f) == 0 {
		t.Error("expected an entropy finding for a random api_key value")
	}
	if f := ScanSecrets(`API_TOKEN=aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx`); len(f) == 0 {
		t.Error("expected an entropy finding for an unquoted random token (e.g. a .env value)")
	}
	if f := ScanSecrets(`auth_token = "850cb6abb7fc844f89c7bcc8adce5e9d0a2e7dc80f6c96c8f4022d8c45"`); len(f) == 0 {
		t.Error("expected an entropy finding for a literal hex auth_token")
	}
	if f := ScanSecrets(`client_secret: "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"`); len(f) == 0 {
		t.Error("expected an entropy finding for a literal Google client secret")
	}
	// A random value is still flagged even on a 'password' key — the word-guard only drops
	// values that literally contain a credential/fixture word, not every password line.
	if f := ScanSecrets(`password = "aB3xK9mP2qL7vR4tY8wZ"`); len(f) == 0 {
		t.Error("expected an entropy finding for a random password value")
	}
	// A token using underscores as separators is still flagged — the bare-identifier guard
	// only skips all-lower / all-upper names, not a mixed-case value that merely uses '_'.
	if f := ScanSecrets(`api_key = "tok_Ab3kP9xR2_mQ7vL4Tn8wZ1Cf6"`); len(f) == 0 {
		t.Error("expected an entropy finding for an underscore-separated mixed-case token")
	}
	// Specific *_key credential names now flagged: Rails secret_key_base, master_key, encryption_key.
	if f := ScanSecrets(`secret_key_base = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sG2eU4iO7p"`); len(f) == 0 {
		t.Error("expected an entropy finding for a literal Rails secret_key_base")
	}
	if f := ScanSecrets(`master_key: "850cb6abb7fc844f89c7bcc8adce5e9d0a2e7dc80f6c96c8"`); len(f) == 0 {
		t.Error("expected an entropy finding for a literal master_key")
	}
	if f := ScanSecrets(`encryption_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx"`); len(f) == 0 {
		t.Error("expected an entropy finding for a literal encryption_key")
	}
	// No false positives: a hash on a non-secret key, a short secret, prose, plain code.
	for _, clean := range []string{
		`commit = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"`, // not a secret-named key
		// Bare *_key names that are NOT credentials must stay unflagged (we add specific names,
		// never a bare "_key"): a DB key, a foreign key, and a PUBLIC key.
		`primary_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx"`,
		`foreign_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx"`,
		`public_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUvWx"`,
		`password = "hunter2"`,                               // too short for the entropy gate
		`greeting = "hello there, how are you today friend"`, // spaces / low entropy
		`func main() { fmt.Println("ok") }`,
		`name: Jane Doe`,
		// Code expressions on a secret-named key are references, not literal secrets.
		`databricks_api_key        = var.blitz_databricks_api_key`, // a Terraform var reference
		`password = config.database.api_password_field`,            // a dotted config reference
		`secret_token = process.env.SOME_LONG_SECRET_NAME`,         // an env reference
		`api_key = "${var.databricks_api_key_reference_here}"`,     // a template interpolation
		`api_key = generateApiKeyFromTheVaultService()`,            // a function call
		`ssh_authorized_keys = [tls_private_key.glass.public_key]`, // a bracketed list/ref
		// Values that aren't literal credentials: URLs and filesystem paths.
		`token_url = https://github.com/login/oauth/access_token`,          // a URL endpoint
		`GOOGLE_APPLICATION_CREDENTIALS = "/secrets/gcp_credentials.json"`, // a file path
		// Comments hold examples/placeholders, not live secrets.
		`;secret_key = SW2YcwTIb9zpOOhoPsMm`,            // a commented-out example
		`# api_key = aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sUv`, // ditto
		// Long cloud-resource references (a long first segment, no quotes).
		`s3_secret_key = google_storage_hmac_key.tolgee_s3.secret`,
		// Shell command substitutions / calls — captured only up to the first space.
		`nomad_token="$(create_or_reuse_nomad_token scraper scraper)"`,
		`auth_token = try(var.nomad_acl.drain_token, "")`,
		// An obvious placeholder, not a live secret.
		`password = "your-very-strong-initial-password"`,
		// A config key whose name merely contains a credential word but doesn't end in one.
		`authenticator = org.apache.cassandra.auth.PasswordAuthenticator`,
		`auth_read_consistency_level = LOCAL_QUORUM_WITH_FALLBACK_MODE`,
		// Code-file references: trailing punctuation, bare snake vars, curly interpolation.
		`email_token: session.email_token,`,                // dotted ref + trailing comma
		`apiKey = process.env.OPENROUTER_KEY;`,             // dotted ref + trailing semicolon
		`private_key: braintree_sandbox_private_key`,       // a bare snake_case variable
		`apiKey={context.row.apiKey} /* jsx expression */`, // a JSX/curly expression
		// More code shapes: Rust generics / namespaces, an Elixir module attribute, a
		// SCREAMING_SNAKE constant reference, and a value with a trailing ~S|…| sigil pipe.
		`credentials: Option<Credentials>,`,                                             // a Rust generic type
		`credentials: snownet::Credentials {`,                                           // a Rust namespace path
		`private_key: @google_workspace_private_key,`,                                   // an Elixir module attribute
		`access_token: PUBLIC_ACCESS_TOKEN,`,                                            // a SCREAMING_SNAKE constant
		`assert file =~ ~S|new Socket("/socket", {params: {token: window.userToken}})|`, // a code ref + sigil pipe
		// AWS's canonical documentation example keys — never live credentials.
		`access_key_id: "AKIAIOSFODNN7EXAMPLE"`,                            // the example AKIA id
		`AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY12`, // the example secret
		// Test fixtures: the value literally contains a credential/fixture word.
		`password: "very-long-password-1234"`,             // a test password (contains "password")
		`client_secret = "super-secret-fixture-value-x"`,  // a fixture (contains "secret")
		`payment_method_token: "fake-payment-method-tok"`, // a fixture (contains "fake")
	} {
		if f := ScanSecrets(clean); len(f) != 0 {
			t.Errorf("false positive on %q: %v", clean, f)
		}
	}
}

// A minified/generated bundle puts a whole program on one huge line, where a high-entropy
// token:"…" literal is a build artifact — the entropy heuristic skips a match drowning in
// more than maxEntropyLineSlack of surrounding code. The skip is per-match, not per-file
// or per-length: a secret pasted next to the blob still fires, a huge all-value line still
// fires, and the precise provider patterns scan the long line regardless.
func TestScanSecretsMinifiedLines(t *testing.T) {
	blob := strings.Repeat(`var e=function(t){return t.replace(/x/g,"")};`, 60)
	minified := `!function(){` + blob + `var s={token:"aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sG2eU4iO7p"}}();`
	if len(minified) <= maxEntropyLineSlack+300 {
		t.Fatalf("fixture line is only %d bytes — too short to exercise the guard", len(minified))
	}
	if f := ScanSecrets(minified); len(f) != 0 {
		t.Errorf("entropy heuristic fired on a minified bundle line: %v", f)
	}
	// A secret hand-pasted next to minified code sits on its own short line — still caught.
	pasted := minified + "\n" + `api_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sG2eU4iO7p"`
	if f := ScanSecrets(pasted); len(f) != 1 || f[0].Line != 2 {
		t.Errorf("expected exactly one finding on line 2 (the pasted secret), got %v", f)
	}
	// A multi-KB credential blob makes a long line too, but it's all VALUE, no slack —
	// a base64-encoded service-account key must keep firing however long it is.
	b64 := strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 48)
	if f := ScanSecrets(`credentials_b64 = "` + b64 + `"`); len(f) != 1 {
		t.Errorf("a %d-byte all-value credential line must keep firing, got %v", len(b64), f)
	}
	// The provider patterns have no slack cap: a real token INSIDE the minified line is
	// still reported.
	withTok := `!function(){var g="ghp_abcdefghijklmnopqrstuvwxyz0123456789";` + blob + `}();`
	if f := ScanSecrets(withTok); len(f) != 1 || f[0].Kind != "GitHub token" {
		t.Errorf("provider patterns must still scan a minified line, got %v", f)
	}
}

// A committed 3-part JWT used to be suppressed — its dotted shape parses as a code
// reference (codeRefRe) on the entropy path — so it has its own provider pattern now.
func TestScanSecretsJWT(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	for _, line := range []string{
		`token = "` + jwt + `"`,        // assigned — the shape the code-ref guard would skip
		"Authorization: Bearer " + jwt, // bare — no assignment for the entropy path at all
	} {
		found := false
		for _, x := range ScanSecrets(line) {
			if x.Kind == "JWT" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a JWT finding for %q", line)
		}
	}
	// A single eyJ… base64 segment is NOT a JWT: an inline-sourcemap data URI must stay
	// clean (provider patterns scan comment lines too, so this would be a loud FP).
	clean := `//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoiYXBwLmpzIn0=`
	if f := ScanSecrets(clean); len(f) != 0 {
		t.Errorf("single-segment base64 must not match the JWT pattern: %v", f)
	}
}

// A UUID-shaped value on a credential key KEEPS firing — evaluated and deliberately NOT
// exempted: real credentials ARE canonical UUIDs (Heroku API keys are lowercase v4), so
// blanking the 8-4-4-4-12 shape would hide a live credential class. Value-shape guards
// must be structural tells a random token never has, and hex+dash is a credential
// alphabet (.agent/rules/secret-scan-literals-not-refs.md).
func TestScanSecretsUUIDValueStillFlagged(t *testing.T) {
	if f := ScanSecrets(`heroku_api_key = "f47ac10b-58cc-4372-a567-0e02b2c3d479"`); len(f) == 0 {
		t.Error("a UUID-shaped credential value must keep firing (Heroku API keys are UUIDs)")
	}
}
