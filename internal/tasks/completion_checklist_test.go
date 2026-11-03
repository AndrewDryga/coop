package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// A refusal names what is left, not only how much: each open item's own text in task order,
// never a checked item or a fenced example, a long list capped with a count, an overlong label
// cut short, and an empty checklist told apart from an unfinished one.
func TestIncompleteChecklistNamesOpenItems(t *testing.T) {
	long := strings.Repeat("x", 200)
	for name, tc := range map[string]struct {
		checklist string
		want      string
		absent    []string
	}{
		"one open":     {"- [x] Write the fix\n- [ ] Run the gate\n", `has 1/2 subtasks done; open: "Run the gate"; finish`, []string{"Write the fix"}},
		"several open": {"- [ ] Run the gate\n- [x] Write the fix\n- [ ] Review it\n```\n- [ ] fenced example\n```\n", `has 1/3 subtasks done; open: "Run the gate", "Review it"; finish`, []string{"Write the fix", "fenced example"}},
		"capped":       {"- [ ] a1\n- [ ] a2\n- [ ] a3\n- [ ] a4\n- [ ] a5\n- [ ] a6\n- [ ] a7\n", `open: "a1", "a2", "a3", "a4", "a5" and 2 more; finish`, []string{`"a6"`}},
		"long label":   {"- [ ] " + long + "\n", `open: "` + strings.Repeat("x", maxShownLabel-1) + `…"; finish`, []string{long}},
		"empty":        {"Nothing to tick yet.\n", "has 0/0 subtasks done; record a nonempty, truthful checklist before completing", []string{"open:"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), StateInProgress, "task")
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# Task\n\n## Subtasks\n"+tc.checklist)
			item, ok, err := parseTaskFolder(dir, StateInProgress)
			if err != nil || !ok {
				t.Fatalf("parse = %v, %v", ok, err)
			}
			err = RequireCompletedChecklist(item)
			if !errors.Is(err, ErrIncompleteChecklist) {
				t.Fatalf("refusal = %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q\nwant it to contain %q", err, tc.want)
			}
			for _, absent := range tc.absent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("refusal = %q\nnames %q", err, absent)
				}
			}
		})
	}
}

// A person at the host sees what is still open, the task.md to tick it in and the command to run
// again. The drawn refusal still matches ErrIncompleteChecklist, and the task does not move.
func TestDoneRefusalShowsOpenItemsAndWhereToTickThem(t *testing.T) {
	for name, tc := range map[string]struct{ checklist, cause, tick string }{
		"one open": {"- [x] Write the fix\n- [ ] Run the gate\n",
			"      1 of 2 checklist items is still open:\n      [ ] Run the gate\n",
			"  Finish it, then tick it in:"},
		"two open": {"- [ ] Run the gate\n- [ ] Review it\n",
			"      2 of 2 checklist items are still open:\n      [ ] Run the gate\n      [ ] Review it\n",
			"  Finish them, then tick them in:"},
		"empty": {"",
			"      It has no checklist.\n      List its work as checklist items and tick only what is finished.\n",
			"  Add them to:"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
			root := t.TempDir()
			dir := filepath.Join(root, StateInProgress, "fix-login-retries")
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# Fix login retries\n\n## Subtasks\n"+tc.checklist)
			code, err := tasksFolderMoveWith(root, []string{"fix-login-retries"}, StateDone, "done", "done", claimOptions{})
			var usage *ui.UsageError
			if code == 0 || !errors.As(err, &usage) || !errors.Is(err, ErrIncompleteChecklist) {
				t.Fatalf("done = %d, %v; want the drawn checklist refusal", code, err)
			}
			pad := strings.Repeat(" ", len(tc.tick)-len("  Then run:"))
			want := "\n✗ Could not complete fix-login-retries\n\n" + tc.cause + "\n" +
				tc.tick + " " + displayPath(filepath.Join(dir, "task.md")) + "\n" +
				"  Then run:" + pad + " coop tasks done fix-login-retries\n"
			if got := usage.Render(ui.Palette{}); got != want {
				t.Fatalf("refusal:\n%s\nwant:\n%s", got, want)
			}
			if current, ok := mustCurrentTask(t, root, "fix-login-retries"); !ok || current.State != StateInProgress {
				t.Fatal("refused task moved")
			}
		})
	}
}

// Authority and cleanup tests need otherwise-completable tasks; legacy and
// unfinished checklist tests keep taskForLease or write their own metadata.
func taskWithCompletedChecklist(t *testing.T, root, state, id string) Item {
	t.Helper()
	item := taskForLease(t, root, state, id)
	if err := RewriteSubtasks(item.Dir, []Subtask{{Text: "fixture work verified", Done: true}}); err != nil {
		t.Fatal(err)
	}
	current, _ := mustCurrentTask(t, root, id)
	return current
}

func TestQueuedCompletionRestoresAnUnfinishedChecklist(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	root := t.TempDir()
	item := taskForLease(t, root, StateDone, "raw-move")
	writeTaskFile(t, filepath.Join(item.Dir, "task.md"), "# Task\n- [x] implementation\n- [ ] required check\n")
	writeTaskFile(t, filepath.Join(item.Dir, "tmp", "proof"), "retain\n")
	if err := FinalizeQueuedCompletion(QueuedTask{Root: root, Item: item}); !errors.Is(err, ErrIncompleteChecklist) {
		t.Fatalf("finalize = %v", err)
	}
	current, ok := mustCurrentTask(t, root, item.ID)
	if !ok || current.State != StateInProgress || taskCompletionRecorded(root, current) {
		t.Fatal("raw completion was not restored without a receipt")
	}
	if got := readFileString(filepath.Join(current.Dir, "tmp", "proof")); got != "retain\n" {
		t.Fatalf("evidence = %q", got)
	}
	if got := readFileString(filepath.Join(current.Dir, "state.md")); !strings.Contains(got, "finish the remaining checklist") {
		t.Fatalf("missing actionable recovery: %s", got)
	}
}

func TestIncompleteForkCompletionRefusesAcceptanceButCanResume(t *testing.T) {
	for _, checklist := range []string{"", "- [x] implementation\n- [ ] required check\n"} {
		t.Run(checklist, func(t *testing.T) {
			repo, root, assignment := proposalAssignment(t, "unfinished")
			projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
			writeTaskFile(t, filepath.Join(projected.Dir, "task.md"), "# Task\n"+checklist)
			writeTaskFile(t, filepath.Join(projected.Dir, "tmp", "proof"), "retain\n")
			if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
				t.Fatal(err)
			}
			canonical, _ := mustCurrentTask(t, root, "assigned")
			before := readFileString(filepath.Join(canonical.Dir, "task.md"))
			ownerBefore, _, err := ReadTaskOwnerRecord(root, "assigned")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptForkProjection(repo, root, "assigned", assignment.Owner); !errors.Is(err, ErrIncompleteChecklist) {
				t.Fatalf("accept = %v", err)
			}
			ownerAfter, _, err := ReadTaskOwnerRecord(root, "assigned")
			if err != nil || *ownerBefore.Fork != *ownerAfter.Fork {
				t.Fatalf("refusal changed owner: %#v, %v", ownerAfter, err)
			}
			if got := readFileString(filepath.Join(canonical.Dir, "task.md")); got != before {
				t.Fatal("refusal synced unfinished projection to canonical task")
			}
			if err := PrepareForkProjectionForRun(repo, root, "assigned", assignment.Owner); err != nil {
				t.Fatalf("unfinished projection cannot recover: %v", err)
			}
			restored, ok := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
			if !ok || restored.State != StateInProgress || readFileString(filepath.Join(restored.Dir, "tmp", "proof")) != "retain\n" {
				t.Fatal("recovery did not retain unfinished work and evidence")
			}
		})
	}
}

func TestTrustedCompletionRequiresCurrentChecklist(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "empty", body: "# Legacy task\n", want: "0/0"},
		{name: "required host verification pending", body: "# Task\n" + strings.Repeat("- [x] verified work\n", 7) + "- [ ] required host test\n", want: "7/8"},
		{name: "checked decision", body: "# Decision\n- [x] verified that no source change is required\n"},
		{name: "fenced example is not a requirement", body: "# Task\n- [X] verified work\n\n\x60\x60\x60markdown\n- [ ] example only\n\x60\x60\x60\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
			root := t.TempDir()
			item := taskForLease(t, root, StateInProgress, "checklist")
			// Deliberately pass the stale Item: trusted completion must check
			// the current metadata under its existing authority lock.
			writeTaskFile(t, filepath.Join(item.Dir, "task.md"), test.body)
			writeTaskFile(t, filepath.Join(item.Dir, "state.md"), "resume snapshot\n")
			writeTaskFile(t, filepath.Join(item.Dir, "tmp", "evidence"), "retain\n")
			err := CompleteTrustedTask("", root, item)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				current, ok := mustCurrentTask(t, root, item.ID)
				if !ok || current.State != StateDone || !taskCompletionRecorded(root, current) {
					t.Fatal("complete checklist did not receive trusted completion")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("completion = %v, want checklist refusal containing %q", err, test.want)
			}
			current, ok := mustCurrentTask(t, root, item.ID)
			if !ok || current.State != StateInProgress || taskCompletionRecorded(root, current) {
				t.Fatal("refused completion changed lifecycle or created a receipt")
			}
			for name, want := range map[string]string{
				"task.md": test.body, "state.md": "resume snapshot\n", "tmp/evidence": "retain\n",
			} {
				got, err := os.ReadFile(filepath.Join(item.Dir, name))
				if err != nil || string(got) != want {
					t.Fatalf("refusal changed %s: %q, %v", name, got, err)
				}
			}
		})
	}
}

func TestUnfinishedCompletionPreservesExistingReceipt(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	root := t.TempDir()
	item := taskWithCompletedChecklist(t, root, StateInProgress, "receipt")
	if err := CompleteTrustedTask("", root, item); err != nil {
		t.Fatal(err)
	}
	done, _ := mustCurrentTask(t, root, item.ID)
	before, ok := taskCompletionReceipt(root, done)
	if !ok {
		t.Fatal("fixture has no receipt")
	}
	if err := RewriteSubtasks(done.Dir, []Subtask{{Text: "required follow-up"}}); err != nil {
		t.Fatal(err)
	}
	if err := CompleteTrustedTask("", root, done); !errors.Is(err, ErrIncompleteChecklist) {
		t.Fatalf("completion = %v", err)
	}
	after, ok := taskCompletionReceipt(root, done)
	if !ok || before != after {
		t.Fatalf("refusal changed existing receipt: before=%+v after=%+v present=%v", before, after, ok)
	}
}

func TestForkChecklistCheckedAgainAtPublicationAndLanding(t *testing.T) {
	for _, stage := range []string{"publication", "landing"} {
		t.Run(stage, func(t *testing.T) {
			repo, root, assignment := proposalAssignment(t, "checklist-candidate")
			projected, _ := mustCurrentTask(t, assignment.Owner.Projection, "assigned")
			if err := RewriteSubtasks(projected.Dir, []Subtask{{Text: "verified", Done: true}}); err != nil {
				t.Fatal(err)
			}
			if err := MoveTaskDir(assignment.Owner.Projection, projected, StateDone); err != nil {
				t.Fatal(err)
			}
			if _, err := AcceptForkProjection(repo, root, "assigned", assignment.Owner); err != nil {
				t.Fatal(err)
			}
			var candidate ForkCandidate
			if stage == "landing" {
				var err error
				candidate, _, err = reviewAndPublishForkCandidate(t, repo, assignment.Owner.Fork, strings.Repeat("b", 40), strings.Repeat("c", 40))
				if err != nil {
					t.Fatal(err)
				}
			}
			doneDir := filepath.Join(assignment.Owner.Projection, StateDone, "assigned")
			if err := RewriteSubtasks(doneDir, []Subtask{{Text: "unfinished required verification"}}); err != nil {
				t.Fatal(err)
			}
			if stage == "publication" {
				// Simulate a projection accepted by an older binary. Matching
				// identity/digest alone must not authorize unfinished work.
				result, err := ValidateForkProjection(repo, root, "assigned", assignment.Owner)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := UpdateForkTaskAssignment(root, "assigned", assignment.Owner, func(owner *ForkTaskOwner) error {
					owner.ProjectionDigest = result.Digest
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			canonical, _ := mustCurrentTask(t, root, "assigned")
			before := readFileString(filepath.Join(canonical.Dir, "task.md"))
			ownerBefore, _, err := ReadTaskOwnerRecord(root, "assigned")
			if err != nil {
				t.Fatal(err)
			}
			if stage == "publication" {
				_, _, err = reviewAndPublishForkCandidate(t, repo, assignment.Owner.Fork, strings.Repeat("b", 40), strings.Repeat("c", 40))
			} else {
				err = FinalizeForkCandidateTask(repo, candidate, candidate.Assignments[0])
			}
			if !errors.Is(err, ErrIncompleteChecklist) {
				t.Fatalf("%s = %v", stage, err)
			}
			ownerAfter, _, err := ReadTaskOwnerRecord(root, "assigned")
			if err != nil || *ownerBefore.Fork != *ownerAfter.Fork {
				t.Fatalf("refusal changed owner: %+v %v", ownerAfter, err)
			}
			if readFileString(filepath.Join(canonical.Dir, "task.md")) != before || taskCompletionRecorded(root, canonical) {
				t.Fatal("refusal synced task or created receipt")
			}
		})
	}
}

// A task folder can be named from a box, so the drawn refusal escapes its id and path instead of
// printing a control sequence to the person's terminal.
func TestDoneRefusalEscapesTheTaskFolderName(t *testing.T) {
	id := "fix\x1b[2Jlogin"
	refusal := checklistUsage(&IncompleteChecklistError{ID: id, Done: 0, Total: 1, Open: []string{"Run the gate"}},
		Item{ID: id, Dir: filepath.Join(t.TempDir(), StateInProgress, id)})
	if got := refusal.Render(ui.Palette{}); strings.Contains(got, "\x1b") || !strings.Contains(got, "Run the gate") {
		t.Fatalf("refusal printed a raw control sequence or lost its item:\n%q", got)
	}
}
