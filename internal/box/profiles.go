package box

import (
	"encoding/json"
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
// declare every accepted token key; AuthMarker owns the file shape. Callers may share a parsed env
// key set when scanning providers, but they never reconstruct provider-specific credential rules.
// A provider-wide env token represents one effective default account, never every named profile.
func profileCredentialPresent(ag agents.Agent, profileDir string, envKeys map[string]bool, allowEnv bool) bool {
	if allowEnv {
		for _, key := range ag.CredentialEnvKeys() {
			if envKeys[key] {
				return true
			}
		}
	}
	return profileMarkerPresent(ag, profileDir)
}

func profileMarkerPresent(ag agents.Agent, profileDir string) bool {
	file, _ := ag.AuthMarker()
	return fileExists(filepath.Join(profileDir, file))
}

// ProfileTokenExpiry returns when agent's named-profile credential expires, and whether that's
// knowable. Only an OAuth login carries a readable expiry — claude's .credentials.json
// (claudeAiOauth.expiresAt, ms epoch); an API-key login or another agent returns ok=false (nothing
// to check). ProfileAuthed is a presence heuristic and can't tell a live token from an expired one
// that's still on disk; this can, so callers (e.g. `coop credentials`) don't report a dead token as
// "signed in" — the exact trap behind a "signed in but 401" run.
func ProfileTokenExpiry(cfg *config.Config, agent, profile string) (time.Time, bool) {
	if agent != "claude" {
		return time.Time{}, false
	}
	data, err := os.ReadFile(filepath.Join(cfg.AgentProfileDir(agent, profile), ".credentials.json"))
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		ClaudeAiOauth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &c) != nil || c.ClaudeAiOauth.ExpiresAt == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(c.ClaudeAiOauth.ExpiresAt), true
}

// ProfileRenewable reports whether agent's named-profile OAuth login carries a refresh token. An
// access token past its expiresAt is NOT a dead login when one is present — the agent CLI renews
// it on use and writes the fresh token back (verified: a claude profile shown "expired" answered
// live in-box, then its expiresAt moved forward). So callers treat expired-but-renewable as signed
// in, not "token expired". Only claude exposes a readable OAuth credential; anything else is false.
func ProfileRenewable(cfg *config.Config, agent, profile string) bool {
	if agent != "claude" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(cfg.AgentProfileDir(agent, profile), ".credentials.json"))
	if err != nil {
		return false
	}
	var c struct {
		ClaudeAiOauth struct {
			RefreshToken string `json:"refreshToken"`
		} `json:"claudeAiOauth"`
	}
	return json.Unmarshal(data, &c) == nil && c.ClaudeAiOauth.RefreshToken != ""
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
