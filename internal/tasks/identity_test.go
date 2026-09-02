package tasks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTaskIdentitySurvivesLifecycleAndFencesReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent", "tasks")
	item := taskForLease(t, root, StateTodo, "stable")
	first, err := EnsureTaskInstance(root, item)
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTaskDir(root, item, StateInProgress); err != nil {
		t.Fatal(err)
	}
	moved, _ := mustCurrentTask(t, root, item.ID)
	second, err := ReadTaskInstance(root, moved)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("identity changed across lifecycle move: before=%+v after=%+v", first, second)
	}

	if err := os.RemoveAll(moved.Dir); err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(root, StateInProgress, item.ID, "task.md"), "# replacement\n")
	replacement, _ := mustCurrentTask(t, root, item.ID)
	third, err := EnsureTaskInstance(root, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if third.Ref.QueueID != first.Ref.QueueID || third.Ref.TaskID == first.Ref.TaskID || third.Generation == first.Generation {
		t.Fatalf("replacement identity = %+v, want same queue but new task and inode from %+v", third, first)
	}
}

func TestTaskIdentityDistinguishesQueuesAndRejectsLinks(t *testing.T) {
	q1 := filepath.Join(t.TempDir(), "tasks")
	q2 := filepath.Join(t.TempDir(), "tasks")
	a := taskForLease(t, q1, StateTodo, "same")
	b := taskForLease(t, q2, StateTodo, "same")
	ia, err := EnsureTaskInstance(q1, a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := EnsureTaskInstance(q2, b)
	if err != nil {
		t.Fatal(err)
	}
	if ia.Ref.QueueID == ib.Ref.QueueID || ia.Ref.TaskID == ib.Ref.TaskID {
		t.Fatalf("distinct queues/tasks shared identity: %+v %+v", ia, ib)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(a.Dir, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := taskFolderGeneration(linked); err == nil {
		t.Fatal("symlinked task folder was accepted as an identity")
	}
}

func TestCopiedProjectionCarriesLogicalIdentityButNotFolderGeneration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	item := taskForLease(t, root, StateTodo, "copy")
	original, err := EnsureTaskInstance(root, item)
	if err != nil {
		t.Fatal(err)
	}
	projection := filepath.Join(t.TempDir(), "projection")
	if err := ScaffoldStateDirs(projection); err != nil {
		t.Fatal(err)
	}
	if err := copyProjectionFile(filepath.Join(root, QueueIdentityFile), filepath.Join(projection, QueueIdentityFile)); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(projection, StateInProgress, item.ID)
	if err := copyProjectionTree(item.Dir, dst); err != nil {
		t.Fatal(err)
	}
	projectedItem, _ := mustCurrentTask(t, projection, item.ID)
	projected, err := ReadTaskInstance(projection, projectedItem)
	if err != nil {
		t.Fatal(err)
	}
	if projected.Ref != original.Ref || projected.Generation == original.Generation {
		t.Fatalf("projection identity = %+v, canonical = %+v", projected, original)
	}
}
