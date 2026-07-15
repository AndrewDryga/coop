package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Detect validates a COOP_RUNTIME override up front, so a bogus value fails clearly here instead
// of later as a misleading "image not built".
func TestDetectOverride(t *testing.T) {
	// A known runtime that's actually installed is accepted on PATH alone.
	accepted := false
	for _, rt := range []string{"docker", "podman", "container"} {
		if _, err := exec.LookPath(rt); err == nil {
			if got, err := Detect(rt); err != nil || got.Name != rt {
				t.Errorf("Detect(%q) = (%+v, %v), want it accepted", rt, got, err)
			}
			accepted = true
			break
		}
	}
	if !accepted {
		t.Skip("no container runtime installed to test the accept path")
	}
	// A name that doesn't resolve on PATH → a clear "not found".
	if _, err := Detect("definitely-not-a-runtime-xyz"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a bogus override should be 'not found', got %v", err)
	}
	// An executable that resolves but isn't a runtime (fails --version) → "not usable".
	if _, err := exec.LookPath("false"); err == nil {
		if _, err := Detect("false"); err == nil || !strings.Contains(err.Error(), "usable") {
			t.Errorf("a non-runtime override (`false`) should be 'not usable', got %v", err)
		}
	}
}

func TestRunExitCodes(t *testing.T) {
	// Use coreutils true/false as stand-in runtimes to exercise exit handling
	// without needing a real container daemon.
	if code, err := (Runtime{Name: "true"}).Run(nil, nil, nil); err != nil || code != 0 {
		t.Errorf("true -> code=%d err=%v, want 0,nil", code, err)
	}
	if code, err := (Runtime{Name: "false"}).Run(nil, nil, nil); err != nil || code != 1 {
		t.Errorf("false -> code=%d err=%v, want 1,nil", code, err)
	}
	if _, err := (Runtime{Name: "agent-no-such-binary-xyz"}).Run(nil, nil, nil); err == nil {
		t.Error("missing binary should return a start error")
	}
}

func TestRunInterruptible(t *testing.T) {
	// Exit-code parity with Run, using a shell as a stand-in runtime (no daemon needed).
	if code, err := (Runtime{Name: "sh"}).RunInterruptible(context.Background(), nil, nil, nil, "-c", "exit 7"); err != nil || code != 7 {
		t.Fatalf("sh -c 'exit 7' -> code=%d err=%v, want 7,nil", code, err)
	}
}

func TestRunInterruptibleCancelKillsGroup(t *testing.T) {
	// Canceling the context must tear down the WHOLE process group, not just the direct child:
	// sh backgrounds `sleep 30` (a grandchild), records its pid, then waits. A child-only kill
	// would orphan the sleep; a group kill (what Setpgid + kill(-pid) buys) reaps it too.
	pidfile := filepath.Join(t.TempDir(), "gpid")
	script := "sleep 30 & echo $! > " + pidfile + "; wait"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() {
		(Runtime{Name: "sh"}).RunInterruptible(ctx, nil, nil, nil, "-c", script)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(killGrace + 5*time.Second):
			t.Errorf("RunInterruptible cleanup did not finish")
		}
	}()

	gpid := 0
	for i := 0; i < 300 && gpid == 0; i++ {
		if b, err := os.ReadFile(pidfile); err == nil {
			gpid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if gpid == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if gpid == 0 {
		t.Fatal("grandchild pid never recorded — script didn't start")
	}
	if err := syscall.Kill(gpid, 0); err != nil {
		t.Fatalf("grandchild %d should be alive before cancel: %v", gpid, err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(killGrace + 5*time.Second):
		t.Fatal("RunInterruptible did not return after cancel")
	}
	if elapsed := time.Since(start); elapsed > killGrace+5*time.Second {
		t.Fatalf("cancel took %v — the 30s sleep wasn't cut short", elapsed)
	}
	// The grandchild must be gone — proof the process group was signaled, not just the child.
	if err := waitGone(gpid, 3*time.Second); err != nil {
		t.Fatalf("grandchild %d survived cancel (process group not torn down): %v", gpid, err)
	}
}

func TestRunInterruptibleCancelKillsDescendantAfterLeaderExits(t *testing.T) {
	// The runtime leader exits promptly on TERM while its child ignores TERM. Cleanup must keep
	// owning the process group after cmd.Wait returns and escalate the surviving child to KILL.
	pidfile := filepath.Join(t.TempDir(), "leader-first-gpid")
	childScript := "trap '' TERM; while :; do sleep 1; done"
	script := "trap 'exit 0' TERM; sh -c " + shellQuote(childScript) + " & echo $! > " + shellQuote(pidfile) + "; while :; do sleep 1; done"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = (Runtime{Name: "sh"}).RunInterruptible(ctx, nil, nil, nil, "-c", script)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(killGrace + 5*time.Second):
			t.Errorf("leader-first RunInterruptible cleanup did not finish")
		}
	}()

	gpid := 0
	for i := 0; i < 300 && gpid == 0; i++ {
		if b, err := os.ReadFile(pidfile); err == nil {
			gpid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if gpid == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if gpid == 0 {
		t.Fatal("TERM-resistant descendant pid was not recorded")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(killGrace + 5*time.Second):
		t.Fatal("RunInterruptible did not finish leader-first cancellation")
	}
	if err := waitGone(gpid, 3*time.Second); err != nil {
		_ = syscall.Kill(gpid, syscall.SIGKILL)
		t.Fatalf("TERM-resistant descendant %d survived leader exit: %v", gpid, err)
	}
}

func TestWaitLeaderDoneIsBounded(t *testing.T) {
	done := make(chan error, 1)
	if waitLeaderDone(done, 10*time.Millisecond) {
		t.Fatal("waitLeaderDone reported a completion that never arrived")
	}
	done <- nil
	if !waitLeaderDone(done, time.Second) {
		t.Fatal("waitLeaderDone missed a buffered completion")
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// waitGone polls until pid is no longer running or the deadline passes.
func waitGone(pid int, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if processGoneOrZombie(pid) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("process still running")
}

func processGoneOrZombie(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return true
	}
	state, ok := procState(pid)
	return ok && state == 'Z'
}

func procState(pid int) (byte, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "State:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] != "" {
				return fields[1][0], true
			}
			return 0, false
		}
	}
	return 0, false
}

func TestSilent(t *testing.T) {
	if !(Runtime{Name: "true"}).Silent() {
		t.Error("true should be silent-success")
	}
	if (Runtime{Name: "false"}).Silent() {
		t.Error("false should be silent-failure")
	}
}

// Canceling an error-reporting label reap kills the runtime CLI's whole process group, including
// helpers it spawned. This keeps `fork stop` bounded without leaking a child behind its deadline.
func TestRemoveByLabelCancellationKillsRuntimeGroup(t *testing.T) {
	dir := t.TempDir()
	runtimeCLI := filepath.Join(dir, "runtime")
	childPIDFile := filepath.Join(dir, "child-pid")
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
if [ "$1" = ps ]; then
	sleep 30 &
	child=$!
	printf '%s\n' "$child" > "$COOP_TEST_CHILD_PID_FILE"
	wait "$child"
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_CHILD_PID_FILE", childPIDFile)

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := (Runtime{Name: runtimeCLI}).RemoveByLabel(ctx, "coop.fork", "perf")
		done <- err
	}()

	var data []byte
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		data, err = os.ReadFile(childPIDFile)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read runtime helper pid: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime helper pid was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RemoveByLabel cancellation error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveByLabel did not return after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("RemoveByLabel returned after %v, want a bounded cancellation", elapsed)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse runtime helper pid: %v", err)
	}
	if err := waitGone(childPID, 2*time.Second); err != nil {
		t.Fatalf("runtime helper %d survived context cancellation: %v", childPID, err)
	}
}

// Apple container has no Docker-style `ps --filter`; list its running resources as JSON, then
// match labels locally so one fork's stop cannot remove another fork's box.
func TestRemoveByLabelAppleContainerExactMatch(t *testing.T) {
	dir := t.TempDir()
	runtimeCLI := filepath.Join(dir, "container")
	events := filepath.Join(dir, "events")
	if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
printf '%s\n' "$*" >> "$COOP_TEST_EVENTS"
if [ "$1" = list ]; then
	printf '%s\n' '[{"id":"box-perf","configuration":{"labels":{"coop.fork":"perf"}}},{"id":"box-other","configuration":{"labels":{"coop.fork":"other"}}}]'
fi
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_EVENTS", events)

	// This deadline only guards a broken fixture; timing is not the behavior under test, and the
	// race suite may run beside the full gate on a loaded host.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if n, err := (Runtime{Name: runtimeCLI}).RemoveByLabel(ctx, "coop.fork", "perf"); err != nil || n != 1 {
		t.Fatalf("RemoveByLabel(Apple, perf) = (%d, %v), want (1, nil)", n, err)
	}
	data, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "list --format json\nrm -f box-perf\n"; got != want {
		t.Errorf("Apple runtime calls = %q, want %q", got, want)
	}
}

func TestRemoveByLabelPreservesRuntimeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, failure, command, detail string
	}{
		{name: "query", failure: "ps", command: "ps -q --filter label=coop.fork=perf", detail: "daemon query broke"},
		{name: "remove", failure: "rm", command: "rm -f box-perf", detail: "container remove broke"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runtimeCLI := filepath.Join(dir, "runtime")
			if err := os.WriteFile(runtimeCLI, []byte(`#!/bin/sh
if [ "$1" = ps ]; then
	if [ "$COOP_TEST_FAILURE" = ps ]; then echo 'daemon query broke' >&2; exit 42; fi
	echo box-perf
fi
if [ "$1" = rm ] && [ "$COOP_TEST_FAILURE" = rm ]; then
	echo 'container remove broke' >&2
	exit 43
fi
`), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("COOP_TEST_FAILURE", tc.failure)
			_, err := (Runtime{Name: runtimeCLI}).RemoveByLabel(context.Background(), "coop.fork", "perf")
			if err == nil || !strings.Contains(err.Error(), "run: "+runtimeCLI+" "+tc.command) || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("RemoveByLabel diagnostic = %v, want command %q and stderr %q", err, tc.command, tc.detail)
			}
		})
	}
}
