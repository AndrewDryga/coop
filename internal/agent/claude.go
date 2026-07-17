package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

type claudeAgent struct{}

func init() { register(claudeAgent{}) }

func (claudeAgent) Name() string        { return "claude" }
func (claudeAgent) DisplayName() string { return "Claude Code" }
func (claudeAgent) Badge() string       { return ui.Magenta("c") }
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
	if !ValidSessionID(id) {
		return a.Interactive(cfg), false
	}
	root, err := openSessionRoot(filepath.Join(cfg.AgentDir("claude"), "projects"))
	if err != nil {
		return a.Interactive(cfg), false
	}
	defer root.Close()
	bucket := ClaudeProjectKey(ws)
	info, err := root.Lstat(bucket)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return a.Interactive(cfg), false
	}
	info, err = root.Lstat(filepath.Join(bucket, id+".jsonl"))
	if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
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

// Effort: Claude Code takes --effort <level> (low/medium/high/xhigh/max) on its command.
func (claudeAgent) Effort() EffortSpec { return EffortSpec{Flag: "--effort"} }

// EffortEnv: the claude-agent-acp adapter takes no flags, so its effort rides
// CLAUDE_CODE_EFFORT_LEVEL — the effort analog of ANTHROPIC_MODEL, which box.Run exports.
func (claudeAgent) EffortEnv() string { return "CLAUDE_CODE_EFFORT_LEVEL" }

func (claudeAgent) InstructionFile() string { return "CLAUDE.md" }

func (claudeAgent) NativeSubagents() NativeSubagentSupport {
	return NativeSubagentSupport{HomeDir: ".claude/agents", Render: renderClaudeSubagent}
}

func renderClaudeSubagent(role NativeSubagent) (filename, content string) {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + role.Name + "\n")
	b.WriteString("description: " + role.Description + "\n")
	if role.Model != "" {
		b.WriteString("model: " + role.Model + "\n")
	}
	if role.Effort != "" {
		b.WriteString("effort: " + role.Effort + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(role.Prompt + "\n")
	return role.Name + ".md", b.String()
}

func (claudeAgent) AuthMarker() (file, envKey string) {
	return ".credentials.json", "ANTHROPIC_API_KEY"
}

// CredentialEnvKeys lists every env var Claude Code reads a token from: the API key plus
// the two alternates (a custom auth token and a headless OAuth token).
func (claudeAgent) CredentialEnvKeys() []string {
	return []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}
}

func (claudeAgent) LiveCredentials() LiveCredentialSpec {
	return LiveCredentialSpec{
		Artifacts: []CredentialArtifact{{
			Name: ".credentials.json", Primary: true, Project: projectClaudeCredential,
		}},
		Portability: claudeCredentialPortability,
		AuthSignals: []string{"not logged in", "invalid auth", "authentication_error", "please run /login"},
	}
}

type claudeAccessCredential struct {
	AccessToken string   `json:"accessToken"`
	ExpiresAt   int64    `json:"expiresAt"`
	Scopes      []string `json:"scopes"`
}

type claudeSourceCredential struct {
	claudeAccessCredential
	RefreshToken string `json:"refreshToken"`
}

func parseClaudeSourceCredential(data []byte) (claudeSourceCredential, error) {
	var source struct {
		OAuth *claudeSourceCredential `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return claudeSourceCredential{}, fmt.Errorf("decode Claude credential: %w", err)
	}
	if source.OAuth == nil {
		return claudeSourceCredential{}, fmt.Errorf("claude credential has no OAuth shape")
	}
	return *source.OAuth, nil
}

func parseClaudeAccessCredential(data []byte) (claudeAccessCredential, error) {
	source, err := parseClaudeSourceCredential(data)
	return source.claudeAccessCredential, err
}

func decodeClaudeAccessCredential(data []byte) (claudeAccessCredential, error) {
	projected, err := parseClaudeAccessCredential(data)
	if err != nil {
		return claudeAccessCredential{}, err
	}
	if projected.AccessToken == "" || projected.ExpiresAt <= 0 ||
		!slices.Contains(projected.Scopes, "user:inference") {
		return claudeAccessCredential{}, fmt.Errorf("claude credential has no access-only OAuth shape")
	}
	projected.Scopes = []string{"user:inference"}
	return projected, nil
}

func projectClaudeCredential(data []byte) ([]byte, error) {
	projected, err := parseClaudeAccessCredential(data)
	if err != nil {
		return nil, err
	}
	// A refresh-only login is safe to stage after stripping refresh authority, but unusable. Keep
	// the empty access shape so Portability can report credential_refresh_required before launch.
	if !slices.Contains(projected.Scopes, "user:inference") {
		return nil, fmt.Errorf("claude credential has no inference scope")
	}
	projected.Scopes = []string{"user:inference"}
	encoded, err := json.Marshal(struct {
		OAuth claudeAccessCredential `json:"claudeAiOauth"`
	}{OAuth: projected})
	if err != nil {
		return nil, fmt.Errorf("encode Claude credential: %w", err)
	}
	return append(encoded, '\n'), nil
}

func (a claudeAgent) ActiveCredentialEnvKeys(_ string, markerPresent bool) []string {
	if markerPresent {
		return nil
	}
	return a.CredentialEnvKeys()
}

func (claudeAgent) StoredCredentialStatus(profileDir string, now time.Time) StoredCredentialStatus {
	data, err := os.ReadFile(filepath.Join(profileDir, ".credentials.json"))
	if err != nil {
		return StoredCredentialReauthRequired
	}
	credential, err := parseClaudeSourceCredential(data)
	if err != nil || !slices.Contains(credential.Scopes, "user:inference") {
		return StoredCredentialReauthRequired
	}
	if credential.RefreshToken != "" || (credential.AccessToken != "" && credential.ExpiresAt > 0 &&
		time.UnixMilli(credential.ExpiresAt).After(now)) {
		return StoredCredentialReady
	}
	return StoredCredentialReauthRequired
}

func claudeCredentialPortability(profileDir string, deadline time.Time) CredentialPortability {
	data, err := os.ReadFile(filepath.Join(profileDir, ".credentials.json"))
	if err != nil {
		return CredentialRefreshRequired
	}
	credentials, err := decodeClaudeAccessCredential(data)
	if err == nil && time.UnixMilli(credentials.ExpiresAt).After(deadline) {
		return CredentialPortable
	}
	return CredentialRefreshRequired
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

// ACPSessionSettings keeps Claude's live session aligned with Coop's target. Model
// precedes effort because each model owns its available effort levels.
func (claudeAgent) ACPSessionSettings(target Target) []ACPSessionSetting {
	settings := []ACPSessionSetting{{Method: ACPSetConfigOption, ConfigID: "mode", Value: "bypassPermissions"}}
	if target.Model != "" {
		settings = append(settings, ACPSessionSetting{Method: ACPSetConfigOption, ConfigID: "model", Value: target.Model})
	}
	if target.Effort != "" {
		settings = append(settings, ACPSessionSetting{Method: ACPSetConfigOption, ConfigID: "effort", Value: target.Effort})
	}
	return settings
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

func (claudeAgent) HomeFallbacks() []HomeFallback {
	return []HomeFallback{
		{Source: ".agent/claude/settings.json", Project: ".claude/settings.json", Target: ".claude/settings.json"},
		{Source: ".agent/claude/hooks", Project: ".claude/hooks", Target: ".claude/hooks", Dir: true},
	}
}

// coop-consult / coop-delegate shell — the wrapper generators concatenate these per-agent
// arms (see internal/fusion/wrapper.go, internal/preset/wrapper.go). They run against the
// wrapper's $prompt/$id/$model/$candidate_idfile and its run/new_id helpers.

func (claudeAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$candidate_idfile\"\n" +
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
