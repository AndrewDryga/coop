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
		cfg,
		ag,
		agent,
		profile,
		envFileKeys(cfg.EnvFile()),
		profile == cfg.DefaultProfileOf(agent),
	)
}

// ProfileCredentialReady refines credential presence only where a caller needs a runnable account.
// Inspectable native markers that cannot recover without another login are excluded; opaque stores
// and env-backed credentials preserve the presence-based behavior because the adapter cannot prove
// them invalid locally.
func ProfileCredentialReady(cfg *config.Config, agent, profile string, now time.Time) bool {
	ag, ok := agents.Get(agent)
	if !ok {
		return false
	}
	profileDir := cfg.AgentProfileDir(agent, profile)
	markerPresent := profileMarkerPresent(ag, profileDir)
	activeEnvKeys := ag.ActiveCredentialEnvKeys(profileDir, markerPresent)
	if profile == cfg.DefaultProfileOf(agent) && anyCredentialEnvPresent(activeEnvKeys, envFileKeys(cfg.EnvFile())) {
		return true
	}
	if hostCredentialSelected(ag, profileDir, markerPresent) {
		_, _, hostFound, hostErr := LoadHostCredential(cfg, ag, profile)
		if hostErr != nil {
			return false
		}
		if hostFound {
			return true
		}
	}
	if !ProfileAuthed(cfg, agent, profile) {
		return false
	}
	if !ProfileMarkerPresent(cfg, agent, profile) {
		return true
	}
	return ag.StoredCredentialStatus(cfg.AgentProfileDir(agent, profile), now) != agents.StoredCredentialReauthRequired
}

func anyCredentialEnvPresent(keys []string, present map[string]bool) bool {
	for _, key := range keys {
		if present[key] {
			return true
		}
	}
	return false
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

// ProfileHostCredentialPresent reports whether this account has a valid Coop-owned credential
// outside the provider home. It distinguishes that stored key from a provider-wide env-only
// default without exposing its value.
func ProfileHostCredentialPresent(cfg *config.Config, agent, profile string) bool {
	ag, ok := agents.Get(agent)
	if !ok {
		return false
	}
	profileDir := cfg.AgentProfileDir(agent, profile)
	if !hostCredentialSelected(ag, profileDir, profileMarkerPresent(ag, profileDir)) {
		return false
	}
	_, _, found, err := LoadHostCredential(cfg, ag, profile)
	return err == nil && found
}

// profileCredentialPresent is the canonical presence heuristic for one adapter profile. Adapters
// declare the active token keys for this account and may additionally declare that their native
// marker can satisfy the selected mode. Callers may share a parsed env key set when scanning
// providers, but they never reconstruct provider-specific precedence. A provider-wide env token
// represents one effective default account, never every named profile.
func profileCredentialPresent(cfg *config.Config, ag agents.Agent, agent, profile string, envKeys map[string]bool, allowEnv bool) bool {
	profileDir := cfg.AgentProfileDir(agent, profile)
	markerPresent := profileMarkerPresent(ag, profileDir)
	activeEnvKeys := ag.ActiveCredentialEnvKeys(profileDir, markerPresent)
	if hostCredentialSelected(ag, profileDir, markerPresent) {
		if _, _, found, err := LoadHostCredential(cfg, ag, profile); err == nil && found {
			return true
		}
	}
	if allowEnv {
		for _, key := range activeEnvKeys {
			if envKeys[key] {
				return true
			}
		}
	}
	return markerPresent && agents.MarkerProvidesActiveCredential(ag, profileDir, activeEnvKeys)
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
	profileDir := cfg.AgentProfileDir(agent, profile)
	if hostCredentialSelected(ag, profileDir, profileMarkerPresent(ag, profileDir)) {
		if info, ok := hostCredentialMtime(cfg, ag, profile); ok {
			return info.ModTime(), true
		}
	}
	file, _ := ag.AuthMarker()
	fi, err := os.Stat(filepath.Join(profileDir, file))
	if err != nil {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// EnsureProfilesDir creates or tightens the host-owned credential ancestors before a profile is
// read or mounted. Provider-owned descendants keep their own modes beneath this 0700 boundary.
func EnsureProfilesDir(cfg *config.Config, agent string) error {
	if err := config.EnsurePrivateDir(cfg.ConfigDir); err != nil {
		return err
	}
	if err := config.EnsurePrivateDir(filepath.Join(cfg.ConfigDir, agent)); err != nil {
		return err
	}
	profiles := filepath.Join(cfg.ConfigDir, agent, "profiles")
	return config.EnsurePrivateDir(profiles)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
