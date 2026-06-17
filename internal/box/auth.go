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
