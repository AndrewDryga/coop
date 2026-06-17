// Package agent is the registry of coding agents coop can drive. Each agent is one
// file implementing Agent and self-registering; adding or removing an agent is a
// single-file change, and the compiler enforces that every agent answers every
// question — no switch case to forget.
package agent

import (
	"os"
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
	// Resume re-enters the agent's last session, scoped to the fork at ws; the bool
	// reports whether a session was found (else the caller starts fresh).
	Resume(cfg *config.Config, ws string) ([]string, bool)
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
	// MCP returns the config files to mount so the agent sees the shared mcp.json — its
	// native translation (gemini/codex) or none when it reads mcp.json directly (claude).
	MCP(cfg *config.Config) ([]MCPMount, error)
	// EnsureDefaults pre-answers the agent's first-run prompts (theme, folder-trust,
	// sandbox) in its config dir so a fresh box goes straight to work. Best-effort; an
	// agent that needs nothing leaves it empty. workdir is the resolved box cwd.
	EnsureDefaults(cfg *config.Config, workdir string)
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

// hasEntries reports whether dir exists and holds at least one entry — i.e. a session.
func hasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
