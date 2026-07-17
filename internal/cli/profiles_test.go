package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestCmdProfiles(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{ConfigDir: dir}
	// claude: a signed-in "work" profile (cred file present) + an unsigned "personal".
	work := cfg.AgentProfileDir("claude", "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	credential := `{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"]}}`
	if err := os.WriteFile(filepath.Join(work, ".credentials.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.AgentProfileDir("claude", "personal"), 0o700); err != nil {
		t.Fatal(err)
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := (&app{cfg: cfg}).cmdCredentials([]string{"claude"})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	if code != 0 || err != nil {
		t.Fatalf("cmdCredentials: code=%d err=%v", code, err)
	}
	for _, want := range []string{"work", "signed in", "personal", "not signed in"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// When the marked default points at a profile that no longer exists, the listing must surface it
// (otherwise an interactive run silently lands on nothing).
func TestCmdProfilesDanglingDefault(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if err := os.MkdirAll(cfg.AgentProfileDir("claude", "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetDefaultProfile("claude", "ghost"); err != nil { // ghost was never created / was removed
		t.Fatal(err)
	}
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	(&app{cfg: cfg}).cmdCredentials([]string{"claude"})
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "missing") || !strings.Contains(string(out), "ghost") {
		t.Errorf("expected a dangling-default note naming ghost:\n%s", out)
	}
}

// `coop profiles ls`/`list` steers to the bare listing (which already lists) instead of reading `ls`
// as an unknown agent (rule: `ls` is the list verb, it must lead somewhere useful).
func TestProfilesLsRedirect(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	code, err := a.cmdCredentials([]string{"ls"}) // v3: only `ls` redirects; `list` reads as an unknown agent
	if code != 2 || err == nil || !strings.Contains(err.Error(), "coop credentials") {
		t.Errorf("cmdCredentials([ls]) = (%d, %v), want (2, pointing at bare `coop credentials`)", code, err)
	}
}

func TestCmdProfilesUnknownAgent(t *testing.T) {
	a := &app{cfg: &config.Config{ConfigDir: t.TempDir()}}
	if code, err := a.cmdCredentials([]string{"nope"}); code != 2 || err == nil {
		t.Errorf("unknown agent: code=%d err=%v, want 2 + error", code, err)
	}
}

func TestCmdProfilesDefault(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	for _, p := range []string{"work", "personal"} {
		if err := os.MkdirAll(cfg.AgentProfileDir("claude", p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{cfg: cfg}

	for _, tc := range []struct {
		name string
		args []string
	}{ // path grammar: coop profiles <agent> <profile> default (verb-first is retired)
		{"unknown agent", []string{"nope", "work", "default"}},
		{"unknown profile", []string{"claude", "ghost", "default"}},
	} {
		if code, err := a.cmdCredentials(tc.args); code != 2 || err == nil {
			t.Errorf("%s: code=%d err=%v, want 2 + error", tc.name, code, err)
		}
	}

	// Set the default (discard the confirmation listing on stdout).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := a.cmdCredentials([]string{"claude", "personal", "default"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 || err != nil {
		t.Fatalf("set default: code=%d err=%v", code, err)
	}
	if got := cfg.DefaultProfileOf("claude"); got != "personal" {
		t.Errorf("default = %q, want personal", got)
	}
}

// profiles rm without --yes (non-TTY) refuses and keeps the profile — deleting one drops its login
// token + session history with no undo, so it can't happen unattended without an explicit opt-in.
func TestProfilesRemoveGate(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	for _, p := range []string{"personal", "work"} {
		if err := os.MkdirAll(cfg.AgentProfileDir("claude", p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	code, err := a.cmdCredentials([]string{"claude", "work", "rm"}) // path grammar (verb-first retired)
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("profiles rm without --yes = (%d, %v), want (2, a refusal naming --yes)", code, err)
	}
	if !pathExists(cfg.AgentProfileDir("claude", "work")) {
		t.Error("a refused profile rm must not delete the profile dir")
	}
}

func TestRemoveProfile(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	for _, p := range []string{"personal", "personal_backup", "default"} {
		if err := os.MkdirAll(cfg.AgentProfileDir("claude", p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}

	for _, tc := range []struct {
		name string
		args []string
	}{ // path grammar: coop profiles <agent> <profile> rm (verb-first is retired)
		{"unknown agent", []string{"nope", "default", "rm"}},
		{"unknown profile", []string{"claude", "ghost", "rm"}},
		{"refuses the marked default", []string{"claude", "personal", "rm"}},
	} {
		if code, err := a.cmdCredentials(tc.args); code != 2 || err == nil {
			t.Errorf("%s: code=%d err=%v, want 2 + error", tc.name, code, err)
		}
	}
	// personal (the default) must survive the refused deletion.
	if !pathExists(cfg.AgentProfileDir("claude", "personal")) {
		t.Fatal("refused deletion still removed the default profile dir")
	}

	// Remove the stray "default" profile (--yes skips the gate in this non-TTY test; discard the
	// confirmation listing on stdout).
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	code, err := a.cmdCredentials([]string{"claude", "default", "rm", "--yes"})
	_ = w.Close()
	os.Stdout = old
	_, _ = io.ReadAll(r)
	if code != 0 || err != nil {
		t.Fatalf("rm default: code=%d err=%v", code, err)
	}
	if pathExists(cfg.AgentProfileDir("claude", "default")) {
		t.Error("default profile dir was not removed")
	}
	if !pathExists(cfg.AgentProfileDir("claude", "personal")) {
		t.Error("removing default wrongly affected personal")
	}
}

// TestProfileStateRenewable: an expired access token with a refresh token reads as signed in
// (claude renews it on use), not "token expired" — the false alarm that read as blocked. Only an
// expired token with no refresh token, a genuinely dead OAuth login, needs a re-login.
func TestProfileStateRenewable(t *testing.T) {
	dir := t.TempDir()
	a := &app{cfg: &config.Config{ConfigDir: dir}}
	write := func(profile, body string) {
		d := a.cfg.AgentProfileDir("claude", profile)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, ".credentials.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Expired access token (expiresAt in the past) but a refresh token present → signed in.
	write("renew", `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1,"scopes":["user:inference"]}}`)
	if label, expired := a.profileState("claude", "renew"); expired || label != "signed in" {
		t.Errorf("expired-but-renewable = (%q, %v), want (\"signed in\", false)", label, expired)
	}
	// Expired access token, no refresh token → genuinely needs a re-login.
	write("dead", `{"claudeAiOauth":{"accessToken":"a","expiresAt":1,"scopes":["user:inference"]}}`)
	if label, expired := a.profileState("claude", "dead"); !expired || label != "re-login required" {
		t.Errorf("expired-no-refresh = (%q, %v), want (\"re-login required\", true)", label, expired)
	}
	// A projected/stripped OAuth marker has no token or refresh authority. Presence alone must not
	// claim it is signed in, or live verification sends the operator toward the wrong diagnosis.
	write("stripped", `{"claudeAiOauth":{"expiresAt":0,"scopes":["user:inference"]}}`)
	if label, expired := a.profileState("claude", "stripped"); !expired || label != "re-login required" {
		t.Errorf("stripped = (%q, %v), want (\"re-login required\", true)", label, expired)
	}
	write("future-no-access", `{"claudeAiOauth":{"expiresAt":4102444800000,"scopes":["user:inference"]}}`)
	if label, expired := a.profileState("claude", "future-no-access"); !expired || label != "re-login required" {
		t.Errorf("future-no-access = (%q, %v), want (\"re-login required\", true)", label, expired)
	}
	write("malformed", `{`)
	if label, expired := a.profileState("claude", "malformed"); !expired || label != "re-login required" {
		t.Errorf("malformed = (%q, %v), want (\"re-login required\", true)", label, expired)
	}
	// No credential at all → not signed in.
	if label, _ := a.profileState("claude", "ghost"); label != "not signed in" {
		t.Errorf("missing credential = %q, want \"not signed in\"", label)
	}
}

func TestProfileStateEnvOnlyDoesNotRequireRelogin(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := cfg.DefaultProfileOf("claude")
	if label, needsLogin := (&app{cfg: cfg}).profileState("claude", profile); needsLogin || label != "signed in" {
		t.Errorf("env-only Claude = (%q, %v), want (\"signed in\", false)", label, needsLogin)
	}
}

func TestProfileStateExpiredGrok(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	dir := cfg.AgentProfileDir("grok", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := grokProfileCredential("2020-01-01T00:00:00Z", "")
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if label, expired := (&app{cfg: cfg}).profileState("grok", "default"); !expired || label != "re-login required" {
		t.Errorf("expired Grok = (%q, %v), want (\"re-login required\", true)", label, expired)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(grokProfileCredential("2020-01-01T00:00:00Z", "refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	if label, expired := (&app{cfg: cfg}).profileState("grok", "default"); expired || label != "signed in" {
		t.Errorf("refreshable Grok = (%q, %v), want (\"signed in\", false)", label, expired)
	}
}

func TestCredentialOutputOffersReloginForInvalidMarker(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	dir := cfg.AgentProfileDir("claude", "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: cfg}
	for name, render := range map[string]func(){
		"listing": func() { _, _ = a.cmdCredentials([]string{"claude"}) },
		"detail":  func() { _, _ = a.showProfile("claude", "work") },
	} {
		out := captureStdout(t, render)
		if !strings.Contains(out, "re-login required") || strings.Count(out, "coop login claude@work") != 1 {
			t.Errorf("%s did not show one exact re-login remedy:\n%s", name, out)
		}
	}
}

func grokProfileCredential(expiresAt, refresh string) string {
	return fmt.Sprintf(`{"issuer::id":{"key":"access","refresh_token":%q,"expires_at":%q,"auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2026-07-16T01:00:00Z"}}`, refresh, expiresAt)
}
