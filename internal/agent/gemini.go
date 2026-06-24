package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type geminiAgent struct{}

func init() { register(geminiAgent{}) }

func (geminiAgent) Name() string { return "gemini" }

func (geminiAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_GEMINI_CMD", "gemini --yolo")
	if len(b) == 0 { // match codex's guard: an empty override must still yield a runnable command
		b = []string{"gemini"}
	}
	return b
}

func (a geminiAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a geminiAgent) Headless(cfg *config.Config, prompt string) []string {
	return append(a.base(cfg), "-p", prompt)
}

func (geminiAgent) ACP() []string { return []string{"gemini", "--acp"} }

func (geminiAgent) PresetSessionID() bool { return true }

func (a geminiAgent) StartSession(cfg *config.Config, id string) []string {
	if id == "" {
		return a.Interactive(cfg)
	}
	return append(a.base(cfg), "--session-id", id)
}

// Resume pins the coop-owned session id rather than "latest" — a loop or consult in
// the same cwd could be the latest, and gemini's tmp bucket is keyed by basename (so
// same-named forks in different repos can collide), but resuming an explicit uuid is
// immune to both. The id is matched by file content, so a change to gemini's session
// filename scheme can't silently break detection.
func (a geminiAgent) Resume(cfg *config.Config, ws, id string) ([]string, bool) {
	if id != "" && geminiHasSession(cfg, ws, id) {
		return append(a.base(cfg), "--resume", id), true
	}
	return a.Interactive(cfg), false
}

// geminiHasSession reports whether any chats file for ws records session id. gemini
// keys sessions by project basename under ~/.gemini/tmp/<base>/chats.
func geminiHasSession(cfg *config.Config, ws, id string) bool {
	dir := filepath.Join(cfg.AgentDir("gemini"), "tmp", filepath.Base(ws), "chats")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && strings.Contains(string(data), id) {
			return true
		}
	}
	return false
}

func (geminiAgent) Login(*config.Config) []string { return []string{"gemini"} }

func (geminiAgent) ConsultCmd(question string) []string {
	// -p takes the prompt as its value, so it must come last (right before the
	// question); otherwise -p swallows --approval-mode and gemini prints help.
	return []string{"gemini", "--approval-mode", "plan", "-p", question}
}

// Packages is just the CLI: gemini's ACP mode is built in (gemini --acp).
const geminiCLIPackage = "@google/gemini-cli@latest"

func (geminiAgent) Packages() []string { return []string{geminiCLIPackage} }

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

// EnsureDefaults guarantees a valid settings.json (an empty/missing one makes gemini
// fail at launch) and turns off its folder-trust prompt — the box is the sandbox. An
// existing choice is kept; a non-blank but unparseable file is left for the user.
func (a geminiAgent) EnsureDefaults(cfg *config.Config, _ string) {
	dir := cfg.AgentDir(a.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "settings.json")
	data, _ := os.ReadFile(path)
	blank := strings.TrimSpace(string(data)) == ""
	m := map[string]any{}
	if !blank {
		if json.Unmarshal(data, &m) != nil {
			return // non-blank but unparseable — don't clobber it
		}
	}
	if disableGeminiFolderTrust(m) || blank {
		writeJSONFile(path, m, 0o644)
	}
}

// disableGeminiFolderTrust sets security.folderTrust.enabled=false unless the user
// already chose a value, reporting whether it changed m.
func disableGeminiFolderTrust(m map[string]any) bool {
	security, _ := m["security"].(map[string]any)
	if security == nil {
		security = map[string]any{}
		m["security"] = security
	}
	ft, _ := security["folderTrust"].(map[string]any)
	if ft == nil {
		ft = map[string]any{}
		security["folderTrust"] = ft
	}
	if _, ok := ft["enabled"]; ok {
		return false // user already chose — respect it
	}
	ft["enabled"] = false
	return true
}
