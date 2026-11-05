package forkspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A box that can write the folder holding a project's forks folder can replace it with a link.
// Removing a fork then refuses, rather than deleting a same-named folder wherever the link leads.
func TestDestroyRefusesALinkedForksFolder(t *testing.T) {
	parent := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(elsewhere, "perf"), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(parent, "project")
	if err := os.Symlink(elsewhere, Home(repo)); err != nil {
		t.Fatal(err)
	}
	if err := Destroy(repo, "perf"); err == nil {
		t.Fatal("Destroy removed a fork through a linked forks folder")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "perf")); err != nil {
		t.Fatalf("a folder the link led to was removed: %v", err)
	}
}

// Builds up to v12.1.0 moved fork records out of <repo>-forks/.coop the first time a project opened,
// and never looked there again once the project's private records folder existed. This build does not
// move them: a project with no private folder whose old folder still holds records is refused by name,
// and those records stay exactly where they are.
func TestOldForkRecordsFolderIsRefusedByName(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	old := filepath.Join(Home(repo), ".coop")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(old, "perf.generation.json")
	if err := os.WriteFile(record, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := EnsureStateDir(repo)
	if err == nil || !strings.Contains(err.Error(), "v12.1.0") || !strings.Contains(err.Error(), old) ||
		!strings.Contains(err.Error(), "delete "+old) {
		t.Fatalf("EnsureStateDir = %v; want a refusal naming %s, the converting build and the delete fix", err, old)
	}
	if _, _, err := ReadGeneration(repo, "perf"); err == nil {
		t.Fatal("a fork whose records are in the old folder read as unbound")
	}
	if data, err := os.ReadFile(record); err != nil || string(data) != "{}\n" {
		t.Fatalf("the old record changed: %q, %v", data, err)
	}
	if _, err := os.Lstat(recordsPath(repo)); !os.IsNotExist(err) {
		t.Fatalf("the refusal created the private records folder: %v", err)
	}
}

// Lock files and a Finder .DS_Store are not records: an old folder holding only those is no reason
// to refuse a project.
func TestOldForkRecordsFolderOfLocksIsNotInTheWay(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	old := filepath.Join(Home(repo), ".coop")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"perf.lock", ".DS_Store"} {
		if err := os.WriteFile(filepath.Join(old, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnsureStateDir(repo); err != nil {
		t.Fatalf("an old folder of locks and .DS_Store refused the project: %v", err)
	}
	if _, err := os.Stat(filepath.Join(old, "perf.lock")); err != nil {
		t.Fatalf("the old lock file was removed: %v", err)
	}
}

// Once the private records folder exists the move happened, as far as any build is concerned: what an
// older build left behind in the old folder, a link, an anchor on another disk or an entry past its
// bounds, is never read and never refuses the project.
func TestOldForkRecordsLeftBehindAfterTheMoveAreIgnored(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(Home(repo), ".coop")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "perf.anchor"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RequireNoOldForkRecords(repo); err != nil {
		t.Fatalf("an entry left behind after the move refused the project: %v", err)
	}
	if _, _, err := ReadGeneration(repo, "perf"); err != nil {
		t.Fatalf("reading a fork after the move refused: %v", err)
	}
}

// An old folder Coop cannot read is not treated as empty: the project is refused with the error.
func TestOldForkRecordsUnreadableFolderRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	repo := filepath.Join(t.TempDir(), "project")
	old := filepath.Join(Home(repo), ".coop")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(old, 0o700) })
	if err := EnsureStateDir(repo); err == nil || !strings.Contains(err.Error(), old) {
		t.Fatalf("EnsureStateDir with an unreadable old folder = %v; want a refusal naming it", err)
	}
}

// A link at the old place is never followed: nothing behind it is read or refused.
func TestOldForkRecordsLinkIsNotFollowed(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(Home(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	behind := t.TempDir()
	if err := os.WriteFile(filepath.Join(behind, "elsewhere.pid"), []byte("owner-v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(behind, filepath.Join(Home(repo), ".coop")); err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(behind, "elsewhere.pid")); err != nil {
		t.Fatalf("what the link led to was touched: %v", err)
	}
}
