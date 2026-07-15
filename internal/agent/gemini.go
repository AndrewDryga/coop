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

func (geminiAgent) Name() string        { return "gemini" }
func (geminiAgent) DisplayName() string { return "Gemini CLI" }
func (geminiAgent) Stream() StreamSpec {
	return StreamSpec{Format: StreamGeminiJSON, Flags: []string{"-o", "stream-json"}, TrailingArgs: 2}
}

func (geminiAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_GEMINI_CMD", "gemini --yolo")
	if len(b) == 0 { // match codex's guard: an empty override must still yield a runnable command
		b = []string{"gemini"}
	}
	return withModel(b, cfg.ModelFor("gemini"))
}

func (a geminiAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a geminiAgent) Headless(cfg *config.Config, prompt string) []string {
	return append(a.base(cfg), "-p", prompt)
}

// ACP is gemini's own binary, so the resolved model rides along as its normal --model flag.
func (geminiAgent) ACP(cfg *config.Config) []string {
	return withModel([]string{"gemini", "--acp"}, cfg.ModelFor("gemini"))
}

// ACPSessionDirs: gemini stores chats under ~/.gemini/tmp/<bucket>/chats (best-effort).
func (geminiAgent) ACPSessionDirs() []string { return []string{"tmp"} }

func (geminiAgent) PresetSessionID() bool { return true }

func (a geminiAgent) StartSession(cfg *config.Config, id string) []string {
	if id == "" {
		return a.Interactive(cfg)
	}
	return append(a.base(cfg), "--session-id", id)
}

// Resume pins the coop-owned session id rather than "latest" — a loop or consult in the same
// cwd could be the latest, but resuming an explicit uuid is immune. It's matched by file
// content across every project bucket, so neither gemini's bucket-naming scheme nor a
// same-named fork in another repo can break or misdirect detection.
func (a geminiAgent) Resume(cfg *config.Config, ws, id string) ([]string, bool) {
	if id != "" && geminiHasSession(cfg, id) {
		return append(a.base(cfg), "--resume", id), true
	}
	return a.Interactive(cfg), false
}

// geminiHasSession reports whether any chats file records session id. gemini stores chats under
// ~/.gemini/tmp/<bucket>/chats where <bucket> is a version-dependent encoding of the project path
// (a slug now, a hash in older versions, with a collision suffix) — so rather than reconstruct it
// (and silently miss when it changes), scan every bucket and match the coop-owned id, a unique
// uuid that appears only in its own session.
func geminiHasSession(cfg *config.Config, id string) bool {
	files, _ := filepath.Glob(filepath.Join(cfg.AgentDir("gemini"), "tmp", "*", "chats", "*"))
	for _, f := range files {
		if data, err := os.ReadFile(f); err == nil && strings.Contains(string(data), id) {
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

// Models are common Gemini model ids. Illustrative — any id the CLI accepts works.
func (geminiAgent) Models() []string {
	return []string{"gemini-3.5-flash", "gemini-2.5-pro", "gemini-2.5-flash"}
}

// ModelEnv: the Gemini CLI reads its default model from GEMINI_MODEL; the flag in base()
// covers coop-driven runs, this covers anything that takes no flags.
func (geminiAgent) ModelEnv() string { return "GEMINI_MODEL" }

// EffortFlag/EffortEnv: the Gemini CLI exposes no reasoning-effort control, so a target that
// names one is rejected in ParseTarget (SupportsEffort is false for gemini).
func (geminiAgent) EffortFlag(string) []string { return nil }
func (geminiAgent) EffortEnv() string          { return "" }

func (geminiAgent) InstructionFile() string { return "GEMINI.md" }

func (geminiAgent) AuthMarker() (file, envKey string) {
	return "gemini-credentials.json", "GEMINI_API_KEY"
}

// CredentialEnvKeys lists every env var the Gemini CLI reads a key from: GEMINI_API_KEY
// and the GOOGLE_API_KEY it also honors.
func (geminiAgent) CredentialEnvKeys() []string {
	return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
}

// MCP builds the settings mounted inside a gemini box: the host settings plus the
// box-only file-filtering override, and shared servers only when MCP is active.
func (geminiAgent) MCP(cfg *config.Config) ([]MCPMount, error) {
	mcpFile := ""
	if cfg.MCPActive() {
		mcpFile = cfg.MCPFile
	}
	gm, err := mcp.GenerateGemini(mcpFile, filepath.Join(cfg.AgentDir("gemini"), "settings.json"))
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

// ACPRateLimitSignals: gemini surfaces a quota hit as the Google API status
// RESOURCE_EXHAUSTED; the value alone is the proof, whatever key carries it.
func (geminiAgent) ACPRateLimitSignals() []ACPSignal {
	return []ACPSignal{{Value: "RESOURCE_EXHAUSTED"}}
}

// ACPSessionSettings: Gemini changes models through session/set_model rather than
// session/set_config_option. Its ACP command also carries the model at launch.
func (geminiAgent) ACPSessionSettings(target Target) []ACPSessionSetting {
	if target.Model == "" {
		return nil
	}
	return []ACPSessionSetting{{Method: ACPSetModel, Value: target.Model}}
}

// BoxEnv: gemini stores everything under its mounted ~/.gemini — nothing extra needed.
func (geminiAgent) BoxEnv(string) []string { return nil }

func (geminiAgent) HomeFallbacks() []HomeFallback { return nil }

func (geminiAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$idfile\"\n" +
		`run gemini --approval-mode plan --session-id "$id" ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) ConsultResume() string {
	return `run gemini --approval-mode plan --resume "$id" ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) DelegateExec() string {
	return `gemini --yolo ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) ShellPrelude() string  { return "" }
func (geminiAgent) InstallScript() string { return "" }
