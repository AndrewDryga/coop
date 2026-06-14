package box

// SecretGlobs are filename patterns that must never enter the box. A match is
// shadowed: a directory becomes an empty tmpfs, a file an empty read-only decoy.
// Matching is by basename, at any depth (the repo's .git is always skipped).
var SecretGlobs = []string{
	".env", ".env.*", "*.secret", "*.secrets",
	"*.tfvars", "*.tfstate", "*.tfstate.*",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks",
	"id_rsa*", "id_ed25519*", "id_ecdsa*",
	".netrc", ".npmrc", ".pypirc", ".git-credentials",
	"secrets", ".secrets", "credentials", ".aws", ".kube", ".ssh", ".gnupg",
}

// AllowGlobs are template/sample files the agent should still see, even when
// their name would otherwise match a secret pattern (e.g. .env.example).
var AllowGlobs = []string{"*.example", "*.sample", "*.template"}
