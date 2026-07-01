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

// waitGone polls until pid is no longer signalable (gone) or the deadline passes.
func waitGone(pid int, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("process still alive")
}

func TestSilent(t *testing.T) {
	if !(Runtime{Name: "true"}).Silent() {
		t.Error("true should be silent-success")
	}
	if (Runtime{Name: "false"}).Silent() {
		t.Error("false should be silent-failure")
	}
}
