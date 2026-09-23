//go:build darwin || linux

package loopcfg

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoopConfigReadsRefuseFIFOsWithoutBlocking(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, filepath.FromSlash(File))
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := LoadSnapshot(repo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO loop config was accepted")
		}
	case <-time.After(2 * time.Second):
		if unblock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			_ = unblock.Close()
		}
		t.Fatal("loop config load blocked on a repository FIFO")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("mcp: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, snap, err := LoadSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	driftDone := make(chan bool, 1)
	go func() {
		_, drifted := snap.Drift()
		driftDone <- drifted
	}()
	select {
	case drifted := <-driftDone:
		if drifted {
			t.Fatal("unsafe unreadable replacement was reported as ordinary config drift")
		}
	case <-time.After(2 * time.Second):
		if unblock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			_ = unblock.Close()
		}
		t.Fatal("loop config drift check blocked on a repository FIFO")
	}
}

func TestLoadSnapshotRejectsOversizedConfig(t *testing.T) {
	repo := write(t, strings.Repeat("#", loopConfigFileLimit+1))
	if _, _, err := LoadSnapshot(repo); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized config error = %v", err)
	}
}
