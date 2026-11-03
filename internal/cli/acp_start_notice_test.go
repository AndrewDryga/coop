package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/config"
)

// A saved sign-in that can no longer work is named in the editor with the login that fixes it: the
// provider alone when no account of it works any more, each dead account when another still does.
func TestEditorSignInProblemsNameWhatToFix(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	write := func(profile, body string) {
		dir := cfg.AgentProfileDir("claude", profile)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dead := acpctl.Problem{Title: "Claude Code isn't available", Detail: "its saved sign-in no longer works.", Command: "coop login claude@dead"}
	same := func(got, want []acpctl.Problem) bool { return fmt.Sprintf("%+v", got) == fmt.Sprintf("%+v", want) }

	// An expired access token with no refresh token cannot renew: it needs a new sign-in.
	write("dead", `{"claudeAiOauth":{"accessToken":"a","expiresAt":1,"scopes":["user:inference"]}}`)
	if got := signInProblems(cfg, "claude", time.Now()); !same(got, []acpctl.Problem{dead}) {
		t.Errorf("only account dead:\n got %+v\nwant %+v", got, dead)
	}
	// One that can renew still works, so the dead account is named.
	write("renew", `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1,"scopes":["user:inference"]}}`)
	named := dead
	named.Title = `Claude Code account "dead" isn't available`
	if got := signInProblems(cfg, "claude", time.Now()); !same(got, []acpctl.Problem{named}) {
		t.Errorf("a working account beside it:\n got %+v\nwant %+v", got, named)
	}
	if got := signInProblems(cfg, "codex", time.Now()); got != nil {
		t.Errorf("a provider with nothing saved has nothing to fix, got %+v", got)
	}
}

// A preset the selector left out is named, with the listing that shows why it failed. The command
// names no preset: `coop presets init` would create one instead.
func TestEditorStartProblemsNameSkippedPresets(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir()}
	got := editorStartProblems(cfg, []presetSkip{{name: "review"}, {name: "init"}}, time.Now())
	want := []acpctl.Problem{
		{Title: `The "review" preset is missing from the toolbar`, Detail: "it could not be loaded.", Command: "coop presets", Purpose: "to see why"},
		{Title: `The "init" preset is missing from the toolbar`, Detail: "it could not be loaded.", Command: "coop presets", Purpose: "to see why"},
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("skipped presets:\n got %+v\nwant %+v", got, want)
	}
}
