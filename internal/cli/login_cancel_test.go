package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/ui"
)

const cancelledLine = "\nSign-in cancelled. Nothing was saved.\n"

// cancelFixture is coop login at a terminal with a stand-in for the container runtime. script is the
// runtime's shell: it runs for every runtime command, and `run` is the provider's own sign-in box.
// The configuration folder already holds another account's data and another sign-in's stage, so a
// cleanup that reaches too far shows. calls is where the stand-in records its `run` commands.
func cancelFixture(t *testing.T, script string) (a *app, cfg *config.Config, calls string) {
	t.Helper()
	repo, _ := gitrepo.New(t)
	t.Chdir(repo)
	oldTTY := loginInputIsTerminal
	loginInputIsTerminal = func() bool { return true }
	t.Cleanup(func() { loginInputIsTerminal = oldTTY })
	calls = filepath.Join(t.TempDir(), "run-calls")
	shim := filepath.Join(t.TempDir(), "runtime")
	body := "#!/bin/sh\ncase \"$1\" in run) echo run >> " + calls + "\n" + script + ";; esac\nexit 0\n"
	if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg = openNetwork(&config.Config{ConfigDir: filepath.Join(t.TempDir(), "coop"), BaseImage: "coop-box", BoxHome: t.TempDir(), HomeInBox: "/home/node", Homes: true})
	for _, keep := range []string{filepath.Join(cfg.ConfigDir, ".sign-in-other", "auth.json"), filepath.Join(cfg.ConfigDir, "agents", "claude", "keep")} {
		if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keep, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &app{cfg: cfg, rt: runtime.Runtime{Name: shim}, rtSet: true}, cfg, calls
}

// assertNothingSaved checks what every cancelled sign-in leaves: no staging folder of its own, no
// account, and everything that was there before.
func assertNothingSaved(t *testing.T, cfg *config.Config, agent string) {
	t.Helper()
	entries, err := os.ReadDir(cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sign-in-") && entry.Name() != ".sign-in-other" {
			t.Errorf("left %s behind", entry.Name())
		}
	}
	for _, kept := range []string{filepath.Join(".sign-in-other", "auth.json"), filepath.Join("agents", "claude", "keep")} {
		if _, err := os.Stat(filepath.Join(cfg.ConfigDir, kept)); err != nil {
			t.Errorf("the cleanup took %s: %v", kept, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, agent, "credentials")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a cancelled sign-in left account state: %v", err)
	}
	if profiles, err := box.EffectiveProfiles(cfg, agent); err == nil && len(profiles) != 0 {
		t.Errorf("a cancelled sign-in left %s accounts %v", agent, profiles)
	}
}

// A person stopping the provider's own screen ends the sign-in however the provider takes it, and
// all of them read the same: Claude and Grok quit with Ctrl-C's status, Codex answers it with
// status 0 and no login. One plain line, status 130, no stage, no account, and no failure to save
// something that was never written.
func TestSignInTheProviderEndsWithoutALoginIsCancelled(t *testing.T) {
	for _, tc := range []struct{ name, agent, script string }{
		{"claude quits with Ctrl-C's status", "claude", "exit 130"},
		{"grok quits with Ctrl-C's status", "grok", "exit 130"},
		{"codex answers Ctrl-C with status 0 and no login", "codex", "exit 0"},
		{"claude exits cleanly with no credentials", "claude", "exit 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, cfg, _ := cancelFixture(t, tc.script)
			var code int
			var err error
			out := captureStderr(t, func() { code, err = a.loginTo(tc.agent, "work") })
			if code != 130 || err != nil {
				t.Fatalf("loginTo = (%d, %v), want (130, nil)\n%s", code, err, out)
			}
			if !strings.HasSuffix(out, cancelledLine) || strings.Contains(out, "could not save") {
				t.Errorf("the output does not end with the one cancel line, or reports a failed save:\n%s", out)
			}
			assertNothingSaved(t, cfg, tc.agent)
		})
	}
}

// A provider that fails on its own is not a cancel: its status stands and nothing says cancelled.
func TestSignInTheProviderFailsOnItsOwnIsNotCancelled(t *testing.T) {
	a, cfg, _ := cancelFixture(t, "exit 1")
	var code int
	var err error
	out := captureStderr(t, func() { code, err = a.loginTo("codex", "work") })
	if code != 1 || err != nil || strings.Contains(out, "cancelled") {
		t.Fatalf("a failing provider = (%d, %v), output:\n%s", code, err, out)
	}
	assertNothingSaved(t, cfg, "codex")
}

// A sign-in that finishes is never mistaken for a cancel.
func TestSignInThatFinishesIsNotCancelled(t *testing.T) {
	repo, _ := gitrepo.New(t)
	t.Chdir(repo)
	oldTTY := loginInputIsTerminal
	loginInputIsTerminal = func() bool { return true }
	t.Cleanup(func() { loginInputIsTerminal = oldTTY })
	cfg := openNetwork(&config.Config{ConfigDir: filepath.Join(t.TempDir(), "coop"), BaseImage: "coop-box", BoxHome: t.TempDir(), HomeInBox: "/home/node", Homes: true})
	out := signInTo(t, cfg, "codex", "work")
	if strings.Contains(out, "cancelled") || !strings.Contains(out, "Signed in to Codex") {
		t.Fatalf("a finished sign-in reads as cancelled:\n%s", out)
	}
}

// Ctrl-C, a kill or a closed terminal while Coop is still admitting the launch, which is where the
// first filtered launch on a host spends its seconds, stops the sign-in and ends it as a stop before
// the box started: the status of the signal, the stage gone, no box run, and the one cancel line.
// Coop used to die there with the check's containers and volumes still running.
func TestSignalWhileAdmittingTheSignInCancelsIt(t *testing.T) {
	for _, tc := range []struct {
		sig    syscall.Signal
		status int
	}{{syscall.SIGINT, 130}, {syscall.SIGHUP, 129}, {syscall.SIGTERM, 143}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			a, cfg, calls := cancelFixture(t, "exit 0")
			started := make(chan struct{}, 1)
			a.admitNetwork = func(_ *config.Config, _ runtime.Runtime, spec box.RunSpec, _ box.NetworkAdmission) (*box.CapturedEgress, error) {
				if spec.Ctx == nil {
					return nil, errors.New("admission ran with no way to stop it")
				}
				started <- struct{}{}
				<-spec.Ctx.Done()
				return nil, fmt.Errorf("restricted networking: %w", spec.Ctx.Err())
			}
			go func() {
				<-started
				_ = syscall.Kill(os.Getpid(), tc.sig)
			}()
			var code int
			var err error
			out := captureStderr(t, func() { code, err = a.loginTo("claude", "work") })
			if code != tc.status || !errors.Is(err, ui.ErrReported) || !errors.Is(err, context.Canceled) {
				t.Fatalf("loginTo = (%d, %v), want status %d and a reported cancellation\n%s", code, err, tc.status, out)
			}
			if want := "\nStopped before the box started.\n" + cancelledLine; !strings.HasSuffix(out, want) {
				t.Errorf("the output does not end with the stop line and the cancel line:\n%s", out)
			}
			if _, statErr := os.Stat(calls); !errors.Is(statErr, os.ErrNotExist) {
				t.Error("a box was started for a launch that was stopped while it was admitted")
			}
			assertNothingSaved(t, cfg, "claude")
		})
	}
}

// Any other interactive launch stopped while it is admitted ends the same way, with only the stop
// line: the status of the signal, and nothing left to start.
func TestSignalWhileAdmittingALaunchStopsItBeforeTheBox(t *testing.T) {
	a, _, calls := cancelFixture(t, "exit 0")
	started := make(chan struct{}, 1)
	a.admitNetwork = func(_ *config.Config, _ runtime.Runtime, spec box.RunSpec, _ box.NetworkAdmission) (*box.CapturedEgress, error) {
		started <- struct{}{}
		<-spec.Ctx.Done()
		return nil, spec.Ctx.Err()
	}
	go func() {
		<-started
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	var code int
	var err error
	out := captureStderr(t, func() { code, err = a.runInBox([]string{"true"}, "", nil) })
	if code != 130 || !errors.Is(err, ui.ErrReported) {
		t.Fatalf("runInBox = (%d, %v), want 130 and an error already reported", code, err)
	}
	if out != "\nStopped before the box started.\n" {
		t.Errorf("a stopped launch said %q", out)
	}
	if _, statErr := os.Stat(calls); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a box was started for a launch that was stopped while it was admitted")
	}
}

// Ctrl-C, or Ctrl-D before anything is typed, at the API key prompt Coop itself shows for Gemini is
// a cancel too: the person gave no key, so nothing is staged or saved.
func TestSignInCancelledAtTheKeyPromptSavesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		read error
	}{{"Ctrl-C or Ctrl-D", ui.ErrCancelled}, {"the same, wrapped", fmt.Errorf("read the key: %w", ui.ErrCancelled)}} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg, _ := cancelFixture(t, "exit 0")
			oldRead := readLoginSecret
			readLoginSecret = func(string) ([]byte, error) { return nil, tc.read }
			t.Cleanup(func() { readLoginSecret = oldRead })
			var code int
			var err error
			out := captureStderr(t, func() { code, err = (&app{cfg: cfg}).loginTo("gemini", "work") })
			if code != 130 || err != nil || !strings.HasSuffix(out, cancelledLine) {
				t.Fatalf("loginTo = (%d, %v), output:\n%s", code, err, out)
			}
			assertNothingSaved(t, cfg, "gemini")
		})
	}
}

// Which endings of the provider's box are a sign-in a person stopped, and with what status. A stop
// Coop heard is a cancellation whatever the launch said; a stop before the box started comes back
// reported with its signal's status; the provider quitting with Ctrl-C's status is one too. Every
// other ending is the provider's own.
func TestSignInStoppedNamesOnlyTheEndingsAPersonCaused(t *testing.T) {
	reported := ui.Reported(errors.New("could not start"))
	canceled := ui.Reported(fmt.Errorf("stopped: %w", errors.Join(errors.New("docker said"), context.Canceled)))
	for _, tc := range []struct {
		name       string
		code       int
		err        error
		wantStatus int
		wantStop   bool
	}{
		{"a stop once the box ran", 130, canceled, 130, true},
		{"a hangup once the box ran", 129, canceled, 129, true},
		{"a stop that carried no status", -1, canceled, 130, true},
		{"a stop before the box started", 130, reported, 130, true},
		{"a hangup before the box started", 129, reported, 129, true},
		{"a kill before the box started", 143, reported, 143, true},
		{"the provider took Ctrl-C itself", 130, nil, 130, true},
		{"the provider's own success", 0, nil, 0, false},
		{"the provider's own failure", 1, nil, 0, false},
		{"a hangup status the provider chose itself", 129, nil, 0, false},
		{"a failure the launch reported", 1, reported, 0, false},
		{"a failure with no status", -1, reported, 0, false},
		{"a refusal", -1, errors.New("no box"), 0, false},
	} {
		status, stopped := signInStopped(tc.code, tc.err)
		if stopped != tc.wantStop || status != tc.wantStatus {
			t.Errorf("%s: signInStopped(%d, %v) = (%d, %v), want (%d, %v)", tc.name, tc.code, tc.err, status, stopped, tc.wantStatus, tc.wantStop)
		}
	}
	if got := admissionStatus(errors.New("refused")); got != 1 {
		t.Errorf("a refused admission exits %d, want 1", got)
	}
	if got := admissionStatus(ui.Reported(&stoppedLaunch{status: 129})); got != 129 {
		t.Errorf("a hangup during admission exits %d, want 129", got)
	}
}
