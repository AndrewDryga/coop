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
		marker, _ := ag.AuthMarker()
		sources := append([]string{"file"}, ag.CredentialEnvKeys()...)
		for _, source := range sources {
			t.Run(name+"/"+source, func(t *testing.T) {
				cfg := &config.Config{ConfigDir: t.TempDir()}
				profile := "work"
				if source == "file" {
					dir := cfg.AgentProfileDir(name, profile)
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, marker), []byte("token"), 0o600); err != nil {
						t.Fatal(err)
					}
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
				if got := EffectiveProfiles(cfg, name); !slices.Contains(got, profile) || len(got) != 1 {
					t.Errorf("EffectiveProfiles(%s via %s) = %v, want exactly [%s]", name, source, got, profile)
				}
				if source != "file" && ProfileAuthed(cfg, name, "personal") {
					t.Errorf("env credential %s authenticated unrelated %s@personal", source, name)
				}
			})
		}
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
		{name: "Gemini key selected but only Vertex key present", profile: "default", selected: "gemini-api-key", env: "GOOGLE_API_KEY=token\n", withMarker: true},
		{name: "Vertex key selected and present", profile: "default", selected: "vertex-ai", env: "GOOGLE_API_KEY=token\n", want: true},
		{name: "Vertex key selected but only Gemini key present", profile: "default", selected: "vertex-ai", env: "GEMINI_API_KEY=token\n", withMarker: true},
		{name: "OAuth marker selected and present", profile: "default", selected: "oauth-personal", env: "GEMINI_API_KEY=token\nGOOGLE_API_KEY=token\n", withMarker: true, want: true},
		{name: "OAuth selected without marker", profile: "default", selected: "oauth-personal", env: "GEMINI_API_KEY=token\nGOOGLE_API_KEY=token\n"},
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

	// Missing marker → not knowable (rendered as "—" by the caller), never an error.
	if _, ok := ProfileTokenMtime(cfg, "claude", "ghost"); ok {
		t.Error("a missing credential file should report ok=false")
	}
	// Unknown agent → not knowable.
	if _, ok := ProfileTokenMtime(cfg, "nope", "default"); ok {
		t.Error("an unknown agent should report ok=false")
	}
	// A marker file present → its mtime is the rotation clock. Set it to a known past instant.
	work := cfg.AgentProfileDir("claude", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(work, ".credentials.json")
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	got, ok := ProfileTokenMtime(cfg, "claude", "work")
	if !ok {
		t.Fatal("a present marker should report ok=true")
	}
	if got.Unix() != old.Unix() {
		t.Errorf("mtime = %v, want %v", got, old)
	}
	// It stats the AuthMarker specifically, NOT the newest file in the dir — a later session
	// write must not masquerade as a rotation.
	if err := os.WriteFile(filepath.Join(work, "session.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := ProfileTokenMtime(cfg, "claude", "work"); got.Unix() != old.Unix() {
		t.Errorf("mtime moved to %v after an unrelated write; must track only the marker", got)
	}
}
