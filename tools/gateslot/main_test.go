package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the tests' gates outside any slot: make check runs these tests inside its own gate,
// which marks every process under it. With GATESLOT_TEST_GATE set, this binary is a gateslot on two
// slots in that folder instead, so a test can start one gate inside another.
func TestMain(m *testing.M) {
	if dir := os.Getenv("GATESLOT_TEST_GATE"); dir != "" {
		os.Exit(gate(dir, 2, 10*time.Millisecond, os.Stderr, os.Args[1:]))
	}
	os.Unsetenv(held)
	os.Exit(m.Run())
}

// TestGateInsideAGateTakesNoSecondSlot runs a gate inside a gate, as the fork merge gate's
// coop-gate-slot runs this repo's make check: the inner gate runs in the outer gate's slot and
// leaves the other slot free.
func TestGateInsideAGateTakesNoSecondSlot(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	started, release := filepath.Join(scratch, "started"), filepath.Join(scratch, "release")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The outer gate's command is a second gate, which runs the shell.
	outer, out := startGate(ctx, t, dir, os.Args[0],
		"sh", "-c", `touch "$1"; while [ ! -e "$2" ]; do sleep 0.01; done`, "sh", started, release)
	waitFor(t, started)
	if first, second := busy(t, dir, 0), busy(t, dir, 1); !first || second {
		t.Errorf("first slot taken %v, second %v; want only the outer gate's", first, second)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := outer.Wait(); err != nil {
		t.Fatalf("gates exit = %v\n%s", err, out)
	}
}

// TestTwoGatesWithGatesInsideFinishOnTwoSlots is the deadlock of two fork merges at once: each
// merge's coop-gate-slot took one of the two slots, and the make check inside each waited for a
// slot of its own, forever. Here each outer gate starts its inner gate only once both outer gates
// hold a slot, and both finish.
func TestTwoGatesWithGatesInsideFinishOnTwoSlots(t *testing.T) {
	dir, scratch := t.TempDir(), t.TempDir()
	a, b := filepath.Join(scratch, "a"), filepath.Join(scratch, "b")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	inside := `touch "$1"; while [ ! -e "$2" ]; do sleep 0.01; done; exec "$3" true`
	first, firstOut := startGate(ctx, t, dir, "sh", "-c", inside, "sh", a, b, os.Args[0])
	second, secondOut := startGate(ctx, t, dir, "sh", "-c", inside, "sh", b, a, os.Args[0])
	for i, run := range []struct {
		gate *exec.Cmd
		out  *bytes.Buffer
	}{{first, firstOut}, {second, secondOut}} {
		err := run.gate.Wait()
		if ctx.Err() != nil {
			t.Fatalf("the gates inside waited for the slots their outer gates held until the test stopped them:\n%s", run.out)
		}
		if err != nil {
			t.Fatalf("gate %d exit = %v\n%s", i+1, err, run.out)
		}
	}
}

// startGate starts this test binary as a gateslot on dir that runs argv, in a process group of its
// own: when ctx ends, the gate is killed with every gate it started.
func startGate(ctx context.Context, t *testing.T, dir string, argv ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, os.Args[0], argv...)
	cmd.Env = append(os.Environ(), "GATESLOT_TEST_GATE="+dir)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, &out
}

// busy reports whether a running gate holds slot i under dir.
func busy(t *testing.T, dir string, i int) bool {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, fmt.Sprintf("slot-%d", i)))
	if errors.Is(err, os.ErrNotExist) {
		return false
	} else if err != nil {
		t.Fatal(err)
	}
	defer file.Close() // closing drops the lock if this probe got it
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal(err)
	}
	return err != nil
}

// TestSecondGateWaitsForTheFirst runs two gates on one slot: the second says it waits, starts only
// once the first has finished, and then runs.
func TestSecondGateWaitsForTheFirst(t *testing.T) {
	dir := t.TempDir()
	slotDir := filepath.Join(dir, "slots")
	release, started, second := filepath.Join(dir, "release"), filepath.Join(dir, "started"), filepath.Join(dir, "second")
	first := make(chan int, 1)
	go func() {
		first <- gate(slotDir, 1, 10*time.Millisecond, &bytes.Buffer{},
			[]string{"sh", "-c", `touch "$1"; while [ ! -e "$2" ]; do sleep 0.01; done`, "sh", started, release})
	}()
	waitFor(t, started)

	var mu sync.Mutex
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- gate(slotDir, 1, 10*time.Millisecond, &lockedWriter{&mu, &out}, []string{"touch", second})
	}()
	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(out.String(), "Waiting for another gate to finish: 1 already running on this host")
	})
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(second); err == nil {
		t.Fatal("the second gate ran while the first held the only slot")
	}

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code := <-first; code != 0 {
		t.Fatalf("first gate exit = %d", code)
	}
	if code := <-done; code != 0 {
		t.Fatalf("second gate exit = %d", code)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatal("the second gate never ran after the first finished")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(out.String(), "Got a gate slot after") {
		t.Errorf("the second gate did not say when it got its slot:\n%s", out.String())
	}
}

// TestGatesUpToTheLimitDoNotWait holds one of two slots: another gate starts at once, silently.
func TestGatesUpToTheLimitDoNotWait(t *testing.T) {
	dir := t.TempDir()
	held, err := acquire(dir, 2, time.Millisecond, func() { t.Fatal("the first slot waited") })
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	var out bytes.Buffer
	if code := gate(dir, 2, time.Hour, &out, []string{"true"}); code != 0 || out.Len() != 0 {
		t.Fatalf("exit = %d, output %q; want 0 and no wait", code, out.String())
	}
}

// TestGateReturnsTheCommandsStatus keeps a failed gate failed.
func TestGateReturnsTheCommandsStatus(t *testing.T) {
	if code := gate(t.TempDir(), 1, time.Millisecond, &bytes.Buffer{}, []string{"sh", "-c", "exit 3"}); code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
}

// TestKilledGateFreesItsSlot kills a process holding the only slot with SIGKILL: no cleanup runs,
// and the next gate still gets the slot.
func TestKilledGateFreesItsSlot(t *testing.T) {
	if dir := os.Getenv("GATESLOT_TEST_HOLD"); dir != "" {
		if _, err := acquire(dir, 1, time.Millisecond, func() {}); err != nil {
			os.Exit(1)
		}
		os.Stdout.WriteString("held\n")
		select {}
	}
	dir := t.TempDir()
	holder := exec.Command(os.Args[0], "-test.run=^TestKilledGateFreesItsSlot$")
	holder.Env = append(os.Environ(), "GATESLOT_TEST_HOLD="+dir)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	line := make([]byte, 5)
	if _, err := stdout.Read(line); err != nil || string(line) != "held\n" {
		t.Fatalf("holder did not take the slot: %q %v", line, err)
	}
	probe, err := os.Open(filepath.Join(dir, "slot-0"))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("the holder's slot was free before it was killed")
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	free := make(chan error, 1)
	go func() {
		file, err := acquire(dir, 1, 10*time.Millisecond, func() {})
		if err == nil {
			file.Close()
		}
		free <- err
	}()
	select {
	case err := <-free:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the killed holder's slot was never freed")
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	waitUntil(t, func() bool { _, err := os.Stat(path); return err == nil })
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}
