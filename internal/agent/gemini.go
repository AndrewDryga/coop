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
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type geminiAgent struct{}

func init() { register(geminiAgent{}) }

func (geminiAgent) Name() string        { return "gemini" }
func (geminiAgent) DisplayName() string { return "Gemini CLI" }
func (geminiAgent) Vendor() string      { return "Google" }

// Stream: gemini pairs tool_use with tool_result under `tool_id`, so its foreground tools are
// supervisable.
func (geminiAgent) Stream() StreamSpec {
	return StreamSpec{
		Format: StreamGeminiJSON, Flags: []string{"-o", "stream-json"}, TrailingArgs: 2,
		ToolLifecycle: ToolLifecycleIDs,
	}
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

// ACPFinalChunk: every assistant chunk is answer text — gemini's adapter streams no separate commentary phase.
func (geminiAgent) ACPFinalChunk(json.RawMessage) bool    { return true }
func (geminiAgent) ACPProgressChunk(json.RawMessage) bool { return false }

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
	geminiProjectRootLimit = 4 << 10
	geminiMetadataLimit    = 1 << 20
)

// geminiHasSession matches both the Coop-owned id and Gemini's native sha256(cwd) projectHash.
// Bucket names vary between Gemini releases, so scan every bucket whose .project_root owns ws.
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
		if geminiBucketCWD(root, bucket.Name()) != ws {
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
			if !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(chats, entry.Name())
			info, err := root.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			f, err := root.Open(path)
			if err != nil {
				continue
			}
			sessionID, projectHash := geminiSessionMetadata(io.LimitReader(f, geminiMetadataLimit))
			_ = f.Close()
			if sessionID == id && projectHash == wantProject {
				return true
			}
		}
	}
	return false
}

// geminiBucketCWD reads Gemini's bucket ownership marker. An absent or malformed marker is not
// authoritative and returns empty.
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

// geminiSessionMetadata decodes only the first JSONL record's two lookup keys. Callers bound that
// record because encoding/json buffers one top-level value even when the target is narrow.
func geminiSessionMetadata(r io.Reader) (sessionID, projectHash string) {
	dec := json.NewDecoder(r)
	var metadata struct {
		SessionID   string `json:"sessionId"`
		ProjectHash string `json:"projectHash"`
	}
	if err := dec.Decode(&metadata); err != nil {
		return "", ""
	}
	return metadata.SessionID, metadata.ProjectHash
}

func (geminiAgent) Login(*config.Config) []string { return []string{"gemini"} }

func (geminiAgent) LoginConfig(cfg *config.Config) (MCPConfig, error) {
	gm, _, err := mcp.GenerateGemini("", "")
	if err != nil {
		return MCPConfig{}, err
	}
	gm, err = ensureGeminiBoxDefaults(gm)
	if err != nil {
		return MCPConfig{}, err
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(gm), &settings); err != nil {
		return MCPConfig{}, err
	}
	// Gemini intersects this system allowlist with user/workspace lists. An explicitly empty
	// list disables all MCP; an empty mcpServers object would merely merge with existing servers.
	settings["mcp"] = map[string]any{"allowed": []string{}}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return MCPConfig{}, err
	}
	systemPath := cfg.HomeInBox + "/.coop-gemini-login.json"
	return MCPConfig{
		Mounts:      []MCPMount{{Content: string(append(data, '\n')), BoxPath: systemPath}},
		CommandArgs: []string{"--extensions", "none"},
		Env:         []string{"GEMINI_CLI_SYSTEM_SETTINGS_PATH=" + systemPath, "NO_BROWSER=true"},
	}, nil
}

func (geminiAgent) ConsultCmd(question string) []string {
	// -p takes the prompt as its value, so it must come last (right before the
	// question); otherwise -p swallows --approval-mode and gemini prints help.
	return []string{"gemini", "--approval-mode", "plan", "-p", question}
}

// RestrictedCommand: no restricted mode is qualified on this CLI yet (see unqualifiedRestrictedCommand).
func (a geminiAgent) RestrictedCommand(mode ExecutionMode, cmd []string) ([]string, error) {
	return unqualifiedRestrictedCommand(a, mode, cmd)
}

// ACPRestrictedSessionMeta: nor on its ACP adapter (see unqualifiedRestrictedACPSession).
func (a geminiAgent) ACPRestrictedSessionMeta(mode ExecutionMode) (map[string]any, error) {
	return unqualifiedRestrictedACPSession(a, mode)
}

// Packages is just the CLI: gemini's ACP mode is built in (gemini --acp).
const geminiCLIPackage = "@google/gemini-cli@latest"

func (geminiAgent) Packages() []string { return []string{geminiCLIPackage} }

// Models are common Gemini model ids. Illustrative — any id the CLI accepts works.
func (geminiAgent) Models() []string {
	return []string{"gemini-3.5-flash", "gemini-2.5-pro", "gemini-2.5-flash"}
}

// ExampleModel: the current default tier.
func (geminiAgent) ExampleModel() string { return "gemini-3.5-flash" }

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

func (geminiAgent) StoredCredentialStatus(string, time.Time) StoredCredentialStatus {
	return StoredCredentialUnknown
}

// MCP builds the settings mounted inside a gemini box: the host settings plus the box-only
// file-filtering override and the managed-client defaults (no auto-update, no update prompt,
// no usage statistics), and shared servers only when MCP is active. The host file is never
// written here; EnsureDefaults owns the one host-side change (folder trust).
func (geminiAgent) MCP(cfg *config.Config, _ string) (MCPConfig, error) {
	gm, requiredEnv, err := mcp.GenerateGemini(cfg.MCPFile, filepath.Join(cfg.AgentDir("gemini"), "settings.json"))
	if err != nil {
		return MCPConfig{}, err
	}
	gm, err = ensureGeminiBoxDefaults(gm)
	if err != nil {
		return MCPConfig{}, err
	}
	return MCPConfig{Mounts: []MCPMount{{Content: gm, BoxPath: cfg.HomeInBox + "/.gemini/settings.json"}}, RequiredEnv: requiredEnv}, nil
}

func ensureGeminiBoxDefaults(settingsJSON string) (string, error) {
	settings := map[string]any{}
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		return "", fmt.Errorf("assemble Gemini box defaults: %w", err)
	}
	if !disableGeminiFolderTrust(settings) {
		return settingsJSON, nil
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("assemble Gemini box defaults: %w", err)
	}
	return string(append(data, '\n')), nil
}

// ACPMCPServers is nil: this agent's ACP adapter reads the settings.json MCP mounts,
// so passing the servers again would register every one of them twice.
func (geminiAgent) ACPMCPServers(string, func(string) (string, bool)) ([]map[string]any, error) {
	return nil, nil
}

// EnsureDefaults guarantees a valid settings.json (an empty/missing one makes gemini
// fail at launch) and turns off its folder-trust prompt — the box is the sandbox. An
// existing choice is kept; a non-blank but unparseable file stops launch without being changed.
func (a geminiAgent) EnsureDefaults(cfg *config.Config, _ string) error {
	dir := cfg.AgentDir(a.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create Gemini defaults directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, "settings.json")
	m, blank, err := readJSONDefaults(path)
	if err != nil {
		return err
	}
	if disableGeminiFolderTrust(m) || blank {
		if err := writeJSONFile(path, m, 0o644); err != nil {
			return err
		}
	}
	return nil
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

// BoxEnv: gemini stores everything under its mounted ~/.gemini; the one variable is the
// managed-client telemetry switch (docs/cli/telemetry.md: `GEMINI_TELEMETRY_ENABLED` overrides
// `telemetry.enabled`, default false), pinned explicitly so a box is deterministic whatever the
// host settings say. The update and usage-statistics switches ride the generated settings
// (mcp.GenerateGemini). None of this qualifies gemini for filtered networking.
func (geminiAgent) BoxEnv(string) []string { return []string{"GEMINI_TELEMETRY_ENABLED=false"} }

func (geminiAgent) HomeFallbacks() []HomeFallback { return nil }

// Native 0.59.0 fresh/resume captures (2026-09-13): assistant message deltas are
// reply text, and result.stats.input_tokens already includes cached input.
const geminiConsultText = `gemini_text() {
	jq -ers 'select(all(.[]; type=="object"))
		| select(.[-1].type=="result" and .[-1].status=="success")
		| select([.[] | select(.type=="result")]|length==1)
		| select(all(.[]; .type!="error"))
		| [.[] | select(.type=="message" and .role=="assistant") | .content | select(type=="string")]
		| join("") | select(test("[^[:space:]]"))'
}
`

const geminiConsultUsage = `select(.[-1].type=="result" and .[-1].status=="success")
		| select([.[] | select(.type=="result")]|length==1)
		| .[-1].stats | select(type=="object")
		| {input:.input_tokens, output:.output_tokens}
		| select(.input|token) | select(.output|token)`

func (geminiAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$candidate_idfile\"\n" +
		`gemini_run gemini --approval-mode plan --session-id "$id" -o stream-json ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) ConsultResume() string {
	return `gemini_run gemini --approval-mode plan --resume "$id" -o stream-json ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) DelegateExec() string {
	return `gemini --yolo ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) ShellPrelude() string {
	return geminiConsultText + consultPeerRowShell("gemini", geminiConsultUsage) + consultCaptureShell("gemini", "Gemini")
}
func (geminiAgent) InstallScript() string { return "" }

// LockedClients is nil: gemini has no qualified locked client build yet, so
// restricted networking cannot launch it.
func (geminiAgent) LockedClients(ClientPlatform) []LockedClient { return nil }

func (a geminiAgent) NetworkBundle(NetworkBundleInput) (egress.Bundle, error) {
	return egress.Bundle{}, fmt.Errorf("%s is unsupported for restricted networking", a.Name())
}
