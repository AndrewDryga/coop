package agent

import (
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
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
