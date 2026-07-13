package box

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// ProfileAuthed reports whether agent's named profile looks signed in: its credential
// marker file is present in that profile's dir, or the agent's API key is set in the env
// file (a key authenticates every profile). Like AuthedAgents, it's a presence heuristic,
// not a validity check.
func ProfileAuthed(cfg *config.Config, agent, profile string) bool {
	ag, ok := agents.Get(agent)
	if !ok {
		return false
	}
	file, envKey := ag.AuthMarker()
	if envFileKeys(cfg.EnvFile())[envKey] {
		return true
	}
	return fileExists(filepath.Join(cfg.AgentProfileDir(agent, profile), file))
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
