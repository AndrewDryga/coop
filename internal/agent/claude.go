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

func (claudeAgent) Name() string { return "claude" }

// base is claude's command plus --mcp-config when a shared mcp.json exists — claude
// reads it directly, where gemini/codex get generated config files.
func (claudeAgent) base(cfg *config.Config) []string {
	cmd := cfg.Cmd("COOP_CLAUDE_CMD", "claude --dangerously-skip-permissions")
	if cfg.MCPActive() {
		cmd = append(cmd, "--mcp-config", cfg.MCPInBox)
	}
	return cmd
}

func (a claudeAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a claudeAgent) Headless(cfg *config.Config, prompt string) []string {
	return append(a.base(cfg), "-p", prompt)
}

func (claudeAgent) ACP() []string { return []string{"claude-agent-acp"} }

func (a claudeAgent) Resume(cfg *config.Config, ws string) ([]string, bool) {
	if hasEntries(filepath.Join(cfg.AgentDir("claude"), "projects", claudeProjectKey(ws))) {
		return append(a.base(cfg), "--continue"), true
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

func (claudeAgent) Packages() []string {
	return []string{"@anthropic-ai/claude-code", "@agentclientprotocol/claude-agent-acp"}
}

func (claudeAgent) InstructionFile() string { return "CLAUDE.md" }

func (claudeAgent) AuthMarker() (file, envKey string) {
	return ".credentials.json", "ANTHROPIC_API_KEY"
}

// MCP is nil: claude reads the shared mcp.json directly via --mcp-config (see base).
func (claudeAgent) MCP(*config.Config) ([]MCPMount, error) { return nil, nil }

// claudeProjectKey is how Claude Code names a project's session dir: the absolute cwd
// with path separators turned into dashes.
func claudeProjectKey(ws string) string { return strings.ReplaceAll(ws, "/", "-") }

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
