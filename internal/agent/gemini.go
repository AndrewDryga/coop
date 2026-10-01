package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type geminiAgent struct{}

func (geminiAgent) Usage() UsageSpec {
	return UsageSpec{Quota: geminiUsageQuota, HistoryDirs: []string{"tmp"},
		HistoryFile: func(path string) bool {
			return strings.Contains(path, "/chats/") && (strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".json"))
		}, ParseHistory: geminiUsageHistory, Price: geminiUsagePrice,
		NativeCredentialLease: true, NativeCredentialCheck: geminiUsageCredentialCheck,
		NativeEnv: []string{"NO_BROWSER=true", "GEMINI_FORCE_ENCRYPTED_FILE_STORAGE=false"}}
}

func geminiUsageCredentialCheck(profile string) error {
	data, err := ReadCredentialArtifact(filepath.Join(profile, "oauth_creds.json"), 1<<20)
	if os.IsNotExist(err) {
		return ErrUsageSignIn
	}
	var credential struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if err != nil || json.Unmarshal(data, &credential) != nil || credential.Access == "" && credential.Refresh == "" {
		return fmt.Errorf("limits require the original plain-file OAuth store; encrypted/keychain storage is not portable")
	}
	return nil
}

// Standard text-token rates, https://ai.google.dev/gemini-api/docs/pricing — 2026-10-01.
// Native history does not split audio input; exclude media/tool/storage surcharges from this value.
// Flash 3.6–3.8 use the published promotion through 2026-12-31, not their future tariffs.
func geminiUsagePrice(event UsageEvent) (float64, bool) {
	rates := map[string]usageTariff{
		"gemini-3.8-flash":                   {Input: .75, Read: .075, Output: 3.75},
		"gemini-3.7-flash":                   {Input: .75, Read: .075, Output: 3.75},
		"gemini-3.6-flash":                   {Input: .75, Read: .075, Output: 3.75},
		"gemini-3.5-flash":                   {Input: 1.5, Read: .15, Output: 9},
		"gemini-3.5-flash-lite":              {Input: .3, Read: .03, Output: 2.5},
		"gemini-3.1-pro-preview":             {Input: 2, Read: .2, Output: 12, LongAt: 200001, LongInput: 2, LongOutput: 1.5},
		"gemini-3.1-pro-preview-customtools": {Input: 2, Read: .2, Output: 12, LongAt: 200001, LongInput: 2, LongOutput: 1.5},
		"gemini-2.5-pro":                     {Input: 1.25, Read: .125, Output: 10, LongAt: 200001, LongInput: 2, LongOutput: 1.5},
		"gemini-2.5-flash":                   {Input: .3, Read: .03, Output: 2.5},
	}
	rate, ok := rates[event.Model]
	if !ok {
		return 0, false
	}
	return rate.value(event)
}

type geminiUsageMessage struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Model     string `json:"model"`
	Tokens    *struct {
		Input    *int64 `json:"input"`
		Read     *int64 `json:"cached"`
		Output   *int64 `json:"output"`
		Thoughts *int64 `json:"thoughts"`
		Tool     *int64 `json:"tool"`
	} `json:"tokens"`
}

func geminiUsageHistory(reader io.Reader) (UsageHistory, error) {
	// Failed/auxiliary calls are not all retained by the native recorder.
	out := UsageHistory{Partial: true}
	events := make(map[string]UsageEvent)
	session := ""
	add := func(row geminiUsageMessage) {
		if row.Type != "gemini" || row.Tokens == nil {
			return
		}
		u := row.Tokens
		event := UsageEvent{ID: session + ":" + row.ID, Model: row.Model, Time: usageReset(row.Timestamp), WriteKnown: true,
			Input: usageTokens(u.Input) - usageTokens(u.Read), Read: usageTokens(u.Read), Output: usageTokens(u.Output) + usageTokens(u.Thoughts),
			Partial: u.Input == nil || u.Output == nil || usageTokens(u.Tool) != 0, Approximate: true, ContextKnown: true}
		if session == "" || row.ID == "" || !event.valid() {
			return
		}
		prior, exists := events[event.ID]
		if !exists || event.Time.After(prior.Time) || event.Time.Equal(prior.Time) {
			events[event.ID] = event
		}
	}
	decoder := json.NewDecoder(reader)
	for {
		var row struct {
			geminiUsageMessage
			Session  string               `json:"sessionId"`
			Messages []geminiUsageMessage `json:"messages"`
			Set      struct {
				Messages []geminiUsageMessage `json:"messages"`
			} `json:"$set"`
		}
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if row.Session != "" {
			session = row.Session
		}
		add(row.geminiUsageMessage)
		for _, message := range row.Messages {
			add(message)
		}
		for _, message := range row.Set.Messages {
			add(message)
		}
		// $rewindTo changes visible conversation history, not already consumed tokens.
	}
	for _, event := range events {
		out.Events = append(out.Events, event)
	}
	return out, nil
}

func geminiUsageQuota(ctx context.Context, input UsageQuotaInput) (UsageQuota, error) {
	auth, _, err := geminiSelectedAuthType(input.ProfileDir)
	if err != nil {
		return UsageQuota{}, fmt.Errorf("cannot read selected authentication mode")
	}
	if input.APIKey || auth == "gemini-api-key" || auth == "vertex-ai" {
		return UsageQuota{Note: "Code Assist limits unavailable for API-key or Vertex authentication"}, nil
	}
	if auth != "oauth-personal" {
		return UsageQuota{}, ErrUsageSignIn
	}
	if input.Native == nil {
		return UsageQuota{}, fmt.Errorf("native quota helper unavailable")
	}
	data, err := input.Native(ctx, []string{"/usr/local/bin/node", "--input-type=module", "-e", geminiQuotaHelper})
	if err != nil {
		return UsageQuota{}, err
	}
	var raw geminiQuotaResponse
	if len(data) > 1<<20 || json.Unmarshal(data, &raw) != nil || raw.Error != "" {
		return UsageQuota{}, fmt.Errorf("native quota lookup unavailable")
	}
	return raw.quota(), nil
}

// Import only the native auth/server exports: CLI/model initialization can load hooks, MCP
// servers or perform onboarding. Let the process drain asynchronous native token persistence.
const geminiQuotaHelper = `import {getOauthClient,CodeAssistServer} from "/opt/coop/clients/node_modules/@google/gemini-cli/bundle/core-BXNHRQQE.js";
process.on("unhandledRejection",()=>{process.exitCode=1});
try {
  const config={getProxy:()=>undefined,isBrowserLaunchSuppressed:()=>true,isInteractive:()=>false};
  const client=await getOauthClient("oauth-personal",config);
  const server=new CodeAssistServer(client,undefined,{});
  const info=await server.loadCodeAssist({metadata:{ideType:"IDE_UNSPECIFIED",platform:"PLATFORM_UNSPECIFIED",pluginType:"GEMINI"}});
  const project=info.cloudaicompanionProject;
  if(!info.currentTier||typeof project!=="string"||!project) throw new Error();
  const quota=await server.retrieveUserQuota({project});
  console.log(JSON.stringify({tier:info.paidTier?.name??info.currentTier.name,buckets:quota.buckets?.map(b=>({modelId:b.modelId,tokenType:b.tokenType,remainingAmount:b.remainingAmount,remainingFraction:b.remainingFraction,resetTime:b.resetTime}))}));
} catch {process.exitCode=1;console.log(JSON.stringify({error:"quota lookup unavailable"}));}`

type geminiQuotaResponse struct {
	Tier    string `json:"tier"`
	Error   string `json:"error"`
	Buckets []struct {
		Model     string   `json:"modelId"`
		TokenType string   `json:"tokenType"`
		Remaining *string  `json:"remainingAmount"`
		Fraction  *float64 `json:"remainingFraction"`
		Reset     string   `json:"resetTime"`
	} `json:"buckets"`
}

func (raw geminiQuotaResponse) quota() UsageQuota {
	out := UsageQuota{Plan: raw.Tier}
	for _, entry := range raw.Buckets {
		name := entry.Model
		if name == "" {
			name = "Code Assist"
		}
		if entry.TokenType != "" {
			name += " · " + entry.TokenType
		}
		bucket := UsageBucket{Name: name, Reset: usageReset(entry.Reset)}
		if entry.Fraction != nil && *entry.Fraction >= 0 && *entry.Fraction <= 1 {
			used := (1 - *entry.Fraction) * 100
			bucket.Used = &used
		}
		if entry.Remaining != nil {
			bucket.Remaining = *entry.Remaining
		}
		out.Buckets = append(out.Buckets, bucket)
	}
	return out
}

func (geminiAgent) Scaffold() ScaffoldSpec {
	return ScaffoldSpec{
		Project: ScaffoldLayout{Dir: ".gemini"},
		SelectedIgnore: []string{
			"# .gemini may be globally ignored (local Gemini state); keep just the skills symlink",
			"!.gemini/", ".gemini/*", "!.gemini/skills",
		},
	}
}

func (geminiAgent) ModelCatalog() ModelCatalogSpec {
	return ModelCatalogSpec{ParseACP: func(raw json.RawMessage) []Model {
		var doc acpModelCatalog
		if json.Unmarshal(raw, &doc) != nil {
			return nil
		}
		return ParseACPAvailableModels(doc.Models)
	}}
}

func (geminiAgent) ReviewOutput(raw string, _ ReviewOutputContract) (string, bool) { return raw, true }
func (geminiAgent) ReviewFooterLine(string) bool                                   { return false }
func (geminiAgent) PlainOutputProbe() PlainOutputProbe                             { return nil }

func init() { register(geminiAgent{}) }

func (geminiAgent) Name() string        { return "gemini" }
func (geminiAgent) SkillsCapable() bool { return true }
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

func (a geminiAgent) HeadlessSession(cfg *config.Config, prompt, id string, resume bool) ([]string, bool) {
	if !ValidSessionID(id) {
		return nil, false
	}
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	return append(a.base(cfg), flag, id, "-p", prompt), true
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
	found := scanSessionDir(dir, func(bucket os.DirEntry) bool {
		if !bucket.IsDir() || bucket.Type()&os.ModeSymlink != 0 {
			return false
		}
		if geminiBucketCWD(root, bucket.Name()) != ws {
			return false
		}
		chats := filepath.Join(bucket.Name(), "chats")
		info, err := root.Lstat(chats)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		chatDir, err := root.Open(chats)
		if err != nil {
			return false
		}
		matched := scanSessionDir(chatDir, func(entry os.DirEntry) bool {
			if !strings.HasSuffix(entry.Name(), ".jsonl") {
				return false
			}
			path := filepath.Join(chats, entry.Name())
			info, err := root.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return false
			}
			f, err := root.Open(path)
			if err != nil {
				return false
			}
			sessionID, projectHash := geminiSessionMetadata(io.LimitReader(f, geminiMetadataLimit))
			_ = f.Close()
			return sessionID == id && projectHash == wantProject
		})
		_ = chatDir.Close()
		return matched
	})
	_ = dir.Close()
	return found
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

func (geminiAgent) LoginConfig(*config.Config) (MCPConfig, error) {
	return MCPConfig{
		CommandArgs: []string{"--extensions", "none"},
		Env:         []string{"GEMINI_CLI_SYSTEM_SETTINGS_PATH=/etc/gemini-cli/login.json", "NO_BROWSER=true"},
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

// geminiNoUpdates is Gemini's update switch, a system-settings block. /etc/gemini-cli/settings.json
// is the pinned 0.62.0's system layer, which overrides both the user's and a workspace's settings
// (it reports a bad file there by path). Pointing GEMINI_CLI_SYSTEM_SETTINGS_PATH at another file
// replaces that layer instead of merging with it, so every system file Coop writes repeats the block.
var geminiNoUpdates = map[string]any{"enableAutoUpdate": false, "enableAutoUpdateNotification": false}

func (geminiAgent) UpdateControls() UpdateControls {
	// Gemini 0.62 requires every system file and ancestor to be root-owned and non-writable
	// by others. Image-owned files satisfy that; host-generated mounts below the user home do not.
	files := []SystemFile{
		{Path: "/etc/gemini-cli/settings.json", Content: geminiSystemSettings(map[string]any{"general": geminiNoUpdates})},
		// An empty system allowlist disables MCP, rather than merging with native servers.
		{Path: "/etc/gemini-cli/login.json", Content: geminiSystemSettings(map[string]any{
			"general": geminiNoUpdates, "mcp": map[string]any{"allowed": []string{}},
			"context":  map[string]any{"fileFiltering": map[string]any{"respectGitIgnore": false}},
			"privacy":  map[string]any{"usageStatisticsEnabled": false},
			"security": map[string]any{"folderTrust": map[string]any{"enabled": false}},
		})},
	}
	for _, effort := range []string{"low", "high"} {
		files = append(files, SystemFile{Path: geminiThinkingDir + "/" + effort + ".json", Content: geminiThinkingSettings(effort)})
	}
	return UpdateControls{Files: files}
}

// Models are common Gemini model ids. Illustrative — any id the CLI accepts works.
func (geminiAgent) Models() []string {
	return []string{"gemini-3.5-flash", "gemini-2.5-pro", "gemini-2.5-flash"}
}

// ExampleModel: the current default tier.
func (geminiAgent) ExampleModel() string { return "gemini-3.5-flash" }

// ModelEnv: the Gemini CLI reads its default model from GEMINI_MODEL; the flag in base()
// covers coop-driven runs, this covers anything that takes no flags.
func (geminiAgent) ModelEnv() string { return "GEMINI_MODEL" }

// Effort/EffortEnv: the Gemini CLI has no reasoning-effort flag or environment variable; it takes
// thinking from image-owned settings, which MCP selects (see geminiThinkingWiring).
func (geminiAgent) Effort() EffortSpec {
	return EffortSpec{Settings: true, Validate: validateGeminiEffort}
}
func (geminiAgent) EffortEnv() string { return "" }

func (geminiAgent) InstructionFile() string { return "GEMINI.md" }

// NativeSubagents: Gemini CLI 0.59 loads local subagents from ~/.gemini/agents/*.md. The format has no
// reasoning effort, so a native Gemini role cannot set one.
func (geminiAgent) NativeSubagents() NativeSubagentSupport {
	return NativeSubagentSupport{HomeDir: ".gemini/agents", Render: renderGeminiSubagent}
}

func renderGeminiSubagent(role NativeSubagent) (filename, content string) {
	return role.Name + ".md", nativeMarkdown(role.Prompt, [2]string{"name", role.Name},
		[2]string{"description", role.Description}, [2]string{"kind", "local"}, [2]string{"model", role.Model})
}

func (geminiAgent) AuthMarker() (file, envKey string) {
	return "gemini-credentials.json", "GEMINI_API_KEY"
}

func (geminiAgent) HostCredential() HostCredentialSpec {
	return HostCredentialSpec{
		Prompt:       "Gemini API key (input hidden): ",
		Instructions: "Create or copy a key at https://aistudio.google.com/apikey",
		File:         "api-key",
		EnvKey:       "GEMINI_API_KEY",
		Activate:     selectGeminiAPIKey,
	}
}

// selectGeminiAPIKey changes only Gemini's selected auth family. The user's other settings and a
// native encrypted credential file are left alone; malformed or linked settings fail closed.
func selectGeminiAPIKey(profileDir string) error {
	path := filepath.Join(profileDir, "settings.json")
	settings, _, err := readJSONDefaults(path)
	if err != nil {
		return fmt.Errorf("read Gemini settings before saving API key: %w", err)
	}
	security, _ := settings["security"].(map[string]any)
	if security == nil {
		security = map[string]any{}
		settings["security"] = security
	}
	auth, _ := security["auth"].(map[string]any)
	if auth == nil {
		auth = map[string]any{}
		security["auth"] = auth
	}
	auth["selectedType"] = "gemini-api-key"
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Gemini settings after selecting API key: %w", err)
	}
	return config.WriteFileAtomic(path, append(encoded, '\n'))
}

// CredentialEnvKeys lists every env var the Gemini CLI reads a key from: GEMINI_API_KEY
// and the GOOGLE_API_KEY it also honors.
func (geminiAgent) CredentialEnvKeys() []string {
	return []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}
}

func (geminiAgent) CredentialBroker() CredentialBrokerSpec {
	return CredentialBrokerSpec{
		CredentialEnv: "GEMINI_API_KEY",
		BaseURLEnv:    "GOOGLE_GEMINI_BASE_URL",
		Upstream:      "generativelanguage.googleapis.com",
		Header:        "x-goog-api-key",
		Method:        "POST",
		Path:          "/v1beta/models/",
		PathPrefix:    true,
		AllowQuery:    true,
		Port:          443,
	}
}

func (geminiAgent) StoredAPIKey(profileDir string) (bool, error) {
	selected, ok, err := geminiSelectedAuthType(profileDir)
	return ok && selected == "gemini-api-key", err
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
		// A rejected key comes back as the service's own error inside the pinned CLI's failed result:
		// "[API Error: {… API key not valid …}]".
		AuthSignals: []string{"manual authorization is required", "authentication required", "must specify the gemini_api_key",
			"api key not valid"},
	}
}

// ActiveCredentialEnvKeys grants exactly the key family selected in settings.json. Without a
// marker or selector, Gemini may auto-detect either supported key; a marker without a selector is
// file-backed and receives no env authority.
func (a geminiAgent) ActiveCredentialEnvKeys(profileDir string, markerPresent bool) []string {
	selectedType, ok, err := geminiSelectedAuthType(profileDir)
	if err != nil {
		return nil
	}
	if !ok {
		if markerPresent {
			return nil
		}
		return a.CredentialEnvKeys()
	}
	switch selectedType {
	case "gemini-api-key":
		return []string{"GEMINI_API_KEY"}
	case "vertex-ai":
		return []string{"GOOGLE_API_KEY"}
	}
	return nil
}

// MarkerProvidesSelectedCredential recognizes only Gemini's native OAuth store. Its encrypted
// API-key store is bound to one container hostname and is therefore not runnable in a fresh box;
// Coop's host credential is the portable API-key authority.
func (geminiAgent) MarkerProvidesSelectedCredential(profileDir string) bool {
	selectedType, ok, err := geminiSelectedAuthType(profileDir)
	return err == nil && ok && selectedType == "oauth-personal"
}

func geminiSelectedAuthType(profileDir string) (string, bool, error) {
	data, present, err := readDefaultsFile(filepath.Join(profileDir, "settings.json"))
	if err != nil {
		return "", false, err
	}
	if !present {
		return "", false, nil
	}
	var settings struct {
		Security struct {
			Auth struct {
				SelectedType string `json:"selectedType"`
			} `json:"auth"`
		} `json:"security"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", false, err
	}
	return settings.Security.Auth.SelectedType, true, nil
}

func (geminiAgent) StoredCredentialStatus(profileDir string, _ time.Time) StoredCredentialStatus {
	selectedType, ok, err := geminiSelectedAuthType(profileDir)
	if err != nil || !ok {
		return StoredCredentialReauthRequired
	}
	if selectedType == "gemini-api-key" {
		return StoredCredentialReauthRequired
	}
	return StoredCredentialUnknown
}

// MCP builds the settings mounted inside a gemini box: the host settings plus the box-only
// file-filtering override and the managed-client defaults (no auto-update, no update prompt,
// no usage statistics), and shared servers only when MCP is active. The per-effort system files
// come from the image. The host file is never written here; EnsureDefaults owns the one
// host-side change (folder trust).
func (geminiAgent) MCP(cfg *config.Config, _ string) (MCPConfig, error) {
	gm, requiredEnv, err := mcp.GenerateGemini(cfg.MCPFile, filepath.Join(cfg.AgentDir("gemini"), "settings.json"))
	if err != nil {
		return MCPConfig{}, err
	}
	gm, err = ensureGeminiBoxDefaults(gm, cfg.EvalDisableWebTools)
	if err != nil {
		return MCPConfig{}, err
	}
	env, err := geminiThinkingWiring(cfg)
	if err != nil {
		return MCPConfig{}, err
	}
	mounts := []MCPMount{{Content: gm, BoxPath: cfg.HomeInBox + "/.gemini/settings.json"}}
	return MCPConfig{Mounts: mounts, Env: env, RequiredEnv: requiredEnv}, nil
}

func ensureGeminiBoxDefaults(settingsJSON string, disableWebTools bool) (string, error) {
	settings := map[string]any{}
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		return "", fmt.Errorf("assemble Gemini box defaults: %w", err)
	}
	changed := disableGeminiFolderTrust(settings)
	if disableWebTools {
		tools, ok := settings["tools"].(map[string]any)
		if settings["tools"] != nil && !ok {
			return "", fmt.Errorf("gemini tools settings must be an object")
		}
		if tools == nil {
			tools = map[string]any{}
			settings["tools"] = tools
		}
		exclude, ok := tools["exclude"].([]any)
		if tools["exclude"] != nil && !ok {
			return "", fmt.Errorf("gemini tools.exclude must be an array")
		}
		for _, tool := range []string{"google_web_search", "web_fetch"} {
			if !slices.Contains(exclude, any(tool)) {
				exclude = append(exclude, tool)
			}
		}
		tools["exclude"] = exclude
		changed = true
	}
	if !changed {
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

func (geminiAgent) DefaultsPublication(*config.Config) ([]ConfigPublication, error) { return nil, nil }

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
gemini_delegate_text() {
	jq --unbuffered -jr '
		if .type=="message" and .role=="assistant" and (.content|type)=="string" then .content
		elif .type=="error" and (.message|type)=="string" then .message, "\n"
		elif .type=="result" then "\n"
		else empty end'
}
gemini_errors() {
	jq -r 'def text: strings | select(test("[^[:space:]]"));
		def message: (.message|text) // (.error|text) // (.error|objects|.message|text);
		select(type=="object")
		| if .type=="error" then (message // "error")
		  elif .type=="result" and .status!="success" then (message // (.status|text))
		  else empty end'
}
`

const geminiConsultUsage = `select(.[-1].type=="result" and .[-1].status=="success")
		| select([.[] | select(.type=="result")]|length==1)
		| .[-1].stats | select(type=="object")
		| {input:.input_tokens, fresh:.input, read:.cached, output:.output_tokens, duration:.duration_ms}
		| select(.input|token) | select(.output|token)
		| if (.fresh|token) then . else del(.fresh) end
		| if (.read|token) then . else del(.read) end
		| if (.duration|token) then . else del(.duration) end`

// geminiEffortEnv hands a consult or delegate call the thinking settings for its own $effort (see
// geminiThinkingWiring). Without one the call keeps the box's.
const geminiEffortEnv = `env ${effort:+GEMINI_CLI_SYSTEM_SETTINGS_PATH="$` + geminiThinkingEnv + `/$effort.json"} `

func (geminiAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$candidate_idfile\"\n" +
		`gemini_run ` + geminiEffortEnv + `gemini --approval-mode plan --session-id "$id" -o stream-json ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) ConsultResume() string {
	return `gemini_run ` + geminiEffortEnv + `gemini --approval-mode plan --resume "$id" -o stream-json ${model:+--model "$model"} -p "$prompt"`
}

func (geminiAgent) DelegateExec() string {
	return geminiEffortEnv + `gemini --yolo ${model:+--model "$model"} -o stream-json -p "$prompt"`
}

func (geminiAgent) UsagePrelude() string {
	return geminiConsultText + consultPeerRowShell("gemini", geminiConsultUsage)
}
func (a geminiAgent) ShellPrelude() string {
	return a.UsagePrelude() + consultCaptureShell("gemini", "Gemini")
}

// LockedClients pins Gemini's bundled CLI once while recording both ways Coop
// launches it. The npm tarball is integrity-locked by package-lock.json; the
// bundle is the executable entry point for ordinary and ACP sessions alike.
func (geminiAgent) LockedClients(platform ClientPlatform) []LockedClient {
	if !platform.valid() {
		return nil
	}
	client := LockedClient{Package: "@google/gemini-cli", Version: "0.62.0", Binary: "gemini",
		Exec:                []string{"/usr/local/bin/node", lockedClientRoot + "/node_modules/@google/gemini-cli/bundle/gemini.js"},
		RequiredExecutables: []LockedExecutable{{Path: lockedClientRoot + "/node_modules/@google/gemini-cli/bundle/gemini.js", Version: "0.62.0"}}}
	cli, acp := client, client
	cli.Client, acp.Client = egress.ClientCLI, egress.ClientACP
	return []LockedClient{cli, acp}
}

func (a geminiAgent) NetworkBundle(input NetworkBundleInput) (egress.Bundle, error) {
	return directNetworkBundle(a.Name(), "api-key", input,
		[]string{"generativelanguage.googleapis.com"},
		[]string{"https://github.com/google-gemini/gemini-cli/blob/v0.62.0/packages/core/src/core/contentGenerator.ts"})
}

// NetworkAuthSelection refuses native OAuth and Vertex until each has its own
// portable authority and endpoint trace. A saved or env-backed AI Studio key is
// the one account family qualified by this release.
func (geminiAgent) NetworkAuthSelection(profileDir string, markerPresent bool) (NetworkAuthSelection, error) {
	selected, ok, err := geminiSelectedAuthType(profileDir)
	if err != nil {
		return NetworkAuthSelection{}, fmt.Errorf("read Gemini authentication selection: %w", err)
	}
	if ok && selected != "gemini-api-key" {
		return NetworkAuthSelection{}, fmt.Errorf("gemini authentication %q is unsupported for restricted networking", selected)
	}
	if markerPresent && !ok {
		return NetworkAuthSelection{}, fmt.Errorf("gemini stored authentication is unsupported for restricted networking")
	}
	return NetworkAuthSelection{AuthMode: "api-key", EnvKey: "GEMINI_API_KEY"}, nil
}

// Gemini has no reasoning-effort flag or variable: the pinned client (0.62.0) takes thinking only
// from its settings. What it does with them was captured at the request level, against a local
// listener (.agent/kb/gemini-effort-thinking-settings.md), and two facts decide the shape here.
//
// Every chat model's settings chain runs through one family base — chat-base-3 for Gemini 3 and
// Gemma, whose thinkingLevel has only LOW and HIGH; chat-base-2.5 for Gemini 2.5, whose
// thinkingBudget is a token count. The model named at launch does not decide which one applies:
// the client remaps names (gemini-3-pro-preview is sent as gemini-3.1-pro-preview, and a 2.5 flash
// request goes to the current GA flash on an API key), routes auto, and falls back on quota. A setting
// keyed on the typed name is silently a no-op; a setting on both bases reaches whichever model is
// actually called.
//
// And since any Gemini target can end up on a Gemini 3 model, an effort has to mean something in
// both families. Low and high do; medium and every other level have no Gemini 3 level and are
// refused before launch rather than rounded.
//
// Each effort is one small system-settings file, chosen per invocation by
// GEMINI_CLI_SYSTEM_SETTINGS_PATH, so a lead and a preset role in one box can think at different
// levels. The client merges system settings over the user's and the project's key by key, so
// nothing of theirs is replaced — though a thinkingConfig they pin on one model is more specific
// than a family base, and still wins.

// geminiThinking is what each Coop effort becomes in each family. The budgets sit inside every 2.5
// model's accepted range (Pro 128-32768, Flash 0-24576, Flash-Lite 512-24576).
var geminiThinking = map[string]struct {
	level  string
	budget int
}{
	"low":  {"LOW", 1024},
	"high": {"HIGH", 24576},
}

// geminiThinkingModels are the names the pinned client runs through a family base: its request
// aliases and every chat model it defines (packages/core/src/config/models.ts,
// defaultModelConfigs.ts). An effort on any other name would reach no request, so it is refused.
var geminiThinkingModels = []string{
	"auto", "pro", "flash", "flash-lite", "auto-gemini-3", "auto-gemini-2.5",
	"gemini-3-pro-preview", "gemini-3.1-pro-preview", "gemini-3.1-pro-preview-customtools",
	"gemini-3-flash-preview", "gemini-3.5-flash", "gemini-3.8-flash", "gemini-3-flash", "gemini-3.1-flash-lite", "gemini-3.5-flash-lite",
	"gemini-3.1-flash-lite-preview", "gemini-2.5-pro", "gemini-2.5-flash", "gemini-2.5-flash-lite",
	"gemma-4-31b-it", "gemma-4-26b-a4b-it",
}

// validateGeminiEffort refuses an effort the pinned client cannot carry for model, naming what
// works. An empty model is the client's default, which resolves to one of the known models.
func validateGeminiEffort(model, effort string) error {
	if _, ok := geminiThinking[effort]; !ok {
		return fmt.Errorf("gemini takes effort low or high, not %q: Gemini 3 has no other thinking level, and any Gemini target can end up on a Gemini 3 model", effort)
	}
	if model != "" && !slices.Contains(geminiThinkingModels, model) {
		return fmt.Errorf("gemini applies effort only to the models Coop maps to a Gemini thinking level (%s); run %s without an effort", strings.Join(geminiThinkingModels, ", "), model)
	}
	return nil
}

// geminiThinkingEnv names the in-box directory holding one settings file per effort, for the
// consult and delegate arms to pick from by their $effort.
const geminiThinkingEnv = "COOP_GEMINI_THINKING"
const geminiThinkingDir = "/etc/gemini-cli/thinking"

// geminiThinkingWiring selects one image-owned system file per call. Consult and delegate
// arms choose again per call, without mutating the account's shared settings.
func geminiThinkingWiring(cfg *config.Config) ([]string, error) {
	env := []string{geminiThinkingEnv + "=" + geminiThinkingDir}
	if effort := cfg.EffortFor("gemini"); effort != "" {
		if _, ok := geminiThinking[effort]; !ok {
			return nil, fmt.Errorf("gemini effort %q has no thinking setting; use low or high", effort)
		}
		env = append(env, "GEMINI_CLI_SYSTEM_SETTINGS_PATH="+geminiThinkingDir+"/"+effort+".json")
	}
	return env, nil
}

// geminiThinkingSettings is one effort's system settings: both family bases at that effort, as
// customAliases so they merge over any the user or project defines, and the update switch this file
// displaces from the image's system layer. gemini-3-flash is the one chat model the pinned client
// gives no alias, so it would inherit neither base; here it gets its family's.
func geminiThinkingSettings(effort string) string {
	thinking := geminiThinking[effort]
	base := func(config map[string]any) map[string]any {
		return map[string]any{"extends": "chat-base", "modelConfig": map[string]any{
			"generateContentConfig": map[string]any{"thinkingConfig": config}}}
	}
	settings := map[string]any{"general": geminiNoUpdates, "modelConfigs": map[string]any{"customAliases": map[string]any{
		"chat-base-3":    base(map[string]any{"thinkingLevel": thinking.level}),
		"chat-base-2.5":  base(map[string]any{"thinkingBudget": thinking.budget}),
		"gemini-3-flash": map[string]any{"extends": "chat-base-3", "modelConfig": map[string]any{"model": "gemini-3-flash"}},
	}}}
	return geminiSystemSettings(settings)
}

func geminiSystemSettings(settings map[string]any) string {
	data, _ := json.MarshalIndent(settings, "", "  ") // internal literal settings: cannot fail
	return string(append(data, '\n'))
}
