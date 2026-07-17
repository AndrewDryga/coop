package box

import (
	"os"
	"path/filepath"
	"slices"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// EffectiveProfiles returns stored profile directories plus one synthetic env-backed default when
// needed. Consumers that mutate credential directories must continue to use Config.Profiles.
func EffectiveProfiles(cfg *config.Config, agent string) []string {
	profiles := cfg.Profiles(agent)
	def := cfg.DefaultProfileOf(agent)
	if !slices.Contains(profiles, def) && ProfileAuthed(cfg, agent, def) {
		profiles = append(profiles, def)
	}
	return profiles
}

// ProfileAuthed reports whether the named agent profile has its credential marker file, or is the
// one default profile represented by a provider-wide env token. Like AuthedAgents, it's a presence
// heuristic, not a validity check.
func ProfileAuthed(cfg *config.Config, agent, profile string) bool {
	ag, ok := agents.Get(agent)
	if !ok {
		return false
	}
	return profileCredentialPresent(
		ag,
		cfg.AgentProfileDir(agent, profile),
		envFileKeys(cfg.EnvFile()),
		profile == cfg.DefaultProfileOf(agent),
	)
}

// ProfileMarkerPresent reports whether this exact profile has the adapter's login marker. It lets
// callers distinguish a file-backed account from an env-backed default even after Box has created
// the profile directory for mounts and session state.
func ProfileMarkerPresent(cfg *config.Config, agent, profile string) bool {
	ag, ok := agents.Get(agent)
	if !ok {
		return false
	}
	return profileMarkerPresent(ag, cfg.AgentProfileDir(agent, profile))
}

// profileCredentialPresent is the canonical presence heuristic for one adapter profile. Adapters
// declare the active token keys for this account; AuthMarker owns the file shape only when no env
// family is selected. Callers may share a parsed env key set when scanning providers, but they never
// reconstruct provider-specific precedence. A provider-wide env token represents one effective
// default account, never every named profile.
func profileCredentialPresent(ag agents.Agent, profileDir string, envKeys map[string]bool, allowEnv bool) bool {
	markerPresent := profileMarkerPresent(ag, profileDir)
	activeEnvKeys := ag.ActiveCredentialEnvKeys(profileDir, markerPresent)
	if allowEnv {
		for _, key := range activeEnvKeys {
			if envKeys[key] {
				return true
			}
		}
	}
	return markerPresent && len(activeEnvKeys) == 0
}

func profileMarkerPresent(ag agents.Agent, profileDir string) bool {
	file, _ := ag.AuthMarker()
	return fileExists(filepath.Join(profileDir, file))
}

// ProfileTokenMtime returns when agent's named-profile token material last changed on disk — the
// mtime of its AuthMarker file (claude's .credentials.json, codex/grok's auth.json). ANY rewrite
// bumps it: a fresh login OR an OAuth refresh, both of which mint new material and retire the old
// copy — so this answers "how stale is the token a leak could still use", the signal behind
// rotating a credential to contain a blast radius. It stats ONLY the marker file, not the whole
// profile dir, so unrelated session-transcript writes don't masquerade as a rotation. ok=false
// when the login is an env-key one (no file) or the marker is missing/unreadable — the caller
// renders that as a graceful "—", never an error.
func ProfileTokenMtime(cfg *config.Config, agent, profile string) (time.Time, bool) {
	ag, ok := agents.Get(agent)
	if !ok {
		return time.Time{}, false
	}
	file, _ := ag.AuthMarker()
	fi, err := os.Stat(filepath.Join(cfg.AgentProfileDir(agent, profile), file))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// EnsureProfilesDir creates agent's profiles/ dir (0700) if it's missing — run before a
// profile other than the default is created, so config.AgentProfileDir resolves "default"
// (and every named profile) under it. Idempotent: a no-op once profiles/ exists.
func EnsureProfilesDir(cfg *config.Config, agent string) error {
	profiles := filepath.Join(cfg.ConfigDir, agent, "profiles")
	if dirExists(profiles) {
		return nil
	}
	return os.MkdirAll(profiles, 0o700)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
