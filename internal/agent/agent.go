// Package agent is the registry of coding agents coop can drive. Each agent is one
// file implementing Agent and self-registering; adding or removing an agent is a
// single-file change, and the compiler enforces that every agent answers every
// question — no switch case to forget.
package agent

import (
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
)

// Agent is everything coop needs to drive one coding agent. To add an agent, write a
// new file implementing this interface and self-register it from an init().
type Agent interface {
	Name() string
	// Interactive is the autonomous default command — what `coop <agent>` runs.
	Interactive(cfg *config.Config) []string
	// Headless is the one-shot, non-interactive form carrying a prompt (the loop).
	Headless(cfg *config.Config, prompt string) []string
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
	// is the coop-owned session id for this (fork, agent): agents that honor a preset
	// id (claude, gemini) resume exactly that id — immune to loop/consult sessions that
	// share the cwd — while codex, which mints its own id, ignores it and scans for its
	// most-recent interactive (non-exec) session.
	Resume(cfg *config.Config, ws, id string) ([]string, bool)
	// StartSession is the fresh interactive command under the coop-chosen session id:
	// claude/gemini stamp it via --session-id so a later Resume can pin exactly it;
	// codex ignores id and mints its own. An empty id falls back to Interactive.
	StartSession(cfg *config.Config, id string) []string
	// PresetSessionID reports whether the agent honors a caller-chosen session id. When
	// false (codex), coop allocates none and relies on Resume's scan.
	PresetSessionID() bool
	// Login authenticates the agent (its token persists in its config dir).
	Login(cfg *config.Config) []string
	// ConsultCmd is the read-only, non-interactive command to ask this agent a
	// question as a fusion peer — it returns analysis and never edits files.
	ConsultCmd(question string) []string
	// InstructionFile is the agent's native global instruction filename, e.g.
	// "CLAUDE.md" — where coop writes the shared INSTRUCTIONS.md and fusion directive.
	InstructionFile() string
	// AuthMarker is the credential file (under the agent's config dir) it writes on
	// login, and the env-file key it reads an API key from — either present means it's
	// set up and worth offering as a consult peer.
	AuthMarker() (file, envKey string)
	// CredentialEnvKeys is every env-file key this agent reads a token from — the
	// AuthMarker key plus any alternates it honors (e.g. claude also reads
	// ANTHROPIC_AUTH_TOKEN and CLAUDE_CODE_OAUTH_TOKEN). A scoped run strips all of an
	// out-of-scope agent's keys, so a peer's alternate token can't leak into a box that
	// isn't authorized for it.
	CredentialEnvKeys() []string
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
	// ACPSessionConfig are per-session config options coop force-sets on this agent's
	// ACP adapter after a session is (re)established — what the adapter exposes as a
	// config option that must follow coop's policy (claude's mode=bypassPermissions, so
	// its toolbar reflects yolo). Re-applied on every restart; nil when nothing is forced.
	ACPSessionConfig() map[string]string
	// BoxEnv are env vars this agent's CLI needs inside the box (beyond ModelEnv and
	// credentials), given the box home dir. Exported into every box — a var is inert
	// where its agent isn't running — so a new agent's env needs no box.Run edit.
	BoxEnv(homeInBox string) []string
	// ConsultFresh is the shell body for a fresh read-only consult session in the
	// coop-consult wrapper — run against the wrapper's variables $prompt, $id, $model
	// (uniformly resolved) and $idfile, plus the run/new_id helpers. It analyses and
	// reports; it never edits files.
	ConsultFresh() string
	// ConsultResume is the shell body for resuming a consult by $id (read from $idfile).
	ConsultResume() string
	// DelegateExec is the write-capable shell body for coop-delegate — run against
	// $prompt and $model. The wrapper enforces commit:never and serialization around it.
	DelegateExec() string
	// ShellPrelude is optional helper-function shell the wrappers emit ONCE before the
	// per-agent case (e.g. codex's output filter); "" for agents that need none.
	ShellPrelude() string
	// InstallScript is a non-npm box-image install command (e.g. an install-script
	// download); "" means this agent installs via Packages() on the npm layer.
	InstallScript() string
}

// withModel appends `--model <model>` to cmd — the flag all three CLIs accept, on their
// main command and their exec/resume forms alike. A no-op when no model is chosen, or when
// cmd already names one (a COOP_<AGENT>_CMD baking its own --model/-m stays authoritative;
// appending a second would make clap-based CLIs like codex error on the duplicate).
func withModel(cmd []string, model string) []string {
	if model == "" || hasModelFlag(cmd) {
		return cmd
	}
	return append(cmd, "--model", model)
}

// hasModelFlag reports whether cmd already carries a model flag (--model/-m, split or =-joined).
func hasModelFlag(cmd []string) bool {
	for _, a := range cmd {
		if a == "--model" || a == "-m" || strings.HasPrefix(a, "--model=") || strings.HasPrefix(a, "-m=") {
			return true
		}
	}
	return false
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
