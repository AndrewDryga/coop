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

// Coop's scaffold state.md: Traps (and Next action) still hold the template's <…> text.
const scaffoldState = "<!-- Replace this short summary at each checkpoint so another session can continue.\n     Keep the detailed history in log.md. -->\n\n" +
	"# State — Title of t1\n\n**Status:** in progress\n**Done so far:** —\n**Next action:** <the next concrete step>\n**Traps:** <anything the next session needs to watch for, or —>\n"

// A log.md holding Coop's template and only lines Coop writes itself: a claim, a review verdict and
// a refused completion. A reworked task that kept only these was closed again without an entry.
const coopOnlyLog = "<!-- Append progress, decisions and problems here.\n     Keep earlier entries. Put the latest resume summary in state.md. -->\n\n# Log — Title of t1\n\n" +
	"- Claimed by claude (PID 4242)\n\n- review: fail — BEGIN UNTRUSTED REVIEW EVIDENCE — gate: g — findings: f — END UNTRUSTED REVIEW EVIDENCE\n\n" +
	"- completion rejected: this iteration also moved an unleased task\n"

// One night's final review reopened 7 of 12 completed tasks only because log.md held nothing but
// Coop's template, and completed state.md files still read the template's Traps text.
func TestCompletionRefusesTemplateStateAndALogWithoutAnEntry(t *testing.T) {
	for _, assigned := range []string{"t1", ""} {
		t.Run("assigned="+assigned, func(t *testing.T) {
			root := queue(t, map[string]string{"t1": tasks.StateInProgress})
			finishChecklist(t, root, "t1")
			dir := filepath.Join(root, tasks.StateInProgress, "t1")
			for name, body := range map[string]string{"state.md": scaffoldState, "log.md": coopOnlyLog} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sess := newSession(t, newServer(t, root, assigned))
			before := taskFiles(t, root)
			text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{}})
			for _, want := range []string{
				"t1 has not moved", "state.md's Traps still holds the template's <…> text", `send traps: the gotchas for the next agent, or "—" when there are none`,
				"log.md has no entry yet: append what changed and why with tasks_append_log",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("completion refusal lacks %q: %s", want, text)
				}
			}
			// Done so far is "—" and completion rewrites Next action, so neither is named.
			if strings.Contains(text, "Done so far") || strings.Contains(text, "Next action") || !maps.Equal(before, taskFiles(t, root)) {
				t.Fatalf("refusal named a field completion handles, or changed the task: %s", text)
			}
			// "—" is a real Traps value; then the log alone is missing.
			sess.mustCall("tasks_update_state", map[string]any{"id": "t1", "traps": "—"})
			if text := sess.mustRefuse("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{}}); strings.Contains(text, "Traps") || !strings.Contains(text, "log.md has no entry yet") {
				t.Fatalf("refusal after filling Traps = %s", text)
			}
			sess.mustCall("tasks_append_log", map[string]any{"id": "t1", "entry": "## 2026-11-05 — fixed the parser\n- One parse site, so the fallback has one home."})
			sess.mustCall("tasks_complete", map[string]any{"id": "t1", "unrun_checks": []string{}})
			if _, err := os.Stat(filepath.Join(root, tasks.StateDone, "t1")); err != nil {
				t.Fatalf("a filled record did not complete: %v", err)
			}
		})
	}
}

// A no-change outcome's reason and evidence become its log entry, so it needs none first; template
// text in Traps would still outlive the closure, which rewrites only Done so far.
func TestNoChangeCompletionWritesItsOwnLogEntry(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	finishChecklist(t, root, "t1")
	dir := filepath.Join(root, tasks.StateInProgress, "t1")
	if err := os.WriteFile(filepath.Join(dir, "log.md"), []byte(coopOnlyLog), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.md"), []byte(scaffoldState), 0o644); err != nil {
		t.Fatal(err)
	}
	sess := newSession(t, newServer(t, root, "t1"))
	args := map[string]any{"id": "t1", "unrun_checks": []string{}, "outcome": "already_satisfied", "reason": "The base already parses it.", "evidence": "Focused parser test passed."}
	if text := sess.mustRefuse("tasks_complete", args); !strings.Contains(text, "Traps") || strings.Contains(text, "log.md") {
		t.Fatalf("no-change refusal = %s", text)
	}
	sess.mustCall("tasks_update_state", map[string]any{"id": "t1", "traps": "—"})
	sess.mustCall("tasks_complete", args)
	log, err := os.ReadFile(filepath.Join(root, tasks.StateDone, "t1", "log.md"))
	if err != nil || !tasks.LogHasEntry(string(log)) {
		t.Fatalf("closed log has no entry: %q, %v", log, err)
	}
}

// The final refresh is where a worker writes "complete", so that is where template text is refused.
func TestCompleteStatusRefusesTemplateState(t *testing.T) {
	root := queue(t, map[string]string{"t1": tasks.StateInProgress})
	path := filepath.Join(root, tasks.StateInProgress, "t1", "state.md")
	if err := os.WriteFile(path, []byte(scaffoldState), 0o644); err != nil {
		t.Fatal(err)
	}
	sess := newSession(t, newServer(t, root, "t1"))
	sess.mustCall("tasks_update_state", map[string]any{"id": "t1", "status": "in progress — tests next"})
	before, _ := os.ReadFile(path)
	text := sess.mustRefuse("tasks_update_state", map[string]any{"id": "t1", "status": "complete", "next_action": "none"})
	for _, want := range []string{"update state of t1: a complete status keeps no template text", "state.md's Traps still holds the template's <…> text", "send traps"} {
		if !strings.Contains(text, want) {
			t.Errorf("complete status refusal lacks %q: %s", want, text)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("the refused update wrote state.md: %q", after)
	}
	sess.mustCall("tasks_update_state", map[string]any{"id": "t1", "status": "complete — all checks green", "next_action": "none", "traps": "—"})
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
