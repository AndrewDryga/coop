package box

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
)

// authMarker is, per agent, the credential file it writes under its config dir on
// login and the env-file key it reads an API key from. Either one present means the
// agent is set up — and therefore worth offering as a consult peer.
var authMarker = map[string]struct{ file, envKey string }{
	"claude": {".credentials.json", "ANTHROPIC_API_KEY"},
	"codex":  {"auth.json", "OPENAI_API_KEY"},
	"gemini": {"gemini-credentials.json", "GEMINI_API_KEY"},
}

// AuthedAgents returns the configured agents that look authenticated: a credential
// file in their config dir, or their API key set in the env file. It's a presence
// heuristic — not a validity check, which would mean running each CLI — but enough
// to decide whether a peer is worth consulting.
func AuthedAgents(cfg *config.Config) []string {
	keys := envFileKeys(cfg.EnvFile())
	var authed []string
	for _, a := range cfg.Agents {
		m, ok := authMarker[a]
		if !ok {
			continue
		}
		if keys[m.envKey] || fileExists(filepath.Join(cfg.AgentDir(a), m.file)) {
			authed = append(authed, a)
		}
	}
	return authed
}

// authedPeers returns the authenticated agents other than lead, preserving order.
func authedPeers(cfg *config.Config, lead string) []string {
	peers := make([]string, 0, len(cfg.Agents))
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
