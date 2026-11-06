// Package secretname decides whether a file or folder name looks like a secret, from one built-in
// list. A launch hides every name Hidden reports (shadowpath), and the filesystem helper builds the
// same list into the table its eBPF guard reads, so a file whose name looks like a secret is
// unreadable in the box even when it appears after the box starts.
package secretname

import (
	"path/filepath"
	"strings"
)

// SecretGlobs are the names that look like secrets. Hidden matches them against the lowercased name.
var SecretGlobs = []string{
	".env", ".env.*", ".envrc", "*.secret", "*.secrets",
	"*.tfvars", "*.tfvars.json", "*.tfstate", "*.tfstate.*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.p8", "*.ppk", "*.kdbx", "*.ovpn", "*.pkcs12",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
	".netrc", "_netrc", ".npmrc", ".yarnrc", ".yarnrc.yml", ".pypirc", ".git-credentials", ".htpasswd",
	".dockercfg", ".pgpass", ".my.cnf", ".s3cfg", ".boto", ".vault-token", "vault-token",
	"secrets", ".secrets", "credentials", ".aws", ".kube", ".ssh", ".gnupg", ".docker",
	"credentials.json", "service_account.json", "service-account.json", "*-sa.json", "client_secret*.json",
	"firebase-adminsdk*.json", "gha-creds-*.json", "auth.json", "secret.json", "secrets.json", "*.secret.json",
	// Config-style names that say "secrets" (a sourced .shell-secrets, secrets.env), never source or docs.
	".*secrets", "secrets.env", "secret.env", "*-secrets.env", "*_secrets.env", "secrets.conf", "secrets.cfg",
	"secrets.ini", "secrets.toml", "secrets.properties", "secrets.txt", "secrets.sh",
	"kubeconfig", "kubeconfig.yaml", "kubeconfig.yml", "database.yml", "credentials.y*ml", "secrets.y*ml",
}

// AllowGlobs are public certificate bundles that SecretGlobs would otherwise hide.
var AllowGlobs = []string{
	"cacerts.pem", "cacert.pem", "ca-bundle.pem", "ca-bundle.crt", "ca-certificates.crt", "ca-cert.pem",
}

// TemplateGlobs are examples that stay visible, unless HardGlobs says the name is a key whatever it
// ends with.
var TemplateGlobs = []string{"*.example", "*.sample", "*.template"}

// HardGlobs are key and keystore names that stay hidden even as a template.
var HardGlobs = []string{
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.p8", "*.ppk", "*.kdbx", "*.pkcs12",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
}

// Hidden reports whether a file or folder name looks like a secret: its lowercased form matches
// SecretGlobs, and neither AllowGlobs nor a TemplateGlobs that HardGlobs does not override rescues it.
func Hidden(name string) bool {
	lname := strings.ToLower(name)
	allowed := matchesAny(lname, AllowGlobs) || matchesAny(lname, TemplateGlobs) && !matchesAny(lname, HardGlobs)
	return matchesAny(lname, SecretGlobs) && !allowed
}

func matchesAny(name string, globs []string) bool {
	for _, glob := range globs {
		if ok, _ := filepath.Match(glob, name); ok {
			return true
		}
	}
	return false
}
