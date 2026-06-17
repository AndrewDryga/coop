package box

import "testing"

func TestScanSecretsPatterns(t *testing.T) {
	hits := map[string]string{
		"Anthropic API key": `client = "sk-ant-api03-abcDEF1234567890abcDEF12"`,
		"OpenAI API key":    `OPENAI_KEY=sk-proj-abcDEF1234567890abcDEF1234`,
		"AWS access key id": `aws_key: AKIAIOSFODNN7EXAMPLE`,
		"GitHub token":      `token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"`,
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

func TestScanSecretsEntropy(t *testing.T) {
	// A long, high-entropy value on a secret-named key is flagged.
	if f := ScanSecrets(`api_key = "aB3xK9mP2qL7vR4tY8wZ1cF6nH5jD0sG2eU4iO7p"`); len(f) == 0 {
		t.Error("expected an entropy finding for a random api_key value")
	}
	// No false positives: a hash on a non-secret key, a short secret, prose, plain code.
	for _, clean := range []string{
		`commit = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"`, // not a secret-named key
		`password = "hunter2"`,                                // too short for the entropy gate
		`greeting = "hello there, how are you today friend"`,  // spaces / low entropy
		`func main() { fmt.Println("ok") }`,
		`name: Jane Doe`,
	} {
		if f := ScanSecrets(clean); len(f) != 0 {
			t.Errorf("false positive on %q: %v", clean, f)
		}
	}
}
