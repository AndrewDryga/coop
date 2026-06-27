package box

import (
	"os"
	"path/filepath"
	"strings"
)

// SecretGlobs are filename patterns that must never enter the box. A match is
// shadowed: a directory becomes an empty tmpfs, a file an empty read-only decoy.
// Matching is by basename, at any depth (the repo's .git is always skipped).
//
// This is a denylist: it catches well-known credential names, NOT every secret a
// given repo might hold (e.g. a token in an app config under a custom name). Add
// repo-specific paths in a .coopignore (see CoopIgnoreFile / LoadUserGlobs).
// Matching is case-insensitive (see NewShadowDecider), so .ENV / ID_RSA can't slip past.
var SecretGlobs = []string{
	".env", ".env.*", "*.secret", "*.secrets",
	"*.tfvars", "*.tfstate", "*.tfstate.*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.p8", "*.ppk", "*.kdbx", "*.ovpn",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
	".netrc", ".npmrc", ".pypirc", ".git-credentials", ".htpasswd", ".dockercfg", ".pgpass",
	"secrets", ".secrets", "credentials", ".aws", ".kube", ".ssh", ".gnupg",
	// High-confidence service-credential filenames the list missed: GCP service-account keys, a
	// literal kubeconfig (only the .kube dir was caught before), and Rails DB creds. NOT added:
	// *.crt/*.cer (usually PUBLIC certs — shadowing them breaks in-box TLS, cf. the cacerts task)
	// nor application*.yml / settings.local.json (commonly non-secret app config — too false-positive).
	"credentials.json", "service_account.json", "*-sa.json", "kubeconfig", "database.yml",
	// YAML credential/secret files (the .json/.yml variants above missed .yaml/.yml here): a
	// committed credentials.yaml or secrets.yml is a common real leak. `y*ml` covers .yaml and .yml.
	"credentials.y*ml", "secrets.y*ml",
}

// AllowGlobs are EXACT, known-PUBLIC filenames that stay visible even though they match a secret
// pattern: the well-known CA bundles that `*.pem`/`*.crt` would otherwise shadow — emptying a
// trusted CA bundle breaks TLS verification inside the box (e.g. Elixir's castore at
// deps/castore/priv/cacerts.pem). These are specific public files, so they override even a
// high-confidence key pattern. An explicit .coopignore entry is still authoritative and can
// re-hide one.
var AllowGlobs = []string{
	"cacerts.pem", "cacert.pem", "ca-bundle.pem", "ca-bundle.crt", "ca-certificates.crt", "ca-cert.pem",
}

// allowTemplateGlobs are template/sample suffixes that rescue an ordinary secret-named file (a
// .env.example) from a false positive. They override only the SOFT secret patterns — NEVER a
// private-key/keystore pattern (hardSecretGlobs): a file literally named `id_rsa.example` or
// `server.key.sample` is too dangerous to wave through on a suffix alone, because the content
// scanner does NOT run against what a live box reads — only shadowing protects it at runtime.
var allowTemplateGlobs = []string{"*.example", "*.sample", "*.template"}

// hardSecretGlobs are the high-confidence private-key / keystore patterns (the subset of
// SecretGlobs) that a template suffix must never un-shadow.
var hardSecretGlobs = []string{
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.p8", "*.ppk", "*.kdbx",
	"id_rsa*", "id_ed25519*", "id_ecdsa*", "id_dsa*",
}

// CoopIgnoreFile is the repo-local file listing extra paths to shadow, one per line
// (# comments and blank lines ignored). It extends the SecretGlobs denylist with
// project-specific secrets the defaults can't know about.
const CoopIgnoreFile = ".coopignore"

// UserGlobs are the extra shadow patterns parsed from a repo's .coopignore, split by
// whether they target a basename (no slash, matched at any depth, like SecretGlobs) or
// a repo-relative path (contains a slash, matched against the path with filepath.Match,
// so `config/*.yaml` and `config/creds.yaml` work; there is no `**`).
type UserGlobs struct {
	Base []string
	Path []string
}

// LoadUserGlobs reads <repo>/.coopignore into a UserGlobs. A missing or unreadable
// file yields no patterns (the defaults still apply) — it never errors, so a typo'd
// file can't open a hole by aborting the scan.
func LoadUserGlobs(repo string) UserGlobs {
	var g UserGlobs
	data, err := os.ReadFile(filepath.Join(repo, CoopIgnoreFile))
	if err != nil {
		return g
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Normalize gitignore-ish anchoring: a leading "/" or "./" is repo-root
		// relative, which path matching already is; a trailing "/" (dir marker) is
		// dropped since we match dirs and files alike.
		line = strings.TrimPrefix(line, "./")
		line = strings.TrimPrefix(line, "/")
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		if strings.Contains(line, "/") {
			g.Path = append(g.Path, filepath.ToSlash(line))
		} else {
			g.Base = append(g.Base, line)
		}
	}
	return g
}
