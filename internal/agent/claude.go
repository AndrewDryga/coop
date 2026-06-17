package agent

import (
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

func (claudeAgent) Login(*config.Config) []string { return []string{"claude"} }

func (claudeAgent) ConsultCmd(question string) []string {
	return []string{"claude", "-p", "--permission-mode", "plan", question}
}

// claudeProjectKey is how Claude Code names a project's session dir: the absolute cwd
// with path separators turned into dashes.
func claudeProjectKey(ws string) string { return strings.ReplaceAll(ws, "/", "-") }
