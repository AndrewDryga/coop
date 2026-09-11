package cli

// The inventory of what `coop doctor` checks. Each id is the name the probes speak and the report
// reads; it never reaches a human. The two labels are what a person sees — one for the check
// holding, one for it not — written as statements of fact so a report can be skimmed for the
// lines that are not in the expected shape.
//
// Adding a check here without emitting its id from a probe would make the report claim a check
// that never ran, so the ids live in exactly two places: this table and the probe that reports it.

type doctorCheckDef struct {
	id      string
	pass    string
	fail    string
	failWhy string // the safe measured fact behind the failure; never the secret itself
}

const (
	sectionSecrets     = "Protecting secrets"
	sectionHost        = "Host access and privileges"
	sectionOffline     = "Offline access"
	sectionTasks       = "Task access"
	sectionCredentials = "Credentials and settings"
	sectionClone       = "Fork handoff"
)

// doctorSecretChecks are the in-box sandbox assertions, in probe order.
var doctorSecretChecks = []doctorCheckDef{
	{"sandbox.env", ".env is hidden", ".env is readable in the box", ""},
	{"sandbox.envrc", ".envrc is hidden", ".envrc is readable in the box", ""},
	{"sandbox.tfvars", "Terraform variable files are hidden", "config/prod.tfvars is readable in the box", ""},
	{"sandbox.private_key", "Private keys are hidden", "deploy/id_ed25519 is readable in the box", ""},
	{"sandbox.coopignore", "Custom .coopignore paths are hidden", "config/credentials.yaml is readable in the box", ""},
	{"sandbox.secret_directory", "Secret directories are empty", "Secret directories expose files", "secrets/"},
	{"sandbox.secret_symlink", "Links cannot reveal hidden secrets", "A link can reveal a hidden secret", "notes-link → .env"},
	{"sandbox.readonly_decoy", "Hidden secret files cannot be written", "A hidden secret file can be written", ".env"},
	{"sandbox.template", "Environment templates stay readable", ".env.example is not readable", ""},
	{"sandbox.source", "Source files stay readable", "src/app.js is not readable", ""},
	{"sandbox.secret_value", "The test secret cannot be read", "The test secret can be read", ""},
}

// doctorHostChecks are the two host-reach assertions the same in-box probe reports. The privilege
// measurements that complete this section are interpreted on the host (they depend on the image
// and the runtime), so they are not table-driven.
var doctorHostChecks = []doctorCheckDef{
	{"host.coop_cli", "The host Coop CLI is absent", "The host Coop CLI is available in the box", "command -v coop resolved in the box"},
	{"host.docker_socket", "The host Docker socket is absent", "The host Docker socket is available in the box", "/var/run/docker.sock"},
}

// doctorCredentialChecks are the credential-boundary assertions from the claude-scoped box. Only
// the NAME of an environment variable is ever reported, never its value.
var doctorCredentialChecks = []doctorCheckDef{
	{"credential.own_home", "Claude's credential home is available", "Claude's credential home is missing", ".claude/.credentials.json"},
	{"credential.codex_home", "Codex's credential home is hidden from Claude", "Codex's credential home is visible to Claude", ".codex/auth.json"},
	{"credential.gemini_home", "Gemini's credential home is hidden from Claude", "Gemini's credential home is visible to Claude", ".gemini/gemini-credentials.json"},
	{"credential.own_env", "Claude's saved login takes priority over its environment key", "Claude's environment key overrides its saved login", "ANTHROPIC_API_KEY"},
	{"credential.peer_env", "Codex's environment key is hidden from Claude", "Codex's environment key is visible to Claude", "OPENAI_API_KEY"},
	{"credential.peer_alias", "Gemini's environment key is hidden from Claude", "Gemini's environment key is visible to Claude", "GOOGLE_API_KEY"},
}

// doctorCloneChecks are the host-side fork-handoff assertions.
var doctorCloneChecks = []doctorCheckDef{
	{"clone.env", ".env is absent from the clone", ".env entered the clone", ""},
	{"clone.envrc", ".envrc is absent from the clone", ".envrc entered the clone", ""},
	{"clone.secrets", "Secret directories are absent from the clone", "secrets/ entered the clone", ""},
	{"clone.keys", "Private keys are absent from the clone", "deploy/id_ed25519 entered the clone", ""},
	{"clone.source", "Tracked source files are present in the clone", "Tracked source files are missing from the clone", "src/app.js"},
	{"clone.secret_value", "The test secret is absent from the clone", "The test secret entered the clone", ""},
	{"clone.origin", "The clone's origin is a local path", "The clone's origin is not a local path", ""},
}

// record applies one probe verdict to a section, choosing the label from the table.
func (s *doctorSection) record(def doctorCheckDef, ok bool) {
	if ok {
		s.pass(def.pass)
		return
	}
	s.fail(def.fail, def.failWhy, "")
}
