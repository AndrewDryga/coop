package box

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestNativeHomeOwnsACompleteRepositoryAccountDomain(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	a, b := t.TempDir(), t.TempDir()
	for _, provider := range []string{"claude", "codex", "gemini", "grok"} {
		var calls int
		seed := func(home string) error {
			calls++
			return os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"theme":"initial"}`), 0o600)
		}
		home, err := prepareNativeHome(context.Background(), cfg, provider, "work", a, false, seed)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "history-canary"), []byte("private-A"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"theme":"native-edit"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		again, err := prepareNativeHome(context.Background(), cfg, provider, "work", a, false, seed)
		if err != nil || again != home || calls != 1 {
			t.Fatalf("reuse=%q seed calls=%d err=%v", again, calls, err)
		}
		data, err := os.ReadFile(filepath.Join(again, "settings.json"))
		if err != nil || !strings.Contains(string(data), "native-edit") {
			t.Fatal("host defaults overwrote native settings")
		}
		for _, selection := range []struct{ account, repo string }{{"work", b}, {"personal", a}} {
			other, err := prepareNativeHome(context.Background(), cfg, provider, selection.account, selection.repo, false, seed)
			if err != nil || other == home {
				t.Fatalf("selection mixed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(other, "history-canary")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("other repository/account sees history")
			}
		}
		if _, err := os.Stat(filepath.Join(home, "owner.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("host ownership is inside writable native home")
		}
	}
}

func TestNativeHomeACPIsRepositoryScopedAndCredentialIndependent(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	repo := t.TempDir()
	first, err := prepareNativeHome(context.Background(), cfg, "codex", "work", repo, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, err := prepareNativeHome(context.Background(), cfg, "codex", "personal", repo, true, nil)
	if err != nil || first != otherAccount {
		t.Fatal("ACP account switch loses the native home")
	}
	otherRepo, err := prepareNativeHome(context.Background(), cfg, "codex", "work", t.TempDir(), true, nil)
	if err != nil || first == otherRepo {
		t.Fatal("ACP home crosses repositories")
	}
}

func TestNativeHomeConcurrentCreationSeedsOnce(t *testing.T) {
	cfg, repo := &config.Config{ConfigDir: t.TempDir()}, t.TempDir()
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := prepareNativeHome(context.Background(), cfg, "codex", "work", repo, false, func(string) error { calls.Add(1); return nil })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("seeding raced: %d", calls.Load())
	}
}

func TestNativeHomeSeedCannotPublishNonprivateDirectory(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	var seeded string
	_, err := prepareNativeHome(context.Background(), cfg, "codex", "work", t.TempDir(), false, func(home string) error {
		seeded = home
		return os.Chmod(home, 0o755)
	})
	if err == nil {
		t.Fatal("nonprivate home was published")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(seeded), "owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed home acquired authoritative ownership")
	}
}

func TestNativeHomeRefusesAmbiguousInterruptedAndRedirectedHomes(t *testing.T) {
	for _, kind := range []string{"interrupted", "wrong-owner", "linked-owner", "linked-home"} {
		t.Run(kind, func(t *testing.T) {
			cfg, repo := &config.Config{ConfigDir: t.TempDir()}, t.TempDir()
			seed := func(home string) error {
				if kind == "interrupted" {
					return errors.New("inert seed failure")
				}
				return nil
			}
			home, err := prepareNativeHome(context.Background(), cfg, "codex", "work", repo, false, seed)
			if kind != "interrupted" && err != nil {
				t.Fatal(err)
			}
			if kind == "interrupted" && err == nil {
				t.Fatal("interrupted initialization succeeded")
			}
			owner := filepath.Join(filepath.Dir(home), "owner.json")
			switch kind {
			case "wrong-owner":
				if err := os.WriteFile(owner, []byte(`{"version":1,"project":"/another"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "linked-owner":
				if err := os.Link(owner, filepath.Join(t.TempDir(), "alias")); err != nil {
					t.Fatal(err)
				}
			case "linked-home":
				if err := os.Rename(home, home+".retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), home); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			if _, err := prepareNativeHome(context.Background(), cfg, "codex", "work", repo, false, func(string) error { called = true; return nil }); err == nil || called {
				t.Fatal("ambiguous state was silently replaced or admitted")
			}
		})
	}
}
