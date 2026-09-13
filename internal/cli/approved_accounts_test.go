package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/ui"
)

// The accounts, presets and models family against its approved transcripts. Every fixture in
// testdata/approved is the exact bytes of one invocation; a mismatch here means the renderer
// drifted from a design decision, not that the test needs fixing (see that folder's README).

// TestApprovedAccountPages pins every help page this family owns, including the four generated
// provider pages — an agent's page is built from its adapter, so a new agent gets these bytes free.
func TestApprovedAccountPages(t *testing.T) {
	for _, tc := range []struct{ fixture, key string }{
		{"04-login-help", "login"},
		{"06-credentials-help", "credentials"},
		{"06-credentials-default-help", "credentials default"},
		{"06-credentials-rm-help", "credentials rm"},
		{"06-credentials-account-help", "credentials account"},
		{"12-models-help", "models"},
		{"10-presets-init-help", "presets init"},
		{"09-presets-help", "presets"},
	} {
		t.Run(tc.fixture, func(t *testing.T) { assertApprovedPage(t, tc.fixture, commandHelp[tc.key]) })
	}
	for _, agent := range agents.Names() {
		t.Run("agent-"+agent+"-help", func(t *testing.T) {
			assertApprovedPage(t, "agent-"+agent+"-help", agentHelp(agent))
		})
	}
}

// TestApprovedAccountInputErrors drives each rejected target, account and preset through its REAL
// parser and pins the shared refusal block it renders. Nothing here may start a browser, a login,
// a deletion or a model fetch: every case returns before any of that.
func TestApprovedAccountInputErrors(t *testing.T) {
	newApp := func(t *testing.T) *app { return &app{cfg: freshConfig(t), argv: []string{"claude"}} }
	launch := func(target string) func(*testing.T) error {
		return func(t *testing.T) error {
			code, err := newApp(t).launchAgent(target, nil)
			return codeErr(t, code, err)
		}
	}
	for _, tc := range []struct {
		fixture string
		run     func(*testing.T) error
	}{
		{"03i-unknown-agent-suggestion", func(t *testing.T) error {
			code, err := newApp(t).cmdModels([]string{"claud"})
			return codeErr(t, code, err)
		}},
		{"03i-unknown-agent-no-suggestion", func(t *testing.T) error {
			code, err := newApp(t).cmdModels([]string{"cursor"})
			return codeErr(t, code, err)
		}},
		{"03i-empty-model", launch("claude:")},
		{"03i-invalid-model", launch("claude:opus:extra")},
		{"03i-empty-effort", launch("codex/")},
		{"03i-invalid-effort", launch("codex/HIGH")},
		{"03i-effort-unsupported", launch("gemini/high")},
		{"03i-empty-account", launch("claude@")},
		{"03i-invalid-account", launch("claude@../work")},
		{"03i-repeated-account-separator", launch("claude@work@personal")},
		{"03i-account-list-outside-loop", launch("claude@work,personal")},
		{"03i-peer-account", func(*testing.T) error {
			_, err := resolvePeerTargets("coop claude", []string{"codex@work"}, []string{"claude", "codex"})
			return err
		}},
		{"03i-unknown-account", func(t *testing.T) error {
			return newApp(t).selectRunProfile("codex", "work")
		}},
		{"03i-provider-needs-account", func(*testing.T) error {
			_, err := resolvePeerTargets("coop claude", []string{"codex"}, []string{"claude"})
			return err
		}},
		{"03i-unknown-preset", func(t *testing.T) error {
			a := newApp(t)
			code, err := a.showPreset(a.cfg.RepoOverride, "frontire")
			return codeErr(t, code, err)
		}},
		{"03i-preset-load-failed", func(t *testing.T) error {
			a := newApp(t)
			writePresetFile(t, a.cfg.RepoOverride, "review",
				"lead: {agent: codex}\nroles: {critic: {mode: readonly, agent: claude}}\n")
			code, err := a.showPreset(a.cfg.RepoOverride, "review")
			return codeErr(t, code, err)
		}},
		{"05-login-needs-terminal", func(t *testing.T) error {
			code, err := newApp(t).loginTo("claude", "work")
			return codeErr(t, code, err)
		}},
		{"05-login-takes-no-model", func(t *testing.T) error {
			code, err := newApp(t).cmdLogin([]string{"claude:opus"})
			return codeErr(t, code, err)
		}},
		{"08-remove-default-refused", func(t *testing.T) error {
			cfg := freshConfig(t)
			writeAccount(t, cfg, "codex", "personal", codexReadyCredential)
			setDefault(t, cfg, "codex", "personal")
			code, err := (&app{cfg: cfg}).cmdCredentials([]string{"codex", "personal", "rm"})
			return codeErr(t, code, err)
		}},
		{"10-presets-init-exists", func(t *testing.T) error {
			a := newApp(t)
			if _, err := preset.Scaffold(a.cfg.RepoOverride, "frontier"); err != nil {
				t.Fatal(err)
			}
			code, err := a.presetsInit(a.cfg.RepoOverride, nil)
			return codeErr(t, code, err)
		}},
		{"10-presets-init-incomplete", func(t *testing.T) error {
			a := newApp(t)
			if err := os.MkdirAll(filepath.Join(a.cfg.RepoOverride, ".agent", "presets", "frontier"), 0o755); err != nil {
				t.Fatal(err)
			}
			code, err := a.presetsInit(a.cfg.RepoOverride, nil)
			return codeErr(t, code, err)
		}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("invocation was accepted; the approved refusal is untested")
			}
			assertApprovedOutput(t, tc.fixture, usageBlock(t, err))
		})
	}
}

// The unauthenticated heads-up is a WARNING, not a refusal: the launch carries on, and only a run
// that truly needs an account refuses (03i-provider-needs-account above).
func TestApprovedUnauthenticatedNudge(t *testing.T) {
	out := captureStderr(t, func() {
		warnRows("Codex is not signed in", [2]string{"Sign in:", "coop login codex"})
	})
	assertApprovedOutput(t, "03i-unauthenticated-nudge", out)
	// It is TTY-only: a piped run (this test) must print nothing at all.
	quiet := captureStderr(t, func() { (&app{cfg: freshConfig(t)}).nudgeIfUnauthed("codex") })
	if quiet != "" {
		t.Errorf("the nudge must stay off a non-terminal run, got %q", quiet)
	}
}

// TestApprovedLoginStates pins what coop says around the provider's own sign-in screen: the
// handoff before it, and each outcome after it. The provider's output between them is its own.
func TestApprovedLoginStates(t *testing.T) {
	assertApprovedOutput(t, "05-login-handoff", captureStderr(t, func() { loginHandoff("claude", "work") }))
	assertApprovedOutput(t, "05-login-success", captureStderr(t, func() { loginResult("claude", "work", true) }))
	assertApprovedOutput(t, "05-login-unconfirmed", captureStderr(t, func() { loginResult("claude", "work", false) }))
	assertApprovedOutput(t, "05-login-cancelled", captureStderr(t, loginStopped))
}

// TestApprovedCredentialViews pins the overview's exceptional states and every detail variant.
func TestApprovedCredentialViews(t *testing.T) {
	t.Run("07-credentials-no-accounts", func(t *testing.T) {
		cfg := freshConfig(t)
		out := captureStdout(t, func() {
			if code, err := (&app{cfg: cfg}).cmdCredentials(nil); code != 0 || err != nil {
				t.Fatalf("cmdCredentials = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "07-credentials-no-accounts", out)
	})

	t.Run("07-credentials-issues", func(t *testing.T) {
		cfg := freshConfig(t)
		writeAccount(t, cfg, "codex", "work", `{"tokens":{}}`) // a marker that cannot authenticate
		setDefault(t, cfg, "codex", "personal")                // …and a default account nobody created
		out := captureStdout(t, func() {
			if code, err := (&app{cfg: cfg}).cmdCredentials([]string{"codex"}); code != 0 || err != nil {
				t.Fatalf("cmdCredentials = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "07-credentials-issues", out)
	})

	t.Run("07-credential-default", func(t *testing.T) {
		cfg := freshConfig(t)
		writeAccount(t, cfg, "codex", "personal", codexReadyCredential)
		setDefault(t, cfg, "codex", "personal")
		assertApprovedOutput(t, "07-credential-default", showProfileOutput(t, cfg, "codex", "personal"))
	})

	t.Run("07-credential-nondefault", func(t *testing.T) {
		cfg := freshConfig(t)
		writeAccount(t, cfg, "claude", "personal", claudeReadyCredential)
		writeAccount(t, cfg, "claude", "work", claudeReadyCredential)
		setDefault(t, cfg, "claude", "personal")
		assertApprovedOutput(t, "07-credential-nondefault", showProfileOutput(t, cfg, "claude", "work"))
	})

	t.Run("07-credential-env-backed", func(t *testing.T) {
		cfg := freshConfig(t)
		if err := os.WriteFile(cfg.EnvFile(), []byte("XAI_API_KEY=key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertApprovedOutput(t, "07-credential-env-backed", showProfileOutput(t, cfg, "grok", cfg.DefaultProfileOf("grok")))
	})
}

// TestApprovedDefaultSelection pins choosing a default: the change, the no-op, and the case where
// the chosen account has no login yet — which changes the default and says so, rather than refusing.
func TestApprovedDefaultSelection(t *testing.T) {
	t.Run("08-default-changed", func(t *testing.T) {
		cfg := freshConfig(t)
		writeAccount(t, cfg, "codex", "personal", codexReadyCredential)
		writeAccount(t, cfg, "codex", "work", codexReadyCredential)
		setDefault(t, cfg, "codex", "personal")
		out := captureStderr(t, func() {
			if code, err := (&app{cfg: cfg}).setProfileDefault("codex", "work"); code != 0 || err != nil {
				t.Fatalf("setProfileDefault = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "08-default-changed", out)
		if got := cfg.DefaultProfileOf("codex"); got != "work" {
			t.Errorf("default = %q, want work", got)
		}
	})

	t.Run("08-default-already", func(t *testing.T) {
		cfg := freshConfig(t)
		writeAccount(t, cfg, "codex", "work", codexReadyCredential)
		setDefault(t, cfg, "codex", "work")
		out := captureStderr(t, func() {
			if code, err := (&app{cfg: cfg}).setProfileDefault("codex", "work"); code != 0 || err != nil {
				t.Fatalf("setProfileDefault = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "08-default-already", out)
	})

	t.Run("08-default-not-signed-in", func(t *testing.T) {
		cfg := freshConfig(t)
		writeAccount(t, cfg, "codex", "personal", codexReadyCredential)
		if err := os.MkdirAll(cfg.AgentProfileDir("codex", "work"), 0o700); err != nil { // a folder, no login
			t.Fatal(err)
		}
		setDefault(t, cfg, "codex", "personal")
		out := captureStderr(t, func() {
			if code, err := (&app{cfg: cfg}).setProfileDefault("codex", "work"); code != 0 || err != nil {
				t.Fatalf("setProfileDefault = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "08-default-not-signed-in", out)
	})
}

// TestApprovedAccountRemoval pins the destructive transcript: the future-tense preview, the shared
// gate's question, and each answer. The question and the answer belong to the TERMINAL (ui.Confirm
// writes the prompt, the user types the rest), so the test puts that one line back where the gate
// writes it — its exact text is pinned in internal/ui/confirm_test.go.
func TestApprovedAccountRemoval(t *testing.T) {
	removable := func(t *testing.T) (*config.Config, *app) {
		t.Helper()
		cfg := freshConfig(t)
		writeAccount(t, cfg, "codex", "personal", codexReadyCredential)
		writeAccount(t, cfg, "codex", "old-work", codexReadyCredential)
		setDefault(t, cfg, "codex", "personal")
		return cfg, &app{cfg: cfg}
	}

	t.Run("08-remove-confirmed", func(t *testing.T) {
		cfg, a := removable(t)
		out := captureStderr(t, func() {
			if code, err := a.removeProfile("codex", "old-work", true); code != 0 || err != nil {
				t.Fatalf("removeProfile = (%d, %v)", code, err)
			}
		})
		preview, result, ok := strings.Cut(out, "\n\n")
		if !ok {
			t.Fatalf("the preview and the result should be separate paragraphs: %q", out)
		}
		assertApprovedOutput(t, "08-remove-confirmed", preview+"\n\nContinue? [y/N] y\n\n"+result)
		if pathExists(cfg.AgentProfileDir("codex", "old-work")) {
			t.Error("the account folder survived a confirmed removal")
		}
		if !pathExists(cfg.AgentProfileDir("codex", "personal")) {
			t.Error("removing one account touched another")
		}
	})

	t.Run("08-remove-declined", func(t *testing.T) {
		cfg, a := removable(t)
		// The gate has no terminal to ask at here, so it refuses instead of prompting: the preview
		// it already printed is the same one a person sees, and the decline is its own say.
		preview := captureStderr(t, func() {
			if code, _ := a.removeProfile("codex", "old-work", false); code != 2 {
				t.Errorf("a piped removal without --yes exited %d, want 2", code)
			}
		})
		preview = strings.TrimSuffix(preview, "\n\n")
		declined := captureStderr(t, removalCancelled)
		assertApprovedOutput(t, "08-remove-declined", preview+"\n\nContinue? [y/N] n\n"+declined)
		if !pathExists(cfg.AgentProfileDir("codex", "old-work")) {
			t.Error("a declined removal still deleted the account")
		}
	})
}

// A piped removal cannot be confirmed: it refuses with the shared block and deletes nothing.
func TestRemovalWithoutATerminalRefuses(t *testing.T) {
	cfg := freshConfig(t)
	writeAccount(t, cfg, "codex", "personal", codexReadyCredential)
	writeAccount(t, cfg, "codex", "old-work", codexReadyCredential)
	setDefault(t, cfg, "codex", "personal")
	code, err := (&app{cfg: cfg}).removeProfile("codex", "old-work", false)
	if code != 2 || err == nil {
		t.Fatalf("removeProfile(piped) = (%d, %v), want (2, a refusal)", code, err)
	}
	var usage *ui.UsageError
	if !errors.As(err, &usage) || usage.Headline != "Confirmation required" {
		t.Errorf("refusal = %v, want the shared Confirmation required block", err)
	}
	if !pathExists(cfg.AgentProfileDir("codex", "old-work")) {
		t.Error("a refused removal deleted the account")
	}
}

// TestApprovedPresetViews pins the listing (comparison table, broken block, empty), the two detail
// shapes, and a successful creation.
func TestApprovedPresetViews(t *testing.T) {
	t.Run("09-presets-list", func(t *testing.T) {
		cfg := freshConfig(t)
		writePresetFile(t, cfg.RepoOverride, "frontier",
			"lead: {agent: [claude:claude-fable-5/xhigh, codex:gpt-5.6-sol/xhigh]}\n"+
				"roles:\n"+
				"  thinker: {mode: consult, agent: codex:gpt-5.6-terra/xhigh}\n"+
				"  critic: {mode: consult, agent: grok:grok-4.5/high}\n"+
				"  fast: {mode: delegate, agent: codex:gpt-5.6-luna/xhigh}\n")
		writeGlobalPreset(t, cfg, "review", "lead: {agent: codex:gpt-5.6-sol/high}\n")
		out := captureStdout(t, func() {
			if code, err := (&app{cfg: cfg}).cmdPresets(nil); code != 0 || err != nil {
				t.Fatalf("cmdPresets = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "09-presets-list", out)
	})

	t.Run("09-presets-broken", func(t *testing.T) {
		cfg := freshConfig(t)
		writePresetFile(t, cfg.RepoOverride, "review",
			"lead: {agent: codex}\nroles: {critic: {mode: consult, agent: claude, prompt: roles/critic.md}}\n")
		out := captureStdout(t, func() {
			if code, err := (&app{cfg: cfg}).cmdPresets(nil); code != 0 || err != nil {
				t.Fatalf("cmdPresets = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "09-presets-broken", out)
	})

	t.Run("09-presets-empty", func(t *testing.T) {
		cfg := freshConfig(t)
		out := captureStdout(t, func() {
			if code, err := (&app{cfg: cfg}).cmdPresets(nil); code != 0 || err != nil {
				t.Fatalf("cmdPresets = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "09-presets-empty", out)
	})

	t.Run("11-preset-detail-single-lead", func(t *testing.T) {
		cfg := freshConfig(t)
		writePresetFile(t, cfg.RepoOverride, "review", "lead: {agent: codex:gpt-5.6-sol/high}\n")
		out := captureStdout(t, func() {
			if code, err := (&app{cfg: cfg}).cmdPresets([]string{"review"}); code != 0 || err != nil {
				t.Fatalf("cmdPresets = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "11-preset-detail-single-lead", out)
	})

	t.Run("11-preset-detail-native-role", func(t *testing.T) {
		// A global preset renders ~-shortened paths, so the fixture's home-relative bytes need a
		// home this test owns.
		home := t.TempDir()
		t.Setenv("HOME", home)
		cfg := freshConfig(t)
		cfg.BoxHome = filepath.Join(home, ".config", "coop")
		dir := writeGlobalPreset(t, cfg, "review",
			"lead: {agent: claude:opus/high, prompt: roles/lead.md}\n"+
				"roles: {critic: {mode: native, agent: claude:sonnet/high, subagent: reviewer}}\n")
		if err := os.MkdirAll(filepath.Join(dir, "roles"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "roles", "lead.md"), []byte("lead\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := captureStdout(t, func() {
			if code, err := (&app{cfg: cfg}).cmdPresets([]string{"review"}); code != 0 || err != nil {
				t.Fatalf("cmdPresets = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "11-preset-detail-native-role", out)
	})

	t.Run("10-presets-init-created", func(t *testing.T) {
		cfg := freshConfig(t)
		out := captureStderr(t, func() {
			if code, err := (&app{cfg: cfg}).presetsInit(cfg.RepoOverride, nil); code != 0 || err != nil {
				t.Fatalf("presetsInit = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "10-presets-init-created", out)
		if !pathExists(filepath.Join(cfg.RepoOverride, ".agent", "presets", "frontier", "preset.yaml")) {
			t.Error("the preset was reported created but is not on disk")
		}
	})
}

// TestApprovedModelMenus pins the exceptional model menus: a stale list with the cause that kept it
// stale, bundled examples when nothing usable was ever fetched, and a healthy block with a standing
// default. The wrap width is the terminal's, so the fixtures' capture width is pinned with it.
func TestApprovedModelMenus(t *testing.T) {
	narrow := func(t *testing.T) {
		t.Helper()
		old := modelMenuWidth
		modelMenuWidth = func() int { return 60 }
		t.Cleanup(func() { modelMenuWidth = old })
	}
	// No fetch may run: every case is inside the retry window a failed attempt left behind.
	noFetch := func(t *testing.T) func(string) ([]agents.Model, error) {
		return func(agent string) ([]agents.Model, error) {
			t.Errorf("the menu refetched %s instead of backing off", agent)
			return nil, nil
		}
	}

	t.Run("12-models-stale-cache", func(t *testing.T) {
		narrow(t)
		cfg := freshConfig(t)
		writeModelsCacheFixture(t, cfg, "codex", modelsCache{
			Models: modelList("gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
				"gpt-5.5", "gpt-5.3-codex-spark"),
			FetchedAt:    time.Now().Add(-30 * time.Hour),
			AttemptedAt:  time.Now().Add(-5 * time.Minute),
			AttemptError: "Codex is unavailable on this host.",
		})
		a := &app{cfg: cfg, acpModels: noFetch(t)}
		out := captureStdout(t, func() {
			if code, err := a.cmdModels([]string{"codex"}); code != 0 || err != nil {
				t.Fatalf("cmdModels = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "12-models-stale-cache", out)
	})

	t.Run("12-models-examples", func(t *testing.T) {
		narrow(t)
		cfg := freshConfig(t)
		writeModelsCacheFixture(t, cfg, "gemini", modelsCache{
			AttemptedAt:  time.Now().Add(-5 * time.Minute),
			AttemptError: "Docker is unavailable.",
		})
		a := &app{cfg: cfg, acpModels: noFetch(t)}
		out := captureStdout(t, func() {
			if code, err := a.cmdModels([]string{"gemini"}); code != 0 || err != nil {
				t.Fatalf("cmdModels = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "12-models-examples", out)
	})

	t.Run("12-models-default", func(t *testing.T) {
		narrow(t)
		t.Setenv("COOP_CLAUDE_MODEL", "fable")
		cfg := freshConfig(t)
		writeModelsCacheFixture(t, cfg, "claude", modelsCache{
			Models:    modelList("opus[1m]", "claude-fable-5", "sonnet", "haiku"),
			FetchedAt: time.Now().Add(-time.Hour),
		})
		a := &app{cfg: cfg, acpModels: noFetch(t)}
		out := captureStdout(t, func() {
			if code, err := a.cmdModels([]string{"claude"}); code != 0 || err != nil {
				t.Fatalf("cmdModels = (%d, %v)", code, err)
			}
		})
		assertApprovedOutput(t, "12-models-default", out)
	})
}

// ---- fixtures ---------------------------------------------------------------------------------

// A codex marker with a refresh token reads as a usable login; claude's wants an access token plus
// its own refresh token. Both are inert test values, not credentials.
const (
	codexReadyCredential  = `{"tokens":{"refresh_token":"r"}}`
	claudeReadyCredential = `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":4102444800000,"scopes":["user:inference"]}}`
)

// writeAccount gives agent a stored account whose marker file holds body.
func writeAccount(t *testing.T, cfg *config.Config, agent, account, body string) {
	t.Helper()
	dir := cfg.AgentProfileDir(agent, account)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ag, ok := agents.Get(agent)
	if !ok {
		t.Fatalf("unknown agent %q", agent)
	}
	marker, _ := ag.AuthMarker()
	if err := os.WriteFile(filepath.Join(dir, marker), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setDefault(t *testing.T, cfg *config.Config, agent, account string) {
	t.Helper()
	if err := cfg.SetDefaultProfile(agent, account); err != nil {
		t.Fatal(err)
	}
}

// showProfileOutput renders one account's detail view.
func showProfileOutput(t *testing.T, cfg *config.Config, agent, account string) string {
	t.Helper()
	return captureStdout(t, func() {
		if code, err := (&app{cfg: cfg}).showProfile(agent, account); code != 0 || err != nil {
			t.Fatalf("showProfile(%s, %s) = (%d, %v)", agent, account, code, err)
		}
	})
}

// writePresetFile writes one project preset.yaml verbatim — including one that will not load.
func writePresetFile(t *testing.T, repo, name, body string) string {
	t.Helper()
	dir := filepath.Join(repo, ".agent", "presets", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeGlobalPreset writes a preset under the per-user global root, which the listing sources as
// "global" and the detail renders ~-shortened.
func writeGlobalPreset(t *testing.T, cfg *config.Config, name, body string) string {
	t.Helper()
	dir := filepath.Join(cfg.GlobalPresetsDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeModelsCacheFixture plants one agent's saved catalog, attempt stamp and cause.
func writeModelsCacheFixture(t *testing.T, cfg *config.Config, agent string, mc modelsCache) {
	t.Helper()
	if err := storeModelsCache(cfg, agent, mc); err != nil {
		t.Fatal(err)
	}
}

func modelList(ids ...string) []agents.Model {
	out := make([]agents.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, agents.Model{ID: id, Name: id})
	}
	return out
}
