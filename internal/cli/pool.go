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
			return 2, fmt.Errorf("usage: coop loop pool %s <agent> <credential[@model]...>", verb)
		}
		agent, members := rest[0], rest[1:]
		if _, ok := agents.Get(agent); !ok {
			return 2, unknownErr("agent", agent, agents.Names())
		}
		for _, m := range members { // a mistyped flag mustn't be stored as a member named "--x"
			t := parsePoolTarget(m)
			if !validProfileName(t.credential) {
				return 2, fmt.Errorf("invalid credential name %q — a member is credential[@model], the name a single segment (no '/', '..', or leading '-')", m)
			}
			if strings.Contains(m, "@") && t.model == "" {
				return 2, fmt.Errorf("member %q has an empty model — use credential@model, or drop the @", m)
			}
		}
		if verb == "add" {
			for _, m := range members { // a pool member you haven't signed into yet won't rotate
				if t := parsePoolTarget(m); !box.ProfileAuthed(a.cfg, agent, t.credential) {
					ui.Warn("%s credential %q isn't signed in — run: coop login %s --profile %s", agent, t.credential, agent, t.credential)
				}
			}
		}
		// One locked read-modify-write: read the current list and apply add/rm inside the lock so a
		// concurrent edit can't be lost (read-then-separate-write used to drop updates).
		err := modifyPoolRegistry(a.cfg, func(reg poolRegistry) {
			cur := reg[repo][agent]
			if verb == "add" {
				assignPool(reg, repo, agent, addProfiles(cur, members))
			} else {
				assignPool(reg, repo, agent, removeProfiles(cur, members))
			}
		})
		if err != nil {
			return -1, err
		}
		return a.showPool(repo)
	case "clear":
		if len(rest) != 1 {
			return 2, errors.New("usage: coop loop pool clear <agent>")
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
	p := ui.For(os.Stdout) // stdout view — keep pipes clean
	fmt.Println(p.Bold(filepath.Base(repo) + " — loop rotation pool"))
	byAgent := reg[repo]
	if len(byAgent) == 0 {
		fmt.Println("  none set — the loop rotates across ALL signed-in credentials")
		fmt.Println("  narrow it with: coop loop pool add <agent> <credential[@model]...>")
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

// poolTarget is one rotation-pool member: a credential (a stored account) and an
// optional model to run it on — the `credential@model` grammar. Two targets on the SAME
// credential with different models are distinct members, so a rate limit on `work@opus`
// can fall back to `work@sonnet` (same login, cheaper model, no re-auth) before the pool
// rotates to another account.
type poolTarget struct {
	credential string
	model      string // "" = the credential's own defaults resolve (mark/env/CLI default)
}

// parsePoolTarget splits a pool member "credential[@model]" into its parts.
func parsePoolTarget(s string) poolTarget {
	cred, model, _ := strings.Cut(s, "@")
	return poolTarget{credential: strings.TrimSpace(cred), model: strings.TrimSpace(model)}
}

// String renders the member back in its wire form (what pools.json stores and errors show).
func (t poolTarget) String() string {
	if t.model == "" {
		return t.credential
	}
	return t.credential + "@" + t.model
}

// profilePool is the loop's view of a repo+agent's rotation pool: an ordered set of
// signed-in credential targets and which ones are rate-limited, with a sticky cursor that
// stays on one target until it's limited, then advances. The limit map is keyed per
// TARGET, so `work@sonnet` stays available while `work@opus` cools down. It is pure (the
// clock is injected) so the rotation policy is unit-tested without sleeping.
type profilePool struct {
	targets []poolTarget             // ordered, signed-in members; len >= 1
	limited map[poolTarget]time.Time // target -> when its limit resets
	idx     int                      // sticky cursor: advances only on a limit
}

func newProfilePool(members []string) *profilePool {
	p := &profilePool{limited: map[poolTarget]time.Time{}}
	for _, m := range members {
		p.targets = append(p.targets, parsePoolTarget(m))
	}
	return p
}

// buildPool resolves the rotation pool for repo+agent: the repo's configured pool if set,
// otherwise every profile (so two logins rotate with zero config). It keeps only signed-in
// profiles, preserving order, and errors if none are — the loop can't run unauthenticated.
func buildPool(cfg *config.Config, repo, agent string) (*profilePool, error) {
	if len(cfg.LoopCmd) > 0 {
		// A custom COOP_LOOP_CMD may not be an agent at all — no rotation, no auth gate, just
		// the agent's MARKED default profile (not the literal "default", which would re-create
		// a husk profile dir every iteration for a user whose default is a named profile).
		return newProfilePool([]string{cfg.DefaultProfileOf(agent)}), nil
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

// authedPool wraps members ("credential" or "credential@model") in a rotation pool,
// keeping only those whose CREDENTIAL is signed in (order preserved) and erroring when
// none are — the loop can't run unauthenticated. Shared by buildPool (repo pool / all
// signed-in) and a fork's explicit per-fork credential list (fleet credentials:, or
// --credential).
func authedPool(cfg *config.Config, agent string, members []string) (*profilePool, error) {
	var authed []string
	for _, m := range members {
		if box.ProfileAuthed(cfg, agent, parsePoolTarget(m).credential) {
			authed = append(authed, m)
		}
	}
	if len(authed) == 0 {
		return nil, fmt.Errorf("%s has no signed-in credential — run: coop login %s [--profile <name>]", agent, agent)
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

// applyPoolTarget points cfg at the pool's active target: the credential the next
// iteration mounts, and the target's model (empty clears it, so a bare credential falls
// through to the fallback/mark/env tiers). One choke point for loop start + every rotation.
func (a *app) applyPoolTarget(agent string, pool *profilePool) {
	t := pool.active()
	a.cfg.SetActiveProfile(agent, t.credential)
	a.cfg.SetTargetModel(agent, t.model)
}

// rotateOnLimit handles a rate limit when the pool has more than one target: it advances
// the pool, points cfg at the new active target (credential + its model, so the next
// iteration mounts and runs it), and either switches immediately (resetting the wait
// counter, since a free rotation is progress) or, when every target is limited, sleeps
// until the soonest reset.
func (a *app) rotateOnLimit(agent string, pool *profilePool, resetAt time.Time, waits *int, wake <-chan struct{}) {
	prev := pool.active()
	sleep, until := pool.onLimit(resetAt, *waits, time.Now())
	a.applyPoolTarget(agent, pool)
	if sleep > 0 {
		ui.Info("all %d %s pool targets are rate limited — waiting for the soonest reset", len(pool.targets), agent)
		sleepForLimit(sleep, until, wake)
		return
	}
	ui.Info("%s target %q rate limited — switching to %q", agent, prev, pool.active())
	*waits = 0 // only consecutive all-limited waits count toward the stop cap
}

func (p *profilePool) active() poolTarget { return p.targets[p.idx] }

// members renders the pool back in wire form ("credential[@model]"), for messages and tests.
func (p *profilePool) members() []string {
	out := make([]string, len(p.targets))
	for i, t := range p.targets {
		out[i] = t.String()
	}
	return out
}

// rotates reports whether there's more than one target to switch between.
func (p *profilePool) rotates() bool { return len(p.targets) > 1 }

// onLimit records that the active target is rate-limited until resetAt (a zero resetAt
// means "unknown", so it backs off by attempt), then advances to the next usable target.
// The limit is keyed per TARGET, so `work@opus` cooling down leaves `work@sonnet` free —
// the same-credential model fallback. It returns how long to sleep before the next
// iteration — 0 when another target is free now (switch and retry immediately) — and,
// when sleeping, the time it's waiting until.
func (p *profilePool) onLimit(resetAt time.Time, attempt int, now time.Time) (sleep time.Duration, until time.Time) {
	if resetAt.IsZero() {
		resetAt = now.Add(limitWait(limitHint{limited: true}, attempt, now))
	}
	p.limited[p.targets[p.idx]] = resetAt
	// Sticky rotation: scan the targets after the current one (wrapping) for the first
	// that isn't limited as of now — a target becomes usable again once its reset passes.
	n := len(p.targets)
	for i := 1; i <= n; i++ {
		cand := (p.idx + i) % n
		if t, ok := p.limited[p.targets[cand]]; !ok || !t.After(now) {
			p.idx = cand
			return 0, time.Time{}
		}
	}
	// Every target is limited: move to the soonest-resetting one and wait for it.
	earliest := 0
	for i := range p.targets {
		if p.limited[p.targets[i]].Before(p.limited[p.targets[earliest]]) {
			earliest = i
		}
	}
	p.idx = earliest
	until = p.limited[p.targets[earliest]]
	return limitWait(limitHint{limited: true, resetAt: until}, attempt, now), until
}
