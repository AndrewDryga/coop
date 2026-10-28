package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestSummaryCountsAFailedLaunchWithoutTimingIt(t *testing.T) {
	seconds := func(s float64) *float64 { return &s }
	for _, tc := range []struct {
		rows []row
		want string
	}{
		{[]row{{Seconds: seconds(4)}, {}, {Seconds: seconds(2)}, {Seconds: seconds(3)}}, `{"n":3,"failed":1,"p50":3,"min":2,"max":4}`},
		{[]row{{Seconds: seconds(4)}, {Seconds: seconds(2.5)}}, `{"n":2,"failed":0,"p50":3.25,"min":2.5,"max":4}`},
		{[]row{{}}, `{"n":0,"failed":1,"p50":null,"min":null,"max":null}`},
	} {
		if got, _ := json.Marshal(summarize(tc.rows)); string(got) != tc.want {
			t.Errorf("summary = %s, want %s", got, tc.want)
		}
	}
}

// standIn is a stand-in coop: it records each call (where it ran, its arguments, and the project and
// auto-up it was given), and `run` does what the box would, or hangs after its marker, or talks a
// lot first.
const standIn = `#!/bin/sh
printf '%s|%s|%s|%s\n' "$(pwd -P)" "$*" "$COOP_REPO" "$COOP_AUTO_UP" >> "@LOG@"
[ "$1" = up ] && [ -n "$STAND_IN_UP_FAILS" ] && { echo "services did not start" >&2; exit 1; }
[ "$1" = run ] || exit 0
echo $$ > "@LOG@.tmp" && mv "@LOG@.tmp" "@LOG@.pid"
[ -n "$STAND_IN_CHATTY" ] && head -c 1000000 /dev/zero | tr '\0' x
shift 2
"$@"
[ -n "$STAND_IN_HANG" ] && exec sleep 30
exit 0
`

// project is a project with the stand-in beside it; it returns the project, the stand-in and its
// call log.
func project(t *testing.T) (string, string, string) {
	root := t.TempDir()
	repo, coop, log := filepath.Join(root, "repo"), filepath.Join(root, "coop"), filepath.Join(root, "calls.log")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coop, []byte(strings.ReplaceAll(standIn, "@LOG@", log)), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, coop, log
}

// calls is every call the stand-in recorded, as its working directory, arguments, project and auto-up.
func calls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for line := range strings.Lines(strings.TrimSpace(string(data))) {
		out = append(out, strings.Split(strings.TrimSpace(line), "|"))
	}
	return out
}

func verbs(t *testing.T, log string) []string {
	var out []string
	for _, call := range calls(t, log) {
		out = append(out, strings.Fields(call[1])[0])
	}
	return out
}

// benchOn runs the command on the stand-in, returning its exit code, the summary it wrote and what
// it said on stderr.
func benchOn(t *testing.T, ctx context.Context, b *bench, repo, coop string, samples int) (int, map[string]summary, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	var stderr bytes.Buffer
	code := run(ctx, []string{"--repo", repo, "--samples", strconv.Itoa(samples), "--out", out, "--coop", coop}, b, io.Discard, &stderr)
	var written struct {
		Summary map[string]summary
		Samples map[string][]row
	}
	if data, err := os.ReadFile(filepath.Join(out, "samples.json")); err == nil {
		if err := json.Unmarshal(data, &written); err != nil {
			t.Fatal(err)
		}
	}
	return code, written.Summary, stderr.String()
}

func counted(summaries map[string]summary) map[string]int {
	return map[string]int{"stopped": summaries["stopped"].N, "running": summaries["running"].N}
}

func TestItOnlyRunsUpDownAndRunInItsProject(t *testing.T) {
	// The bench runs coop up, coop down and coop run in the project it was given, and nothing else:
	// it never removes a box or volume itself, and it always leaves the project's services stopped.
	// A shell pointed at another project, with services off, changes neither.
	t.Setenv("COOP_REPO", "/somewhere/else")
	t.Setenv("COOP_AUTO_UP", "0")
	repo, coop, log := project(t)
	code, summaries, said := benchOn(t, context.Background(), newBench(), repo, coop, 2)
	if code != 0 || counted(summaries)["stopped"] != 2 || counted(summaries)["running"] != 2 {
		t.Fatalf("exit %d, summary %v, said %q", code, summaries, said)
	}
	want := slices.Concat(slices.Repeat([]string{"down", "run", "up", "run"}, 3), []string{"down"})
	if got := verbs(t, log); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	physical, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls(t, log) {
		if call[0] != physical || call[2] != repo || call[3] != "1" {
			// auto-up 1: the stopped case must time the launch that starts the services
			t.Errorf("call %q ran in %s on project %s with auto-up %q", call[1], call[0], call[2], call[3])
		}
	}
	if _, err := os.Stat(filepath.Join(repo, mark)); err == nil {
		t.Error("the marker was left behind")
	}
}

func TestARelativeCoopIsTheOneBesideTheCaller(t *testing.T) {
	// make passes ./coop, but every call runs inside the project.
	repo, coop, _ := project(t)
	t.Chdir(filepath.Dir(coop))
	if code, summaries, said := benchOn(t, context.Background(), newBench(), repo, "./coop", 1); code != 0 ||
		counted(summaries)["stopped"] != 1 || counted(summaries)["running"] != 1 {
		t.Errorf("exit %d, summary %v, said %q", code, summaries, said)
	}
}

func TestALaunchThatHangsAfterItsMarkerIsStopped(t *testing.T) {
	repo, coop, log := project(t)
	b := newBench()
	b.coop, b.repo, b.env = coop, repo, append(os.Environ(), "STAND_IN_HANG=1")
	b.linger = time.Second // the hang is the behavior; the step and the marker keep their generous bound
	r, err := b.sample(context.Background(), "down")
	if err != nil || r.Seconds == nil || r.Exit != -int(syscall.SIGTERM) {
		t.Fatalf("row = %+v, %v; want the start timed and the launch ended by TERM", r, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(read(t, log+".pid")))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the hung launch was left running: %v", err)
	}
	if got := verbs(t, log); !slices.Equal(got, []string{"down", "run"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestAStopEndsAChildThatIgnoresTerm(t *testing.T) {
	// The launch leaves on TERM and its child does not: the stop goes on until the session is empty.
	// The child outsleeps the wait below, so only a stop that kills it passes.
	child := filepath.Join(t.TempDir(), "child")
	cmd := exec.Command("sh", "-c", `sh -c 'trap "" TERM; echo $$ > "$0.tmp" && mv "$0.tmp" "$0"; exec sleep 120' "$1" &
exec sleep 30`, "launch", child)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		if alive(cmd.Process.Pid) { // a live member holds the group id, so it is still this test's
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	wait.ForFile(t, child)
	(&bench{stopGrace: 500 * time.Millisecond}).stop(cmd.Process.Pid, exited)
	// A killed orphan stays in the group as a zombie until init reaps it.
	wait.For(t, "the launch's session emptying after its stop", func() bool { return !alive(cmd.Process.Pid) })
}

func TestAChattyLaunchDoesNotStallBeforeItsMarker(t *testing.T) {
	// A megabyte of output is more than a pipe holds: unread, it would block the launch.
	t.Setenv("STAND_IN_CHATTY", "1")
	repo, coop, _ := project(t)
	b := newBench()
	b.timeout = 20 * time.Second
	code, summaries, said := benchOn(t, context.Background(), b, repo, coop, 1)
	if code != 0 || summaries["stopped"].Failed != 0 || summaries["running"].Failed != 0 {
		t.Errorf("exit %d, summary %v, said %q", code, summaries, said)
	}
}

func TestAnInterruptedBenchStillStopsTheServices(t *testing.T) {
	// Ctrl-C once the first launch is running.
	t.Setenv("STAND_IN_HANG", "1")
	repo, coop, log := project(t)
	ctx, interrupt := context.WithCancel(context.Background())
	defer interrupt()
	go func() {
		for deadline := time.Now().Add(wait.Deadline); time.Now().Before(deadline); time.Sleep(wait.Poll) {
			if _, err := os.Stat(log + ".pid"); err == nil {
				break
			}
		}
		interrupt()
	}()
	code, _, said := benchOn(t, ctx, newBench(), repo, coop, 1)
	if code == 0 || !strings.Contains(said, "interrupted") {
		t.Errorf("exit %d, said %q", code, said)
	}
	if got := verbs(t, log); got[len(got)-1] != "down" {
		t.Errorf("calls = %v; the services were left up", got)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(read(t, log+".pid")))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the interrupted launch was left running: %v", err)
	}
}

func TestAFailedUpStopsTheBenchInsteadOfMislabelingSamples(t *testing.T) {
	t.Setenv("STAND_IN_UP_FAILS", "1")
	repo, coop, log := project(t)
	out := filepath.Join(t.TempDir(), "out")
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"--repo", repo, "--samples", "1", "--out", out, "--coop", coop},
		newBench(), io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "coop up exited 1: services did not start") {
		t.Errorf("exit %d, said %q", code, stderr.String())
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("samples were written for a run that could not set its cases up")
	}
	if got := verbs(t, log); got[len(got)-1] != "down" {
		t.Errorf("calls = %v; the services were left up", got)
	}
}

func TestRunRefusesNonpositiveSamplesWithoutTouchingServices(t *testing.T) {
	for _, samples := range []string{"0", "-1"} {
		repo, coop, log := project(t)
		var stderr bytes.Buffer
		code := run(context.Background(), []string{"--repo", repo, "--out", filepath.Join(t.TempDir(), "results"), "--coop", coop, "--samples", samples}, newBench(), io.Discard, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "samples must be positive") {
			t.Errorf("samples=%s: exit %d, said %q", samples, code, stderr.String())
		}
		if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid sample count invoked services", err)
		}
	}
}

func TestRunRefusesAnIncompleteCommand(t *testing.T) {
	for _, args := range [][]string{{"--out", "x"}, {"--repo", "x"}, {"--repo", "x", "--out", "y", "extra"}} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &bench{}, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "usage:") {
			t.Errorf("%q: exit %d, said %q", args, code, stderr.String())
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
