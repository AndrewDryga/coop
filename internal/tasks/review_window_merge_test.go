package tasks

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// mergeSideWork merges an empty side commit built on base into HEAD, as a person merging upstream
// work does, and returns the merge commit.
func mergeSideWork(t *testing.T, repo, base string) string {
	t.Helper()
	side := forkspace.GitOut(repo, "commit-tree", "-p", base, "-m", "side work", base+"^{tree}")
	git(t, repo, "merge", "-q", "--no-ff", "--no-edit", side)
	return forkspace.GitOut(repo, "rev-parse", "HEAD")
}

// requireHandedOver checks a subject whose review met merge went to a person: blocked, its decision
// and log say why and how to close it, its record is gone, a loop start loads cleanly, and the advice
// works: a person's coop tasks done closes it.
func requireHandedOver(t *testing.T, repo, root, id, merge string) {
	t.Helper()
	blocked, ok := mustCurrentTask(t, root, id)
	if !ok || blocked.State != StateBlocked {
		t.Fatalf("handed-over task is %s, want blocked", blocked.State)
	}
	decision := readFileString(filepath.Join(blocked.Dir, "decision.md"))
	for _, want := range []string{"Merge commit " + shortCommit(merge), "coop tasks done " + id + " --force", "coop tasks unblock " + id} {
		if !strings.Contains(decision, want) {
			t.Errorf("decision.md omitted %q:\n%s", want, decision)
		}
	}
	if log := readFileString(filepath.Join(blocked.Dir, "log.md")); !strings.Contains(log, "final review could not run: merge commit "+shortCommit(merge)) {
		t.Errorf("log.md does not say why the review was dropped:\n%s", log)
	}
	if _, ok, err := readPendingReviewRecord(root, id); err != nil || ok {
		t.Fatalf("record after hand-over = ok %v, err %v; want it retired", ok, err)
	}
	if cohort, err := RecoverPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects)+len(cohort.Parked) != 0 {
		t.Fatalf("loop start after hand-over = %+v, %v; want nothing pending", cohort, err)
	}
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo}); code == 0 || err == nil {
		t.Fatal("plain done of the handed-over task went through; only a person's --force may close it")
	}
	if code, err := tasksFolderMoveWith(root, []string{id}, StateDone, "done", claimOptions{project: repo, forceClose: true}); code != 0 || err != nil {
		t.Fatalf("person's done --force after hand-over = %d, %v", code, err)
	}
	if done, _ := mustCurrentTask(t, root, id); done.State != StateDone {
		t.Fatalf("task is %s after done --force", done.State)
	}
}

// A person merged upstream work after the loop stopped (2026-11-21: merge 0c6400a3, two days after a
// review was left waiting). The review cannot read its window, so the round that would stop every
// loop hands the task to a person and drops the review with a note instead.
func TestFinalReviewPastAPersonsMergeGoesToAPerson(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	merge := mergeSideWork(t, repo, base)
	var mergeErr *auditMergeCommitError
	if _, err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 1); !errors.As(err, &mergeErr) {
		t.Fatalf("fixture: review round past the merge = %v; want the merge refused", err)
	}
	handed, err := HandOverUnreadableReviews(repo, []string{root}, []string{done.ID})
	if err != nil || len(handed) != 1 || handed[0] != done.ID {
		t.Fatalf("hand-over = %v, %v; want %s", handed, err, done.ID)
	}
	requireHandedOver(t, repo, root, done.ID, merge)
}

// A box that merges inside its own attempt, beside a change no trailer names, gets no narrower
// review that would skip that change: its task is handed to a person, never left done.
func TestFinalReviewPastAMergeInsideTheAttemptGoesToAPerson(t *testing.T) {
	var merge string
	repo, root, _, assignment, done, plan := pendingReviewTestCompletionWith(t, func(repo, base string) {
		writeTaskFile(t, filepath.Join(repo, "hidden.txt"), "unreviewed\n")
		git(t, repo, "add", "hidden.txt")
		git(t, repo, "commit", "-qm", "unbound change")
		merge = mergeSideWork(t, repo, base)
	})
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	handed, err := HandOverUnreadableReviews(repo, []string{root}, []string{done.ID})
	if err != nil || len(handed) != 1 {
		t.Fatalf("hand-over = %v, %v; want the task", handed, err)
	}
	requireHandedOver(t, repo, root, done.ID, merge)
}

// A straight window keeps its review: nothing is handed over.
func TestFinalReviewOnAStraightLineIsNotHandedOver(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if handed, err := HandOverUnreadableReviews(repo, []string{root}, []string{done.ID}); err != nil || len(handed) != 0 {
		t.Fatalf("hand-over on a straight line = %v, %v; want none", handed, err)
	}
	if item, _ := mustCurrentTask(t, root, done.ID); item.State != StateDone {
		t.Fatalf("task is %s, want done", item.State)
	}
}

// A subject waiting for verification whose window later gained a merge goes back to signoff when a
// loop starts, where the round hands it to a person, instead of failing every loop start.
func TestVerifyPendingReviewPastAMergeGoesBackToSignoff(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginPendingReviewRound([]string{root}, []string{done.ID}, 1); err != nil {
		t.Fatal(err)
	}
	cohort, err := LoadPendingReviews(repo, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkExpectedPendingReviewsVerify([]string{root}, cohort.Subjects); err != nil {
		t.Fatal(err)
	}
	mergeSideWork(t, repo, base)
	cohort, err = RecoverPendingReviews(repo, []string{root})
	if err != nil || len(cohort.Subjects) != 1 || cohort.Subjects[0].Phase != PendingReviewSignoff {
		t.Fatalf("loop start past the merge = %+v, %v; want the subject back in signoff", cohort, err)
	}
}

// The decision's other way out works too: unblocking sends the task back to be worked, under review
// authority that leaves the commits after it alone.
func TestHandedOverReviewTaskUnblocksToWork(t *testing.T) {
	repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	mergeSideWork(t, repo, base)
	if _, err := HandOverUnreadableReviews(repo, []string{root}, []string{done.ID}); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderUnblock(repo, root, []string{done.ID, "check the change again"}); code != 0 || err != nil {
		t.Fatalf("unblock after hand-over = %d, %v", code, err)
	}
	item, _ := mustCurrentTask(t, root, done.ID)
	reopen, ok, err := ReadAuditReopenRecord(root, done.ID)
	if item.State != StateTodo || err != nil || !ok || !auditReopenRecordActive(reopen) || reopen.DisputedReceipt != "" {
		t.Fatalf("after unblock: task %s, authority %+v (ok %v, %v); want todo under active authority", item.State, reopen, ok, err)
	}
}

// A box can make its task's own commit the merge. Review authority cannot be captured for such a
// commit, so the task goes to a person without it, rather than stopping every loop again.
func TestFinalReviewOfAMergeTaskCommitGoesToAPerson(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletionWith(t, func(repo, base string) {
		side := forkspace.GitOut(repo, "commit-tree", "-p", base, "-m", "side work", base+"^{tree}")
		git(t, repo, "merge", "-q", "--no-ff", "--no-commit", side) // the task's commit completes it
	})
	merge := forkspace.GitOut(repo, "rev-parse", "HEAD")
	if parents := strings.Fields(forkspace.GitOut(repo, "show", "-s", "--format=%P", merge)); len(parents) != 2 {
		t.Fatalf("fixture: the task's commit has parents %v, want a merge", parents)
	}
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareReviewReopen(repo, root, done.ID, "x"); err == nil {
		t.Fatal("fixture: review authority was captured for a merge commit")
	}
	handed, err := HandOverUnreadableReviews(repo, []string{root}, []string{done.ID})
	if err != nil || len(handed) != 1 {
		t.Fatalf("hand-over = %v, %v; want the task", handed, err)
	}
	if _, ok, err := ReadAuditReopenRecord(root, done.ID); err != nil || ok {
		t.Fatalf("audit authority = %v, %v; want none", ok, err)
	}
	requireHandedOver(t, repo, root, done.ID, merge)
}
