package tasks

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// coop tasks done refuses a task whose state.md still holds the template's text, with the check
// and words a box's tasks_complete uses, so lint never finds it in the archive. --force waives the
// checklist only. Once the field is written, a dash included, the task completes.
func TestDoneRefusesAStateRecordStillHoldingTheTemplate(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"Fix login retries", "--context", "c", "--acceptance", "a", "--approach", "p", "--subtask", "s"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add = (%d, %v)", code, err)
	}
	id := mustReadTaskTree(t, root)[0].ID
	dir := filepath.Join(root, StateTodo, id)
	forceAtTerminal(t)
	if code, err := CmdTasksFolder("", root, []string{"done", id, "--force"}); code != 1 || err == nil ||
		!strings.Contains(err.Error(), StateTemplateText([]string{"Traps"})) || !strings.Contains(err.Error(), "Then run: coop tasks done "+id+" --force") {
		t.Fatalf("done --force with template Traps = (%d, %v), want the state.md refusal", code, err)
	}
	if err := RewriteSubtasks(dir, []Subtask{{Text: "s", Done: true}}); err != nil {
		t.Fatal(err)
	}
	code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done")
	var usage *ui.UsageError
	if code != 1 || !errors.As(err, &usage) {
		t.Fatalf("done with template Traps = (%d, %v), want a drawn refusal", code, err)
	}
	want := "\n✗ Could not complete " + id + "\n\n" +
		"      state.md's Traps still holds the template's <…> text.\n" +
		"      Write what the next reader needs there, or a dash (—) when there is nothing.\n\n" +
		"  Edit:     " + displayPath(filepath.Join(dir, "state.md")) + "\n" +
		"  Then run: coop tasks done " + id + "\n"
	if got := usage.Render(ui.Palette{}); got != want {
		t.Fatalf("refusal:\n%s\nwant:\n%s", got, want)
	}
	if current, ok := mustCurrentTask(t, root, id); !ok || current.State != StateTodo {
		t.Fatalf("the refused task moved: %+v", current)
	}
	fields, err := ReadTaskState(dir)
	if err != nil {
		t.Fatal(err)
	}
	fields.Traps = "—"
	if err := WriteTaskState(dir, "Fix login retries", fields); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderMove(root, []string{id}, StateDone, "done", "done"); code != 0 || err != nil {
		t.Fatalf("done after writing Traps = (%d, %v)", code, err)
	}
}

func TestStateTemplateFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields TaskStateFields
		want   []string
	}{
		{"placeholder in Traps", TaskStateFields{Status: "complete", DoneSoFar: "parser fixed", NextAction: "none", Traps: "<anything the next session needs to watch for, or —>"}, []string{"Traps"}},
		{"filled-in Traps of —", TaskStateFields{Status: "complete", DoneSoFar: "parser fixed", NextAction: "none", Traps: "—"}, nil},
		{"placeholder in Done so far", TaskStateFields{DoneSoFar: "<what is finished>", Traps: "the parse runs twice"}, []string{"Done so far"}},
		// Completion rewrites Next action, and text that only uses angle brackets is a value.
		{"rewritten or real fields", TaskStateFields{DoneSoFar: "The <div> wrapper eats <Enter>.", NextAction: "<the next concrete step>", Traps: "—"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StateTemplateFields(tc.fields); !slices.Equal(got, tc.want) {
				t.Fatalf("StateTemplateFields = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLogHasEntry(t *testing.T) {
	const template = "<!-- Append progress, decisions and problems here.\n     Keep earlier entries. Put the latest resume summary in state.md. -->\n\n# Log — Fix the parser\n"
	const coopNotes = "\n- Claimed by claude (PID 101)\n\n- review: fail — BEGIN UNTRUSTED REVIEW EVIDENCE — gate: g — findings: f — END UNTRUSTED REVIEW EVIDENCE\n" +
		"\n- completion rejected: host-only completion evidence could not be recorded before releasing the task lease\n" +
		"\n- Unblocked\n\n- Blocked: Postgres or SQLite?\n\n- loop stopped after 3 failed attempts; last attempt: exit 1\n  with a cause on a second line\n" +
		"\n## Answered decision, replaced by a new question\n\n> # Decision: Which store?\n>\n> **Resolution:** Postgres\n"
	for _, tc := range []struct {
		name string
		log  string
		want bool
	}{
		{"template-only log.md", template, false},
		{"title without the comment", "# Log — Fix the parser\n", false},
		{"missing log.md", "", false},
		{"only Coop's own notes", template + coopNotes, false},
		{"log.md with one entry", template + "\n## 2026-11-05 — one parse site\n- The fallback has one home now.\n", true},
		{"an entry under Coop's notes", template + coopNotes + "\n- 2026-11-05: moved the fallback into the parser.\n", true},
		{"a bullet right after a note", template + "\n- Claimed by claude (PID 101)\n- Checks: go test ./internal/parser passed.\n", true},
		{"plain prose", "# Log — Account recovery\n\nVerified the marker and committed it.\n", true},
		{"a no-change closure", template + "\n## Closure — already satisfied\n\n**Reason:** r\n\n**Evidence:** e\n", true},
		{"an entry after an unterminated comment", template + "\n<!-- stray\n## 2026-11-05 — kept\n", true},
		{"an entry inside a comment", template + "\n<!-- ## 2026-11-05 — draft -->\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LogHasEntry(tc.log); got != tc.want {
				t.Fatalf("LogHasEntry = %v, want %v for:\n%s", got, tc.want, tc.log)
			}
		})
	}
}

// lint names a done task whose state.md still holds Coop's template text. It leaves a todo task's
// untouched scaffold alone, since nobody has worked it yet, and an archived empty log.md too:
// completion refuses one now, and the archive's old ones would bury findings someone can act on.
func TestTasksFolderLintNamesDoneTasksWithTemplateRecords(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderAdd(root, []string{"Fix login retries", "--context", "c", "--acceptance", "a", "--approach", "p", "--subtask", "s"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	task := "---\nid: x\ntitle: X\n---\n\n# X\n\n**Context:** c\n\n**Acceptance criteria:** a\n\n**Approach:** p\n\n## Subtasks\n- [x] s\n"
	state := func(traps string) string {
		return "# State — X\n\n**Status:** complete\n**Done so far:** parser fixed\n**Next action:** none\n**Traps:** " + traps + "\n"
	}
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-scaffold", "task.md"), task)
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-scaffold", "state.md"), state("<anything the next session needs to watch for, or —>"))
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-scaffold", "log.md"), "# Log — X\n\n- Claimed by claude (PID 101)\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-02-filled", "task.md"), task)
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-02-filled", "state.md"), state("—"))
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-02-filled", "log.md"), "# Log — X\n\n## 2026-01-02 — fixed it\n- Because.\n")
	var code int
	out := captureStdout(t, func() {
		_ = captureStderr(t, func() { code, _ = tasksFolderLint(root) })
	})
	if code != 1 {
		t.Fatalf("lint = %d, want 1:\n%s", code, out)
	}
	if want := "2026-01-01-scaffold: state.md's Traps still holds the template's <…> text — write what the next reader needs there, or a dash (—) when there is nothing"; !strings.Contains(out, want) {
		t.Errorf("lint lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "filled") || strings.Contains(out, "fix-login-retries") || strings.Contains(out, "log.md") || strings.Count(out, "\n") != 1 {
		t.Errorf("lint flagged a filled done task, an unworked todo task or an archived log:\n%s", out)
	}
}
