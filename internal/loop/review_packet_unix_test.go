//go:build darwin || linux

package loop

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReviewPacketRefusesTaskMetadataFIFOWithoutBlocking(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".agent", "tasks", "99_done", "task-a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "task.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.md"), []byte("**Done so far:** complete\n**Traps:** none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		done <- reviewPacket(repo, nil, []string{"task-a — " + dir}, loopChangeSet{})
	}()
	select {
	case packet := <-done:
		if !strings.Contains(packet, "acceptance: not stated") {
			t.Fatalf("packet did not safely omit unreadable task metadata:\n%s", packet)
		}
	case <-time.After(2 * time.Second):
		if unblock, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			_ = unblock.Close()
		}
		t.Fatal("review packet blocked on task metadata FIFO")
	}
}
