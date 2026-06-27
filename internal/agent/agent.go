// Package agent is the registry of coding agents coop can drive. Each agent is one
// file implementing Agent and self-registering; adding or removing an agent is a
// single-file change, and the compiler enforces that every agent answers every
// question — no switch case to forget.
package agent

import (
	"sort"

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
	// ACP is the agent's ACP adapter command over stdio (for editors like Zed).
	ACP() []string
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
}

// Default is the agent used when a command takes one but none is given.
func Default() string { return "claude" }

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
