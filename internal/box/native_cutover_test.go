package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
)

func cutoverFixture(t *testing.T) (*config.Config, agents.Agent, string, []byte) {
	t.Helper()
	cfg := &config.Config{ConfigDir: t.TempDir()}
	ag, _ := agents.Get("codex")
	home := cfg.AgentProfileDir("codex", "work")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-original-access"}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, ag, home, raw
}

func TestNativeCutoverRecoversEveryRetirementCheckpoint(t *testing.T) {
	for failAt := 1; failAt <= 4; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			cfg, ag, home, raw := cutoverFixture(t)
			checks := 0
			fault := errors.New("inert cutover checkpoint interruption")
			_, _, err := migrateNativeAccount(context.Background(), cfg, ag, "work", func() error {
				checks++
				if checks == failAt {
					return fault
				}
				return nil
			})
			if !errors.Is(err, fault) {
				t.Fatalf("checkpoint %d: %v", failAt, err)
			}
			if failAt < 4 {
				data, err := os.ReadFile(filepath.Join(home, "auth.json"))
				if err != nil || !bytes.Equal(data, raw) {
					t.Fatal("grant retired before its fenced checkpoint", err)
				}
			}
			record, exists, err := migrateNativeAccount(context.Background(), cfg, ag, "work", func() error { return nil })
			if err != nil || !exists || record.Epoch != 1 || record.Revision != 1 || record.Migration == "" {
				t.Fatal("cutover did not recover exactly once", record, err)
			}
			if _, err := os.Stat(filepath.Join(home, "auth.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("legacy grant remains discoverable")
			}
			retained, err := os.ReadFile(filepath.Join(cfg.ConfigDir, "codex", "credentials", "work", "cutover.json"))
			if err != nil {
				t.Fatal(err)
			}
			var journal nativeCutover
			if json.Unmarshal(retained, &journal) != nil || !bytes.Equal(journal.Sources[0].Data, raw) {
				t.Fatal("original recovery bytes lost")
			}
			again, _, err := ensureNativeAccount(context.Background(), cfg, runtime.Runtime{}, ag, "work")
			if err != nil || again.Epoch != record.Epoch || again.Revision != record.Revision {
				t.Fatal("reused legacy authority or new epoch", err)
			}
		})
	}
}

func TestNativeCutoverChangedSourceAndInvalidReceiptPreserveBoth(t *testing.T) {
	cfg, ag, home, _ := cutoverFixture(t)
	sources, err := legacyAccountSources(cfg, ag, "work")
	if err != nil {
		t.Fatal(err)
	}
	root, lock, data, err := prepareNativeCutover(context.Background(), cfg, ag, "work", sources)
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	root.close()
	changed := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"inert-new-native-login"}`)
	if err := config.WriteFileAtomic(filepath.Join(home, "auth.json"), changed); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrateNativeAccount(context.Background(), cfg, ag, "work", func() error { return nil }); err == nil {
		t.Fatal("source replacement accepted")
	}
	actual, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || !bytes.Equal(actual, changed) {
		t.Fatal("new native grant discarded")
	}
	var journal nativeCutover
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatal(err)
	}
	journal.Next.Revoked = true
	if err := validateNativeCutover(cfg, ag, "work", journal); err == nil {
		t.Fatal("invalid authority reaches destructive retirement")
	}
}

func TestNativeCutoverRetirementSurvivesDeviceRenumbering(t *testing.T) {
	_, _, home, _ := cutoverFixture(t)
	source, err := readCutoverSource(home, "auth.json", 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	source.Device++ // A remount must not invalidate the exact retained grant.
	if err := retireCutoverSource(source); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired grant remains: %v", err)
	}
}

func TestNativeCutoverInvalidImportDoesNotPoisonCanonicalNamespace(t *testing.T) {
	cfg, ag, home, raw := cutoverFixture(t)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrateNativeAccount(context.Background(), cfg, ag, "work", func() error { return nil }); err == nil {
		t.Fatal("malformed legacy auth accepted")
	}
	if _, exists, err := readNativeAccount(context.Background(), cfg, ag, "work"); err != nil || exists {
		t.Fatal("failed import created canonical authority", err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrateNativeAccount(context.Background(), cfg, ag, "work", func() error { return nil }); err != nil {
		t.Fatal("intact authority could not retry", err)
	}
}

func TestNativeRemovalPurgesRetainedGrantCustodyAndKeepsTombstone(t *testing.T) {
	cfg, ag, _, _ := cutoverFixture(t)
	if _, _, err := migrateNativeAccount(context.Background(), cfg, ag, "work", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.ConfigDir, "codex", "credentials", "work")
	for _, name := range []string{"recovery-fixture.json", ".private-fixture", ".renewal-fixture"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte("retained-grant"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveNativeAccount(context.Background(), cfg, "codex", "work"); err != nil {
		t.Fatal(err)
	}
	if !NativeAccountRemoved(cfg, "codex", "work") {
		t.Fatal("removal has no permanent tombstone")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "authority.json" && entry.Name() != ".authority.lock" {
			t.Fatal("retained grant survived confirmed removal", entry.Name())
		}
	}
	first, _, err := readNativeAccount(context.Background(), cfg, ag, "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveNativeAccount(context.Background(), cfg, "codex", "work"); err != nil {
		t.Fatal(err)
	}
	again, _, err := readNativeAccount(context.Background(), cfg, ag, "work")
	if err != nil || again.Epoch != first.Epoch {
		t.Fatal("cleanup retry replaces tombstone", err)
	}
}

func TestNativeSignInRefusesEncryptedOnlyLegacyGrant(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{ConfigDir: t.TempDir()}
	legacy := cfg.AgentProfileDir("gemini", "default")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(legacy, "gemini-credentials.json")
	old := []byte("inert-unknown-encrypted-cache")
	if err := os.WriteFile(cache, old, 0600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := os.Chmod(stage, 0700); err != nil {
		t.Fatal(err)
	}
	settings := []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`)
	key := []byte("inert-new-host-key")
	for name, data := range map[string][]byte{"settings.json": settings, "api-key": key} {
		if err := os.WriteFile(filepath.Join(stage, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	err := ImportNativeSignIn(ctx, cfg, "gemini", "default", stage)
	if err == nil || !strings.Contains(err.Error(), "encrypted cache") {
		t.Fatal("encrypted legacy grant bypassed sign-in retirement", err)
	}
	data, err := os.ReadFile(cache)
	if err != nil || !bytes.Equal(data, old) {
		t.Fatal("unknown original grant lost", err)
	}
	data, err = os.ReadFile(filepath.Join(stage, "api-key"))
	if err != nil || !bytes.Equal(data, key) {
		t.Fatal("failed sign-in stage lost", err)
	}
	ag, _ := agents.Get("gemini")
	if _, exists, err := readNativeAccount(ctx, cfg, ag, "default"); err != nil || exists {
		t.Fatal("unsafe sign-in published canonical authority", err)
	}
}
