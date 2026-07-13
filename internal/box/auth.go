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

// runPrimary is the lead agent whose box this is: the fusion governor, else the consult lead,
// else the launched agent. "" for a raw/maintenance run (no agent session).
func runPrimary(spec RunSpec) string {
	switch {
	case spec.FusionGovernor != "":
		return spec.FusionGovernor
	case spec.ConsultLead != "":
		return spec.ConsultLead
	default:
		return spec.Agent
	}
}

// credentialScope is the set of agents whose credential home (~/.<name>) and env-file API key a
// run may mount. A plain agent run (spec.Agent set) gets only that agent; a fusion governor or
// consult lead ALSO gets the EXPLICIT peers it was told to invoke (spec.Peers) — never a blanket
// "every authed agent" widening — plus a preset's own role agents; a raw or maintenance run (no
// agent) gets none. Homes off → none. Narrowing to the named peers is the security dividend: an
// agent the run didn't name never has its credentials mounted.
func credentialScope(cfg *config.Config, spec RunSpec) []string {
	if !spec.Homes {
		return nil
	}
	primary := runPrimary(spec)
	if primary == "" {
		return nil // raw/maintenance run — no agent session, no credentials
	}
	scope := []string{primary}
	add := func(agent string, gate bool) {
		if agent != "" && agent != primary && !slices.Contains(scope, agent) && gate {
			scope = append(scope, agent)
		}
	}
	// The EXPLICIT peers named by --peer mount as peers (they were validated authed at
	// the CLI; scope them unconditionally — the run asked for them by name).
	for _, p := range spec.Peers {
		add(p.Provider, true)
	}
	// A preset's consult/delegate roles run their own agent CLIs from inside the lead's box, so
	// their (authed) agents join the scope. A native role under a Claude lead runs in-session and
	// adds nothing — but under a non-Claude lead it degrades to a consult on its own agent, which
	// then also needs mounting.
	if spec.Preset != nil {
		for _, agent := range spec.Preset.RoleAgents() {
			add(agent, ProfileAuthed(cfg, agent, cfg.ActiveProfile(agent)))
		}
		for _, r := range spec.Preset.DegradedNativeRoles(primary) {
			add(r.Agent, ProfileAuthed(cfg, r.Agent, cfg.ActiveProfile(r.Agent)))
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

// peerProviders returns the provider names of a run's explicit peers, order-preserving.
func peerProviders(peers []agents.Target) []string {
	out := make([]string, 0, len(peers))
	for _, p := range peers {
		out = append(out, p.Provider)
	}
	return out
}

// excluding returns names with every element equal to drop removed, order-preserving.
func excluding(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
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
