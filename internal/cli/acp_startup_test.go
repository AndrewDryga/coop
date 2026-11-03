package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	boxruntime "github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestACPAutomaticStartup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		providers []string
		account   string
		want      string
	}{
		{name: "one signed-in provider", providers: []string{"codex"}, want: "codex@default"},
		{name: "registry order", providers: []string{"grok", "gemini", "codex", "claude"}, want: "claude@default"},
		{name: "unsigned earlier providers", providers: []string{"grok", "gemini"}, want: "gemini@default"},
		{name: "marked default account", providers: []string{"codex"}, account: "work", want: "codex@work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := openNetwork(&config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()})
			for _, provider := range tc.providers {
				account := "default"
				if tc.account != "" {
					account = tc.account
					if err := cfg.SetDefaultProfile(provider, account); err != nil {
						t.Fatal(err)
					}
				}
				signInCred(t, cfg, provider, account)
			}
			called := false
			a := &app{cfg: cfg, acpSupervise: func(args []string, ctrl *acpctl.Control) (int, error) {
				called = true
				if len(args) != 0 {
					t.Fatalf("automatic startup rewrote editor arguments: %v", args)
				}
				target, preset, ok := ctrl.SpawnTarget()
				if !ok || preset != "" || target.String() != tc.want {
					t.Fatalf("automatic target = (%s, %q, %v), want %s", target.String(), preset, ok, tc.want)
				}
				if selection := ctrl.Snapshot().Selection; selection != (acpctl.Selection{}) {
					t.Fatalf("automatic startup pinned the selection: %+v", selection)
				}
				return 0, nil
			}}
			if code, err := a.cmdACP(nil); code != 0 || err != nil || !called {
				t.Fatalf("no-target ACP = (%d, %v), supervised=%v", code, err, called)
			}
		})
	}
}

func TestACPAutomaticStartupRequiresSignIn(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()}
	a := &app{cfg: cfg, acpSupervise: func([]string, *acpctl.Control) (int, error) {
		t.Fatal("unsigned startup launched the supervisor")
		return 0, nil
	}}
	var code int
	var err error
	output := captureStdout(t, func() { code, err = a.cmdACP(nil) })
	if code != 1 || err == nil || !strings.Contains(err.Error(), "coop login") {
		t.Fatalf("unsigned startup = (%d, %v), want sign-in guidance", code, err)
	}
	var failure *ui.UsageError
	if !errors.As(err, &failure) || failure.ExitCode != 1 || failure.Cause != "No providers are signed in." {
		t.Fatalf("unsigned startup must use the shared actionable command-failure block: %v", err)
	}
	if output != "" {
		t.Fatalf("unsigned startup wrote to the ACP protocol stream: %q", output)
	}
}

func TestACPAutomaticStartupPreservesExplicitTarget(t *testing.T) {
	cfg := openNetwork(&config.Config{ConfigDir: t.TempDir(), RepoOverride: t.TempDir()})
	signInCred(t, cfg, "claude", "default")
	signInCred(t, cfg, "codex", "work")
	const want = "codex:chosen-model/high@work"
	a := &app{cfg: cfg, acpSupervise: func(_ []string, ctrl *acpctl.Control) (int, error) {
		target, _, ok := ctrl.SpawnTarget()
		if !ok || target.String() != want || ctrl.Snapshot().Selection.Provider != "codex" {
			t.Fatalf("explicit launch lost target or pin: %s, %+v", target, ctrl.Snapshot().Selection)
		}
		return 0, nil
	}}
	if code, err := a.cmdACP([]string{want}); code != 0 || err != nil {
		t.Fatalf("explicit ACP = (%d, %v)", code, err)
	}
	if code, err := a.cmdACP([]string{"codex:chosen-model/high@missing"}); code != 2 || err == nil {
		t.Fatalf("invalid explicit account fell back: (%d, %v)", code, err)
	}
}

// An editor child its supervisor stopped stays quiet: a clean disconnect printed one bare
// "✗ interrupted" per parked box. The exit status still reports the stop, and a failure on the way
// out, or one while the child was running, still prints.
func TestEditorChildStoppedBySupervisorIsQuiet(t *testing.T) {
	signalled, cancel := context.WithCancel(context.Background())
	cancel()
	_, stopped := box.Run(&config.Config{}, boxruntime.Runtime{}, box.RunSpec{Ctx: signalled})
	if !box.Interrupted(stopped) {
		t.Fatalf("a launch whose context was cancelled returned %v", stopped)
	}
	cleanup := errors.New("filesystem custody retained at /state/coop-fs-1: permission denied")
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		quiet bool
	}{
		{"stopped by its supervisor", signalled, stopped, true},
		{"stopped while its cleanup failed", signalled, errors.Join(stopped, cleanup), false},
		{"a failure while it ran", context.Background(), cleanup, false},
		{"a child with no stop signal", nil, stopped, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStderr(t, func() { code = reportExit(stoppedQuietly(tc.ctx, 143, tc.err)) })
			if code != 143 {
				t.Errorf("exit = %d, want the run's 143", code)
			}
			if quiet := out == ""; quiet != tc.quiet {
				t.Errorf("output %q, want quiet: %v", out, tc.quiet)
			}
		})
	}
}
