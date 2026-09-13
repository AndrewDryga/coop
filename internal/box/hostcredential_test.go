package box

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestGeminiHostCredentialIsPrivatePortableAndLeavesNativeStoreAlone(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	profile := "personal"
	profileDir := cfg.AgentProfileDir("gemini", profile)
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(profileDir, "gemini-credentials.json")
	if err := os.WriteFile(native, []byte("corrupt-but-user-owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "settings.json"), []byte(`{"theme":"dark","security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveHostCredential(cfg, gemini, profile, []byte("portable-personal-key")); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(native); string(data) != "corrupt-but-user-owned" {
		t.Fatalf("native Gemini credential changed: %q", data)
	}
	dir, err := hostCredentialDir(cfg, "gemini", profile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(dir, profileDir+string(filepath.Separator)) {
		t.Fatalf("host credential %s is inside mounted profile %s", dir, profileDir)
	}
	info, err := os.Stat(filepath.Join(dir, "api-key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("host credential mode = %v, %v", info, err)
	}
	key, value, found, err := LoadHostCredential(cfg, gemini, profile)
	if err != nil || !found || key != "GEMINI_API_KEY" || value != "portable-personal-key" {
		t.Fatalf("loaded host credential = (%q, %q, %v, %v)", key, value, found, err)
	}
	if !ProfileAuthed(cfg, "gemini", profile) || !ProfileCredentialReady(cfg, "gemini", profile, time.Now()) {
		t.Fatal("portable Gemini credential was not ready")
	}
	settings, err := os.ReadFile(filepath.Join(profileDir, "settings.json"))
	if err != nil || !strings.Contains(string(settings), `"selectedType": "gemini-api-key"`) || !strings.Contains(string(settings), `"theme": "dark"`) {
		t.Fatalf("Gemini selector was not updated safely: %s, %v", settings, err)
	}
}

func TestGeminiHostCredentialScopesAccountsAndPreservesDefaultEnvPrecedence(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	for profile, key := range map[string]string{"personal": "personal-key", "work": "work-key"} {
		if err := SaveHostCredential(cfg, gemini, profile, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	artifacts := defaultCompositionArtifactOps()

	cfg.SetActiveProfile("gemini", "work")
	envFile, tmp, err := prepareBoxEnvFile(cfg, RunSpec{Homes: true, Agent: "gemini"}, artifacts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	values := EnvFileValues(envFile)
	if values["GEMINI_API_KEY"] != "work-key" || strings.Contains(string(mustReadFile(t, envFile)), "personal-key") {
		t.Fatalf("named Gemini account env = %#v", values)
	}

	cfg.SetActiveProfile("gemini", "personal")
	if err := cfg.SetDefaultProfile("gemini", "personal"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=explicit-default\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile, tmp, err = prepareBoxEnvFile(cfg, RunSpec{Homes: true, Agent: "gemini"}, artifacts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	if got := EnvFileValues(envFile)["GEMINI_API_KEY"]; got != "explicit-default" {
		t.Fatalf("default env precedence = %q, want explicit-default", got)
	}
	// Even a damaged stored key cannot veto the explicit default environment authority.
	personalDir, _ := hostCredentialDir(cfg, "gemini", "personal")
	if err := os.Chmod(filepath.Join(personalDir, "api-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ProfileCredentialReady(cfg, "gemini", "personal", time.Now()) {
		t.Fatal("unsafe unused host key overrode the explicit default environment key")
	}
	envFile, tmp, err = prepareBoxEnvFile(cfg, RunSpec{Homes: true, Agent: "gemini"}, artifacts, nil)
	if err != nil {
		t.Fatalf("explicit default env was blocked by unused host state: %v", err)
	}
	defer os.Remove(tmp)
	if got := EnvFileValues(envFile)["GEMINI_API_KEY"]; got != "explicit-default" {
		t.Fatalf("default env after damaged host state = %q", got)
	}
}

func TestGeminiHostCredentialIsInactiveOutsideItsSelectedAuthFamily(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "work", []byte("api-key-must-stay-idle")); err != nil {
		t.Fatal(err)
	}
	profileDir := cfg.AgentProfileDir("gemini", "work")
	if err := os.WriteFile(filepath.Join(profileDir, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"oauth-personal"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.SetActiveProfile("gemini", "work")
	if ProfileAuthed(cfg, "gemini", "work") || ProfileCredentialReady(cfg, "gemini", "work", time.Now()) {
		t.Fatal("inactive API key authenticated an OAuth-selected account")
	}
	envFile, tmp, err := prepareBoxEnvFile(cfg, RunSpec{Homes: true, Agent: "gemini"}, defaultCompositionArtifactOps(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	if got := EnvFileValues(envFile)["GEMINI_API_KEY"]; got != "" {
		t.Fatal("inactive API key entered the OAuth-selected box")
	}
}

func TestHostCredentialRejectsUnsafeStateAndRemovalIsAccountScoped(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "work", []byte("work-key")); err != nil {
		t.Fatal(err)
	}
	if err := SaveHostCredential(cfg, gemini, "personal", []byte("personal-key")); err != nil {
		t.Fatal(err)
	}
	if err := SaveHostCredential(cfg, gemini, "large", []byte(strings.Repeat("x", hostCredentialSizeLimit+1))); err == nil {
		t.Fatal("oversized API key was saved")
	}
	dir, _ := hostCredentialDir(cfg, "gemini", "work")
	path := filepath.Join(dir, "api-key")
	if err := os.WriteFile(path, []byte("bad\nkey"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadHostCredential(cfg, gemini, "work"); err == nil {
		t.Fatal("multiline stored API key was accepted")
	}
	if err := os.WriteFile(path, []byte("work-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadHostCredential(cfg, gemini, "work"); err == nil {
		t.Fatal("group-readable stored API key was accepted")
	}
	if err := RemoveHostCredential(cfg, gemini, "work"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := LoadHostCredential(cfg, gemini, "work"); err != nil || found {
		t.Fatalf("removed work key = (%v, %v)", found, err)
	}
	if _, value, found, err := LoadHostCredential(cfg, gemini, "personal"); err != nil || !found || value != "personal-key" {
		t.Fatalf("personal key changed during work removal = (%q, %v, %v)", value, found, err)
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
