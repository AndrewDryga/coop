package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/procharness"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkreport"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// captureStderr returns whatever fn writes to os.Stderr — the ui helpers emit there when no live
// sink owns the terminal. The test binary's stderr is not a terminal, so the bytes carry no ANSI.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	out, err := procharness.CaptureOutput(fn, &os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// grant is one compiled allowance with the provenance the section reads.
func grant(id, domain string, origins ...egress.Origin) egress.Grant {
	return egress.Grant{ID: id, Rule: egress.Rule{To: egress.Destination{Domain: domain}, Protocol: "tls", Ports: []int{443}}, Origins: origins}
}

// Acceptance: the interactive launch narrates secrets, one stable `Internet access` section and
// the agent start as bold, unprefixed sections — the filtered rows name every allowance before
// claiming the rest is blocked, and no `coop:` prefix appears before the agent's output.
func TestLaunchSectionsNarrateAFilteredInteractiveLaunch(t *testing.T) {
	spec := RunSpec{Agent: "codex", Cmd: []string{"codex"}}
	policy := egress.Snapshot{Mode: egress.Filtered,
		Dependencies: []egress.Dependency{{Provider: "codex"}},
		Grants: []egress.Grant{
			grant("g1", "api.openai.com", egress.Origin{Kind: "provider", Provider: "codex"}),
			grant("g2", "auth.openai.com", egress.Origin{Kind: "provider", Provider: "codex"}),
			grant("g3", "example.com", egress.Origin{Kind: "project", Name: "repo"}),
			grant("g4", "registry.example", egress.Origin{Kind: "project", Name: "repo"}),
			grant("g5", "docs.example", egress.Origin{Kind: "operator", Name: "allow-domain"}),
			grant("g6", "mcp-a.example", egress.Origin{Kind: "mcp", Name: "alpha"}),
			grant("g7", "mcp-b.example", egress.Origin{Kind: "mcp", Name: "beta"}),
			grant("g8", "mcp-b2.example", egress.Origin{Kind: "mcp", Name: "beta"}),
		}}
	got := captureStderr(t, func() {
		s := newLaunchSections(spec)
		s.secrets(8)
		s.internet(&config.Config{Egress: "filtered"}, spec, &policy, nil)
		s.starting()
	})
	want := "Protecting secrets\n" +
		"  ✓ 8 secret paths hidden from the box\n" +
		"\nConfiguring network access\n" +
		"  ✓ OpenAI endpoints allowed\n" +
		"  ✓ Applied 2 approved network rules\n" +
		"  ✓ Applied 1 network rule for this run only\n" +
		"  ✓ 2 configured MCP services allowed\n" +
		"  ✓ Everything else blocked\n" +
		"\nStarting Codex\n"
	if got != want {
		t.Fatalf("launch narrated:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(got, "coop:") || strings.Contains(got, "\x1b") {
		t.Fatalf("pre-launch sections must carry no coop: prefix and no ANSI off a terminal:\n%s", got)
	}
}

// A run nothing chose a mode for is filtered by default, so its network section opens by saying
// so and how to leave it. A chosen filtered run, from a flag, COOP_EGRESS, the project or an
// approval, keeps its usual rows. A raw run allowed nothing is told everything is blocked.
func TestDefaultFilteredRunSaysSoFirst(t *testing.T) {
	policy := egress.Snapshot{Mode: egress.Filtered,
		Dependencies: []egress.Dependency{{Provider: "codex"}},
		Grants:       []egress.Grant{grant("g1", "api.openai.com", egress.Origin{Kind: "provider", Provider: "codex"})}}
	cfg := &config.Config{Egress: "filtered"}
	for _, c := range []struct {
		name   string
		spec   RunSpec
		policy egress.Snapshot
		want   string
	}{
		{"default agent", RunSpec{Agent: "codex", CapturedEgress: &CapturedEgress{Default: true}}, policy,
			"  Filtered by default. Use --egress open for unrestricted access.\n  ✓ OpenAI endpoints allowed\n  ✓ Everything else blocked\n"},
		{"chosen agent", RunSpec{Agent: "codex", CapturedEgress: &CapturedEgress{}}, policy,
			"  ✓ OpenAI endpoints allowed\n  ✓ Everything else blocked\n"},
		{"default raw run", RunSpec{Cmd: []string{"true"}, CapturedEgress: &CapturedEgress{Default: true}}, egress.Snapshot{Mode: egress.Filtered},
			"  Filtered by default. Use --egress open for unrestricted access.\n  ✓ Everything blocked\n"},
	} {
		got := captureStderr(t, func() { newLaunchSections(c.spec).internet(cfg, c.spec, &c.policy, nil) })
		if want := "Configuring network access\n" + c.want; got != want {
			t.Errorf("%s: rendered %q, want %q", c.name, got, want)
		}
	}
}

// A signed-in or API-key account reaches its provider through Coop's broker, so the agent's own
// policy grants it nothing, and every such run printed "Everything blocked" over a provider it then
// used. The section names each account's provider, lead first, before the rest is called blocked,
// and a raw run that connects no account still reaches nothing.
func TestFilteredRunNamesTheProviderItsAccountReaches(t *testing.T) {
	cfg, brokered := &config.Config{Egress: "filtered"}, egress.Snapshot{Mode: egress.Filtered}
	for _, c := range []struct {
		name      string
		spec      RunSpec
		providers []string
		want      string
	}{
		{"one account", RunSpec{Agent: "claude"}, []string{"claude"}, "  ✓ Anthropic endpoints allowed\n  ✓ Everything else blocked\n"},
		{"a peer", RunSpec{Agent: "claude"}, []string{"claude", "codex", "claude"},
			"  ✓ Anthropic endpoints allowed\n  ✓ OpenAI endpoints allowed\n  ✓ Everything else blocked\n"},
		{"raw run", RunSpec{Cmd: []string{"true"}}, nil, "  ✓ Everything blocked\n"},
	} {
		got := captureStderr(t, func() { newLaunchSections(c.spec).internet(cfg, c.spec, &brokered, c.providers) })
		if want := "Configuring network access\n" + c.want; got != want {
			t.Errorf("%s: rendered %q, want %q", c.name, got, want)
		}
	}
}

// A rule from --allow-domain or --egress-rules lets this run reach its destination and no other run,
// and nobody approved it, so it is never counted with the approved rules. A rule the project also
// asks for, and that was approved, stays approved.
func TestOneRunRulesAreNeverCalledApproved(t *testing.T) {
	policy := egress.Snapshot{Grants: []egress.Grant{
		grant("g1", "docs.example", egress.Origin{Kind: "operator", Name: "allow-domain"}),
		grant("g2", "files.example", egress.Origin{Kind: "operator", Name: "rules-file"}),
	}}
	rows := networkAllowances(policy, nil)
	if want := []string{"Applied 2 network rules for this run only"}; !slices.Equal(rows, want) {
		t.Fatalf("one-run rules = %q, want %q", rows, want)
	}
	policy.Grants = append(policy.Grants, grant("g3", "proxy.golang.org", egress.Origin{Kind: "operator", Name: "allow-domain"}, egress.Origin{Kind: "project"}))
	if rows := networkAllowances(policy, nil); !slices.Equal(rows, []string{"Applied 1 approved network rule", "Applied 2 network rules for this run only"}) {
		t.Fatalf("approved beside one-run rules = %q", rows)
	}
}

// Every mode shares the heading; open and offline replace the green rows with one warning, and
// a raw run — no agent — is told what offline means for it without inventing a provider.
func TestInternetSectionIsOneHeadingInEveryMode(t *testing.T) {
	cases := []struct {
		name string
		spec RunSpec
		cfg  *config.Config
		want string
	}{
		{"open", RunSpec{Agent: "codex"}, &config.Config{Egress: "open"}, "  ⚠ Unrestricted: nothing is blocked\n"},
		{"offline agent", RunSpec{Agent: "codex"}, &config.Config{Egress: "none"}, "  ⚠ Offline: Codex cannot reach OpenAI\n"},
		{"offline raw", RunSpec{Cmd: []string{"true"}}, &config.Config{Egress: "none"}, "  ⚠ Offline: nothing outside the box can be reached\n"},
	}
	for _, c := range cases {
		got := captureStderr(t, func() { newLaunchSections(c.spec).internet(c.cfg, c.spec, nil, nil) })
		if got != "Configuring network access\n"+c.want {
			t.Errorf("%s: rendered %q, want %q", c.name, got, "Configuring network access\n"+c.want)
		}
	}
}

// A one-off --egress open or offline runs without the project's network rules; the network
// section says so under the mode's warning, and says nothing when the project has none.
func TestInternetSectionSaysTheProjectRulesDoNotApply(t *testing.T) {
	spec := RunSpec{Agent: "codex"}
	for _, c := range []struct {
		egress string
		rules  int
		want   string
	}{
		{"open", 2, "  ⚠ Unrestricted: nothing is blocked\n  This project's network rules do not apply to this run.\n"},
		{"none", 1, "  ⚠ Offline: Codex cannot reach OpenAI\n  This project's network rules do not apply to this run.\n"},
		{"none", 0, "  ⚠ Offline: Codex cannot reach OpenAI\n"},
	} {
		got := captureStderr(t, func() {
			s := newLaunchSections(spec)
			s.internet(&config.Config{Egress: c.egress}, spec, nil, nil)
			s.projectRulesSetAside(c.rules)
		})
		if want := "Configuring network access\n" + c.want; got != want {
			t.Errorf("%s with %d rules: rendered %q, want %q", c.egress, c.rules, got, want)
		}
	}
	quiet := RunSpec{Agent: "codex", Batch: true}
	if got := captureStderr(t, func() { newLaunchSections(quiet).projectRulesSetAside(3) }); got != "" {
		t.Errorf("a batch run narrated %q", got)
	}
}

// The account section names every account a launch connects, one stable row each, and — once, and
// only when an API key is protected — what that protection means; several accounts pluralize both.
// A launch with no account prints no section, and batch output stays quiet.
func TestAccountSectionNamesEveryConnectedAccount(t *testing.T) {
	spec := RunSpec{Agent: "gemini", Cmd: []string{"gemini"}}
	for _, c := range []struct {
		name string
		rows []accountRow
		want string
	}{
		{"one protected key", []accountRow{{"gemini", "personal2", true}},
			"Connecting account\n  ✓ Gemini (personal2) · API key protected\n\n" +
				"  The agent can use this account; the key never leaves this computer.\n"},
		{"two protected keys", []accountRow{{"gemini", "personal2", true}, {"codex", "work", true}},
			"Connecting accounts\n  ✓ Gemini (personal2) · API key protected\n  ✓ Codex (work) · API key protected\n\n" +
				"  Agents can use these accounts; the keys never leave this computer.\n"},
		{"a protected key and a login", []accountRow{{"gemini", "personal2", true}, {"claude", "work", false}},
			"Connecting accounts\n  ✓ Gemini (personal2) · API key protected\n  ✓ Claude (work) · Signed in\n\n" +
				"  Agents can use these accounts; the keys never leave this computer.\n"},
		{"signed in only", []accountRow{{"claude", "work", false}}, "Connecting account\n  ✓ Claude (work) · Signed in\n"},
		{"no account", nil, ""},
	} {
		if got := captureStderr(t, func() { newLaunchSections(spec).accounts(c.rows) }); got != c.want {
			t.Errorf("%s: rendered\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
	batch := RunSpec{Agent: "gemini", Batch: true}
	if got := captureStderr(t, func() { newLaunchSections(batch).accounts([]accountRow{{"gemini", "personal2", true}}) }); got != "" {
		t.Errorf("batch output narrated its accounts: %q", got)
	}
}

// Only a brokered route is a protected key; a signed-in teammate is a login, and a provider with no
// credential at all is no account the launch connects.
func TestLaunchAccountsComeFromTheRunsOwnScope(t *testing.T) {
	cfg, _ := brokerFixture(t, "GEMINI_API_KEY=gemini-secret\n")
	seedCanonicalFixture(t, cfg, "claude", "default")

	spec := RunSpec{Agent: "gemini", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "claude"}, {Provider: "codex"}}}
	spec.native = nativeBrokerPlanFixture(t, cfg, spec)
	plan, err := selectCredentialPlan(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	rows := launchAccounts(cfg, spec, plan)
	want := []accountRow{{"gemini", "default", true}, {"claude", "default", false}}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("launch accounts = %+v, want %+v", rows, want)
	}
	if rows := launchAccounts(cfg, RunSpec{Cmd: []string{"sh"}, Homes: true}, nil); len(rows) != 0 {
		t.Fatalf("a raw run connected accounts: %+v", rows)
	}
}

// An editor names what a filtered session may reach, so the grouping keeps each approved rule's
// destination, once and in policy order, beside the vendor, MCP server and other counts the
// terminal prints.
func TestNetworkAllowedByNamesWhatEachApprovalReaches(t *testing.T) {
	policy := egress.Snapshot{
		Dependencies: []egress.Dependency{{Provider: "claude"}},
		Grants: []egress.Grant{
			grant("g1", "proxy.golang.org", egress.Origin{Kind: "project"}),
			grant("g2", "api.anthropic.com", egress.Origin{Kind: "provider", Provider: "claude"}),
			grant("g3", "sum.golang.org", egress.Origin{Kind: "operator"}, egress.Origin{Kind: "project"}),
			grant("g4", "docs.example", egress.Origin{Kind: "mcp", Name: "docs"}),
			grant("g5", "x.example", egress.Origin{Kind: "session"}),
			{ID: "g6", Rule: egress.Rule{To: egress.Destination{Service: "db"}}, Origins: []egress.Origin{{Kind: "project"}}},
		},
	}
	got := fmt.Sprintf("%+v", NetworkAllowedBy(policy, nil, "emisar"))
	want := fmt.Sprintf("%+v", NetworkAllowed{Vendors: []string{"Anthropic"}, Approved: []string{"proxy.golang.org", "sum.golang.org", "the db service"},
		Servers: []string{"docs", "emisar"}, Other: 1})
	if got != want {
		t.Fatalf("allowed = %s, want %s", got, want)
	}
}

// An editor lists sidecar links only for a session whose box starts the services itself, decided
// as box.Run decides it: open, joined to the services network, auto_up on, on a runtime that runs
// Compose, with the project's own box: section applied first.
func TestEditorStartsServicesOnlyWhereTheBoxDoes(t *testing.T) {
	docker, apple := runtime.Runtime{Name: "docker"}, runtime.Runtime{Name: "container"}
	open, plain := &config.Config{AutoUp: true, Network: true, Egress: "open"}, &project.Project{}
	if !EditorStartsServices(open, docker, plain) {
		t.Fatal("an open session with auto_up starts its services")
	}
	for name, cfg := range map[string]*config.Config{
		"filtered":    {AutoUp: true, Network: true, Egress: "filtered"},
		"offline":     {AutoUp: true, Network: true, Egress: "none"},
		"auto_up off": {AutoUp: false, Network: true, Egress: "open"},
		"no network":  {AutoUp: true, Network: false, Egress: "open"},
	} {
		if EditorStartsServices(cfg, docker, plain) {
			t.Errorf("%s: the box starts no services, so nothing may be listed", name)
		}
	}
	if EditorStartsServices(open, apple, plain) {
		t.Error("Apple container runs no Compose, so nothing may be listed")
	}
	off := false
	for name, p := range map[string]*project.Project{
		"project auto_up: false": {Box: project.Box{AutoUp: &off}},
		"project network: false": {Box: project.Box{Network: &off}},
	} {
		if EditorStartsServices(open, docker, p) {
			t.Errorf("%s: the box starts no services, so nothing may be listed", name)
		}
	}
}

// A coop acp session's services run as its own Compose project, so their host ports are keyed by
// the session as well as the workspace; the workspace's own project keeps the plain key.
func TestSessionServicePortsAreKeyedByTheOwner(t *testing.T) {
	workspace := t.TempDir()
	if got, want := servicePortScope(workspace, ""), canonicalWorkspace(workspace); got != want {
		t.Fatalf("the workspace's own scope = %q, want %q", got, want)
	}
	owned := servicePortScope(workspace, "sup1")
	if !strings.HasSuffix(owned, "#coop-service-owner:sup1") ||
		project.HostPortFor(owned, "db:5432") == project.HostPortFor(canonicalWorkspace(workspace), "db:5432") {
		t.Fatalf("a session's services must not share the workspace's host ports: scope %q", owned)
	}
}

// "Everything else blocked" is only true once every grant has a row. A grant whose provenance
// the section does not recognize still gets counted rather than silently dropped, and a run
// with nothing to hide says so instead of inventing a count.
func TestNetworkAllowancesNeverOmitAGrant(t *testing.T) {
	policy := egress.Snapshot{Grants: []egress.Grant{
		grant("g1", "a.example", egress.Origin{Kind: "session"}),
		grant("g2", "b.example", egress.Origin{Kind: "provider", Provider: "claude"}, egress.Origin{Kind: "project"}),
	}}
	rows := networkAllowances(policy, nil)
	want := []string{"Anthropic endpoints allowed", "Applied 1 approved network rule", "1 other destination allowed"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("allowances = %q, want %q", rows, want)
	}
	// A server reached through the credential broker has no grant of its own, and is still allowed.
	policy.Grants = append(policy.Grants, grant("g3", "docs.example", egress.Origin{Kind: "mcp", Name: "docs"}))
	if rows := networkAllowances(policy, nil, "emisar"); !slices.Contains(rows, "2 configured MCP services allowed") {
		t.Fatalf("allowances with a brokered MCP server = %q", rows)
	}
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Cmd: []string{"sh"}}).secrets(0) }); got != "Protecting secrets\n  ✓ No secret paths to hide\n" {
		t.Fatalf("zero secrets rendered %q", got)
	}
	// A .coopignore entry with a comment after its pattern hides nothing: the launch says so under
	// Protecting secrets, as a plain warning outside a narrated launch, and nothing when quiet.
	entries := []string{"prod.yml   # basename"}
	caution := `  ⚠ .coopignore entry "prod.yml   # basename" is one pattern, comment and all; put comments on their own line` + "\n"
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Cmd: []string{"sh"}}).coopignoreComments(entries, false) }); got != caution {
		t.Fatalf("narrated commented entry = %q", got)
	}
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Batch: true}).coopignoreComments(entries, false) }); !strings.Contains(got, `"prod.yml   # basename" is one pattern`) {
		t.Fatalf("batch commented entry = %q", got)
	}
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Quiet: true}).coopignoreComments(entries, true) }); got != "" {
		t.Fatalf("quiet commented entry = %q", got)
	}
}

// What is known about the image goes under its own heading as cautions, never as a `coop:` line
// before the agent speaks — and an image with nothing to say costs no section at all.
func TestBoxSectionCarriesImageNudgesAsCautions(t *testing.T) {
	s := newLaunchSections(RunSpec{Agent: "codex"})
	if got := captureStderr(t, func() { s.box(nil) }); got != "" {
		t.Fatalf("a current image printed %q", got)
	}
	got := captureStderr(t, func() {
		s.box([]string{"box image is 40 days old — 'coop update' rebuilds it on the newest OS packages and Node"})
	})
	want := "Checking the Coop box\n  ⚠ box image is 40 days old — 'coop update' rebuilds it on the newest OS packages and Node\n"
	if got != want {
		t.Fatalf("nudges rendered %q, want %q", got, want)
	}
}

// A cancellation the stop line already named comes back marked reported, so the dispatcher adds
// no bare "✗ interrupted" beneath the run; anything else — a cancellation nobody signalled, a
// runtime failure — keeps its message for the dispatcher.
func TestExplainedMarksOnlyANamedInterruption(t *testing.T) {
	s := newLaunchSections(RunSpec{Agent: "codex"})
	signalled := &hostInterrupt{sig: os.Interrupt}
	if err := s.explained(interruptedRun{}, signalled); !errors.Is(err, ui.ErrReported) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a signalled interruption = %v, want reported and still a cancellation", err)
	}
	if err := s.explained(interruptedRun{}, nil); errors.Is(err, ui.ErrReported) {
		t.Fatal("a cancellation no signal explains must reach the dispatcher")
	}
	if err := s.explained(errors.New("restricted network health lost"), signalled); errors.Is(err, ui.ErrReported) {
		t.Fatal("a runtime failure keeps its message")
	}
	if err := newLaunchSections(RunSpec{Batch: true}).explained(interruptedRun{}, signalled); errors.Is(err, ui.ErrReported) {
		t.Fatal("a batch run's errors are the caller's to report")
	}
}

// A launch a person stopped before its main process ran is one line and the shell's status for a
// signal, whatever the stop broke on the way. A failure nobody caused, one that cleanup could not
// undo and a batch run's errors keep their message and the launch's own status.
func TestEndedSaysOnlyThatAStoppedLaunchNeverStarted(t *testing.T) {
	ctrlC, term, hangup := &hostInterrupt{sig: os.Interrupt}, &hostInterrupt{sig: syscall.SIGTERM}, &hostInterrupt{sig: syscall.SIGHUP}
	raw := errors.New("Docker file copy requires one bounded regular file") // what a call the stop killed says
	cleanup := errors.New("filesystem custody retained at /state/coop-fs-1")
	for _, test := range []struct {
		name         string
		spec         RunSpec
		started      bool
		interrupt    *hostInterrupt
		teardownErr  error
		result       error
		wantOutput   string // exact; contains when it starts with "~"
		wantStatus   int
		wantReported bool
	}{
		{"Ctrl-C before the box started", RunSpec{}, false, ctrlC, nil, raw, "\nStopped before the box started.\n", 130, true},
		{"SIGTERM before the box started", RunSpec{}, false, term, nil, raw, "\nStopped before the box started.\n", 143, true},
		{"a closed terminal before the box started", RunSpec{}, false, hangup, nil, raw, "\nStopped before the box started.\n", 129, true},
		{"a closed terminal after the box started", RunSpec{}, true, hangup, nil, interruptedRun{}, "", 129, true},
		{"a Ctrl-C only the box heard", RunSpec{}, false, nil, nil, fmt.Errorf("%w: %w", runtime.ErrDockerStartInterrupted, raw), "\nStopped before the box started.\n", 130, true},
		{"cleanup could not finish", RunSpec{}, false, ctrlC, cleanup, errors.Join(raw, cleanup), "~✗ Could not start sleep", -1, true},
		{"a failure nobody caused", RunSpec{}, false, nil, nil, raw, "~✗ Could not start sleep", -1, true},
		{"Ctrl-C after the box started", RunSpec{}, true, ctrlC, nil, interruptedRun{}, "", 130, true},
		{"a failure after the box started", RunSpec{}, true, ctrlC, nil, raw, "", -1, false},
		{"a batch run", RunSpec{Batch: true}, false, ctrlC, nil, raw, "", 130, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := test.spec
			spec.Cmd = []string{"sleep"}
			var status int
			var got error
			output := captureStderr(t, func() {
				s := newLaunchSections(spec)
				s.starting()
				status, got = s.ended(test.started, test.interrupt, test.teardownErr, -1, test.result)
			})
			output = strings.TrimPrefix(output, "Starting sleep\n")
			if want, contains := strings.CutPrefix(test.wantOutput, "~"); contains && !strings.Contains(output, want) || !contains && output != want {
				t.Errorf("printed %q, want %q", output, test.wantOutput)
			}
			if status != test.wantStatus || errors.Is(got, ui.ErrReported) != test.wantReported {
				t.Errorf("status %d, reported %v; want %d, %v", status, errors.Is(got, ui.ErrReported), test.wantStatus, test.wantReported)
			}
			if !errors.Is(got, test.result) && got != test.result {
				t.Errorf("the error changed to %v", got)
			}
		})
	}
}

// The status of a signal's death is 128 plus its number; a recorder no signal reached has none.
func TestHostInterruptExitCodeIsTheShellsForASignal(t *testing.T) {
	var none *hostInterrupt
	for _, test := range []struct {
		recorder *hostInterrupt
		want     int
	}{{&hostInterrupt{sig: os.Interrupt}, 130}, {&hostInterrupt{sig: syscall.SIGTERM}, 143}, {&hostInterrupt{sig: syscall.SIGHUP}, 129}, {&hostInterrupt{}, 0}, {none, 0}} {
		if got := test.recorder.exitCode(); got != test.want {
			t.Errorf("exitCode() = %d, want %d", got, test.want)
		}
	}
}

func TestLaunchStoppingImmediatelyPrecedesCleanup(t *testing.T) {
	got := captureStderr(t, func() {
		s := newLaunchSections(RunSpec{Agent: "claude"})
		s.stopping()
		ui.Note("CLEANUP")
		s.stopped("main process exited with status 0")
	})
	want := "\nStopping Claude Code\nCLEANUP\n\nThe Coop box has stopped: main process exited with status 0.\n"
	if got != want {
		t.Fatalf("shutdown feedback = %q, want %q", got, want)
	}
}

// A command run with an agent's account that is not the agent, such as the loop's debug shell, is
// started and stopped under its own name, with its guidance right above its prompt.
func TestLaunchSubjectNamesACommandThatIsNotTheAgent(t *testing.T) {
	got := captureStderr(t, func() {
		s := newLaunchSections(RunSpec{Agent: "claude", Cmd: []string{"/bin/bash"}, Subject: "bash", StartingNotice: "Exit the shell to retry."})
		s.starting()
		s.stopping()
	})
	if want := "Starting bash\nExit the shell to retry.\n\nStopping bash\n"; got != want {
		t.Fatalf("shell launch = %q, want %q", got, want)
	}
}

// Ordinary batch runs, quiet probes and ACP children keep their bounded output: every narration
// call is a no-op and a failure comes back exactly as it was, for the caller's own reporting.
func TestLaunchSectionsAreSilentOutsideInteractiveRuns(t *testing.T) {
	for _, spec := range []RunSpec{{Batch: true}, {Quiet: true}, {ForceNoTTY: true}} {
		spec.StartingNotice = "must remain silent"
		s := newLaunchSections(spec)
		cause := errors.New("no")
		got := captureStderr(t, func() {
			s.secrets(3)
			s.internet(&config.Config{Egress: "open"}, spec, nil, nil)
			s.starting()
			s.stopping()
			s.stopped("main process exited with status 0")
			if err := s.failed(cause); err != cause {
				t.Errorf("%+v: failed() = %v, want the cause untouched", spec, err)
			}
		})
		if got != "" {
			t.Errorf("%+v printed %q", spec, got)
		}
	}
}

func TestLoopLaunchSectionsNestSetupWithoutChangingBatchSilence(t *testing.T) {
	spec := RunSpec{Agent: "claude", AgentCommand: true, Batch: true, LoopPresentation: true}
	got := captureStderr(t, func() {
		s := newLaunchSections(spec)
		s.secrets(8)
		s.internet(&config.Config{Egress: "none"}, spec, nil, nil)
		s.servicesFailed("compose up exited with status 1")
		s.starting()
	})
	want := "Preparing task environment\n" +
		"  Protecting secrets\n" +
		"  ✓ 8 secret paths hidden from the box\n" +
		"\n  Configuring network access\n" +
		"  ⚠ Offline: Claude cannot reach Anthropic\n" +
		"\n  Starting services\n" +
		"  ⚠ Services failed to start · exit 1\n" +
		"    Continuing without project services.\n" +
		"    To retry: coop up\n"
	if got != want {
		t.Fatalf("loop setup = %q, want %q", got, want)
	}
	custom := RunSpec{Agent: "claude", Cmd: []string{"make", "update-reference\x1b[31m"}, Batch: true, LoopPresentation: true}
	got = captureStderr(t, func() { newLaunchSections(custom).starting() })
	if got != "\nStarting make update-reference\n" {
		t.Fatalf("custom loop start = %q", got)
	}
	lines := loopStartingLines("make "+strings.Repeat("long-target", 5)+"\x1b[31m", 28)
	for _, line := range lines {
		if strings.Contains(line, "\x1b") || len([]rune(line)) > 28 {
			t.Fatalf("wrapped custom loop start retained controls or exceeded width: %q", lines)
		}
	}
	raw := RunSpec{Agent: "claude", Batch: true, LoopPresentation: true}
	got = captureStderr(t, func() {
		newLaunchSections(raw).internet(&config.Config{Egress: "none"}, raw, nil, nil)
	})
	if got != "Preparing task environment\n  Configuring network access\n  ⚠ Offline: nothing outside the box can be reached\n" {
		t.Fatalf("offline custom command must remain a caution, got %q", got)
	}
}

func TestLoopServiceWarningsStayNestedAndSanitized(t *testing.T) {
	got := captureStderr(t, func() {
		s := newLaunchSections(RunSpec{Agent: "claude", AgentCommand: true, Batch: true, LoopPresentation: true})
		s.servicesPreparing()
		s.serviceSecrets([]string{"dev/keycloak/certs/generated/tls.key\x1b[31m"}, "/repo/compose.yml")
		s.servicesFailed("compose detail: \x1b[31mred\x1b[0m\nexit status 1")
	})
	for _, want := range []string{
		"Preparing task environment\n  Starting services",
		"Services received an empty file for a secret path",
		"    dev/keycloak/certs/generated/tls.key",
		"    To allow the real file, run coop up and approve compose.yml.",
		"Services failed to start · exit 1",
		"    compose detail: red",
		"    Continuing without project services.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("nested service output missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "Starting services") != 1 || strings.Contains(got, "\x1b") {
		t.Fatalf("service output repeated its section or retained controls: %q", got)
	}
}

func TestServiceWarningsNameObservedPartialAvailability(t *testing.T) {
	db := ServicePort{Service: "db", ContainerPort: 5432, HostPort: 25432, Scheme: "postgresql"}
	loop := captureStderr(t, func() {
		s := newLaunchSections(RunSpec{Agent: "claude", AgentCommand: true, Batch: true, LoopPresentation: true})
		s.servicesFailed("keycloak unhealthy", db)
	})
	for _, want := range []string{"Some services failed to start", "Continuing with the running services: db.", "To retry: coop up"} {
		if !strings.Contains(loop, want) {
			t.Errorf("partial loop warning lacks %q:\n%s", want, loop)
		}
	}
	interactive := captureStderr(t, func() {
		newLaunchSections(RunSpec{Agent: "claude"}).servicesFailed("keycloak unhealthy", db)
	})
	if !strings.Contains(interactive, "Some project services could not start") || !strings.Contains(interactive, "Available now: db.") {
		t.Fatalf("partial interactive warning is not actionable:\n%s", interactive)
	}
}

func TestInteractiveServiceWarningsAreSeparateParagraphs(t *testing.T) {
	for _, report := range []func(*launchSections){
		func(s *launchSections) { s.servicesFailed("compose failed") },
	} {
		got := captureStderr(t, func() {
			s := newLaunchSections(RunSpec{Agent: "gemini"})
			s.internet(&config.Config{Egress: "open"}, RunSpec{Agent: "gemini"}, nil, nil)
			report(s)
		})
		if !strings.HasPrefix(got, "Configuring network access\n  ⚠ Unrestricted: nothing is blocked\n\n⚠ Project services") {
			t.Fatalf("service warning is not separated from network results: %q", got)
		}
		if strings.Contains(got, "\n\n\n") {
			t.Fatalf("service warning has repeated separators: %q", got)
		}
	}
}

func TestLoopServiceDetailsWrapBeforeStyling(t *testing.T) {
	long := "dev/" + strings.Repeat("generated-secret-path/", 6) + "tls.key"
	got := captureStderr(t, func() {
		s := newLaunchSections(RunSpec{Batch: true, LoopPresentation: true})
		s.serviceSecrets([]string{long}, "/repo/compose.yml")
		s.servicesFailed("inspection failed for " + long)
	})
	for _, line := range strings.Split(got, "\n") {
		if len([]rune(line)) > 79 {
			t.Fatalf("service detail exceeded the static terminal width: %q", line)
		}
	}
	if !strings.Contains(strings.ReplaceAll(got, "\n    ", ""), long) || !strings.Contains(got, "To retry: coop up") {
		t.Fatalf("wrapped service detail lost its path or remedy: %q", got)
	}
}

func TestLoginStartingNoticeRunOrdering(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "rt")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1\" in run) printf 'PROVIDER OUTPUT\\n' >&2 ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), HomeInBox: "/home/node", Egress: "open"}
	spec := RunSpec{Image: "i", Repo: t.TempDir(), Workdir: "/workspace", Agent: "grok", Cmd: []string{"grok", "login", "--device-auth"}, StartingNotice: "Follow Grok's sign-in instructions below"}
	var code int
	var err error
	got := captureStderr(t, func() { code, err = Run(cfg, runtime.Runtime{Name: shim}, spec) })
	if code != 0 || err != nil {
		t.Fatalf("Run = %d, %v", code, err)
	}
	want := "Protecting secrets\n  ✓ No secret paths to hide\n" + mountProtectionLine + "\nConfiguring network access\n  ⚠ Unrestricted: nothing is blocked\n\nStarting Grok\nFollow Grok's sign-in instructions below\nPROVIDER OUTPUT\n\nThe Coop box has stopped: main process exited with status 0.\n"
	if got != want {
		t.Fatalf("login launch = %q, want %q", got, want)
	}
}

// A launch that stops before its main process renders ONE nested failure under the section in
// progress and marks the error reported, so the dispatcher's fallback line cannot repeat it. A
// cancellation and an already-reported error pass through untouched.
func TestLaunchFailureIsRenderedOnceAndMarkedReported(t *testing.T) {
	s := newLaunchSections(RunSpec{Agent: "claude"})
	// Before any section opened there is nothing to nest under: a refusal by name passes through
	// for the dispatcher's plain ✗ line, exactly like a usage error.
	early := errors.New("the repository cannot be mounted at /tmp")
	if got := captureStderr(t, func() {
		if again := s.failed(early); again != early {
			t.Error("a failure before any section must pass through untouched")
		}
	}); got != "" {
		t.Fatalf("a failure before any section rendered %q", got)
	}
	var err error
	got := captureStderr(t, func() {
		s.starting()
		err = s.failed(errors.New("network gateway did not become ready; no agent started"))
	})
	want := "Starting Claude Code\n  ✗ Could not start Claude Code\n\n        network gateway did not become ready; no agent started\n"
	if got != want {
		t.Fatalf("failure rendered %q, want %q", got, want)
	}
	if !errors.Is(err, ui.ErrReported) {
		t.Fatal("a rendered failure must come back marked reported")
	}
	got = captureStderr(t, func() {
		if again := s.failed(err); again != err {
			t.Error("a reported error must pass through untouched")
		}
		if cancelled := s.failed(interruptedRun{}); errors.Is(cancelled, ui.ErrReported) {
			t.Error("a cancellation is not a failure to explain")
		}
	})
	if got != "" {
		t.Fatalf("printed again: %q", got)
	}
}

// Interrupted is the whole error being a stop request: the run's interruption or a canceled setup
// step, alone or joined only with each other. A cleanup failure joined to it, a message that wraps
// it, a deadline or a bare error are something else, and an editor child still prints those.
func TestInterruptedIsOnlyAStopRequest(t *testing.T) {
	canceled := ctxStep(canceledContext(), "sibling services")
	expired, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	cleanup := errors.New("filesystem custody retained at /state/coop-fs-1: remove helper: permission denied")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the run's interruption", interruptedRun{}, true},
		{"the interruption after a clean teardown", errors.Join(interruptedRun{}, nil), true},
		{"a setup step the stop cut short", canceled, true},
		{"both", errors.Join(canceled, interruptedRun{}), true},
		{"the interruption and a failed cleanup", errors.Join(interruptedRun{}, cleanup), false},
		{"a message wrapping the interruption", fmt.Errorf("stop native broker: %w", interruptedRun{}), false},
		{"a setup step past its deadline", ctxStep(expired, "argument assembly"), false},
		{"a bare cancellation", context.Canceled, false},
		{"no error", nil, false},
	} {
		if got := Interrupted(tc.err); got != tc.want {
			t.Errorf("%s: Interrupted(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
	if canceled.Error() != "box setup canceled before sibling services: context canceled" || !errors.Is(canceled, context.Canceled) {
		t.Errorf("setup cancellation reads %q", canceled)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// The stop line is truthful: a signal is named only when one arrived, and an exit status is
// reported as the number it was — 130 is not evidence of Ctrl-C.
func TestStopReasonNeverInfersASignalFromAnExitStatus(t *testing.T) {
	if got := stopReason(130, nil, nil); got != "main process exited with status 130" {
		t.Fatalf("exit 130 read as %q", got)
	}
	if got := stopReason(0, nil, &hostInterrupt{}); got != "main process exited with status 0" {
		t.Fatalf("no signal read as %q", got)
	}
	if got := stopReason(-1, interruptedRun{}, &hostInterrupt{sig: os.Interrupt}); got != "interrupted by Ctrl-C" {
		t.Fatalf("SIGINT read as %q", got)
	}
	if got := stopReason(-1, interruptedRun{}, &hostInterrupt{sig: syscall.SIGTERM}); got != "stopped by SIGTERM" {
		t.Fatalf("SIGTERM read as %q", got)
	}
	if got := stopReason(-1, interruptedRun{}, &hostInterrupt{sig: syscall.SIGHUP}); got != "stopped by SIGHUP" {
		t.Fatalf("SIGHUP read as %q", got)
	}
	if got := stopReason(-1, errors.New("restricted network health lost: probe\nsecond line"), nil); got != "restricted network health lost: probe" {
		t.Fatalf("failure read as %q", got)
	}
	// The client's own statuses are not claimed as the main process's: a daemon that refused the
	// run returns 125 with no box ever started, and a workload can return 125 too.
	for _, code := range []int{125, 126, 127} {
		if got := stopReason(code, nil, nil); got != fmt.Sprintf("the runtime returned status %d (its own error, or the main process's)", code) {
			t.Fatalf("exit %d read as %q", code, got)
		}
	}
}

// The recorder turns the first host signal into the cancellation cleanup acts on AND remembers
// which signal it was.
func TestHostInterruptRecordsTheSignalAndCancels(t *testing.T) {
	ctx, interrupt := newHostInterrupt()
	if interrupt.reason() != "" || ctx.Err() != nil {
		t.Fatal("nothing arrived yet")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the run")
	}
	if got := interrupt.reason(); got != "interrupted by Ctrl-C" {
		t.Fatalf("reason = %q", got)
	}
}

// A closed terminal, a kill and Ctrl-C all stop a launch the same way: the context cancels so cleanup
// runs, and the recorder names which signal it was and the status a shell gives for it. A terminal
// closing used to kill Coop where it stood, with the box's gateway and volumes still running.
func TestEverySignalAPersonSendsAStopRecordsItself(t *testing.T) {
	for _, tc := range []struct {
		sig    syscall.Signal
		reason string
		status int
	}{
		{syscall.SIGINT, "interrupted by Ctrl-C", 130},
		{syscall.SIGHUP, "stopped by SIGHUP", 129},
		{syscall.SIGTERM, "stopped by SIGTERM", 143},
	} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			ctx, interrupt, stop := listenForInterrupt(context.Background())
			defer stop()
			if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not cancel the launch", tc.sig)
			}
			if interrupt.reason() != tc.reason || interrupt.exitCode() != tc.status {
				t.Fatalf("recorded %q with status %d, want %q with %d", interrupt.reason(), interrupt.exitCode(), tc.reason, tc.status)
			}
		})
	}
}

// UntilInterrupted gives the work before a launch the same stop: the step's context cancels on the
// signal and the caller gets the signal's status. A step nothing interrupts reports none, and the
// signals take their default action again afterwards, so a Ctrl-C in the stretches no handler covers
// still ends Coop as it always did, instead of being swallowed by a handler nobody reads.
func TestUntilInterruptedStopsItsStepAndHandsTheSignalsBack(t *testing.T) {
	for _, tc := range []struct {
		sig    syscall.Signal
		status int
	}{{syscall.SIGINT, 130}, {syscall.SIGHUP, 129}, {syscall.SIGTERM, 143}} {
		t.Run(tc.sig.String(), func(t *testing.T) {
			var seen error
			status, err := UntilInterrupted(func(ctx context.Context) error {
				if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
					return err
				}
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
					t.Error("the signal did not cancel the step")
				}
				seen = ctx.Err()
				return seen
			})
			if status != tc.status || !errors.Is(seen, context.Canceled) || !errors.Is(err, context.Canceled) {
				t.Fatalf("UntilInterrupted = (%d, %v) with step context %v, want status %d and a cancellation", status, err, seen, tc.status)
			}
		})
	}
	sentinel := errors.New("step failed")
	if status, err := UntilInterrupted(func(context.Context) error { return sentinel }); status != 0 || !errors.Is(err, sentinel) {
		t.Fatalf("an uninterrupted step = (%d, %v), want its own error and no status", status, err)
	}
}

// Once UntilInterrupted returns, a hangup ends the process the way it does with no handler: the
// handler it registered is gone. Checked in a child, because the default action is death.
func TestUntilInterruptedLeavesNoHandlerBehind(t *testing.T) {
	if os.Getenv("COOP_TEST_UNTIL_INTERRUPTED") == "child" {
		_, _ = UntilInterrupted(func(context.Context) error { return nil })
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		time.Sleep(10 * time.Second)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestUntilInterruptedLeavesNoHandlerBehind$")
	cmd.Env = append(os.Environ(), "COOP_TEST_UNTIL_INTERRUPTED=child")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the child survived a hangup after UntilInterrupted returned: %v", err)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGHUP {
		t.Fatalf("the child ended %v, want killed by SIGHUP", exit)
	}
}

// A stop during the host check is no verdict on the host: it comes back as the cancellation, with no
// pass or failure to print. A check that reached a verdict first keeps it, and so does one nobody
// stopped.
func TestSetupStoppedIsOnlyAStopThatBeatTheVerdict(t *testing.T) {
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	failure := errors.New("the setup check did not finish")
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		code    int
		err     error
		wantErr bool
	}{
		{"stopped before it ran to the end", stopped, 0, failure, true},
		{"stopped with a failing check", stopped, 33, nil, true},
		{"passed before the stop landed", stopped, 0, nil, false},
		{"failed with nobody stopping it", context.Background(), 0, failure, false},
		{"a check that failed by itself", context.Background(), 33, nil, false},
	} {
		got := setupStopped(tc.ctx, tc.code, tc.err)
		if (got != nil) != tc.wantErr || got != nil && !errors.Is(got, context.Canceled) {
			t.Errorf("%s: setupStopped = %v, want a cancellation: %v", tc.name, got, tc.wantErr)
		}
	}
}

// The main process is "reached" only on the daemon's evidence: a launch the fixture never
// starts leaves started() false, and a normal one sets it before the exit status comes back.
func TestFilteredStartedFollowsDaemonEvidence(t *testing.T) {
	f, d := filteredFixture(t)
	d.corruptRole = "agent"
	if _, err := f.launch(context.Background(), RunSpec{}, nil, nil, io.Discard, io.Discard); err == nil || f.started() {
		t.Fatal("a launch that never started its workload reported a main process", err)
	}
	f, _ = filteredFixture(t)
	if code, err := f.launch(context.Background(), RunSpec{Cmd: []string{"fixture"}}, nil, nil, io.Discard, io.Discard); err != nil || code != 7 || !f.started() {
		t.Fatal("a normal launch must record its main process", code, err)
	}
}

// Acceptance: after cleanup seals the receipt, the interactive box prints the SAME projection
// `coop net inspect` prints — fed from the record it already holds — under the `coop:` anchor.
// Off a terminal the bytes are plain. The box prints the shared projection in its
// INLINE form: `Networking stats:` instead of a run id its reader never chose,
// and each destination's own totals on its row — the same renderer, same
// aggregate, same exceptions, same footer.
func TestInlineRunResultIsTheStandaloneProjection(t *testing.T) {
	f, _ := filteredFixture(t)
	if _, err := f.launch(context.Background(), RunSpec{Cmd: []string{"fixture"}}, nil, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cleanup("exited"); err != nil {
		t.Fatal(err)
	}
	got := captureStderr(t, f.printRun)
	inspection, err := networkstate.InspectExecution(f.record, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	var inline bytes.Buffer
	networkreport.WriteRun(&inline, ui.Palette{}, networkreport.View{ID: f.record.ID, Inline: true}, inspection)
	// One blank line separates the box's stop sentence from the stats block.
	if got != "\n"+inline.String() {
		t.Fatalf("inline result:\n%s\nis not the shared projection's inline form:\n%s", got, inline.String())
	}
	var standalone bytes.Buffer
	networkreport.WriteRun(&standalone, ui.Palette{}, networkreport.View{ID: f.record.ID}, inspection)
	_, inlineBody, _ := strings.Cut(inline.String(), "\n")
	_, standaloneBody, _ := strings.Cut(standalone.String(), "\n")
	if !strings.HasSuffix(inlineBody, "\nFull details: coop net inspect "+f.record.ID+" --json\n") ||
		!strings.HasSuffix(standaloneBody, "\nFull details: coop net inspect "+f.record.ID+" --json\n") {
		t.Errorf("the two views do not share one footer:\n%s\n%s", inlineBody, standaloneBody)
	}
	for _, want := range []string{"Networking stats:\n", "\n  Allowed  "} {
		if !strings.Contains(got, want) {
			t.Errorf("inline result is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Network run ") {
		t.Errorf("the inline summary repeats an opaque run id in its heading:\n%s", got)
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "nothing was refused") {
		t.Fatalf("inline result carries ANSI off a terminal, or the retired refusal-only summary:\n%s", got)
	}
}

// The open path narrates too: a raw interactive command gets the sections, then exactly one
// truthful stop line with the status the box's main process exited with — and a batch run of
// the same command prints none of it.
func TestRunNarratesAnInteractiveOpenLaunchAndItsStop(t *testing.T) {
	repo := t.TempDir()
	recorder := filepath.Join(t.TempDir(), "runtime-args")
	shim := filepath.Join(t.TempDir(), "rt")
	script := "#!/bin/sh\necho \"$@\" >> " + strconv.Quote(recorder) + "\ncase \"$1\" in run) exit 3 ;; esac\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), HomeInBox: "/home/node", Egress: "open"}
	spec := RunSpec{Image: "i", Repo: repo, Workdir: "/workspace", Cmd: []string{"sh", "-c", "exit 3"}}
	var code int
	var err error
	got := captureStderr(t, func() { code, err = Run(cfg, runtime.Runtime{Name: shim}, spec) })
	if err != nil || code != 3 {
		t.Fatalf("Run = %d, %v; want the box's own exit 3", code, err)
	}
	// This fake Docker cannot run kernel protection, so the launch says what mount protection leaves out.
	want := "Protecting secrets\n  ✓ No secret paths to hide\n" + mountProtectionLine +
		"\nConfiguring network access\n  ⚠ Unrestricted: nothing is blocked\n" +
		"\nStarting sh\n" +
		"\nThe Coop box has stopped: main process exited with status 3.\n"
	if got != want {
		t.Fatalf("interactive open run narrated:\n%q\nwant:\n%q", got, want)
	}
	spec.Batch = true
	if got := captureStderr(t, func() { _, _ = Run(cfg, runtime.Runtime{Name: shim}, spec) }); got != "" {
		t.Fatalf("a batch run printed %q", got)
	}
	// A client that cannot start is a launch failure: the nested red result under `Starting`,
	// no stop line, and the error comes back marked reported for the dispatcher.
	spec.Batch = false
	got = captureStderr(t, func() { code, err = Run(cfg, runtime.Runtime{Name: filepath.Join(t.TempDir(), "absent")}, spec) })
	if code != -1 || !errors.Is(err, ui.ErrReported) {
		t.Fatalf("Run = %d, %v; want -1 and a reported error", code, err)
	}
	if !strings.Contains(got, "\nStarting sh\n  ✗ Could not start sh\n\n        ") || strings.Contains(got, "has stopped") {
		t.Fatalf("a client that never started narrated:\n%q", got)
	}
	// An image with something to say opens the narration with its own section — a caution, not a
	// `coop:` line — and an age-only nudge stays a suggestion.
	StampImageMeta(cfg, "i", "v1")
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(cfg.BoxHome, "image-meta", "i"), old, old); err != nil {
		t.Fatal(err)
	}
	got = captureStderr(t, func() { _, _ = Run(cfg, runtime.Runtime{Name: shim}, spec) })
	if !strings.HasPrefix(got, "Checking the Coop box\n  ⚠ box image is 40 days old — 'coop update' rebuilds it on the newest OS packages and Node\n\nProtecting secrets\n") {
		t.Fatalf("an old image narrated:\n%q", got)
	}
}

// Offline and open runs end in the same sentence as a filtered one: the stop line carries the status
// the box's main process exited with. On a host with kernel filesystem protection, where coop removes
// these boxes itself, neither printed it (retireFilesystem); this is the shim-run side of that.
func TestRunNarratesAnInteractiveOfflineLaunchAndItsStop(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "rt")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1\" in run) exit 3 ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ egress, network string }{
		{"none", "⚠ Offline: nothing outside the box can be reached"},
		{"open", "⚠ Unrestricted: nothing is blocked"},
	} {
		t.Run(test.egress, func(t *testing.T) {
			cfg := &config.Config{ConfigDir: t.TempDir(), BoxHome: t.TempDir(), HomeInBox: "/home/node", Egress: test.egress}
			spec := RunSpec{Image: "i", Repo: t.TempDir(), Workdir: "/workspace", Cmd: []string{"sh", "-c", "exit 3"}}
			var code int
			var err error
			got := captureStderr(t, func() { code, err = Run(cfg, runtime.Runtime{Name: shim}, spec) })
			if err != nil || code != 3 {
				t.Fatalf("Run = %d, %v; want the box's own exit 3", code, err)
			}
			want := "Protecting secrets\n  ✓ No secret paths to hide\n" + mountProtectionLine +
				"\nConfiguring network access\n  " + test.network + "\n" +
				"\nStarting sh\n" +
				"\nThe Coop box has stopped: main process exited with status 3.\n"
			if got != want {
				t.Fatalf("interactive %s run narrated:\n%q\nwant:\n%q", test.egress, got, want)
			}
		})
	}
}

// A box that coop removes itself (kernel filesystem protection on an open or offline run) has not
// stopped until that removal is confirmed, so the run holds the stop line back and says it only
// then. Docker's inventory running out of time keeps the protection but not the box, so that stays
// one warning and a true stop; any other failure is the run's, and no stop is claimed over it. A run
// whose main process never started has no stop to name.
func TestAnOpenOrOfflineBoxCoopRemovesNamesItsStopOnlyOnceItIsGone(t *testing.T) {
	const stop = "\nThe Coop box has stopped: main process exited with status 0.\n"
	const kept = "⚠ Coop could not check every Docker container in time, so this box's filesystem protection stays until the next run in this project removes it\n"
	failed := errors.New("filesystem consumer still present")
	for _, test := range []struct {
		name        string
		stopped     string
		closeErr    error
		wantOut     string
		wantFailure error
	}{
		{name: "removed", stopped: "main process exited with status 0", wantOut: stop},
		{name: "protection kept", stopped: "main process exited with status 0", closeErr: fmt.Errorf("check consumers: %w", runtime.ErrConsumerInventoryUnfinished), wantOut: kept + stop},
		{name: "removal failed", stopped: "main process exited with status 0", closeErr: failed, wantFailure: failed},
		{name: "never started", closeErr: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			sections := newLaunchSections(RunSpec{Agent: "claude"})
			var closed, failure error
			var closes int
			out := captureStderr(t, func() {
				closed, failure = retireFilesystem(func() error { closes++; return test.closeErr }, sections, test.stopped)
			})
			if closes != 1 || closed != test.closeErr || failure != test.wantFailure {
				t.Fatalf("closes %d, closed %v, failure %v; want 1, %v, %v", closes, closed, failure, test.closeErr, test.wantFailure)
			}
			if out != test.wantOut {
				t.Fatalf("retiring the protection printed %q, want %q", out, test.wantOut)
			}
		})
	}
}

// A filtered launch starts its approved services just before the box, so an interactive one names
// them ahead of the box's own section; a loop narrates them as they start instead.
func TestFilteredLaunchNamesTheServicesItStarts(t *testing.T) {
	got := captureStderr(t, func() {
		s := newLaunchSections(RunSpec{Agent: "claude", AgentCommand: true})
		s.servicesWithBox([]string{"db", "redis\x1b[31m"})
		s.starting()
	})
	if !strings.HasPrefix(got, "Starting project services\n  db\n  redis\n") || !strings.Contains(got, "Starting Claude") ||
		strings.Index(got, "Starting project services") > strings.Index(got, "Starting Claude") {
		t.Fatalf("interactive filtered launch = %q", got)
	}
	got = captureStderr(t, func() {
		newLaunchSections(RunSpec{Agent: "claude", AgentCommand: true, Batch: true, LoopPresentation: true}).servicesWithBox([]string{"db"})
	})
	if got != "" {
		t.Fatalf("a loop launch named its services ahead of their start: %q", got)
	}
}

const mountProtectionLine = "  Secrets saved after the box starts stay readable to it. `coop doctor` explains why this launch uses mount protection.\n"

// A launch on kernel protection hides a secret saved later too, so only a mount-protected one says
// what it leaves out.
func TestOnlyMountProtectionSaysWhatItLeavesOut(t *testing.T) {
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Cmd: []string{"sh"}}).mountProtection() }); got != mountProtectionLine {
		t.Fatalf("mount protection said %q", got)
	}
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Cmd: []string{"sh"}, Quiet: true}).mountProtection() }); got != "" {
		t.Fatalf("a quiet launch said %q", got)
	}
}
