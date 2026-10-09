package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func TestHistoryKeyFollowsTheCanonicalRepository(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "My Repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	key, project, err := HistoryKey(repo)
	if err != nil {
		t.Fatal(err)
	}
	if viaLink, _, err := HistoryKey(link); err != nil || viaLink != key {
		t.Errorf("a symlinked spelling got its own store: %q vs %q (%v)", viaLink, key, err)
	}
	if !strings.HasPrefix(key, "My-Repo-") || len(key) != len("My-Repo-")+16 || strings.ContainsAny(key, "/ ") {
		t.Errorf("key %q should be the sanitized folder name and 16 hex digits", key)
	}
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if otherKey, _, _ := HistoryKey(other); otherKey == key {
		t.Error("two repositories share a store key")
	}
	if !filepath.IsAbs(project) {
		t.Errorf("project %q should be canonical and absolute", project)
	}
	if _, _, err := HistoryKey("relative/path"); err == nil {
		t.Error("a relative repository path must be refused")
	}
}

func TestHistoryStoresListOnlyCompleteNativeHomes(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	home, err := prepareNativeHome(context.Background(), cfg, "claude", "work", t.TempDir(), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := HistoryAccountRoot(cfg, "claude", "work")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "planted")); err != nil {
		t.Fatal(err)
	}
	if got := HistoryStores(cfg, "claude", "work"); len(got) != 1 || got[0] != home {
		t.Fatalf("HistoryStores=%v, want %s", got, home)
	}
}

func TestRunSelectsCompleteNativeHomesWithoutAccountMounts(t *testing.T) {
	for _, name := range agents.Names() {
		for _, acp := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "-ordinary", true: "-acp"}[acp], func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Egress: "none"}
				repo := t.TempDir()
				home, err := PrepareNativeHome(context.Background(), cfg, runtime.Runtime{}, name, cfg.ActiveProfile(name), repo, acp)
				if err != nil {
					t.Fatal(err)
				}
				scoped := cfg.WithNativeHomes(map[string]string{name: home})
				mounts := mountedWritables(scoped, RunSpec{Agent: name, Homes: true, ShareACPSessions: acp})
				if len(mounts) != 1 || mounts[0].Host != home || mounts[0].Box != "/home/node/."+name {
					t.Fatalf("not one complete native home: %+v", mounts)
				}
				if strings.Contains(home, "/profiles/") {
					t.Fatal("host account state entered mount plan")
				}
				if got := mountedWritables(cfg, RunSpec{Agent: name, Homes: true}); len(got) != 0 {
					t.Fatal("missing selection fell back to a raw account profile")
				}
			})
		}
	}
}
