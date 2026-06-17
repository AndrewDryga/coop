package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A repo's rotation pool is which credential profiles its unattended loop cycles through,
// by agent. It's stored as names only (never secrets) in pools.json under the vault, keyed
// by the repo's absolute path, so nothing about it lands in the repo tree.

type poolRegistry map[string]map[string][]string // repo abs path -> agent -> profiles

func poolsFile(cfg *config.Config) string { return filepath.Join(cfg.ConfigDir, "pools.json") }

// loadPools reads the registry; a missing file is an empty registry, a malformed one is a
// surfaced error (so a corrupt config is fixed, not silently ignored).
func loadPools(cfg *config.Config) (poolRegistry, error) {
	data, err := os.ReadFile(poolsFile(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return poolRegistry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var reg poolRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("read %s: %w", poolsFile(cfg), err)
	}
	if reg == nil {
		reg = poolRegistry{}
	}
	return reg, nil
}

// savePools writes the registry atomically (temp + rename) so a crash can't truncate it.
func savePools(cfg *config.Config, reg poolRegistry) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return err
	}
	tmp := poolsFile(cfg) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, poolsFile(cfg))
}

// repoPool returns the profiles configured for repo+agent (nil if none).
func repoPool(cfg *config.Config, repo, agent string) ([]string, error) {
	reg, err := loadPools(cfg)
	if err != nil {
		return nil, err
	}
	return reg[repo][agent], nil
}

// setRepoPool replaces the profile list for repo+agent; an empty list clears the entry.
func setRepoPool(cfg *config.Config, repo, agent string, profiles []string) error {
	reg, err := loadPools(cfg)
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		delete(reg[repo], agent)
		if len(reg[repo]) == 0 {
			delete(reg, repo)
		}
	} else {
		if reg[repo] == nil {
			reg[repo] = map[string][]string{}
		}
		reg[repo][agent] = profiles
	}
	return savePools(cfg, reg)
}

// cmdPool shows or edits the current repo's rotation pool.
func (a *app) cmdPool(args []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if len(args) == 0 {
		return a.showPool(repo)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "add", "rm":
		if len(rest) < 2 {
			return 2, fmt.Errorf("usage: coop pool %s <agent> <profile...>", verb)
		}
		agent, profiles := rest[0], rest[1:]
		if _, ok := agents.Get(agent); !ok {
			return 2, fmt.Errorf("unknown agent %q — use %s", agent, strings.Join(agents.Names(), ", "))
		}
		cur, err := repoPool(a.cfg, repo, agent)
		if err != nil {
			return -1, err
		}
		var next []string
		if verb == "add" {
			next = addProfiles(cur, profiles)
			for _, p := range profiles { // a pool member you haven't signed into yet won't rotate
				if !box.ProfileAuthed(a.cfg, agent, p) {
					ui.Info("note: %s profile %q isn't signed in — run: coop login %s --profile %s", agent, p, agent, p)
				}
			}
		} else {
			next = removeProfiles(cur, profiles)
		}
		if err := setRepoPool(a.cfg, repo, agent, next); err != nil {
			return -1, err
		}
		return a.showPool(repo)
	case "clear":
		if len(rest) != 1 {
			return 2, errors.New("usage: coop pool clear <agent>")
		}
		if _, ok := agents.Get(rest[0]); !ok {
			return 2, fmt.Errorf("unknown agent %q — use %s", rest[0], strings.Join(agents.Names(), ", "))
		}
		if err := setRepoPool(a.cfg, repo, rest[0], nil); err != nil {
			return -1, err
		}
		return a.showPool(repo)
	default:
		return 2, fmt.Errorf("unknown pool command %q — use: add, rm, clear", verb)
	}
}

func (a *app) showPool(repo string) (int, error) {
	reg, err := loadPools(a.cfg)
	if err != nil {
		return -1, err
	}
	fmt.Println(ui.Bold(filepath.Base(repo) + " — loop rotation pool"))
	byAgent := reg[repo]
	if len(byAgent) == 0 {
		fmt.Println("  none set — the loop rotates across ALL signed-in profiles")
		fmt.Println("  narrow it with: coop pool add <agent> <profile...>")
		return 0, nil
	}
	for _, agent := range agents.Names() {
		if pool := byAgent[agent]; len(pool) > 0 {
			fmt.Printf("  %-8s %s\n", agent, strings.Join(pool, ", "))
		}
	}
	return 0, nil
}

// addProfiles appends each new profile to cur, skipping duplicates (order preserved).
func addProfiles(cur, add []string) []string {
	out := append([]string{}, cur...)
	for _, p := range add {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// removeProfiles returns cur without any profile named in rm.
func removeProfiles(cur, rm []string) []string {
	var out []string
	for _, p := range cur {
		if !slices.Contains(rm, p) {
			out = append(out, p)
		}
	}
	return out
}
