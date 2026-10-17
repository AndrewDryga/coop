package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// sweepRuntime is a container runtime holding one unlabeled coop box, recording every call. The
// orphan sweep's judgment is proven in internal/box (SurveyOrphanBoxes' decision table); what these
// tests pin is the WIRING: how often it runs, and that doctor only ever looks. Only the box survey's
// one listing of every coop container finds the box; the services listing finds nothing.
func sweepRuntime(t *testing.T) (containerruntime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	cli := filepath.Join(dir, "runtime")
	events := filepath.Join(dir, "events")
	if err := os.WriteFile(cli, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
case "$*" in
	"`+boxSurveyListing+`"*) printf '%s\n' '["legacy-box","box","","",""]' ;;
esac
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // the sweep settles filtered runs: never the host's records
	return containerruntime.Runtime{Name: cli}, events
}

// boxSurveyListing starts the one runtime call box.SurveyOrphanBoxes makes: every container carrying
// the coop label, whatever its kind.
const boxSurveyListing = "ps -a --no-trunc --filter label=coop --format "

func sweepEvents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// Multiple starts through one app would otherwise re-ask the runtime for the same repo's boxes.
// One sweep per repo per process.
func TestSweepOrphanBoxesRunsOncePerRepo(t *testing.T) {
	rt, events := sweepRuntime(t)
	repo, other := t.TempDir(), t.TempDir()
	a := &app{cfg: &config.Config{}, rt: rt, rtSet: true}
	a.sweepOrphanBoxes(repo)
	a.sweepOrphanBoxes(repo)
	a.sweepOrphanBoxes(other)
	if got := strings.Count(sweepEvents(t, events), boxSurveyListing); got != 2 {
		t.Fatalf("orphan listings = %d, want one per repo:\n%s", got, sweepEvents(t, events))
	}
	// Networks are not per repo: one listing per process, however many repos start.
	if got := strings.Count(sweepEvents(t, events), "network ls -q --filter label=com.docker.compose.project\n"); got != 1 {
		t.Fatalf("network listings = %d, want one per process:\n%s", got, sweepEvents(t, events))
	}
}

// doctor reports the box it cannot attribute to anyone and removes nothing: a box with no
// supervisor label predates the label and may be someone's. A clean survey says nothing at all —
// a healthy inspection shows findings, not a ledger of successful lookups.
func TestDoctorReportsOrphanBoxesWithoutReaping(t *testing.T) {
	rt, events := sweepRuntime(t)
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: rt, rtSet: true}
	report := &doctorReport{}
	a.doctorReportOrphanBoxes(report, repo, "")
	out := captureStderr(t, func() { report.print() })
	for _, want := range []string{"Running boxes", "Could not identify the supervisor for 1 box", "legacy-box", "removing it manually"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor orphan report missing %q:\n%s", want, out)
		}
	}
	// The unattributed box is host hygiene, not a hole in the isolation: it must not change the
	// check tally the verdict reports.
	if got := report.tally(); got != (doctorTally{}) {
		t.Errorf("orphan survey entered the check tally: %+v", got)
	}
	if strings.Contains(sweepEvents(t, events), "rm ") {
		t.Fatalf("doctor removed a container:\n%s", sweepEvents(t, events))
	}
}

// Doctor attacks its own fixture, so it runs outside a project, and in a home folder that is a Git
// repository, which Coop refuses as one: it checks the shared image, never an image the working
// folder's .agent/Dockerfile names, and says it looked for no project's abandoned boxes.
func TestDoctorRunsWithoutAProject(t *testing.T) {
	home, _ := gitrepo.New(t)
	t.Setenv("HOME", home)
	t.Chdir(home)
	writeTaskFile(t, filepath.Join(home, ".agent", "Dockerfile"), "FROM scratch\n")
	repo, withoutProject, err := doctorProject("")
	if err != nil || repo != "" || !strings.Contains(withoutProject, "at your home folder") {
		t.Fatalf("doctor's project from a home-folder repository = %q, %q, %v", repo, withoutProject, err)
	}
	cfg := &config.Config{BaseImage: "coop-box"}
	if img, _ := doctorImage(repo, cfg, func(string) bool { return true }); img != "coop-box" {
		t.Fatalf("doctor outside a project probes %q, want the shared image", img)
	}
	rt, events := sweepRuntime(t)
	a := &app{cfg: cfg, rt: rt, rtSet: true}
	report := &doctorReport{}
	a.doctorReportOrphanBoxes(report, repo, withoutProject)
	out := captureStderr(t, func() { report.print() })
	if !strings.Contains(out, "Not run in a project, so Coop did not look for its abandoned boxes") || !strings.Contains(out, "at your home folder") {
		t.Fatalf("doctor did not say it skipped the box survey:\n%s", out)
	}
	if got := sweepEvents(t, events); got != "" {
		t.Fatalf("doctor surveyed boxes outside a project:\n%s", got)
	}
	t.Chdir(t.TempDir())
	if _, withoutProject, err := doctorProject(""); err != nil || withoutProject != "This folder is not in a Git repository." {
		t.Fatalf("doctor's project outside any repository = %q, %v", withoutProject, err)
	}
}

// stubRecoverNetworkRuns stands in for network-run recovery and counts the calls made to it.
func stubRecoverNetworkRuns(t *testing.T, results []box.NetworkRecovery, err error) *int {
	t.Helper()
	calls := 0
	saved := recoverNetworkRuns
	recoverNetworkRuns = func(context.Context, containerruntime.Runtime, string) ([]box.NetworkRecovery, error) {
		calls++
		return results, err
	}
	t.Cleanup(func() { recoverNetworkRuns = saved })
	return &calls
}

// A filtered run whose coop was killed mid-teardown is settled by the next launch that asks — once
// per process, whichever path asks first — and only a run recovery settled whole counts as one.
func TestSettleInterruptedFilteredRunsOncePerProcess(t *testing.T) {
	calls := stubRecoverNetworkRuns(t, []box.NetworkRecovery{
		{RunID: "removed", Removed: []string{"guard", "controller", "ipc", "observations"}, Sealed: true},
		{RunID: "already-absent", Sealed: true},
		{RunID: "live", Skipped: "its supervisor is still running", Live: true},
		{RunID: "unproved", Pending: []string{"ipc (a container that could still use it is not proved gone)"}, Unverified: true},
		{RunID: "failed", Pending: []string{"guard"}, Failures: []error{errors.New("daemon hung up")}},
	}, nil)
	rt, _ := sweepRuntime(t)
	a := &app{cfg: &config.Config{}, rt: rt, rtSet: true}
	if got := a.settleInterruptedFilteredRuns(); got != 2 {
		t.Errorf("settled = %d, want 2: a live, unproved or failed run is not settled", got)
	}
	if got := a.settleInterruptedFilteredRuns(); got != 0 {
		t.Errorf("second settle = %d, want 0", got)
	}
	if got := a.collectOrphanBoxes(t.TempDir()).RecoveredFilteredRuns; got != 0 {
		t.Errorf("the sweep after a launch settled = %d, want 0", got)
	}
	if *calls != 1 {
		t.Fatalf("recovery ran %d times in one process, want once", *calls)
	}

	// A recovery that cannot read its records is housekeeping that failed, not the launch's error.
	calls = stubRecoverNetworkRuns(t, nil, errors.New("records unreadable"))
	a = &app{cfg: &config.Config{}, rt: rt, rtSet: true}
	if got := a.collectOrphanBoxes(t.TempDir()).RecoveredFilteredRuns; got != 0 || *calls != 1 {
		t.Fatalf("sweep with unreadable records = %d after %d calls, want 0 after 1", got, *calls)
	}
}

// A filtered launch settles what earlier killed runs left before its own box starts, and says so
// unless it runs quiet (an editor's child); a launch with no filtered capture never asks.
func TestRunBoxSettlesBeforeOnlyAFilteredLaunch(t *testing.T) {
	calls := stubRecoverNetworkRuns(t, []box.NetworkRecovery{{RunID: "removed", Sealed: true}}, nil)
	canceled, cancel := context.WithCancel(context.Background())
	cancel() // box.Run refuses a canceled launch before any host work, so the settle is all that runs
	launch := func(spec box.RunSpec) string {
		t.Helper()
		a := &app{cfg: &config.Config{}} // a fresh process each time: the settle is once per process
		return captureStderr(t, func() {
			if _, err := a.runBox(spec); err == nil || !strings.Contains(err.Error(), "canceled") {
				t.Errorf("runBox(%+v) = %v, want the canceled launch refused", spec, err)
			}
		})
	}
	if out := launch(box.RunSpec{Ctx: canceled}); *calls != 0 || out != "" {
		t.Fatalf("an unfiltered launch settled %d times and said %q, want neither", *calls, out)
	}
	filtered := box.RunSpec{Ctx: canceled, CapturedEgress: &box.CapturedEgress{}}
	if out := launch(filtered); *calls != 1 || !strings.Contains(out, "recovered 1 interrupted filtered run") {
		t.Fatalf("a filtered launch settled %d times and said %q, want once, noted", *calls, out)
	}
	filtered.Quiet = true
	if out := launch(filtered); *calls != 2 || out != "" {
		t.Fatalf("a quiet filtered launch settled %d times in total and said %q, want a second settle, silent", *calls, out)
	}
}

// A fork's review or merge gate is a filtered launch too. The CLI's gate settles once per process
// and says so; the session daemon's settles before every gate, quietly, on the runtime the gate runs
// on — it serves for days, so no single answer lasts it.
func TestForkGatesSettleInterruptedFilteredRuns(t *testing.T) {
	var runtimes []string
	calls := stubRecoverNetworkRuns(t, []box.NetworkRecovery{{RunID: "removed", Sealed: true}}, nil)
	stubbed := recoverNetworkRuns
	recoverNetworkRuns = func(ctx context.Context, rt containerruntime.Runtime, runID string) ([]box.NetworkRecovery, error) {
		runtimes = append(runtimes, rt.Name)
		return stubbed(ctx, rt, runID)
	}
	gateRuntime := containerruntime.Runtime{Name: "docker"}

	cli := (&app{cfg: &config.Config{}, rt: gateRuntime, rtSet: true}).forkHost()
	out := captureStderr(t, func() {
		cli.SettleFilteredRuns(gateRuntime)
		cli.SettleFilteredRuns(gateRuntime)
	})
	if *calls != 1 || !strings.Contains(out, "recovered 1 interrupted filtered run") {
		t.Fatalf("the CLI's gates settled %d times and said %q, want once, noted", *calls, out)
	}

	daemon := sessionReviewGateHost(&config.Config{}, containerruntime.Runtime{}) // not yet detected
	out = captureStderr(t, func() {
		daemon.SettleFilteredRuns(gateRuntime)
		daemon.SettleFilteredRuns(gateRuntime)
	})
	if *calls != 3 || out != "" {
		t.Fatalf("after two daemon gates recovery ran %d times in all and said %q, want 3, silent", *calls, out)
	}
	if want := []string{"docker", "docker", "docker"}; !slices.Equal(runtimes, want) {
		t.Fatalf("recovery ran on runtimes %q, want the gate's each time: %q", runtimes, want)
	}
}

// Finding the interrupted runs reads every run record the host kept, so only a filtered launch —
// whose gateway costs far more — pays for it. An open or offline launch never touches them.
func TestOpenAndOfflineLaunchesNeverSettleFilteredRuns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	calls := stubRecoverNetworkRuns(t, []box.NetworkRecovery{{RunID: "removed", Sealed: true}}, nil)
	for _, egress := range []string{"open", "none"} {
		cfg := &config.Config{ConfigDir: t.TempDir(), HomeInBox: "/home/node", BaseImage: "coop-box", Homes: true, Egress: "open", RepoOverride: t.TempDir()}
		signInCred(t, cfg, "claude", "default")
		a := &app{cfg: cfg, rt: nativeOnlineRuntime(t, filepath.Join(t.TempDir(), "runtime-args")), rtSet: true}
		var code int
		var err error
		out := captureStderr(t, func() { code, err = a.cmdRun([]string{"--egress", egress, "--", "true"}) })
		if code != 0 || err != nil {
			t.Fatalf("coop run --egress %s = (%d, %v)\n%s", egress, code, err, out)
		}
	}
	if *calls != 0 {
		t.Fatalf("open and offline launches settled filtered runs %d times, want never", *calls)
	}
}
