package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
)

type claudeAgent struct{}

func init() { register(claudeAgent{}) }

func (claudeAgent) Name() string        { return "claude" }
func (claudeAgent) DisplayName() string { return "Claude Code" }
func (claudeAgent) Stream() StreamSpec {
	return StreamSpec{
		Format: StreamClaudeJSON,
		Flags:  []string{"--output-format", "stream-json", "--verbose"},
	}
}

// base is claude's command plus the resolved model and --mcp-config when a shared
// mcp.json exists — claude reads it directly, where gemini/codex get generated config files.
func (claudeAgent) base(cfg *config.Config) []string {
	cmd := cfg.Cmd("COOP_CLAUDE_CMD", "claude --dangerously-skip-permissions")
	if len(cmd) == 0 { // an explicitly-empty override would otherwise yield a no-executable argv
		cmd = []string{"claude"}
	}
	cmd = withModel(cmd, cfg.ModelFor("claude"))
	cmd = withEffort(cmd, claudeAgent{}, cfg.EffortFor("claude"))
	if cfg.MCPActive() {
		cmd = append(cmd, "--mcp-config", cfg.MCPInBox)
	}
	return cmd
}

func (a claudeAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a claudeAgent) Headless(cfg *config.Config, prompt string) []string {
	return append(a.base(cfg), "-p", prompt)
}

// ACP is a separate adapter binary that takes no agent flags — the chosen model reaches
// the claude it spawns via ModelEnv (ANTHROPIC_MODEL), which box.Run exports.
func (claudeAgent) ACP(*config.Config) []string { return []string{"claude-agent-acp"} }

// ACPSessionDirs: claude stores the transcript in projects/ and a session index + aux state in
// sessions/ (and session-env/, file-history/); session/load needs the index too, so share them all.
func (claudeAgent) ACPSessionDirs() []string {
	return []string{"projects", "sessions", "session-env", "file-history"}
}

func (claudeAgent) PresetSessionID() bool { return true }

func (a claudeAgent) StartSession(cfg *config.Config, id string) []string {
	if id == "" {
		return a.Interactive(cfg)
	}
	return append(a.base(cfg), "--session-id", id)
}

// Resume pins the coop-owned session id — claude stores a session at
// projects/<cwd>/<id>.jsonl — so re-entry lands on exactly that conversation, never a
// loop or consult session that merely shares the cwd (which `--continue` would pick).
// No file for id yet → it was never created; the caller starts it fresh under that id.
func (a claudeAgent) Resume(cfg *config.Config, ws, id string) ([]string, bool) {
	if id == "" {
		return a.Interactive(cfg), false
	}
	sess := filepath.Join(cfg.AgentDir("claude"), "projects", ClaudeProjectKey(ws), id+".jsonl")
	if _, err := os.Stat(sess); err == nil {
		return append(a.base(cfg), "--resume", id), true
	}
	return a.Interactive(cfg), false
}

// Login forces the sign-in flow via the explicit subcommand. Bare `claude` only
// prompts to log in when no credentials exist, so it's a no-op once authenticated
// (it just opens a session) — `auth login` re-authenticates and switches accounts.
func (claudeAgent) Login(*config.Config) []string { return []string{"claude", "auth", "login"} }

func (claudeAgent) ConsultCmd(question string) []string {
	return []string{"claude", "-p", "--permission-mode", "plan", question}
}

const (
	claudeCLIPackage = "@anthropic-ai/claude-code@latest"
	claudeACPPackage = "@agentclientprotocol/claude-agent-acp@latest"
)

func (claudeAgent) Packages() []string {
	return []string{claudeCLIPackage, claudeACPPackage}
}

// Models are the stable Claude Code aliases (each resolves to that family's current
// model), plus full ids as examples. Illustrative — any id the CLI accepts works.
func (claudeAgent) Models() []string {
	return []string{"fable", "opus", "sonnet", "haiku", "claude-fable-5", "claude-opus-4-8", "claude-sonnet-5"}
}

// ModelEnv: Claude Code reads its default model from ANTHROPIC_MODEL — how the model
// reaches the claude-agent-acp adapter (and any claude subprocess) that takes no flags.
func (claudeAgent) ModelEnv() string { return "ANTHROPIC_MODEL" }

// EffortFlag: Claude Code takes --effort <level> (low/medium/high/xhigh/max) on its command.
func (claudeAgent) EffortFlag(level string) []string { return []string{"--effort", level} }

// EffortEnv: the claude-agent-acp adapter takes no flags, so its effort rides
// CLAUDE_CODE_EFFORT_LEVEL — the effort analog of ANTHROPIC_MODEL, which box.Run exports.
func (claudeAgent) EffortEnv() string { return "CLAUDE_CODE_EFFORT_LEVEL" }

func (claudeAgent) InstructionFile() string { return "CLAUDE.md" }

func (claudeAgent) AuthMarker() (file, envKey string) {
	return ".credentials.json", "ANTHROPIC_API_KEY"
}

// CredentialEnvKeys lists every env var Claude Code reads a token from: the API key plus
// the two alternates (a custom auth token and a headless OAuth token).
func (claudeAgent) CredentialEnvKeys() []string {
	return []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
}

// MCP is nil: claude reads the shared mcp.json directly via --mcp-config (see base).
func (claudeAgent) MCP(*config.Config) ([]MCPMount, error) { return nil, nil }

// ClaudeProjectKey is how Claude Code names a project's session dir: the absolute cwd with
// every non-alphanumeric character (not just "/") turned into a dash. coop must match it
// exactly to find the session file to resume.
func ClaudeProjectKey(ws string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, ws)
}

// EnsureDefaults pre-answers Claude Code's first-run prompts (theme, folder-trust, the
// bypass-permissions warning) and pins its bash OS-sandbox off — the box is itself the
// sandbox and ships no bubblewrap. Existing values are preserved; only missing flags
// are filled, and a file is rewritten only when something changes.
func (a claudeAgent) EnsureDefaults(cfg *config.Config, workdir string) {
	dir := cfg.AgentDir(a.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	// settings.json: a default theme (kept if already chosen), the flag that skips the
	// bypass warning, and the sandbox pinned off.
	settings := filepath.Join(dir, "settings.json")
	sm := map[string]any{}
	if data, err := os.ReadFile(settings); err == nil {
		_ = json.Unmarshal(data, &sm)
	}
	sChanged := false
	if _, ok := sm["theme"]; !ok {
		sm["theme"] = "dark"
		sChanged = true
	}
	if ensureTrue(sm, "skipDangerousModePermissionPrompt") {
		sChanged = true
	}
	if ensureSandboxOff(sm) {
		sChanged = true
	}
	if sChanged {
		writeJSONFile(settings, sm, 0o644)
	}
	// .claude.json: accept onboarding/bypass and trust the workdir, merged so the
	// account/onboarding state survives the disposable box.
	cj := filepath.Join(dir, ".claude.json")
	m := map[string]any{}
	if data, err := os.ReadFile(cj); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	changed := ensureTrue(m, "hasCompletedOnboarding")
	if ensureTrue(m, "bypassPermissionsModeAccepted") {
		changed = true
	}
	if ensureWorkdirTrusted(m, workdir) {
		changed = true
	}
	if changed {
		writeJSONFile(cj, m, 0o600)
	}
}

// ensureSandboxOff pins Claude Code's bash OS-sandbox off in settings.json, filling
// only the missing keys so a user's explicit choice survives. Reports a change.
func ensureSandboxOff(m map[string]any) bool {
	sb, _ := m["sandbox"].(map[string]any)
	if sb == nil {
		sb = map[string]any{}
		m["sandbox"] = sb
	}
	changed := false
	if _, ok := sb["enabled"]; !ok {
		sb["enabled"] = false
		changed = true
	}
	if _, ok := sb["failIfUnavailable"]; !ok {
		sb["failIfUnavailable"] = false
		changed = true
	}
	return changed
}

// ensureWorkdirTrusted marks the box workdir as a trusted project in .claude.json.
func ensureWorkdirTrusted(m map[string]any, workdir string) bool {
	projects, _ := m["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		m["projects"] = projects
	}
	wp, _ := projects[workdir].(map[string]any)
	if wp == nil {
		wp = map[string]any{}
		projects[workdir] = wp
	}
	if v, ok := wp["hasTrustDialogAccepted"].(bool); ok && v {
		return false
	}
	wp["hasTrustDialogAccepted"] = true
	return true
}

// ACPRateLimitSignals: claude-agent-acp tags a limit error with errorKind=rate_limit
// in the error data (both rate_limit and rateLimit spellings compact-match).
func (claudeAgent) ACPRateLimitSignals() []ACPSignal {
	return []ACPSignal{{Key: "errorKind", Value: "rate_limit"}}
}

// ACPSessionConfig: force bypassPermissions so claude's own toolbar reflects coop's
// yolo (the box is the sandbox) and it skips the per-tool permission round-trips.
func (claudeAgent) ACPSessionConfig() map[string]string {
	return map[string]string{"mode": "bypassPermissions"}
}

// BoxEnv: point $CLAUDE_CONFIG_DIR at the mounted ~/.claude so account + onboarding
// state persists across disposable boxes (the default ~/.claude.json in $HOME would be
// lost every run, re-prompting login), and turn off the bubblewrap subprocess env scrub
// — the box ships no bubblewrap and is itself the isolation boundary.
func (claudeAgent) BoxEnv(homeInBox string) []string {
	return []string{
		"CLAUDE_CONFIG_DIR=" + homeInBox + "/.claude",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=0",
	}
}

// coop-consult / coop-delegate shell — the wrapper generators concatenate these per-agent
// arms (see internal/fusion/wrapper.go, internal/preset/wrapper.go). They run against the
// wrapper's $prompt/$id/$model/$idfile and its run/new_id helpers.

func (claudeAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$idfile\"\n" +
		`run claude -p --permission-mode plan --session-id "$id" ${model:+--model "$model"} ${effort:+--effort "$effort"} "$prompt"`
}

func (claudeAgent) ConsultResume() string {
	return `run claude -p --permission-mode plan --resume "$id" ${model:+--model "$model"} ${effort:+--effort "$effort"} "$prompt"`
}

func (claudeAgent) DelegateExec() string {
	return `claude -p --dangerously-skip-permissions ${model:+--model "$model"} ${effort:+--effort "$effort"} "$prompt"`
}

func (claudeAgent) ShellPrelude() string  { return "" }
func (claudeAgent) InstallScript() string { return "" }
