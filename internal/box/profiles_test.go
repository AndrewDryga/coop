package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

func TestEnsureProfilesDirPreservesExisting(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	// A vault already in the profiles/ layout with a real default credential.
	def := filepath.Join(dir, "codex", "profiles", "default")
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, filepath.Join(dir, "codex"), filepath.Join(dir, "codex", "profiles")} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
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
	for _, path := range []string{dir, filepath.Join(dir, "codex"), filepath.Join(dir, "codex", "profiles")} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("%s was not tightened to 0700: info=%v err=%v", path, info, err)
		}
	}
	if info, err := os.Stat(filepath.Join(def, "auth.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("provider-owned credential mode changed: info=%v err=%v", info, err)
	}
}

func TestEnsureProfilesDirRefusesUnsafeRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "agents")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProfilesDir(&config.Config{ConfigDir: root}, "codex"); err == nil {
		t.Fatal("symlinked credential root was accepted")
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
	// A saved sign-in → authed.
	seedCanonicalFixture(t, cfg, "claude", "work")
	if !ProfileAuthed(cfg, "claude", "work") {
		t.Error("credential present should be authed")
	}
	// A login only builds before v11 saved is not read: it needs a new sign-in and is left alone.
	old := cfg.AgentProfileDir("claude", "old")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	oldMarker := filepath.Join(old, ".credentials.json")
	if err := os.WriteFile(oldMarker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ProfileAuthed(cfg, "claude", "old") || ProfileCredentialReady(cfg, "claude", "old", time.Now()) {
		t.Error("a pre-v11 login was read as a sign-in")
	}
	if got := ProfileSignIn(cfg, "claude", "old", time.Now()); got != agents.SignInAgain {
		t.Errorf("pre-v11 login needs = %v, want a new sign-in", got)
	}
	if data, err := os.ReadFile(oldMarker); err != nil || string(data) != "tok" {
		t.Errorf("the pre-v11 login was changed: %q, %v", data, err)
	}
	// An API key in the env file occupies only the default profile slot.
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=sk-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ProfileAuthed(cfg, "claude", "default") {
		t.Error("env API key should authenticate the default profile")
	}
	if ProfileAuthed(cfg, "claude", "personal") {
		t.Error("env API key must not authenticate an unrelated profile")
	}
}

func TestProfileAuthedCredentialMatrix(t *testing.T) {
	for _, name := range agents.Names() {
		ag, _ := agents.Get(name)
		sources := append([]string{"file"}, ag.CredentialEnvKeys()...)
		for _, source := range sources {
			t.Run(name+"/"+source, func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir()}
				profile := "work"
				if source == "file" {
					seedCanonicalFixture(t, cfg, name, profile)
				} else {
					if err := os.WriteFile(cfg.EnvFile(), []byte(source+"=token\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := cfg.SetDefaultProfile(name, profile); err != nil {
						t.Fatal(err)
					}
				}
				if !ProfileAuthed(cfg, name, profile) {
					t.Errorf("ProfileAuthed(%s, %s via %s) = false", name, profile, source)
				}
				if got, err := EffectiveProfiles(cfg, name); err != nil || !slices.Contains(got, profile) || len(got) != 1 {
					t.Errorf("EffectiveProfiles(%s via %s) = %v, %v; want exactly [%s]", name, source, got, err, profile)
				}
				if source != "file" && ProfileAuthed(cfg, name, "personal") {
					t.Errorf("env credential %s authenticated unrelated %s@personal", source, name)
				}
			})
		}
	}
}

// A credential store that cannot be listed is an error, never silently fewer accounts; the
// accounts the other store lists still come back.
func TestEffectiveProfilesReportListingErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	for _, store := range []string{"profiles", "credentials"} {
		t.Run(store, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			for _, dir := range []string{cfg.AgentProfileDir("claude", "work"), filepath.Join(cfg.ConfigDir, "claude", "credentials", "solo")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := EffectiveProfiles(cfg, "claude"); err != nil || len(got) != 2 {
				t.Fatalf("EffectiveProfiles = %v, %v; want work and solo", got, err)
			}
			locked := filepath.Join(cfg.ConfigDir, "claude", store)
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
			if got, err := EffectiveProfiles(cfg, "claude"); err == nil || len(got) != 1 {
				t.Fatalf("EffectiveProfiles with %s unreadable = %v, %v; want the other account and an error", store, got, err)
			}
		})
	}
}

func TestProfileCredentialReady(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	now := time.Now()
	if ProfileCredentialReady(cfg, "claude", "missing", now) {
		t.Fatal("missing credential was runnable")
	}
	seedCanonicalFixture(t, cfg, "claude", "ready")
	if !ProfileCredentialReady(cfg, "claude", "ready", now) {
		t.Fatal("saved Claude sign-in was not runnable")
	}
	// Markers of builds before v11 are never read, whatever they hold.
	for _, old := range []struct{ agent, profile, marker, body string }{
		{"claude", "dead", ".credentials.json", `{"claudeAiOauth":{"scopes":["user:inference"]}}`},
		{"claude", "refreshable", ".credentials.json", `{"claudeAiOauth":{"refreshToken":"refresh","scopes":["user:inference"]}}`},
		{"gemini", "opaque", "gemini-credentials.json", `{"encrypted":"opaque"}`},
	} {
		dir := cfg.AgentProfileDir(old.agent, old.profile)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, old.marker), []byte(old.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if ProfileCredentialReady(cfg, old.agent, old.profile, now) {
			t.Fatalf("%s@%s: a pre-v11 marker was runnable", old.agent, old.profile)
		}
	}
	gemini, _ := agents.Get("gemini")
	if err := SaveHostCredential(cfg, gemini, "portable", []byte("portable-key")); err != nil {
		t.Fatal(err)
	}
	if !ProfileCredentialReady(cfg, "gemini", "portable", now) {
		t.Fatal("Coop-owned Gemini key was not runnable")
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ProfileCredentialReady(cfg, "claude", "default", now) {
		t.Fatal("env-backed credential lost presence-based behavior")
	}
}

func TestGeminiProfileAuthFollowsSelectedAuthority(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		selected   string
		env        string
		withMarker bool
		want       bool
	}{
		{name: "Gemini key selected and present", profile: "default", selected: "gemini-api-key", env: "GEMINI_API_KEY=token\n", want: true},
		{name: "Gemini native key selected and marker present", profile: "default", selected: "gemini-api-key", env: "GOOGLE_API_KEY=token\n", withMarker: true},
		{name: "Gemini key selected without marker or key", profile: "default", selected: "gemini-api-key", env: "GOOGLE_API_KEY=token\n"},
		{name: "Vertex key selected and present", profile: "default", selected: "vertex-ai", env: "GOOGLE_API_KEY=token\n", want: true},
		{name: "Vertex key selected but only Gemini key present", profile: "default", selected: "vertex-ai", env: "GEMINI_API_KEY=token\n", withMarker: true},
		{name: "OAuth marker of a pre-v11 login is not read", profile: "default", selected: "oauth-personal", env: "GEMINI_API_KEY=token\nGOOGLE_API_KEY=token\n", withMarker: true},
		{name: "OAuth selected without marker", profile: "default", selected: "oauth-personal", env: "GEMINI_API_KEY=token\nGOOGLE_API_KEY=token\n"},
		{name: "named account rejects host-bound native Gemini key", profile: "work", selected: "gemini-api-key", withMarker: true},
		{name: "named account cannot use provider env", profile: "work", selected: "gemini-api-key", env: "GEMINI_API_KEY=token\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir()}
			profileDir := cfg.AgentProfileDir("gemini", tt.profile)
			if err := os.MkdirAll(profileDir, 0o700); err != nil {
				t.Fatal(err)
			}
			settings := `{"security":{"auth":{"selectedType":"` + tt.selected + `"}}}`
			if err := os.WriteFile(filepath.Join(profileDir, "settings.json"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.withMarker {
				if err := os.WriteFile(filepath.Join(profileDir, "gemini-credentials.json"), []byte(`{"encrypted":"stale"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(cfg.EnvFile(), []byte(tt.env), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := ProfileAuthed(cfg, "gemini", tt.profile); got != tt.want {
				t.Errorf("ProfileAuthed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProfileTokenMtime(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}

	// Missing account → not knowable (rendered as "—" by the caller), never an error.
	if _, ok := ProfileTokenMtime(cfg, "claude", "ghost"); ok {
		t.Error("a missing credential file should report ok=false")
	}
	// Unknown agent → not knowable.
	if _, ok := ProfileTokenMtime(cfg, "nope", "default"); ok {
		t.Error("an unknown agent should report ok=false")
	}
	// A pre-v11 marker is not read, so it has no rotation clock.
	work := cfg.AgentProfileDir("claude", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".credentials.json"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ProfileTokenMtime(cfg, "claude", "work"); ok {
		t.Error("a pre-v11 marker supplied a rotation time")
	}
	// A saved sign-in is dated by its authority file, not by anything beside it.
	seedCanonicalFixture(t, cfg, "claude", "saved")
	authority := filepath.Join(cfg.ConfigDir, "claude", "credentials", "saved", "authority.json")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(authority, old, old); err != nil {
		t.Fatal(err)
	}
	got, ok := ProfileTokenMtime(cfg, "claude", "saved")
	if !ok || got.Unix() != old.Unix() {
		t.Fatalf("mtime = %v, %v; want %v", got, ok, old)
	}
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "claude", "credentials", "saved", "unrelated.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := ProfileTokenMtime(cfg, "claude", "saved"); got.Unix() != old.Unix() {
		t.Errorf("mtime moved to %v after an unrelated write", got)
	}
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func TestProfileTokenMtimeUsesSelectedNativeAuthorityRoot(t *testing.T) {
	pinHome(t)
	shared := &config.Config{ConfigDir: t.TempDir()}
	agent, _ := agents.Get("gemini")
	if err := SaveHostCredential(shared, agent, "work", []byte("fixture-selected-key")); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	authority := filepath.Join(shared.ConfigDir, "gemini", "credentials", "work", "authority.json")
	if err := os.Chtimes(authority, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	private, err := (&config.Config{ConfigDir: t.TempDir()}).WithNativeAuthorityRoot(shared.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ProfileTokenMtime(private, "gemini", "work")
	if !ok || !got.Equal(stamp) {
		t.Fatalf("selected authority mtime: %v %v, want %v", got, ok, stamp)
	}
}
