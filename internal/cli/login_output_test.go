package cli

import (
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
		ui.Note("The Coop box has stopped — main process exited with status 0.")
		loginResult("grok", config.DefaultProfile, true)
	})
	want := "The Coop box has stopped — main process exited with status 0.\n\n✓ Signed in to Grok\n\nStart Grok:\n  coop grok\n"
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
