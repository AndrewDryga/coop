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
	if err := (claudeAgent{}).EnsureDefaults(cfg, "/workspace"); err != nil {
		t.Fatal(err)
	}

	s := readJSONMap(t, filepath.Join(cfg.AgentDir("claude"), "settings.json"))
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

	c := readJSONMap(t, filepath.Join(cfg.AgentDir("claude"), ".claude.json"))
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
	cfg := &config.Config{ConfigDir: dir}
	cdir := cfg.AgentDir("claude")
	os.MkdirAll(cdir, 0o755)
	// Pre-existing login state + a user setting that must survive.
	os.WriteFile(filepath.Join(cdir, ".claude.json"),
		[]byte(`{"oauthAccount":{"u":"x"},"numStartups":5}`), 0o600)
	os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(`{"theme":"light"}`), 0o644)

	if err := (claudeAgent{}).EnsureDefaults(cfg, "/workspace"); err != nil {
		t.Fatal(err)
	}

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
	if err := (claudeAgent{}).EnsureDefaults(cfg, "/workspace"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(cdir, ".claude.json"))
	if !bytes.Equal(before, after) {
		t.Error("second call rewrote .claude.json (not idempotent)")
	}
}

func TestEnsureCodexDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	cfgPath := filepath.Join(cfg.AgentDir("codex"), "config.toml")

	// Fresh: appends a trust entry for the workdir.
	if err := (codexAgent{}).EnsureDefaults(cfg, "/Users/x/proj"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, cfgPath)
	if !strings.Contains(got, `[projects."/Users/x/proj"]`) || !strings.Contains(got, `trust_level = "trusted"`) {
		t.Errorf("config.toml missing trust entry:\n%s", got)
	}

	// Idempotent: a second call must not duplicate or rewrite.
	before := readFile(t, cfgPath)
	if err := (codexAgent{}).EnsureDefaults(cfg, "/Users/x/proj"); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, cfgPath); after != before {
		t.Error("second call changed config.toml (not idempotent)")
	}

	// A different workdir adds a second entry; the first survives.
	if err := (codexAgent{}).EnsureDefaults(cfg, "/Users/x/other"); err != nil {
		t.Fatal(err)
	}
	got = readFile(t, cfgPath)
	if !strings.Contains(got, `[projects."/Users/x/proj"]`) || !strings.Contains(got, `[projects."/Users/x/other"]`) {
		t.Errorf("expected both project entries:\n%s", got)
	}
}

func TestEnsureCodexDefaultsPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	cdir := cfg.AgentDir("codex")
	os.MkdirAll(cdir, 0o755)
	os.WriteFile(filepath.Join(cdir, "config.toml"), []byte("model = \"o3\"\n"), 0o644)

	if err := (codexAgent{}).EnsureDefaults(cfg, "/w"); err != nil {
		t.Fatal(err)
	}
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
	cfg := &config.Config{ConfigDir: dir}
	cdir := cfg.AgentDir("codex")
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

	if err := (codexAgent{}).EnsureDefaults(cfg, "/w"); err != nil {
		t.Fatal(err)
	}

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
	path := filepath.Join(cfg.AgentDir("gemini"), "settings.json")

	folderTrust := func(t *testing.T) (any, bool) {
		t.Helper()
		m := readJSONMap(t, path)
		sec, _ := m["security"].(map[string]any)
		ft, _ := sec["folderTrust"].(map[string]any)
		v, ok := ft["enabled"]
		return v, ok
	}

	// Missing → valid JSON with the folder-trust prompt disabled.
	if err := (geminiAgent{}).EnsureDefaults(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if v, ok := folderTrust(t); !ok || v != false {
		t.Errorf("missing settings: folderTrust.enabled = %v (present=%v), want false", v, ok)
	}

	// Empty file (the launch crash) → same.
	os.WriteFile(path, []byte(""), 0o644)
	if err := (geminiAgent{}).EnsureDefaults(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if v, ok := folderTrust(t); !ok || v != false {
		t.Errorf("empty settings: folderTrust.enabled = %v (present=%v), want false", v, ok)
	}

	// Existing settings preserved; the folder-trust disable is added alongside.
	os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o644)
	if err := (geminiAgent{}).EnsureDefaults(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if m := readJSONMap(t, path); m["theme"] != "dark" {
		t.Errorf("existing theme dropped: %v", m["theme"])
	}
	if v, _ := folderTrust(t); v != false {
		t.Errorf("folderTrust not disabled on existing settings: %v", v)
	}

	// A user's explicit folderTrust choice is respected, not overridden.
	os.WriteFile(path, []byte(`{"security":{"folderTrust":{"enabled":true}}}`), 0o644)
	if err := (geminiAgent{}).EnsureDefaults(cfg, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := folderTrust(t); v != true {
		t.Errorf("user's folderTrust=true should be respected, got %v", v)
	}
}

func TestEnsureDefaultsPreservesMalformedFiles(t *testing.T) {
	tests := []struct {
		name, provider, file, body string
		ensure                     func(*config.Config) error
	}{
		{
			name: "Claude settings JSON", provider: "claude", file: "settings.json", body: "{\n",
			ensure: func(cfg *config.Config) error { return (claudeAgent{}).EnsureDefaults(cfg, "/workspace") },
		},
		{
			name: "Claude state is not an object", provider: "claude", file: ".claude.json", body: "null\n",
			ensure: func(cfg *config.Config) error { return (claudeAgent{}).EnsureDefaults(cfg, "/workspace") },
		},
		{
			name: "Gemini settings JSON", provider: "gemini", file: "settings.json", body: "[1]\n",
			ensure: func(cfg *config.Config) error { return (geminiAgent{}).EnsureDefaults(cfg, "") },
		},
		{
			name: "Codex config TOML", provider: "codex", file: "config.toml", body: "broken = {\n",
			ensure: func(cfg *config.Config) error { return (codexAgent{}).EnsureDefaults(cfg, "/workspace") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			path := filepath.Join(cfg.AgentDir(tc.provider), tc.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			err := tc.ensure(cfg)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("EnsureDefaults error = %v, want path-specific parse error", err)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != tc.body {
				t.Fatalf("malformed file = %q, %v; want original %q", got, readErr, tc.body)
			}
			if tc.provider == "claude" && tc.file == ".claude.json" {
				if _, statErr := os.Stat(filepath.Join(cfg.AgentDir("claude"), "settings.json")); !os.IsNotExist(statErr) {
					t.Fatalf("Claude wrote settings before validating .claude.json: %v", statErr)
				}
			}
		})
	}
}

func TestDefaultsFileReaderRejectsLinksAndNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readDefaultsFile(link); err == nil {
		t.Fatal("defaults reader unexpectedly followed a symbolic link")
	}
	nonRegular := filepath.Join(dir, "directory")
	if err := os.Mkdir(nonRegular, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readDefaultsFile(nonRegular); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory read error = %v, want non-regular refusal", err)
	}
}

func TestWriteJSONFileReportsWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "settings.json")
	if err := writeJSONFile(path, map[string]any{"enabled": true}, 0o600); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("writeJSONFile error = %v, want target-specific failure", err)
	}
}
