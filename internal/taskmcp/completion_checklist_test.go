package taskmcp

import (
	"maps"
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
		for _, tc := range []struct{ checklist, names string }{
			{"- [x] implementation\n- [ ] required host verification\n", `open: "required host verification";`},
			{"```markdown\n- [x] example only\n```\n", "has 0/0 subtasks done; record a nonempty, truthful checklist"},
		} {
			t.Run(assigned+"/"+tc.checklist, func(t *testing.T) {
				root := queue(t, map[string]string{"t1": tasks.StateInProgress})
				dir := filepath.Join(root, tasks.StateInProgress, "t1")
				if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("# Task\n\n## Subtasks\n"+tc.checklist), 0o644); err != nil {
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
				text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{}})
				if !strings.Contains(text, "task checklist is unfinished") || !strings.Contains(text, tc.names) {
					t.Fatalf("refusal = %s; want it to contain %q", text, tc.names)
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
				sess.mustCall("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{}})
			})
		}
	}
}

// Workers ticked a required check they could not run, with the excuse appended to its text. Now
// they must name every unrun check, and any named one refuses completion before anything moves.
func TestCompletionRefusesUnrunRequiredChecks(t *testing.T) {
	for _, assigned := range []string{"t1", ""} {
		t.Run("assigned="+assigned, func(t *testing.T) {
			root := queue(t, map[string]string{"t1": tasks.StateInProgress})
			finishChecklist(t, root, "t1")
			s := newServer(t, root, assigned)
			validated := 0
			s.authority.ValidateAssignedCompletion = func(CompletionClaim) error { validated++; return nil }
			sess := newSession(t, s)
			before := taskFiles(t, root)

			missing := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1"})
			if !strings.Contains(missing, "missing required fields: unrun_checks") || !strings.Contains(missing, "Send [] only when all of them ran") {
				t.Fatalf("omitted unrun_checks = %s", missing)
			}
			// Null must never read as "every check ran", and each entry must fit a task file.
			for _, bad := range []any{nil, "./run gate", []any{nil}, []string{""}, []string{"line\nbreak"}, make([]string, maxItems+1)} {
				if text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1", "unrun_checks": bad}); strings.Contains(text, "has not moved") {
					t.Fatalf("invalid unrun_checks %v was read as a list of checks: %s", bad, text)
				}
			}
			unrun := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{"./run gate tooling on the host", "make e2e"}})
			for _, want := range []string{"t1 has not moved", "./run gate tooling on the host; make e2e", "call tasks_block on that missing authority"} {
				if !strings.Contains(unrun, want) {
					t.Fatalf("unrun refusal lacks %q: %s", want, unrun)
				}
			}
			if !maps.Equal(before, taskFiles(t, root)) || validated != 0 {
				t.Fatalf("a refused completion changed the task or reached binding validation %d time(s)", validated)
			}
			sess.mustCall("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{}})
			if _, err := os.Stat(filepath.Join(root, tasks.StateDone, "t1")); err != nil {
				t.Fatalf("an empty unrun_checks did not complete the task: %v", err)
			}
		})
	}
}

// A premature call still learns its checklist is open first: the live task-channel contract counts
// that refusal, and an honest worker names the very check it has not run yet.
func TestOpenChecklistRefusesBeforeUnrunChecks(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	sess := newSession(t, newServer(t, root, "t1"))
	text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{"second"}})
	if !strings.HasPrefix(text, "task checklist is unfinished: t1 has ") {
		t.Fatalf("premature completion refusal = %s", text)
	}
}

func TestAssignedCompletionRereadsChecklistAfterBindingValidation(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	finishChecklist(t, root, "t1")
	s := newServer(t, root, "t1")
	s.authority.ValidateAssignedCompletion = func(CompletionClaim) error {
		return tasks.RewriteSubtasks(filepath.Join(root, tasks.StateInProgress, "t1"), []tasks.Subtask{{Text: "new required check"}})
	}
	sess := newSession(t, s)
	if text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{}}); !strings.Contains(text, "0/1") {
		t.Fatalf("stale checklist was trusted: %s", text)
	}
}
