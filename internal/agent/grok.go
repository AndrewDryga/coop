package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type grokAgent struct{}

func (grokAgent) Scaffold() ScaffoldSpec { return ScaffoldSpec{} }

func (grokAgent) ReviewOutput(raw string, _ ReviewOutputContract) (string, bool) { return raw, true }
func (grokAgent) ReviewFooterLine(string) bool                                   { return false }
func (grokAgent) PlainOutputProbe() PlainOutputProbe                             { return nil }

func init() { register(grokAgent{}) }

func (grokAgent) Name() string { return "grok" }

// SkillsCapable: the pinned 1.0.25 client discovers skills natively from ~/.grok/skills/<name>/
// SKILL.md — the directory Coop's shared projection already targets (~/.<agent>/skills) — and lists
// them in `grok inspect`. Project-scope .grok/skills outranks user scope in its own precedence, so a
// repository's own skills still win over the projected copy.
func (grokAgent) SkillsCapable() bool { return true }
func (grokAgent) DisplayName() string { return "Grok" }
func (grokAgent) Vendor() string      { return "xAI" }

// Stream: grok's streaming-json carries a tool lifecycle with ids. Probed against the pinned 1.0.25
// client (the v0.2.101 CLI emitted only `thought`, `text` and `end`): every tool opens with a
// `tool_call` under a `toolCallId` and ends at ACP's completed, failed or cancelled — on a
// `tool_call_update`, or on the `tool_call` itself — as the binary's own format notes say the
// stream is "derived from the agent's ACP session updates". A shell command that exits non-zero still completes — its exit code is in
// rawOutput. Re-probe before changing this: the declaration is the authority, and the watchdog
// refuses tool events from a stream that declares none.
func (grokAgent) Stream() StreamSpec {
	return StreamSpec{
		Format: StreamGrokJSON, Flags: []string{"--output-format", "streaming-json"}, TrailingArgs: 2,
		ToolLifecycle: ToolLifecycleIDs,
	}
}

// grokReadOnlyTools locks a consult to file-read + search only. grok's --permission-mode
// plan is a NO-OP in headless (only bypassPermissions takes effect via that flag —
// artifacts/doc-14-headless-mode.md), so it can't make a peer read-only. With --tools set,
// ONLY the listed tools exist and default/MCP tool injection is disabled, so the agent
// physically can't edit, write, or run shell — a genuine read-only advisor.
const grokReadOnlyTools = "read_file,grep,list_dir"

// base is grok's command plus the resolved model. The box IS the sandbox, so the default
// bakes in bypassPermissions — the ONE permission mode grok honors via --permission-mode in
// headless, and it applies in the TUI too. An empty COOP_GROK_CMD still yields a runnable grok.
func (grokAgent) base(cfg *config.Config) []string {
	b := cfg.Cmd("COOP_GROK_CMD", "grok --permission-mode bypassPermissions")
	if len(b) == 0 { // an explicitly-empty override must still leave a runnable executable
		b = []string{"grok"}
	}
	return withEffort(withModel(b, cfg.ModelFor("grok")), grokAgent{}, cfg.EffortFor("grok"))
}

func (a grokAgent) Interactive(cfg *config.Config) []string { return a.base(cfg) }

// Headless is grok's single-turn form: `grok -p "<prompt>"` prints one response and exits.
// -p/--single takes the prompt as its VALUE, so the prompt must be the token right after it
// (never a flag) — hence it's appended last, after base's model/permission flags.
func (a grokAgent) Headless(cfg *config.Config, prompt string) []string {
	return append(a.base(cfg), "-p", prompt)
}

func (a grokAgent) HeadlessSession(cfg *config.Config, prompt, id string, resume bool) ([]string, bool) {
	if !ValidSessionID(id) {
		return nil, false
	}
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	return append(a.base(cfg), flag, id, "-p", prompt), true
}

// ACP is grok's own binary running an ACP (JSON-RPC-over-stdio) server. The model flag
// belongs to `grok agent` and must come BEFORE the `stdio` mode (the stdio subcommand takes
// no options — artifacts/doc-15-agent-mode-ACP.md), so it's `grok agent [--model <m>] stdio`.
func (grokAgent) ACP(cfg *config.Config) []string {
	a := withEffort(withModel([]string{"grok", "agent"}, cfg.ModelFor("grok")), grokAgent{}, cfg.EffortFor("grok"))
	return append(a, "stdio")
}

// ACPSessionDirs: grok persists sessions under ~/.grok/sessions/ (organized by working
// directory, alongside a session_search.sqlite index). Share it so an ACP box keeps the
// conversation across a credential switch.
func (grokAgent) ACPSessionDirs() []string { return []string{"sessions"} }

// ACPFinalChunk: every assistant chunk is answer text — grok's adapter streams no separate commentary phase.
func (grokAgent) ACPFinalChunk(json.RawMessage) bool    { return true }
func (grokAgent) ACPProgressChunk(json.RawMessage) bool { return false }

// PresetSessionID: grok's -s/--session-id names a NEW conversation by UUID and --resume
// re-enters one, so coop can pin its own id like claude/gemini.
func (grokAgent) PresetSessionID() bool { return true }

func (a grokAgent) StartSession(cfg *config.Config, id string) []string {
	if id == "" {
		return a.Interactive(cfg)
	}
	return append(a.base(cfg), "--session-id", id)
}

// Resume re-enters the coop-owned session id in Grok's cwd-scoped native store. No exact
// cwd/id match means fresh, so another fork, loop, or consult session cannot be selected.
func (a grokAgent) Resume(cfg *config.Config, ws, id string) ([]string, bool) {
	if ValidSessionID(id) && grokHasSession(cfg, ws, id) {
		return append(a.base(cfg), "--resume", id), true
	}
	return a.Interactive(cfg), false
}

// grokHasSession matches Grok's native sessions/<cwd-bucket>/<session-id> layout. Ordinary
// buckets URL-encode cwd; overlong names record it in a bounded .cwd file instead.
func grokHasSession(cfg *config.Config, ws, id string) bool {
	root, err := openSessionRoot(filepath.Join(cfg.AgentDir("grok"), "sessions"))
	if err != nil {
		return false
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return false
	}
	entries, _ := dir.ReadDir(-1)
	_ = dir.Close()
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if grokBucketCWD(root, entry.Name()) != ws {
			continue
		}
		session := filepath.Join(entry.Name(), id)
		info, err := root.Lstat(session)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		marker, err := root.Lstat(filepath.Join(session, "summary.json"))
		if err == nil && marker.Mode().IsRegular() && marker.Mode()&os.ModeSymlink == 0 {
			return true
		}
	}
	return false
}

func grokBucketCWD(root *os.Root, bucket string) string {
	if cwd, err := url.PathUnescape(bucket); err == nil && filepath.IsAbs(cwd) {
		return cwd
	}
	path := filepath.Join(bucket, ".cwd")
	info, err := root.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4<<10 {
		return ""
	}
	f, err := root.Open(path)
	if err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, (4<<10)+1))
	_ = f.Close()
	if err != nil || len(data) > 4<<10 {
		return ""
	}
	cwd := strings.TrimSpace(string(data))
	if !filepath.IsAbs(cwd) {
		return ""
	}
	return cwd
}

func (grokAgent) LoginConfig(*config.Config) (MCPConfig, error) { return MCPConfig{}, nil }

// Login: device-code flow for the box (no browser, and grok's OAuth redirect can't reach the
// host), mirroring codex's split.
func (grokAgent) Login(*config.Config) []string {
	return []string{"grok", "login", "--device-auth"}
}

// ConsultCmd is the read-only peer command — locked read-only via the tool allowlist
// (see grokReadOnlyTools), NOT --permission-mode plan (a no-op in headless). -p takes the
// prompt as its value, so the question goes last.
func (grokAgent) ConsultCmd(question string) []string {
	return []string{"grok", "--tools", grokReadOnlyTools, "-p", question}
}

// RestrictedCommand: no restricted mode is qualified on this CLI yet (see unqualifiedRestrictedCommand).
func (a grokAgent) RestrictedCommand(mode ExecutionMode, cmd []string) ([]string, error) {
	return unqualifiedRestrictedCommand(a, mode, cmd)
}

// ACPRestrictedSessionMeta: nor on its ACP adapter (see unqualifiedRestrictedACPSession).
func (a grokAgent) ACPRestrictedSessionMeta(mode ExecutionMode) (map[string]any, error) {
	return unqualifiedRestrictedACPSession(a, mode)
}

// Packages is empty: grok is a native binary, not an npm package.
func (grokAgent) Packages() []string { return nil }

// Models are grok's current model ids. Illustrative — any id the CLI accepts works.
func (grokAgent) Models() []string {
	return []string{"grok-4.5", "grok-composer-2.5-fast"}
}

// ExampleModel: the flagship id.
func (grokAgent) ExampleModel() string { return "grok-4.5" }

// ModelEnv: grok reads no default-model env var; the model is -m/--model or config.toml.
func (grokAgent) ModelEnv() string { return "" }

// Effort: grok takes --reasoning-effort <level> (alias --effort) on `grok` and `grok agent`.
func (grokAgent) Effort() EffortSpec {
	return EffortSpec{Flag: "--reasoning-effort", Aliases: []string{"--effort"}}
}

// EffortEnv: grok reads no effort env var; the flag in base()/ACP is the coop-driven path.
func (grokAgent) EffortEnv() string { return "" }

// InstructionFile: grok's primary project-rules file is AGENTS.md (it also reads CLAUDE.md
// for compatibility).
func (grokAgent) InstructionFile() string { return "AGENTS.md" }

func (grokAgent) NativeSubagents() NativeSubagentSupport { return NativeSubagentSupport{} }

func (grokAgent) AuthMarker() (file, envKey string) { return "auth.json", "XAI_API_KEY" }

func (grokAgent) HostCredential() HostCredentialSpec { return HostCredentialSpec{} }

// CredentialEnvKeys is grok's only token env var (the OIDC/auth-provider vars configure a
// mechanism, not a token coop scopes).
func (grokAgent) CredentialEnvKeys() []string { return []string{"XAI_API_KEY"} }

func (grokAgent) CredentialBroker() CredentialBrokerSpec { return CredentialBrokerSpec{} }

func (grokAgent) StoredAPIKey(profileDir string) (bool, error) {
	data, present, err := readDefaultsFile(filepath.Join(profileDir, "auth.json"))
	if err != nil {
		return false, err
	}
	if !present {
		return false, nil
	}
	var credentials map[string]struct {
		Key      string `json:"key"`
		AuthMode string `json:"auth_mode"`
	}
	if err := json.Unmarshal(data, &credentials); err != nil {
		return false, err
	}
	for _, credential := range credentials {
		if credential.AuthMode == "api_key" && credential.Key != "" {
			return true, nil
		}
	}
	return false, nil
}

func (grokAgent) LiveCredentials() LiveCredentialSpec {
	return LiveCredentialSpec{
		Artifacts: []CredentialArtifact{{
			Name: "auth.json", Primary: true, Project: projectGrokCredential,
		}},
		Portability: grokCredentialPortability,
		Prepare:     renewGrokCredential,
		AuthSignals: []string{"not signed in", "authentication required", "unauthorized"},
	}
}

type grokAccessCredential struct {
	Key           string `json:"key"`
	ExpiresAt     string `json:"expires_at"`
	AuthMode      string `json:"auth_mode"`
	OIDCIssuer    string `json:"oidc_issuer"`
	OIDCClientID  string `json:"oidc_client_id"`
	PrincipalID   string `json:"principal_id"`
	PrincipalType string `json:"principal_type"`
	UserID        string `json:"user_id"`
	TeamID        string `json:"team_id"`
	CreateTime    string `json:"create_time"`
}

type grokSourceCredential struct {
	grokAccessCredential
	RefreshToken string `json:"refresh_token"`
}

func decodeGrokSourceCredential(data []byte) (map[string]grokSourceCredential, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, fmt.Errorf("decode Grok credential: %w", err)
	}
	if len(source) == 0 {
		return nil, fmt.Errorf("grok credential has no access-only auth shape")
	}
	credentials := make(map[string]grokSourceCredential, len(source))
	for key, raw := range source {
		var entry grokSourceCredential
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("grok credential contains a non-object entry")
		}
		if entry.Key == "" || entry.AuthMode == "" || entry.OIDCIssuer == "" ||
			entry.OIDCClientID == "" || entry.PrincipalID == "" || entry.PrincipalType == "" ||
			entry.UserID == "" || entry.TeamID == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.ExpiresAt); err != nil {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.CreateTime); err != nil {
			continue
		}
		credentials[key] = entry
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("grok credential has no access-only auth shape")
	}
	return credentials, nil
}

func decodeGrokAccessCredential(data []byte) (map[string]grokAccessCredential, error) {
	source, err := decodeGrokSourceCredential(data)
	if err != nil {
		return nil, err
	}
	projected := make(map[string]grokAccessCredential, len(source))
	for key, entry := range source {
		projected[key] = entry.grokAccessCredential
	}
	return projected, nil
}

func projectGrokCredential(data []byte) ([]byte, error) {
	projected, err := decodeGrokAccessCredential(data)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil, fmt.Errorf("encode Grok credential: %w", err)
	}
	return append(encoded, '\n'), nil
}

func (a grokAgent) ActiveCredentialEnvKeys(_ string, markerPresent bool) []string {
	if markerPresent {
		return nil
	}
	return a.CredentialEnvKeys()
}

// readGrokCredentials decodes the profile's stored login; false means there is none usable.
func readGrokCredentials(profileDir string) (map[string]grokSourceCredential, bool) {
	data, err := os.ReadFile(filepath.Join(profileDir, "auth.json"))
	if err != nil {
		return nil, false
	}
	credentials, err := decodeGrokSourceCredential(data)
	return credentials, err == nil
}

func (grokAgent) StoredCredentialStatus(profileDir string, now time.Time) StoredCredentialStatus {
	credentials, ok := readGrokCredentials(profileDir)
	if !ok {
		return StoredCredentialReauthRequired
	}
	for _, credential := range credentials {
		expiresAt, _ := time.Parse(time.RFC3339Nano, credential.ExpiresAt)
		if credential.RefreshToken != "" || expiresAt.After(now) {
			return StoredCredentialReady
		}
	}
	return StoredCredentialReauthRequired
}

func grokCredentialPortability(profileDir string, deadline time.Time) CredentialPortability {
	credentials, ok := readGrokCredentials(profileDir)
	if !ok {
		return CredentialRefreshRequired
	}
	for _, credential := range credentials {
		expiresAt, err := time.Parse(time.RFC3339Nano, credential.ExpiresAt)
		if err == nil && expiresAt.After(deadline) {
			return CredentialPortable
		}
	}
	return CredentialRefreshRequired
}

// MCP: Grok reads [mcp_servers.*] TOML from ~/.grok/config.toml. Its HTTP servers accept headers
// with ${VAR} expansion, so GenerateGrok translates shared bearer references and returns the names
// whose values the box must capture. The user's other settings are preserved and never rewritten.
func (grokAgent) MCP(cfg *config.Config, _ string) (MCPConfig, error) {
	if cfg.MCPFile == "" {
		return MCPConfig{}, nil
	}
	gx, requiredEnv, err := mcp.GenerateGrok(cfg.MCPFile, filepath.Join(cfg.AgentDir("grok"), "config.toml"))
	if err != nil {
		return MCPConfig{}, err
	}
	return MCPConfig{
		Mounts:      []MCPMount{{Content: gx, BoxPath: cfg.HomeInBox + "/.grok/config.toml"}},
		RequiredEnv: requiredEnv,
	}, nil
}

// ACPMCPServers is nil: this agent's ACP adapter reads the config.toml MCP mounts,
// so passing the servers again would register every one of them twice.
func (grokAgent) ACPMCPServers(string, func(string) (string, bool)) ([]map[string]any, error) {
	return nil, nil
}

// EnsureDefaults is a no-op: grok launches in the mounted repo (a project dir) with its
// auth.json mounted, so it goes straight to work without a first-run prompt to pre-answer.
// (Any config.toml keys a fresh box turns out to need are a box-verified finalization item.)
func (grokAgent) EnsureDefaults(*config.Config, string) error { return nil }

// ACPRateLimitSignals: the pinned 1.0.25 ACP adapter, replayed against each quota status, reports a
// 402 — its "run out of credits" — as a generic -32603 "Internal error" whose only mark is
// data.http_status 402, set from the HTTP response whatever the server's body says. Its 429 needs no
// signal: the error's own message is "Rate limited", which the shared prose check already reads.
func (grokAgent) ACPRateLimitSignals() []ACPSignal {
	return []ACPSignal{{Key: "http_status", Value: "402"}}
}

// ACPSessionSettings: Grok carries Coop's restart target in the `grok agent ... stdio` launch
// command. Its adapter exposes models through session/new and session/set_model; a cross-agent
// selection asks for a fresh session, which the ACP controller handles by restarting at this target.
func (grokAgent) ACPSessionSettings(Target) []ACPSessionSetting { return nil }

// BoxEnv: grok reads its config + auth from ~/.grok, where coop mounts its profile. The one
// variable is the client's telemetry switch. Left on, the pinned 1.0.25 client looks up
// api.mixpanel.com and grok.com dozens of times per prompt, and api.x.ai too — 128 blocked lookups
// in one filtered run, a burst alert every time, and detail the record had to drop — and with it
// off it looks up none of them and answers the same.
func (grokAgent) BoxEnv(string) []string { return []string{"GROK_TELEMETRY_ENABLED=false"} }

func (grokAgent) HomeFallbacks() []HomeFallback { return nil }

// Native 1.0.25 fresh/resume captures (2026-09-13): text deltas exclude thought,
// end carries invocation-local usage/cost, and output_tokens includes reasoning.
// A total matching input+output identifies this format; otherwise keep older
// streams' separate reasoning count, as the loop decoder does.
const grokConsultText = `grok_text() {
	jq -ers 'select(all(.[]; type=="object")) | select(.[-1].type=="end")
		| select([.[] | select(.type=="end")]|length==1)
		| select(all(.[]; .type!="error"))
		| [.[] | select(.type=="text") | .data | select(type=="string")]
		| join("") | select(test("[^[:space:]]"))'
}
grok_delegate_text() {
	jq --unbuffered -jr '
		if .type=="text" and (.data|type)=="string" then .data
		elif .type=="error" and (.message|type)=="string" then .message, "\n"
		elif .type=="end" then "\n"
		else empty end'
}
`

const grokConsultUsage = `select(.[-1].type=="end")
		| select([.[] | select(.type=="end")]|length==1) | .[-1]
		| select((.usage|type)=="object")
		| {input:.usage.input_tokens, fresh:.usage.input_tokens, output:.usage.output_tokens,
		   read:(.usage.cache_read_input_tokens // 0), write:(.usage.cache_creation_input_tokens // 0),
		   reasoning:(.usage.reasoning_tokens // 0), total:.usage.total_tokens, cost:.total_cost_usd}
		| select(.input|token) | select(.output|token) | select(.read|token) | select(.write|token)
		| .input=(.input + .read + .write) | select(.input<=1000000000)
		| if .total>0 and .total==(.input + .output) then .
		  else select(.reasoning|token) | .output=(.output + .reasoning) | select(.output<=1000000000) end
		| if (.cost|type=="number" and isfinite and .>=0) then . else del(.cost) end`

func (grokAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$candidate_idfile\"\n" +
		`grok_run grok --tools "` + grokReadOnlyTools + `" --session-id "$id" --output-format streaming-json ${model:+--model "$model"} ${effort:+--reasoning-effort "$effort"} -p "$prompt"`
}

func (grokAgent) ConsultResume() string {
	return `grok_run grok --tools "` + grokReadOnlyTools + `" --resume "$id" --output-format streaming-json ${model:+--model "$model"} ${effort:+--reasoning-effort "$effort"} -p "$prompt"`
}

func (grokAgent) DelegateExec() string {
	return `grok --permission-mode bypassPermissions --output-format streaming-json ${model:+--model "$model"} ${effort:+--reasoning-effort "$effort"} -p "$prompt"`
}

func (grokAgent) UsagePrelude() string {
	return grokConsultText + consultPeerRowShell("grok", grokConsultUsage)
}
func (a grokAgent) ShellPrelude() string {
	return a.UsagePrelude() + consultCaptureShell("grok", "Grok")
}

// InstallScript bakes grok's CLI into the box image. grok ships a piped installer
// (`curl … | bash`), not npm and not a checksummed release — so, per the settled supply-chain
// call, coop runs THAT (we don't invent a checksum grok doesn't publish; matching how grok
// distributes). The installer symlinks /usr/local/bin/grok into $HOME/.grok (root's home during
// this root build layer), which the box's non-root `node` user can't traverse — so we resolve
// the real binary and replace the symlink with a world-executable copy, verified as the node
// user in a box e2e. `curl -f` fails the build on an HTTP error instead of piping an error page.
func (grokAgent) InstallScript() string {
	return `curl -fsSL https://x.ai/cli/install.sh | bash` +
		` && b="$(readlink -f /usr/local/bin/grok)" && rm -f /usr/local/bin/grok && install -m 0755 "$b" /usr/local/bin/grok`
}

// LockedClients uses the exact native build whose CLI and ACP behavior is
// retained in this repository. The immutable vendor object is checked against
// Coop's embedded digest before it is decompressed or made executable.
func (grokAgent) LockedClients(platform ClientPlatform) []LockedClient {
	if !platform.valid() {
		return nil
	}
	arch, digest := "aarch64", "c401805423a934de6ae1544da5ab210ad406fd446328e6b06557f1a3a003721c"
	if platform.Architecture == "amd64" {
		arch, digest = "x86_64", "54bfe73e542b2207a21a5888f58ceb2e4fb22bccc66d53d19b54bc8289cc2476"
	}
	destination := lockedClientRoot + "/native/grok"
	client := LockedClient{Version: "1.0.25", Binary: "grok", Exec: []string{destination},
		RequiredExecutables: []LockedExecutable{{Path: destination, Version: "1.0.25"}},
		NativeArtifact: &LockedNativeArtifact{
			URL:    "https://storage.googleapis.com/grok-build-public-artifacts/cli/grok-1.0.25-linux-" + arch + ".gz",
			SHA256: digest, Destination: destination,
		}}
	cli, acp := client, client
	cli.Client, acp.Client = egress.ClientCLI, egress.ClientACP
	return []LockedClient{cli, acp}
}

func (a grokAgent) NetworkBundle(input NetworkBundleInput) (egress.Bundle, error) {
	return directNetworkBundle(a.Name(), "access-file", input,
		[]string{"auth.x.ai", "cli-chat-proxy.grok.com", "code.grok.com"},
		[]string{"https://storage.googleapis.com/grok-build-public-artifacts/cli/grok-1.0.25-linux-aarch64.gz"})
}

// NetworkAuthSelection asks for a long-lived access token only where nothing can renew one. A box
// that mounts this profile renews its own: the pinned client refreshes through auth.x.ai, which the
// bundle allows, and writes the new pair back to the mounted file. A credential without a refresh
// token — a session's access-only projection — has to outlive the horizon by itself.
func (grokAgent) NetworkAuthSelection(profileDir string, markerPresent bool) (NetworkAuthSelection, error) {
	if !markerPresent {
		return NetworkAuthSelection{}, fmt.Errorf("grok API-key authentication is unsupported for restricted networking")
	}
	return NetworkAuthSelection{AuthMode: "access-file", RequirePortable: !grokCanRefresh(profileDir)}, nil
}

const (
	// grokIssuer is the pinned client's OIDC issuer, and grokTokenURL the token endpoint its
	// discovery document names. Both are constants, never read from the stored login: the profile
	// is mounted read-write into boxes, so a stored or discovered endpoint would let an agent aim
	// the host's refresh — a POST of fields it chose — anywhere, past any network filter.
	grokIssuer          = "https://auth.x.ai"
	grokTokenURL        = "https://auth.x.ai/oauth2/token"
	grokCredentialLimit = 1 << 20
	// grokLockWait bounds the wait for the client's lock. The client holds it across a refresh and
	// its auth recovery — seconds — and a session turn must not stall behind a wedged holder.
	grokLockWait = 30 * time.Second
)

var errGrokCredentialChanged = errors.New("grok credential changed during refresh")

// renewGrokCredential renews the stored login on the host before a session projects an access-only
// copy of it, the way the pinned client does itself: under the client's own lock (a flock on
// auth.json.lock, which the binary re-reads after waiting on — its "refresh adopted sibling" path),
// with the same form fields, and the rotated pair written back in place. A login that already
// outlives the deadline is left alone: refresh tokens rotate, so a needless refresh is a needless
// chance to lose a working login.
func renewGrokCredential(profileDir string, deadline time.Time) error {
	path := filepath.Join(profileDir, "auth.json")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open Grok credential lock: %w", err)
	}
	defer lock.Close()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return errors.New("grok credential lock is unsafe")
	}
	if err := lockGrokCredential(lock, deadline); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	for range 3 {
		err := renewGrokCredentialLocked(path, deadline)
		if !errors.Is(err, errGrokCredentialChanged) {
			return err
		}
	}
	return errors.New("grok credential changed repeatedly during refresh")
}

// lockGrokCredential takes the client's lock, giving up at the turn deadline or after grokLockWait.
func lockGrokCredential(lock *os.File, deadline time.Time) error {
	giveUp := time.Now().Add(grokLockWait)
	if deadline.Before(giveUp) {
		giveUp = deadline
	}
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock Grok credential refresh: %w", err)
		}
		if time.Now().After(giveUp) {
			return errors.New("grok credential is being refreshed by another process — try again")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func renewGrokCredentialLocked(path string, deadline time.Time) error {
	data, err := readGrokCredentialFile(path)
	if err != nil {
		return fmt.Errorf("read Grok credential for refresh: %w", err)
	}
	credentials, err := decodeGrokSourceCredential(data)
	if err != nil {
		return fmt.Errorf("decode Grok credential for refresh: %w", err)
	}
	for _, credential := range credentials {
		if expiresAt, err := time.Parse(time.RFC3339Nano, credential.ExpiresAt); err == nil && expiresAt.After(deadline) {
			return nil // a sibling may have renewed it while this waited on the lock: adopt it
		}
	}
	var renewable []string
	foreign := false
	for key, credential := range credentials {
		switch {
		case credential.RefreshToken == "":
		case credential.OIDCIssuer != grokIssuer:
			foreign = true // another issuer's login is not one coop may refresh
		default:
			renewable = append(renewable, key)
		}
	}
	if len(renewable) == 0 && foreign {
		// The stored issuer is box-writable, so it is not repeated back to the operator.
		return errors.New("grok credential is from an issuer other than " + grokIssuer + " — sign in again")
	}
	if len(renewable) == 0 {
		return errors.New("grok credential needs sign-in")
	}
	slices.Sort(renewable) // one order every time, not the map's
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("decode Grok credential document: %w", err)
	}
	var expiries []time.Time
	var refreshErr error
	for _, key := range renewable {
		response, err := requestGrokCredentialRefresh(credentials[key], deadline)
		if err == nil {
			var entry json.RawMessage
			var expiresAt time.Time
			if entry, expiresAt, err = mergeGrokCredentialRefresh(document[key], response); err == nil {
				document[key] = entry
				expiries = append(expiries, expiresAt)
				continue
			}
		}
		refreshErr = err
		break
	}
	if len(expiries) == 0 {
		return refreshErr // nothing rotated: the stored login is exactly as it was
	}
	renewed, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("encode refreshed Grok credential: %w", err)
	}
	current, err := readGrokCredentialFile(path)
	if err != nil {
		return fmt.Errorf("re-read Grok credential before refresh persistence: %w", err)
	}
	if !bytes.Equal(current, data) {
		return errGrokCredentialChanged
	}
	if err := config.WriteFileAtomic(path, renewed); err != nil {
		return fmt.Errorf("persist refreshed Grok credential: %w", err)
	}
	// Persist first, judge second: a grant already rotated its refresh token upstream, so what came
	// back is the only working login left — even when a later entry's refresh failed, and whether
	// or not it covers this turn.
	if refreshErr != nil {
		return refreshErr
	}
	for _, expiresAt := range expiries {
		if !expiresAt.After(deadline) {
			return errors.New("renewed Grok credential expires before the turn deadline")
		}
	}
	return nil
}

// mergeGrokCredentialRefresh edits the renewed fields into one stored entry, as the client writes
// them — key, refresh_token, create_time and expires_at — and keeps every field coop does not model.
func mergeGrokCredentialRefresh(stored json.RawMessage, response grokRefreshResponse) (json.RawMessage, time.Time, error) {
	if response.AccessToken == "" || response.ExpiresIn <= 0 {
		return nil, time.Time{}, errors.New("grok credential refresh returned an unusable access token")
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(stored, &entry); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode Grok credential entry: %w", err)
	}
	issued := time.Now().UTC()
	expiresAt := issued.Add(time.Duration(response.ExpiresIn) * time.Second)
	fields := map[string]string{
		"key": response.AccessToken, "create_time": issued.Format(time.RFC3339Nano), "expires_at": expiresAt.Format(time.RFC3339Nano),
	}
	if response.RefreshToken != "" {
		fields["refresh_token"] = response.RefreshToken // an omitted one means the stored one still stands
	}
	for name, value := range fields {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, time.Time{}, err
		}
		entry[name] = encoded
	}
	renewed, err := json.Marshal(entry)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("encode Grok credential entry: %w", err)
	}
	return renewed, expiresAt, nil
}

type grokRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// requestGrokCredentialRefresh sends the exact form the pinned 1.0.25 client sends, captured
// against a logging issuer: grant_type, refresh_token, client_id, principal_type and principal_id.
func requestGrokCredentialRefresh(credential grokSourceCredential, deadline time.Time) (grokRefreshResponse, error) {
	endpoint := strings.TrimSpace(os.Getenv("GROK_REFRESH_TOKEN_URL_OVERRIDE"))
	if endpoint == "" {
		endpoint = grokTokenURL
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname())) {
		return grokRefreshResponse{}, errors.New("grok credential refresh endpoint is unsafe")
	}
	form := url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {credential.RefreshToken}, "client_id": {credential.OIDCClientID},
		"principal_type": {credential.PrincipalType}, "principal_id": {credential.PrincipalID},
	}
	requestDeadline := time.Now().Add(30 * time.Second)
	if deadline.Before(requestDeadline) {
		requestDeadline = deadline
	}
	ctx, cancel := context.WithDeadline(context.Background(), requestDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return grokRefreshResponse{}, fmt.Errorf("create Grok credential refresh: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return grokRefreshResponse{}, fmt.Errorf("refresh Grok credential: %w", err)
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 64<<10)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, limited)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusBadRequest {
			return grokRefreshResponse{}, errors.New("grok credential needs sign-in")
		}
		return grokRefreshResponse{}, errors.New("grok credential refresh failed with HTTP " + strconv.Itoa(resp.StatusCode))
	}
	var result grokRefreshResponse
	if err := json.NewDecoder(limited).Decode(&result); err != nil {
		return grokRefreshResponse{}, fmt.Errorf("decode Grok credential refresh: %w", err)
	}
	return result, nil
}

// readGrokCredentialFile reads the stored login without following a link, bounded.
func readGrokCredentialFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("credential is not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, grokCredentialLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > grokCredentialLimit {
		return nil, errors.New("credential is too large")
	}
	return data, nil
}

// grokCanRefresh reports whether the stored login carries refresh authority — its presence, never
// its value.
func grokCanRefresh(profileDir string) bool {
	credentials, _ := readGrokCredentials(profileDir)
	for _, credential := range credentials {
		if credential.RefreshToken != "" {
			return true
		}
	}
	return false
}

func (grokAgent) ModelCatalog() ModelCatalogSpec {
	return ModelCatalogSpec{HostCommand: []string{"grok", "models"}, ParseHost: func(out []byte) ([]Model, error) {
		// The host CLI's login is distinct from a Coop boxed profile.
		if grokUnauthenticated(out) {
			return nil, ModelCatalogError("The host grok CLI is not signed in.")
		}
		return parseGrokModels(out), nil
	}}
}

func grokUnauthenticated(out []byte) bool {
	return bytes.Contains(out, []byte("not authenticated"))
}

// parseGrokModels reads `grok models` output — a bullet per model, the default marked:
//
//   - grok-4.5 (default)
//   - grok-composer-2.5-fast
//
// It returns ids in listed order (name = id; grok prints no separate display name), skipping
// blanks and duplicates.
func parseGrokModels(out []byte) []Model {
	var models []Model
	seen := map[string]bool{}
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(line, "* ")
		if !ok {
			rest, ok = strings.CutPrefix(line, "- ")
		}
		if !ok {
			continue
		}
		fields := strings.Fields(rest) // the id, then an optional " (default)" marker
		if len(fields) == 0 || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		models = append(models, Model{ID: fields[0], Name: fields[0]})
	}
	return models
}
