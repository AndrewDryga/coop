package forkspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordFolders is every folder in repo's fork records, the records folder included: none when it
// does not exist.
func recordFolders(t *testing.T, repo string) []string {
	t.Helper()
	var folders []string
	err := filepath.WalkDir(StateDir(repo), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			folders = append(folders, path)
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return folders
}

// An execution that ends, its launch notes included, leaves no folder behind: before, a plain
// loop's run left its project's records folder holding an empty executions folder forever.
func TestEndedExecutionLeavesNoEmptyFolder(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if before := recordFolders(t, repo); len(before) != 0 {
		t.Fatalf("folders before the run: %v", before)
	}
	record, err := BeginExecution(repo, ExecutionSpec{Kind: ExecutionInteractive, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	held := NoteServiceLaunchHeld(repo, record.ID)
	waiting := NoteServiceLaunchWaiting(repo, record.ID)
	if during := recordFolders(t, repo); len(during) != 4 {
		t.Fatalf("folders during the run: %v; want the records folder, executions and both launch notes", during)
	}
	waiting()
	held()
	if err := EndExecution(repo, record); err != nil {
		t.Fatal(err)
	}
	if after := recordFolders(t, repo); len(after) != 0 {
		t.Fatalf("folders after the run: %v; want none", after)
	}

	// A second execution still holding its record keeps the folder its sibling's end would remove.
	first, err := BeginExecution(repo, ExecutionSpec{Kind: ExecutionInteractive, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BeginExecution(repo, ExecutionSpec{Kind: ExecutionInteractive, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := EndExecution(repo, first); err != nil {
		t.Fatal(err)
	}
	if observations, problems := Executions(repo); len(problems) != 0 || len(observations) != 1 || observations[0].Record.ID != second.ID {
		t.Fatalf("executions after a sibling ended = %+v, %v; want the second one kept", observations, problems)
	}
	if err := EndExecution(repo, second); err != nil {
		t.Fatal(err)
	}
	if after := recordFolders(t, repo); len(after) != 0 {
		t.Fatalf("folders after both runs: %v; want none", after)
	}
}

// A reservation that ends removes its empty folder, under the fork's lifecycle lock that made it.
// The lock file that lock lives in stays, with the records folder holding it: another process may
// hold it, and removing it would split the lock.
func TestEndedReservationLeavesNoEmptyFolderAndKeepsTheLockFile(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "remote")
	identity := ensureTestGeneration(t, repo, "remote")
	before := recordFolders(t, repo)
	record := WorkspaceReservation{
		Version: workspaceReservationVersion, Fork: identity,
		Kind: WorkspaceReservationRemoteSession, OwnerStoreID: testReservationStoreID, OwnerID: "remote_session", CreatedAt: time.Now().UTC(),
	}
	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := ReserveWorkspaceLocked(repo, record); err != nil {
		t.Fatal(err)
	}
	if during := recordFolders(t, repo); len(during) != len(before)+1 {
		t.Fatalf("folders during the reservation: %v; want %v and the reservations folder", during, before)
	}
	if err := RemoveWorkspaceReservationIfMatchesLocked(repo, record); err != nil {
		t.Fatal(err)
	}
	if after := recordFolders(t, repo); len(after) != len(before) {
		t.Fatalf("folders after the reservation: %v; want %v", after, before)
	}
	if _, err := os.Lstat(LockPath(repo, identity.Name)); err != nil {
		t.Fatalf("the held lock file went: %v", err)
	}
}

// A writer whose folder a release removed, empty, between opening it and making its file there gets
// ENOENT from the removed folder, never a file nobody can find, and makes the folder again.
func TestStateFolderWriterMakesAFolderARaceRemovedAgain(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	attempts := 0
	err := createInStateFolder(repo, executionFolder, func(root *os.Root) error {
		attempts++
		if attempts == 1 {
			pruneEmptyStateFolders(repo, executionFolder) // another execution's end, right now
			if _, err := os.Lstat(StateDir(repo)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the race did not remove the records folder: %v", err)
			}
		}
		f, err := root.OpenFile("note", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		return f.Close()
	})
	if err != nil || attempts != 2 {
		t.Fatalf("write across a removal = %v after %d attempts; want it made on the second", err, attempts)
	}
	if _, err := os.Lstat(filepath.Join(executionDir(repo), "note")); err != nil {
		t.Fatalf("the file is not where readers look: %v", err)
	}
}

// A release can remove an empty folder and another writer make a new one at its name between this
// writer's open by descriptor and its open by name. That is the same race, so the writer opens the
// folder again rather than failing with "changed while opening".
func TestStateFolderWriterOpensAFolderMadeAgainUnderIt(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(StateDir(repo), reservationFolder)
	previous := folderChecked
	t.Cleanup(func() { folderChecked = previous })
	remade := 0
	folderChecked = func(folder string) {
		// Runs right after the writer opened sub by descriptor, before it opens it by name.
		if filepath.Clean(folder) == filepath.Clean(StateDir(repo)) && remade == 0 {
			remade++
			if err := os.Remove(sub); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(sub, 0o700); err != nil {
				t.Error(err)
			}
		}
	}
	root, err := OpenStateRoot(repo, reservationFolder, true)
	if err != nil || remade != 1 {
		t.Fatalf("open across a folder made again = %v (made again %d times)", err, remade)
	}
	_ = root.Close()
	// Without the retry the same race is the bare refusal.
	remade = 0
	if _, err := openStateRoot(repo, reservationFolder, true); !errors.Is(err, errStatePathChanged) {
		t.Fatalf("one attempt across a folder made again = %v; want the changed refusal", err)
	}
}

// A writer whose folder is removed under it on every attempt gets a plain answer, not a bare
// "no such file or directory".
func TestStateFolderWriterGivesUpPlainly(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	err := createInStateFolder(repo, executionFolder, func(*os.Root) error {
		pruneEmptyStateFolders(repo, executionFolder)
		return &fs.PathError{Op: "open", Path: "x", Err: os.ErrNotExist}
	})
	if err == nil || !strings.Contains(err.Error(), "Run the command again") || strings.Contains(err.Error(), "no such file") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writer that never got its folder = %v; want the plain answer", err)
	}
}

// A launch wait note that ends removes its folder too, like an execution's notes.
func TestEndedLaunchWaitLeavesNoEmptyFolder(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	done := NoteLaunchWait(repo, "alpha", []string{"box"})
	if during := recordFolders(t, repo); len(during) != 2 {
		t.Fatalf("folders during the wait: %v; want the records folder and the wait notes", during)
	}
	if _, waiting := LaunchWaits(repo, "alpha"); !waiting {
		t.Fatal("the wait was not noted")
	}
	done()
	if after := recordFolders(t, repo); len(after) != 0 {
		t.Fatalf("folders after the wait: %v; want none", after)
	}
}
