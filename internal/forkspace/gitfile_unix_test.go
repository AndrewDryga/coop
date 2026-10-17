//go:build darwin || linux

package forkspace

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// A box can make a .git file name anything by the time the host opens it. readGitFile never waits
// on a FIFO with no writer, never reads a device or a folder, and never reads past
// gitPointerLimit, whatever the size the file claims.
func TestReadGitFileRefusesWhatIsNotAPointer(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	device, folder := filepath.Join(dir, "device"), filepath.Join(dir, "folder")
	if err := os.Symlink("/dev/zero", device); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, folder); err != nil {
		t.Fatal(err)
	}
	huge := filepath.Join(dir, "huge")
	if err := os.WriteFile(huge, []byte("gitdir: x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(huge, 1<<40); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"a FIFO": fifo, "a device": device, "a folder": folder, "a 1 TiB file": huge} {
		done := make(chan error, 1)
		go func() {
			_, err := readGitFile(path)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s was read as a git pointer", name)
			}
			if path == huge && (err == nil || !strings.Contains(err.Error(), "is 1024 GiB, more than the 64 KiB Coop reads")) {
				t.Errorf("a 1 TiB file = %v, want it refused for its size", err)
			}
		case <-time.After(wait.Deadline):
			t.Fatalf("reading %s did not return within %v", name, wait.Deadline)
		}
	}
	pointer := filepath.Join(dir, "pointer")
	if err := os.WriteFile(pointer, []byte("gitdir: ../repo/.git/worktrees/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readGitFile(pointer); err != nil || !strings.HasPrefix(string(data), "gitdir: ") {
		t.Fatalf("readGitFile(a real pointer) = %q, %v", data, err)
	}
}
