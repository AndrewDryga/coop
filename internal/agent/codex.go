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
	"github.com/AndrewDryga/coop/internal/mcp"
)

type codexAgent struct{}

const (
	// Float on npm's stable latest tag so `coop update` pulls new agent fixes
	// without a source edit. The profile trigger below remains the local guard
	// for openai/codex#28224.
	codexCLIPackage = "@openai/codex@latest"
	codexACPPackage = "@agentclientprotocol/codex-acp@latest"
)

func init() { register(codexAgent{}) }

func (codexAgent) Name() string        { return "codex" }
func (codexAgent) DisplayName() string { return "Codex" }
func (codexAgent) Badge() string       { return "x" }

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

// PresetSessionID is false: codex has no flag to start a session under a caller-chosen
// id (it mints its own UUIDv7), so coop allocates none and Resume scans instead.
func (codexAgent) PresetSessionID() bool { return false }

// StartSession ignores id (codex can't preset one) and just starts interactively.
func (a codexAgent) StartSession(cfg *config.Config, _ string) []string {
	return a.Interactive(cfg)
}

func (a codexAgent) Resume(cfg *config.Config, ws, id string) ([]string, bool) {
	// `codex resume --last` is global. Fork launch owns legacy discovery explicitly, while
	// the adapter accepts only a persisted exact native ID for ordinary resumes.
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

// LatestSessionID supports one-time adoption of an old fork that predates exact native-ID hints.
func (codexAgent) LatestSessionID(cfg *config.Config, cwd string) string {
	latest, _ := codexSessionSnapshot(cfg.AgentDir("codex"), cwd)
	return latest
}

func (codexAgent) SessionIDs(cfg *config.Config, cwd string) []string {
	_, ids := codexSessionSnapshot(cfg.AgentDir("codex"), cwd)
	return ids
}

func (codexAgent) Login(*config.Config) []string {
	// Device-code flow: the box has no browser and codex's localhost OAuth redirect
	// can't reach the host, so browser login hangs. --device-auth prints a URL + code.
	return []string{"codex", "login", "--device-auth"}
}

func (codexAgent) ConsultCmd(question string) []string {
	return []string{"codex", "exec", "-s", "read-only", question}
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

// CredentialEnvKeys is Codex's only token env var.
func (codexAgent) CredentialEnvKeys() []string { return []string{"OPENAI_API_KEY"} }

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

// MCP emits the shared servers as [mcp_servers.*] in codex's config.toml.
func (codexAgent) MCP(cfg *config.Config) ([]MCPMount, error) {
	cx, err := mcp.GenerateCodex(cfg.MCPFile, filepath.Join(cfg.AgentDir("codex"), "config.toml"))
	if err != nil {
		return nil, err
	}
	return []MCPMount{{Content: cx, BoxPath: cfg.HomeInBox + "/.codex/config.toml"}}, nil
}

// ACPMCPServers is nil: this agent's ACP adapter reads the [mcp_servers.*] config.toml MCP mounts,
// so passing the servers again would register every one of them twice.
func (codexAgent) ACPMCPServers(string, func(string) (string, bool)) ([]map[string]any, error) {
	return nil, nil
}

// EnsureDefaults pre-trusts the workdir in codex's config.toml so a fresh box doesn't
// stop at "Do you trust this directory?". Codex records trust as
// [projects."<dir>"] trust_level = "trusted"; we append it idempotently. The box is the
// sandbox, so trusting the one mounted repo is the intended posture.
func (a codexAgent) EnsureDefaults(cfg *config.Config, workdir string) {
	if workdir == "" {
		return
	}
	dir := cfg.AgentDir(a.Name())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	hardenCodexSQLiteFeedbackLog(dir)
	path := filepath.Join(dir, "config.toml")
	data, _ := os.ReadFile(path) // missing file → empty, which is fine
	if strings.Contains(string(data), fmt.Sprintf("projects.%q", workdir)) {
		return // leave any existing entry for this dir untouched
	}
	out := string(data)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if out != "" {
		out += "\n"
	}
	out += fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", workdir)
	os.WriteFile(path, []byte(out), 0o644)
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
		_, ids := codexSessionSnapshot(codexDir, cwd)
		if slices.Contains(ids, id) {
			return id
		}
	}
	return ""
}

const codexSessionMetadataLimit = 64 << 10

// codexSessionSnapshot returns newest-first identity plus the complete unique ID set for cwd.
// Rollout metadata is provider-writable, so only bounded regular JSONL files are inspected.
func codexSessionSnapshot(codexDir, cwd string) (string, []string) {
	root, err := openSessionRoot(filepath.Join(codexDir, "sessions"))
	if err != nil {
		return "", nil
	}
	defer root.Close()
	var latest string
	var latestTime time.Time
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
		if info.ModTime().After(latestTime) {
			latestTime, latest = info.ModTime(), meta.Payload.ID
		}
		return nil
	})
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return latest, out
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

// codexConsultPrelude validates the JSON stream, extracts reply text, and prepares bounded
// best-effort usage; the wrapper publishes it only after accepting the complete attempt.
const codexConsultPrelude = `codex_text() {
	jq -ers '[.[] | select(.type=="item.completed" and .item.type=="agent_message") | .item.text | select(type=="string" and test("[^[:space:]]"))] | if length==0 then error("no usable agent reply") else .[] end'
}
# codex_peer_row logs this consult's token usage (the turn.completed event, read from stdin) to the
# run's peer-usage file so the loop's closing digest can tally the peer per model. Best-effort: no
# COOP_RUN_ID (not a loop), no usage event, or any write error → nothing, and it never fails the
# consult. codex reports tokens but no cost. Args: <role> <model>.
codex_peer_row() {
	[ -n "${COOP_RUN_ID:-}" ] || return 0
	case "$COOP_RUN_ID" in *[!a-zA-Z0-9._-]*) return 0 ;; esac
	peer_dir=.agent/runs
	peer_file=$peer_dir/$COOP_RUN_ID.peers.jsonl
	[ ! -L "$peer_dir" ] && [ -d "$peer_dir" ] || return 0
	[ ! -L "$peer_file" ] && [ -f "$peer_file" ] || return 0
	peer_links=$(stat -c %h "$peer_file" 2>/dev/null || stat -f %l "$peer_file" 2>/dev/null || :)
	peer_permissions=$(stat -c %a "$peer_file" 2>/dev/null || stat -f %Lp "$peer_file" 2>/dev/null || :)
	[ "$peer_links" = 1 ] && [ "$peer_permissions" = 600 ] || return 0
	# Open once, then prove the descriptor still names the private regular file just checked. All
	# later size checks and the append use that descriptor, so a pathname swap cannot redirect usage.
	eval 'exec 9>>"$peer_file"' 2>/dev/null || return 0
	[ ! -L "$peer_file" ] && [ -f "$peer_file" ] || { exec 9>&-; return 0; }
	peer_links=$(stat -c %h "$peer_file" 2>/dev/null || stat -f %l "$peer_file" 2>/dev/null || :)
	peer_permissions=$(stat -c %a "$peer_file" 2>/dev/null || stat -f %Lp "$peer_file" 2>/dev/null || :)
	[ "$peer_links" = 1 ] && [ "$peer_permissions" = 600 ] || { exec 9>&-; return 0; }
	peer_fd=/dev/fd/9
	[ -e "$peer_fd" ] || peer_fd=/proc/self/fd/9
	peer_path_identity=$(stat -c 'gnu:%d:%i' "$peer_file" 2>/dev/null || stat -f 'bsd:%i' "$peer_file" 2>/dev/null || :)
	peer_fd_identity=$(stat -Lc 'gnu:%d:%i' "$peer_fd" 2>/dev/null || stat -Lf 'bsd:%i' "$peer_fd" 2>/dev/null || :)
	[ -n "$peer_path_identity" ] && [ "$peer_path_identity" = "$peer_fd_identity" ] && [ ! -L "$peer_file" ] || { exec 9>&-; return 0; }
	peer_bytes=$(stat -Lc %s "$peer_fd" 2>/dev/null || stat -Lf %z "$peer_fd" 2>/dev/null || :)
	case "$peer_bytes" in '' | *[!0-9]*) exec 9>&-; return 0 ;; esac
	peer_usage_limit=1048576
	[ "$peer_bytes" -le $((peer_usage_limit - 4096)) ] || { exec 9>&-; return 0; }
	row=$(jq -sc --arg run "$COOP_RUN_ID" --arg role "$1" --arg model "$2" '
		def token: type=="number" and isfinite and floor==. and .>=0 and .<=1000000000;
		[.[] | select(type=="object" and .type=="turn.completed") | .usage
		 | select(type=="object")
		 | {input:(.input_tokens // 0), output:(.output_tokens // 0), reasoning:(.reasoning_output_tokens // 0)}
		 | select(.input|token) | select(.output|token) | select(.reasoning|token)
		 | .out=(.output + .reasoning) | select(.out<=1000000000)] | last // empty
		| {run:$run,role:$role,provider:"codex",model:$model,in:.input,out:.out}' 2>/dev/null) || { exec 9>&-; return 0; }
	[ -n "$row" ] || { exec 9>&-; return 0; }
	[ "$(printf '%s' "$row" | wc -c | tr -d '[:space:]')" -le 4096 ] || { exec 9>&-; return 0; }
	printf '%s\n' "$row" >&9 2>/dev/null || true
	exec 9>&-
}
# codex_finish is the shared fresh/resume result path. Telemetry reads the bounded raw stream before
# validation, while a real provider failure keeps its status and raw events for fallback
# classification. Only a successful provider must also prove it returned a usable reply.
codex_finish() {
	provider_status=$1
	raw=$2
	if [ "$provider_status" -ne 0 ]; then
		cat "$raw" >&2
		return "$provider_status"
	fi
	reply=$(codex_text <"$raw")
	reply_status=$?
	if [ "$reply_status" -ne 0 ]; then
		echo "[$peer: Codex returned malformed output or no usable reply — retry with: $fresh_retry; if it repeats, check or upgrade Codex]" >&2
		return 1
	fi
	candidate_telemetry_raw=$raw
	printf '%s\n' "$reply"
}
# Codex's native JSON is itself bounded before decoding; otherwise its command substitution could
# grow without limit even though the generic decoded-reply spool is capped.
codex_run() {
	codex_raw=$attempt_dir/codex-raw-$index
	codex_overflow=$attempt_dir/codex-overflow-$index
	codex_capture_status_file=$attempt_dir/codex-capture-status-$index
	codex_pipe=$attempt_dir/codex-pipe-$index
	rm -f "$codex_overflow" "$codex_capture_status_file"
	mkfifo "$codex_pipe" || return 1
	start_capture "$codex_raw" "$codex_overflow" "$attempt_dir/codex-chunk-$index" "$codex_pipe" "$codex_capture_status_file"
	codex_capture_pid=$capture_pid
	run "$@" >"$codex_pipe"
	codex_status=$?
	await_capture "$codex_capture_pid" "$codex_capture_status_file"
	codex_capture_status=$capture_status
	codex_capture_pid=
	rm -f "$codex_pipe"
	if [ "$codex_capture_status" -ne 0 ]; then
		echo "[$peer: failed to capture Codex output safely — retry with: $fresh_retry]" >&2
		return 1
	fi
	if [ -f "$codex_overflow" ]; then
		echo "[$peer: Codex output exceeded ${consult_stream_limit} bytes — narrow the question and retry with: $fresh_retry]" >&2
		return 1
	fi
	codex_finish "$codex_status" "$codex_raw"
}`

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

func (codexAgent) ShellPrelude() string  { return codexConsultPrelude }
func (codexAgent) InstallScript() string { return "" }
