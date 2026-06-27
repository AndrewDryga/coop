package box

import (
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// AuthedAgents returns the agents that look authenticated: a credential file in their
// config dir, or their API key set in the env file (each adapter names its own marker).
// It's a presence heuristic — not a validity check, which would mean running each CLI —
// but enough to decide whether a peer is worth consulting.
func AuthedAgents(cfg *config.Config) []string {
	keys := envFileKeys(cfg.EnvFile())
	var authed []string
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		file, envKey := ag.AuthMarker()
		if keys[envKey] || fileExists(filepath.Join(cfg.AgentDir(name), file)) {
			authed = append(authed, name)
		}
	}
	return authed
}

// credentialScope is the set of agents whose credential home (~/.<name>) and env-file API
// key a run may mount. A plain agent run (spec.Agent set) gets only that agent; a fusion
// governor or consult lead also gets its authenticated peers, since it is explicitly told
// to invoke them read-only; a raw or maintenance run (no agent) gets none. Homes off → none.
func credentialScope(cfg *config.Config, spec RunSpec) []string {
	if !spec.Homes {
		return nil
	}
	primary := spec.Agent
	consultsPeers := spec.FusionGovernor != "" || spec.ConsultLead != ""
	switch {
	case spec.FusionGovernor != "":
		primary = spec.FusionGovernor
	case spec.ConsultLead != "":
		primary = spec.ConsultLead
	}
	if primary == "" {
		return nil // raw/maintenance run — no agent session, no credentials
	}
	scope := []string{primary}
	if consultsPeers {
		scope = append(scope, authedPeers(cfg, primary)...)
	}
	return scope
}

// envKeysOutsideScope is the set of agent token env-file keys to strip for a run scoped to
// the given agents: every credential key (the API key plus alternates like
// ANTHROPIC_AUTH_TOKEN) of every agent except those in scope. Non-agent runtime vars in the
// env file are never in this set, so they always pass through.
func envKeysOutsideScope(scope []string) map[string]bool {
	in := map[string]bool{}
	for _, a := range scope {
		in[a] = true
	}
	drop := map[string]bool{}
	for _, name := range agents.Names() {
		if in[name] {
			continue
		}
		if ag, ok := agents.Get(name); ok {
			for _, envKey := range ag.CredentialEnvKeys() {
				if envKey != "" {
					drop[envKey] = true
				}
			}
		}
	}
	return drop
}

// writeFilteredEnvFile copies the env file to a temp file, dropping the given API-key lines
// (KEY=...) so peer credentials don't enter a scoped box; comments, blanks, and every other
// key are preserved verbatim. Returns the temp path the caller must clean up.
func writeFilteredEnvFile(path string, drop map[string]bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			if k, _, ok := strings.Cut(t, "="); ok && drop[strings.TrimSpace(k)] {
				continue // strip this peer's API key
			}
		}
		kept = append(kept, line)
	}
	return writeTempFile(strings.Join(kept, "\n"))
}

// authedPeers returns the authenticated agents other than lead, preserving order.
func authedPeers(cfg *config.Config, lead string) []string {
	peers := make([]string, 0, len(agents.Names()))
	for _, a := range AuthedAgents(cfg) {
		if a != lead {
			peers = append(peers, a)
		}
	}
	return peers
}

// envFileKeys parses a KEY=VALUE env file into the set of keys with a non-empty
// value (comments and blanks ignored). A missing file yields an empty set.
func envFileKeys(path string) map[string]bool {
	keys := map[string]bool{}
	data, err := os.ReadFile(path)
	if err != nil {
		return keys
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(v) != "" {
			keys[strings.TrimSpace(k)] = true
		}
	}
	return keys
}
