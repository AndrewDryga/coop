package box

import "github.com/AndrewDryga/coop/internal/secretscan"

// SecretFinding preserves the box package's public scan result while the pure scanner is shared
// with the outbound worker checkpoint boundary.
type SecretFinding = secretscan.SecretFinding

const maxEntropyLineSlack = 2048

// ScanSecrets reports likely secrets in textual content.
func ScanSecrets(content string) []SecretFinding {
	return secretscan.ScanSecrets(content)
}
