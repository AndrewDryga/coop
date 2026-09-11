package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	want := "\nProtecting secrets\n" +
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
		if got != "\nConfiguring network access\n"+c.want {
			t.Errorf("%s: rendered %q, want %q", c.name, got, "\nConfiguring network access\n"+c.want)
		}
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
	if got := captureStderr(t, func() { newLaunchSections(RunSpec{Cmd: []string{"sh"}}).secrets(0) }); got != "\nProtecting secrets\n  ✓ No secret paths to hide\n" {
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
		s.box([]string{"box image is 40 days old — 'coop update' refreshes the agent CLIs baked into it"})
	})
	want := "\nChecking the Coop box\n  ⚠ box image is 40 days old — 'coop update' refreshes the agent CLIs baked into it\n"
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

// Loops, quiet probes and ACP children keep their bounded output: every narration call is a
// no-op and a failure comes back exactly as it was, for the caller's own reporting.
func TestLaunchSectionsAreSilentOutsideInteractiveRuns(t *testing.T) {
	for _, spec := range []RunSpec{{Batch: true}, {Quiet: true}, {ForceNoTTY: true}} {
		s := newLaunchSections(spec)
		cause := errors.New("no")
		got := captureStderr(t, func() {
			s.secrets(3)
			s.internet(&config.Config{Egress: "open"}, spec, nil)
			s.starting()
			s.stopping("main process exited with status 0")
			if err := s.failed(cause); err != cause {
				t.Errorf("%+v: failed() = %v, want the cause untouched", spec, err)
			}
		})
		if got != "" {
			t.Errorf("%+v printed %q", spec, got)
		}
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
	want := "\nStarting Claude Code\n  ✗ Could not start Claude Code\n\n        network gateway did not become ready; no agent started\n"
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
	want := "\nProtecting secrets\n  ✓ No secret paths to hide\n" +
		"\nConfiguring network access\n  ⚠ Unrestricted — nothing is blocked\n" +
		"\nStarting sh\n" +
		"stopping the box — main process exited with status 3\n"
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
	if !strings.Contains(got, "\nStarting sh\n  ✗ Could not start sh\n\n        ") || strings.Contains(got, "stopping the box") {
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
	if !strings.HasPrefix(got, "\nChecking the Coop box\n  ⚠ box image is 40 days old — 'coop update' refreshes the agent CLIs baked into it\n\nProtecting secrets\n") {
		t.Fatalf("an old image narrated:\n%q", got)
	}
}
