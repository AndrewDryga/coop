package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestEnsureClaudeDefaultsFresh(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	claudeAgent{}.EnsureDefaults(cfg, "/workspace")

	s := readJSONMap(t, filepath.Join(dir, "claude", "settings.json"))
	if s["theme"] != "dark" {
		t.Errorf("settings.json theme = %v, want dark", s["theme"])
	}
	if s["skipDangerousModePermissionPrompt"] != true {
		t.Error("settings.json should skip the bypass-permissions prompt")
	}
	sb, _ := s["sandbox"].(map[string]any)
	if sb == nil || sb["enabled"] != false || sb["failIfUnavailable"] != false {
		t.Errorf("sandbox should be pinned off: %v", s["sandbox"])
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

	claudeAgent{}.EnsureDefaults(cfg, "/workspace")

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
	claudeAgent{}.EnsureDefaults(cfg, "/workspace")
	after, _ := os.ReadFile(filepath.Join(cdir, ".claude.json"))
	if !bytes.Equal(before, after) {
		t.Error("second call rewrote .claude.json (not idempotent)")
	}
}

func TestEnsureCodexDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	cfgPath := filepath.Join(dir, "codex", "config.toml")

	// Fresh: appends a trust entry for the workdir.
	codexAgent{}.EnsureDefaults(cfg, "/Users/x/proj")
	got := readFile(t, cfgPath)
	if !strings.Contains(got, `[projects."/Users/x/proj"]`) || !strings.Contains(got, `trust_level = "trusted"`) {
		t.Errorf("config.toml missing trust entry:\n%s", got)
	}

	// Idempotent: a second call must not duplicate or rewrite.
	before := readFile(t, cfgPath)
	codexAgent{}.EnsureDefaults(cfg, "/Users/x/proj")
	if after := readFile(t, cfgPath); after != before {
		t.Error("second call changed config.toml (not idempotent)")
	}

	// A different workdir adds a second entry; the first survives.
	codexAgent{}.EnsureDefaults(cfg, "/Users/x/other")
	got = readFile(t, cfgPath)
	if !strings.Contains(got, `[projects."/Users/x/proj"]`) || !strings.Contains(got, `[projects."/Users/x/other"]`) {
		t.Errorf("expected both project entries:\n%s", got)
	}
}

func TestEnsureCodexDefaultsPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "codex")
	os.MkdirAll(cdir, 0o755)
	os.WriteFile(filepath.Join(cdir, "config.toml"), []byte("model = \"o3\"\n"), 0o644)

	codexAgent{}.EnsureDefaults(&config.Config{ConfigDir: dir}, "/w")
	got := readFile(t, filepath.Join(cdir, "config.toml"))
	if !strings.Contains(got, `model = "o3"`) {
		t.Error("existing config was dropped")
	}
	if !strings.Contains(got, `[projects."/w"]`) {
		t.Error("trust entry not appended")
	}
}

func TestEnsureCodexDefaultsHardensSQLiteFeedbackLog(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not available")
	}
	dir := t.TempDir()
	cdir := filepath.Join(dir, "codex")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(cdir, "logs_2.sqlite")
	sql := `
CREATE TABLE logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  ts_nanos INTEGER NOT NULL,
  level TEXT NOT NULL,
  target TEXT NOT NULL,
  estimated_bytes INTEGER NOT NULL DEFAULT 0
);
INSERT INTO logs(ts, ts_nanos, level, target) VALUES (1, 0, 'INFO', 'before');
`
	if out, err := exec.Command(sqlite, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("sqlite setup: %v\n%s", err, out)
	}

	codexAgent{}.EnsureDefaults(&config.Config{ConfigDir: dir}, "/w")

	insert := `INSERT INTO logs(ts, ts_nanos, level, target) VALUES (2, 0, 'INFO', 'after'); SELECT count(*) FROM logs;`
	out, err := exec.Command(sqlite, db, insert).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite insert after hardening: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("logs row count after hardening = %q, want 1", strings.TrimSpace(string(out)))
	}
	trigger := `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='block_log_inserts';`
	out, err = exec.Command(sqlite, db, trigger).CombinedOutput()
	if err != nil {
		t.Fatalf("sqlite trigger lookup: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("block_log_inserts trigger count = %q, want 1", strings.TrimSpace(string(out)))
	}
}

func TestEnsureGeminiDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	path := filepath.Join(dir, "gemini", "settings.json")

	folderTrust := func(t *testing.T) (any, bool) {
		t.Helper()
		m := readJSONMap(t, path)
		sec, _ := m["security"].(map[string]any)
		ft, _ := sec["folderTrust"].(map[string]any)
		v, ok := ft["enabled"]
		return v, ok
	}

	// Missing → valid JSON with the folder-trust prompt disabled.
	geminiAgent{}.EnsureDefaults(cfg, "")
	if v, ok := folderTrust(t); !ok || v != false {
		t.Errorf("missing settings: folderTrust.enabled = %v (present=%v), want false", v, ok)
	}

	// Empty file (the launch crash) → same.
	os.WriteFile(path, []byte(""), 0o644)
	geminiAgent{}.EnsureDefaults(cfg, "")
	if v, ok := folderTrust(t); !ok || v != false {
		t.Errorf("empty settings: folderTrust.enabled = %v (present=%v), want false", v, ok)
	}

	// Existing settings preserved; the folder-trust disable is added alongside.
	os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o644)
	geminiAgent{}.EnsureDefaults(cfg, "")
	if m := readJSONMap(t, path); m["theme"] != "dark" {
		t.Errorf("existing theme dropped: %v", m["theme"])
	}
	if v, _ := folderTrust(t); v != false {
		t.Errorf("folderTrust not disabled on existing settings: %v", v)
	}

	// A user's explicit folderTrust choice is respected, not overridden.
	os.WriteFile(path, []byte(`{"security":{"folderTrust":{"enabled":true}}}`), 0o644)
	geminiAgent{}.EnsureDefaults(cfg, "")
	if v, _ := folderTrust(t); v != true {
		t.Errorf("user's folderTrust=true should be respected, got %v", v)
	}
}
