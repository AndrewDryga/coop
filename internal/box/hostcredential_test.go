package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestGeminiHostCredentialIsCanonicalPrivateAndLeavesDefaultsAlone(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	profile := cfg.AgentProfileDir("gemini", "personal")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	defaults := []byte(`{"theme":"dark","security":{"auth":{"selectedType":"oauth-personal"}}}`)
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), defaults, 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveHostCredential(cfg, gemini, "personal", []byte("portable-personal-key")); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(cfg.ConfigDir, "gemini", "credentials", "personal", "authority.json")
	if info, err := os.Stat(canonical); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("authority is not private", err)
	}
	if strings.HasPrefix(canonical, profile+string(filepath.Separator)) {
		t.Fatal("authority is in legacy profile")
	}
	key, value, found, err := LoadHostCredential(cfg, gemini, "personal")
	if err != nil || !found || key != "GEMINI_API_KEY" || value != "portable-personal-key" {
		t.Fatal("host key unavailable", err)
	}
	if !ProfileAuthed(cfg, "gemini", "personal") || !ProfileCredentialReady(cfg, "gemini", "personal", time.Now()) {
		t.Fatal("canonical key not ready")
	}
	if string(mustReadFile(t, filepath.Join(profile, "settings.json"))) != string(defaults) {
		t.Fatal("host sign-in rewrote repository defaults")
	}
	record, _, err := readNativeAccount(context.Background(), cfg, gemini, "personal")
	if err != nil || record.Selection != "gemini-api-key" {
		t.Fatal("canonical selector missing", err)
	}
	legacy, _ := hostCredentialDir(cfg, "gemini", "personal")
	if _, err := os.Stat(filepath.Join(legacy, "api-key")); !os.IsNotExist(err) {
		t.Fatal("sign-in created a second serving key")
	}
}

func TestGeminiHostCredentialScopesAccountsAndPreservesDefaultEnvPrecedence(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	for account, key := range map[string]string{"personal": "personal-key", "work": "work-key"} {
		if err := SaveHostCredential(cfg, gemini, account, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.SetDefaultProfile("gemini", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=explicit-default\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for account, want := range map[string]string{"personal": "explicit-default", "work": "work-key"} {
		selected, found, err := previewNativeAuthority(context.Background(), cfg, gemini, account)
		if err != nil || !found || selected.state.AccessToken != want {
			t.Fatal("account selection escaped its scope", account, err)
		}
	}
	// An environment value must never hide damaged canonical authority.
	path := filepath.Join(cfg.ConfigDir, "gemini", "credentials", "personal", "authority.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := previewNativeAuthority(context.Background(), cfg, gemini, "personal"); err == nil {
		t.Fatal("environment bypassed unsafe canonical account")
	}
}

func TestGeminiDefaultChangeReinterpretsCredentialSource(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "work", []byte("work-key")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=default-env\n"), 0600); err != nil {
		t.Fatal(err)
	}
	selected, _, err := previewNativeAuthority(context.Background(), cfg, gemini, "work")
	if err != nil || selected.state.AccessToken != "work-key" {
		t.Fatal("named account used default env", err)
	}
	if err := cfg.SetDefaultProfile("gemini", "work"); err != nil {
		t.Fatal(err)
	}
	selected, _, err = previewNativeAuthority(context.Background(), cfg, gemini, "work")
	if err != nil || selected.state.AccessToken != "default-env" {
		t.Fatal("explicit default not honored", err)
	}
}

func TestGeminiCanonicalFamilyIgnoresRetiredNativeSelector(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "work", []byte("canonical-key")); err != nil {
		t.Fatal(err)
	}
	profile := cfg.AgentProfileDir("gemini", "work")
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	selected, found, err := previewNativeAuthority(context.Background(), cfg, gemini, "work")
	if err != nil || !found || !selected.state.APIKey || selected.state.AccessToken != "canonical-key" {
		t.Fatal("retired selector reinterpreted current authority", err)
	}
}

func TestHostCredentialRejectsUnsafeStateAndRemovalIsAccountScoped(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	for _, account := range []string{"work", "personal"} {
		if err := SaveHostCredential(cfg, gemini, account, []byte(account+"-key")); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []string{"bad\nkey", strings.Repeat("x", hostCredentialSizeLimit+1)} {
		if err := SaveHostCredential(cfg, gemini, "invalid", []byte(input)); err == nil {
			t.Fatal("invalid key saved")
		}
	}
	path := filepath.Join(cfg.ConfigDir, "gemini", "credentials", "work", "authority.json")
	original := mustReadFile(t, path)
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadHostCredential(cfg, gemini, "work"); err == nil {
		t.Fatal("malformed canonical authority accepted")
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadHostCredential(cfg, gemini, "work"); err == nil {
		t.Fatal("group-readable authority accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveNativeAccount(context.Background(), cfg, gemini.Name(), "work"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := LoadHostCredential(cfg, gemini, "work"); err != nil || found {
		t.Fatal("removed account is usable", err)
	}
	if !NativeAccountRemoved(cfg, "gemini", "work") {
		t.Fatal("removal did not leave revocation tombstone")
	}
	if _, value, found, err := LoadHostCredential(cfg, gemini, "personal"); err != nil || !found || value != "personal-key" {
		t.Fatal("removal affected sibling", err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
