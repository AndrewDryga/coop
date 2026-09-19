package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/networkreport"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// captureStderr returns whatever fn writes to os.Stderr — the ui helpers emit there when no live
// sink owns the terminal. The test binary's stderr is not a terminal, so the bytes carry no ANSI.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	return string(out)
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
		s.internet(&config.Config{Egress: "filtered"}, spec, &policy)
		s.starting()
	})
	want := "Protecting secrets\n" +
		"  ✓ 8 secret paths hidden from the box\n" +
		"\nConfiguring network access\n" +
		"  ✓ OpenAI endpoints allowed\n" +
		"  ✓ Applied 3 approved network rules\n" +
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

// Every mode shares the heading; open and offline replace the green rows with one warning, and
// a raw run — no agent — is told what offline means for it without inventing a provider.
func TestInternetSectionIsOneHeadingInEveryMode(t *testing.T) {
	cases := []struct {
		name string
		spec RunSpec
		cfg  *config.Config
		want string
	}{
		{"open", RunSpec{Agent: "codex"}, &config.Config{Egress: "open"}, "  ⚠ Unrestricted — nothing is blocked\n"},
		{"offline agent", RunSpec{Agent: "codex"}, &config.Config{Egress: "none"}, "  ⚠ Offline — Codex cannot reach OpenAI\n"},
		{"offline raw", RunSpec{Cmd: []string{"true"}}, &config.Config{Egress: "none"}, "  ⚠ Offline — nothing outside the box can be reached\n"},
	}
	for _, c := range cases {
		got := captureStderr(t, func() { newLaunchSections(c.spec).internet(c.cfg, c.spec, nil) })
		if got != "Configuring network access\n"+c.want {
			t.Errorf("%s: rendered %q, want %q", c.name, got, "Configuring network access\n"+c.want)
		}
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
				"  The key stays on this computer. The agent can use the account but never see the key.\n"},
		{"two protected keys", []accountRow{{"gemini", "personal2", true}, {"codex", "work", true}},
			"Connecting accounts\n  ✓ Gemini (personal2) · API key protected\n  ✓ Codex (work) · API key protected\n\n" +
				"  Keys stay on this computer. Agents can use the accounts but never see the keys.\n"},
		{"a protected key and a login", []accountRow{{"gemini", "personal2", true}, {"claude", "work", false}},
			"Connecting accounts\n  ✓ Gemini (personal2) · API key protected\n  ✓ Claude (work) · Signed in\n\n" +
				"  Keys stay on this computer. Agents can use the accounts but never see the keys.\n"},
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
	signIn := cfg.AgentProfileDir("claude", cfg.ActiveProfile("claude"))
	if err := os.MkdirAll(signIn, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(signIn, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := RunSpec{Agent: "gemini", AgentCommand: true, Homes: true, Peers: []agents.Target{{Provider: "claude"}, {Provider: "codex"}}}
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

// "Everything else blocked" is only true once every grant has a row. A grant whose provenance
// the section does not recognize still gets counted rather than silently dropped, and a run
// with nothing to hide says so instead of inventing a count.
func TestNetworkAllowancesNeverOmitAGrant(t *testing.T) {
	policy := egress.Snapshot{Grants: []egress.Grant{
		grant("g1", "a.example", egress.Origin{Kind: "session"}),
		grant("g2", "b.example", egress.Origin{Kind: "provider", Provider: "claude"}, egress.Origin{Kind: "project"}),
	}}
	rows := networkAllowances(policy)
	want := []string{"Anthropic endpoints allowed", "Applied 1 approved network rule", "1 other destination allowed"}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("allowances = %q, want %q", rows, want)
	}
	// A server reached through the credential broker has no grant of its own, and is still allowed.
	policy.Grants = append(policy.Grants, grant("g3", "docs.example", egress.Origin{Kind: "mcp", Name: "docs"}))
	if rows := networkAllowances(policy, "emisar"); !slices.Contains(rows, "2 configured MCP services allowed") {
		t.Fatalf("allowances with a brokered MCP server = %q", rows)
	}
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Cmd: []string{"sh"}}).secrets(0) }); got != "Protecting secrets\n  ✓ No secret paths to hide\n" {
		t.Fatalf("zero secrets rendered %q", got)
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

// Ordinary batch runs, quiet probes and ACP children keep their bounded output: every narration
// call is a no-op and a failure comes back exactly as it was, for the caller's own reporting.
func TestLaunchSectionsAreSilentOutsideInteractiveRuns(t *testing.T) {
	for _, spec := range []RunSpec{{Batch: true}, {Quiet: true}, {ForceNoTTY: true}} {
		spec.StartingNotice = "must remain silent"
		s := newLaunchSections(spec)
		cause := errors.New("no")
		got := captureStderr(t, func() {
			s.secrets(3)
			s.internet(&config.Config{Egress: "open"}, spec, nil)
			s.starting()
			s.stopping()()
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
		s.internet(&config.Config{Egress: "none"}, spec, nil)
		s.servicesFailed("compose up exited with status 1")
		s.starting()
	})
	want := "Preparing task environment\n" +
		"  Protecting secrets\n" +
		"  ✓ 8 secret paths hidden from the box\n" +
		"\n  Configuring network access\n" +
		"  ⚠ Offline — Claude cannot reach Anthropic\n" +
		"\n  Starting services\n" +
		"  ⚠ Services failed to start · exit 1\n" +
		"    Continuing without sibling services.\n" +
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
		newLaunchSections(raw).internet(&config.Config{Egress: "none"}, raw, nil)
	})
	if got != "Preparing task environment\n  Configuring network access\n  ⚠ Offline — nothing outside the box can be reached\n" {
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
		"    Continuing without sibling services.",
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
	for _, want := range []string{"Some services failed to start", "Continuing with observed service(s): db.", "To retry: coop up"} {
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
		func(s *launchSections) { s.servicesSkipped("inspection unavailable") },
	} {
		got := captureStderr(t, func() {
			s := newLaunchSections(RunSpec{Agent: "gemini"})
			s.internet(&config.Config{Egress: "open"}, RunSpec{Agent: "gemini"}, nil)
			report(s)
		})
		if !strings.HasPrefix(got, "Configuring network access\n  ⚠ Unrestricted — nothing is blocked\n\n⚠ Project services") {
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
		s.servicesSkipped("inspection failed for " + long)
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
	want := "Protecting secrets\n  ✓ No secret paths to hide\n\nConfiguring network access\n  ⚠ Unrestricted — nothing is blocked\n\nStarting Grok\nFollow Grok's sign-in instructions below\nPROVIDER OUTPUT\n\nThe Coop box has stopped — main process exited with status 0.\n"
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
	early := errors.New("a readonly run cannot mount /tmp as the repository")
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
	want := "Protecting secrets\n  ✓ No secret paths to hide\n" +
		"\nConfiguring network access\n  ⚠ Unrestricted — nothing is blocked\n" +
		"\nStarting sh\n" +
		"\nThe Coop box has stopped — main process exited with status 3.\n"
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
