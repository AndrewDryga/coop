package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

// EffectiveProfiles returns stored profile directories plus one synthetic env-backed default when
// needed. Consumers that mutate credential directories must continue to use Config.Profiles. A
// listing error other than a missing directory comes back with the accounts that could be listed.
func EffectiveProfiles(cfg *config.Config, agent string) ([]string, error) {
	profiles, err := cfg.Profiles(agent)
	entries, listErr := os.ReadDir(filepath.Join(cfg.ConfigDir, agent, "credentials"))
	if listErr != nil && !errors.Is(listErr, os.ErrNotExist) {
		err = errors.Join(err, listErr)
	}
	for _, entry := range entries {
		if entry.IsDir() && !slices.Contains(profiles, entry.Name()) {
			profiles = append(profiles, entry.Name())
		}
	}
	def := cfg.DefaultProfileOf(agent)
	if !slices.Contains(profiles, def) && ProfileAuthed(cfg, agent, def) {
		profiles = append(profiles, def)
	}
	if ag, ok := agents.Get(agent); ok {
		profiles = slices.DeleteFunc(profiles, func(account string) bool {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			record, exists, err := readNativeAccount(ctx, cfg, ag, account)
			return err == nil && exists && record.Revoked
		})
	}
	return profiles, err
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

// ProfileCredentialReady reports whether an account can run: a canonical record that is ready, or
// the default account's env-file credential. A login only builds before v11 saved is not ready.
func ProfileCredentialReady(cfg *config.Config, agent, profile string, now time.Time) bool {
	ag, ok := agents.Get(agent)
	if !ok {
		return false
	}
	if ready, exists := nativeAccountReady(cfg, ag, profile, now); exists {
		return ready
	}
	profileDir := cfg.AgentProfileDir(agent, profile)
	activeEnvKeys := ag.ActiveCredentialEnvKeys(profileDir, profileMarkerPresent(ag, profileDir))
	return profile == cfg.DefaultProfileOf(agent) && anyCredentialEnvPresent(activeEnvKeys, envFileKeys(cfg.EnvFile()))
}

// ProfileSignIn is what an account needs before it can run, judged from local state alone, so it
// is the one answer coop credentials, the model menu, coop usage and a launch all give. A saved
// sign-in that cannot run needs a new one: a native record that is damaged, can no longer renew,
// or whose grant the provider refused at its last renewal, and a login only builds before v11 saved. An account with nothing saved needs its first. One whose lock another process
// holds, as a renewal does while its provider answers, reads as signed in (nativeAccountReady).
func ProfileSignIn(cfg *config.Config, agent, profile string, now time.Time) agents.SignIn {
	if ProfileCredentialReady(cfg, agent, profile, now) {
		return agents.SignedIn
	}
	ag, ok := agents.Get(agent)
	if !ok {
		return agents.NotSignedIn
	}
	if _, native := nativeAccountReady(cfg, ag, profile, now); native || ProfileAuthed(cfg, agent, profile) {
		return agents.SignInAgain
	}
	// A login only builds before v11 saved: coop login replaces it.
	if present, _ := legacyLoginPresent(cfg, ag, profile); present {
		return agents.SignInAgain
	}
	return agents.NotSignedIn
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
	if ready, exists := nativeAccountReady(cfg, ag, profile, time.Now()); exists {
		return ready
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
	if ready, exists := nativeAccountReady(cfg, ag, profile, time.Now()); exists {
		return ready
	}
	return false
}

// profileCredentialPresent is the presence heuristic for one adapter profile: a canonical record, or
// for the default account a provider-wide env token that the adapter declares active. Callers may
// share a parsed env key set when scanning providers, but they never reconstruct provider-specific
// precedence. A provider-wide env token represents one effective default account, never every
// named profile.
func profileCredentialPresent(cfg *config.Config, ag agents.Agent, agent, profile string, envKeys map[string]bool, allowEnv bool) bool {
	if ready, exists := nativeAccountReady(cfg, ag, profile, time.Now()); exists {
		return ready
	}
	if !allowEnv {
		return false
	}
	profileDir := cfg.AgentProfileDir(agent, profile)
	for _, key := range ag.ActiveCredentialEnvKeys(profileDir, profileMarkerPresent(ag, profileDir)) {
		if envKeys[key] {
			return true
		}
	}
	return false
}

func profileMarkerPresent(ag agents.Agent, profileDir string) bool {
	file, _ := ag.AuthMarker()
	return fileExists(filepath.Join(profileDir, file))
}

// ProfileTokenMtime returns when agent's named-account token material last changed on disk — the
// mtime of its canonical authority.json. ANY rewrite bumps it: a
// fresh login OR an OAuth refresh, both of which mint new material and retire the old copy — so this
// answers "how stale is the token a leak could still use", the signal behind rotating a credential
// to contain a blast radius. It stats ONLY that file, so provider state beside it doesn't
// masquerade as a rotation. ok=false when the login is an env-key one (no file) or the file is
// missing/unreadable — the caller renders that as a graceful "—", never an error.
func ProfileTokenMtime(cfg *config.Config, agent, profile string) (time.Time, bool) {
	ag, ok := agents.Get(agent)
	if !ok {
		return time.Time{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, exists, err := readNativeAccount(ctx, cfg, ag, profile); exists {
		// A record another process holds, as a renewal does while its provider answers, is still
		// dated by its file, which publication replaces whole: a listing need not wait it out.
		if err != nil && !accountBusy(err) {
			return time.Time{}, false
		}
		info, err := os.Lstat(filepath.Join(cfg.NativeAuthorityConfig().ConfigDir, agent, "credentials", profile, "authority.json"))
		if err != nil || !info.Mode().IsRegular() {
			return time.Time{}, false
		}
		return info.ModTime(), true
	}
	return time.Time{}, false
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
