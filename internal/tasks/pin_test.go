package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Once pinned, a queue is read and written through the folders the pin opened. A box that then
// swaps .agent, the queue root or a state folder for a link to another project's leads no read and
// no write there: the swap either changes nothing the host sees or makes it refuse.
func TestPinnedQueueIgnoresFoldersSwappedAfterThePin(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".agent", "tasks")
	mine := taskForLease(t, root, StateInProgress, "shared")
	writeTaskFile(t, filepath.Join(mine.Dir, "log.md"), "# Log\n")
	other := t.TempDir()
	theirs := filepath.Join(other, ".agent", "tasks", StateInProgress, "shared")
	writeTaskFile(t, filepath.Join(theirs, "task.md"), "# Theirs\n")
	writeTaskFile(t, filepath.Join(theirs, "log.md"), "theirs\n")
	key, err := LeaseAuthorityKey(root, "shared")
	if err != nil {
		t.Fatal(err)
	}
	release, err := PinQueues(workspace, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	for _, rel := range []string{".agent", filepath.Join(".agent", "tasks"), filepath.Join(".agent", "tasks", StateInProgress)} {
		path := filepath.Join(workspace, rel)
		if err := os.Rename(path, path+".kept"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(other, rel), path); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(mine.Dir, "log.md")); err != nil || readFileString(filepath.Join(mine.Dir, "task.md")) != "# Theirs\n" {
			t.Fatalf("swapped %s: by name the queue does not lead to the other project (%v)", rel, err)
		}
		items, err := ReadTaskTree(root)
		for _, item := range items {
			if item.Title == "Theirs" {
				t.Fatalf("swapped %s: the pinned queue read the other project's task", rel)
			}
		}
		if err == nil && len(items) != 1 {
			t.Fatalf("swapped %s: read %d tasks, want the pinned one", rel, len(items))
		}
		// Some callers resolve a queue's links first; its real path stays pinned too.
		real, err := filepath.EvalSymlinks(workspace)
		if err != nil {
			t.Fatal(err)
		}
		if opened, err := OpenTaskMetadataRoot(filepath.Join(real, ".agent", "tasks", StateInProgress, "shared")); err == nil {
			data, readErr := ReadTaskMetadataFile(opened, "task.md")
			opened.Close()
			if readErr == nil && string(data) == "# Theirs\n" {
				t.Fatalf("swapped %s: the task folder's real path read the other project", rel)
			}
		}
		_ = AppendTaskLogEntry(mine.Dir, "## pinned")
		if got, err := LeaseAuthorityKey(root, "shared"); err != nil || got != key {
			t.Fatalf("swapped %s: lease key = %q, %v; want the pinned queue's %q", rel, got, err, key)
		}
		if got := readFileString(filepath.Join(theirs, "log.md")); got != "theirs\n" {
			t.Fatalf("swapped %s: a write reached the other project: %q", rel, got)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".kept", path); err != nil {
			t.Fatal(err)
		}
	}
	// The .agent and queue-root swaps left the pinned folders in place, so those writes landed.
	if got := readFileString(filepath.Join(mine.Dir, "log.md")); strings.Count(got, "## pinned") != 2 {
		t.Fatalf("the pinned queue's log = %q, want the two writes made while the pin held", got)
	}
}

// A box can swap a checked folder for a FIFO before the host opens it. The walk opens each folder
// without waiting, so it fails at once instead of hanging the call.
func TestOpeningAFolderSwappedForAFIFOFailsAtOnce(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "state"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	done := make(chan error, 1)
	go func() {
		opened, err := openSubrootNoWait(root, "state")
		if opened != nil {
			opened.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO opened as a folder")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO as a folder waited")
	}
}
