//go:build darwin || linux

package sessionsvc

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSessionCredentialReaderRefusesFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := readCredentialArtifact(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO credential was accepted for a remote session")
		}
	case <-time.After(2 * time.Second):
		if unblock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			_ = unblock.Close()
		}
		t.Fatal("remote-session credential reader blocked on a FIFO")
	}
}
