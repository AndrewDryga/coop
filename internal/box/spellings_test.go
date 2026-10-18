package box

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOtherSpellingsCoversEveryLetterCase(t *testing.T) {
	got, ok := otherSpellings(".env")
	want := []string{".Env", ".eNv", ".ENv", ".enV", ".EnV", ".eNV", ".ENV"}
	slices.Sort(got)
	slices.Sort(want)
	if !ok || !slices.Equal(got, want) {
		t.Fatalf("otherSpellings(.env) = %v, %v; want %v", got, ok, want)
	}
	if got, ok := otherSpellings("database.yml"); !ok || len(got) != 1<<11-1 {
		t.Fatalf("database.yml has %d other spellings (%v), want %d", len(got), ok, 1<<11-1)
	}
	if got, ok := otherSpellings("2024.01"); !ok || len(got) != 0 {
		t.Fatalf("a name without letters has no other spelling, got %v", got)
	}
	for _, name := range []string{"credentials.json", ".env.production", "secrét.env"} {
		if _, ok := otherSpellings(name); ok {
			t.Errorf("otherSpellings(%q) claims every spelling can be covered", name)
		}
	}
}

// foldsCase asks the disk, not the platform: true exactly where another spelling opens the same
// file, which is what decides whether covers are needed.
func TestFoldsCaseAsksTheDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("SECRET=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(filepath.Join(dir, ".ENV"))
	if folds := err == nil; foldsCase(path) != folds {
		t.Fatalf("foldsCase = %v, but .ENV opens the file: %v", foldsCase(path), folds)
	}
	if foldsCase(filepath.Join(dir, "missing")) {
		t.Fatal("a missing file folds nothing")
	}
}
