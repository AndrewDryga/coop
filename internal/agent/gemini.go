package agent

import (
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type geminiAgent struct{}

func init() { register(geminiAgent{}) }

func (geminiAgent) Name() string { return "gemini" }

func (geminiAgent) base(cfg *config.Config) []string {
	return cfg.Cmd("COOP_GEMINI_CMD", "gemini --yolo")
}

func (a geminiAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a geminiAgent) Headless(cfg *config.Config, prompt string) []string {
	return append(a.base(cfg), "-p", prompt)
}

func (geminiAgent) ACP() []string { return []string{"gemini", "--acp"} }

func (a geminiAgent) Resume(cfg *config.Config, ws string) ([]string, bool) {
	// gemini keys sessions by project basename under ~/.gemini/tmp/<base>/chats.
	if hasEntries(filepath.Join(cfg.AgentDir("gemini"), "tmp", filepath.Base(ws), "chats")) {
		return append(a.base(cfg), "--resume", "latest"), true
	}
	return a.Interactive(cfg), false
}

func (geminiAgent) Login(*config.Config) []string { return []string{"gemini"} }

func (geminiAgent) ConsultCmd(question string) []string {
	// -p takes the prompt as its value, so it must come last (right before the
	// question); otherwise -p swallows --approval-mode and gemini prints help.
	return []string{"gemini", "--approval-mode", "plan", "-p", question}
}

func (geminiAgent) InstructionFile() string { return "GEMINI.md" }

func (geminiAgent) AuthMarker() (file, envKey string) {
	return "gemini-credentials.json", "GEMINI_API_KEY"
}

// MCP merges the shared servers into gemini's settings.json.
func (geminiAgent) MCP(cfg *config.Config) ([]MCPMount, error) {
	gm, err := mcp.GenerateGemini(cfg.MCPFile, filepath.Join(cfg.AgentDir("gemini"), "settings.json"))
	if err != nil {
		return nil, err
	}
	return []MCPMount{{Content: gm, BoxPath: cfg.HomeInBox + "/.gemini/settings.json"}}, nil
}
