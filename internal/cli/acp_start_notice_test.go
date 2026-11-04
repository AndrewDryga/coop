package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/acpctl"
	"github.com/AndrewDryga/coop/internal/acpproxy"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/testutil/threadtext"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
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
		"box start failed":   acpBoxFailedText(acpproxy.ChildExited),
		"box silent":         acpBoxFailedText(acpproxy.ChildSilent),
		"box not ready":      acpBoxFailedText(acpproxy.ChildNotReady),
		"box ready":          acpBoxReadyNotice,
		"rules moved":        acpRefreshMoved,
		"wait: approval":     acpBoxWaitText(pending, nil, false),
		"wait: reconnect":    acpBoxWaitText(errACPNetworkModeChanged, nil, false),
		"wait: build fails":  acpBoxWaitText(errors.New("no"), required, true),
		"wait: rebuilding":   acpBoxWaitText(errors.New("no"), approved, false),
		"wait: build":        acpBoxWaitText(errors.New("no"), required, false),
		"wait: services":     acpBoxWaitText(servicesRefusal(t, servicesCompose, ""), nil, false),
		"wait: network":      acpBoxWaitText(servicesRefusal(t, "", "shared"), nil, false),
		"wait: other":        acpBoxWaitText(errors.New("Some services could not start:\n\n  db failed its healthcheck"), nil, false),
		"kept: changed":      acpRefreshNotice(errACPNetworkModeChanged),
		"kept: withdrawn":    acpRefreshNotice(errACPApprovalWithdrawn),
		"kept: review":       acpRefreshNotice(pending),
		"kept: setup":        acpRefreshNotice(box.ErrNetworkSetupRequired),
		"kept: other":        acpRefreshNotice(errors.New("no")),
		"wait: broken file":  acpBoxWaitText(&box.BrokenProjectFile{Err: errors.New(".agent/project.yaml: yaml: line 1: did not find expected node content")}, nil, false),
		"wait: not admitted": acpBoxWaitText(box.ErrProjectNotAdmitted, nil, false),
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

// servicesCompose has a service with a port, one without, and one whose name no rule can carry.
const servicesCompose = "services:\n  db:\n    image: postgres:18\n    expose: [\"5432\"]\n  web:\n    image: nginx:1\n" +
	"  \"evil](http://example.test)\":\n    image: example/other:1\n"

// servicesRefusal is the refusal a filtered box of a project with no rules really gets, from this Compose
// file or, with none, from COOP_SERVICES_NET.
func servicesRefusal(t *testing.T, compose, servicesNet string) *box.ServicesNeedRules {
	t.Helper()
	a, repo := servicesProject(t, "", compose)
	a.cfg.ServicesNet = servicesNet
	refusal, err := box.ProjectServicesRefusal(t.Context(), a.cfg, repo)
	if err != nil || refusal == nil {
		t.Fatalf("refused=%t err=%v, want a refusal", refusal != nil, err)
	}
	return refusal
}

// threadHTML renders a thread's text the way an editor does.
func threadHTML(t *testing.T, text string) string {
	t.Helper()
	var html bytes.Buffer
	if err := goldmark.New(goldmark.WithExtensions(extension.GFM)).Convert([]byte(text), &html); err != nil {
		t.Fatal(err)
	}
	return html.String()
}

// A services refusal reads whole at any panel width. An editor does not wrap a code block, so only the
// rules to paste and the command to run are blocks, and all the rest is text that wraps.
func TestServicesRemedyInAThreadIsTextAroundItsBlocks(t *testing.T) {
	const want = `<p>⚠ <strong>This project's box cannot start yet.</strong></p>
<p>Filtered boxes reach project services only through approved network rules. Your messages wait here and go out once it is ready.</p>
<p>Ask for each service it uses under box.egress_rules in .agent/project.yaml:</p>
<pre><code>    - to: {service: &quot;db&quot;}
      protocol: tcp
      ports: [5432]
    - to: {service: &quot;web&quot;}
      protocol: tcp
      ports: [&lt;port&gt;]
</code></pre>
<p>Replace &lt;port&gt; with the port the service listens on.</p>
<p>Then run this in this project, on the host:</p>
<pre><code>coop approve
</code></pre>
<p>To start without project services, set box.network: false.</p>
`
	if got := threadHTML(t, acpBoxWaitText(servicesRefusal(t, servicesCompose, ""), nil, false)); got != want {
		t.Errorf("the thread renders\n%s\nwant\n%s", got, want)
	}
	// Every service has its port: nothing to replace, so the thread does not ask.
	portsOnly := acpBoxWaitText(servicesRefusal(t, "services:\n  db:\n    image: postgres:18\n    expose: [\"5432\"]\n", ""), nil, false)
	if strings.Contains(portsOnly, "Replace") || strings.Contains(portsOnly, "port\\>") {
		t.Errorf("the thread asks to replace a port it already has:\n%s", portsOnly)
	}
	// No approval settles a network no filtered box joins, so nothing here says to run one.
	const noRules = `<p>⚠ <strong>This project's box cannot start yet.</strong></p>
<p>A filtered box does not join the network COOP_SERVICES_NET names. Your messages wait here and go out once it is ready.</p>
<p>Unset COOP_SERVICES_NET, or set box.network: false in .agent/project.yaml.</p>
`
	if got := threadHTML(t, acpBoxWaitText(servicesRefusal(t, "", "shared"), nil, false)); got != noRules {
		t.Errorf("the thread renders\n%s\nwant\n%s", got, noRules)
	}
}
