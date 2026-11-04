package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/testutil/threadtext"
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
	dead := acpctl.Problem{Title: "Claude Code isn't available.", Detail: "Its saved sign-in no longer works.", Command: "coop login claude@dead", Purpose: "on the host"}
	same := func(got, want []acpctl.Problem) bool { return fmt.Sprintf("%+v", got) == fmt.Sprintf("%+v", want) }

	// An expired access token with no refresh token cannot renew: it needs a new sign-in.
	write("dead", `{"claudeAiOauth":{"accessToken":"a","expiresAt":1,"scopes":["user:inference"]}}`)
	if got := signInProblems(cfg, "claude", time.Now()); !same(got, []acpctl.Problem{dead}) {
		t.Errorf("only account dead:\n got %+v\nwant %+v", got, dead)
	}
	// One that can renew still works, so the dead account is named.
	write("renew", `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1,"scopes":["user:inference"]}}`)
	named := dead
	named.Title = `Claude Code account "dead" isn't available.`
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
		{Title: `The "review" preset is missing from the toolbar.`, Detail: "It could not be loaded.", Command: "coop presets", Purpose: "on the host, to see why"},
		{Title: `The "init" preset is missing from the toolbar.`, Detail: "It could not be loaded.", Command: "coop presets", Purpose: "on the host, to see why"},
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("skipped presets:\n got %+v\nwant %+v", got, want)
	}
}

// Every sentence the session writes into a thread by itself, however the box or the rules got there.
func TestThreadMessagesAreStructured(t *testing.T) {
	required := &box.ProjectBuildRequired{}
	approved := &box.ProjectBuildRequired{Approved: true}
	pending := &networkstate.PendingApproval{}
	for name, text := range map[string]string{
		"box start failed":  acpBoxStartFailedText,
		"box ready":         acpBoxReadyNotice,
		"rules moved":       acpRefreshMoved,
		"wait: approval":    acpBoxWaitText(pending, nil, false),
		"wait: reconnect":   acpBoxWaitText(errACPNetworkModeChanged, nil, false),
		"wait: build fails": acpBoxWaitText(errors.New("no"), required, true),
		"wait: rebuilding":  acpBoxWaitText(errors.New("no"), approved, false),
		"wait: build":       acpBoxWaitText(errors.New("no"), required, false),
		"wait: services":    acpBoxWaitText(errors.New("Some services could not start:\n\n  db failed its healthcheck"), nil, false),
		"kept: changed":     acpRefreshNotice(errACPNetworkModeChanged),
		"kept: withdrawn":   acpRefreshNotice(errACPApprovalWithdrawn),
		"kept: review":      acpRefreshNotice(pending),
		"kept: setup":       acpRefreshNotice(box.ErrNetworkSetupRequired),
		"kept: other":       acpRefreshNotice(errors.New("no")),
	} {
		t.Run(name, func(t *testing.T) { threadtext.Check(t, text) })
	}
	cfg := &config.Config{ConfigDir: t.TempDir()}
	dir := cfg.AgentProfileDir("claude", "dead")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"a","expiresAt":1,"scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	problems := editorStartProblems(cfg, []presetSkip{{name: "review", err: errors.New("no")}}, time.Now())
	if len(problems) != 2 {
		t.Fatalf("want a dead sign-in and a skipped preset, got %+v", problems)
	}
	for _, problem := range problems {
		threadtext.Check(t, acpctl.ProblemText(problem))
	}
}
