package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
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
			code, err := tasksFolderMoveWith(root, []string{"fix-login-retries"}, StateDone, "done", claimOptions{})
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

// Loop start names a second binding only where it can still cost a review: a task that will still
// complete, a done task a pending review may reopen, and a done task bound after one (its reopen
// replays that history). Old archived history, which nothing reopens, stays quiet.
func TestExtraBindingsNamesOnlyTasksAReviewCanReach(t *testing.T) {
	repo, git := gitRepo(t)
	bind := func(id string) { git("commit", "-q", "--allow-empty", "-m", "Work\n\nCoop-Task: "+id) }
	bind("archived")
	bind("archived")
	bind("subject")
	bind("bound-after")
	bind("bound-after")
	bind("doing")
	bind("doing")
	snapshot := map[string]string{
		"archived": StateDone, "subject": StateDone, "bound-after": StateDone, "doing": StateInProgress,
	}
	ids := func(subjects ...string) []string {
		var out []string
		for _, e := range ExtraBindings(repo, snapshot, subjects) {
			out = append(out, e.TaskID)
		}
		return out
	}
	for _, c := range []struct {
		subjects, want []string
	}{
		{nil, []string{"doing"}},
		{[]string{"subject"}, []string{"bound-after", "doing"}},
		{[]string{"archived"}, []string{"archived", "bound-after", "doing"}},
	} {
		if got := ids(c.subjects...); !slices.Equal(got, c.want) {
			t.Errorf("review subjects %v: listed %v, want %v", c.subjects, got, c.want)
		}
	}
}

// A follow-up commit carrying the task's own trailer is refused when the task is closed, not later,
// when a final review's reopen of it (or of an older task) fails on the second binding. Dropping the
// follow-up's trailer lets the same close through, and the loop's startup names the task too.
func TestDoneRefusesASecondBinding(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, git := gitRepo(t)
	root := t.TempDir()
	id := "fix-login-retries"
	git("commit", "-q", "--allow-empty", "-m", "Fix login retries\n\nCoop-Task: "+id)
	first := forkspace.GitOut(repo, "rev-parse", "--short", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "Cover the retry cap\n\nCoop-Task: "+id)
	second := forkspace.GitOut(repo, "rev-parse", "--short", "HEAD")
	taskWithCompletedChecklist(t, root, StateInProgress, id)
	taskWithCompletedChecklist(t, root, StateInProgress, "bound-once")
	git("commit", "-q", "--allow-empty", "-m", "Other work\n\nCoop-Task: bound-once")

	snapshot, err := QueueSnapshot([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if extra := ExtraBindings(repo, snapshot, nil); len(extra) != 1 || extra[0].TaskID != id || extra[0].Found != 2 {
		t.Fatalf("startup report = %+v; want only %s with 2 commits", extra, id)
	}
	code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo})
	var usage *ui.UsageError
	if code == 0 || !errors.As(err, &usage) {
		t.Fatalf("done = %d, %v; want the drawn refusal", code, err)
	}
	got := usage.Render(ui.Palette{})
	for _, want := range []string{"Could not complete " + id, "has 2 commits carrying \"Coop-Task: " + id + "\"",
		second + " by ", first + " by ", "Make the follow-up unbound by dropping its trailer, or file a new task for that work, then retry: coop tasks done " + id,
		"Are those commits published? You can close it as it stands: coop tasks done " + id + " --force"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal omitted %q:\n%s", want, got)
		}
	}
	if current, ok := mustCurrentTask(t, root, id); !ok || current.State != StateInProgress {
		t.Fatal("refused task moved")
	}
	// A box's tasks_complete on another task takes the same path.
	box, _ := mustCurrentTask(t, root, id)
	if err := CompleteBoxTask(repo, root, box, ClaimActor{}); !errors.As(err, new(*ExtraBindingsError)) {
		t.Fatalf("box completion = %v; want the second binding refused", err)
	}

	git("reset", "-q", "--soft", "HEAD~2")
	git("commit", "-q", "--allow-empty", "-m", "Cover the retry cap")
	git("commit", "-q", "--allow-empty", "-m", "Other work\n\nCoop-Task: bound-once")
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo}); code != 0 || err != nil {
		t.Fatalf("done after the follow-up was unbound = %d, %v", code, err)
	}
}

// Published history cannot drop a trailer, so a person's --force closes a task bound twice and its
// log.md says so. An agent's close of the same task is still refused.
func TestForcedDoneAcceptsASecondBinding(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, git := gitRepo(t)
	root := t.TempDir()
	id := "published-twice"
	git("commit", "-q", "--allow-empty", "-m", "Fix login retries\n\nCoop-Task: "+id)
	first := forkspace.GitOut(repo, "rev-parse", "--short", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "Cover the retry cap\n\nCoop-Task: "+id)
	second := forkspace.GitOut(repo, "rev-parse", "--short", "HEAD")
	taskWithCompletedChecklist(t, root, StateInProgress, id)

	item, _ := mustCurrentTask(t, root, id)
	if err := CompleteBoxTask(repo, root, item, ClaimActor{}); !errors.As(err, new(*ExtraBindingsError)) {
		t.Fatalf("box completion = %v; want the second binding refused", err)
	}
	// --from-fork needs exactly one commit, so its refusal does not offer --force.
	if got := trustedCompletionError(extraBindings(repo, id, []string{first, second}), item, claimOptions{fromFork: true}).Error(); strings.Contains(got, "--force") {
		t.Errorf("--from-fork refusal offers --force:\n%s", got)
	}
	// A running loop's final review would take the close as a subject it cannot enroll.
	loop, err := BeginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo, forceClose: true})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "Try again when the loop ends: coop tasks done "+id+" --force") {
		t.Fatalf("done --force while a loop runs = %d, %v; want the wait", code, err)
	}
	if current, ok := mustCurrentTask(t, root, id); !ok || current.State != StateInProgress {
		t.Fatal("refused task moved")
	}
	if err := loop.Abandon(); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() {
		code, err = tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo, forceClose: true})
	})
	if code != 0 || err != nil {
		t.Fatalf("done --force = %d, %v; want the close", code, err)
	}
	if want := "Its log.md names the 2 commits that carry its trailer. A later review cannot reopen it."; !strings.Contains(out, want) {
		t.Errorf("output omitted %q:\n%s", want, out)
	}
	done, ok := mustCurrentTask(t, root, id)
	if !ok || done.State != StateDone || !taskCompletionRecorded(root, done) {
		t.Fatal("forced close did not reach done with its receipt")
	}
	log := readFileString(filepath.Join(done.Dir, "log.md"))
	for _, want := range []string{"Closed with coop tasks done --force. 2 commits carry its Coop-Task trailer (", first + " by ", second + " by ", "), so a later review cannot reopen it or an older task whose review spans them."} {
		if !strings.Contains(log, want) {
			t.Errorf("log.md omitted %q:\n%s", want, log)
		}
	}
	// Forcing it again finishes nothing new and records no second note.
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo, forceClose: true}); code != 0 || err != nil {
		t.Fatalf("second done --force = %d, %v", code, err)
	}
	if n := strings.Count(readFileString(filepath.Join(done.Dir, "log.md")), "Closed with coop tasks done --force"); n != 1 {
		t.Fatalf("log.md has %d forced-close notes, want 1", n)
	}
}

// In emisar a person unblocked a task whose partial commit from before the decision was already
// bound, and the work done after it carried the same trailer: nothing said the task was bound until
// a final review failed on the second binding. Unblock and claim now say it, and how to commit more.
func TestUnblockAndClaimSayTheTaskIsAlreadyBound(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, git := gitRepo(t)
	root := t.TempDir()
	id := "partial-before-decision"
	git("commit", "-q", "--allow-empty", "-m", "Start the importer\n\nCoop-Task: "+id)
	bound := forkspace.GitOut(repo, "rev-parse", "HEAD")
	blocked := taskForLease(t, root, StateBlocked, id)
	writeTaskFile(t, filepath.Join(blocked.Dir, "decision.md"), "# Decision\n\n**Resolution:** <!-- unresolved -->\n")
	taskForLease(t, root, StateTodo, "not-bound-yet")

	run := func(args ...string) string {
		t.Helper()
		return captureStderr(t, func() {
			if code, err := CmdTasksFolder(repo, root, args); code != 0 || err != nil {
				t.Fatalf("coop tasks %s = %d, %v", strings.Join(args, " "), code, err)
			}
		})
	}
	for _, args := range [][]string{{"unblock", id, "keep the importer"}, {"claim", id}} {
		out := run(args...)
		for _, want := range []string{
			"Its commit " + bound[:12] + " already carries Coop-Task: " + id + ", and a task binds one commit.",
			"Commit further work on it without that trailer, and name the task in the message body instead.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("coop tasks %s output lacks %q:\n%s", args[0], want, out)
			}
		}
	}
	if out := run("claim", "not-bound-yet"); strings.Contains(out, "already carries") {
		t.Errorf("a task with no commit was called bound:\n%s", out)
	}
	// A second binding is named, with the remedy done gives when it refuses one.
	git("commit", "-q", "--allow-empty", "-m", "Finish the importer\n\nCoop-Task: "+id)
	out := run("claim", id)
	for _, want := range []string{"has 2 commits carrying \"Coop-Task: " + id + "\"", "Make the follow-up unbound by dropping its trailer, or file a new task for that work."} {
		if !strings.Contains(out, want) {
			t.Errorf("claim of a task bound twice lacks %q:\n%s", want, out)
		}
	}
}

// Authority and cleanup tests need otherwise-completable tasks; tests of a missing or
// unfinished checklist keep taskForLease or write their own metadata.
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
		{name: "empty", body: "# Task with no checklist\n", want: "0/0"},
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
				// Simulate a projection accepted with its checklist unfinished. Matching
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
		Item{ID: id, Dir: filepath.Join(t.TempDir(), StateInProgress, id)}, "coop tasks done "+ui.SafeInline(id))
	if got := refusal.Render(ui.Palette{}); strings.Contains(got, "\x1b") || !strings.Contains(got, "Run the gate") {
		t.Fatalf("refusal printed a raw control sequence or lost its item:\n%q", got)
	}
}

// The checklist refusal offers the call's own retry: a --from-fork close is told to run
// --from-fork again, not a plain done that is refused for another reason.
func TestChecklistRefusalKeepsFromFork(t *testing.T) {
	task := Item{ID: "fork-task", Dir: filepath.Join(t.TempDir(), StateInProgress, "fork-task")}
	err := trustedCompletionError(&IncompleteChecklistError{ID: task.ID, Total: 1, Open: []string{"Run the gate"}},
		task, claimOptions{fromFork: true})
	var usage *ui.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("refusal = %v, want a drawn usage error", err)
	}
	if got := usage.Render(ui.Palette{}); !strings.Contains(got, "coop tasks done fork-task --from-fork") {
		t.Fatalf("refusal lost --from-fork:\n%s", got)
	}
}

// forceAtTerminal makes done --force see a person at a terminal for one test.
func forceAtTerminal(t *testing.T) {
	t.Helper()
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsTerminal = old })
}

// A person's --force closes a task whose checklist is open or empty: the flag before or after the
// id, from todo, in progress, blocked or a folder already in done. The checklist stays as it was,
// the log says what was open, and state.md, tmp/ and the completion receipt finish as usual.
// Forcing it again finds it done and leaves no second note; a plain done still refuses its open
// checklist, saying the task is already done, and leaves the receipt as it was.
func TestDoneForceClosesAnUnfinishedChecklistAndRecordsIt(t *testing.T) {
	for name, tc := range map[string]struct {
		state, checklist string
		forceFirst       bool
		note, notice     string
	}{
		"open, in progress": {StateInProgress, "- [x] Write the fix\n- [ ] Run the gate\n", false,
			` while 1 of 2 checklist items was open: "Run the gate".`, "Its log.md names the checklist item still open."},
		"open, blocked, flag first": {StateBlocked, "- [ ] Run the gate\n- [ ] Review it\n", true,
			` while 2 of 2 checklist items were open: "Run the gate", "Review it".`, "Its log.md names the 2 checklist items still open."},
		"empty, todo": {StateTodo, "", false, " without a checklist.", "Its log.md notes that it had no checklist."},
		"already done": {StateDone, "- [ ] Run the gate\n", false,
			` while 1 of 1 checklist items was open: "Run the gate".`, "Its log.md names the checklist item still open."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
			forceAtTerminal(t)
			root := t.TempDir()
			id := "fix-login-retries"
			dir := filepath.Join(root, tc.state, id)
			body := "# Fix login retries\n\n## Subtasks\n" + tc.checklist
			writeTaskFile(t, filepath.Join(dir, "task.md"), body)
			if tc.state == StateBlocked {
				writeTaskFile(t, filepath.Join(dir, "decision.md"), "# Decision: Ship it?\n")
			}
			writeTaskFile(t, filepath.Join(dir, "tmp", "scratch"), "scratch\n")
			args := []string{"done", id, "--force"}
			if tc.forceFirst {
				args = []string{"done", "--force", id}
			}
			out := captureStderr(t, func() {
				if code, err := CmdTasksFolder("", root, args); code != 0 || err != nil {
					t.Fatalf("done --force = %d, %v", code, err)
				}
			})
			if !strings.Contains(out, "Closed with --force. "+tc.notice) {
				t.Errorf("output does not say what the forced close recorded:\n%s", out)
			}
			done := filepath.Join(root, StateDone, id)
			if got := readFileString(filepath.Join(done, "task.md")); got != body {
				t.Fatalf("task.md changed:\n%s", got)
			}
			if log := readFileString(filepath.Join(done, "log.md")); !strings.Contains(log, "- Closed with coop tasks done --force") || !strings.Contains(log, tc.note) {
				t.Fatalf("log.md = %q\nwant the forced close ending %q", log, tc.note)
			}
			if pathExists(filepath.Join(done, "tmp")) {
				t.Fatal("tmp/ kept after a forced close")
			}
			if state := readFileString(filepath.Join(done, "state.md")); !strings.Contains(state, "complete") {
				t.Fatalf("state.md not finalized: %q", state)
			}
			current, ok := mustCurrentTask(t, root, id)
			if !ok || current.State != StateDone || !taskCompletionRecorded(root, current) {
				t.Fatal("forced close did not receive trusted completion")
			}
			out = captureStderr(t, func() {
				if code, err := CmdTasksFolder("", root, []string{"done", id, "--force"}); code != 0 || err != nil {
					t.Fatalf("done --force again = %d, %v; want it already done", code, err)
				}
			})
			if !strings.Contains(out, "is already done.") || strings.Contains(out, "Closed with --force") {
				t.Errorf("done --force again said:\n%s", out)
			}
			if n := strings.Count(readFileString(filepath.Join(done, "log.md")), "Closed with coop tasks done --force"); n != 1 {
				t.Fatalf("log.md has %d forced-close notes, want 1", n)
			}
			current, _ = mustCurrentTask(t, root, id)
			before, _ := taskCompletionReceipt(root, current)
			code, err := CmdTasksFolder("", root, []string{"done", id})
			var usage *ui.UsageError
			if code == 0 || !errors.Is(err, ErrIncompleteChecklist) || !errors.As(err, &usage) || !strings.Contains(usage.Headline, "is already done") {
				t.Fatalf("plain done on a forced close = %d, %v; want the already-done checklist refusal", code, err)
			}
			if after, ok := taskCompletionReceipt(root, current); !ok || after != before {
				t.Fatal("the plain refusal changed the forced close's receipt")
			}
		})
	}
}

// --force is a person's tool. Without a terminal, which is how agents run commands, it is
// refused before anything is read or moved.
func TestDoneForceNeedsATerminal(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "unfinished", "task.md"), "# Unfinished\n\n## Subtasks\n- [ ] Run the gate\n")
	code, err := CmdTasksFolder("", root, []string{"done", "unfinished", "--force"})
	if code != 2 || !errors.Is(err, errForceCloseNeedsTerminal) {
		t.Fatalf("done --force without a terminal = %d, %v; want the terminal refusal", code, err)
	}
	if !pathExists(filepath.Join(root, StateInProgress, "unfinished")) || pathExists(filepath.Join(root, StateInProgress, "unfinished", "log.md")) {
		t.Fatal("a refused forced close moved the task or left a note")
	}
}

// --force overrides nothing when nothing is open: a finished checklist completes as usual and
// leaves no forced-close note.
func TestDoneForceWithAFinishedChecklistLeavesNoNote(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	forceAtTerminal(t)
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "finished", "task.md"), "# Finished\n\n## Subtasks\n- [x] Run the gate\n")
	out := captureStderr(t, func() {
		if code, err := CmdTasksFolder("", root, []string{"done", "finished", "--force"}); code != 0 || err != nil {
			t.Fatalf("done --force = %d, %v", code, err)
		}
	})
	if strings.Contains(out, "--force") || strings.Contains(readFileString(filepath.Join(root, StateDone, "finished", "log.md")), "--force") {
		t.Fatalf("a finished checklist was reported as forced:\n%s", out)
	}
}

// --force waives only the checklist: a lease another controller holds, a fork's assigned task and
// audit-reopen authority all still refuse, leaving the task where it was and writing no note.
func TestDoneForceKeepsEveryOtherRefusal(t *testing.T) {
	refusedInPlace := func(t *testing.T, root, id, state string) {
		t.Helper()
		current, ok := mustCurrentTask(t, root, id)
		if !ok || current.State != state || strings.Contains(readFileString(filepath.Join(current.Dir, "log.md")), "--force") {
			t.Fatal("a refused forced close moved the task or left a note")
		}
	}
	t.Run("leased", func(t *testing.T) {
		t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
		forceAtTerminal(t)
		root := filepath.Join(t.TempDir(), "tasks")
		item := taskForLease(t, root, StateInProgress, "leased")
		lease, _, err := TryTaskLease(root, item, testLeaseOwner())
		if err != nil || lease == nil {
			t.Fatalf("lease fixture: %v, %v", lease, err)
		}
		defer func() { _ = lease.Release() }()
		code, err := CmdTasksFolder("", root, []string{"done", "leased", "--force"})
		if code == 0 || !errors.Is(err, ErrTaskLeased) || !strings.Contains(err.Error(), "then retry: coop tasks done leased --force") {
			t.Fatalf("done --force on a leased task = %d, %v; want the lease refusal with a forced retry", code, err)
		}
		refusedInPlace(t, root, "leased", StateInProgress)
	})
	t.Run("final review running", func(t *testing.T) {
		forceAtTerminal(t)
		repo := initRepo(t)
		root := filepath.Join(repo, TasksRoot)
		subject := taskWithCompletedChecklist(t, root, StateDone, "review-subject")
		if err := CompleteTrustedTask("", root, subject); err != nil {
			t.Fatal(err)
		}
		taskForLease(t, root, StateInProgress, "unfinished")
		plan := pendingReviewTestPlan(t, repo, root, forkspace.GitOut(repo, "rev-parse", "HEAD"))
		windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, []string{subject.ID}, plan)
		if err != nil {
			t.Fatal(err)
		}
		code, err := CmdTasksFolder("", root, []string{"done", "unfinished", "--force"})
		if code == 0 || err == nil || !strings.Contains(err.Error(), "final review is running") ||
			!strings.Contains(err.Error(), "Try again when that review ends: coop tasks done unfinished --force") {
			t.Fatalf("done --force during a final review = %d, %v", code, err)
		}
		refusedInPlace(t, root, "unfinished", StateInProgress)
		// Once the review ends the same close goes through, and no review can undo it.
		if _, err := windows.FinishReview(); err != nil {
			t.Fatal(err)
		}
		if code, err := CmdTasksFolder("", root, []string{"done", "unfinished", "--force"}); code != 0 || err != nil {
			t.Fatalf("done --force after the review = %d, %v", code, err)
		}
		if _, enrolled, err := readPendingReviewRecord(root, "unfinished"); err != nil || enrolled {
			t.Fatalf("forced close enrolled for review: %t, %v", enrolled, err)
		}
	})
	t.Run("final review of a killed loop", func(t *testing.T) {
		forceAtTerminal(t)
		repo := initRepo(t)
		root := filepath.Join(repo, TasksRoot)
		subject := taskWithCompletedChecklist(t, root, StateDone, "review-subject")
		if err := CompleteTrustedTask("", root, subject); err != nil {
			t.Fatal(err)
		}
		taskForLease(t, root, StateInProgress, "unfinished")
		plan := pendingReviewTestPlan(t, repo, root, forkspace.GitOut(repo, "rev-parse", "HEAD"))
		windows, err := BeginReviewCompletionWindowsWithPending([]string{root}, []string{subject.ID}, plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.Abandon(); err != nil { // what a killed process leaves
			t.Fatal(err)
		}
		if code, err := CmdTasksFolder("", root, []string{"done", "unfinished", "--force"}); code != 0 || err != nil {
			t.Fatalf("done --force after a killed loop = %d, %v", code, err)
		}
		if _, enrolled, err := readPendingReviewRecord(root, "unfinished"); err != nil || enrolled {
			t.Fatalf("forced close enrolled in a dead review: %t, %v", enrolled, err)
		}
	})
	// In ../responder a person's forced close of one task was refused because an interrupted command
	// had left a window behind and someone had since edited another, archived task. Recovery
	// reports that edit; it is not a reason to refuse this close.
	t.Run("leftover window and an edited archived task", func(t *testing.T) {
		forceAtTerminal(t)
		repo := initRepo(t)
		root := filepath.Join(repo, TasksRoot)
		archived := taskWithCompletedChecklist(t, root, StateDone, "archived")
		if err := CompleteTrustedTask("", root, archived); err != nil {
			t.Fatal(err)
		}
		taskForLease(t, root, StateInProgress, "unfinished")
		windows, err := BeginCompletionWindows([]string{root})
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.Abandon(); err != nil { // what a killed process leaves
			t.Fatal(err)
		}
		writeTaskFile(t, filepath.Join(root, StateDone, "archived", "log.md"), "edited after it was archived\n")
		var code int
		out := captureStderr(t, func() {
			code, err = CmdTasksFolder("", root, []string{"done", "unfinished", "--force"})
		})
		if code != 0 || err != nil {
			t.Fatalf("done --force past an edited archived task = %d, %v; want the close", code, err)
		}
		if want := "the archived task archived was edited since"; !strings.Contains(out, want) {
			t.Errorf("output does not report the edited task (%q):\n%s", want, out)
		}
		if done, ok := mustCurrentTask(t, root, "unfinished"); !ok || done.State != StateDone {
			t.Fatal("the forced close did not reach done")
		}
		if still, ok := mustCurrentTask(t, root, "archived"); !ok || still.State != StateDone {
			t.Fatal("the edited archived task left done")
		}
		if index, err := ReadCompletionWindowIndex(root); err != nil || len(index.Windows) != 0 {
			t.Fatalf("leftover windows after the close: %d, %v", len(index.Windows), err)
		}
	})
	// Only that report is passed over: an error of any other kind still stops the close.
	t.Run("recovery reports", func(t *testing.T) {
		report := &BaselineMutationError{IDs: []string{"b", "a"}}
		if got := OnlyBaselineMutations(errors.Join(nil, report, &BaselineMutationError{IDs: []string{"a"}})); !slices.Equal(got, []string{"a", "b"}) {
			t.Errorf("reports only = %v, want [a b]", got)
		}
		for name, err := range map[string]error{
			"nothing":        nil,
			"another error":  errors.New("lock"),
			"mixed":          errors.Join(report, errors.New("lock")),
			"wrapped report": errors.Join(errors.New("context"), errors.New("lock"), report),
		} {
			if got := OnlyBaselineMutations(err); got != nil {
				t.Errorf("%s: passed over as %v", name, got)
			}
		}
	})
	t.Run("fork owned", func(t *testing.T) {
		repo, root, _ := proposalAssignment(t, "forced")
		assigned, _ := mustCurrentTask(t, root, "assigned")
		if _, err := completeTrustedTask(repo, root, assigned, completionOptions{force: true}); err == nil {
			t.Fatal("a forced close of a fork's assigned task was accepted")
		}
		refusedInPlace(t, root, "assigned", assigned.State)
	})
	t.Run("audit authority", func(t *testing.T) {
		t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
		root := t.TempDir()
		item := taskForLease(t, root, StateInProgress, "audited")
		if err := WriteAuditReopenRecord(root, testAuditReopenRecord("audited", "generation-1")); err != nil {
			t.Fatal(err)
		}
		var audit *auditCompletionStateError
		if _, err := completeTrustedTask("", root, item, completionOptions{force: true}); !errors.As(err, &audit) {
			t.Fatalf("forced close with audit authority = %v, want the audit refusal", err)
		}
		refusedInPlace(t, root, "audited", StateInProgress)
	})
}

// The note waits for the receipt: a forced close that fails partway leaves neither, and the retry
// that succeeds leaves exactly one note.
func TestForcedCloseNoteWaitsForTheReceipt(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "flaky", "task.md"), "# Flaky\n\n## Subtasks\n- [ ] Run the gate\n")
	item, _ := mustCurrentTask(t, root, "flaky")
	old := taskTmpCleaner
	t.Cleanup(func() { taskTmpCleaner = old })
	taskTmpCleaner = func(string) error { return errors.New("injected cleanup failure") }
	if _, err := completeTrustedTask("", root, item, completionOptions{force: true}); err == nil {
		t.Fatal("the injected failure did not fail the close")
	}
	taskTmpCleaner = old
	current, _ := mustCurrentTask(t, root, "flaky")
	if strings.Contains(readFileString(filepath.Join(current.Dir, "log.md")), "--force") || taskCompletionRecorded(root, current) {
		t.Fatal("a failed close left a note or a receipt")
	}
	if forced, err := completeTrustedTask("", root, current, completionOptions{force: true}); err != nil || forced == nil {
		t.Fatalf("retry = %v, %v; want a forced close", forced, err)
	}
	done, _ := mustCurrentTask(t, root, "flaky")
	if n := strings.Count(readFileString(filepath.Join(done.Dir, "log.md")), "Closed with coop tasks done --force"); n != 1 || !taskCompletionRecorded(root, done) {
		t.Fatalf("the retry left %d notes and receipt %v; want one of each", n, taskCompletionRecorded(root, done))
	}
}

// The note names the override and what was open.
func TestForcedCompletionNote(t *testing.T) {
	open := &IncompleteChecklistError{ID: "t", Done: 1, Total: 3, Open: []string{"Run the gate", "Review it"}}
	if got, want := forcedCompletionNote(&forcedClose{checklist: open}),
		`Closed with coop tasks done --force while 2 of 3 checklist items were open: "Run the gate", "Review it".`; got != want {
		t.Errorf("note = %q\nwant %q", got, want)
	}
	if got, want := forcedCompletionNote(&forcedClose{checklist: &IncompleteChecklistError{ID: "t"}}), "Closed with coop tasks done --force without a checklist."; got != want {
		t.Errorf("note = %q\nwant %q", got, want)
	}
}
