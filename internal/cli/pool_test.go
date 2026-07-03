package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestPoolStoreRoundTrip(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := "/abs/repo"

	// Missing file → empty pool, no error.
	if got, err := repoPool(cfg, repo, "claude"); err != nil || got != nil {
		t.Fatalf("empty pool: got %v err %v", got, err)
	}
	// Set, then read back.
	if err := setRepoPool(cfg, repo, "claude", []string{"work", "personal"}); err != nil {
		t.Fatal(err)
	}
	got, err := repoPool(cfg, repo, "claude")
	if err != nil || !slices.Equal(got, []string{"work", "personal"}) {
		t.Fatalf("repoPool = %v err %v", got, err)
	}
	// A different agent under the same repo is independent.
	if got, _ := repoPool(cfg, repo, "codex"); got != nil {
		t.Errorf("codex pool should be empty, got %v", got)
	}
	// Clear removes the entry.
	if err := setRepoPool(cfg, repo, "claude", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := repoPool(cfg, repo, "claude"); got != nil {
		t.Errorf("cleared pool not empty: %v", got)
	}
}

func TestPoolHelpers(t *testing.T) {
	if got := addProfiles([]string{"a"}, []string{"a", "b", "b"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("addProfiles dedupe = %v", got)
	}
	if got := removeProfiles([]string{"a", "b", "c"}, []string{"b"}); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("removeProfiles = %v", got)
	}
	// parseProfileList trims, drops empties, and de-dupes (first-seen) so "a,a,b" / "work, work"
	// isn't a fake rotating pool that waits the full reset on a limit instead of rotating.
	if got := parseProfileList("a, a ,b,,a"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("parseProfileList dedupe = %v, want [a b]", got)
	}
}

func TestModifyPoolRegistryConcurrent(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := "/abs/repo"
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := fmt.Sprintf("p%d", i)
			_ = modifyPoolRegistry(cfg, func(reg poolRegistry) {
				assignPool(reg, repo, "claude", addProfiles(reg[repo]["claude"], []string{p}))
			})
		}(i)
	}
	wg.Wait()
	got, err := repoPool(cfg, repo, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Errorf("after %d concurrent adds the pool has %d profiles, want %d (lost updates — the lock didn't serialize)", n, len(got), n)
	}
}

func TestLoadPoolsCorrupt(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if err := os.WriteFile(poolsFile(cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPools(cfg); err == nil {
		t.Error("corrupt pools.json should error, not silently succeed")
	}
}

func TestCmdPoolDenials(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown agent", []string{"add", "nope", "work"}},
		{"unknown verb", []string{"frobnicate"}},
		{"add missing profiles", []string{"add", "claude"}},
		{"clear missing agent", []string{"clear"}},
		{"flag-like profile", []string{"add", "claude", "--x"}},  // a mistyped flag, not a profile
		{"traversal profile", []string{"add", "claude", "../e"}}, // must not store a path
		{"rm flag-like profile", []string{"rm", "claude", "-foo"}},
		{"target with empty model", []string{"add", "claude", "work@"}},     // credential@model needs the model
		{"target with bad credential", []string{"add", "claude", "../e@m"}}, // the name part is still validated
	} {
		if code, err := a.cmdPool(tc.args); code != 2 || err == nil {
			t.Errorf("%s: code=%d err=%v, want code 2 + error", tc.name, code, err)
		}
	}
}

// A credential@model member is stored verbatim in pools.json ([]string, so existing
// pools load unchanged) and rendered by `coop loop pool` with its model visible.
func TestCmdPoolTargetMembers(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	// Sign the credential in so add doesn't warn (and to mirror real use).
	if err := os.MkdirAll(cfg.AgentProfileDir("claude", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.AgentProfileDir("claude", "work"), ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	out := captureStdout(t, func() {
		if code, err := a.cmdPool([]string{"add", "claude", "work@opus", "work@sonnet"}); code != 0 || err != nil {
			t.Errorf("pool add targets = (%d, %v)", code, err)
		}
	})
	for _, want := range []string{"work@opus", "work@sonnet"} {
		if !strings.Contains(out, want) {
			t.Errorf("showPool should render the target model (%q):\n%s", want, out)
		}
	}
	// pools.json stays a plain []string registry — the members are the wire strings.
	pool, err := repoPool(cfg, cfg.RepoOverride, "claude")
	if err != nil || !slices.Equal(pool, []string{"work@opus", "work@sonnet"}) {
		t.Errorf("stored pool = %v (%v), want the wire strings", pool, err)
	}
	// rm takes the member as shown.
	_ = captureStdout(t, func() {
		if code, err := a.cmdPool([]string{"rm", "claude", "work@opus"}); code != 0 || err != nil {
			t.Errorf("pool rm target = (%d, %v)", code, err)
		}
	})
	pool, _ = repoPool(cfg, cfg.RepoOverride, "claude")
	if !slices.Equal(pool, []string{"work@sonnet"}) {
		t.Errorf("after rm: pool = %v, want [work@sonnet]", pool)
	}
}

// `coop loop pool …` is the documented home — the rotation pool is a setting of the loop —
// and it routes to the same command the top-level `coop pool` alias reaches.
func TestLoopPoolSubcommand(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}}
	if code, err := a.cmdLoop([]string{"pool"}); code != 0 || err != nil {
		t.Fatalf("coop loop pool: code=%d err=%v, want the pool listing", code, err)
	}
	if code, err := a.cmdLoop([]string{"pool", "frobnicate"}); code != 2 || err == nil {
		t.Errorf("coop loop pool frobnicate: code=%d err=%v, want the pool command's own rejection", code, err)
	}
}

func TestProfilePoolSingle(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newProfilePool([]string{"only"})
	if p.rotates() {
		t.Error("a single-profile pool shouldn't rotate")
	}
	reset := now.Add(time.Hour)
	sleep, until := p.onLimit(reset, 1, now)
	if sleep <= 0 || !until.Equal(reset) || p.active().String() != "only" {
		t.Errorf("single-profile limit: sleep=%v until=%v active=%q, want a wait to %v on only", sleep, until, p.active(), reset)
	}
}

func TestProfilePoolStickyRotatesThenWaits(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newProfilePool([]string{"a", "b", "c"})
	if p.active().String() != "a" {
		t.Fatalf("start active = %q, want a", p.active())
	}
	// a limited (resets +2h) → switch to b immediately, no sleep.
	if sleep, _ := p.onLimit(now.Add(2*time.Hour), 1, now); sleep != 0 || p.active().String() != "b" {
		t.Fatalf("after a limited: sleep=%v active=%q, want 0 + b", sleep, p.active())
	}
	// b limited (resets +1h) → switch to c, no sleep.
	if sleep, _ := p.onLimit(now.Add(time.Hour), 2, now); sleep != 0 || p.active().String() != "c" {
		t.Fatalf("after b limited: sleep=%v active=%q, want 0 + c", sleep, p.active())
	}
	// c limited (resets +3h) → all limited → wait for the SOONEST reset (b, +1h).
	sleep, until := p.onLimit(now.Add(3*time.Hour), 3, now)
	if sleep <= 0 {
		t.Fatalf("all limited should sleep, got %v", sleep)
	}
	if !until.Equal(now.Add(time.Hour)) || p.active().String() != "b" {
		t.Errorf("should park on the soonest-resetting profile b (+1h): until=%v active=%q", until, p.active())
	}
}

func TestProfilePoolUnknownResetBacksOff(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newProfilePool([]string{"a", "b"})
	// a limited with no stated reset → b is free → switch, no sleep.
	if sleep, _ := p.onLimit(time.Time{}, 1, now); sleep != 0 || p.active().String() != "b" {
		t.Fatalf("unknown reset, b free: sleep=%v active=%q", sleep, p.active())
	}
	// b also limited, unknown reset → both limited → a bounded backoff sleep.
	sleep, _ := p.onLimit(time.Time{}, 2, now)
	if sleep <= 0 || sleep > limitMaxWait {
		t.Errorf("all limited w/ unknown reset: sleep=%v, want a bounded backoff", sleep)
	}
}

func TestProfilePoolReusesAfterReset(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newProfilePool([]string{"a", "b"})
	p.onLimit(now.Add(time.Hour), 1, now) // a limited until +1h, now active on b
	// Two hours later b hits a limit; a's reset (+1h) is long past → rotate back to a.
	later := now.Add(2 * time.Hour)
	if sleep, _ := p.onLimit(later.Add(time.Hour), 2, later); sleep != 0 || p.active().String() != "a" {
		t.Errorf("a's reset passed: sleep=%v active=%q, want 0 + a", sleep, p.active())
	}
}

func TestBuildPool(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := "/abs/repo"
	signIn := func(profile string) {
		t.Helper()
		p := cfg.AgentProfileDir("claude", profile)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, ".credentials.json"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// No signed-in profile → error (the loop can't run unauthenticated).
	if _, err := buildPool(cfg, repo, "claude"); err == nil {
		t.Error("no signed-in profile should error")
	}
	// Default (no pool configured): every signed-in profile rotates.
	signIn("work")
	signIn("personal")
	pool, err := buildPool(cfg, repo, "claude")
	if err != nil {
		t.Fatal(err)
	}
	got := append([]string{}, pool.members()...)
	slices.Sort(got)
	if !slices.Equal(got, []string{"personal", "work"}) {
		t.Errorf("default pool = %v, want both signed-in profiles", pool.members())
	}
	// A configured pool narrows it, and the authed filter drops a member not signed in.
	if err := setRepoPool(cfg, repo, "claude", []string{"work", "ghost"}); err != nil {
		t.Fatal(err)
	}
	pool, err = buildPool(cfg, repo, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pool.members(), []string{"work"}) {
		t.Errorf("configured pool = %v, want [work] (ghost isn't signed in)", pool.members())
	}
}

func TestRotateOnLimitSwitchesProfile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	a := &app{cfg: cfg}
	pool := newProfilePool([]string{"work", "personal"})
	waits := 3
	// work is limited (resets far ahead) while personal is free → switch now, don't sleep,
	// reset the wait counter, and point cfg at the new profile for the next iteration.
	a.rotateOnLimit("claude", pool, time.Now().Add(time.Hour), &waits, nil)
	if pool.active().String() != "personal" {
		t.Errorf("pool active = %q, want personal", pool.active())
	}
	if got, want := cfg.AgentDir("claude"), cfg.AgentProfileDir("claude", "personal"); got != want {
		t.Errorf("cfg points at %q, want %q", got, want)
	}
	if waits != 0 {
		t.Errorf("waits = %d, want 0 after a free rotation", waits)
	}
}

func TestParsePoolTarget(t *testing.T) {
	for in, want := range map[string]poolTarget{
		"work":             {credential: "work"},
		"work@opus":        {credential: "work", model: "opus"},
		"w@claude-fable-5": {credential: "w", model: "claude-fable-5"},
	} {
		if got := parsePoolTarget(in); got != want {
			t.Errorf("parsePoolTarget(%q) = %+v, want %+v", in, got, want)
		}
	}
	if got := parsePoolTarget("work@opus").String(); got != "work@opus" {
		t.Errorf("String() = %q, want the wire form back", got)
	}
	if got := parsePoolTarget("work").String(); got != "work" {
		t.Errorf("bare String() = %q, want work", got)
	}
}

// The acceptance scenario: pool [work@opus, work@sonnet, other@opus]. A rate limit on
// work@opus falls back to work@sonnet (same credential, no re-auth) BEFORE rotating to
// other@opus, because the limit map is keyed per TARGET — work@sonnet stays available
// while work@opus cools down.
func TestPoolSameCredentialModelFallback(t *testing.T) {
	p := newProfilePool([]string{"work@opus", "work@sonnet", "other@opus"})
	now := time.Now()
	if got := p.active().String(); got != "work@opus" {
		t.Fatalf("start active = %q", got)
	}
	// work@opus limited → the SAME credential's next model, not the other account.
	if sleep, _ := p.onLimit(now.Add(time.Hour), 1, now); sleep != 0 || p.active().String() != "work@sonnet" {
		t.Fatalf("after work@opus limited: sleep=%v active=%q, want 0 + work@sonnet", sleep, p.active())
	}
	if p.active().credential != "work" || p.active().model != "sonnet" {
		t.Fatalf("active target = %+v, want work/sonnet", p.active())
	}
	// work@sonnet limited too → now the other account.
	if sleep, _ := p.onLimit(now.Add(time.Hour), 2, now); sleep != 0 || p.active().String() != "other@opus" {
		t.Fatalf("after work@sonnet limited: sleep=%v active=%q, want 0 + other@opus", sleep, p.active())
	}
	// work@opus's cooldown never blocked work@sonnet: their limits are separate keys.
	if len(p.limited) != 2 {
		t.Fatalf("limited = %v, want two distinct target keys", p.limited)
	}
}

// applyPoolTarget points cfg at the target's credential AND model; a bare target clears
// the model so lower tiers resolve.
func TestApplyPoolTarget(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	pool := newProfilePool([]string{"work@sonnet", "other"})
	a.applyPoolTarget("claude", pool)
	if got := a.cfg.ActiveProfile("claude"); got != "work" {
		t.Errorf("active credential = %q, want work", got)
	}
	if got := a.cfg.ModelFor("claude"); got != "sonnet" {
		t.Errorf("target model = %q, want sonnet", got)
	}
	// Rotate to the bare target → the model clears (falls through to lower tiers).
	pool.onLimit(time.Now().Add(time.Hour), 1, time.Now())
	a.applyPoolTarget("claude", pool)
	if got := a.cfg.ModelFor("claude"); got != "" {
		t.Errorf("bare target: model = %q, want \"\" (cleared)", got)
	}
}

// The loop's model tiers end to end: explicit --model > pool target model > preset lead
// model (fallback slot) > COOP_LOOP_MODEL (only when no preset filled the slot).
func TestLoopModelTiers(t *testing.T) {
	// Explicit --model beats a target's model.
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir(), LoopModel: "loop-default"}}
	a.applyLoopModel("claude", "explicit")
	pool := newProfilePool([]string{"work@sonnet"})
	a.applyPoolTarget("claude", pool)
	if got := a.cfg.ModelFor("claude"); got != "explicit" {
		t.Errorf("explicit vs target = %q, want explicit", got)
	}

	// A target model beats the preset lead's (fallback) model.
	b := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	b.applyPreset(cliFrontier(), "claude") // lead model → the fallback slot
	b.applyLoopModel("claude", "")
	b.applyPoolTarget("claude", pool)
	if got := b.cfg.ModelFor("claude"); got != "sonnet" {
		t.Errorf("target vs preset lead = %q, want sonnet", got)
	}
	// ...and with a BARE target, the preset lead's model resolves again.
	b.applyPoolTarget("claude", newProfilePool([]string{"work"}))
	if got := b.cfg.ModelFor("claude"); got != "claude-fable-5" {
		t.Errorf("bare target under preset = %q, want the lead model", got)
	}

	// COOP_LOOP_MODEL fills the fallback slot only when a preset didn't.
	c := &app{cfg: &config.Config{ConfigDir: t.TempDir(), LoopModel: "loop-default"}}
	c.applyLoopModel("claude", "")
	if got := c.cfg.ModelFor("claude"); got != "loop-default" {
		t.Errorf("LOOP_MODEL fallback = %q, want loop-default", got)
	}
	d := &app{cfg: &config.Config{ConfigDir: t.TempDir(), LoopModel: "loop-default"}}
	d.applyPreset(cliFrontier(), "claude")
	d.applyLoopModel("claude", "")
	if got := d.cfg.ModelFor("claude"); got != "claude-fable-5" {
		t.Errorf("preset lead vs LOOP_MODEL = %q, want the lead model", got)
	}
}
