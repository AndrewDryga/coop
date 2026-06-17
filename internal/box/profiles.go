package box

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/config"
)

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
