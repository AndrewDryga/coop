package taskmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/tasks"
)

func finishChecklist(t *testing.T, root, id string) {
	t.Helper()
	if err := tasks.RewriteSubtasks(filepath.Join(root, tasks.StateInProgress, id), []tasks.Subtask{{Text: "required checks passed", Done: true}}); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteRequiresChecklistAndAllowsSameSessionRepair(t *testing.T) {
	for _, assigned := range []string{"t1", ""} {
		for _, checklist := range []string{"- [x] implementation\n- [ ] required host verification\n", "```markdown\n- [x] example only\n```\n"} {
			t.Run(assigned+"/"+checklist, func(t *testing.T) {
				root := queue(t, map[string]string{"t1": tasks.StateInProgress})
				dir := filepath.Join(root, tasks.StateInProgress, "t1")
				if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("# Task\n\n## Subtasks\n"+checklist), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "tmp", "proof"), []byte("retain"), 0o644); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(dir, "state.md"))
				if err != nil {
					t.Fatal(err)
				}
				sess := newSession(t, newServer(t, root, assigned))
				text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1"})
				if !strings.Contains(text, "task checklist is unfinished") {
					t.Fatalf("refusal = %s", text)
				}
				after, err := os.ReadFile(filepath.Join(dir, "state.md"))
				if err != nil || string(after) != string(before) {
					t.Fatalf("refusal changed state: %q, %v", after, err)
				}
				if got, err := os.ReadFile(filepath.Join(dir, "tmp", "proof")); err != nil || string(got) != "retain" {
					t.Fatalf("refusal removed evidence: %q, %v", got, err)
				}
				if _, err := os.Stat(filepath.Join(root, tasks.StateDone, "t1")); !os.IsNotExist(err) {
					t.Fatalf("refused task moved: %v", err)
				}
				sess.mustCall("tasks_set_subtasks", map[string]any{"id": "t1", "subtasks": []map[string]any{{"text": "required verification passed", "done": true}}})
				sess.mustCall("tasks_complete", map[string]any{"id": "t1"})
			})
		}
	}
}

func TestAssignedCompletionRereadsChecklistAfterBindingValidation(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	finishChecklist(t, root, "t1")
	s := newServer(t, root, "t1")
	s.authority.ValidateAssignedCompletion = func() error {
		return tasks.RewriteSubtasks(filepath.Join(root, tasks.StateInProgress, "t1"), []tasks.Subtask{{Text: "new required check"}})
	}
	sess := newSession(t, s)
	if text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1"}); !strings.Contains(text, "0/1") {
		t.Fatalf("stale checklist was trusted: %s", text)
	}
}
