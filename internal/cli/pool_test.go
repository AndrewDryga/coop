package cli

import (
	"os"
	"slices"
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
