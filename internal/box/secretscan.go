package box

import "github.com/AndrewDryga/coop/internal/secretscan"

// ScanSecrets reports likely secrets in textual content.
func ScanSecrets(content string) []secretscan.SecretFinding {
	return secretscan.ScanSecrets(content)
}
