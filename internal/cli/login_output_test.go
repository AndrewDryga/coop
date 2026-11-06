package cli

import (
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

func TestLoginNarrationHandoffAndSuccess(t *testing.T) {
	got := captureStderr(t, func() { loginHandoff("grok", config.DefaultProfile) })
	if want := "Signing in to grok\n\n"; got != want {
		t.Fatalf("handoff = %q, want %q", got, want)
	}
	got = captureStderr(t, func() {
		ui.Note("The Coop box has stopped: main process exited with status 0.")
		loginResult("grok", config.DefaultProfile, true, false)
	})
	want := "The Coop box has stopped: main process exited with status 0.\n\n✓ Signed in to Grok\n\nStart Grok:\n  coop grok\n"
	if got != want {
		t.Fatalf("success = %q, want %q", got, want)
	}
}

func TestLoginNarrationStartsOnlyAtLoginLaunch(t *testing.T) {
	for _, tc := range []struct{ command, loginProvider, want string }{
		{"login", "grok", "Follow Grok's sign-in instructions below"},
		{"grok", "grok", "Follow Grok's sign-in instructions below"},
		{"grok@work", "grok", "Follow Grok's sign-in instructions below"},
		{"grok", "", ""}, {"run", "", ""}, {"acp", "", ""},
		{"login", "claude", ""},
	} {
		a := &app{argv: []string{tc.command, "grok"}, loginProvider: tc.loginProvider}
		if got := a.loginStartingNotice("grok"); got != tc.want {
			t.Fatalf("%s notice = %q, want %q", tc.command, got, tc.want)
		}
	}
}

// A sign-in whose provider stops leaves no .sign-in-* folder behind: nothing names it, and it may
// hold a provider's partial files.
func TestStoppedLoginLeavesNoStage(t *testing.T) {
	repo, _ := gitrepo.New(t)
	t.Chdir(repo)
	oldTTY := loginInputIsTerminal
	loginInputIsTerminal = func() bool { return true }
	defer func() { loginInputIsTerminal = oldTTY }()
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1\" in run) exit 130;; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := openNetwork(&config.Config{ConfigDir: filepath.Join(t.TempDir(), "coop"), BaseImage: "coop-box", BoxHome: t.TempDir()})
	a := &app{cfg: cfg, rt: runtime.Runtime{Name: shim}, rtSet: true}
	var code int
	captureStderr(t, func() { code, _ = a.loginTo("claude", config.DefaultProfile) })
	if code == 0 {
		t.Fatal("a stopped sign-in reported success")
	}
	entries, err := os.ReadDir(cfg.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sign-in-") {
			t.Errorf("left %s behind", entry.Name())
		}
	}
}
