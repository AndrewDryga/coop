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
// given repo might hold (e.g. a committed config/credentials.yaml). Add repo-specific
// paths in a .coopignore (see CoopIgnoreFile / LoadUserGlobs).
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
}

// AllowGlobs are template/sample files the agent should still see, even when
// their name would otherwise match a secret pattern (e.g. .env.example). They win
// over both SecretGlobs and a repo's .coopignore, so a template is never hidden.
var AllowGlobs = []string{"*.example", "*.sample", "*.template"}

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
