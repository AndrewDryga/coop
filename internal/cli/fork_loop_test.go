package cli

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
	"github.com/AndrewDryga/coop/internal/project"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
)

func TestParseForkCreateLoopFlags(t *testing.T) {
	tests := []struct {
		args                 []string
		loop, detach, worker bool
		agent, tasks         string
	}{
		{[]string{"perf", "codex", "--loop", "--tasks", "q.md"}, true, false, false, "codex", "q.md"},
		{[]string{"perf", "-d", "--tasks", "q.md"}, true, true, false, "", "q.md"},                      // no positional agent → "" (required later)
		{[]string{"perf", "--loop", "--detach", "--tasks", "q.md"}, true, true, false, "", "q.md"},      // long form of -d
		{[]string{"perf", "gemini", "--loop", "-d", "-t", "q.md"}, true, true, false, "gemini", "q.md"}, // short -t
		{[]string{"perf", "--loop", "--tasks=q.md"}, true, false, false, "", "q.md"},                    // --tasks=VALUE form
		{[]string{"perf", "--_detached", "--tasks", "q.md"}, true, false, true, "", "q.md"},
	}
	for _, tc := range tests {
		fa, err := parseForkCreate(tc.args)
		if err != nil {
			t.Errorf("parseForkCreate(%v) err = %v", tc.args, err)
			continue
		}
		if fa.loop != tc.loop || fa.detach != tc.detach || fa.worker != tc.worker || fa.agent != tc.agent || fa.tasks != tc.tasks {
			t.Errorf("parseForkCreate(%v) = {loop:%v detach:%v worker:%v agent:%q tasks:%q}, want {loop:%v detach:%v worker:%v agent:%q tasks:%q}",
				tc.args, fa.loop, fa.detach, fa.worker, fa.agent, fa.tasks, tc.loop, tc.detach, tc.worker, tc.agent, tc.tasks)
		}
	}

	// --loop / -d without --tasks is allowed now — runForkLoop defaults it to every
	// project.TaskDirs queue (it knows the repo). --tasks without --loop is still an error.
	if _, err := parseForkCreate([]string{"perf", "--loop"}); err != nil {
		t.Errorf("parseForkCreate(--loop, no --tasks): want no error (defaults later), got %v", err)
	}
	if _, err := parseForkCreate([]string{"perf", "-d"}); err != nil {
		t.Errorf("parseForkCreate(-d, no --tasks): want no error (defaults later), got %v", err)
	}
	if _, err := parseForkCreate([]string{"perf", "--tasks", "q.md"}); err == nil {
		t.Error("parseForkCreate(--tasks without --loop): want error")
	}
}

func TestParseForkCreateCredential(t *testing.T) {
	// The target pins the account + model; it sits happily among the loop flags.
	fa, err := parseForkCreate([]string{"perf", "claude:opus-4.8@work", "--loop", "--tasks", "q.md"})
	if err != nil {
		t.Fatalf("parseForkCreate err = %v", err)
	}
	if fa.credential != "work" || fa.model != "opus-4.8" {
		t.Errorf("credential=%q model=%q, want work / opus-4.8", fa.credential, fa.model)
	}
	// --profile/--credential/--model are not fork flags — each errors as an unknown arg,
	// in both space and = forms.
	for _, args := range [][]string{
		{"perf", "--profile", "work"}, {"perf", "--profile=work"},
		{"perf", "--credential", "work"}, {"perf", "--credential=work"},
		{"perf", "--model", "opus"},
	} {
		if _, err := parseForkCreate(args); err == nil {
			t.Errorf("parseForkCreate(%v): an unknown flag must error, got nil", args)
		}
	}
}

func TestForkStopMessages(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	// A fork that doesn't exist → "no such fork" (matching ls/path/rm), not "not running".
	if code, err := a.forkStop([]string{"ghost"}); code != 1 || err == nil || !strings.Contains(err.Error(), "no such fork") {
		t.Errorf("forkStop(ghost) = (%d, %v), want (1, no such fork)", code, err)
	}
	// Stopping an already-idle fork is idempotent.
	if err := os.MkdirAll(forkWorkspace(repo, "idle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, err := a.forkStop([]string{"idle"}); code != 0 || err != nil {
		t.Errorf("forkStop(idle) = (%d, %v), want (0, nil)", code, err)
	}
}

func TestDetachStartFailureReleasesReservation(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	name := "perf"
	if err := os.MkdirAll(forkWorkspace(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(forkLog(repo, name), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	if code, err := a.detachForkLoop(repo, name, "codex", "", "", "", "", "", nil); code != -1 || err == nil {
		t.Fatalf("detach with unwritable log target = (%d, %v), want startup failure", code, err)
	}
	if pathExists(forkPid(repo, name)) {
		t.Fatal("failed detached start retained its reservation")
	}
}

func TestForkStopKeepsLegacyContainerCleanupVisible(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(repo, "perf"), []byte("2147483646\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "predates repository-scoped") || !strings.Contains(err.Error(), "coop.fork=perf") {
		t.Fatalf("legacy cleanup stop = (%d, %v), want explicit manual recovery", code, err)
	}
	data, readErr := os.ReadFile(forkPid(repo, "perf"))
	state, parseErr := parseForkWorkerState(string(data))
	if readErr != nil || parseErr != nil || !state.legacy || !state.pending {
		t.Fatalf("legacy cleanup state = %q, read %v parse %v state %+v; want retained legacy pending", data, readErr, parseErr, state)
	}
}

func TestForkStopSignalsLiveLegacyWorkerWithoutScopedReap(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	events := filepath.Join(dir, "runtime-events")
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COOP_TEST_EVENTS\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	ready := filepath.Join(dir, "ready")
	worker := exec.Command("sh", "-c", `
trap 'exit 0' TERM
: > "$COOP_TEST_READY"
while :; do sleep 10; done
`)
	worker.Env = append(os.Environ(), "COOP_TEST_READY="+ready)
	worker.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = worker.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = syscall.Kill(-worker.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("legacy worker %d did not exit", worker.Process.Pid)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for !pathExists(ready) {
		if time.Now().After(deadline) {
			t.Fatal("legacy worker did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	token := procStartToken(worker.Process.Pid)
	if !stableProcToken(token) {
		t.Skip("stable process identity unavailable")
	}
	legacy := fmt.Sprintf("%d\n%s\n", worker.Process.Pid, token)
	if err := os.WriteFile(forkPid(repo, "perf"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}, rtSet: true}
	code, err := a.forkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "predates repository-scoped") {
		t.Fatalf("stop live legacy worker = (%d, %v), want manual cleanup result", code, err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy worker remained alive after stop")
	}
	if pathExists(events) {
		data, _ := os.ReadFile(events)
		t.Fatalf("legacy stop invoked unsafe scoped runtime cleanup: %s", data)
	}
	data, readErr := os.ReadFile(forkPid(repo, "perf"))
	state, parseErr := parseForkWorkerState(string(data))
	if readErr != nil || parseErr != nil || !state.legacy || !state.pending || state.pid != worker.Process.Pid {
		t.Fatalf("live legacy cleanup state = %q, read %v parse %v state %+v", data, readErr, parseErr, state)
	}
}

func TestForkStopFindsReservedLegacyNameWithoutWorkspace(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkWorkerState(repo, "stop", forkWorkerState{pending: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if names := forkLifecycleNames(repo); !slices.Contains(names, "stop") {
		t.Fatalf("lifecycle names = %v, want reserved legacy name", names)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: containerruntime.Runtime{Name: runtimeCLI}, rtSet: true}
	if code, err := a.forkStop([]string{"stop"}); code != 0 || err != nil {
		t.Fatalf("stop reserved legacy fork = (%d, %v), want success", code, err)
	}
}

func TestForkStopRejectsMalformedStateWithoutSignaling(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
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
	if err := os.WriteFile(forkPid(repo, "perf"), []byte(fmt.Sprintf("reap-pend\n%d\n", worker.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "malformed") || !strings.Contains(err.Error(), "coop fork stop perf") {
		t.Fatalf("forkStop malformed state = (%d, %v), want actionable refusal", code, err)
	}
	if err := worker.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("malformed state must not signal a possibly live worker: %v", err)
	}
}

func TestForkStopRefusesUnverifiedLegacyLivePID(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
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
	if err := os.WriteFile(forkPid(repo, "perf"), []byte(fmt.Sprintf("%d\nWed Jun 18 10:00:00 2026\n", worker.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "legacy state") || !strings.Contains(err.Error(), "kill -TERM") || !strings.Contains(err.Error(), "coop fork stop perf") {
		t.Fatalf("forkStop legacy state = (%d, %v), want actionable refusal", code, err)
	}
	if err := worker.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("legacy state must not signal an unverified live pid: %v", err)
	}
}

func TestForkStopRejectsPIDOneWithoutSignaling(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(repo, "perf"), []byte("1\nlinux-proc-v1:fake:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSignal := signalPID
	called := false
	signalPID = func(int, syscall.Signal) error {
		called = true
		return nil
	}
	t.Cleanup(func() { signalPID = oldSignal })
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("forkStop(pid 1) = (%d, %v), want malformed-state refusal", code, err)
	}
	if called {
		t.Fatal("pid 1 state must be rejected before any liveness probe or signal")
	}
}

func TestForkStopReportsCleanupStateRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkWorkerState(repo, "perf", forkWorkerState{pending: true}); err != nil {
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
	t.Setenv("COOP_TEST_PID_PATH", forkPid(repo, "perf"))
	a := &app{
		cfg:   &config.Config{RepoOverride: repo},
		rt:    containerruntime.Runtime{Name: runtimeCLI},
		rtSet: true,
	}
	code, err := a.forkStop([]string{"perf"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "cleanup state could not be cleared") || !strings.Contains(err.Error(), "coop fork stop perf") {
		t.Fatalf("forkStop state removal failure = (%d, %v), want actionable error", code, err)
	}
}

func TestForkStopReapsBoxAfterWorkerCrash(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	runtimeCLI := filepath.Join(dir, "runtime")
	events := filepath.Join(dir, "events")
	if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkWorkerState(repo, "perf", forkWorkerState{pending: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = ps ]; then printf '%s\n' box-perf; fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)
	a := &app{
		cfg:   &config.Config{RepoOverride: repo},
		rt:    containerruntime.Runtime{Name: runtimeCLI},
		rtSet: true,
	}
	if code, err := a.forkStop([]string{"perf"}); code != 0 || err != nil {
		t.Fatalf("forkStop after worker crash = (%d, %v), want success", code, err)
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	want := "ps -q -a --filter label=coop.fork-owner=" + forkContainerOwner(repo, "perf") + "\nrm -f box-perf\n"
	if got := string(data); got != want {
		t.Errorf("crash cleanup calls = %q, want %q", got, want)
	}
	if pathExists(forkPid(repo, "perf")) {
		t.Fatal("successful crash cleanup should remove worker state")
	}
}

// forkStop signals the detached worker before reaping only that fork's labeled box. The runtime
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

			if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			token := procStartToken(pid)
			if token == "" {
				t.Fatal("could not read worker start identity")
			}
			pidState, err := (forkWorkerState{pid: pid, token: token, pending: tc.pending}).marshal()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(forkPid(repo, "perf"), pidState, 0o644); err != nil {
				t.Fatal(err)
			}
			a := &app{
				cfg:   &config.Config{RepoOverride: repo},
				rt:    containerruntime.Runtime{Name: runtimeCLI},
				rtSet: true,
			}

			code, stopErr := a.forkStop([]string{"perf"})
			if tc.failure == "" {
				if code != 0 || stopErr != nil {
					t.Fatalf("forkStop(perf) = (%d, %v), want (0, nil)", code, stopErr)
				}
			} else if code != 1 || stopErr == nil || !strings.Contains(stopErr.Error(), "box reap failed") || !strings.Contains(stopErr.Error(), "coop fork stop perf") {
				t.Fatalf("forkStop(perf) = (%d, %v), want (1, box reap failed)", code, stopErr)
			}
			data, err := os.ReadFile(events)
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			termAt := strings.Index(got, "worker:term\n")
			psCall := "runtime:ps -q -a --filter label=coop.fork-owner=" + forkContainerOwner(repo, "perf") + "\n"
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
				marker, err := os.ReadFile(forkPid(repo, "perf"))
				markerState, parseErr := parseForkWorkerState(string(marker))
				if err != nil || parseErr != nil || !markerState.pending || markerState.pid != pid {
					t.Fatalf("failed reap marker = %q, read %v, parse %v; want pending state for pid %d", marker, err, parseErr, pid)
				}
				if err := claimForkPid(repo, "perf"); err == nil || !strings.Contains(err.Error(), "coop fork stop perf") {
					t.Fatalf("claim during pending cleanup = %v, want actionable refusal", err)
				}
				t.Setenv("COOP_TEST_RUNTIME_FAILURE", "")
				if retryCode, retryErr := a.forkStop([]string{"perf"}); retryCode != 0 || retryErr != nil {
					t.Fatalf("forkStop(perf) retry = (%d, %v), want (0, nil)", retryCode, retryErr)
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
			if pathExists(forkPid(repo, "perf")) {
				t.Error("pidfile should be removed after a successful reap or retry")
			}
		})
	}
}

func TestForkStatePaths(t *testing.T) {
	repo := "/home/me/proj"
	if got, want := forkStateDir(repo), "/home/me/proj-forks/.coop"; got != want {
		t.Errorf("forkStateDir = %q, want %q", got, want)
	}
	if got, want := forkLog(repo, "perf"), "/home/me/proj-forks/.coop/perf.log"; got != want {
		t.Errorf("forkLog = %q, want %q", got, want)
	}
	if got, want := forkPid(repo, "perf"), "/home/me/proj-forks/.coop/perf.pid"; got != want {
		t.Errorf("forkPid = %q, want %q", got, want)
	}
}

func TestForkRunningPid(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// No pidfile → 0.
	if got := forkRunningPid(repo, "perf"); got != 0 {
		t.Errorf("forkRunningPid(no file) = %d, want 0", got)
	}
	// A legacy live pid without a stable token is not authoritative enough to call running.
	if err := os.WriteFile(forkPid(repo, "perf"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "perf"); got != 0 {
		t.Errorf("forkRunningPid(pid-only live) = %d, want cleanup-pending 0", got)
	}
	token := procStartToken(os.Getpid())
	if err := writeForkWorkerState(repo, "pending-live", forkWorkerState{pid: os.Getpid(), token: token, pending: true}); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "pending-live"); got != os.Getpid() {
		t.Errorf("forkRunningPid(pending live) = %d, want %d", got, os.Getpid())
	}
	// A dead/out-of-range pid → 0, but its state remains until forkStop reaps any orphaned box.
	if err := os.WriteFile(forkPid(repo, "dead"), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "dead"); got != 0 {
		t.Errorf("forkRunningPid(dead) = %d, want 0", got)
	}
	if !pathExists(forkPid(repo, "dead")) {
		t.Error("dead worker state must remain as the exact-label reap handle")
	}
}

func TestParsePidfile(t *testing.T) {
	if pid, tok := parsePidfile("123\n"); pid != 123 || tok != "" { // legacy, pid-only
		t.Errorf("parsePidfile(legacy) = %d,%q want 123,\"\"", pid, tok)
	}
	// Legacy start-time tokens may contain spaces; parsing keeps them intact for fail-closed recovery.
	if pid, tok := parsePidfile("456\nWed Jun 18 10:00:00 2026\n"); pid != 456 || tok != "Wed Jun 18 10:00:00 2026" {
		t.Errorf("parsePidfile(with token) = %d,%q", pid, tok)
	}
	if pid, _ := parsePidfile("nonsense"); pid != 0 {
		t.Errorf("parsePidfile(junk) pid = %d, want 0", pid)
	}
}

func TestForkWorkerStateWireFormat(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    forkWorkerState
		wantErr bool
	}{
		{name: "owner scoped running", raw: forkOwnerStateV1 + "42\nlinux-proc-v1:boot:123\n", want: forkWorkerState{pid: 42, token: "linux-proc-v1:boot:123"}},
		{name: "legacy running", raw: "42\nlinux-proc-v1:boot:123\n", want: forkWorkerState{pid: 42, token: "linux-proc-v1:boot:123", legacy: true}},
		{name: "legacy token", raw: "42\nWed Jun 18 10:00:00 2026\n", want: forkWorkerState{pid: 42, token: "Wed Jun 18 10:00:00 2026", legacy: true}},
		{name: "owner scoped pending", raw: forkOwnerStateV1 + forkReapPending, want: forkWorkerState{pending: true}},
		{name: "legacy bare pending", raw: forkReapPending, want: forkWorkerState{pending: true, legacy: true}},
		{name: "identified pending", raw: forkOwnerStateV1 + forkReapPending + "42\ndarwin-kinfo-v1:1:2\n", want: forkWorkerState{pid: 42, token: "darwin-kinfo-v1:1:2", pending: true}},
		{name: "empty", wantErr: true},
		{name: "pid zero", raw: "0\ntoken\n", wantErr: true},
		{name: "pid one", raw: "1\ntoken\n", wantErr: true},
		{name: "pending pid one", raw: forkReapPending + "1\ntoken\n", wantErr: true},
		{name: "pending junk", raw: forkReapPending + "not-a-pid\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseForkWorkerState(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseForkWorkerState(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseForkWorkerState(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
			encoded, err := got.marshal()
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := parseForkWorkerState(string(encoded))
			if err != nil || !reflect.DeepEqual(roundTrip, got) {
				t.Fatalf("state round trip = %+v, %v; want %+v", roundTrip, err, got)
			}
		})
	}
}

// A pidfile whose stable start-time token no longer matches the pid's process means the pid was
// reused by an unrelated process after the worker crashed → not running, but still cleanup-pending.
// A matching token (a genuinely live worker) is still reported running.
func TestForkRunningPidReusedPid(t *testing.T) {
	if procStartToken(os.Getpid()) == "" {
		t.Skip("kernel process identity unavailable — can't test start-time corroboration")
	}
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	// Our own (live) pid, but recorded with a start time from a different process → reused → 0.
	if err := os.WriteFile(forkPid(repo, "reused"), []byte(fmt.Sprintf("%d\nlinux-proc-v1:0\n", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "reused"); got != 0 {
		t.Errorf("forkRunningPid(reused pid) = %d, want 0", got)
	}
	if !pathExists(forkPid(repo, "reused")) {
		t.Error("reused-pid state must remain for exact-label cleanup")
	}
	// The same pid recorded with its real start time (writeForkPid round-trip) → genuinely running.
	if err := writeForkPid(repo, "live", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if got := forkRunningPid(repo, "live"); got != os.Getpid() {
		t.Errorf("forkRunningPid(live, matching token) = %d, want %d", got, os.Getpid())
	}
}

func TestWriteForkPidRejectsMissingStableToken(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	oldRead := readProcStartToken
	readProcStartToken = func(int) string { return "" }
	t.Cleanup(func() { readProcStartToken = oldRead })

	if err := writeForkPid(repo, "live", os.Getpid()); err == nil || !strings.Contains(err.Error(), "no stable process identity") {
		t.Fatalf("writeForkPid without a stable token = %v, want fail-closed error", err)
	}
	if pathExists(forkPid(repo, "live")) {
		t.Fatal("failed identity publication left a pidfile")
	}
}

func TestProcStartTokenIgnoresCallerTimezoneAndLocale(t *testing.T) {
	t.Setenv("TZ", "America/New_York")
	t.Setenv("LC_ALL", "C")
	first := procStartToken(os.Getpid())
	t.Setenv("TZ", "Asia/Tokyo")
	t.Setenv("LC_ALL", "POSIX")
	second := procStartToken(os.Getpid())
	if first == "" || first != second || !stableProcToken(first) {
		t.Fatalf("stable process tokens = %q and %q", first, second)
	}
}

// runningForkNames is the destructive-operation guard: a live loop and pending exact-label cleanup
// both count, as does dead-worker state that may own an orphan; only an absent one does not.
func TestRunningForkNames(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(repo, "live"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(repo, "dead"), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeForkWorkerState(repo, "pending", forkWorkerState{pending: true}); err != nil {
		t.Fatal(err)
	}
	got := runningForkNames(repo, []string{"live", "dead", "pending", "absent"})
	if !reflect.DeepEqual(got, []string{"live", "dead", "pending"}) {
		t.Errorf("runningForkNames = %v, want [live dead pending]", got)
	}
}

func TestDetachForkLoopRefusesDoubleStart(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkWorkspace(repo, "perf"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A live pidfile (our own pid) stands in for a worker already looping this fork.
	if err := writeForkPid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{}}
	code, err := a.detachForkLoop(repo, "perf", "claude", "", "", "", "", "", nil)
	if err == nil {
		t.Fatal("detachForkLoop started a second worker for an already-running fork")
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	// The original pid must survive — overwriting it is exactly the orphan bug.
	if got := forkRunningPid(repo, "perf"); got != os.Getpid() {
		t.Errorf("pidfile clobbered: forkRunningPid = %d, want %d", got, os.Getpid())
	}
}

func TestRecordStartedForkKillsChildWhenStateWriteFails(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "perf"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	oldReplace := replaceForkState
	replaceForkState = func(string, string, []byte) error { return errors.New("injected state write failure") }
	t.Cleanup(func() { replaceForkState = oldReplace })
	if err := recordStartedFork(repo, "perf", cmd); err == nil {
		t.Fatal("recordStartedFork should report the injected persistence failure")
	}
	if cmd.ProcessState == nil {
		t.Fatal("child must be reaped when its state cannot be persisted")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Fatal("child must be dead when its state cannot be persisted")
	}
	if pathExists(forkPid(repo, "perf")) {
		t.Fatal("failed child reservation should be cleared")
	}
}

// claimForkPid is the atomic reservation that closes the double-start race: a first claim wins, a
// second claim of a LIVE fork is refused, and dead-worker state also refuses a new start until its
// exact-label cleanup is completed.
func TestClaimForkPid(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "x"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !pathExists(forkPid(repo, "x")) {
		t.Fatal("claim should create the pidfile")
	}
	// A cleanup tombstone reserves the name without authorizing a signal, so a second claim is refused.
	if err := claimForkPid(repo, "x"); err == nil {
		t.Error("a second claim of a live fork must be refused")
	}
	// A dead pidfile may still own an orphaned box, so it cannot be silently reclaimed.
	if err := os.WriteFile(forkPid(repo, "stale"), []byte("2147483646\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := claimForkPid(repo, "stale"); err == nil || !strings.Contains(err.Error(), "coop fork stop stale") {
		t.Errorf("dead worker claim should require cleanup, got %v", err)
	}
}

// The lifecycle lock closes the same-fork stop/start window: a new detach cannot reserve the
// pidfile until the in-flight stop either removes its cleanup marker or leaves it retryable.
func TestForkLifecycleLockBlocksClaim(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	unlock, err := lockForkState(repo, "perf")
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

func TestWorkerCleanupDoesNotWaitForStopLock(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := writeForkPid(repo, "perf", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockForkState(repo, "perf")
	if err != nil {
		t.Fatal(err)
	}
	cleared := make(chan struct{})
	go func() {
		clearForkPidIfMine(repo, "perf")
		close(cleared)
	}()
	select {
	case <-cleared:
	case <-time.After(2 * time.Second):
		unlock()
		t.Fatal("worker cleanup waited behind stop's lifecycle lock")
	}
	if !pathExists(forkPid(repo, "perf")) {
		unlock()
		t.Fatal("worker cleanup must leave stop's state marker alone while stop owns the lock")
	}
	unlock()
}

// waitForExit returns immediately-true for a dead pid and false (after the timeout) for a live one
// — forkStop uses it to confirm death before clearing the pidfile.
func TestWaitForExit(t *testing.T) {
	if exited, err := waitForExit(2147483646, "", 2*time.Second); !exited || err != nil { // a pid that isn't running
		t.Errorf("waitForExit(dead) = (%v, %v), want (true, nil)", exited, err)
	}
	token := procStartToken(os.Getpid())
	if exited, err := waitForExit(os.Getpid(), token, 80*time.Millisecond); exited || err != nil { // we're alive
		t.Errorf("waitForExit(live) = (%v, %v), want (false, nil)", exited, err)
	}
}

// clearForkPidIfMine removes the pidfile only when it names THIS process — never one a different
// live worker owns (the orphan-cascade guard).
func TestClearForkPidIfMine(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeForkPid(repo, "mine", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	clearForkPidIfMine(repo, "mine")
	if pathExists(forkPid(repo, "mine")) {
		t.Error("clearForkPidIfMine should remove a pidfile that names us")
	}
	// A pidfile owned by another pid must be left alone.
	if err := os.WriteFile(forkPid(repo, "other"), []byte("424242\ntoken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clearForkPidIfMine(repo, "other")
	if !pathExists(forkPid(repo, "other")) {
		t.Error("clearForkPidIfMine must NOT remove a pidfile owned by another process")
	}
	if err := writeForkWorkerState(repo, "pending", forkWorkerState{pid: os.Getpid(), token: procStartToken(os.Getpid()), pending: true}); err != nil {
		t.Fatal(err)
	}
	clearForkPidIfMine(repo, "pending")
	if !pathExists(forkPid(repo, "pending")) {
		t.Error("clearForkPidIfMine must leave an in-progress stop marker for forkStop")
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

	queues, err := seedForkQueues(repo, ws, "", nil)
	if err != nil {
		t.Fatalf("seedForkQueues: %v", err)
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

	queues, err := seedForkQueues(repo, ws, "", nil)
	if err != nil {
		t.Fatalf("seedForkQueues: %v", err)
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

	if _, err := seedForkQueues(repo, ws, src, nil); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if !isTaskDir(filepath.Join(ws, tasksRoot, stateTodo, "2026-01-01-a")) {
		t.Fatal("explicit --tasks was not seeded into .agent/tasks")
	}
	// Second call: the fork already has a queue → onKept fires, source not re-applied.
	kept := false
	if _, err := seedForkQueues(repo, ws, src, func() { kept = true }); err != nil {
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

// A default `coop fork <name> --loop` (no --tasks) in a repo with NO task queue fails fast
// BEFORE any clone — no stray fork workspace is left behind — instead of cloning and only
// erroring later in the worker's log.
func TestForkLoopDefaultNoQueueFailsFast(t *testing.T) {
	repo := t.TempDir()
	a := &app{cfg: &config.Config{RepoOverride: repo}}
	code, err := a.forkCreate([]string{"x", "--loop"})
	if err == nil || !strings.Contains(err.Error(), "no task queue found") {
		t.Fatalf("forkCreate(x --loop, no queue) = (%d, %v), want a 'no task queue found' error", code, err)
	}
	if pathExists(forkWorkspace(repo, "x")) {
		t.Error("a fork workspace was created despite the fast-fail")
	}
}
