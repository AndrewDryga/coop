package box

import (
	"os"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// AuthedAgents returns agents whose active account looks authenticated. The active account is the
// configured default unless a concrete run selected another stored profile; a provider-wide env
// token counts only in the default slot. This is a presence heuristic, not a live validity check.
func AuthedAgents(cfg *config.Config) []string {
	keys := envFileKeys(cfg.EnvFile())
	var authed []string
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		active := cfg.ActiveProfile(name)
		if profileCredentialPresent(ag, cfg.AgentProfileDir(name, active), keys, active == cfg.DefaultProfileOf(name)) {
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
	// their (authed) agents join the scope. A native role under a capable lead runs in-session and
	// adds nothing; under any other lead it degrades to a consult whose agent needs mounting.
	if spec.Preset != nil {
		for _, agent := range spec.Preset.RunnableRoleAgents(primary) {
			add(agent, ProfileAuthed(cfg, agent, cfg.ActiveProfile(agent)))
		}
	}
	return scope
}

// envKeysOutsideScope is the set of adapter token keys to strip for this concrete run. A provider
// keeps its env token only when it is in scope and running the default account that token represents;
// a named file-backed account must not be shadowed by the provider-wide token. Non-agent runtime
// variables are never in this set, so they always pass through.
func envKeysOutsideScope(cfg *config.Config, scope []string) map[string]bool {
	in := map[string]bool{}
	for _, a := range scope {
		in[a] = true
	}
	drop := map[string]bool{}
	for _, name := range agents.Names() {
		ag, ok := agents.Get(name)
		if !ok {
			continue
		}
		active := cfg.ActiveProfile(name)
		if in[name] && active == cfg.DefaultProfileOf(name) &&
			!profileMarkerPresent(ag, cfg.AgentProfileDir(name, active)) {
			continue
		}
		for _, envKey := range ag.CredentialEnvKeys() {
			if envKey != "" {
				drop[envKey] = true
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

// envFileKeys resolves an env file into the keys whose effective values are non-empty. A bare KEY
// imports the ambient value, and later duplicate assignments win, matching the runtime env-file
// contract. Comments, blanks, and a missing file yield no keys.
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
		if k, v, ok := strings.Cut(line, "="); ok {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if strings.TrimSpace(v) != "" {
				keys[k] = true
			} else {
				delete(keys, k)
			}
			continue
		}
		if value, ok := os.LookupEnv(line); ok {
			if value != "" {
				keys[line] = true
			} else {
				delete(keys, line)
			}
		}
	}
	return keys
}
