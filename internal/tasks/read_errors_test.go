package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestReadTaskTreeRejectsUnsafePresentEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*testing.T, string)
	}{
		{"queue symlink", func(t *testing.T, root string) {
			if err := os.Symlink(t.TempDir(), root); err != nil {
				t.Fatal(err)
			}
		}},
		{"state file", func(t *testing.T, root string) {
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, StateTodo), []byte("hidden\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"state symlink", func(t *testing.T, root string) {
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(root, StateTodo)); err != nil {
				t.Fatal(err)
			}
		}},
		{"task entry symlink", func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, StateTodo), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(root, StateTodo, "redirected")); err != nil {
				t.Fatal(err)
			}
		}},
		{"task file symlink", func(t *testing.T, root string) {
			dir := filepath.Join(root, StateTodo, "linked")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "task.md")
			if err := os.WriteFile(target, []byte("# linked\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, "task.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"task file fifo", func(t *testing.T, root string) {
			dir := filepath.Join(root, StateTodo, "fifo")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(dir, "task.md"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"oversized task file", func(t *testing.T, root string) {
			writeTaskFile(t, filepath.Join(root, StateTodo, "large", "task.md"), strings.Repeat("x", taskMetadataFileLimit+1))
		}},
		{"decision symlink", func(t *testing.T, root string) {
			dir := filepath.Join(root, StateBlocked, "decision")
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# decision\n")
			if err := os.Symlink(filepath.Join(dir, "task.md"), filepath.Join(dir, "decision.md")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "tasks")
			tc.make(t, root)
			if _, err := ReadTaskTree(root); err == nil {
				t.Fatal("unsafe present queue entry was treated as an empty queue")
			}
		})
	}
}

func TestReadBacklogRejectsUnsafeQueueRootAndMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBacklog(root); err == nil {
		t.Fatal("symlinked queue root was treated as an empty backlog")
	}

	root = filepath.Join(t.TempDir(), "tasks")
	dir := filepath.Join(root, StateBacklog, "unsafe")
	writeTaskFile(t, filepath.Join(dir, "task.md"), "# unsafe\n")
	if err := os.Symlink(filepath.Join(dir, "task.md"), filepath.Join(dir, "decision.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBacklog(root); err == nil {
		t.Fatal("unsafe backlog metadata was accepted")
	}
}

func TestUnreadableQueueStopsAssignmentAndDeletion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	done := taskForLease(t, root, StateDone, "keep-done")
	if err := os.Remove(filepath.Join(root, StateTodo)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, StateTodo), []byte("hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), ""); err == nil {
		t.Fatalf("loop assignment = %+v, %v; unreadable queue must return an error", assignment, err)
	}
	repo := filepath.Join(t.TempDir(), "repo")
	workspace, identity := testAssignmentFork(t, repo, "unreadable")
	request := ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	}
	if assignment, err := AssignForkTask([]string{root}, request); err == nil {
		t.Fatalf("fork assignment = %+v, %v; unreadable queue must return an error", assignment, err)
	}
	if code, err := tasksFolderList(root, false); err == nil || code == 0 {
		t.Fatalf("task list = code %d, err %v; unreadable queue must be reported", code, err)
	}
	if code, err := tasksFolderLint(root); err == nil || code == 0 {
		t.Fatalf("task lint = code %d, err %v; unreadable queue must be reported", code, err)
	}
	if removed, err := removeAllDone(root); err == nil || removed != 0 {
		t.Fatalf("remove all done = %d, %v; deletion must stop before mutation", removed, err)
	}
	if _, err := os.Lstat(done.Dir); err != nil {
		t.Fatalf("done task was removed after a failed queue read: %v", err)
	}
}

func TestPublishForkCandidateStopsWhenCanonicalTaskBecomesUnreadable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	item := taskForLease(t, root, StateTodo, "candidate")
	repo := filepath.Join(t.TempDir(), "repo")
	workspace, identity := testAssignmentFork(t, repo, "candidate-read-error")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	projected, _ := mustCurrentTask(t, assignment.Owner.Projection, item.ID)
	if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptForkProjection(repo, root, item.ID, assignment.Owner); err != nil {
		t.Fatal(err)
	}
	canonical, _ := mustCurrentTask(t, root, item.ID)
	if err := os.Remove(filepath.Join(canonical.Dir, "task.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(canonical.Dir, "state.md"), filepath.Join(canonical.Dir, "task.md")); err != nil {
		t.Fatal(err)
	}
	if _, published, err := PublishForkCandidate(repo, identity, strings.Repeat("b", 40), strings.Repeat("c", 40)); err == nil || published {
		t.Fatalf("candidate publication = published %v, err %v", published, err)
	}
	if _, err := os.Lstat(ForkCandidatePath(repo, identity)); !os.IsNotExist(err) {
		t.Fatalf("candidate intent exists after failed canonical read: %v", err)
	}
	if _, ok, err := forkspace.ReadGeneration(repo, identity.Name); err != nil || !ok {
		t.Fatalf("test generation was lost: ok=%v err=%v", ok, err)
	}
}

// A regular file in a lifecycle dir is exactly what Finder (.DS_Store) and editors (swap files)
// leave behind; it cannot redirect authority, so it must never hide the queue. Lint still names the
// non-dotfile ones: a task written as a file is misplaced work, not noise.
func TestReadTaskTreeSkipsStrayRegularFilesAndLintNamesThem(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	done := taskForLease(t, root, StateDone, "keep-done")
	for _, name := range []string{".DS_Store", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(root, StateTodo, name), []byte("stray\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	items, err := ReadTaskTree(root)
	if err != nil || len(items) != 1 || items[0].ID != done.ID {
		t.Fatalf("ReadTaskTree = %+v, %v; want only %s", items, err, done.ID)
	}
	if code, err := tasksFolderLint(root); err != nil || code != 1 {
		t.Fatalf("lint with a misplaced file = %d, %v; want 1", code, err)
	}
	if err := os.Remove(filepath.Join(root, StateTodo, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderLint(root); err != nil || code != 0 {
		t.Fatalf("lint with only a dotfile = %d, %v; want clean", code, err)
	}
	if err := os.MkdirAll(filepath.Join(root, StateBacklog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, StateBacklog, ".DS_Store"), []byte("stray\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if items, err := ReadBacklog(root); err != nil || len(items) != 0 {
		t.Fatalf("ReadBacklog beside a dotfile = %+v, %v; want an empty backlog", items, err)
	}
}
