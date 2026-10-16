package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
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
	for _, want := range []string{"Accounts available to Coop", "work", "refreshed", "personal", "⚠ Not signed in", "coop login claude@personal"} {
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
	if !strings.Contains(string(out), `⚠ Default account "ghost" is missing`) || !strings.Contains(string(out), "coop credentials claude work default") {
		t.Errorf("expected a dangling-default note naming ghost and its remedy:\n%s", out)
	}
}

// A failed listing may hide accounts, so the listing says so with its cause instead of offering a
// sign-in for an empty block or calling a default it could not see missing. Other agents stay whole.
func TestCredentialsListingSaysWhenAccountsCannotBeListed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	cfg := freshConfig(t)
	// Claude lists work, but its default, solo, is in a credentials folder that can be searched and
	// not listed. Codex's only account is in such a profiles folder.
	writeAccount(t, cfg, "claude", "work", claudeReadyCredential)
	claudeCredentials := filepath.Join(cfg.ConfigDir, "claude", "credentials")
	if err := os.MkdirAll(filepath.Join(claudeCredentials, "solo"), 0o700); err != nil {
		t.Fatal(err)
	}
	setDefault(t, cfg, "claude", "solo")
	writeAccount(t, cfg, "codex", "work", codexReadyCredential)
	codexProfiles := filepath.Dir(cfg.AgentProfileDir("codex", "work"))
	for _, dir := range []string{claudeCredentials, codexProfiles} {
		if err := os.Chmod(dir, 0o100); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	}
	out := captureStdout(t, func() {
		if code, err := (&app{cfg: cfg}).cmdCredentials(nil); code != 0 || err != nil {
			t.Fatalf("cmdCredentials = (%d, %v)", code, err)
		}
	})
	_, rest, _ := strings.Cut(out, "\nClaude\n")
	claude, rest, _ := strings.Cut(rest, "\nCodex\n")
	codex, rest, _ := strings.Cut(rest, "\nGemini\n")
	gemini, _, _ := strings.Cut(rest, "\nGrok\n")
	if want := "    work   refreshed just now\n\n  ⚠ Could not list every Claude account\n    open " + claudeCredentials + ": permission denied\n"; claude != want {
		t.Errorf("Claude block = %q, want %q", claude, want)
	}
	if want := "  ⚠ Could not list every Codex account\n    open " + codexProfiles + ": permission denied\n"; codex != want {
		t.Errorf("Codex block = %q, want %q", codex, want)
	}
	if want := "  No accounts. Sign in: coop login gemini\n"; gemini != want {
		t.Errorf("one agent's failed listing changed another's block: Gemini block = %q", gemini)
	}
}

// Every command that names an account refuses one a failed listing may hide, as before, but says
// the accounts could not be listed, with the cause, instead of advising a sign-in that would not
// help, and exits 1: the command could not do its job, the input was fine. An account that is
// simply missing keeps its exit code, and an account the listing still returned stays usable.
func TestAccountRefusalsNameAFailedListing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory regardless of its mode")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := freshConfig(t)
	if err := os.Mkdir(filepath.Join(cfg.RepoOverride, ".git"), 0o755); err != nil { // eval resolves the repository
		t.Fatal(err)
	}
	writeAccount(t, cfg, "claude", "work", claudeReadyCredential)
	credentials := filepath.Join(cfg.ConfigDir, "claude", "credentials")
	if err := os.MkdirAll(filepath.Join(credentials, "solo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(credentials, 0o100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(credentials, 0o700) })
	// A preset whose lead pins each account: its refusal arrives wrapped in the preset's own words.
	for _, account := range []string{"solo", "ghost"} {
		writePresetFile(t, cfg.RepoOverride, "lead-"+account, "lead:\n  agent: [claude@"+account+"]\n")
	}
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	a := &app{cfg: cfg, rt: recordingRuntime(t, recorder), rtSet: true}
	commands := []struct {
		name    string
		missing int // the exit code for an account that does not exist
		refuse  func(account string) (int, error)
	}{
		{"credential", 2, func(account string) (int, error) { return a.cmdCredentials([]string{"claude", account}) }},
		{"default", 2, func(account string) (int, error) { return a.cmdCredentials([]string{"claude", account, "default"}) }},
		{"remove", 2, func(account string) (int, error) { return a.cmdCredentials([]string{"claude", account, "rm"}) }},
		{"launch", 2, func(account string) (int, error) { return a.launchAgent("claude@"+account, nil) }},
		{"preset", 2, func(account string) (int, error) {
			p, err := a.loadRunPreset("lead-" + account)
			if err != nil {
				return 0, err
			}
			return a.launchPreset(p, nil)
		}},
		{"acp", 2, func(account string) (int, error) { return a.cmdACP([]string{"claude@" + account}) }},
		{"fork", 2, func(account string) (int, error) { return a.forkCreate([]string{"scratchfork", "claude@" + account}) }},
		{"eval", 1, func(account string) (int, error) {
			return a.evalRun([]string{"core", "claude@" + account, "--timeout", "35m"})
		}},
	}
	// Each refusal is checked as the block a person sees, which a wrapped one prints unwrapped.
	want := "Could not list every Claude account — open " + credentials + ": permission denied"
	for _, tc := range commands {
		t.Run(tc.name, func(t *testing.T) {
			if code, err := tc.refuse("solo"); code != 1 || asUsage(err) == nil || asUsage(err).Error() != want {
				t.Fatalf("refusal = (%d, %v), want (1, %q)", code, err, want)
			}
		})
	}
	if pathExists(recorder) || cfg.DefaultProfileOf("claude") == "solo" || !pathExists(filepath.Join(credentials, "solo")) {
		t.Fatal("an account a failed listing hides was used or removed")
	}
	if err := a.selectRunProfile("claude", "work"); err != nil {
		t.Fatalf("an account the listing returned was refused: %v", err)
	}
	if err := os.Chmod(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range commands {
		t.Run(tc.name+"-missing", func(t *testing.T) {
			if code, err := tc.refuse("ghost"); code != tc.missing || asUsage(err) == nil || !strings.HasPrefix(asUsage(err).Error(), `No Claude account named "ghost"`) {
				t.Fatalf("missing account = (%d, %v), want (%d, the missing-account refusal)", code, err, tc.missing)
			}
		})
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

func TestRemoveGeminiProfileAlsoRemovesHostCredential(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	gemini, _ := agents.Get("gemini")
	if err := box.SaveHostCredential(cfg, gemini, "personal", []byte("personal-key")); err != nil {
		t.Fatal(err)
	}
	if err := box.SaveHostCredential(cfg, gemini, "work", []byte("work-key")); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetDefaultProfile("gemini", "personal"); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		if code, err := (&app{cfg: cfg}).removeProfile("gemini", "work", true); code != 0 || err != nil {
			t.Fatalf("remove Gemini work profile = (%d, %v)", code, err)
		}
	})
	if _, _, found, err := box.LoadHostCredential(cfg, gemini, "work"); err != nil || found {
		t.Fatalf("removed Gemini key = (%v, %v)", found, err)
	}
	if _, value, found, err := box.LoadHostCredential(cfg, gemini, "personal"); err != nil || !found || value != "personal-key" {
		t.Fatalf("default Gemini key changed = (%q, %v, %v)", value, found, err)
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
	if issue := a.profileIssue("claude", "renew"); issue != "" {
		t.Errorf("expired-but-renewable = %q, want no issue", issue)
	}
	// Expired access token, no refresh token → genuinely needs a re-login.
	write("dead", `{"claudeAiOauth":{"accessToken":"a","expiresAt":1,"scopes":["user:inference"]}}`)
	if issue := a.profileIssue("claude", "dead"); issue != "Sign in again" {
		t.Errorf("expired-no-refresh = %q, want \"Sign in again\"", issue)
	}
	// A projected/stripped OAuth marker has no token or refresh authority. Presence alone must not
	// claim it is signed in, or live verification sends the operator toward the wrong diagnosis.
	write("stripped", `{"claudeAiOauth":{"expiresAt":0,"scopes":["user:inference"]}}`)
	if issue := a.profileIssue("claude", "stripped"); issue != "Sign in again" {
		t.Errorf("stripped = %q, want \"Sign in again\"", issue)
	}
	write("future-no-access", `{"claudeAiOauth":{"expiresAt":4102444800000,"scopes":["user:inference"]}}`)
	if issue := a.profileIssue("claude", "future-no-access"); issue != "Sign in again" {
		t.Errorf("future-no-access = %q, want \"Sign in again\"", issue)
	}
	write("malformed", `{`)
	if issue := a.profileIssue("claude", "malformed"); issue != "Sign in again" {
		t.Errorf("malformed = %q, want \"Sign in again\"", issue)
	}
	// No credential at all → not signed in.
	if issue := a.profileIssue("claude", "ghost"); issue != "Not signed in" {
		t.Errorf("missing credential = %q, want \"Not signed in\"", issue)
	}
}

func TestProfileStateEnvOnlyDoesNotRequireRelogin(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := cfg.DefaultProfileOf("claude")
	if issue := (&app{cfg: cfg}).profileIssue("claude", profile); issue != "" {
		t.Errorf("env-only Claude = %q, want no issue", issue)
	}
}

func TestGeminiNamedNativeAPIKeyRequiresPortableLogin(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	dir := cfg.AgentProfileDir("gemini", "personal2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gemini-credentials.json"), []byte("opaque-native-store"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &app{cfg: cfg}
	if issue := a.profileIssue("gemini", "personal2"); issue != "Sign in again" {
		t.Fatalf("native stored Gemini API key issue = %q, want Sign in again", issue)
	}
	out := captureStdout(t, func() {
		if code, err := a.cmdCredentials([]string{"gemini"}); code != 0 || err != nil {
			t.Fatalf("cmdCredentials = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(out, "personal2") || !strings.Contains(out, "Sign in again") || !strings.Contains(out, "coop login gemini@personal2") {
		t.Fatalf("host-bound Gemini API key did not ask for portable login:\n%s", out)
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
	if issue := (&app{cfg: cfg}).profileIssue("grok", "default"); issue != "Sign in again" {
		t.Errorf("expired Grok = %q, want \"Sign in again\"", issue)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(grokProfileCredential("2020-01-01T00:00:00Z", "refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	if issue := (&app{cfg: cfg}).profileIssue("grok", "default"); issue != "" {
		t.Errorf("refreshable Grok = %q, want no issue", issue)
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
		if !strings.Contains(out, "Sign in again") || strings.Count(out, "coop login claude@work") != 1 {
			t.Errorf("%s did not show one exact re-login remedy:\n%s", name, out)
		}
	}
}

func grokProfileCredential(expiresAt, refresh string) string {
	return fmt.Sprintf(`{"issuer::id":{"key":"access","refresh_token":%q,"expires_at":%q,"auth_mode":"oauth","oidc_issuer":"issuer","oidc_client_id":"client","principal_id":"principal","principal_type":"user","user_id":"user","team_id":"team","create_time":"2026-07-16T01:00:00Z"}}`, refresh, expiresAt)
}

// The listing is a human overview, not a status ledger: being listed says an account is usable,
// so a healthy row carries no status word — only when it was last refreshed. The default is
// marked twice on purpose (a leading accent and an explicit `default`), and every `default`
// starts at the same column across ALL agent blocks, so the eye can run down it.
func TestCredentialsListingIsAnOverviewNotALedger(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	for _, p := range []string{"personal", "backup"} {
		if err := os.MkdirAll(cfg.AgentProfileDir("claude", p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.AgentProfileDir("claude", p), ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"x","refreshToken":"r","scopes":["user:inference"]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A codex account with nothing in it: the one row that must say what is wrong and how to fix it.
	if err := os.MkdirAll(cfg.AgentProfileDir("codex", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SetDefaultProfile("claude", "personal"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if code, err := (&app{cfg: cfg}).cmdCredentials(nil); code != 0 || err != nil {
			t.Fatalf("cmdCredentials = (%d, %v)", code, err)
		}
	})
	if !strings.HasPrefix(out, "Accounts available to Coop\n") {
		t.Errorf("listing does not open with its title:\n%s", out)
	}
	for _, unwanted := range []string{"rotated ", "(default)", "✓"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("healthy listing still carries %q:\n%s", unwanted, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "signed in") && !strings.Contains(line, "Not signed in") {
			t.Errorf("a healthy row still claims a status:\n%s", out)
		}
	}
	var defaults, issue, remedy int
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, "default") && strings.Contains(line, "*") {
			defaults++
			if got := strings.Index(line, "default"); got != strings.Index(firstDefaultLine(out), "default") {
				t.Errorf("`default` starts at a different column than the first block's:\n%s", out)
			}
		}
		if strings.Contains(line, "Not signed in") {
			issue++
		}
		if strings.TrimSpace(line) == "coop login codex@empty" {
			remedy++
		}
	}
	if defaults == 0 || issue != 1 || remedy != 1 {
		t.Errorf("defaults=%d issue=%d remedy=%d in:\n%s", defaults, issue, remedy, out)
	}
	if strings.Contains(out, "  \n") || strings.Contains(out, " \n") {
		t.Errorf("a row ends in trailing whitespace:\n%q", out)
	}
}

// firstDefaultLine is the first row carrying the `default` tag, for the column comparison above.
func firstDefaultLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, "default") && strings.Contains(line, "*") {
			return line
		}
	}
	return ""
}

// The human durations are the ones a person says out loud; the compressed form stays where a
// column is scanned for drift.
func TestHumanAgeReadsAloud(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-90 * time.Second), "a minute ago"},
		{now.Add(-7 * time.Minute), "7 minutes ago"},
		{now.Add(-100 * time.Minute), "an hour ago"},
		{now.Add(-7 * time.Hour), "7 hours ago"},
		{now.Add(-30 * time.Hour), "yesterday"},
		{now.Add(-19 * 24 * time.Hour), "19 days ago"},
	} {
		if got := humanAge(tc.at); got != tc.want {
			t.Errorf("humanAge(%s) = %q, want %q", tc.at.Format(time.RFC3339), got, tc.want)
		}
	}
}
