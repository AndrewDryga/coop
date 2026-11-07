package forkctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
)

// settleFixture is a killed fork loop's leftovers for coop fork stop: the dead worker's record, a fake
// runtime that logs each call to the events file, and a settle seam that logs its own call there too, so
// the order of the two shows in one place.
type settleFixture struct {
	repo, workspace, events string
	control                 *Control
	asked                   *[]string // the workspaces the stop asked to settle
}

func newSettleFixture(t *testing.T, settle func(ctx context.Context, workspace string) (int, error)) settleFixture {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	events := filepath.Join(dir, "events")
	runtimeCLI := filepath.Join(dir, "runtime")
	identity := bindTestFork(t, repo, "perf")
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "perf", deadWorkerOf(identity)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = ps ]; then printf '%s\n' box-perf; fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	var asked []string
	return settleFixture{
		repo: repo, workspace: forkspace.Workspace(repo, "perf"), events: events, asked: &asked,
		control: &Control{
			cfg: &config.Config{RepoOverride: repo},
			rt:  containerruntime.Runtime{Name: runtimeCLI},
			settleRuns: func(ctx context.Context, rt containerruntime.Runtime, workspace string) (int, error) {
				if rt.Name != runtimeCLI {
					t.Errorf("the stop settled on runtime %q, want the one it reaped on", rt.Name)
				}
				if _, bounded := ctx.Deadline(); !bounded {
					t.Error("the stop settled without a deadline")
				}
				asked = append(asked, workspace)
				f, err := os.OpenFile(events, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(f, "settle %s\n", workspace)
				f.Close()
				return settle(ctx, workspace)
			},
		},
	}
}

// A worker killed with kill -9 leaves its agent box, which carries the fork's labels, and the gateway
// pair, volumes and filesystem helper of its filtered runs, which carry the run's own. Stop reaps the
// box and then settles the fork's workspace, so nothing of the killed run keeps running until the next
// launch finds it.
func TestForkStopSettlesTheRunsAKilledWorkerLeft(t *testing.T) {
	fx := newSettleFixture(t, func(context.Context, string) (int, error) { return 1, nil })
	var code int
	var err error
	stderr := captureStderr(t, func() { code, err = fx.control.ForkStop([]string{"perf"}) })
	if code != 0 || err != nil {
		t.Fatalf("ForkStop after a kill -9 = (%d, %v), want success", code, err)
	}
	data, readErr := os.ReadFile(fx.events)
	if readErr != nil {
		t.Fatal(readErr)
	}
	generation := bindTestFork(t, fx.repo, "perf").Generation
	want := "ps -q -a " + reapFilters(fx.repo, "perf", generation) + "\nrm -f box-perf\nsettle " + fx.workspace + "\n"
	if string(data) != want {
		t.Errorf("stop's calls = %q, want the box reaped, then the workspace's runs settled: %q", data, want)
	}
	if !strings.Contains(stderr, "recovered 1 interrupted filtered run") || strings.Contains(stderr, "could not finish") {
		t.Errorf("stop did not say what it settled:\n%s", stderr)
	}
	if pathExists(forkspace.PidPath(fx.repo, "perf")) {
		t.Error("the worker record survived a stop that settled everything")
	}
}

// A stop that could not finish a run says which fork it was and the command that finishes it. The
// fork's own records are clean by then, so the stop still ends: nothing it could retry remains.
func TestForkStopNamesTheRunsItCouldNotSettle(t *testing.T) {
	cause := "controller: Docker container cleanup remains pending"
	fx := newSettleFixture(t, func(context.Context, string) (int, error) { return 0, errors.New(cause) })
	var code int
	var err error
	stderr := captureStderr(t, func() { code, err = fx.control.ForkStop([]string{"perf"}) })
	if code != 0 || err != nil {
		t.Fatalf("ForkStop with unsettled runs = (%d, %v), want the stop itself to end", code, err)
	}
	for _, want := range []string{"Coop could not finish removing the network containers of fork perf", cause, "coop net recover", "Fork perf stopped"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stop's output lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "recovered") {
		t.Errorf("stop claimed a recovery it did not make:\n%s", stderr)
	}
	if pathExists(forkspace.PidPath(fx.repo, "perf")) {
		t.Error("the stop left the worker record behind")
	}
}

// A fork with no worker record at all, whose loop ran in the foreground and was killed, has only its
// dead sandbox record. Stop reaps its box by that record and settles the workspace's runs the same way.
func TestForkStopSettlesTheRunsOfAForegroundLoopThatWasKilled(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	identity := bindTestFork(t, repo, "perf")
	workspace := forkspace.Workspace(repo, "perf")
	stale := forkspace.ExecutionRecord{
		Version: 1, ID: strings.Repeat("c", 32), Kind: forkspace.ExecutionForkLoop,
		Role: forkspace.ExecutionRoleSandbox, Workspace: workspace, Fork: &identity,
		SourceID: "dead-loop", PID: 2147483646, Token: starttoken.Stale(), StartedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	executions := filepath.Join(forkspace.StateDir(repo), "executions")
	if err := os.MkdirAll(executions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(executions, stale.ID+".json"), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var asked []string
	c := &Control{
		cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI},
		settleRuns: func(_ context.Context, _ containerruntime.Runtime, ws string) (int, error) {
			asked = append(asked, ws)
			return 2, nil
		},
	}
	var code int
	stderr := captureStderr(t, func() { code, err = c.ForkStop([]string{"perf"}) })
	if code != 0 || err != nil {
		t.Fatalf("ForkStop of a killed foreground loop = (%d, %v)", code, err)
	}
	if len(asked) != 1 || asked[0] != workspace {
		t.Errorf("settled workspaces = %v, want only %s", asked, workspace)
	}
	if !strings.Contains(stderr, "recovered 2 interrupted filtered runs") {
		t.Errorf("stop did not say what it settled:\n%s", stderr)
	}
}

// A stop that finds nothing of the fork running has nothing to settle and no reason to ask the runtime.
func TestForkStopOfAForkThatIsNotRunningSettlesNothing(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	bindTestFork(t, repo, "perf")
	c := &Control{
		cfg: &config.Config{RepoOverride: repo},
		host: Host{EnsureRuntime: func() (containerruntime.Runtime, error) {
			t.Error("stop consulted the runtime for a fork that is not running")
			return containerruntime.Runtime{}, errors.New("runtime must not be consulted")
		}},
		settleRuns: func(context.Context, containerruntime.Runtime, string) (int, error) {
			t.Error("stop settled the runs of a fork that is not running")
			return 0, nil
		},
	}
	var code int
	var err error
	stderr := captureStderr(t, func() { code, err = c.ForkStop([]string{"perf"}) })
	if code != 0 || err != nil || !strings.Contains(stderr, "Fork perf is not running.") {
		t.Fatalf("ForkStop of an idle fork = (%d, %v)\n%s", code, err, stderr)
	}
}
