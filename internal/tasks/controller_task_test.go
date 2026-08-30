package tasks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirmedControllerTaskHasOneDurableChecklistIdentity(t *testing.T) {
	workspace := t.TempDir()
	draft := ControllerTaskDraft{
		OfferRef:        "record:task_offer:0123456789abcdef",
		Title:           "Fix parser retries",
		Prompt:          "Change the parser without widening its authority.",
		SuccessChecks:   []string{"focused tests pass", "retry remains idempotent"},
		AuthorityLimits: []string{"must not deploy"},
		InstructionRef:  "input:trusted:1",
		SourceRefs:      []string{"artifact:incident:1"},
	}

	first, err := EnsureControllerTask(workspace, draft)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureControllerTask(workspace, draft)
	if err != nil {
		t.Fatal(err)
	}
	if first.Ref != second.Ref || first.Ref.QueueID == "" || first.Ref.TaskID == "" || first.Ref.ID == "" {
		t.Fatalf("durable task identities = first=%+v second=%+v", first, second)
	}
	item, ok := CurrentTask(filepath.Join(workspace, TasksRoot), first.Ref.ID)
	if !ok || item.State != StateTodo || len(item.Subtasks) != 2 {
		t.Fatalf("controller task = %+v, ok=%v", item, ok)
	}
	body, err := os.ReadFile(filepath.Join(item.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{
		"Change the parser without widening its authority.",
		"- [ ] focused tests pass",
		"- [ ] retry remains idempotent",
		"must not deploy",
		"input:trusted:1",
		"artifact:incident:1",
	} {
		if !strings.Contains(string(body), exact) {
			t.Fatalf("task.md does not contain %q:\n%s", exact, body)
		}
	}

	changed := draft
	changed.Prompt = "Different work under the same offer identity."
	if _, err := EnsureControllerTask(workspace, changed); err == nil {
		t.Fatal("changed controller task replay was accepted")
	}
	after, err := os.ReadFile(filepath.Join(item.Dir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(body) {
		t.Fatal("conflicting replay mutated the original task")
	}
}

func TestConfirmedControllerTaskRejectsTaskQueueSymlink(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, ".agent")); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureControllerTask(workspace, ControllerTaskDraft{
		OfferRef: "record:task_offer:unsafe", Title: "Unsafe", Prompt: "Do not escape.",
		SuccessChecks: []string{"remains contained"},
	})
	if err == nil {
		t.Fatal("symlinked task authority was accepted")
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("controller task escaped workspace: %+v", entries)
	}
}
