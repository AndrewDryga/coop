package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	} {
		if code, err := a.cmdPool(tc.args); code != 2 || err == nil {
			t.Errorf("%s: code=%d err=%v, want code 2 + error", tc.name, code, err)
		}
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
	if sleep <= 0 || !until.Equal(reset) || p.active() != "only" {
		t.Errorf("single-profile limit: sleep=%v until=%v active=%q, want a wait to %v on only", sleep, until, p.active(), reset)
	}
}

func TestProfilePoolStickyRotatesThenWaits(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newProfilePool([]string{"a", "b", "c"})
	if p.active() != "a" {
		t.Fatalf("start active = %q, want a", p.active())
	}
	// a limited (resets +2h) → switch to b immediately, no sleep.
	if sleep, _ := p.onLimit(now.Add(2*time.Hour), 1, now); sleep != 0 || p.active() != "b" {
		t.Fatalf("after a limited: sleep=%v active=%q, want 0 + b", sleep, p.active())
	}
	// b limited (resets +1h) → switch to c, no sleep.
	if sleep, _ := p.onLimit(now.Add(time.Hour), 2, now); sleep != 0 || p.active() != "c" {
		t.Fatalf("after b limited: sleep=%v active=%q, want 0 + c", sleep, p.active())
	}
	// c limited (resets +3h) → all limited → wait for the SOONEST reset (b, +1h).
	sleep, until := p.onLimit(now.Add(3*time.Hour), 3, now)
	if sleep <= 0 {
		t.Fatalf("all limited should sleep, got %v", sleep)
	}
	if !until.Equal(now.Add(time.Hour)) || p.active() != "b" {
		t.Errorf("should park on the soonest-resetting profile b (+1h): until=%v active=%q", until, p.active())
	}
}

func TestProfilePoolUnknownResetBacksOff(t *testing.T) {
	now := time.Unix(1000, 0)
	p := newProfilePool([]string{"a", "b"})
	// a limited with no stated reset → b is free → switch, no sleep.
	if sleep, _ := p.onLimit(time.Time{}, 1, now); sleep != 0 || p.active() != "b" {
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
	if sleep, _ := p.onLimit(later.Add(time.Hour), 2, later); sleep != 0 || p.active() != "a" {
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
	got := append([]string{}, pool.profiles...)
	slices.Sort(got)
	if !slices.Equal(got, []string{"personal", "work"}) {
		t.Errorf("default pool = %v, want both signed-in profiles", pool.profiles)
	}
	// A configured pool narrows it, and the authed filter drops a member not signed in.
	if err := setRepoPool(cfg, repo, "claude", []string{"work", "ghost"}); err != nil {
		t.Fatal(err)
	}
	pool, err = buildPool(cfg, repo, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pool.profiles, []string{"work"}) {
		t.Errorf("configured pool = %v, want [work] (ghost isn't signed in)", pool.profiles)
	}
}

func TestRotateOnLimitSwitchesProfile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	a := &app{cfg: cfg}
	pool := newProfilePool([]string{"work", "personal"})
	waits := 3
	// work is limited (resets far ahead) while personal is free → switch now, don't sleep,
	// reset the wait counter, and point cfg at the new profile for the next iteration.
	a.rotateOnLimit("claude", pool, time.Now().Add(time.Hour), &waits)
	if pool.active() != "personal" {
		t.Errorf("pool active = %q, want personal", pool.active())
	}
	if got, want := cfg.AgentDir("claude"), cfg.AgentProfileDir("claude", "personal"); got != want {
		t.Errorf("cfg points at %q, want %q", got, want)
	}
	if waits != 0 {
		t.Errorf("waits = %d, want 0 after a free rotation", waits)
	}
}
