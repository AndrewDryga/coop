package cli

import (
	"os"
	"slices"
	"testing"

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
