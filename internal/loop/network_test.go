package loop

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
)

func target(provider, account string) agents.Target {
	return agents.Target{Provider: provider, Accounts: []string{account}}
}

// The frozen policy has to cover every rung the run may rotate onto — including
// a review stage running on a DIFFERENT provider than the work loop. A rung
// admission never saw would meet a mid-drain denial instead of a launch refusal.
func TestNetworkAdmissionSpecCoversEveryLadderRung(t *testing.T) {
	cfg := &config.Config{Homes: true, Cache: true}
	work := ladder.NewRotation([]agents.Target{target("claude", "work"), target("claude", "personal")})
	signoff := ladder.NewRotation([]agents.Target{target("codex", "work")})
	verify := ladder.NewRotation([]agents.Target{target("gemini", "work")})

	spec := networkAdmissionSpec(cfg, "/repo", "img", "claude", nil, []agents.Target{target("grok", "work")}, work, signoff, nil, verify)

	if spec.Repo != "/repo" || spec.Image != "img" || spec.Agent != "claude" || !spec.Homes || !spec.Cache {
		t.Fatalf("admission spec = %+v, want the run's own repo/image/lead and mount toggles", spec)
	}
	var providers []string
	for _, peer := range spec.Peers {
		if !slices.Contains(providers, peer.Provider) {
			providers = append(providers, peer.Provider)
		}
	}
	slices.Sort(providers)
	if want := []string{"claude", "codex", "gemini", "grok"}; !slices.Equal(providers, want) {
		t.Errorf("credential scope = %q, want every rung and peer %q", providers, want)
	}
}

func TestDirectLoopAdmissionRecognizesBrokeredClaudeLadder(t *testing.T) {
	cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", Homes: true, Egress: "filtered"}
	if err := os.MkdirAll(filepath.Dir(cfg.EnvFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("ANTHROPIC_API_KEY=raw-provider-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := ladder.NewRotation([]agents.Target{target("claude", "default")})
	spec := networkAdmissionSpec(cfg, "/repo", "img", "claude", nil, nil, work)
	bundles, err := box.NetworkProviderBundles(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, bundle := range bundles {
		if bundle.Provider == "claude" {
			t.Fatalf("brokered loop admitted Claude directly: %#v", bundles)
		}
	}
}

// The report arrives from inside the launch, while the live bar owns the
// terminal. Nothing may print there: the block belongs between iterations.
func TestNetworkLogPrintsBetweenIterationsNotDuringOne(t *testing.T) {
	log := newNetworkLog()
	report := box.NetworkReport{
		RunID: "run-1",
		Denials: []box.NetworkDenial{
			{Destination: "a.example", Basis: "dns", Count: 4},
			{Destination: "b.example", Basis: "tls", Count: 1},
		},
		Alerts: []string{"denial_burst (warning): open over 5000ms, threshold 20 denials"},
		Event:  "ev-9",
	}
	log.setStage("Task attempt 1")
	if during := captureStderr(t, func() { log.record(report) }); during != "" {
		t.Fatalf("recording a report printed %q while the bar was up", during)
	}
	out := captureStderr(t, log.finishIteration)
	for _, want := range []string{
		"⚠ Task attempt 1 could not reach 2 remote addresses",
		"a.example · DNS · blocked 4 times",
		"b.example · TLS · blocked 1 time",
		"The network filter raised an alert",
		"denial_burst (warning)",
		"Explain: coop net blocked a.example --run run-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("iteration block %q is missing %q", out, want)
		}
	}
	if log.runID() != "run-1" {
		t.Errorf("stage telemetry run id = %q, want run-1", log.runID())
	}
	if again := captureStderr(t, log.finishIteration); again != "" {
		t.Errorf("a second drain reprinted %q", again)
	}
	if id := log.runID(); id != "" {
		t.Errorf("run id after an iteration with no filtered box = %q, want empty", id)
	}
}

// Nothing refused and no alert is the ordinary case in an overnight drain. It
// must cost zero lines — per iteration and at the close.
func TestNetworkLogIsSilentWhenNothingWasRefused(t *testing.T) {
	log := newNetworkLog()
	log.record(box.NetworkReport{RunID: "run-1", Allowed: "allowed: 3 connections, 0 B sent, 0 B received"})
	if out := captureStderr(t, log.finishIteration); out != "" {
		t.Errorf("a clean iteration printed %q", out)
	}
	if out := captureStderr(t, log.summary); out != "" {
		t.Errorf("a clean run printed a closing summary: %q", out)
	}
}

func TestNetworkLogSummaryRanksAndPointsAtTheReceipts(t *testing.T) {
	log := newNetworkLog()
	log.record(box.NetworkReport{RunID: "run-1", Denials: []box.NetworkDenial{
		{Destination: "a.example", Basis: "dns", Count: 2},
		{Destination: "b.example", Basis: "tls", Count: 9},
	}})
	log.finishIteration()
	log.record(box.NetworkReport{RunID: "run-2", Denials: []box.NetworkDenial{
		{Destination: "a.example", Basis: "dns", Count: 5},
		{Destination: "c.example", Basis: "dns", Count: 1},
		{Destination: "d.example", Basis: "dns", Count: 1},
	}, Alerts: []string{"denial_burst"}})
	log.finishIteration()

	out := captureStderr(t, log.summary)
	if !strings.Contains(out, "Traffic to 4 remote addresses was blocked across 2 network runs, with 1 alert") {
		t.Errorf("summary headline missing from %q", out)
	}
	// Ranked by count across the whole run, bounded to three, ties by first seen.
	for _, want := range []string{"b.example · blocked 9 times", "a.example · blocked 7 times", "c.example · blocked 1 time", "Run details: coop net runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary %q is missing %q", out, want)
		}
	}
	if strings.Contains(out, "d.example") {
		t.Errorf("summary %q listed more than the top three", out)
	}
	if again := captureStderr(t, log.summary); again != "" {
		t.Errorf("the closing summary printed twice: %q", again)
	}
}

// A long line of distinct denied names is exactly what a hostile agent can
// produce, so the iteration line names a few and counts the rest.
func TestNetworkIterationLineStaysBounded(t *testing.T) {
	report := box.NetworkReport{RunID: "r", Omitted: 3}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		report.Denials = append(report.Denials, box.NetworkDenial{Destination: name, Basis: "dns", Count: 1})
	}
	out := captureStderr(t, func() { printNetworkIteration(report, "Task attempt 7") })
	if !strings.Contains(out, "Task attempt 7 could not reach 8 remote addresses") {
		t.Errorf("block %q does not count the omitted destinations", out)
	}
	for _, want := range []string{"a · DNS", "b · DNS", "c · DNS", "… 5 more remote addresses"} {
		if !strings.Contains(out, want) {
			t.Errorf("block %q is missing %q", out, want)
		}
	}
	if strings.Contains(out, "d · DNS") {
		t.Errorf("block %q named more than three destinations", out)
	}
}

// Admission runs ONCE at the top of the run, and the capture it produced reaches
// every box the loop launches — the work iteration here, and the debug shell.
func TestEveryLoopLaunchCarriesTheRunsCapture(t *testing.T) {
	t.Setenv(tasks.TestLeaseAuthorityRootEnv, t.TempDir())
	repo := t.TempDir()
	root := filepath.Join(repo, tasksRoot)
	if err := os.MkdirAll(filepath.Join(root, stateTodo), 0o755); err != nil {
		t.Fatal(err)
	}
	capture := &box.CapturedEgress{Project: repo, Fingerprint: "fp"}
	var launched []box.RunSpec
	c := &Control{
		cfg: &config.Config{}, capture: capture, net: newNetworkLog(),
		boxRun: func(spec box.RunSpec) (int, error) { launched = append(launched, spec); return 0, nil },
	}
	if _, _, _, _, _, err := c.runIteration(context.Background(), repo, "img", "claude", "", []string{"true"},
		false, false, []string{root}, completionWindowStrict, nil, nil, false, io.Discard, nil, "work", "", nil); err != nil {
		t.Fatalf("iteration: %v", err)
	}
	c.debugShell(repo, "img", "claude", "")
	if len(launched) != 2 {
		t.Fatalf("launched %d boxes, want the iteration and the debug shell", len(launched))
	}
	for i, spec := range launched {
		if spec.CapturedEgress != capture {
			t.Errorf("launch %d ran outside the run's frozen policy (%#v)", i, spec.CapturedEgress)
		}
		if spec.OnNetworkReport == nil {
			t.Errorf("launch %d cannot report what it was refused", i)
		}
	}
}

// A run that never admitted (open egress) launches exactly as it does today:
// a nil capture, and a hook that is safe to call.
func TestLoopWithoutFilteredEgressLaunchesUnchanged(t *testing.T) {
	var spec box.RunSpec
	c := &Control{cfg: &config.Config{}, reuseServices: true, boxRun: func(s box.RunSpec) (int, error) { spec = s; return 0, nil }}
	if _, err := c.runBox(box.RunSpec{Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if spec.CapturedEgress != nil {
		t.Errorf("an open run carried a capture: %#v", spec.CapturedEgress)
	}
	if !spec.ReuseServices {
		t.Error("the loop did not pass its unchanged service setup to the work box")
	}
	spec.OnNetworkReport(box.NetworkReport{RunID: "r"}) // a nil log must not panic
}

// The loop admits before its first box and never again: a run whose admission
// fails stops there, having launched nothing.
func TestLoopRefusesAnUnqualifiedNetworkBeforeAnyBox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
	writeTaskFile(t, filepath.Join(repo, ".agent", "project.yaml"), "box:\n  egress: filtered\n")
	cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
	profileDir := cfg.AgentProfileDir("claude", "default")
	writeTaskFile(t, filepath.Join(profileDir, ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"]}}`)
	c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
	c.boxRun = func(box.RunSpec) (int, error) {
		t.Error("a box launched under a network the host never qualified")
		return 0, nil
	}

	code, err := c.Run(RunSpec{Repo: repo, Image: "img", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard})
	if code != 1 || err == nil {
		t.Fatalf("unadmitted filtered loop = (%d, %v), want a refusal", code, err)
	}
	// A filtered launch needs the qualified gateway; this fixture's runtime cannot
	// serve it, so admission refuses at the runtime preflight and names Docker.
	if !strings.Contains(err.Error(), "restricted networking needs docker") {
		t.Errorf("refusal %q does not name the real reason", err)
	}
}
