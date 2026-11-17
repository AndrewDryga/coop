package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
