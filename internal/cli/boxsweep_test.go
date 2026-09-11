package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
)

// sweepRuntime is a container runtime holding one unlabeled coop box, recording every call. The
// orphan sweep's judgment is proven in internal/box (SurveyOrphanBoxes' decision table); what these
// tests pin is the WIRING: how often it runs, and that doctor only ever looks.
func sweepRuntime(t *testing.T) (containerruntime.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	cli := filepath.Join(dir, "runtime")
	events := filepath.Join(dir, "events")
	if err := os.WriteFile(cli, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
case "$1" in
	ps) printf '%s\n' legacy-box ;;
	inspect) printf '%s\n' '{"coop":"box"}' ;;
esac
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	return containerruntime.Runtime{Name: cli}, events
}

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
	if got := strings.Count(sweepEvents(t, events), "ps -q -a --filter label=coop=box\n"); got != 2 {
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
	a.doctorReportOrphanBoxes(report)
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
