package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

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
		return strings.Contains(out.String(), "Waiting for another gate to finish: 1 already run on this host")
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
