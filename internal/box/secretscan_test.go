package box

import "testing"

func TestScanSecretsPatterns(t *testing.T) {
	hits := map[string]string{
		"Anthropic API key": `client = "sk-ant-api03-abcDEF1234567890abcDEF12"`,
		"OpenAI API key":    `OPENAI_KEY=sk-proj-abcDEF1234567890abcDEF1234`,
		"AWS access key id": `aws_key: AKIA1234567890ABCDEF`,
		"GitHub token":      `token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"`,
		"GitHub fine-grained token": `pat = github_pat_11ABCDEFGHIJKLMNOPQRST_0aBcDeFgHiJkLmNoPqRsTuVwXyZ012345`,
		"private key":       "-----BEGIN OPENSSH PRIVATE KEY-----",
		"Google API key":    `key=AIzaSyA1234567890abcdefghijklmnopqrstuv`,
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
