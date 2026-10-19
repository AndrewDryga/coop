package forkspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// legacyFolder makes repo's forks folder hold records the way builds up to v11.3.0 kept them.
func legacyFolder(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	legacy := filepath.Join(Home(repo), ".coop")
	for rel, body := range files {
		path := filepath.Join(legacy, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return legacy
}

// A box that can write the folder holding a project's forks folder can replace it with a link to
// another project's forks folder. The move follows no link, so it never takes that project's
// records: the project gets an empty records folder, and its box launches go on.
func TestLegacyForkRecordsMoveFollowsNoLink(t *testing.T) {
	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	otherRecords := legacyFolder(t, other, map[string]string{"perf.pid": "owner-v2\npid=1\n"})
	repo := filepath.Join(parent, "project")
	if err := os.Symlink(Home(other), Home(repo)); err != nil {
		t.Fatal(err)
	}
	if err := MoveLegacyForkRecords(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(otherRecords, "perf.pid")); err != nil {
		t.Fatalf("the other project's records moved: %v", err)
	}
	if moved, err := os.ReadDir(recordsPath(repo)); err != nil || len(moved) != 0 {
		t.Fatalf("records after a linked forks folder = %v, %v; want an empty folder", moved, err)
	}
	// A box launch records its execution all the same; fork commands in the folder still refuse.
	record, err := BeginExecution(repo, ExecutionSpec{Kind: ExecutionInteractive, Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("a launch beside a linked forks folder = %v", err)
	}
	t.Cleanup(func() { _ = EndExecution(repo, record) })
	if _, err := Names(repo); err == nil {
		t.Fatal("fork commands accepted a linked forks folder")
	}
}

// A box that held the old folder, or a folder in it, open before the move writes nowhere Coop reads
// afterwards: the records are copied into folders Coop made, not renamed.
func TestLegacyForkRecordsLeaveHeldFoldersBehind(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	legacy := legacyFolder(t, repo, map[string]string{"perf.pid": "owner-v2\npid=1\n", "executions/run.json": "{}\n"})
	var held []int
	for _, dir := range []string{legacy, filepath.Join(legacy, "executions")} {
		fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Close(fd) })
		held = append(held, fd)
	}
	if err := MoveLegacyForkRecords(repo); err != nil {
		t.Fatal(err)
	}
	for _, fd := range held {
		if planted, err := unix.Openat(fd, "planted.pid", unix.O_WRONLY|unix.O_CREAT, 0o600); err == nil {
			_ = unix.Close(planted)
		}
	}
	if got, err := os.ReadFile(filepath.Join(StateDir(repo), "perf.pid")); err != nil || string(got) != "owner-v2\npid=1\n" {
		t.Fatalf("moved record = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(StateDir(repo), "executions", "run.json")); err != nil {
		t.Fatalf("a record in a subfolder did not move: %v", err)
	}
	for _, planted := range []string{"planted.pid", "executions/planted.pid"} {
		if _, err := os.Lstat(filepath.Join(StateDir(repo), filepath.FromSlash(planted))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a write through a folder held before the move reached the records: %s (%v)", planted, err)
		}
	}
	if _, err := os.Lstat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old folder is still there: %v", err)
	}
}

// A project with nothing to move gets its records folder on first open, so a .coop a box plants in
// the forks folder later is never read.
func TestLegacyForkRecordsCloseTheOldPlaceWhenNothingMoves(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if names, err := LifecycleNames(repo); err != nil || len(names) != 0 {
		t.Fatalf("lifecycle names of a project without forks = %v, %v", names, err)
	}
	if info, err := os.Lstat(recordsPath(repo)); err != nil || !info.IsDir() {
		t.Fatalf("first open made no records folder: %v", err)
	}
	legacyFolder(t, repo, map[string]string{"planted.pid": "owner-v2\npid=1\n"})
	if names, err := LifecycleNames(repo); err != nil || slices.Contains(names, "planted") {
		t.Fatalf("lifecycle names = %v, %v; a .coop planted after the first open was read", names, err)
	}
}

// A hard link cannot cross disks, so on another disk a fork's anchor stays in the old folder, which
// stays whole for an older build, while the other records and logs move and box launches go on.
func TestLegacyForkRecordsOnAnotherDiskLeaveAnchorsBehind(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	legacy := filepath.Join(Home(repo), ".coop")
	if err := os.Rename(recordsPath(repo), legacy); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "perf.log"), []byte("worker output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := moveLegacyEntry
	moveLegacyEntry = func(int, string, int, string) error { return syscall.EXDEV }
	t.Cleanup(func() { moveLegacyEntry = before })
	if err := MoveLegacyForkRecords(repo); err != nil {
		t.Fatal(err)
	}
	anchor := forkGenerationAnchorName("perf", identity.Generation)
	if _, err := os.Stat(filepath.Join(StateDir(repo), "perf.generation.json")); err != nil {
		t.Fatalf("the generation record did not move: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(StateDir(repo), "perf.log")); err != nil || string(got) != "worker output\n" {
		t.Fatalf("log on another disk = %q, %v; want it copied", got, err)
	}
	if _, err := os.Lstat(filepath.Join(StateDir(repo), anchor)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an anchor crossed disks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, anchor)); err != nil {
		t.Fatalf("the old folder lost the anchor that had to stay: %v", err)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err == nil {
		t.Fatal("a fork whose anchor could not move still validates")
	}
}

// Two first opens at once move the records once, whole.
func TestLegacyForkRecordsMoveOnceUnderConcurrentOpens(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	if err := os.Rename(recordsPath(repo), filepath.Join(Home(repo), ".coop")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = MoveLegacyForkRecords(repo) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatalf("the fork lost its records in concurrent moves: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(recordsPath(repo)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) > len(".moving-") && entry.Name()[:len(".moving-")] == ".moving-" {
			t.Fatalf("a staging folder stayed behind: %s", entry.Name())
		}
	}
}

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

// A move that crashed after taking anchors out of the old folder left them in its staging folder.
// The next move gives them back first, so the fork still validates once its records move.
func TestLegacyForkRecordsResumeAMoveThatCrashed(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	legacy := filepath.Join(Home(repo), ".coop")
	if err := os.Rename(recordsPath(repo), legacy); err != nil {
		t.Fatal(err)
	}
	anchor := forkGenerationAnchorName("perf", identity.Generation)
	leftover := filepath.Join(filepath.Dir(recordsPath(repo)), ".moving-"+forkRecordsKey(repo))
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	// It had copied the records, then moved the anchor.
	record, err := os.ReadFile(filepath.Join(legacy, "perf.generation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "perf.generation.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(legacy, anchor), filepath.Join(leftover, anchor)); err != nil {
		t.Fatal(err)
	}
	if err := MoveLegacyForkRecords(repo); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGenerationWorkspace(repo, identity); err != nil {
		t.Fatalf("the anchor an interrupted move held did not come back: %v", err)
	}
	if _, err := os.Lstat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the interrupted move's staging folder stayed: %v", err)
	}
}

// A box that fills an older project's folder cannot make the move copy without bound: past either
// budget the rest stays behind, in the old folder, which is kept so nothing is lost, and the move
// still finishes.
func TestLegacyForkRecordsMoveWithinItsBudgets(t *testing.T) {
	for _, tc := range []struct {
		name          string
		size          int
		copyBudget    int64
		entries, most int
	}{
		{"bytes", 100, 350, 1000, 3},
		{"entries", 1, 0, 6, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "project")
			files := map[string]string{}
			for i := range 8 {
				files[fmt.Sprintf("junk-%d.json", i)] = strings.Repeat("x", tc.size)
			}
			legacyFolder(t, repo, files)
			copyBudget, entryBudget := legacyCopyBudget, legacyEntryBudget
			if tc.copyBudget != 0 {
				legacyCopyBudget = tc.copyBudget
			}
			legacyEntryBudget = tc.entries
			t.Cleanup(func() { legacyCopyBudget, legacyEntryBudget = copyBudget, entryBudget })
			if err := MoveLegacyForkRecords(repo); err != nil {
				t.Fatal(err)
			}
			moved, err := os.ReadDir(recordsPath(repo))
			if err != nil {
				t.Fatal(err)
			}
			if len(moved) == 0 || len(moved) > tc.most {
				t.Fatalf("moved %d records; want at most %d within the %s budget", len(moved), tc.most, tc.name)
			}
			if left, err := os.ReadDir(filepath.Join(Home(repo), ".coop")); err != nil || len(left) != 8 {
				t.Fatalf("old folder = %d entries, %v; want it kept whole when records stayed behind", len(left), err)
			}
		})
	}
}

// An anchor or log swapped for a folder after the move looked at it is moved in as whatever it has
// become; the move removes it where no box can reach, so no box-held folder enters the records.
func TestLegacyForkRecordsDropAnEntrySwappedBeforeItsMove(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	legacy := legacyFolder(t, repo, map[string]string{"perf.log": "output\n"})
	before := moveLegacyEntry
	moveLegacyEntry = func(fromDir int, from string, toDir int, to string) error {
		if err := os.Remove(filepath.Join(legacy, from)); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(legacy, from), 0o700); err != nil {
			return err
		}
		return renameAtNoReplace(fromDir, from, toDir, to)
	}
	t.Cleanup(func() { moveLegacyEntry = before })
	if err := MoveLegacyForkRecords(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(recordsPath(repo), "perf.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a folder swapped in for a log reached the records: %v", err)
	}
}
