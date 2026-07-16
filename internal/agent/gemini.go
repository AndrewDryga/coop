package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// cwd could be the latest, but resuming an explicit uuid is immune. Metadata is matched across
// every project bucket because Gemini's bucket naming has changed between releases.
func (a geminiAgent) Resume(cfg *config.Config, ws, id string) ([]string, bool) {
	if ValidSessionID(id) && geminiHasSession(cfg, ws, id) {
		return append(a.base(cfg), "--resume", id), true
	}
	return a.Interactive(cfg), false
}

const (
	geminiProjectRootLimit    = 4 << 10
	geminiMetadataLimit       = 1 << 20
	geminiLegacyMetadataLimit = 4 << 20
)

// geminiHasSession matches both the Coop-owned id and Gemini's native sha256(cwd) projectHash.
// Current buckets carry a .project_root owner; markerless legacy buckets fall back to metadata.
func geminiHasSession(cfg *config.Config, ws, id string) bool {
	wantProject := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))
	root, err := openSessionRoot(filepath.Join(cfg.AgentDir("gemini"), "tmp"))
	if err != nil {
		return false
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return false
	}
	buckets, _ := dir.ReadDir(-1)
	_ = dir.Close()
	for _, bucket := range buckets {
		if !bucket.IsDir() || bucket.Type()&os.ModeSymlink != 0 {
			continue
		}
		if bucketCWD := geminiBucketCWD(root, bucket.Name()); bucketCWD != "" && bucketCWD != ws {
			continue
		}
		chats := filepath.Join(bucket.Name(), "chats")
		info, err := root.Lstat(chats)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		chatDir, err := root.Open(chats)
		if err != nil {
			continue
		}
		files, _ := chatDir.ReadDir(-1)
		_ = chatDir.Close()
		for _, entry := range files {
			path := filepath.Join(chats, entry.Name())
			info, err := root.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			f, err := root.Open(path)
			if err != nil {
				continue
			}
			legacy := strings.HasSuffix(entry.Name(), ".json")
			limit := int64(geminiMetadataLimit)
			if legacy {
				limit = geminiLegacyMetadataLimit
				if info.Size() > limit {
					_ = f.Close()
					continue
				}
			}
			sessionID, projectHash := geminiSessionMetadata(io.LimitReader(f, limit), legacy)
			_ = f.Close()
			// Some whole-file legacy records omit projectHash. Accept those only from the old
			// exact sha256(cwd) bucket; a slug without metadata cannot prove cwd safely.
			if sessionID == id && (projectHash == wantProject || (projectHash == "" && bucket.Name() == wantProject)) {
				return true
			}
		}
	}
	return false
}

// geminiBucketCWD reads Gemini's current bucket ownership marker. An absent or malformed marker
// returns empty so older bucket schemes retain the metadata fallback above.
func geminiBucketCWD(root *os.Root, bucket string) string {
	path := filepath.Join(bucket, ".project_root")
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > geminiProjectRootLimit {
		return ""
	}
	f, err := root.Open(path)
	if err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, geminiProjectRootLimit+1))
	_ = f.Close()
	if err != nil || len(data) > geminiProjectRootLimit {
		return ""
	}
	cwd := strings.TrimSpace(string(data))
	if !filepath.IsAbs(cwd) {
		return ""
	}
	return cwd
}

// geminiSessionMetadata decodes only the two top-level keys needed for lookup. Callers cap the
// complete record because encoding/json buffers one top-level value even when the target is narrow.
func geminiSessionMetadata(r io.Reader, wholeRecord bool) (sessionID, projectHash string) {
	dec := json.NewDecoder(r)
	var metadata struct {
		SessionID   string `json:"sessionId"`
		ProjectHash string `json:"projectHash"`
	}
	if err := dec.Decode(&metadata); err != nil {
		return "", ""
	}
	if wholeRecord {
		if _, err := dec.Token(); err != io.EOF {
			return "", ""
		}
	}
	return metadata.SessionID, metadata.ProjectHash
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

// Effort/EffortEnv: the Gemini CLI exposes no reasoning-effort control, so a target that
// names one is rejected in ParseTarget (SupportsEffort is false for gemini).
func (geminiAgent) Effort() EffortSpec { return EffortSpec{} }
func (geminiAgent) EffortEnv() string  { return "" }

func (geminiAgent) InstructionFile() string { return "GEMINI.md" }

func (geminiAgent) NativeSubagents() NativeSubagentSupport { return NativeSubagentSupport{} }

func (geminiAgent) AuthMarker() (file, envKey string) {
	return "gemini-credentials.json", "GEMINI_API_KEY"
}

// CredentialEnvKeys lists every env var the Gemini CLI reads a key from: GEMINI_API_KEY
// and the GOOGLE_API_KEY it also honors.
func (geminiAgent) CredentialEnvKeys() []string {
	return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
}

func (geminiAgent) LiveCredentials() LiveCredentialSpec {
	return LiveCredentialSpec{
		Artifacts: []CredentialArtifact{
			// Gemini's keychain is encrypted from host identity and cannot be made portable. Retain
			// it in the integrity allowlist, but return nil so it is never mounted in a live box.
			{Name: "gemini-credentials.json", Primary: true, Project: func([]byte) ([]byte, error) { return nil, nil }},
			{Name: "google_accounts.json", Project: func([]byte) ([]byte, error) { return nil, nil }},
			{Name: "settings.json", Project: func(data []byte) ([]byte, error) {
				return projectJSONLeaf(data, "security", "auth", "selectedType")
			}},
		},
		Portability: func(string, time.Time) CredentialPortability { return CredentialNotPortable },
		AuthSignals: []string{"manual authorization is required", "authentication required", "must specify the gemini_api_key"},
	}
}

// ActiveCredentialEnvKeys grants exactly the key family selected in settings.json. Without a
// marker or selector, Gemini may auto-detect either supported key; a marker without a selector is
// file-backed and receives no env authority.
func (a geminiAgent) ActiveCredentialEnvKeys(profileDir string, markerPresent bool) []string {
	data, err := os.ReadFile(filepath.Join(profileDir, "settings.json"))
	if err != nil {
		if markerPresent {
			return nil
		}
		return a.CredentialEnvKeys()
	}
	var settings struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	switch settings.Security.Auth.SelectedType {
	case "gemini-api-key":
		return []string{"GEMINI_API_KEY"}
	case "vertex-ai":
		return []string{"GOOGLE_API_KEY"}
	}
	return nil
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
	return "printf '%s' \"$id\" >\"$candidate_idfile\"\n" +
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
