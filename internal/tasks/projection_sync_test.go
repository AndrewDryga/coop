package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A box can swap a checked projection folder for a FIFO before the walk opens it. The walk then
// fails at once instead of waiting on the FIFO while it holds the host's locks.
func TestProjectionWalkDoesNotWaitOnASwappedInFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan error, 1)
	go func() {
		done <- walkOpenedProjectionRoot(root, func(rel string, _ *os.Root, _ string, _ os.FileInfo) error {
			if rel != "artifacts" {
				return nil
			}
			path := filepath.Join(dir, "artifacts")
			if err := os.Remove(path); err != nil {
				return err
			}
			return syscall.Mkfifo(path, 0o644)
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("walk opened a FIFO as a folder")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("walk waited on the swapped-in FIFO")
	}
}

// A projection holding a top-level name the sync does not carry is refused before anything is
// written, so the canonical folder is never left half-updated.
func TestSyncProjectionTreeRefusesBeforeWriting(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{
		"artifacts/report.txt": "new evidence\n",
		"decision.md":          "# Decision\n",
		"gate.log":             "a stray log\n",
	} {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := syncProjectionTree(src, dst, false)
	if err == nil || !strings.Contains(err.Error(), `"gate.log"`) {
		t.Fatalf("syncProjectionTree = %v, want gate.log refused", err)
	}
	if entries, err := os.ReadDir(dst); err != nil || len(entries) != 0 {
		t.Fatalf("refused sync wrote into the canonical folder: %v, %v", entries, err)
	}
}
