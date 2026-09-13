package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/pelletier/go-toml/v2"
)

type codexAgent struct{}

func (codexAgent) Scaffold() ScaffoldSpec {
	return ScaffoldSpec{Project: ScaffoldLayout{Dir: ".codex"}}
}

const (
	// Float on npm's stable latest tag so `coop update` pulls new agent fixes
	// without a source edit. The profile trigger below remains the local guard
	// for openai/codex#28224.
	codexCLIPackage = "@openai/codex@latest"
	codexACPPackage = "@agentclientprotocol/codex-acp@latest"
)

func init() { register(codexAgent{}) }

func (codexAgent) Name() string        { return "codex" }
func (codexAgent) SkillsCapable() bool { return true }
func (codexAgent) DisplayName() string { return "Codex" }
func (codexAgent) Vendor() string      { return "OpenAI" }

// LockedClients pins the exact CLI and ACP adapter builds the qualified client
// image installs. Both drive the same vendored native codex executable.
func (codexAgent) LockedClients(platform ClientPlatform) []LockedClient {
	if !platform.valid() {
		return nil
	}
	cpu, target := "arm64", "aarch64-unknown-linux-musl"
	if platform.Architecture == "amd64" {
		cpu, target = "x64", "x86_64-unknown-linux-musl"
	}
	native := lockedClientRoot + "/node_modules/@openai/codex-linux-" + cpu + "/vendor/" + target + "/bin/codex"
	return []LockedClient{
		{Client: egress.ClientCLI, Package: "@openai/codex", Version: "0.153.4", Binary: "codex", Exec: []string{"/usr/local/bin/node", lockedClientRoot + "/node_modules/@openai/codex/bin/codex.js"}, RequiredExecutables: []LockedExecutable{{Path: native, Version: "0.153.4-linux-" + cpu}}},
		{Client: egress.ClientACP, Package: "@agentclientprotocol/codex-acp", Version: "1.10.0", Binary: "codex-acp", Exec: []string{"/usr/local/bin/node", lockedClientRoot + "/node_modules/@agentclientprotocol/codex-acp/dist/index.js"}, UnsetEnv: []string{"CODEX_PATH"}, RequiredExecutables: []LockedExecutable{{Path: native, Version: "0.153.4-linux-" + cpu}}},
	}
}

// NetworkBundle covers the ChatGPT backend plus its login/token host.
func (a codexAgent) NetworkBundle(input NetworkBundleInput) (egress.Bundle, error) {
	return directNetworkBundle(a.Name(), "chatgpt-file", input,
		[]string{"chatgpt.com", "auth.openai.com"},
		[]string{"https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/model-provider-info/src/lib.rs", "https://github.com/openai/codex/blob/rust-v0.153.4/codex-rs/login/src/auth/manager.rs"})
}

// Stream: codex keys every item lifecycle event on the item id, so command_execution, MCP, and
// collab calls report their own start and completion — a tool lifecycle the watchdog can pair.
func (codexAgent) Stream() StreamSpec {
	return StreamSpec{
		Format: StreamCodexJSON, Flags: []string{"--json"}, TrailingArgs: 1,
		ToolLifecycle: ToolLifecycleIDs,
	}
}

// base guards against an empty COOP_CODEX_CMD override, since the exec/resume forms
// index base[0]. The resolved model rides in base as a trailing --model, which codex
// accepts on its main command and under exec/resume alike.
func (codexAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_CODEX_CMD", "codex --dangerously-bypass-approvals-and-sandbox")
	if len(b) == 0 {
		b = []string{"codex"}
	}
	return withEffort(withModel(b, cfg.ModelFor("codex")), codexAgent{}, cfg.EffortFor("codex"))
}

func (a codexAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

func (a codexAgent) Headless(cfg *config.Config, prompt string) []string {
	// codex runs headless via an `exec` subcommand; the prompt is positional.
	b := a.base(cfg)
	return append(append([]string{b[0], "exec"}, b[1:]...), prompt)
}

// ACP is a separate adapter binary whose default "agent" mode enables Codex's inner
// bubblewrap sandbox. Coop's box is already the security boundary and cannot nest that
// namespace, so pin the adapter's supported full-access mode to this process only.
func (codexAgent) ACP(*config.Config) []string {
	return []string{"env", "INITIAL_AGENT_MODE=agent-full-access", "codex-acp"}
}

// ACPSessionDirs: codex stores rollouts under ~/.codex/sessions (best-effort — codex-acp's resume
// story is weaker than claude's; sharing the dir is the most we can do without a preset-id).
func (codexAgent) ACPSessionDirs() []string { return []string{"sessions"} }

// ACPFinalChunk: codex-acp streams progress commentary and the final answer through the same
// agent_message_chunk event and marks the host-owned phase in `_meta.codex.phase`; only the final
// phase is the answer. A chunk without a phase is treated as answer text, as any other adapter's.
func (codexAgent) ACPFinalChunk(meta json.RawMessage) bool {
	var m struct {
		Codex struct {
			Phase string `json:"phase"`
		} `json:"codex"`
	}
	if len(meta) == 0 || json.Unmarshal(meta, &m) != nil {
		return true
	}
	return m.Codex.Phase == "" || m.Codex.Phase == "final_answer"
}

func (codexAgent) ACPProgressChunk(meta json.RawMessage) bool {
	var m struct {
		Codex struct {
			Phase string `json:"phase"`
		} `json:"codex"`
	}
	return json.Unmarshal(meta, &m) == nil && m.Codex.Phase == "commentary"
}

// PresetSessionID is false: codex has no flag to start a session under a caller-chosen id (it mints
// its own UUIDv7), so coop records the uniquely new native ID after the run and validates it on resume.
func (codexAgent) PresetSessionID() bool { return false }

// StartSession ignores id (codex can't preset one) and just starts interactively.
func (a codexAgent) StartSession(cfg *config.Config, _ string) []string {
	return a.Interactive(cfg)
}

func (a codexAgent) Resume(cfg *config.Config, ws, id string) ([]string, bool) {
	// `codex resume --last` is global, so accept only a persisted exact native ID.
	if id = findCodexSession(cfg.AgentDir("codex"), ws, id); id != "" {
		b := a.base(cfg)
		return append([]string{b[0], "resume", id}, b[1:]...), true
	}
	return a.Interactive(cfg), false
}

// ProducesSession is false for `codex exec …` — headless rollouts (source:"exec") that
// discovery excludes, so they stay concurrent with interactive Codex sessions and need no lock.
func (codexAgent) ProducesSession(args []string) bool {
	return len(args) == 0 || args[0] != "exec"
}

func (codexAgent) SessionIDs(cfg *config.Config, cwd string) []string {
	return codexSessionIDs(cfg.AgentDir("codex"), cwd)
}

func (codexAgent) LoginConfig(cfg *config.Config) (MCPConfig, error) {
	return MCPConfig{Mounts: []MCPMount{{Content: mcp.CodexManagedDefaults, BoxPath: cfg.HomeInBox + "/.codex/config.toml"}}}, nil
}

func (codexAgent) Login(*config.Config) []string {
	// Device-code flow: the box has no browser and codex's localhost OAuth redirect
	// can't reach the host, so browser login hangs. --device-auth prints a URL + code.
	return []string{"codex", "login", "--device-auth"}
}

func (codexAgent) ConsultCmd(question string) []string {
	return []string{"codex", "exec", "-s", "read-only", question}
}

// RestrictedCommand: no restricted mode is qualified on this CLI yet (see unqualifiedRestrictedCommand).
func (a codexAgent) RestrictedCommand(mode ExecutionMode, cmd []string) ([]string, error) {
	return unqualifiedRestrictedCommand(a, mode, cmd)
}

// ACPRestrictedSessionMeta: nor on its ACP adapter (see unqualifiedRestrictedACPSession).
func (a codexAgent) ACPRestrictedSessionMeta(mode ExecutionMode) (map[string]any, error) {
	return unqualifiedRestrictedACPSession(a, mode)
}

func (codexAgent) Packages() []string {
	return []string{codexCLIPackage, codexACPPackage}
}

// Models are common codex model ids. Illustrative — any id the CLI accepts works.
func (codexAgent) Models() []string {
	return []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex-spark",
	}
}

// ExampleModel: the frontier id, and the one this repo's own preset leads with.
func (codexAgent) ExampleModel() string { return "gpt-5.6-sol" }

// ModelEnv: codex reads no model env var (its default lives in config.toml), so the
// flag in base() is the only coop-driven path.
func (codexAgent) ModelEnv() string { return "" }

// Effort: codex takes reasoning effort as a config override, -c model_reasoning_effort=<level>
// (minimal/low/medium/high/xhigh), on its main command and exec/resume alike.
func (codexAgent) Effort() EffortSpec {
	return EffortSpec{
		Style: EffortFlagAssignment, Flag: "-c", Aliases: []string{"--config"},
		Assignment: "model_reasoning_effort",
	}
}

// EffortEnv: codex reads no effort env var (its default lives in config.toml), so the flag in
// base() is the only coop-driven path — like its model.
func (codexAgent) EffortEnv() string { return "" }

func (codexAgent) InstructionFile() string { return "AGENTS.md" }

func (codexAgent) NativeSubagents() NativeSubagentSupport { return NativeSubagentSupport{} }

func (codexAgent) AuthMarker() (file, envKey string) { return "auth.json", "OPENAI_API_KEY" }

// CredentialEnvKeys lists every env var Codex reads a token from: OPENAI_API_KEY plus the two
// alternates native `codex exec` and the ACP adapter accept. Each is credential authority even
// when a particular client ignores one — a name left off this list is never stripped, so the
// token would ride into a box scoped to another provider.
func (codexAgent) CredentialEnvKeys() []string {
	return []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"}
}

func (codexAgent) LiveCredentials() LiveCredentialSpec {
	return LiveCredentialSpec{
		Artifacts: []CredentialArtifact{{
			Name: "auth.json", Primary: true, Project: projectCodexCredential,
		}},
		Prepare:     renewCodexCredential,
		Portability: codexCredentialPortability,
		AuthSignals: []string{"not logged in", "authentication required", "401 unauthorized", "invalid api key"},
	}
}

const (
	codexRefreshTokenURL = "https://auth.openai.com/oauth/token"
	codexOAuthClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexCredentialLimit = 1 << 20
)

var errCodexCredentialChanged = fmt.Errorf("codex credential changed during refresh")

type codexSourceCredential struct {
	AuthMode     string             `json:"auth_mode"`
	OpenAIAPIKey string             `json:"OPENAI_API_KEY"`
	Tokens       *codexSourceTokens `json:"tokens"`
	LastRefresh  string             `json:"last_refresh"`
}

type codexSourceTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type codexAccessTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

type codexAccessCredential struct {
	AuthMode     string             `json:"auth_mode"`
	OpenAIAPIKey string             `json:"OPENAI_API_KEY,omitempty"`
	Tokens       *codexAccessTokens `json:"tokens,omitempty"`
	LastRefresh  string             `json:"last_refresh,omitempty"`
}

func decodeCodexAccessCredential(data []byte) (codexAccessCredential, error) {
	var source codexSourceCredential
	if err := json.Unmarshal(data, &source); err != nil {
		return codexAccessCredential{}, fmt.Errorf("decode Codex credential: %w", err)
	}
	mode := source.AuthMode
	if mode == "" {
		switch {
		case source.Tokens != nil:
			mode = "chatgpt"
		case source.OpenAIAPIKey != "":
			mode = "apikey"
		}
	}
	switch mode {
	case "apikey":
		if source.OpenAIAPIKey == "" {
			return codexAccessCredential{}, fmt.Errorf("codex credential has no API-key auth shape")
		}
		return codexAccessCredential{AuthMode: mode, OpenAIAPIKey: source.OpenAIAPIKey}, nil
	case "chatgpt":
		if source.Tokens == nil || source.Tokens.IDToken == "" || source.Tokens.AccessToken == "" || source.LastRefresh == "" {
			return codexAccessCredential{}, fmt.Errorf("codex credential has no access-only ChatGPT shape")
		}
		return codexAccessCredential{
			AuthMode: mode,
			Tokens: &codexAccessTokens{
				IDToken: source.Tokens.IDToken, AccessToken: source.Tokens.AccessToken,
				RefreshToken: "", AccountID: source.Tokens.AccountID,
			},
			LastRefresh: source.LastRefresh,
		}, nil
	default:
		return codexAccessCredential{}, fmt.Errorf("codex credential has unsupported auth mode")
	}
}

type codexRefreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type codexRefreshResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// renewCodexCredential keeps refresh authority in the trusted source profile. The sandbox only
// receives projectCodexCredential's access-only representation after this function returns.
func renewCodexCredential(profileDir string, deadline time.Time) error {
	path := filepath.Join(profileDir, "auth.json")
	lock, err := os.OpenFile(path+".refresh.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open Codex credential refresh lock: %w", err)
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() {
		return fmt.Errorf("codex credential refresh lock is unsafe")
	}
	if err := lock.Chmod(0o600); err != nil {
		return fmt.Errorf("protect Codex credential refresh lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock Codex credential refresh: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	for range 3 {
		err := renewCodexCredentialLocked(path, deadline)
		if err != errCodexCredentialChanged {
			return err
		}
	}
	return fmt.Errorf("codex credential changed repeatedly during refresh")
}

func renewCodexCredentialLocked(path string, deadline time.Time) error {
	data, err := readCodexCredential(path)
	if err != nil {
		return fmt.Errorf("read Codex credential for refresh: %w", err)
	}
	var source codexSourceCredential
	if err := json.Unmarshal(data, &source); err != nil {
		return fmt.Errorf("decode Codex credential for refresh: %w", err)
	}
	mode := source.AuthMode
	if mode == "" && source.Tokens != nil {
		mode = "chatgpt"
	}
	if mode == "apikey" {
		return nil
	}
	if mode != "chatgpt" || source.Tokens == nil {
		return fmt.Errorf("codex credential needs sign-in")
	}
	if jwtExpiresAfter(source.Tokens.AccessToken, deadline) {
		return nil
	}
	if source.Tokens.RefreshToken == "" {
		return fmt.Errorf("codex credential needs sign-in")
	}

	response, err := requestCodexCredentialRefresh(source.Tokens.RefreshToken, deadline)
	if err != nil {
		return err
	}
	if response.AccessToken == "" || !jwtExpiresAfter(response.AccessToken, deadline) {
		return fmt.Errorf("codex credential refresh returned an unusable access token")
	}

	var document map[string]json.RawMessage
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode Codex credential document: %w", err)
	}
	if err := json.Unmarshal(document["tokens"], &tokens); err != nil {
		return fmt.Errorf("decode Codex token document: %w", err)
	}
	setJSON := func(target map[string]json.RawMessage, key string, value any) error {
		encoded, err := json.Marshal(value)
		if err == nil {
			target[key] = encoded
		}
		return err
	}
	if response.IDToken != "" {
		if err := setJSON(tokens, "id_token", response.IDToken); err != nil {
			return err
		}
	}
	if err := setJSON(tokens, "access_token", response.AccessToken); err != nil {
		return err
	}
	if response.RefreshToken != "" {
		if err := setJSON(tokens, "refresh_token", response.RefreshToken); err != nil {
			return err
		}
	}
	encodedTokens, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("encode refreshed Codex tokens: %w", err)
	}
	document["tokens"] = encodedTokens
	if source.AuthMode != "" {
		if err := setJSON(document, "auth_mode", "chatgpt"); err != nil {
			return err
		}
	}
	if err := setJSON(document, "last_refresh", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode refreshed Codex credential: %w", err)
	}
	current, err := readCodexCredential(path)
	if err != nil {
		return fmt.Errorf("re-read Codex credential before refresh persistence: %w", err)
	}
	if !bytes.Equal(current, data) {
		return errCodexCredentialChanged
	}
	if err := config.WriteFileAtomic(path, append(encoded, '\n')); err != nil {
		return fmt.Errorf("persist refreshed Codex credential: %w", err)
	}
	return nil
}

func readCodexCredential(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credential is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, codexCredentialLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > codexCredentialLimit {
		return nil, fmt.Errorf("credential is too large")
	}
	return data, nil
}

func requestCodexCredentialRefresh(refreshToken string, deadline time.Time) (codexRefreshResponse, error) {
	endpoint := strings.TrimSpace(os.Getenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE"))
	if endpoint == "" {
		endpoint = codexRefreshTokenURL
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname())) {
		return codexRefreshResponse{}, fmt.Errorf("codex credential refresh endpoint is unsafe")
	}
	clientID := strings.TrimSpace(os.Getenv("CODEX_APP_SERVER_LOGIN_CLIENT_ID"))
	if clientID == "" {
		clientID = codexOAuthClientID
	}
	body, err := json.Marshal(codexRefreshRequest{
		ClientID: clientID, GrantType: "refresh_token", RefreshToken: refreshToken,
	})
	if err != nil {
		return codexRefreshResponse{}, fmt.Errorf("encode Codex credential refresh: %w", err)
	}
	requestDeadline := time.Now().Add(30 * time.Second)
	if deadline.Before(requestDeadline) {
		requestDeadline = deadline
	}
	ctx, cancel := context.WithDeadline(context.Background(), requestDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return codexRefreshResponse{}, fmt.Errorf("create Codex credential refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return codexRefreshResponse{}, fmt.Errorf("refresh Codex credential: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 64<<10)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, limited)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest {
			return codexRefreshResponse{}, fmt.Errorf("codex credential needs sign-in")
		}
		return codexRefreshResponse{}, fmt.Errorf("codex credential refresh failed with HTTP %d", resp.StatusCode)
	}
	var result codexRefreshResponse
	if err := json.NewDecoder(limited).Decode(&result); err != nil {
		return codexRefreshResponse{}, fmt.Errorf("decode Codex credential refresh: %w", err)
	}
	return result, nil
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func projectCodexCredential(data []byte) ([]byte, error) {
	projected, err := decodeCodexAccessCredential(data)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil, fmt.Errorf("encode Codex credential: %w", err)
	}
	return append(encoded, '\n'), nil
}

func (a codexAgent) ActiveCredentialEnvKeys(_ string, markerPresent bool) []string {
	if markerPresent {
		return nil
	}
	return a.CredentialEnvKeys()
}

func (codexAgent) StoredCredentialStatus(profileDir string, now time.Time) StoredCredentialStatus {
	data, err := readCodexCredential(filepath.Join(profileDir, "auth.json"))
	if err != nil {
		return StoredCredentialReauthRequired
	}
	var source codexSourceCredential
	if json.Unmarshal(data, &source) != nil {
		return StoredCredentialReauthRequired
	}
	if source.AuthMode == "apikey" || (source.AuthMode == "" && source.OpenAIAPIKey != "") {
		if source.OpenAIAPIKey != "" {
			return StoredCredentialReady
		}
		return StoredCredentialReauthRequired
	}
	if source.Tokens != nil && (source.Tokens.RefreshToken != "" || jwtExpiresAfter(source.Tokens.AccessToken, now)) {
		return StoredCredentialReady
	}
	return StoredCredentialReauthRequired
}

func codexCredentialPortability(profileDir string, deadline time.Time) CredentialPortability {
	data, err := readCodexCredential(filepath.Join(profileDir, "auth.json"))
	if err != nil {
		return CredentialRefreshRequired
	}
	credentials, err := decodeCodexAccessCredential(data)
	if err != nil {
		return CredentialRefreshRequired
	}
	if credentials.AuthMode == "apikey" || jwtExpiresAfter(credentials.Tokens.AccessToken, deadline) {
		return CredentialPortable
	}
	return CredentialRefreshRequired
}

// MCP builds the config.toml mounted over the profile's own in every codex box — the direct CLI
// and codex-acp read the same CODEX_HOME — so the managed-client defaults (no update check, no
// analytics or OTEL export; see mcp.CodexManagedDefaults) apply without a host write, and the
// shared servers land as [mcp_servers.*] when MCP is active. Auth, sessions and the user's other
// settings are the host profile's, kept verbatim.
func (codexAgent) MCP(cfg *config.Config, workdir string) (MCPConfig, error) {
	cx, err := mcp.GenerateCodex(cfg.MCPFile, filepath.Join(cfg.AgentDir("codex"), "config.toml"))
	if err != nil {
		return MCPConfig{}, err
	}
	cx, _ = ensureCodexTrust(cx, workdir)
	return MCPConfig{Mounts: []MCPMount{{Content: cx, BoxPath: cfg.HomeInBox + "/.codex/config.toml"}}}, nil
}

// ACPMCPServers declares the same shared servers to codex-acp. Codex still reads
// their authority from the generated config.toml mount, while codex-acp 1.7 uses
// session/new.mcpServers as the session inventory and startup contract. The
// adapter deduplicates names already present in config.toml.
func (codexAgent) ACPMCPServers(path string, lookupEnv func(string) (string, bool)) ([]map[string]any, error) {
	return mcp.ACPServers(path, lookupEnv)
}

// EnsureDefaults pre-trusts the workdir in codex's config.toml so a fresh box doesn't
// stop at "Do you trust this directory?". Codex records trust as
// [projects."<dir>"] trust_level = "trusted"; we append it idempotently. The box is the
// sandbox, so trusting the one mounted repo is the intended posture.
func (a codexAgent) EnsureDefaults(cfg *config.Config, workdir string) error {
	if workdir == "" {
		return nil
	}
	dir := cfg.AgentDir(a.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create Codex defaults directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config.toml")
	data, _, err := readDefaultsFile(path)
	if err != nil {
		return err
	}
	if err := validateCodexDefaults(data); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	hardenCodexSQLiteFeedbackLog(dir)
	out, changed := ensureCodexTrust(string(data), workdir)
	if !changed {
		return nil
	}
	if err := config.WriteFileAtomicMode(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// validateCodexDefaults refuses a config.toml coop cannot parse, so EnsureDefaults never appends a
// trust stanza to a file it does not understand. A blank file is valid: it becomes the stanza.
func validateCodexDefaults(data []byte) error {
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	var values map[string]any
	return toml.Unmarshal(data, &values)
}

func ensureCodexTrust(configTOML, workdir string) (string, bool) {
	if workdir == "" || strings.Contains(configTOML, fmt.Sprintf("projects.%q", workdir)) {
		return configTOML, false
	}
	out := configTOML
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	out += fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", workdir)
	return out, true
}

// hardenCodexSQLiteFeedbackLog applies the upstream issue's local workaround to
// the mounted Codex profile. It blocks inserts into the feedback-log table only;
// sessions, auth, MCP config, and memories continue to work. Best-effort by
// design: a fresh profile has no DB yet, and custom hosts may not ship sqlite3.
func hardenCodexSQLiteFeedbackLog(dir string) {
	db := filepath.Join(dir, "logs_2.sqlite")
	if info, err := os.Stat(db); err != nil || info.IsDir() {
		return
	}
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		return
	}
	_ = exec.Command(sqlite, db, `CREATE TRIGGER IF NOT EXISTS block_log_inserts BEFORE INSERT ON logs BEGIN SELECT RAISE(IGNORE); END;`).Run()
}

// findCodexSession returns the exact requested CLI session for cwd. Codex stores JSONL by date
// with first-line {type,payload:{id,cwd,source}} metadata.
func findCodexSession(codexDir, cwd, id string) string {
	if ValidSessionID(id) {
		ids := codexSessionIDs(codexDir, cwd)
		if slices.Contains(ids, id) {
			return id
		}
	}
	return ""
}

const codexSessionMetadataLimit = 64 << 10

// codexSessionIDs returns the complete unique CLI-session ID set for cwd. Rollout metadata is
// provider-writable, so only bounded regular JSONL files are inspected.
func codexSessionIDs(codexDir, cwd string) []string {
	root, err := openSessionRoot(filepath.Join(codexDir, "sessions"))
	if err != nil {
		return nil
	}
	defer root.Close()
	ids := map[string]bool{}
	_ = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		f, err := root.Open(path)
		if err != nil {
			return nil
		}
		line, readErr := bufio.NewReader(io.LimitReader(f, codexSessionMetadataLimit+1)).ReadString('\n')
		_ = f.Close()
		if len(line) > codexSessionMetadataLimit || (readErr != nil && readErr != io.EOF) {
			return nil
		}
		var meta struct {
			Type    string `json:"type"`
			Payload struct {
				ID, Cwd, Source string
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &meta) != nil || meta.Type != "session_meta" ||
			meta.Payload.Cwd != cwd || meta.Payload.Source != "cli" || !ValidSessionID(meta.Payload.ID) {
			return nil
		}
		ids[meta.Payload.ID] = true
		return nil
	})
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// ACPRateLimitSignals: codex-acp surfaces a limit as codexErrorInfo=usageLimitExceeded
// (top-level or nested); the value alone is the proof, whatever key carries it.
func (codexAgent) ACPRateLimitSignals() []ACPSignal {
	return []ACPSignal{{Value: "usageLimitExceeded"}}
}

// ACPSessionSettings uses codex-acp's native config IDs. Model MUST precede
// reasoning_effort because a model change resets effort to that model's default.
func (codexAgent) ACPSessionSettings(target Target) []ACPSessionSetting {
	var settings []ACPSessionSetting
	if target.Model != "" {
		settings = append(settings, ACPSessionSetting{Method: ACPSetConfigOption, ConfigID: "model", Value: target.Model})
	}
	if target.Effort != "" {
		settings = append(settings, ACPSessionSetting{Method: ACPSetConfigOption, ConfigID: "reasoning_effort", Value: target.Effort})
	}
	return settings
}

// BoxEnv points codex's single-writer sqlite state (state_*.sqlite, logs_*, memories_*,
// goals_* — the "state runtime") at a CONTAINER-LOCAL path off the shared ~/.codex bind
// mount, via CODEX_SQLITE_HOME (honored by codex and codex-acp). codex ≥0.144 keeps that
// state in $CODEX_HOME, and two boxes sharing one account's home make the second crash
// ("failed to initialize sqlite state runtime") — sqlite's single-writer lock can't span the
// mount. Redirecting only the sqlite keeps the WHOLE home shared as before (auth + its
// in-place refresh, sessions, config), so any number of codex boxes run in parallel on one
// account, each with its own state on its own writable layer. Ephemeral by design: the
// session INDEX is rebuilt from the shared sessions/ rollouts (codex backfills), so resume
// still works; per-box goals/memories don't persist, which is inherent to parallel sessions.
func (codexAgent) BoxEnv(homeInBox string) []string {
	return []string{"CODEX_SQLITE_HOME=" + homeInBox + "/.codex-state"}
}

func (codexAgent) HomeFallbacks() []HomeFallback { return nil }

// Provider-specific decoding stays here; capture and safe append share transport mechanics.
const codexConsultText = `codex_text() {
	jq -ers '[.[] | select(.type=="item.completed" and .item.type=="agent_message") | .item.text | select(type=="string" and test("[^[:space:]]"))] | if length==0 then error("no usable agent reply") else .[] end'
}
`

const codexConsultUsage = `[.[] | select(type=="object" and .type=="turn.completed") | .usage
		 | select(type=="object")
		 | {input:(.input_tokens // 0), output:(.output_tokens // 0), reasoning:(.reasoning_output_tokens // 0)}
		 | select(.input|token) | select(.output|token) | select(.reasoning|token)
		 | .output=(.output + .reasoning) | select(.output<=1000000000)] | last // empty`

func (codexAgent) ConsultFresh() string {
	return `codex_run codex exec -s read-only ${model:+--model "$model"} ${effort:+-c model_reasoning_effort="$effort"} --json "$prompt"; finish_status=$?
	# Only record a thread id parsed from a bounded usable reply. The generic wrapper commits this
	# candidate after validating the decoded stdout, so failed calls never become resumable.
	tid=$(jq -r 'select(.type=="thread.started" and (.thread_id|type)=="string") | .thread_id | select(test("^[A-Za-z0-9._:-]{1,512}$"))' "$codex_raw" 2>/dev/null | head -n1)
	# A thread becomes resumable only after its first usable reply; otherwise --continue would
	# revive a session the lead never received.
	if [ "$finish_status" -eq 0 ] && [ -n "$tid" ]; then printf '%s' "$tid" >"$candidate_idfile"; fi
	return "$finish_status"`
}

func (codexAgent) ConsultResume() string {
	return `codex_run codex exec resume "$id" -c sandbox_mode=read-only ${model:+--model "$model"} ${effort:+-c model_reasoning_effort="$effort"} --json "$prompt"; return "$?"`
}

func (codexAgent) DelegateExec() string {
	return `codex exec --dangerously-bypass-approvals-and-sandbox ${model:+--model "$model"} ${effort:+-c model_reasoning_effort="$effort"} "$prompt"`
}

func (codexAgent) ShellPrelude() string {
	return codexConsultText + consultPeerRowShell("codex", codexConsultUsage) + consultCaptureShell("codex", "Codex")
}
func (codexAgent) InstallScript() string { return "" }

const (
	codexReviewFooter = "tokens used"
	// This marker is returned only through the internal response tail. It keeps an adapter-owned
	// envelope that failed validation from accidentally reaching a later valid-looking receipt.
	codexMalformedReviewEnvelope = "\x00coop-codex-review-envelope-invalid\x00"
)

// ReviewOutput removes only the footer shape emitted by the Codex transport wrapper.
// The wrapper appends `tokens used` plus a formatted count and may repeat the final agent message
// or its exact terminal evidence/receipt block before or after that footer. Earlier agent messages
// are narration, not part of the echoed response. A different trailing block is not a second answer
// to merge: it is an invalid envelope and gets an impossible terminal marker so strict receipt
// parsing fails.
func (codexAgent) ReviewOutput(output string, contract ReviewOutputContract) (string, bool) {
	return (codexReviewNormalizer{contract}).normalize(output)
}

type codexReviewNormalizer struct{ ReviewOutputContract }

func (n codexReviewNormalizer) normalize(output string) (string, bool) {
	output = n.Normalize(output)
	lines := strings.Split(output, "\n")
	end := len(lines) - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end < 0 {
		return output, true
	}

	footer := -1
	for i := 0; i < end; i++ {
		if lines[i] != codexReviewFooter {
			continue
		}
		if !codexTokenCount(lines[i+1]) {
			return rejectCodexReviewEnvelope(output)
		}
		if footer >= 0 {
			return rejectCodexReviewEnvelope(output)
		}
		footer = i
	}
	if end >= 0 && lines[end] == codexReviewFooter {
		return rejectCodexReviewEnvelope(output)
	}
	if footer < 0 {
		if strings.Contains(output, "REVIEW COMPLETE — ") {
			if !n.ValidOutput(output) {
				return rejectCodexReviewEnvelope(output)
			}
			return output, true
		}
		return output, true
	}

	beforeFooter := lines[:footer]
	afterFooter := strings.Join(lines[footer+2:end+1], "\n")
	if afterFooter != "" {
		if n.validCodexReviewCandidate(afterFooter) && !hasCodexStructuredReviewLine(beforeFooter) {
			return afterFooter, true
		}
		if !n.validCodexReviewCandidate(afterFooter) ||
			n.codexReviewReceiptCount(beforeFooter)+n.codexReviewReceiptCount(strings.Split(afterFooter, "\n")) != 2 ||
			!hasCodexReviewSuffix(beforeFooter, afterFooter) {
			return rejectCodexReviewEnvelope(output)
		}
		return afterFooter, true
	}
	if canonical, ok := n.codexReviewCandidate(beforeFooter); ok {
		return canonical, true
	}
	if canonical, ok := n.duplicatedCodexReviewSuffix(beforeFooter); ok {
		return canonical, true
	}
	return rejectCodexReviewEnvelope(output)
}

func hasCodexStructuredReviewLine(lines []string) bool {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "AUDIT EVIDENCE — ") || strings.Contains(line, "REVIEW COMPLETE — ") {
			return true
		}
	}
	return false
}

func (n codexReviewNormalizer) codexReviewCandidate(lines []string) (string, bool) {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || n.codexReviewReceiptCount(lines) != 1 {
		return "", false
	}
	candidate := strings.Join(lines, "\n")
	if !n.ValidOutput(candidate) {
		return "", false
	}
	return candidate, true
}

func (n codexReviewNormalizer) validCodexReviewCandidate(candidate string) bool {
	_, ok := n.codexReviewCandidate(strings.Split(candidate, "\n"))
	return ok
}

func (n codexReviewNormalizer) codexReviewReceiptCount(lines []string) int {
	count := 0
	for _, line := range lines {
		if n.ValidReceiptLine(line) {
			count++
		}
	}
	return count
}

func hasCodexReviewSuffix(lines []string, candidate string) bool {
	before := strings.Join(lines, "\n")
	return before == candidate || strings.HasSuffix(before, "\n"+candidate)
}

func (n codexReviewNormalizer) duplicatedCodexReviewSuffix(lines []string) (string, bool) {
	if n.codexReviewReceiptCount(lines) != 2 {
		return "", false
	}
	for start := 1; start < len(lines); start++ {
		candidate, ok := n.codexReviewCandidate(lines[start:])
		if ok && hasCodexReviewSuffix(lines[:start], candidate) {
			return candidate, true
		}
	}
	return "", false
}

func rejectCodexReviewEnvelope(output string) (string, bool) {
	return strings.TrimRight(output, "\r\n") + "\n" + codexMalformedReviewEnvelope, false
}

func codexTokenCount(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	groups := strings.Split(value, ",")
	if len(groups) > 1 && (len(groups[0]) < 1 || len(groups[0]) > 3) {
		return false
	}
	for i, group := range groups {
		if group == "" || (i > 0 && len(group) != 3) {
			return false
		}
		for _, r := range group {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func (codexAgent) ReviewFooterLine(line string) bool {
	line = strings.TrimSpace(line)
	return line == codexReviewFooter || codexTokenCount(line)
}

func (codexAgent) PlainOutputProbe() PlainOutputProbe { return nil }

func (codexAgent) ModelCatalog() ModelCatalogSpec {
	return ModelCatalogSpec{HostCommand: []string{"codex", "debug", "models"}, ParseHost: parseCodexModels}
}

// parseCodexModels reads `codex debug models` JSON, keeping only list-visible models
// (visibility=="list", dropping bundled/internal ones) with their slug + display name.
func parseCodexModels(out []byte) ([]Model, error) {
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var models []Model
	for _, m := range doc.Models {
		if m.Visibility != "list" || m.Slug == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.Slug
		}
		models = append(models, Model{ID: m.Slug, Name: name})
	}
	return models, nil
}
