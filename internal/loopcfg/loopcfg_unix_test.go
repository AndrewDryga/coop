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

// A linked loop.yaml, or one under a linked .agent folder, is refused with a sentence naming
// the file, not the system's bare "too many levels of symbolic links".
func TestLoadSnapshotNamesALinkedConfig(t *testing.T) {
	target := write(t, "mcp: false\n")
	for name, link := range map[string]func(repo string) error{
		"file": func(repo string) error {
			if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(target, filepath.FromSlash(File)), filepath.Join(repo, filepath.FromSlash(File)))
		},
		"folder": func(repo string) error {
			return os.Symlink(filepath.Join(target, ".agent"), filepath.Join(repo, ".agent"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			if err := link(repo); err != nil {
				t.Fatal(err)
			}
			_, _, err := LoadSnapshot(repo)
			if err == nil || !strings.Contains(err.Error(), File+" is a link, or sits under one") {
				t.Fatalf("linked config error = %v, want it to name %s", err, File)
			}
		})
	}
}

func TestLoadSnapshotRejectsOversizedConfig(t *testing.T) {
	repo := write(t, strings.Repeat("#", loopConfigFileLimit+1))
	if _, _, err := LoadSnapshot(repo); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized config error = %v", err)
	}
}
