package forkctl

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
)

func TestComposeTarget(t *testing.T) {
	cases := []struct {
		agent, model, effort, credential, want string
		wantErr                                bool
	}{
		{agent: "claude", want: "claude"},
		{agent: "claude", model: "opus-4.8", want: "claude:opus-4.8"},
		{agent: "claude", effort: "high", credential: "work", want: "claude/high@work"},
		{agent: "gemini", model: "gemini-3.5-flash@work", want: "gemini:gemini-3.5-flash@work"},
		{agent: "gemini", model: "gemini-3.5-flash@work", credential: "work", want: "gemini:gemini-3.5-flash@work"},
		{agent: "gemini", model: "gemini-3.5-flash@work", credential: "personal", wantErr: true},
	}
	for _, c := range cases {
		got, err := composeTarget(c.agent, c.model, c.effort, c.credential)
		if c.wantErr {
			if err == nil {
				t.Errorf("composeTarget(%q, %q, %q, %q) = %q, want error", c.agent, c.model, c.effort, c.credential, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("composeTarget(%q, %q, %q, %q) = (%q, %v), want %q", c.agent, c.model, c.effort, c.credential, got, err, c.want)
		}
	}
}

func TestForkStopMessages(t *testing.T) {
	repo := t.TempDir()
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	// A fork that doesn't exist → "no such fork" (matching ls/path/rm), not "not running".
	if code, err := a.ForkStop([]string{"ghost"}); code != 1 || err == nil || !strings.Contains(err.Error(), "no such fork") {
		t.Errorf("ForkStop(ghost) = (%d, %v), want (1, no such fork)", code, err)
	}
	// Stopping an already-idle fork is idempotent.
	if err := os.MkdirAll(forkspace.Workspace(repo, "idle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, err := a.ForkStop([]string{"idle"}); code != 0 || err != nil {
		t.Errorf("ForkStop(idle) = (%d, %v), want (0, nil)", code, err)
	}
}

func TestDetachStartFailureReleasesReservation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	name := "perf"
	if err := os.MkdirAll(forkspace.Workspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(forkspace.LogPath(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.DetachForkLoop(repo, name, "codex", "", "", "", "", "", nil); code != -1 || err == nil {
		t.Fatalf("detach with unwritable log target = (%d, %v), want startup failure", code, err)
	}
	if pathExists(forkspace.PidPath(repo, name)) {
		t.Fatal("failed detached start retained its reservation")
	}
}

// The start path itself must clear an abandoned reservation, not just claimForkPid: a fork whose
// last start died before its worker existed has to be startable again with no `coop fork stop`.
func TestDetachReclaimsAbandonedReservation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	name := "perf"
	if err := os.MkdirAll(forkspace.Workspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, name, forkspace.WorkerState{Claim: true, Pid: 2147483646, Token: "linux-proc-v1:1:2"}); err != nil {
		t.Fatal(err)
	}
	// A log path that can't be created stops this start right after the claim, so the failure names
	// the log — proving the reservation was reclaimed rather than refused — and is then released.
	if err := os.Mkdir(forkspace.LogPath(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.DetachForkLoop(repo, name, "codex", "", "", "", "", "", nil)
	if code != -1 || err == nil || strings.Contains(err.Error(), "coop fork stop") {
		t.Fatalf("detach over an abandoned reservation = (%d, %v), want the reclaim to proceed to startup", code, err)
	}
	if pathExists(forkspace.PidPath(repo, name)) {
		t.Fatal("the reclaimed reservation outlived its failed start")
	}
}

func TestForkStopRejectsUnsupportedStateWithoutSideEffects(t *testing.T) {
	stable := fmt.Sprintf("%d\n%s\n", os.Getpid(), forkspace.ProcStartToken(os.Getpid()))
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "pre-v8 worker", raw: stable, want: []string{"pre-v8", "left the exact file unchanged", "will not signal", "do not add an owner-v1 header", "https://github.com/AndrewDryga/coop/"}},
		{name: "pre-v8 pending stable token", raw: forkspace.ReapPending + stable, want: []string{"pre-v8", "MIGRATING.md#detached-worker-state"}},
		{name: "pre-v8 pending legacy token", raw: forkspace.ReapPending + fmt.Sprintf("%d\nWed Jun 18 10:00:00 2026\n", os.Getpid()), want: []string{"pre-v8", "MIGRATING.md#detached-worker-state"}},
		{name: "future owner", raw: "owner-v2\nopaque\n", want: []string{"unsupported detached-worker state version", "use the Coop version that wrote it", "do not edit its header"}},
	}
	oldSignal := forkspace.SignalPID
	signalCalls := 0
	forkspace.SignalPID = func(int, syscall.Signal) error {
		signalCalls++
		return nil
	}
	t.Cleanup(func() { forkspace.SignalPID = oldSignal })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			path := forkspace.PidPath(repo, "perf")
			if err := os.WriteFile(path, []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			runtimeCalls := 0
			a := &Control{
				cfg: &config.Config{RepoOverride: repo},
				host: Host{EnsureRuntime: func() (containerruntime.Runtime, error) {
					runtimeCalls++
					return containerruntime.Runtime{}, errors.New("runtime must not be consulted")
				}},
			}
			code, err := a.ForkStop([]string{"perf"})
			if code != 1 || err == nil {
				t.Fatalf("ForkStop unsupported state = (%d, %v), want (1, error)", code, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ForkStop error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "restore") {
				t.Errorf("ForkStop error recommends fabricating state: %v", err)
			}
			if runtimeCalls != 0 || signalCalls != 0 {
				t.Fatalf("unsupported stop performed side effects: runtime=%d signal=%d", runtimeCalls, signalCalls)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != tc.raw {
				t.Fatalf("unsupported state changed = %q, %v; want exact %q", got, readErr, tc.raw)
			}
		})
	}
}

func TestForkStopFindsPendingNameWithoutWorkspace(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "stop", forkspace.WorkerState{Pending: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if names := forkspace.LifecycleNames(repo); !slices.Contains(names, "stop") {
		t.Fatalf("lifecycle names = %v, want pending name", names)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}}
	if code, err := a.ForkStop([]string{"stop"}); code != 0 || err != nil {
		t.Fatalf("stop pending fork = (%d, %v), want success", code, err)
	}
}

func TestForkStopRejectsMalformedStateWithoutSignaling(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	worker := exec.Command("sleep", "30")
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
	})
	// A torn prefix must not be interpreted as a dead-worker cleanup marker.
	if err := os.WriteFile(forkspace.PidPath(repo, "perf"), []byte(fmt.Sprintf("reap-pend\n%d\n", worker.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.ForkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "malformed") || !strings.Contains(err.Error(), "left the exact file unchanged") {
		t.Fatalf("ForkStop malformed state = (%d, %v), want actionable refusal", code, err)
	}
	if err := worker.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("malformed state must not signal a possibly live worker: %v", err)
	}
}

func TestForkStopRejectsPIDOneWithoutSignaling(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkspace.PidPath(repo, "perf"), []byte(forkspace.OwnerStateV1+"1\nlinux-proc-v1:fake:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSignal := forkspace.SignalPID
	called := false
	forkspace.SignalPID = func(int, syscall.Signal) error {
		called = true
		return nil
	}
	t.Cleanup(func() { forkspace.SignalPID = oldSignal })
	a := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.ForkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("ForkStop(pid 1) = (%d, %v), want malformed-state refusal", code, err)
	}
	if called {
		t.Fatal("pid 1 state must be rejected before any liveness probe or signal")
	}
}

func TestForkStopReportsCleanupStateRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pending: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
if [ "$1" = ps ]; then
	rm -f "$COOP_TEST_PID_PATH"
	mkdir "$COOP_TEST_PID_PATH"
	: > "$COOP_TEST_PID_PATH/blocker"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_PID_PATH", forkspace.PidPath(repo, "perf"))
	a := &Control{
		cfg: &config.Config{RepoOverride: repo},
		rt:  containerruntime.Runtime{Name: runtimeCLI},
	}
	code, err := a.ForkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "cleanup state could not be cleared") || !strings.Contains(err.Error(), "coop fork stop perf") {
		t.Fatalf("ForkStop state removal failure = (%d, %v), want actionable error", code, err)
	}
}

func TestForkStopReapsBoxAfterWorkerCrash(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	events := filepath.Join(dir, "events")
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pending: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = ps ]; then printf '%s\n' box-perf; fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	a := &Control{
		cfg: &config.Config{RepoOverride: repo},
		rt:  containerruntime.Runtime{Name: runtimeCLI},
	}
	if code, err := a.ForkStop([]string{"perf"}); code != 0 || err != nil {
		t.Fatalf("ForkStop after worker crash = (%d, %v), want success", code, err)
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	want := "ps -q -a --filter label=coop.fork-owner=" + ForkContainerOwner(repo, "perf") + "\nrm -f box-perf\n"
	if got := string(data); got != want {
		t.Errorf("crash cleanup calls = %q, want %q", got, want)
	}
	if pathExists(forkspace.PidPath(repo, "perf")) {
		t.Fatal("successful crash cleanup should remove worker state")
	}
}

// ForkStop signals the detached worker before reaping only that fork's labeled box. The runtime
// shim makes both the orphan-present and already-gone paths deterministic without a real daemon.
func TestForkStopReapsBoxAfterWorkerExit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		containerID string
		failure     string
		pending     bool
	}{
		{name: "orphan present", containerID: "box-perf"},
		{name: "already gone"},
		{name: "interrupted stop resumes live worker", containerID: "box-perf", pending: true},
		{name: "query failure is retryable", containerID: "box-perf", failure: "ps"},
		{name: "remove failure is not success", containerID: "box-perf", failure: "rm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo := filepath.Join(dir, "repo")
			events := filepath.Join(dir, "events")
			runtimeCLI := filepath.Join(dir, "runtime")
			if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf 'runtime:%s\n' "$*" >> "$COOP_TEST_EVENTS"
if kill -0 "$COOP_TEST_WORKER_PID" 2>/dev/null; then
	printf 'runtime:worker-still-alive\n' >> "$COOP_TEST_EVENTS"
fi
if [ "$COOP_TEST_RUNTIME_FAILURE" = "$1" ]; then
	exit 42
fi
if [ "$1" = ps ] && [ -n "$COOP_TEST_CONTAINER_ID" ]; then
	printf '%s\n' "$COOP_TEST_CONTAINER_ID"
fi
`), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_TEST_EVENTS", events)
			t.Setenv("COOP_TEST_CONTAINER_ID", tc.containerID)
			t.Setenv("COOP_TEST_RUNTIME_FAILURE", tc.failure)

			worker := exec.Command("sh", "-c", `
trap 'printf "worker:term\n" >> "$COOP_TEST_EVENTS"; exit 0' TERM
printf "worker:ready\n" >> "$COOP_TEST_EVENTS"
while :; do sleep 10; done
`)
			worker.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := worker.Start(); err != nil {
				t.Fatal(err)
			}
			pid := worker.Process.Pid
			workerDone := make(chan struct{})
			go func() {
				_ = worker.Wait()
				close(workerDone)
			}()
			t.Setenv("COOP_TEST_WORKER_PID", strconv.Itoa(pid))
			t.Cleanup(func() {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				select {
				case <-workerDone:
				case <-time.After(2 * time.Second):
					t.Errorf("worker process group %d did not exit", pid)
				}
			})

			deadline := time.Now().Add(2 * time.Second)
			for {
				data, _ := os.ReadFile(events)
				if strings.Contains(string(data), "worker:ready\n") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}

			if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			token := forkspace.ProcStartToken(pid)
			if token == "" {
				t.Fatal("could not read worker start identity")
			}
			pidState, err := (forkspace.WorkerState{Pid: pid, Token: token, Pending: tc.pending}).Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(forkspace.PidPath(repo, "perf"), pidState, 0o644); err != nil {
				t.Fatal(err)
			}
			a := &Control{
				cfg: &config.Config{RepoOverride: repo},
				rt:  containerruntime.Runtime{Name: runtimeCLI},
			}

			code, stopErr := a.ForkStop([]string{"perf"})
			if tc.failure == "" {
				if code != 0 || stopErr != nil {
					t.Fatalf("ForkStop(perf) = (%d, %v), want (0, nil)", code, stopErr)
				}
			} else if code != 1 || stopErr == nil || !strings.Contains(stopErr.Error(), "box reap failed") || !strings.Contains(stopErr.Error(), "coop fork stop perf") {
				t.Fatalf("ForkStop(perf) = (%d, %v), want (1, box reap failed)", code, stopErr)
			}
			data, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			termAt := strings.Index(got, "worker:term\n")
			psCall := "runtime:ps -q -a --filter label=coop.fork-owner=" + ForkContainerOwner(repo, "perf") + "\n"
			psAt := strings.Index(got, psCall)
			if termAt < 0 || psAt < 0 || termAt >= psAt {
				t.Errorf("worker TERM must precede the exact-fork reap:\n%s", got)
			}
			if strings.Contains(got, "runtime:worker-still-alive\n") {
				t.Errorf("runtime reap started before the worker exited:\n%s", got)
			}
			rmCall := "runtime:rm -f " + tc.containerID + "\n"
			wantRuntime := psCall
			if tc.containerID != "" && tc.failure != "ps" {
				wantRuntime += rmCall
			}
			if tc.failure != "" {
				marker, err := os.ReadFile(forkspace.PidPath(repo, "perf"))
				markerState, parseErr := forkspace.ParseWorkerState(string(marker))
				if err != nil || parseErr != nil || !markerState.Pending || markerState.Pid != pid {
					t.Fatalf("failed reap marker = %q, read %v, parse %v; want pending state for pid %d", marker, err, parseErr, pid)
				}
				if err := claimForkPid(repo, "perf"); err == nil || !strings.Contains(err.Error(), "coop fork stop perf") {
					t.Fatalf("claim during pending cleanup = %v, want actionable refusal", err)
				}
				t.Setenv("COOP_TEST_RUNTIME_FAILURE", "")
				if retryCode, retryErr := a.ForkStop([]string{"perf"}); retryCode != 0 || retryErr != nil {
					t.Fatalf("ForkStop(perf) retry = (%d, %v), want (0, nil)", retryCode, retryErr)
				}
				wantRuntime += psCall
				if tc.containerID != "" {
					wantRuntime += rmCall
				}
				data, err = os.ReadFile(events)
				if err != nil {
					t.Fatal(err)
				}
				got = string(data)
			}
			var gotRuntime strings.Builder
			for _, line := range strings.SplitAfter(got, "\n") {
				if strings.HasPrefix(line, "runtime:") {
					gotRuntime.WriteString(line)
				}
			}
			if gotRuntime.String() != wantRuntime {
				t.Errorf("runtime calls = %q, want exact-fork reap %q", gotRuntime.String(), wantRuntime)
			}
			if pathExists(forkspace.PidPath(repo, "perf")) {
				t.Error("pidfile should be removed after a successful reap or retry")
			}
		})
	}
}

// runningForkNames is the destructive-operation guard: a live loop and pending exact-label cleanup
// both count, as does dead-worker state that may own an orphan; only an absent one does not.
func TestRunningForkNames(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WritePid(repo, "live", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "dead", forkspace.WorkerState{Pid: 2147483646, Token: "linux-proc-v1:1:2"}); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WriteWorkerState(repo, "pending", forkspace.WorkerState{Pending: true}); err != nil {
		t.Fatal(err)
	}
	got := runningForkNames(repo, []string{"live", "dead", "pending", "absent"})
	if !reflect.DeepEqual(got, []string{"live", "dead", "pending"}) {
		t.Errorf("runningForkNames = %v, want [live dead pending]", got)
	}
}

func TestDetachForkLoopRefusesDoubleStart(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.Workspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A live pidfile (our own pid) stands in for a worker already looping this fork.
	if err := forkspace.WritePid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	a := &Control{cfg: &config.Config{}}
	code, err := a.DetachForkLoop(repo, "perf", "claude", "", "", "", "", "", nil)
	if err == nil {
		t.Fatal("DetachForkLoop started a second worker for an already-running fork")
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	// The original pid must survive — overwriting it is exactly the orphan bug.
	if got := forkspace.RunningPid(repo, "perf"); got != os.Getpid() {
		t.Errorf("pidfile clobbered: forkspace.RunningPid = %d, want %d", got, os.Getpid())
	}
}

func TestRecordStartedForkKillsChildWhenStateWriteFails(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "perf"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	oldReplace := forkspace.ReplaceState
	forkspace.ReplaceState = func(string, string, []byte) error { return errors.New("injected state write failure") }
	t.Cleanup(func() { forkspace.ReplaceState = oldReplace })
	if err := recordStartedFork(repo, "perf", cmd); err == nil {
		t.Fatal("recordStartedFork should report the injected persistence failure")
	}
	if cmd.ProcessState == nil {
		t.Fatal("child must be reaped when its state cannot be persisted")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("child must be dead when its state cannot be persisted")
	}
	if pathExists(forkspace.PidPath(repo, "perf")) {
		t.Fatal("failed child reservation should be cleared")
	}
}

// claimForkPid is the atomic reservation that closes the double-start race: a first claim wins, a
// second claim of a LIVE fork is refused, and dead-worker state also refuses a new start until its
// exact-label cleanup is completed.
func TestClaimForkPid(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "x"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !pathExists(forkspace.PidPath(repo, "x")) {
		t.Fatal("claim should create the pidfile")
	}
	// A cleanup tombstone reserves the name without authorizing a signal, so a second claim is refused.
	if err := claimForkPid(repo, "x"); err == nil {
		t.Error("a second claim of a live fork must be refused")
	}
	// A dead pidfile may still own an orphaned box, so it cannot be silently reclaimed.
	if err := forkspace.WriteWorkerState(repo, "stale", forkspace.WorkerState{Pid: 2147483646, Token: "linux-proc-v1:1:2"}); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "stale"); err == nil || !strings.Contains(err.Error(), "coop fork stop stale") {
		t.Errorf("dead worker claim should require cleanup, got %v", err)
	}
}

// A reservation records the coop process that made it, so a start killed BEFORE its worker existed
// leaves a tombstone that can be proved abandoned and reclaimed — instead of wedging every later
// start behind a manual `coop fork stop`. Anything coop cannot disprove still refuses: a live owner,
// an owner whose identity is unreadable, and an owner that died after launching a worker.
func TestClaimForkPidReclaimsAbandonedReservation(t *testing.T) {
	// An out-of-range pid the kernel answers ESRCH for: provably gone, with a stable token shape.
	abandoned := forkspace.WorkerState{Claim: true, Pid: 2147483646, Token: "linux-proc-v1:1:2"}
	stateRepo := func(t *testing.T) string {
		t.Helper()
		repo := filepath.Join(t.TempDir(), "proj")
		if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
			t.Fatal(err)
		}
		return repo
	}

	t.Run("dead owner is reclaimed", func(t *testing.T) {
		repo := stateRepo(t)
		if err := forkspace.WriteWorkerState(repo, "perf", abandoned); err != nil {
			t.Fatal(err)
		}
		if err := claimForkPid(repo, "perf"); err != nil {
			t.Fatalf("claim over an abandoned reservation = %v, want it reclaimed", err)
		}
		state, err := forkspace.ReadWorkerState(repo, "perf")
		if err != nil || !state.Claim || state.Launched || state.Pid != os.Getpid() {
			t.Fatalf("reclaimed state = %+v (%v), want this process's own reservation", state, err)
		}
	})

	t.Run("dead owner that launched a worker is refused", func(t *testing.T) {
		repo := stateRepo(t)
		launched := abandoned
		launched.Launched = true
		if err := forkspace.WriteWorkerState(repo, "perf", launched); err != nil {
			t.Fatal(err)
		}
		err := claimForkPid(repo, "perf")
		if err == nil || !strings.Contains(err.Error(), "may still be looping") || !strings.Contains(err.Error(), "coop fork stop perf") {
			t.Fatalf("claim over an interrupted launch = %v, want a refusal: that worker may be running unrecorded", err)
		}
		if state, _ := forkspace.ReadWorkerState(repo, "perf"); state.Pid != abandoned.Pid {
			t.Fatalf("refused claim rewrote the reservation: %+v", state)
		}
	})

	t.Run("live owner is refused", func(t *testing.T) {
		repo := stateRepo(t)
		pid, token := liveProcess(t)
		if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Claim: true, Pid: pid, Token: token}); err != nil {
			t.Fatal(err)
		}
		err := claimForkPid(repo, "perf")
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(pid)) || !strings.Contains(err.Error(), "coop fork stop perf") {
			t.Fatalf("claim over a live reservation = %v, want a refusal naming pid %d", err, pid)
		}
		if state, _ := forkspace.ReadWorkerState(repo, "perf"); state.Pid != pid {
			t.Fatalf("refused claim stole a live owner's reservation: %+v", state)
		}
	})

	t.Run("unverifiable owner is refused", func(t *testing.T) {
		repo := stateRepo(t)
		pid, token := liveProcess(t)
		if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Claim: true, Pid: pid, Token: token}); err != nil {
			t.Fatal(err)
		}
		// The pid answers signals but its identity can no longer be read: liveness is undecidable,
		// which is not permission to reclaim.
		oldRead := forkspace.ReadProcStartToken
		forkspace.ReadProcStartToken = func(int) string { return "" }
		t.Cleanup(func() { forkspace.ReadProcStartToken = oldRead })
		err := claimForkPid(repo, "perf")
		if err == nil || !strings.Contains(err.Error(), "cannot verify") || !strings.Contains(err.Error(), "coop fork stop perf") {
			t.Fatalf("claim over an unverifiable reservation = %v, want a fail-closed refusal", err)
		}
		if state, _ := forkspace.ReadWorkerState(repo, "perf"); state.Pid != pid {
			t.Fatalf("refused claim rewrote an unverifiable reservation: %+v", state)
		}
	})
}

// liveProcess starts a throwaway process and returns the identity a fork state would record for it.
func liveProcess(t *testing.T) (int, string) {
	t.Helper()
	proc := exec.Command("sleep", "30")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	})
	token := forkspace.ProcStartToken(proc.Process.Pid)
	if !forkspace.StableProcToken(token) {
		t.Skip("stable process identity unavailable")
	}
	return proc.Process.Pid, token
}

// The lifecycle lock closes the same-fork stop/start window: a new detach cannot reserve the
// pidfile until the in-flight stop either removes its cleanup marker or leaves it retryable.
func TestForkLifecycleLockBlocksClaim(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	unlock, err := forkspace.LockState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	claimed := make(chan error, 1)
	go func() { claimed <- claimForkPid(repo, "perf") }()
	select {
	case err := <-claimed:
		unlock()
		t.Fatalf("claim returned before lifecycle unlock: %v", err)
	case <-time.After(80 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-claimed:
		if err != nil {
			t.Fatalf("claim after lifecycle unlock: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("claim remained blocked after lifecycle unlock")
	}
}

// waitForExit returns immediately-true for a dead pid and false (after the timeout) for a live one
// — ForkStop uses it to confirm death before clearing the pidfile.
func TestWaitForExit(t *testing.T) {
	if exited, err := waitForExit(2147483646, "", 2*time.Second); !exited || err != nil { // a pid that isn't running
		t.Errorf("waitForExit(dead) = (%v, %v), want (true, nil)", exited, err)
	}
	token := forkspace.ProcStartToken(os.Getpid())
	if exited, err := waitForExit(os.Getpid(), token, 80*time.Millisecond); exited || err != nil { // we're alive
		t.Errorf("waitForExit(live) = (%v, %v), want (false, nil)", exited, err)
	}
}

func TestStreamLog(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.log")
	if err := os.WriteFile(p, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	if err := streamLog(p, "", false, &buf, &mu); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "line1\nline2\n" {
		t.Errorf("streamLog = %q, want unprefixed lines", buf.String())
	}
	buf.Reset()
	_ = streamLog(p, "perf", false, &buf, &mu)
	if buf.String() != "perf | line1\nperf | line2\n" {
		t.Errorf("streamLog prefixed = %q", buf.String())
	}
	// A missing log is not an error and produces nothing.
	buf.Reset()
	if err := streamLog(filepath.Join(dir, "missing.log"), "", false, &buf, &mu); err != nil {
		t.Fatalf("missing log should not error: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("missing log produced %q", buf.String())
	}
}

// TestSeedForkQueuesSingleRepo: the default (empty --tasks) in a single repo seeds exactly the
// one .agent/tasks tree and returns [.agent/tasks] — byte-identical to the old single-queue path.
func TestSeedForkQueuesSingleRepo(t *testing.T) {
	repo := t.TempDir()
	ws := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-a", "task.md"), "# a\n")

	queues, err := SeedForkQueues(repo, ws, "", nil)
	if err != nil {
		t.Fatalf("SeedForkQueues: %v", err)
	}
	if len(queues) != 1 || queues[0] != filepath.FromSlash(tasksRoot) {
		t.Fatalf("queues = %v, want [%s]", queues, tasksRoot)
	}
	if !isTaskDir(filepath.Join(ws, tasksRoot, stateTodo, "2026-01-01-a")) {
		t.Error("the fork's .agent/tasks was not seeded")
	}
	// All four state dirs are scaffolded so the in-box move protocol is safe.
	for _, st := range taskStates {
		if !isDirTest(filepath.Join(ws, tasksRoot, st)) {
			t.Errorf("state dir %s missing in the seeded queue", st)
		}
	}
}

// TestSeedForkQueuesMonorepo: the default seeds EVERY project.TaskDirs queue at its own relative
// path (root + each subproject), so a monorepo fork carries all its subprojects' queues, and the
// returned queue list spans them.
func TestSeedForkQueuesMonorepo(t *testing.T) {
	repo := t.TempDir()
	ws := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, project.File), "subprojects:\n  - api\n  - web\n")
	// The root carries its own queue alongside the members' (TaskDirs includes it when present).
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-root", "task.md"), "# root\n")
	writeTaskFile(t, filepath.Join(repo, "api", tasksRoot, stateTodo, "2026-01-02-api", "task.md"), "# api\n")
	writeTaskFile(t, filepath.Join(repo, "web", tasksRoot, stateTodo, "2026-01-03-web", "task.md"), "# web\n")

	queues, err := SeedForkQueues(repo, ws, "", nil)
	if err != nil {
		t.Fatalf("SeedForkQueues: %v", err)
	}
	want := []string{
		filepath.FromSlash(tasksRoot),
		filepath.Join("api", tasksRoot),
		filepath.Join("web", tasksRoot),
	}
	if strings.Join(queues, "|") != strings.Join(want, "|") {
		t.Fatalf("queues = %v, want %v", queues, want)
	}
	// Each queue's task rode along at its own relative path — a task never left its home tree.
	for _, p := range []string{
		filepath.Join(ws, tasksRoot, stateTodo, "2026-01-01-root"),
		filepath.Join(ws, "api", tasksRoot, stateTodo, "2026-01-02-api"),
		filepath.Join(ws, "web", tasksRoot, stateTodo, "2026-01-03-web"),
	} {
		if !isTaskDir(p) {
			t.Errorf("missing seeded task: %s", p)
		}
	}
}

// TestSeedForkQueuesExplicitKeepsProgress: an explicit --tasks seeds one tree into .agent/tasks,
// and a resumed fork (dst already present) is NOT re-seeded — onKept fires so the caller can say so.
func TestSeedForkQueuesExplicitKeepsProgress(t *testing.T) {
	repo := t.TempDir()
	ws := t.TempDir()
	src := filepath.Join(repo, "src-queue")
	writeTaskFile(t, filepath.Join(src, stateTodo, "2026-01-01-a", "task.md"), "# a\n")

	if _, err := SeedForkQueues(repo, ws, src, nil); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if !isTaskDir(filepath.Join(ws, tasksRoot, stateTodo, "2026-01-01-a")) {
		t.Fatal("explicit --tasks was not seeded into .agent/tasks")
	}
	// Second call: the fork already has a queue → onKept fires, source not re-applied.
	kept := false
	if _, err := SeedForkQueues(repo, ws, src, func() { kept = true }); err != nil {
		t.Fatalf("resumed seed: %v", err)
	}
	if !kept {
		t.Error("onKept should fire when the fork already has its queue")
	}
}

func isDirTest(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
