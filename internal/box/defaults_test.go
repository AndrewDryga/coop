package box

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestEnsureClaudeDefaultsFresh(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	ensureClaudeDefaults(cfg, "/workspace")

	s := readJSONMap(t, filepath.Join(dir, "claude", "settings.json"))
	if s["theme"] != "dark" {
		t.Errorf("settings.json theme = %v, want dark", s["theme"])
	}
	if s["skipDangerousModePermissionPrompt"] != true {
		t.Error("settings.json should skip the bypass-permissions prompt")
	}

	c := readJSONMap(t, filepath.Join(dir, "claude", ".claude.json"))
	if c["hasCompletedOnboarding"] != true {
		t.Error("hasCompletedOnboarding not set")
	}
	if c["bypassPermissionsModeAccepted"] != true {
		t.Error("bypassPermissionsModeAccepted not set")
	}
	proj, _ := c["projects"].(map[string]any)
	wp, _ := proj["/workspace"].(map[string]any)
	if wp == nil || wp["hasTrustDialogAccepted"] != true {
		t.Errorf("workdir trust not set: %v", proj)
	}
}

func TestEnsureClaudeDefaultsPreservesAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "claude")
	os.MkdirAll(cdir, 0o755)
	// Pre-existing login state + a user setting that must survive.
	os.WriteFile(filepath.Join(cdir, ".claude.json"),
		[]byte(`{"oauthAccount":{"u":"x"},"numStartups":5}`), 0o600)
	os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(`{"theme":"light"}`), 0o644)
	cfg := &config.Config{ConfigDir: dir}

	ensureClaudeDefaults(cfg, "/workspace")

	c := readJSONMap(t, filepath.Join(cdir, ".claude.json"))
	if c["oauthAccount"] == nil {
		t.Error("oauthAccount was dropped")
	}
	if c["numStartups"] != float64(5) {
		t.Errorf("numStartups changed: %v", c["numStartups"])
	}
	if c["bypassPermissionsModeAccepted"] != true {
		t.Error("bypass flag not merged in")
	}
	// The user's own settings.json must not be overwritten.
	if s := readJSONMap(t, filepath.Join(cdir, "settings.json")); s["theme"] != "light" {
		t.Errorf("settings.json overwritten: theme=%v", s["theme"])
	}

	// Idempotent: a second call must not rewrite the file.
	before, _ := os.ReadFile(filepath.Join(cdir, ".claude.json"))
	ensureClaudeDefaults(cfg, "/workspace")
	after, _ := os.ReadFile(filepath.Join(cdir, ".claude.json"))
	if !bytes.Equal(before, after) {
		t.Error("second call rewrote .claude.json (not idempotent)")
	}
}
