// Package agent is the registry of coding agents coop can drive. Each agent is one
// file implementing Agent and self-registering; adding or removing an agent is a
// single-file change, and the compiler enforces that every agent answers every
// question — no switch case to forget.
package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

// StreamFormat identifies the provider-owned NDJSON schema emitted by a headless agent.
type StreamFormat uint8

const (
	StreamNone StreamFormat = iota
	StreamClaudeJSON
	StreamCodexJSON
	StreamGeminiJSON
	StreamGrokJSON
)

// StreamSpec describes how a headless command opts into structured output. TrailingArgs
// keeps positional prompts (or a flag/value prompt pair) after the inserted stream flags.
type StreamSpec struct {
	Format       StreamFormat
	Flags        []string
	TrailingArgs int
}

// EffortFlagStyle is how an agent's command expresses one reasoning-effort value.
type EffortFlagStyle uint8

const (
	EffortFlagSplit      EffortFlagStyle = iota // --effort high
	EffortFlagJoined                            // --effort=high
	EffortFlagAssignment                        // -c model_reasoning_effort=high
)

// EffortSpec is the adapter-owned command grammar for reasoning effort. Assignment is used only
// with EffortFlagAssignment; Aliases are alternate flag names accepted by the same grammar.
type EffortSpec struct {
	Style      EffortFlagStyle
	Flag       string
	Aliases    []string
	Assignment string
}

// SessionDiscoverer is the optional capability for an adapter that cannot choose its new
// session ID but can discover the native ID after a run. Forks persist it and resume exactly it.
type SessionDiscoverer interface {
	LatestSessionID(cfg *config.Config, cwd string) string
	SessionIDs(cfg *config.Config, cwd string) []string
}

// ValidSessionID accepts the canonical UUID form used by provider session stores. Session
// metadata is provider-writable, so unchecked values must never reach provider argv.
func ValidSessionID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
			continue
		}
		if !((id[i] >= '0' && id[i] <= '9') || (id[i] >= 'a' && id[i] <= 'f')) {
			return false
		}
	}
	return id[14] >= '1' && id[14] <= '8' && strings.ContainsRune("89ab", rune(id[19]))
}

// openSessionRoot pins a provider-writable history directory across the lstat/open race.
// OpenRoot confines descendants but intentionally follows a symlink in the root path itself.
func openSessionRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("session root is not a real directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("session root changed while opening")
	}
	return root, nil
}

func (s EffortSpec) Args(level string) []string {
	if s.Flag == "" || level == "" {
		return nil
	}
	switch s.Style {
	case EffortFlagJoined:
		return []string{s.Flag + "=" + level}
	case EffortFlagAssignment:
		return []string{s.Flag, s.Assignment + "=" + level}
	default:
		return []string{s.Flag, level}
	}
}

// ACPSettingMethod is how an adapter changes one setting on an established session.
type ACPSettingMethod uint8

const (
	ACPSetConfigOption ACPSettingMethod = iota
	ACPSetModel
)

// ACPSessionSetting is one provider-owned, ordered setting Coop applies after every
// session new/load/recreate. Order is significant when one setting resets another.
type ACPSessionSetting struct {
	Method   ACPSettingMethod
	ConfigID string
	Value    string
}

// NativeSubagent is the provider-neutral role material an adapter may render into its own native
// subagent format. The adapter owns file syntax and destination; preset owns the role semantics.
type NativeSubagent struct {
	Name        string
	Description string
	Model       string
	Effort      string
	Prompt      string
}

// NativeSubagentSupport describes an adapter's complete native-role capability. A zero value
// means unsupported. HomeDir is relative to that adapter's in-box home; Render returns one file.
type NativeSubagentSupport struct {
	HomeDir string
	Render  func(NativeSubagent) (filename, content string)
}

// CredentialArtifact is one adapter-owned file that can be copied into an isolated credential
// home. Name is a single basename. Primary marks the AuthMarker file; every other artifact is an
// optional refresh companion or selector. Project is required: it synthesizes an auth-only
// representation and returns nil data when the source contains no portable credential state.
type CredentialArtifact struct {
	Name    string
	Primary bool
	Project func([]byte) ([]byte, error)
}

// LiveCredentialSpec is the complete adapter-owned boundary for opt-in live compatibility tests.
// Portability inspects only the isolated projected profile, never the source credential.
type LiveCredentialSpec struct {
	Artifacts   []CredentialArtifact
	Portability func(profileDir string, deadline time.Time) CredentialPortability
	AuthSignals []string
}

// CredentialPortability says whether an isolated file credential can be used without carrying
// refresh authority. NotPortable covers host-bound keychains; RefreshRequired covers a portable
// access-token shape whose current token will not outlive the requested deadline.
type CredentialPortability uint8

const (
	CredentialUnknown CredentialPortability = iota
	CredentialPortable
	CredentialRefreshRequired
	CredentialNotPortable
)

// StoredCredentialStatus is the adapter's best-effort validity check for its native credential
// marker. Unknown preserves presence-based behavior for opaque or host-bound stores. Ready includes
// credentials the native CLI can refresh without another login.
type StoredCredentialStatus uint8

const (
	StoredCredentialUnknown StoredCredentialStatus = iota
	StoredCredentialReady
	StoredCredentialReauthRequired
)

// Agent is everything coop needs to drive one coding agent. To add an agent, write a
// new file implementing this interface and self-register it from an init().
type Agent interface {
	Name() string
	// DisplayName is the human product name for UX surfaces (the ACP toolbar dropdowns):
	// "Claude Code", "Codex", … Name() stays the grammar token everywhere a value is parsed.
	DisplayName() string
	// Interactive is the autonomous default command — what `coop <agent>` runs.
	Interactive(cfg *config.Config) []string
	// Headless is the one-shot, non-interactive form carrying a prompt (the loop).
	Headless(cfg *config.Config, prompt string) []string
	// Stream is the agent's structured-output schema and the flags that enable it.
	Stream() StreamSpec
	// ACP is the agent's ACP adapter command over stdio (for editors like Zed). It takes
	// cfg so an adapter that IS the agent's own binary (gemini --acp) can carry the
	// resolved model flag; a separate adapter binary (claude-agent-acp, codex-acp) takes
	// no flags — claude's picks the model up via ModelEnv instead.
	ACP(cfg *config.Config) []string
	// ACPSessionDirs are the agent-home-relative dirs where this agent's ACP adapter keeps session
	// state — the transcript AND any session index/aux state session/load needs (claude keeps a
	// sessions/ index alongside the projects/ transcript). For an ACP box coop bind-mounts a shared,
	// credential-independent copy of each so switching the credential mid-session doesn't lose the
	// conversation — session/load still finds it. Empty → no sharing for this agent.
	ACPSessionDirs() []string
	// Resume re-enters a fork's interactive session, scoped to ws; the bool reports
	// whether a session was found (else the caller starts fresh via StartSession). id
	// is the persisted session id for this (fork, agent, account): preset-id agents resume the
	// coop-owned id; codex resumes its previously discovered native id. Fork launch owns the
	// one-time legacy Codex lookup, so ordinary adapter resumes remain exact-only.
	Resume(cfg *config.Config, ws, id string) ([]string, bool)
	// StartSession is the fresh interactive command under the coop-chosen session id:
	// claude/gemini/grok stamp it via --session-id so a later Resume can pin exactly it;
	// codex ignores id and mints its own. An empty id falls back to Interactive.
	StartSession(cfg *config.Config, id string) []string
	// PresetSessionID reports whether the agent honors a caller-chosen session id. When
	// false (codex), coop allocates none and discovers the native ID around a fresh run.
	PresetSessionID() bool
	// Login authenticates the agent (its token persists in its config dir).
	Login(cfg *config.Config) []string
	// ConsultCmd is the read-only, non-interactive command to ask this agent a
	// question as a fusion peer — it returns analysis and never edits files.
	ConsultCmd(question string) []string
	// InstructionFile is the agent's native global instruction filename, e.g.
	// "CLAUDE.md" — where coop writes the shared INSTRUCTIONS.md and fusion directive.
	InstructionFile() string
	// NativeSubagents owns this adapter's generated native-role format and in-home destination.
	// A zero descriptor means native preset roles degrade to read-only consults.
	NativeSubagents() NativeSubagentSupport
	// AuthMarker is the credential file (under the agent's config dir) it writes on login and
	// its canonical primary env-file key. Presence checks use CredentialEnvKeys so alternate
	// tokens are first-class too.
	AuthMarker() (file, envKey string)
	// CredentialEnvKeys is every env-file key this agent reads a token from — the
	// AuthMarker key plus any alternates it honors (e.g. claude also reads
	// ANTHROPIC_AUTH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN). A scoped run strips all of an
	// out-of-scope agent's keys, so a peer's alternate token can't leak into a box that
	// isn't authorized for it.
	CredentialEnvKeys() []string
	// ActiveCredentialEnvKeys returns the exact env-key authority for one selected account. A
	// nonempty result means those keys, not a stale marker, define presence and execution authority.
	// The caller reports marker presence; adapters decide precedence once from it and their selector.
	ActiveCredentialEnvKeys(profileDir string, markerPresent bool) []string
	// StoredCredentialStatus validates the adapter's native marker for user-facing credential status.
	// It never reads provider-wide env credentials; those retain their presence-based status.
	StoredCredentialStatus(profileDir string, now time.Time) StoredCredentialStatus
	// LiveCredentials declares the access-only credential projection and redacted compatibility
	// diagnostics for this adapter. It is consumed only by opt-in live tests, but compiler-required
	// so a registered provider cannot silently evade the registry-generated suite.
	LiveCredentials() LiveCredentialSpec
	// Models is a short, curated list of model names this agent's CLI accepts — the menu
	// `coop models` shows. Illustrative, not authoritative: model ids churn faster than
	// coop releases, so ANY id the CLI accepts works with --model; coop never validates
	// against this list.
	Models() []string
	// ModelEnv is the environment variable the agent's CLI reads a default model from
	// ("" when it has none). box.Run exports it into the box when a model is resolved, so
	// a separate adapter binary that takes no flags (claude-agent-acp) still honors the
	// chosen model.
	ModelEnv() string
	// Effort is this agent's command grammar for reasoning effort. A zero descriptor means the
	// agent takes no effort flag (gemini has none; a no-flag ACP adapter may use EffortEnv).
	// Levels pass through verbatim; the agent's own CLI validates them.
	Effort() EffortSpec
	// EffortEnv is the environment variable the agent's CLI reads a reasoning effort from
	// ("" when it has none) — the effort analog of ModelEnv, for a no-flag ACP adapter
	// (claude-agent-acp reads CLAUDE_CODE_EFFORT_LEVEL). box.Run exports it when an effort is
	// resolved so that adapter still honors the chosen effort.
	EffortEnv() string
	// MCP returns the config files to mount so the agent sees the shared mcp.json — its
	// native translation (gemini/codex) or none when it reads mcp.json directly (claude).
	MCP(cfg *config.Config) ([]MCPMount, error)
	// EnsureDefaults pre-answers the agent's first-run prompts (theme, folder-trust,
	// sandbox) in its config dir so a fresh box goes straight to work. Best-effort; an
	// agent that needs nothing leaves it empty. workdir is the resolved box cwd.
	EnsureDefaults(cfg *config.Config, workdir string)
	// Packages are the npm packages the box image installs for this agent — its CLI and
	// (if separate) its ACP adapter.
	Packages() []string
	// ACPRateLimitSignals are the STRUCTURED markers this agent's ACP adapter embeds in
	// a JSON-RPC error to signal a rate/usage limit — proof the ACP controller rotates
	// on without parsing prose. The output-token axis (finishReason/stopReason =
	// length/MAX_TOKENS) is a cross-provider convention owned by the controller, not
	// declared here: stopReason is the ACP-protocol stop-reason field and finishReason
	// the common upstream-API leak, so no single adapter owns them.
	ACPRateLimitSignals() []ACPSignal
	// ACPSessionSettings are provider-owned, ordered settings Coop force-applies after a
	// session is (re)established. The target is the complete active provider/model/effort
	// intent. Re-applied on every restart; nil when the adapter uses launch args only.
	ACPSessionSettings(Target) []ACPSessionSetting
	// BoxEnv are env vars this agent's CLI needs inside the box (beyond ModelEnv and
	// credentials), given the box home dir. Exported into every box — a var is inert
	// where its agent isn't running — so a new agent's env needs no box.Run edit.
	BoxEnv(homeInBox string) []string
	// HomeFallbacks are committed repo artifacts Coop may copy into this agent's user-level
	// home for a box run. Each project artifact suppresses its matching fallback. Empty means
	// the agent has no config shape shared through .agent/ beyond workflow skills.
	HomeFallbacks() []HomeFallback
	// ConsultFresh is the shell body for a fresh read-only consult session in the
	// coop-consult wrapper — run against the wrapper's variables $prompt, $id, $model
	// (uniformly resolved), and $candidate_idfile. A fresh arm records only its candidate;
	// the wrapper publishes continuation state after a bounded usable reply. The arm also
	// has the run/new_id helpers. It analyses and reports; it never edits files.
	ConsultFresh() string
	// ConsultResume is the shell body for resuming a consult by the wrapper's validated $id.
	ConsultResume() string
	// DelegateExec is the raw write-capable shell body for coop-delegate, using $prompt,
	// $model, and $effort. It must be one simple command, without a pipeline or control operator,
	// because the wrapper prefixes it with run_delegate to bound the whole provider process group.
	// The wrapper also enforces commit:never and serialization.
	DelegateExec() string
	// ShellPrelude is optional helper-function shell the wrappers emit ONCE before the
	// per-agent case (e.g. codex's output filter); "" for agents that need none.
	ShellPrelude() string
	// InstallScript is a non-npm box-image install command (e.g. an install-script
	// download); "" means this agent installs via Packages() on the npm layer.
	InstallScript() string
}

// projectJSONLeaf emits one scalar nested field and drops sibling auth settings.
func projectJSONLeaf(data []byte, outer, inner, leaf string) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("decode credential selector: %w", err)
	}
	outerData, ok := root[outer]
	if !ok {
		return nil, nil
	}
	var middle map[string]json.RawMessage
	if err := json.Unmarshal(outerData, &middle); err != nil {
		return nil, fmt.Errorf("decode credential selector: %w", err)
	}
	innerData, ok := middle[inner]
	if !ok {
		return nil, nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(innerData, &nested); err != nil {
		return nil, fmt.Errorf("decode credential selector: %w", err)
	}
	value, ok := nested[leaf]
	if !ok {
		return nil, nil
	}
	projected, err := json.Marshal(map[string]map[string]map[string]json.RawMessage{
		outer: {inner: {leaf: value}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode credential selector: %w", err)
	}
	return append(projected, '\n'), nil
}

func jwtExpiresAfter(token string, deadline time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	return json.Unmarshal(payload, &claims) == nil && claims.ExpiresAt > 0 &&
		time.Unix(claims.ExpiresAt, 0).After(deadline)
}

// ClassifyCLIError returns a redacted diagnostic class without retaining provider output.
func ClassifyCLIError(spec LiveCredentialSpec, output string) string {
	lower := strings.ToLower(output)
	if CLIRateLimited(lower) {
		return "rate_limit"
	}
	for _, signal := range spec.AuthSignals {
		if signal != "" && strings.Contains(lower, signal) {
			return "authentication"
		}
	}
	return "process"
}

// withModel applies a resolved model to cmd. A configured model outranks a model baked into
// COOP_<AGENT>_CMD, so an existing --model/-m value is replaced in place; otherwise the common
// `--model <model>` form is appended. Empty leaves a command override's own default untouched.
func withModel(cmd []string, model string) []string {
	if model == "" {
		return cmd
	}
	if out, ok := normalizeFlagValue(cmd, []string{"--model", "-m"}, model); ok {
		return out
	}
	return appendBeforeSeparator(cmd, "--model", model)
}

// normalizeFlagValue replaces the first split (`--flag old`) or joined (`--flag=old`) value,
// removes later duplicates, and leaves tokens after `--` alone. It never mutates cmd.
func normalizeFlagValue(cmd, names []string, value string) ([]string, bool) {
	out := make([]string, 0, len(cmd)+1)
	found := false
	for i := 0; i < len(cmd); i++ {
		arg := cmd[i]
		if arg == "--" {
			out = append(out, cmd[i:]...)
			break
		}
		matched := false
		for _, name := range names {
			switch {
			case arg == name:
				matched = true
				if !found {
					out = append(out, name, value)
				}
				found = true
				if i+1 < len(cmd) && cmd[i+1] != "--" && !strings.HasPrefix(cmd[i+1], "-") {
					i++
				}
			case strings.HasPrefix(arg, name+"="):
				matched = true
				if !found {
					out = append(out, name+"="+value)
				}
				found = true
			}
			if matched {
				break
			}
		}
		if !matched {
			out = append(out, arg)
		}
	}
	if !found {
		return nil, false
	}
	return out, true
}

func appendBeforeSeparator(cmd []string, values ...string) []string {
	for i, arg := range cmd {
		if arg == "--" {
			out := make([]string, 0, len(cmd)+len(values))
			out = append(out, cmd[:i]...)
			out = append(out, values...)
			return append(out, cmd[i:]...)
		}
	}
	return append(cmd, values...)
}

// hasModelFlag reports whether cmd already carries a model flag (--model/-m, split or =-joined).
func hasModelFlag(cmd []string) bool {
	for _, a := range cmd {
		if a == "--" {
			return false
		}
		if a == "--model" || a == "-m" || strings.HasPrefix(a, "--model=") || strings.HasPrefix(a, "-m=") {
			return true
		}
	}
	return false
}

// withEffort applies a resolved effort to cmd. Each CLI supplies its own spelling; a matching
// value baked into COOP_<AGENT>_CMD is replaced so the resolved target/default keeps precedence,
// while empty effort leaves the command override untouched.
func withEffort(cmd []string, a Agent, level string) []string {
	if level == "" {
		return cmd
	}
	spec := a.Effort()
	if spec.Flag == "" {
		return cmd
	}
	names := append([]string{spec.Flag}, spec.Aliases...)
	switch spec.Style {
	case EffortFlagJoined:
		if out, ok := normalizeJoinedFlag(cmd, names, level); ok {
			return out
		}
	case EffortFlagAssignment:
		marker := spec.Assignment + "="
		if out, ok := normalizeAssignmentFlag(cmd, names, marker, marker+level); ok {
			return out
		}
	default:
		if out, ok := normalizeFlagValue(cmd, names, level); ok {
			return out
		}
	}
	return appendBeforeSeparator(cmd, spec.Args(level)...)
}

func normalizeJoinedFlag(cmd, names []string, value string) ([]string, bool) {
	out := make([]string, 0, len(cmd))
	found := false
	for i, arg := range cmd {
		if arg == "--" {
			out = append(out, cmd[i:]...)
			break
		}
		matched := false
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				if !found {
					out = append(out, name+"="+value)
				}
				found, matched = true, true
				break
			}
		}
		if !matched {
			out = append(out, arg)
		}
	}
	if !found {
		return nil, false
	}
	return out, true
}

// normalizeAssignmentFlag replaces and deduplicates `-c key=value`-style options while
// preserving unrelated uses of the same carrier flag.
func normalizeAssignmentFlag(cmd, names []string, marker, value string) ([]string, bool) {
	out := make([]string, 0, len(cmd))
	found := false
	for i := 0; i < len(cmd); i++ {
		if cmd[i] == "--" {
			out = append(out, cmd[i:]...)
			break
		}
		name, matched := matchExact(cmd[i], names)
		if matched && i+1 < len(cmd) && strings.HasPrefix(cmd[i+1], marker) {
			if !found {
				out = append(out, name, value)
			}
			found = true
			i++
			continue
		}
		name, matched = matchJoinedAssignment(cmd[i], names, marker)
		if matched {
			if !found {
				out = append(out, name+"="+value)
			}
			found = true
			continue
		}
		out = append(out, cmd[i])
	}
	if !found {
		return nil, false
	}
	return out, true
}

func matchJoinedAssignment(value string, candidates []string, marker string) (string, bool) {
	for _, candidate := range candidates {
		if strings.HasPrefix(value, candidate+"="+marker) {
			return candidate, true
		}
	}
	return "", false
}

func matchExact(value string, candidates []string) (string, bool) {
	for _, candidate := range candidates {
		if value == candidate {
			return candidate, true
		}
	}
	return "", false
}

// SupportsEffort reports whether the agent has any reasoning-effort control (a CLI flag or an
// env var). A target that names an effort for an agent without one is rejected in ParseTarget.
func SupportsEffort(a Agent) bool {
	return a.Effort().Flag != "" || a.EffortEnv() != ""
}

// Packages is the union of every agent's npm packages, for the box image's install.
func Packages() []string {
	var pkgs []string
	for _, n := range Names() {
		pkgs = append(pkgs, registry[n].Packages()...)
	}
	return pkgs
}

// MCPMount is one generated config file an agent needs to see the shared mcp.json: its
// content and where it mounts inside the box.
type MCPMount struct {
	Content string
	BoxPath string
}

// HomeFallback describes one agent-owned config artifact synthesized from a committed source.
// Paths are repo-relative except Target, which is relative to the agent's user-level home.
type HomeFallback struct {
	Source  string
	Project string
	Target  string
	Dir     bool
}

// ACPSignal is one structured rate-limit marker in an ACP adapter's JSON-RPC errors: a
// string value (optionally pinned to the JSON key carrying it; "" matches any key) that
// structurally proves a rate/usage limit. Matching is compact — lowercased with _-/space
// stripped — so RESOURCE_EXHAUSTED and resourceExhausted are one marker.
type ACPSignal struct {
	Key   string
	Value string
}

var registry = map[string]Agent{}

// register adds an agent to the registry; called from each adapter's init().
func register(a Agent) { registry[a.Name()] = a }

// Get returns the agent registered under name.
func Get(name string) (Agent, bool) { a, ok := registry[name]; return a, ok }

// Valid reports whether name is a known agent.
func Valid(name string) bool { _, ok := registry[name]; return ok }

// Names returns every registered agent name, sorted for a stable order.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
