// Package agent is the registry of coding agents coop can drive. Each agent is one
// file implementing Agent and self-registering; adding or removing an agent is a
// single-file change, and the compiler enforces that every agent answers every
// question — no switch case to forget.
package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"gopkg.in/yaml.v3"
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

// StreamToolLifecycle is what a provider's structured stream proves about the FOREGROUND TOOLS it
// runs — the one capability the attempt watchdog changes policy on. It is a DECLARATION each
// adapter makes about its own schema, never something coop infers at runtime from process names,
// CPU, or the bytes seen so far: a stream that has simply not opened a tool yet is indistinguishable
// from one that never will.
type StreamToolLifecycle uint8

const (
	// ToolLifecycleUndeclared is the zero value — nobody probed this stream. It is not a third
	// behavior: consumers read it as ToolLifecycleAbsent (the conservative side), and
	// TestEveryStreamDeclaresItsToolLifecycle fails so it cannot ship.
	ToolLifecycleUndeclared StreamToolLifecycle = iota
	// ToolLifecycleAbsent: the stream reports no tool start or end at all, so silence during a
	// long foreground gate is indistinguishable from a wedged attempt.
	ToolLifecycleAbsent
	// ToolLifecycleIDs: tool starts and ends arrive under a provider-supplied id the watchdog can
	// pair, so an open tool can suspend the idle deadline and carry its own absolute cap.
	ToolLifecycleIDs
)

// StreamSpec describes how a headless command opts into structured output. TrailingArgs
// keeps positional prompts (or a flag/value prompt pair) after the inserted stream flags.
// ToolLifecycle is the adapter's declaration about its own schema (see StreamToolLifecycle).
type StreamSpec struct {
	Format        StreamFormat
	Flags         []string
	TrailingArgs  int
	ToolLifecycle StreamToolLifecycle
}

// TracksTools reports whether the watchdog may supervise this stream's foreground tools — suspend
// its idle deadline on a tool start and cap the oldest open one. Only an explicit
// ToolLifecycleIDs declaration qualifies: an undeclared stream reads as absent, so a provider
// nobody probed gets the conservative policy rather than a deadline its schema cannot feed.
func (s StreamSpec) TracksTools() bool { return s.ToolLifecycle == ToolLifecycleIDs }

// ExecutionMode is how much of the host a run may reach and where it may write, fixed when the
// run is created and never widened afterwards. Normal is every launch that existed before the
// modes did. ReadOnly and Bare share one restricted filesystem profile — a read-only root, run-
// private tmpfs scratch, nothing host-backed writable, no shared provider state — and differ in
// what they expose: readonly mounts the selected repository read-only to be inspected, bare
// mounts no repository and gives the model no tool at all.
type ExecutionMode string

const (
	ModeNormal   ExecutionMode = "normal"
	ModeReadOnly ExecutionMode = "readonly"
	ModeBare     ExecutionMode = "bare"
)

// ParseExecutionMode accepts exactly the three spellings. The empty string is not a mode here:
// a caller with none says ModeNormal itself, so a misspelled mode can never quietly run as the
// widest one.
func ParseExecutionMode(value string) (ExecutionMode, error) {
	switch mode := ExecutionMode(value); mode {
	case ModeNormal, ModeReadOnly, ModeBare:
		return mode, nil
	}
	return "", fmt.Errorf("unknown execution mode %q — use normal, readonly, or bare", value)
}

// Restricted reports whether the mode runs under the restricted filesystem profile.
func (m ExecutionMode) Restricted() bool { return m == ModeReadOnly || m == ModeBare }

// unqualifiedRestrictedCommand is the answer of an adapter no restricted mode has been proven
// on: normal passes through, anything else refuses by name. Qualification means a live run
// showed the provider's own switch holds (see claudeAgent.RestrictedCommand); until then the
// mode is not offered rather than offered on a promise.
func unqualifiedRestrictedCommand(a Agent, mode ExecutionMode, cmd []string) ([]string, error) {
	if mode == ModeNormal {
		return cmd, nil
	}
	return nil, fmt.Errorf("%s is not qualified for a %s run — its CLI has no proven switch for the mode; use claude", a.Name(), mode)
}

// unqualifiedRestrictedACPSession is the ACP half of the same answer: normal sends no extra
// session parameter, and a restricted mode refuses by name until a live session over the
// adapter proved its switch (see claudeAgent.ACPRestrictedSessionMeta).
func unqualifiedRestrictedACPSession(a Agent, mode ExecutionMode) (map[string]any, error) {
	if mode == ModeNormal {
		return nil, nil
	}
	return nil, fmt.Errorf("%s is not qualified for a %s session — its ACP adapter has no proven switch for the mode; use claude", a.Name(), mode)
}

// hasFlag reports the first of names present in cmd, split (`--flag v`) or joined (`--flag=v`).
func hasFlag(cmd []string, names []string) string {
	for _, arg := range cmd {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return name
			}
		}
	}
	return ""
}

// EffortFlagStyle is how an agent's command expresses one reasoning-effort value.
type EffortFlagStyle uint8

const (
	EffortFlagSplit      EffortFlagStyle = iota // --effort high
	EffortFlagJoined                            // --effort=high
	EffortFlagAssignment                        // -c model_reasoning_effort=high
)

// EffortSpec is the adapter-owned grammar for reasoning effort. Assignment is used only with
// EffortFlagAssignment; Aliases are alternate flag names accepted by the same grammar. Settings
// marks an agent whose CLI takes effort from no flag or variable at all, only from settings its
// adapter generates for the box (see its MCP).
type EffortSpec struct {
	Style      EffortFlagStyle
	Flag       string
	Aliases    []string
	Assignment string
	Settings   bool
	// Validate refuses an effort the agent cannot express for model, naming the choices that work.
	// Nil passes every level through for the agent's own CLI to judge. A Settings agent needs it:
	// its CLI never sees the level, so a bad one would otherwise be silently dropped.
	Validate func(model, effort string) error
}

// SessionDiscoverer is the optional capability for an adapter that cannot choose its new
// session ID but can discover the native ID after a run. Forks persist it and resume exactly it.
type SessionDiscoverer interface {
	SessionIDs(cfg *config.Config, cwd string) []string
	// ProducesSession reports whether launching the agent with these args creates a
	// discoverable interactive session a concurrent producer could collide with — so it
	// needs the interactive-session lock. Codex's `exec` subcommand writes source:"exec"
	// rollouts that discovery excludes, so those are fully concurrent and need no lock.
	ProducesSession(args []string) bool
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
// No renderer lists or restricts tools, so a helper gets what its client grants a definition that
// names none: the lead's full set, as Claude and Gemini document it (Codex and Grok document no
// default, and no live run has shown theirs).
type NativeSubagent struct {
	Name        string
	Description string
	Model       string
	Effort      string
	Prompt      string
}

// NativeSubagentSupport describes an adapter's complete native-role capability. A zero value
// means unsupported. HomeDir is relative to that adapter's in-box home; Render returns one file.
// Effort checks a role's reasoning effort against what the format carries; nil means it carries
// none, so a role that sets one cannot keep it there.
type NativeSubagentSupport struct {
	HomeDir string
	Render  func(NativeSubagent) (filename, content string)
	Effort  func(effort string) error
}

// anyNativeEffort is the Effort check for a format that carries whatever effort the target itself
// accepted.
func anyNativeEffort(string) error { return nil }

// nativeMarkdown renders a Markdown agent definition: YAML frontmatter from fields in order, skipping
// empty values, then the prompt as its body. The YAML is encoded, never pasted: a strict parser
// (Gemini's, Grok's) drops the whole agent over an unquoted `description: Use for: …`.
func nativeMarkdown(prompt string, fields ...[2]string) string {
	var frontmatter yaml.Node
	frontmatter.Kind = yaml.MappingNode
	for _, field := range fields {
		if field[1] != "" {
			frontmatter.Content = append(frontmatter.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: field[0]}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field[1]})
		}
	}
	encoded, err := yaml.Marshal(&frontmatter)
	if err != nil {
		panic(err) // a mapping of plain strings always encodes
	}
	return "---\n" + string(encoded) + "---\n\n" + prompt + "\n"
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

// MarkerCredentialSelector is the optional capability for an adapter whose native marker can
// hold the same selected credential as an environment key. Returning false does not veto the
// existing marker fallback for selectors that declare no environment authority.
type MarkerCredentialSelector interface {
	MarkerProvidesSelectedCredential(profileDir string) bool
}

// HostCredentialSpec is one adapter-owned API-key login that Coop can persist outside the
// provider home mounted into boxes. The adapter owns the human wording and the profile mutation
// that selects this credential family; shared code owns hidden input and private storage. A
// qualified broker consumes the key without exposing it to the box. A zero value means the
// provider keeps its native login flow.
type HostCredentialSpec struct {
	Prompt       string
	Instructions string
	File         string
	EnvKey       string
	Activate     func(profileDir string) error
}

// Declared reports whether an adapter opted into host-owned credential storage at all.
func (s HostCredentialSpec) Declared() bool {
	return s.Prompt != "" || s.Instructions != "" || s.File != "" || s.EnvKey != "" || s.Activate != nil
}

// Valid rejects partial declarations and path-shaped storage names. EnvKey must already belong to
// the adapter's CredentialEnvKeys; the registry test enforces that relationship.
func (s HostCredentialSpec) Valid() bool {
	return s.Declared() && s.Prompt != "" && s.File != "" && filepath.Base(s.File) == s.File &&
		!strings.ContainsAny(s.File, `/\\`) && s.EnvKey != "" && s.Activate != nil
}

// CredentialBrokerSpec is the adapter-owned native HTTP contract Coop may protect without
// placing the reusable credential in a filtered agent container. A zero value means unsupported.
// It describes one deliberately narrow first-party API, never a generic forward proxy.
type CredentialBrokerSpec struct {
	CredentialEnv  string
	BaseURLEnv     string
	Upstream       string
	Header         string
	HeaderPrefix   string
	Method         string
	Path           string
	PathPrefix     bool
	AllowQuery     bool
	ClientBasePath string
	// Config is the system file that points every process of this client in the box — the lead,
	// consult and delegate arms, the ACP adapter — at the broker's base URL, for a client whose
	// BaseURLEnv alone cannot. It replaces the image's file at that path, so it repeats what the
	// image's copy says. Nil when the environment is enough.
	Config func(baseURL string) SystemFile
	Port   int
}

// Declared reports whether an adapter opted into credential brokering at all.
func (s CredentialBrokerSpec) Declared() bool {
	return s.CredentialEnv != "" || s.BaseURLEnv != "" || s.Upstream != "" || s.Header != "" ||
		s.HeaderPrefix != "" || s.Method != "" || s.Path != "" || s.PathPrefix || s.AllowQuery ||
		s.ClientBasePath != "" || s.Config != nil || s.Port != 0
}

// Valid rejects partially declared broker shapes. Exact provider support is still qualified by
// the adapter's locked-client tests; this only keeps malformed declarations out of launch plans.
func (s CredentialBrokerSpec) Valid() bool {
	return s.Declared() && s.CredentialEnv != "" && s.BaseURLEnv != "" && s.Upstream != "" &&
		s.Header != "" && s.Method != "" && strings.HasPrefix(s.Path, "/") &&
		!strings.ContainsAny(s.Path, "?#\x00\r\n") &&
		(s.ClientBasePath == "" || strings.HasPrefix(s.ClientBasePath, "/") &&
			!strings.HasSuffix(s.ClientBasePath, "/") && !strings.ContainsAny(s.ClientBasePath, "?#\x00\r\n")) &&
		!strings.ContainsAny(s.HeaderPrefix, "\x00\r\n") && s.Port >= 1 && s.Port <= 65535
}

// StoredAPIKeyDetector is implemented only by adapters whose native marker can contain a
// reusable API key. Coop uses it to refuse that marker before mounting it when the broker cannot
// safely extract and replace it. OAuth/access-token markers deliberately return false.
type StoredAPIKeyDetector interface {
	StoredAPIKey(profileDir string) (bool, error)
}

// LiveCredentialSpec is the complete adapter-owned boundary for opt-in live compatibility tests.
// Portability inspects only the isolated projected profile, never the source credential.
type LiveCredentialSpec struct {
	Artifacts []CredentialArtifact
	// Prepare runs against the trusted source profile before projection. It may renew an
	// expiring access credential, but refresh authority must remain in the source profile.
	Prepare     func(profileDir string, deadline time.Time) error
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
	// SkillsCapable reports whether the native client discovers project skills.
	SkillsCapable() bool
	// ReviewOutput removes native transport envelopes without owning host verdict grammar.
	ReviewOutput(string, ReviewOutputContract) (string, bool)
	ReviewFooterLine(string) bool
	// PlainOutputProbe recognizes only a complete native denial, not assistant prose.
	PlainOutputProbe() PlainOutputProbe
	// ModelCatalog declares native discovery; the caller owns execution, auth and caching.
	ModelCatalog() ModelCatalogSpec
	Scaffold() ScaffoldSpec
	// DisplayName is the human product name for UX surfaces (the ACP toolbar dropdowns):
	// "Claude Code", "Codex", … Name() stays the grammar token everywhere a value is parsed.
	DisplayName() string
	// Vendor is the company whose service the agent reaches — "Anthropic", "OpenAI" — for the
	// launch line that says whose endpoints a box may reach ("OpenAI endpoints allowed",
	// "Codex cannot reach OpenAI"). DisplayName names the product; this names who is behind it.
	Vendor() string
	// Interactive is the autonomous default command — what `coop <agent>` runs.
	Interactive(cfg *config.Config) []string
	// Headless is the one-shot, non-interactive form carrying a prompt (the loop).
	Headless(cfg *config.Config, prompt string) []string
	// HeadlessSession starts or resumes one exact non-interactive native session. Review uses it
	// once to repair a malformed terminal envelope without paying for a fresh review. The bool is
	// false only when the requested session id is invalid or unsupported.
	HeadlessSession(cfg *config.Config, prompt, id string, resume bool) ([]string, bool)
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
	// ACPFinalChunk reports whether an ACP assistant/agent message chunk carrying meta (the
	// update's `_meta`, possibly empty) is part of the answer. An adapter that streams progress
	// commentary and the final answer through the same chunk event marks them there; every other
	// adapter answers true, the ACP-compatible append behavior. Coop applies this to the assistant
	// text of every admitted prompt, not only structured-output turns.
	ACPFinalChunk(meta json.RawMessage) bool
	// ACPProgressChunk accepts only explicitly public commentary, never reasoning or unknown phases.
	ACPProgressChunk(meta json.RawMessage) bool
	// Resume re-enters a fork's interactive session, scoped to ws; the bool reports
	// whether a session was found (else the caller starts fresh via StartSession). id
	// is the persisted session id for this (fork, agent, account): preset-id agents resume the
	// coop-owned id; codex resumes its previously recorded native id. Every adapter accepts only
	// this exact persisted id.
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
	// LoginConfig supplies only box-managed login settings, never shared MCP or project tools.
	// User authentication state must remain writable in the selected account's home.
	LoginConfig(cfg *config.Config) (MCPConfig, error)
	// ConsultCmd is the read-only, non-interactive command to ask this agent a
	// question as a consult peer — it returns analysis and never edits files.
	ConsultCmd(question string) []string
	// RestrictedCommand rewrites the agent's own command for a restricted ExecutionMode. The box
	// has already fixed the filesystem — a read-only root and repository, run-private scratch —
	// so what is left is what only the provider's own switches can say: in every restricted mode
	// the CLI must ignore the extensions a repository or a home could define (project settings,
	// hooks, project MCP servers), and in bare mode it must also disable every tool, built-in and
	// MCP alike, so the model's request carries none. An adapter whose CLI has no such switch, or
	// that finds a caller argument handing back what the mode takes away, returns an error and the
	// run refuses: a mode the provider cannot enforce is not a weaker one, it is none. ModeNormal
	// returns cmd unchanged.
	RestrictedCommand(mode ExecutionMode, cmd []string) ([]string, error)
	// ACPRestrictedSessionMeta is RestrictedCommand for a session driven over ACP, where the
	// adapter binary takes no flags: it is the `_meta` the ACP client sends with session/new so
	// the adapter starts the provider under the same switches — no repository or home
	// extensions in every restricted mode, no tool at all in bare. The client that launches the
	// session (the session daemon) sends it; the adapter cannot be handed it from inside the box.
	// ModeNormal returns nil; an adapter without a proven switch returns an error, and the
	// session refuses, for the same reason RestrictedCommand does.
	ACPRestrictedSessionMeta(mode ExecutionMode) (map[string]any, error)
	// InstructionFile is the agent's native global instruction filename, e.g.
	// "CLAUDE.md" — where coop writes the shared or consult-augmented instructions.
	InstructionFile() string
	// NativeSubagents owns this adapter's generated native-role format and in-home destination.
	// A zero descriptor means the adapter hosts no native preset roles, and a preset that needs
	// one under this lead is refused.
	NativeSubagents() NativeSubagentSupport
	// AuthMarker is the credential file (under the agent's config dir) it writes on login and
	// its canonical primary env-file key. Presence checks use CredentialEnvKeys so alternate
	// tokens are first-class too.
	AuthMarker() (file, envKey string)
	// HostCredential declares an optional Coop-owned API-key login. Its file lives outside the
	// mounted provider profile; a zero value uses Login.
	HostCredential() HostCredentialSpec
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
	// CredentialBroker declares one qualified API-key transport whose reusable key can stay in
	// the trusted filtered gateway. Unsupported adapters return the zero value.
	CredentialBroker() CredentialBrokerSpec
	// Models is a short, curated list of model names this agent's CLI accepts — the menu
	// `coop models` shows. Illustrative, not authoritative: model ids churn faster than
	// coop releases, so ANY id the CLI accepts works with --model; coop never validates
	// against this list.
	Models() []string
	// ExampleModel is the one id from Models() that reads best in help examples
	// (`coop claude:opus`) — the familiar name, not necessarily the first or newest entry.
	ExampleModel() string
	// ModelEnv is the environment variable the agent's CLI reads a default model from
	// ("" when it has none). box.Run exports it into the box when a model is resolved, so
	// a separate adapter binary that takes no flags (claude-agent-acp) still honors the
	// chosen model.
	ModelEnv() string
	// Effort is this agent's grammar for reasoning effort. A zero descriptor means the agent takes
	// no effort flag (a no-flag ACP adapter may use EffortEnv). Levels pass through verbatim for
	// the agent's own CLI to validate, unless the spec validates them itself.
	Effort() EffortSpec
	// EffortEnv is the environment variable the agent's CLI reads a reasoning effort from
	// ("" when it has none) — the effort analog of ModelEnv, for a no-flag ACP adapter
	// (claude-agent-acp reads CLAUDE_CODE_EFFORT_LEVEL). box.Run exports it when an effort is
	// resolved so that adapter still honors the chosen effort.
	EffortEnv() string
	// MCP returns how an ordinary agent command sees the shared mcp.json: generated config
	// mounts, command arguments for a direct reader, or both. Box applies CommandArgs only to a
	// command explicitly marked as the agent's own CLI, never to ACP or maintenance commands.
	// workdir is the resolved box cwd so a generated native overlay can carry the same prompt-free
	// defaults as EnsureDefaults without mutating the host before every scoped adapter validates.
	MCP(cfg *config.Config, workdir string) (MCPConfig, error)
	// ACPMCPServers is the same servers as the ACP session parameter, for an adapter that
	// cannot be pointed at a file: it takes no flags, so a mount MCP already covers is no
	// use to it. Nil for every agent whose adapter reads what MCP mounts — sending the list
	// as well would register each server twice. mcpFile is the mcp.json THIS session runs
	// with, which is not always the shared one; lookupEnv resolves a bearer_token_env_var
	// against the environment that session's box gets, since ACP carries headers only.
	ACPMCPServers(mcpFile string, lookupEnv func(string) (string, bool)) ([]map[string]any, error)
	// EnsureDefaults pre-answers the agent's first-run prompts (theme, folder-trust,
	// sandbox) in its config dir so a fresh box goes straight to work. A present invalid
	// settings file is an error, never an empty default. workdir is the resolved box cwd.
	EnsureDefaults(cfg *config.Config, workdir string) error
	// UpdateControls are the switches that stop this agent's client updating itself.
	UpdateControls() UpdateControls
	// LockedClients declares this adapter's exact pinned installations for one
	// platform, used to build the qualified client image restricted networking
	// launches. Nil where the adapter has no qualified locked client.
	LockedClients(platform ClientPlatform) []LockedClient
	// NetworkBundle derives the release-owned core endpoints an already selected
	// target needs. It is connectivity, not activation: it neither authorizes an
	// action nor reads a credential, and an unsupported tuple fails rather than
	// warning. Optional features stay explicit operator requests.
	NetworkBundle(NetworkBundleInput) (egress.Bundle, error)
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
	// UsagePrelude supplies the adapter's native reply decoder and provider-reported usage
	// parser. Consult and delegate wrappers share it; neither invents provider pricing.
	UsagePrelude() string
	// ShellPrelude is optional helper-function shell the wrappers emit ONCE before the
	// per-agent case (e.g. codex's output filter); "" for agents that need none.
	ShellPrelude() string
}

// MarkerProvidesActiveCredential reports whether a present native marker can satisfy this
// profile. Adapters with no selected environment family retain the historical marker behavior;
// an optional selector may additionally recognize a native form of an env-backed credential.
func MarkerProvidesActiveCredential(ag Agent, profileDir string, activeEnvKeys []string) bool {
	if len(activeEnvKeys) == 0 {
		return true
	}
	selector, ok := ag.(MarkerCredentialSelector)
	return ok && selector.MarkerProvidesSelectedCredential(profileDir)
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

// AuthenticationFailure reports whether provider-owned output proves that provider's login failed.
// Signals stay adapter-owned; anchoring them to an error-shaped line keeps ordinary narration about
// authentication from killing a healthy credential or replaying work under another account.
func AuthenticationFailure(provider, output string) bool {
	agent, ok := Get(provider)
	if !ok {
		return false
	}
	for _, raw := range strings.Split(strings.ToLower(output), "\n") {
		line := strings.TrimSpace(raw)
		for _, signal := range agent.LiveCredentials().AuthSignals {
			signal = strings.ToLower(strings.TrimSpace(signal))
			if signal == "" {
				continue
			}
			if line == signal || strings.HasPrefix(line, signal+".") || strings.HasPrefix(line, signal+":") ||
				((strings.HasPrefix(line, "error:") || strings.HasPrefix(line, "fatal:") ||
					strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[")) && strings.Contains(line, signal)) {
				return true
			}
		}
	}
	return false
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

// SupportsEffort reports whether the agent has any reasoning-effort control (a CLI flag, an env
// var, or generated settings). A target that names an effort for an agent without one is rejected
// in ParseTarget.
func SupportsEffort(a Agent) bool {
	spec := a.Effort()
	return spec.Flag != "" || spec.Settings || a.EffortEnv() != ""
}

// ValidateEffort refuses an effort a cannot express for model — the check every surface runs
// before anything starts. An agent whose own CLI judges the level (no EffortSpec.Validate) accepts
// every level here.
func ValidateEffort(a Agent, model, effort string) error {
	if validate := a.Effort().Validate; effort != "" && validate != nil {
		return validate(model, effort)
	}
	return nil
}

// MCPConfig is one adapter's complete native projection wiring for the shared mcp.json.
type MCPConfig struct {
	Mounts      []MCPMount
	CommandArgs []string
	// Env is trusted runtime configuration, applied after operator env files/extra arguments.
	Env []string
	// RequiredEnv names authentication references that must be nonempty in the captured box env.
	RequiredEnv []string
	// NestedCommandEnv is trusted KEY=value wiring consumed by this adapter's in-box wrapper
	// commands. box.Run appends it after the user env file so the projected path stays authoritative.
	NestedCommandEnv []string
}

// MCPMount is one generated config file an agent needs to see the shared mcp.json: its content and
// where it mounts inside the box.
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

// ReviewOutputContract keeps host-owned evidence and receipt validation at the caller.
type ReviewOutputContract struct {
	Normalize        func(string) string
	ValidOutput      func(string) bool
	ValidReceiptLine func(string) bool
}

type PlainOutputProbe interface {
	io.Writer
	Limited(exitCode int) bool
}

var registry = map[string]Agent{}

// register adds an agent to the registry; called from each adapter's init().
func register(a Agent) {
	registry[a.Name()] = a
	config.RegisterAdapterConfig(a.Name())
}

// Get returns the agent registered under name.
func Get(name string) (Agent, bool) { a, ok := registry[name]; return a, ok }

// Default is the stable initial provider, independent of registry sorting.
func Default() string { return (claudeAgent{}).Name() }

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
