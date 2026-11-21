package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// threeBoundCommits commits three times with id's trailer, as the 2026-11-20 responder agent did: a
// change and two follow-ups after it saw the deployed result. It returns their short ids.
func threeBoundCommits(t *testing.T, repo, id string) []string {
	t.Helper()
	var shas []string
	for _, subject := range []string{"Fix login retries", "Cover the retry cap", "Tune the retry delay"} {
		git(t, repo, "commit", "-q", "--allow-empty", "-m", subject+"\n\nCoop-Task: "+id)
		shas = append(shas, forkspace.GitOut(repo, "rev-parse", "--short", "HEAD"))
	}
	return shas
}

// Follow-up commits carrying the task's trailer close without a person's override when no review
// can lose by them: no loop runs, and no final review waits on the task or on a task before them.
// log.md names the commits once (a re-run of done adds nothing), and a loop that starts afterwards
// does not list the archived task. Blocked again and closed again, it takes the same rule.
func TestDoneAcceptsSeveralBindingsNoReviewCanLoseBy(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, _ := gitRepo(t)
	root := filepath.Join(repo, ".agent", "tasks")
	id := "fix-login-retries"
	taskWithCompletedChecklist(t, root, StateInProgress, id)
	shas := threeBoundCommits(t, repo, id)

	snapshot, err := QueueSnapshot([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if extra := ExtraBindings(repo, snapshot, nil); len(extra) != 1 || extra[0].TaskID != id || extra[0].Found != 3 {
		t.Fatalf("startup report while open = %+v; want %s with 3 commits", extra, id)
	}
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo}); code != 0 || err != nil {
		t.Fatalf("done = %d, %v; want the close", code, err)
	}
	done, ok := mustCurrentTask(t, root, id)
	if !ok || done.State != StateDone || !taskCompletionRecorded(root, done) {
		t.Fatal("close did not reach done with its receipt")
	}
	log := readFileString(filepath.Join(done.Dir, "log.md"))
	for _, want := range append([]string{"Closed with 3 commits carrying its Coop-Task trailer (", "a later review cannot reopen this task."}, shas...) {
		if !strings.Contains(log, want) {
			t.Errorf("log.md omitted %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "--force") {
		t.Errorf("log.md calls the close forced:\n%s", log)
	}
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo}); code != 0 || err != nil {
		t.Fatalf("second done = %d, %v", code, err)
	}
	if n := strings.Count(readFileString(filepath.Join(done.Dir, "log.md")), "Closed with 3 commits"); n != 1 {
		t.Fatalf("log.md has %d notes after a re-run of done, want 1", n)
	}
	// A loop starting now neither waits on the close nor lists the archived task.
	release, err := HoldLoopRun(repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if snapshot, err = QueueSnapshot([]string{root}); err != nil {
		t.Fatal(err)
	}
	if extra := ExtraBindings(repo, snapshot, nil); len(extra) != 0 {
		t.Fatalf("startup report after the close = %+v; want nothing", extra)
	}

	if err := moveTrustedTaskFromDone(root, done, StateBlocked); err != nil {
		t.Fatal(err)
	}
	blocked, _ := mustCurrentTask(t, root, id)
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo}); code != 0 || err != nil {
		t.Fatalf("done after a block = %d, %v (from %s)", code, err, blocked.State)
	}
	done, _ = mustCurrentTask(t, root, id)
	if n := strings.Count(readFileString(filepath.Join(done.Dir, "log.md")), "Closed with 3 commits"); n != 2 {
		t.Fatalf("log.md has %d notes after closing twice, want 2", n)
	}
}

// While a loop can take the close into its final review, the close is refused, and the refusal
// says why and how out: a running loop (between iterations too, or one from before the run lock in
// its final review) says to wait for it, and a final review a stopped loop left says to run the loop
// again. Nothing moves.
func TestDoneRefusesSeveralBindingsWhileALoopCanTakeTheClose(t *testing.T) {
	for name, tc := range map[string]struct {
		start      func(t *testing.T, repo, root string) func()
		cause, fix string
		force      bool // the refusal offers a person's --force
	}{
		"loop between iterations": {
			start: func(t *testing.T, repo, _ string) func() {
				release, err := HoldLoopRun(repo)
				if err != nil {
					t.Fatal(err)
				}
				return func() { _ = release() }
			},
			cause: "A loop is running in this checkout, and its final review cannot take a task that several commits are bound to.",
			fix:   "Wait for the loop to end, then retry: coop tasks done ",
		},
		"final review of a loop without the run lock": {
			start: func(t *testing.T, _, root string) func() {
				windows, err := BeginReviewCompletionWindows([]string{root}, nil)
				if err != nil {
					t.Fatal(err)
				}
				return func() { _ = windows.Abandon() }
			},
			cause: "A loop is running in this checkout",
			fix:   "Wait for the loop to end, then retry: coop tasks done ",
		},
		"review a stopped loop left": {
			start: func(t *testing.T, _, root string) func() {
				windows, err := BeginReviewCompletionWindows([]string{root}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := unlockLeaseFile(windows.windows[0].live); err != nil {
					t.Fatal(err)
				}
				windows.windows[0].live = nil
				return func() {}
			},
			cause: "A loop stopped during its final review, and the next loop would take this close into that review",
			fix:   "Run coop loop to finish that review, then retry: coop tasks done ",
			force: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
			repo, _ := gitRepo(t)
			root := filepath.Join(repo, ".agent", "tasks")
			id := "fix-login-retries"
			taskWithCompletedChecklist(t, root, StateInProgress, id)
			threeBoundCommits(t, repo, id)
			stop := tc.start(t, repo, root)
			defer stop()
			code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo})
			var usage *ui.UsageError
			if code == 0 || !errors.As(err, &usage) {
				t.Fatalf("done = %d, %v; want the drawn refusal", code, err)
			}
			got := usage.Render(ui.Palette{})
			for _, want := range []string{"has 3 commits carrying \"Coop-Task: " + id + "\"", tc.cause, tc.fix + id} {
				if !strings.Contains(got, want) {
					t.Errorf("refusal omitted %q:\n%s", want, got)
				}
			}
			if offers := strings.Contains(got, "--force"); offers != tc.force {
				t.Errorf("refusal offers --force = %t, want %t:\n%s", offers, tc.force, got)
			}
			if current, ok := mustCurrentTask(t, root, id); !ok || current.State != StateInProgress {
				t.Fatal("refused task moved")
			}
		})
	}
}

// Several agent sessions close tasks in one checkout at once. Each close opens a completion window,
// and with no loop holding the run lock another close's window is not a loop: the close waits for
// it to end, and if it does not, says to run the same command again, never to wait for a loop.
func TestDoneWaitsOutAnotherCloseInsteadOfNamingALoop(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	busyWait := windowBusyWait
	t.Cleanup(func() { windowBusyWait = busyWait })
	windowBusyWait = 300 * time.Millisecond
	repo, _ := gitRepo(t)
	root := filepath.Join(repo, ".agent", "tasks")
	for _, id := range []string{"held-up", "racing"} {
		taskWithCompletedChecklist(t, root, StateInProgress, id)
		threeBoundCommits(t, repo, id)
	}
	other, err := BeginCompletionWindows([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	code, err := tasksFolderMoveWith(root, []string{"held-up"}, StateDone, "done", claimOptions{project: repo})
	var usage *ui.UsageError
	if code == 0 || !errors.As(err, &usage) {
		t.Fatalf("done beside a close that does not end = %d, %v; want the drawn refusal", code, err)
	}
	got := usage.Render(ui.Palette{})
	for _, want := range []string{"Another completion is in progress in this queue.", "Run the same command again, then retry: coop tasks done held-up"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal omitted %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "loop") || strings.Contains(got, "--force") {
		t.Errorf("refusal names a loop or --force:\n%s", got)
	}
	ended := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		ended <- other.Abandon()
	}()
	if code, err := tasksFolderMoveWith(root, []string{"racing"}, StateDone, "done", claimOptions{project: repo}); code != 0 || err != nil {
		t.Fatalf("done while another close finishes = %d, %v; want the close", code, err)
	}
	if err := <-ended; err != nil {
		t.Fatal(err)
	}
}

// A final review waiting on a task before the follow-ups replays them when it reopens that task, so
// the close is refused and names it, and a shallow file a box plants to cut that ancestry does not
// change the answer. A person's --force still closes it, and log.md says so; while a loop runs, even
// --force waits.
func TestDoneRefusesSeveralBindingsAfterAPendingReviewSubject(t *testing.T) {
	repo, root, _, assignment, subject, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, subject, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	id := "follow-up"
	taskWithCompletedChecklist(t, root, StateInProgress, id)
	shas := threeBoundCommits(t, repo, id)
	// Git's ancestry commands honor .git/shallow: listing the first follow-up there hides its parents.
	first := forkspace.GitOut(repo, "rev-parse", shas[0])
	writeTaskFile(t, filepath.Join(repo, ".git", "shallow"), first+"\n")
	subjectCommit := forkspace.GitOut(repo, "rev-parse", "HEAD~3")
	if _, err := forkspace.GitOutErr(repo, "merge-base", "--is-ancestor", subjectCommit, "HEAD"); err == nil {
		t.Fatal("fixture: the planted shallow file did not cut Git's ancestry")
	}

	code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo})
	var usage *ui.UsageError
	if code == 0 || !errors.As(err, &usage) {
		t.Fatalf("done = %d, %v; want the drawn refusal", code, err)
	}
	got := usage.Render(ui.Palette{})
	for _, want := range []string{
		"A loop's final review is still waiting on task " + subject.ID + ", and reopening that task replays these later commits.",
		"Run coop loop, which settles that review before other work, then retry: coop tasks done " + id,
		"Or a person who checked the work closes it as it stands: coop tasks done " + id + " --force",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal omitted %q:\n%s", want, got)
		}
	}
	snapshot, err := QueueSnapshot([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if extra := ExtraBindings(repo, snapshot, []string{subject.ID}); len(extra) != 1 || extra[0].TaskID != id {
		t.Errorf("startup report past the shallow file = %+v; want %s", extra, id)
	}
	if err := os.Remove(filepath.Join(repo, ".git", "shallow")); err != nil {
		t.Fatal(err)
	}
	item, _ := mustCurrentTask(t, root, id)
	if item.State != StateInProgress {
		t.Fatal("refused task moved")
	}

	release, err := HoldLoopRun(repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := completeTrustedTask(repo, root, item, completionOptions{force: true}); !errors.Is(err, errLoopRunning) {
		t.Fatalf("forced close while a loop runs = %v; want the wait", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo, forceClose: true}); code != 0 || err != nil {
		t.Fatalf("done --force = %d, %v; want the close", code, err)
	}
	done, _ := mustCurrentTask(t, root, id)
	log := readFileString(filepath.Join(done.Dir, "log.md"))
	for _, want := range append([]string{"Closed with coop tasks done --force. 3 commits carry its Coop-Task trailer ("}, shas...) {
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

// A final review a stopped loop left weeks ago, with a merge since its subject's commit (2026-11-21:
// merge 0c6400a3 sat between the review's subject and hundreds of later commits), cannot replay the
// commits after that merge: its reopen pins the subject's commit alone and sets them aside. So a host
// close of a task bound by several of them goes through, and the reopen still works after it. A box's
// close keeps the stricter rule, since the box chose that history.
func TestDoneAcceptsSeveralBindingsPastAMergeAfterAPendingReviewSubject(t *testing.T) {
	repo, root, base, assignment, subject, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, subject, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	me := selfActor(t, "claude")
	for _, id := range []string{"follow-up", "box-follow-up"} {
		taskWithCompletedChecklist(t, root, StateTodo, id)
	}
	if _, err := ClaimSessionTask(root, mustTask(t, root, "box-follow-up"), me); err != nil {
		t.Fatal(err)
	}
	side := forkspace.GitOut(repo, "commit-tree", "-p", base, "-m", "side work", base+"^{tree}")
	git(t, repo, "merge", "-q", "--no-ff", "--no-edit", side)
	shas := threeBoundCommits(t, repo, "follow-up")
	threeBoundCommits(t, repo, "box-follow-up")

	err := CompleteBoxTask(repo, root, mustTask(t, root, "box-follow-up"), me)
	if want := "reopening that task replays these later commits"; !strings.Contains(fmt.Sprint(err), want) {
		t.Fatalf("box close past the merge = %v; want %q", err, want)
	}
	if code, err := tasksFolderMoveWith(root, []string{"follow-up"}, StateDone, "done", claimOptions{project: repo}); code != 0 || err != nil {
		t.Fatalf("done past the merge = %d, %v; want the close", code, err)
	}
	done, _ := mustCurrentTask(t, root, "follow-up")
	log := readFileString(filepath.Join(done.Dir, "log.md"))
	for _, want := range append([]string{"Closed with 3 commits carrying its Coop-Task trailer ("}, shas...) {
		if !strings.Contains(log, want) {
			t.Errorf("log.md omitted %q:\n%s", want, log)
		}
	}
	snapshot, err := QueueSnapshot([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ExtraBindings(repo, snapshot, []string{subject.ID}) {
		if e.TaskID == "follow-up" {
			t.Errorf("startup report lists the closed task past the merge: %+v", e)
		}
	}
	// The review still reopens its subject, at HEAD, with every later commit set aside.
	audit, _, err := PrepareReviewReopen(repo, root, subject.ID, "fix it")
	if err != nil {
		t.Fatalf("review reopen after the close = %v", err)
	}
	if head := forkspace.GitOut(repo, "rev-parse", "HEAD"); audit.BaselineHead != head || audit.ReviewedHead == "" || len(audit.History) != 0 {
		t.Fatalf("review reopen = baseline %s, reviewed %s, %d replayed; want rework from HEAD %s replaying nothing",
			audit.BaselineHead, audit.ReviewedHead, len(audit.History), head)
	}
}

// A task a final review reopened is itself a pending subject: closing its rework with a second
// commit carrying its trailer is refused, since the review can reopen it only while one does.
func TestDoneRefusesSeveralBindingsOnAPendingReviewSubject(t *testing.T) {
	repo, root, _, assignment, subject, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, subject, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	current, _ := mustCurrentTask(t, root, subject.ID)
	reopenForRework(t, repo, root, current)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "Rework\n\nCoop-Task: "+subject.ID)
	reworked, _ := mustCurrentTask(t, root, subject.ID)
	var extra *ExtraBindingsError
	if _, err := completeTrustedTask(repo, root, reworked, completionOptions{}); !errors.As(err, &extra) ||
		!strings.Contains(extra.Why, "a loop's final review is still waiting on it") {
		t.Fatalf("close of the reopened subject = %v; want it refused", err)
	}
	if after, _ := mustCurrentTask(t, root, subject.ID); after.State != reworked.State {
		t.Fatalf("refused task moved to %s", after.State)
	}
}

// A pending-review record of the task's own queue that Coop cannot read could be the review that
// loses its verdict, so the close is refused.
func TestDoneRefusesSeveralBindingsBesideAnUnreadableReviewRecord(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, _ := gitRepo(t)
	root := filepath.Join(repo, ".agent", "tasks")
	id := "fix-login-retries"
	item := taskWithCompletedChecklist(t, root, StateInProgress, id)
	threeBoundCommits(t, repo, id)
	name, err := pendingReviewRecordName(root, "broken")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicWriteTaskFile(registry, name, []byte("{not-json\n")); err != nil {
		registry.Close()
		t.Fatal(err)
	}
	registry.Close()
	var extra *ExtraBindingsError
	if _, err := completeTrustedTask(repo, root, item, completionOptions{}); !errors.As(err, &extra) ||
		!strings.Contains(extra.Why, "Coop could not read the pending final reviews") {
		t.Fatalf("close beside an unreadable record = %v; want it refused", err)
	}
}

// A person's --force on an unfinished checklist, for a task several commits carry the trailer of
// with no review at risk, still names those commits in log.md.
func TestForcedChecklistCloseNamesTheCommitsItAccepted(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, _ := gitRepo(t)
	root := filepath.Join(repo, ".agent", "tasks")
	id := "fix-login-retries"
	item := taskForLease(t, root, StateInProgress, id)
	if err := RewriteSubtasks(item.Dir, []Subtask{{Text: "the retry cap", Done: false}}); err != nil {
		t.Fatal(err)
	}
	shas := threeBoundCommits(t, repo, id)
	item, _ = mustCurrentTask(t, root, id)
	if _, err := completeTrustedTask(repo, root, item, completionOptions{force: true}); err != nil {
		t.Fatal(err)
	}
	done, _ := mustCurrentTask(t, root, id)
	log := readFileString(filepath.Join(done.Dir, "log.md"))
	for _, want := range append([]string{"checklist items", "3 commits carry its Coop-Task trailer ("}, shas...) {
		if !strings.Contains(log, want) {
			t.Errorf("log.md omitted %q:\n%s", want, log)
		}
	}
}

// A box's tasks_complete takes the same rule, and closes a task several commits carry the trailer
// of only for the session that claimed it: never a task nobody holds, never from a loop box (which
// holds no claim and whose loop is running).
func TestBoxClosesATaskSeveralCommitsCarryOnlyWhenItHoldsIt(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, _ := gitRepo(t)
	root := filepath.Join(repo, ".agent", "tasks")
	me := selfActor(t, "claude")
	for _, id := range []string{"unclaimed", "mine"} {
		taskWithCompletedChecklist(t, root, StateTodo, id)
		threeBoundCommits(t, repo, id)
	}
	if err := CompleteBoxTask(repo, root, mustTask(t, root, "unclaimed"), me); !strings.Contains(fmt.Sprint(err), "this session has not claimed it") {
		t.Fatalf("box close of an unclaimed task = %v; want it refused", err)
	}
	if _, err := ClaimSessionTask(root, mustTask(t, root, "mine"), me); err != nil {
		t.Fatal(err)
	}
	release, err := HoldLoopRun(repo)
	if err != nil {
		t.Fatal(err)
	}
	var extra *ExtraBindingsError
	if err := CompleteBoxTask(repo, root, mustTask(t, root, "unclaimed"), ClaimActor{}); !errors.As(err, &extra) || !extra.Running {
		t.Fatalf("loop box close = %v; want the running loop named", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := CompleteBoxTask(repo, root, mustTask(t, root, "mine"), me); err != nil {
		t.Fatalf("box close of its own task = %v", err)
	}
	done := mustTask(t, root, "mine")
	if done.State != StateDone || !strings.Contains(readFileString(filepath.Join(done.Dir, "log.md")), "Closed with 3 commits carrying its Coop-Task trailer") {
		t.Fatalf("own task is %s, log:\n%s", done.State, readFileString(filepath.Join(done.Dir, "log.md")))
	}
	if current := mustTask(t, root, "unclaimed"); current.State != StateTodo {
		t.Fatalf("refused task moved to %s", current.State)
	}
}

// A second loop over the same checkout, reached through another path, does not hang on the run
// lock: it waits briefly, then says another loop or a close holds it.
func TestHoldLoopRunRefusesInsteadOfHanging(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	wait := loopRunWait
	t.Cleanup(func() { loopRunWait = wait })
	loopRunWait = 100 * time.Millisecond
	repo, _ := gitRepo(t)
	link := filepath.Join(t.TempDir(), "same-checkout")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	release, err := HoldLoopRun(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	if _, err := HoldLoopRun(link); err == nil || !strings.Contains(err.Error(), "another coop loop is running over") {
		t.Fatalf("second run mark = %v; want the plain refusal", err)
	}
	// Letter case on a case-insensitive volume names the same folder, so the same lock.
	upper := filepath.Join(strings.ToUpper(filepath.Dir(repo)), filepath.Base(repo))
	if info, err := os.Stat(upper); err != nil || upper == repo {
		t.Logf("no case-insensitive spelling of %s here (%v)", repo, err)
	} else if same, _ := os.Stat(repo); os.SameFile(info, same) {
		if _, err := HoldLoopRun(upper); err == nil {
			t.Fatal("another spelling of the checkout took a second run mark")
		}
	}
}

// The run lock is the checkout holding the queue being closed: a fork's queue closed from its
// parent's project still meets the fork loop.
func TestDoneMeetsTheLoopOfTheCheckoutHoldingTheQueue(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	parent, _ := gitRepo(t)
	fork, _ := gitRepo(t)
	root := filepath.Join(fork, ".agent", "tasks")
	id := "fix-login-retries"
	item := taskWithCompletedChecklist(t, root, StateInProgress, id)
	threeBoundCommits(t, parent, id)
	release, err := HoldLoopRun(fork)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	var extra *ExtraBindingsError
	if _, err := completeTrustedTask(parent, root, item, completionOptions{}); !errors.As(err, &extra) || !extra.Running {
		t.Fatalf("close of the fork's queue while its loop runs = %v; want the running loop named", err)
	}
}

// A history Coop cannot walk (here a deleted parent object; a huge commit or a long history does the
// same) skips the count. A box can cause that, so its close is refused; a person's close goes
// through, never stuck, and log.md says the commits were not counted.
func TestCloseOnAHistoryCoopCannotWalk(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	repo, _ := gitRepo(t)
	root := filepath.Join(repo, ".agent", "tasks")
	me := selfActor(t, "claude")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "Lost parent")
	lost := forkspace.GitOut(repo, "rev-parse", "HEAD")
	for _, id := range []string{"box-task", "host-task"} {
		taskWithCompletedChecklist(t, root, StateTodo, id)
		git(t, repo, "commit", "-q", "--allow-empty", "-m", "Work\n\nCoop-Task: "+id)
	}
	if err := os.Remove(filepath.Join(repo, ".git", "objects", lost[:2], lost[2:])); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimSessionTask(root, mustTask(t, root, "box-task"), me); err != nil {
		t.Fatal(err)
	}
	if err := CompleteBoxTask(repo, root, mustTask(t, root, "box-task"), me); err == nil ||
		!strings.Contains(err.Error(), "so a session cannot close it; block it for a person instead") {
		t.Fatalf("box close on an unwalkable history = %v; want it refused", err)
	}
	if current := mustTask(t, root, "box-task"); current.State != StateInProgress {
		t.Fatalf("refused task moved to %s", current.State)
	}
	if code, err := tasksFolderMoveWith(root, []string{"host-task"}, StateDone, "done", claimOptions{project: repo}); code != 0 || err != nil {
		t.Fatalf("host close on an unwalkable history = %d, %v; want the close", code, err)
	}
	done := mustTask(t, root, "host-task")
	if log := readFileString(filepath.Join(done.Dir, "log.md")); !strings.Contains(log, "Closed without counting the commits carrying its Coop-Task trailer: Coop could not read the history (") {
		t.Fatalf("log.md does not say the commits were not counted:\n%s", log)
	}
}

// In emisar a person unblocked a task whose partial commit from before the decision was already
// bound, and the work done after it carried the same trailer: nothing said the task was bound until
// a final review failed on the second binding. Unblock and claim now say it: closing it yourself
// takes more commits with its trailer, while a loop binds one commit per task.
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
			"Its commit " + bound[:12] + " already carries Coop-Task: " + id + ".",
			"If you close it yourself, further commits may carry that trailer too. If a loop will finish it, name the task in the message body instead: a loop binds one commit per task.",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("coop tasks %s output lacks %q:\n%s", args[0], want, out)
			}
		}
	}
	if out := run("claim", "not-bound-yet"); strings.Contains(out, "already carries") {
		t.Errorf("a task with no commit was called bound:\n%s", out)
	}
	// A second binding is named, and that only a close outside a loop finishes the task.
	git("commit", "-q", "--allow-empty", "-m", "Finish the importer\n\nCoop-Task: "+id)
	out := run("claim", id)
	for _, want := range []string{"has 2 commits carrying \"Coop-Task: " + id + "\"", "A loop cannot finish it, since a loop binds one commit per task. Close it yourself with coop tasks done once no loop is running."} {
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

// The 2026-11-21 manual pass closed a blocked task whose decision had no answer with a plain
// `coop tasks done` from an agent's shell, past the question claim and tasks_complete keep for a
// person. A plain close now refuses an open or pending decision and names the way on; an answered
// decision, or none at all, closes as before.
func TestDoneRefusesABlockedTaskWhoseDecisionWaits(t *testing.T) {
	const id = "2026-11-21-pick-storage"
	for name, tc := range map[string]struct {
		decision string
		refused  bool
	}{
		"open decision":    {renderDecisionScaffold(id, "Pick storage"), true},
		"pending note":     {"# Decision: Which store?\n\n**Resolution:** pending owner direction\n", true},
		"answered":         {"# Decision: Which store?\n\n**Resolution:** Postgres.\n", false},
		"without decision": {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
			root := t.TempDir()
			taskWithCompletedChecklist(t, root, StateBlocked, id)
			if tc.decision != "" {
				writeTaskFile(t, filepath.Join(root, StateBlocked, id, "decision.md"), tc.decision)
			}
			code, err := CmdTasksFolder("", root, []string{"done", "pick-storage"})
			current, _ := mustCurrentTask(t, root, id)
			if !tc.refused {
				if code != 0 || err != nil || current.State != StateDone {
					t.Fatalf("done = (%d, %v), state %s; want it closed", code, err, current.State)
				}
				return
			}
			if code != 1 || err == nil {
				t.Fatalf("done = (%d, %v), want a refusal", code, err)
			}
			for _, want := range []string{"is blocked", "coop tasks decisions", `coop tasks unblock ` + id + ` "<answer>"`, "coop tasks done " + id + " --force"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal lacks %q:\n%s", want, err)
				}
			}
			if current.State != StateBlocked {
				t.Fatalf("the refused close moved the task to %s", current.State)
			}
			// The person the refusal names closes it as it stands; the decision stays unanswered.
			forceAtTerminal(t)
			if code, err := CmdTasksFolder("", root, []string{"done", "pick-storage", "--force"}); code != 0 || err != nil {
				t.Fatalf("done --force = (%d, %v), want it closed", code, err)
			}
			if current, _ = mustCurrentTask(t, root, id); current.State != StateDone || readFileString(filepath.Join(current.Dir, "decision.md")) != tc.decision {
				t.Fatalf("done --force left the task %s with decision.md %q", current.State, readFileString(filepath.Join(current.Dir, "decision.md")))
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
	if !strings.Contains(err.Error(), "runs the same command in a terminal") {
		t.Errorf("the terminal refusal does not tell a person what to do:\n%s", err)
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
		for _, want := range []string{"the archived task archived was edited since", "the task stays in done."} {
			if !strings.Contains(out, want) {
				t.Errorf("output does not report the edited task (%q):\n%s", want, out)
			}
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

// A reopen replays the straight line from HEAD back to its subject's commit; only a merge on that
// line, not one below the subject or beside the line, sets the later commits aside.
func TestReplaysAfterLooksOnlyBetweenTheSubjectAndHead(t *testing.T) {
	// root <- below(merge of root, side) <- subject <- later <- merge(later, other) <- head
	h := rawTaskHistory{head: "head", parents: map[string][]string{
		"root": nil, "side": {"root"}, "below": {"root", "side"}, "subject": {"below"},
		"later": {"subject"}, "other": {"subject"}, "merge": {"later", "other"}, "head": {"merge"},
	}}
	for subject, want := range map[string]bool{"head": true, "merge": true, "later": false, "subject": false} {
		if got := h.replaysAfter(subject); got != want {
			t.Errorf("replaysAfter(%s) = %t, want %t", subject, got, want)
		}
	}
	h.head = "later" // a straight line to the subject, with a merge below it
	if !h.replaysAfter("subject") {
		t.Error("a merge below the subject set the later commits aside")
	}
	h.head = "side" // a straight line to the root that never meets the subject
	if !h.replaysAfter("missing") {
		t.Error("a subject off a straight line set the later commits aside")
	}
	h.head = "unread"
	if !h.replaysAfter("subject") {
		t.Error("an unread commit did not count as a straight line")
	}
}
