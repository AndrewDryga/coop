package box

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestEnsureProfilesDirMigratesLegacy(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	base := filepath.Join(dir, "claude")
	// Seed a legacy flat login: credentials + a session dir + settings.
	if err := os.MkdirAll(filepath.Join(base, "projects", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".credentials.json"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureProfilesDir(cfg, "claude"); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(base, "profiles", "default")
	// The legacy login moved into profiles/default — creds and the session dir.
	if !fileExists(filepath.Join(def, ".credentials.json")) {
		t.Error("credentials not migrated into profiles/default")
	}
	if !dirExists(filepath.Join(def, "projects", "x")) {
		t.Error("projects/ not migrated into profiles/default")
	}
	// The bare agent dir no longer holds the flat creds.
	if fileExists(filepath.Join(base, ".credentials.json")) {
		t.Error("legacy credentials still at the flat path")
	}
	// AgentProfileDir now resolves "default" under profiles/ (the post-migration invariant).
	if got := cfg.AgentProfileDir("claude", "default"); got != def {
		t.Errorf("default resolves to %q, want %q", got, def)
	}
	// The vault dir is owner-only.
	if fi, err := os.Stat(filepath.Join(base, "profiles")); err == nil && fi.Mode().Perm() != 0o700 {
		t.Errorf("profiles/ perms = %o, want 700", fi.Mode().Perm())
	}
	// Idempotent: a second call is a no-op and doesn't error.
	if err := EnsureProfilesDir(cfg, "claude"); err != nil {
		t.Fatalf("second call errored: %v", err)
	}
}

func TestEnsureProfilesDirPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	// A vault already in the profiles/ layout with a real default credential.
	def := filepath.Join(dir, "codex", "profiles", "default")
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(def, "auth.json"), []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	// profiles/ exists → EnsureProfilesDir must be a no-op, never clobbering default.
	if err := EnsureProfilesDir(cfg, "codex"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(def, "auth.json"))
	if err != nil || string(got) != "real" {
		t.Errorf("existing default profile clobbered: %q (err %v)", got, err)
	}
}

func TestEnsureProfilesDirFreshVault(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	// No agent dir at all yet.
	if err := EnsureProfilesDir(cfg, "gemini"); err != nil {
		t.Fatal(err)
	}
	if !dirExists(filepath.Join(dir, "gemini", "profiles")) {
		t.Error("profiles/ not created for a fresh vault")
	}
	// Nothing to migrate → no default profile dir is fabricated.
	if dirExists(filepath.Join(dir, "gemini", "profiles", "default")) {
		t.Error("fresh vault should not create a default profile")
	}
}

func TestProfileAuthed(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}

	// Unknown agent, and a missing credential, both read as unauthed.
	if ProfileAuthed(cfg, "nope", "default") {
		t.Error("unknown agent should be unauthed")
	}
	if ProfileAuthed(cfg, "claude", "work") {
		t.Error("missing credential should be unauthed")
	}
	// A credential marker file in the profile dir → authed.
	work := cfg.AgentProfileDir("claude", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".credentials.json"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ProfileAuthed(cfg, "claude", "work") {
		t.Error("credential present should be authed")
	}
	// An API key in the env file authenticates every profile, even one with no cred file.
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=sk-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ProfileAuthed(cfg, "claude", "personal") {
		t.Error("env API key should authenticate any profile")
	}
}

func TestProfileRenewable(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	write := func(profile, body string) {
		d := cfg.AgentProfileDir("claude", profile)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, ".credentials.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A login with a refresh token renews on use → renewable, even with an expiry in the past.
	write("live", `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`)
	if !ProfileRenewable(cfg, "claude", "live") {
		t.Error("a refresh token present should read as renewable")
	}
	// No refresh token (empty or absent) → not renewable: a genuinely dead OAuth login.
	write("empty", `{"claudeAiOauth":{"accessToken":"a","refreshToken":"","expiresAt":1}}`)
	if ProfileRenewable(cfg, "claude", "empty") {
		t.Error("an empty refresh token should not be renewable")
	}
	write("none", `{"claudeAiOauth":{"accessToken":"a","expiresAt":1}}`)
	if ProfileRenewable(cfg, "claude", "none") {
		t.Error("a missing refresh token should not be renewable")
	}
	// Only claude exposes a readable OAuth credential; another agent / a missing file is false.
	if ProfileRenewable(cfg, "codex", "live") {
		t.Error("non-claude agents have no readable OAuth credential")
	}
	if ProfileRenewable(cfg, "claude", "ghost") {
		t.Error("a missing credential file is not renewable")
	}
}
