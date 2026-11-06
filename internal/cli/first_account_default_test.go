package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/nativeauth"
)

// codexSignInRuntime stands in for the provider's own sign-in: its box run writes a fixture login
// into the home the sign-in box mounts, as `codex login` does.
func codexSignInRuntime(t *testing.T, cfg *config.Config, account string) runtime.Runtime {
	t.Helper()
	fixture := t.TempDir()
	for name, data := range nativeauth.Files(t, "codex", account) {
		if err := os.WriteFile(filepath.Join(fixture, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := ",target=" + cfg.HomeInBox + "/.codex"
	script := "#!/bin/sh\n[ \"$1\" = run ] || exit 0\nfor arg in \"$@\"; do\n  case \"$arg\" in\n" +
		"    type=bind,source=*" + target + ")\n" +
		"      src=${arg#type=bind,source=}\n      src=${src%" + target + "}\n" +
		"      cp " + strconv.Quote(fixture) + "/* \"$src\"/ || exit 1;;\n  esac\ndone\n"
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime.Runtime{Name: shim}
}

// signInTo runs coop login for one named account through the agent's own sign-in path: Gemini
// reads an API key at the terminal, Codex signs in inside a box.
func signInTo(t *testing.T, cfg *config.Config, agent, account string) string {
	t.Helper()
	a := &app{cfg: cfg}
	if agent == "codex" {
		a.rt, a.rtSet = codexSignInRuntime(t, cfg, account), true
	}
	var code int
	var err error
	out := captureStderr(t, func() { code, err = a.loginTo(agent, account) })
	if code != 0 || err != nil {
		t.Fatalf("coop login %s@%s = (%d, %v)\n%s", agent, account, code, err, out)
	}
	return out
}

// The first account an agent gets becomes its default even when the person names it, so the bare
// agent runs it; a later named sign-in leaves that default alone.
func TestFirstNamedLoginBecomesTheDefault(t *testing.T) {
	repo, _ := gitrepo.New(t)
	t.Chdir(repo)
	oldRead, oldTTY := readLoginSecret, loginInputIsTerminal
	readLoginSecret = func(string) ([]byte, error) { return []byte("fixture-api-key"), nil }
	loginInputIsTerminal = func() bool { return true }
	t.Cleanup(func() { readLoginSecret, loginInputIsTerminal = oldRead, oldTTY })

	for _, agent := range []string{"gemini", "codex"} {
		t.Run(agent, func(t *testing.T) {
			title := titleName(agent)
			cfg := openNetwork(&config.Config{ConfigDir: filepath.Join(t.TempDir(), "coop"), BaseImage: "coop-box",
				BoxHome: t.TempDir(), HomeInBox: "/home/node", Homes: true})

			out := signInTo(t, cfg, agent, "work")
			if got := cfg.DefaultProfileOf(agent); got != "work" {
				t.Fatalf("after the first named sign-in the default is %q, want work", got)
			}
			if want := "✓ " + title + " will use work by default\n\nStart " + title + ":\n  coop " + agent + "\n"; !strings.HasSuffix(out, want) {
				t.Errorf("first sign-in output does not end with %q:\n%s", want, out)
			}
			// It is saved for later commands, and coop credentials marks it.
			if data, err := os.ReadFile(cfg.DefaultsFile()); err != nil || string(data) != agent+"=work\n" {
				t.Errorf("saved defaults = (%q, %v), want %s=work", data, err, agent)
			}
			listing := captureStdout(t, func() {
				if code, err := (&app{cfg: cfg}).cmdCredentials([]string{agent}); code != 0 || err != nil {
					t.Fatalf("coop credentials %s = (%d, %v)", agent, code, err)
				}
			})
			if !strings.Contains(listing, "default\n") || strings.Contains(listing, "is missing") {
				t.Errorf("coop credentials %s does not mark work as the default:\n%s", agent, listing)
			}
			// coop <agent> runs it: the run's account is work, and its filtered launch is admitted.
			if err := box.MissingDefaultAccount(cfg, agent, cfg.ActiveProfile(agent)); err != nil {
				t.Errorf("a run of the new default is refused: %v", err)
			}
			if _, err := box.NetworkTargetBundle(cfg, agents.Target{Provider: agent, Accounts: []string{cfg.ActiveProfile(agent)}}, egress.ClientCLI); err != nil {
				t.Errorf("a filtered run of the new default is refused: %v", err)
			}

			out = signInTo(t, cfg, agent, "personal")
			if got := cfg.DefaultProfileOf(agent); got != "work" {
				t.Errorf("a second named sign-in moved the default to %q, want work", got)
			}
			if strings.Contains(out, "by default") || !strings.HasSuffix(out, "coop "+agent+"@personal\n") {
				t.Errorf("second sign-in output claims a default or starts the wrong account:\n%s", out)
			}
		})
	}

	// A first sign-in without a name already lands on the default, so it marks nothing.
	cfg := &config.Config{ConfigDir: filepath.Join(t.TempDir(), "coop")}
	out := signInTo(t, cfg, "gemini", "")
	if strings.Contains(out, "by default") || !strings.HasSuffix(out, "Start Gemini:\n  coop gemini\n") || pathExists(cfg.DefaultsFile()) {
		t.Errorf("a first sign-in to the default account changed the default:\n%s", out)
	}
}

// A sign-in Coop could not confirm changes no setting: the person has to sign in again first.
func TestUnconfirmedLoginLeavesTheDefault(t *testing.T) {
	cfg := freshConfig(t)
	writeAccount(t, cfg, "codex", "work", `{"tokens":{}}`) // saved, but it cannot authenticate
	out := captureStderr(t, func() {
		if code, err := finishLogin(cfg, "codex", "work"); code != 0 || err != nil {
			t.Fatalf("finishLogin = (%d, %v)", code, err)
		}
	})
	if got := cfg.DefaultProfileOf("codex"); got != config.DefaultProfile || !strings.Contains(out, "could not confirm sign-in") {
		t.Errorf("an unconfirmed sign-in made %q the default:\n%s", got, out)
	}
}

// A sign-in to an agent that already has an account, even only one in the environment file,
// keeps the default it has.
func TestNamedLoginBesideAnEnvironmentKeyKeepsTheDefault(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	if err := os.WriteFile(cfg.EnvFile(), []byte("GEMINI_API_KEY=env-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRead, oldTTY := readLoginSecret, loginInputIsTerminal
	readLoginSecret = func(string) ([]byte, error) { return []byte("fixture-api-key"), nil }
	loginInputIsTerminal = func() bool { return true }
	t.Cleanup(func() { readLoginSecret, loginInputIsTerminal = oldRead, oldTTY })
	signInTo(t, cfg, "gemini", "work")
	if got := cfg.DefaultProfileOf("gemini"); got != config.DefaultProfile {
		t.Errorf("a named sign-in beside an environment key moved the default to %q", got)
	}
}

// With no usable default, a run names the signed-in accounts and the two commands that use one,
// never another sign-in — for the bare agent, and for any run that admits its account (loop,
// editor and fork runs reach it through the network admission).
func TestRunWithoutADefaultOffersTheSignedInAccounts(t *testing.T) {
	for _, agent := range []string{"gemini", "codex"} {
		t.Run(agent, func(t *testing.T) {
			cfg := freshConfig(t)
			signIn := func(account string) {
				if agent == "gemini" {
					gemini, _ := agents.Get("gemini")
					if err := box.SaveHostCredential(cfg, gemini, account, []byte("key")); err != nil {
						t.Fatal(err)
					}
					return
				}
				seedNativeFixture(t, cfg, agent, account)
			}
			signIn("work")
			signIn("personal")
			setDefault(t, cfg, agent, "gone") // the default's account was deleted by hand
			want := []string{
				titleOf(t, agent) + " has no default account",
				"Signed in: personal, work",
				"Choose a default: coop credentials " + agent + " personal default",
				"Use this account: coop " + agent + "@personal",
			}
			a := &app{cfg: cfg}
			code, launchErr := a.launchAgent(agent, nil)
			if code != 1 || launchErr == nil {
				t.Fatalf("coop %s with a missing default = (%d, %v), want 1 and the refusal", agent, code, launchErr)
			}
			writePresetFile(t, cfg.RepoOverride, "solo", "lead:\n  agent: ["+agent+"]\n")
			p, err := a.loadRunPreset("coop", "solo")
			if err != nil {
				t.Fatal(err)
			}
			presetCode, presetErr := a.launchPreset(p, nil)
			if presetCode != 1 {
				t.Errorf("a preset whose lead has a missing default exited %d, want 1", presetCode)
			}
			for _, err := range []error{launchErr, presetErr, bundleErr(cfg, agent)} {
				for _, line := range want {
					if err == nil || !strings.Contains(err.Error(), line) {
						t.Errorf("refusal %v does not say %q", err, line)
					}
				}
				if err != nil && strings.Contains(err.Error(), "coop login") {
					t.Errorf("refusal sends the person through another sign-in: %v", err)
				}
			}
			// A named account that is missing is not the default's problem: its own refusal applies.
			if err := box.MissingDefaultAccount(cfg, agent, "ghost"); err != nil {
				t.Errorf("a missing account that is not the default was refused as one: %v", err)
			}
		})
	}
}

// With nothing signed in besides the missing default, signing in is still the fix.
func TestRunWithoutAnyUsableAccountStillOffersASignIn(t *testing.T) {
	cfg := freshConfig(t)
	writeAccount(t, cfg, "codex", "work", `{"tokens":{}}`) // a login that cannot authenticate
	if err := box.MissingDefaultAccount(cfg, "codex", cfg.ActiveProfile("codex")); err != nil {
		t.Errorf("an unusable sibling account was offered as the default: %v", err)
	}
	if err := bundleErr(cfg, "codex"); err == nil || !strings.Contains(err.Error(), "Sign in: coop login codex") {
		t.Errorf("filtered run without a usable account = %v, want the sign-in refusal", err)
	}
}

func bundleErr(cfg *config.Config, agent string) error {
	_, err := box.NetworkTargetBundle(cfg, agents.Target{Provider: agent, Accounts: []string{cfg.ActiveProfile(agent)}}, egress.ClientCLI)
	return err
}

func titleOf(t *testing.T, agent string) string {
	t.Helper()
	ag, ok := agents.Get(agent)
	if !ok {
		t.Fatalf("unknown agent %q", agent)
	}
	return ag.DisplayName()
}
