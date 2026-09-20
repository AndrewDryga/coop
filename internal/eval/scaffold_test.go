package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScaffoldWritesAWorkingSuiteAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := Scaffold(dir); err != nil {
		t.Fatal(err)
	}
	// The scaffolded suite loads and validates as-is.
	if _, err := Load(filepath.Join(dir, "suite.yaml")); err != nil {
		t.Fatalf("scaffolded suite does not load: %v", err)
	}
	// Create-only: a second scaffold into the same dir refuses rather than truncating a kept file.
	if err := Scaffold(dir); err == nil {
		t.Fatal("Scaffold overwrote an existing file")
	}
	// Even with suite.yaml gone but another file kept, the kept file is not clobbered.
	if err := os.Remove(filepath.Join(dir, "suite.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := Scaffold(dir); err == nil {
		t.Fatal("Scaffold overwrote a kept file after suite.yaml was removed")
	}
}
