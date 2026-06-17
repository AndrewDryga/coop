package box

import (
	"fmt"
	"os"
	"path/filepath"

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

// EnsureProfilesDir prepares agent's credential vault for the named-profile layout, run
// before a profile other than the default is created. The first time it runs (no
// profiles/ dir yet) it creates profiles/ (0700) and, if a legacy flat login already
// exists at <agent>/, moves that login into profiles/default/ — so the existing default
// credential isn't orphaned when the profiles/ dir appears (config.AgentProfileDir then
// resolves "default" there). Idempotent: a no-op once profiles/ exists.
func EnsureProfilesDir(cfg *config.Config, agent string) error {
	base := filepath.Join(cfg.ConfigDir, agent)
	profiles := filepath.Join(base, "profiles")
	if dirExists(profiles) {
		return nil
	}
	// Snapshot the legacy entries before creating profiles/, so the new dir isn't itself
	// a migration candidate. A missing base dir (fresh vault) yields nothing to move.
	entries, _ := os.ReadDir(base)
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil // fresh vault — nothing to migrate
	}
	def := filepath.Join(profiles, "default")
	if err := os.MkdirAll(def, 0o700); err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "profiles" {
			continue // defensive: never move the dir we just created
		}
		if err := os.Rename(filepath.Join(base, e.Name()), filepath.Join(def, e.Name())); err != nil {
			return fmt.Errorf("migrate %s default profile: %w", agent, err)
		}
	}
	return nil
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
