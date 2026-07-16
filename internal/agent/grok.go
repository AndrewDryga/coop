package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/mcp"
)

type grokAgent struct{}

func init() { register(grokAgent{}) }

func (grokAgent) Name() string        { return "grok" }
func (grokAgent) DisplayName() string { return "Grok" }
func (grokAgent) Stream() StreamSpec {
	return StreamSpec{
		Format: StreamGrokJSON, Flags: []string{"--output-format", "streaming-json"}, TrailingArgs: 2,
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

// Login: device-code flow for the box (no browser, and grok's OAuth redirect can't reach the
// host), mirroring codex's split.
func (grokAgent) Login(*config.Config) []string {
	return []string{"grok", "login", "--device-auth"}
}

// ConsultCmd is the read-only fusion-peer command — locked read-only via the tool allowlist
// (see grokReadOnlyTools), NOT --permission-mode plan (a no-op in headless). -p takes the
// prompt as its value, so the question goes last.
func (grokAgent) ConsultCmd(question string) []string {
	return []string{"grok", "--tools", grokReadOnlyTools, "-p", question}
}

// Packages is empty: grok is a native binary, not an npm package.
func (grokAgent) Packages() []string { return nil }

// Models are grok's current model ids. Illustrative — any id the CLI accepts works.
func (grokAgent) Models() []string {
	return []string{"grok-4.5", "grok-composer-2.5-fast"}
}

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

// CredentialEnvKeys is grok's only token env var (the OIDC/auth-provider vars configure a
// mechanism, not a token coop scopes).
func (grokAgent) CredentialEnvKeys() []string { return []string{"XAI_API_KEY"} }

func (grokAgent) LiveCredentials() LiveCredentialSpec {
	return LiveCredentialSpec{
		Artifacts: []CredentialArtifact{{
			Name: "auth.json", Primary: true, Project: projectGrokCredential,
		}},
		Portability: grokCredentialPortability,
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

func decodeGrokAccessCredential(data []byte) (map[string]grokAccessCredential, error) {
	var source map[string]json.RawMessage
	if err := json.Unmarshal(data, &source); err != nil {
		return nil, fmt.Errorf("decode Grok credential: %w", err)
	}
	if len(source) == 0 {
		return nil, fmt.Errorf("grok credential has no access-only auth shape")
	}
	projected := make(map[string]grokAccessCredential, len(source))
	for key, raw := range source {
		var entry grokAccessCredential
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
		projected[key] = entry
	}
	if len(projected) == 0 {
		return nil, fmt.Errorf("grok credential has no access-only auth shape")
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

func grokCredentialPortability(profileDir string, deadline time.Time) CredentialPortability {
	data, err := os.ReadFile(filepath.Join(profileDir, "auth.json"))
	if err != nil {
		return CredentialRefreshRequired
	}
	credentials, err := decodeGrokAccessCredential(data)
	if err != nil {
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

// MCP: grok reads [mcp_servers.*] TOML from ~/.grok/config.toml — the same schema codex
// uses (artifacts/doc-05-configuration.md), so reuse the codex generator, preserving the
// user's other config.toml settings and mounting the result at grok's config path.
func (grokAgent) MCP(cfg *config.Config) ([]MCPMount, error) {
	gx, err := mcp.GenerateCodex(cfg.MCPFile, filepath.Join(cfg.AgentDir("grok"), "config.toml"))
	if err != nil {
		return nil, err
	}
	return []MCPMount{{Content: gx, BoxPath: cfg.HomeInBox + "/.grok/config.toml"}}, nil
}

// EnsureDefaults is a no-op: grok launches in the mounted repo (a project dir) with its
// auth.json mounted, so it goes straight to work without a first-run prompt to pre-answer.
// (Any config.toml keys a fresh box turns out to need are a box-verified finalization item.)
func (grokAgent) EnsureDefaults(*config.Config, string) {}

// ACPRateLimitSignals: the structured marker grok's ACP adapter embeds on a usage/rate limit
// isn't captured yet (needs a live limit in a box), so declare none — the controller still
// rotates on the cross-provider output-token axis. Add the marker once observed.
func (grokAgent) ACPRateLimitSignals() []ACPSignal { return nil }

// ACPSessionSettings: Grok carries Coop's restart target in the `grok agent ... stdio` launch
// command. Its adapter exposes models through session/new and session/set_model; a cross-agent
// selection asks for a fresh session, which the ACP controller handles by restarting at this target.
func (grokAgent) ACPSessionSettings(Target) []ACPSessionSetting { return nil }

// BoxEnv: grok reads its config + auth from ~/.grok by default, which is where coop mounts
// its profile — nothing extra needed.
func (grokAgent) BoxEnv(string) []string { return nil }

func (grokAgent) HomeFallbacks() []HomeFallback { return nil }

func (grokAgent) ConsultFresh() string {
	return "printf '%s' \"$id\" >\"$candidate_idfile\"\n" +
		`run grok --tools "` + grokReadOnlyTools + `" --session-id "$id" ${model:+--model "$model"} ${effort:+--reasoning-effort "$effort"} -p "$prompt"`
}

func (grokAgent) ConsultResume() string {
	return `run grok --tools "` + grokReadOnlyTools + `" --resume "$id" ${model:+--model "$model"} ${effort:+--reasoning-effort "$effort"} -p "$prompt"`
}

func (grokAgent) DelegateExec() string {
	return `grok --permission-mode bypassPermissions ${model:+--model "$model"} ${effort:+--reasoning-effort "$effort"} -p "$prompt"`
}

func (grokAgent) ShellPrelude() string { return "" }

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
