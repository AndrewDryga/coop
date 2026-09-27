package workerconnector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateWorkerRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestInterruptedTransferCleanupPreservesPublishedAndJournaledCustody(t *testing.T) {
	root := privateWorkerRoot(t)
	removed := []string{"job-sources/.source-123/repository/object", "host-git-tmp/git-123", "connector/body-tmp/transfer-123"}
	retained := []string{"job-sources/" + strings.Repeat("a", 64) + "/repository/object", "connector/commands/command.json.body", "connector/body-tmp/unrecognized", "host-git-tmp/git-not-owned"}
	for _, path := range append(removed, retained...) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("private custody"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "job-sources", ".source-456")
	if err := os.Symlink(filepath.Join(root, "job-sources", strings.Repeat("a", 64)), link); err != nil {
		t.Fatal(err)
	}
	for range 2 { // Restart cleanup is idempotent and never consumes retry bodies.
		if err := ReclaimInterruptedTransfers(root); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range removed {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("interrupted scratch survived: %s: %v", path, err)
		}
	}
	for _, path := range retained {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("removed retained custody: %s: %v", path, err)
		}
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("cleanup followed a source-stage symlink")
	}
	unsafe := privateWorkerRoot(t)
	if err := os.Symlink(root, filepath.Join(unsafe, "connector")); err != nil {
		t.Fatal(err)
	}
	if err := ReclaimInterruptedTransfers(unsafe); err == nil {
		t.Fatal("cleanup accepted a symlinked parent")
	}
}
