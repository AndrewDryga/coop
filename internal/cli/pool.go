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
	"time"

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

// savePools writes the registry atomically (unique temp + rename) so a crash can't truncate it and
// concurrent writers don't clobber a shared temp. Call it inside modifyPoolRegistry's lock.
func savePools(cfg *config.Config, reg poolRegistry) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomic(poolsFile(cfg), append(data, '\n'))
}

// modifyPoolRegistry applies fn to the pool registry as one locked read-modify-write, so concurrent
// edits (parallel `coop pool add`, fleet ops) don't lose updates (last-writer-wins used to drop them).
func modifyPoolRegistry(cfg *config.Config, fn func(poolRegistry)) error {
	return config.WithLock(poolsFile(cfg), func() error {
		reg, err := loadPools(cfg)
		if err != nil {
			return err
		}
		fn(reg)
		return savePools(cfg, reg)
	})
}

// assignPool sets repo+agent's profile list in reg, or clears the entry (pruning an emptied repo
// map) when the list is empty.
func assignPool(reg poolRegistry, repo, agent string, list []string) {
	if len(list) == 0 {
		delete(reg[repo], agent)
		if len(reg[repo]) == 0 {
			delete(reg, repo)
		}
		return
	}
	if reg[repo] == nil {
		reg[repo] = map[string][]string{}
	}
	reg[repo][agent] = list
}

// repoPool returns the profiles configured for repo+agent (nil if none).
func repoPool(cfg *config.Config, repo, agent string) ([]string, error) {
	reg, err := loadPools(cfg)
	if err != nil {
		return nil, err
	}
	return reg[repo][agent], nil
}

// setRepoPool replaces the profile list for repo+agent (locked); an empty list clears the entry.
func setRepoPool(cfg *config.Config, repo, agent string, profiles []string) error {
	return modifyPoolRegistry(cfg, func(reg poolRegistry) {
		assignPool(reg, repo, agent, profiles)
	})
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
			return 2, unknownErr("agent", agent, agents.Names())
		}
		for _, p := range profiles { // a mistyped flag mustn't be stored as a profile named "--x"
			if !validProfileName(p) {
				return 2, fmt.Errorf("invalid profile name %q — a profile is a single name (no '/', '..', or leading '-')", p)
			}
		}
		if verb == "add" {
			for _, p := range profiles { // a pool member you haven't signed into yet won't rotate
				if !box.ProfileAuthed(a.cfg, agent, p) {
					ui.Info("note: %s profile %q isn't signed in — run: coop login %s --profile %s", agent, p, agent, p)
				}
			}
		}
		// One locked read-modify-write: read the current list and apply add/rm inside the lock so a
		// concurrent edit can't be lost (read-then-separate-write used to drop updates).
		err := modifyPoolRegistry(a.cfg, func(reg poolRegistry) {
			cur := reg[repo][agent]
			if verb == "add" {
				assignPool(reg, repo, agent, addProfiles(cur, profiles))
			} else {
				assignPool(reg, repo, agent, removeProfiles(cur, profiles))
			}
		})
		if err != nil {
			return -1, err
		}
		return a.showPool(repo)
	case "clear":
		if len(rest) != 1 {
			return 2, errors.New("usage: coop pool clear <agent>")
		}
		if _, ok := agents.Get(rest[0]); !ok {
			return 2, unknownErr("agent", rest[0], agents.Names())
		}
		if err := setRepoPool(a.cfg, repo, rest[0], nil); err != nil {
			return -1, err
		}
		return a.showPool(repo)
	default:
		return 2, unknownErr("pool command", verb, []string{"add", "rm", "clear"})
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

// profilePool is the loop's view of a repo+agent's rotation pool: an ordered set of
// signed-in profiles and which ones are rate-limited, with a sticky cursor that stays on
// one profile until it's limited, then advances. It is pure (the clock is injected) so the
// rotation policy is unit-tested without sleeping.
type profilePool struct {
	profiles []string             // ordered, signed-in members; len >= 1
	limited  map[string]time.Time // profile -> when its limit resets
	idx      int                  // sticky cursor: advances only on a limit
}

func newProfilePool(profiles []string) *profilePool {
	return &profilePool{profiles: profiles, limited: map[string]time.Time{}}
}

// buildPool resolves the rotation pool for repo+agent: the repo's configured pool if set,
// otherwise every profile (so two logins rotate with zero config). It keeps only signed-in
// profiles, preserving order, and errors if none are — the loop can't run unauthenticated.
func buildPool(cfg *config.Config, repo, agent string) (*profilePool, error) {
	if len(cfg.LoopCmd) > 0 {
		// A custom COOP_LOOP_CMD may not be an agent at all — keep today's behavior: the
		// default profile, no rotation, no auth gate.
		return newProfilePool([]string{config.DefaultProfile}), nil
	}
	names, err := repoPool(cfg, repo, agent)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		names = cfg.Profiles(agent)
	}
	return authedPool(cfg, agent, names)
}

// authedPool wraps names in a rotation pool, keeping only the signed-in ones (order preserved) and
// erroring when none are — the loop can't run unauthenticated. Shared by buildPool (repo pool / all
// signed-in) and a fork's explicit per-fork profile list (`profile=` in .agent/fleet, or --profile).
func authedPool(cfg *config.Config, agent string, names []string) (*profilePool, error) {
	var authed []string
	for _, p := range names {
		if box.ProfileAuthed(cfg, agent, p) {
			authed = append(authed, p)
		}
	}
	if len(authed) == 0 {
		return nil, fmt.Errorf("%s has no signed-in profile — run: coop login %s [--profile <name>]", agent, agent)
	}
	return newProfilePool(authed), nil
}

// parseProfileList splits a comma-separated profile list ("work,personal") into names, trimming
// spaces, dropping empties, and de-duplicating (first-seen order) — so "a,a,b" doesn't masquerade
// as a rotating multi-account pool that, on a rate limit, finds no free profile and waits anyway.
func parseProfileList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// rotateOnLimit handles a rate limit when the pool has more than one profile: it advances
// the pool, points cfg at the new active profile (so the next iteration mounts and runs it),
// and either switches immediately (resetting the wait counter, since a free rotation is
// progress) or, when every profile is limited, sleeps until the soonest reset.
func (a *app) rotateOnLimit(agent string, pool *profilePool, resetAt time.Time, waits *int) {
	prev := pool.active()
	sleep, until := pool.onLimit(resetAt, *waits, time.Now())
	a.cfg.SetActiveProfile(agent, pool.active())
	if sleep > 0 {
		ui.Info("all %d %s profiles are rate limited — waiting for the soonest reset", len(pool.profiles), agent)
		sleepForLimit(sleep, until)
		return
	}
	ui.Info("%s profile %q rate limited — switching to %q", agent, prev, pool.active())
	*waits = 0 // only consecutive all-limited waits count toward the stop cap
}

func (p *profilePool) active() string { return p.profiles[p.idx] }

// rotates reports whether there's more than one profile to switch between.
func (p *profilePool) rotates() bool { return len(p.profiles) > 1 }

// onLimit records that the active profile is rate-limited until resetAt (a zero resetAt
// means "unknown", so it backs off by attempt), then advances to the next usable profile.
// It returns how long to sleep before the next iteration — 0 when another profile is free
// now (switch and retry immediately) — and, when sleeping, the time it's waiting until.
func (p *profilePool) onLimit(resetAt time.Time, attempt int, now time.Time) (sleep time.Duration, until time.Time) {
	if resetAt.IsZero() {
		resetAt = now.Add(limitWait(limitHint{limited: true}, attempt, now))
	}
	p.limited[p.profiles[p.idx]] = resetAt
	// Sticky rotation: scan the profiles after the current one (wrapping) for the first
	// that isn't limited as of now — a profile becomes usable again once its reset passes.
	n := len(p.profiles)
	for i := 1; i <= n; i++ {
		cand := (p.idx + i) % n
		if t, ok := p.limited[p.profiles[cand]]; !ok || !t.After(now) {
			p.idx = cand
			return 0, time.Time{}
		}
	}
	// Every profile is limited: move to the soonest-resetting one and wait for it.
	earliest := 0
	for i := range p.profiles {
		if p.limited[p.profiles[i]].Before(p.limited[p.profiles[earliest]]) {
			earliest = i
		}
	}
	p.idx = earliest
	until = p.limited[p.profiles[earliest]]
	return limitWait(limitHint{limited: true, resetAt: until}, attempt, now), until
}
