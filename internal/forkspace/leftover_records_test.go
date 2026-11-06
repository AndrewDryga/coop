package forkspace

import (
	"os"
	"path/filepath"
	"testing"
)

// Deleting a fork removes what only it could use, by its name or its generation, and goes no
// further: a link is removed as a link and never followed, another fork's records stay, the lock
// file stays, and without a valid generation only the log goes.
func TestRemoveLeftoverRecordsTakesOnlyTheForksOwn(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	mine := Identity{Name: "perf", Generation: NewGeneration()}
	theirs := Identity{Name: "other", Generation: NewGeneration()}
	state := StateDir(repo)
	outside := t.TempDir()
	write := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	candidate := IsolatedRecordPrefix("candidate", mine) + "-1a2b3c4d"
	preview := IsolatedRecordPrefix("preview", mine) + "-5e6f7a8b"
	link := IsolatedRecordPrefix("candidate", mine) + "-link"
	own := []string{
		"perf.log",
		IsolatedRecordPrefix("source", mine) + ".json",
		filepath.Join(candidate, "objects", "pack"),
		filepath.Join(preview, "README.md"),
	}
	for _, name := range own {
		write(filepath.Join(state, name))
	}
	write(filepath.Join(outside, "keep.txt"))
	if err := os.Symlink(outside, filepath.Join(state, link)); err != nil {
		t.Fatal(err)
	}
	kept := []string{
		"other.log", "perf.lock", "perf.generation.json",
		IsolatedRecordPrefix("source", theirs) + ".json",
		filepath.Join(IsolatedRecordPrefix("candidate", theirs)+"-1a2b3c4d", "HEAD"),
		filepath.Join(IsolatedRecordPrefix("preview", theirs)+"-5e6f7a8b", "README.md"),
	}
	for _, name := range kept {
		write(filepath.Join(state, name))
	}
	exists := func(path string) bool {
		_, err := os.Lstat(path)
		return err == nil
	}

	if err := RemoveLeftoverRecordsLocked(repo, mine.Name, mine); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(own, link) {
		if exists(filepath.Join(state, name)) {
			t.Errorf("%s survived the fork", name)
		}
	}
	if !exists(filepath.Join(outside, "keep.txt")) {
		t.Error("a link in the records was followed out of the folder")
	}
	for _, name := range kept {
		if !exists(filepath.Join(state, name)) {
			t.Errorf("%s was removed with another fork", name)
		}
	}

	// A fork that never had a generation leaves only its log, and its name matches no other fork's records.
	if err := RemoveLeftoverRecordsLocked(repo, theirs.Name, Identity{Name: theirs.Name}); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(state, "other.log")) {
		t.Error("the log of a fork with no generation survived it")
	}
	for _, name := range kept[3:] {
		if !exists(filepath.Join(state, name)) {
			t.Errorf("%s was removed by a fork with no generation", name)
		}
	}
}

// With no records folder there is nothing to remove, and that is no failure.
func TestRemoveLeftoverRecordsWithNoRecordsFolder(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLeftoverRecordsLocked(repo, "perf", Identity{Name: "perf", Generation: NewGeneration()}); err != nil {
		t.Fatalf("RemoveLeftoverRecordsLocked with no records folder = %v, want nil", err)
	}
}
