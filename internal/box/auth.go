package box

import (
	"os"
	"path/filepath"
	"slices"
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
	// A preset scopes precisely: only its consult/delegate role agents join (below), never
	// the blanket every-authed-peer widening — the preset says exactly who plays.
	consultsPeers := spec.FusionGovernor != "" || (spec.ConsultLead != "" && spec.Preset == nil)
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
	// A preset's consult/delegate roles run their own agent CLIs from inside the lead's box,
	// so their (authed) agents join the scope. A native role under a Claude lead runs in-session
	// and adds nothing — but under a non-Claude lead it degrades to a consult on its own agent,
	// which then also needs mounting.
	if spec.Preset != nil {
		add := func(agent string) {
			if agent != primary && !slices.Contains(scope, agent) && ProfileAuthed(cfg, agent, cfg.ActiveProfile(agent)) {
				scope = append(scope, agent)
			}
		}
		for _, agent := range spec.Preset.RoleAgents() {
			add(agent)
		}
		for _, r := range spec.Preset.DegradedNativeRoles(primary) {
			add(r.Agent)
		}
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
// so peer credentials don't enter a scoped box; comments, blanks, and every other key are
// preserved verbatim. Returns the temp path the caller must clean up. Both `KEY=val` and a
// BARE `KEY` line are stripped: docker --env-file treats a bare key as "import it from the
// current environment", so leaving one in would leak a peer key from coop's own env.
func writeFilteredEnvFile(path string, drop map[string]bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			// strings.Cut returns the whole token as the key when there's no "=", so this
			// catches a bare key too — not only KEY=val.
			if k, _, _ := strings.Cut(t, "="); drop[strings.TrimSpace(k)] {
				continue // strip this peer's API key (KEY=val or a bare imported KEY)
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
