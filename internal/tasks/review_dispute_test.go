package tasks

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestReviewDisputeRollbackAndHumanUnblock(t *testing.T) {
	repo, root, _, assignment, done, plan := pendingReviewTestCompletion(t)
	if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	prior, _, err := readPendingReviewRecord(root, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior.Finding, prior.ReclosedWithoutCommit = "missing desktop evidence", true
	if err := writePendingReviewRecord(root, prior); err != nil {
		t.Fatal(err)
	}
	audit, pending, err := PrepareReviewReopen(repo, root, done.ID, prior.Finding)
	if err != nil || audit.DisputedReceipt == "" {
		t.Fatalf("dispute: %+v, %v", audit, err)
	}
	injected := errors.New("decision write failed")
	later := taskWithCompletedChecklist(t, root, StateDone, "z-later")
	if err := CompleteTrustedTask("", root, later); err != nil {
		t.Fatal(err)
	}
	later, _ = mustCurrentTask(t, root, later.ID)
	laterReceipt, ok := taskCompletionReceipt(root, later)
	if !ok {
		t.Fatal("later subject has no receipt")
	}
	move := TrustedTaskMove{Root: root, Task: done, NewState: StateBlocked, Reopen: &audit, PendingReview: pending,
		MetadataNames: []string{"state.md", "log.md", "decision.md"},
		AfterMove: func(dir string) error {
			if err := WriteDecision(dir, done.ID, done.Title, Decision{Question: "Require evidence?", Why: "Repeated finding", Options: []string{"Run checks"}, Recommendation: "Run checks"}); err != nil {
				return err
			}
			return injected
		},
	}
	if err := MoveTrustedTasksFromDoneWith([]TrustedTaskMove{move, {Root: root, Task: later, NewState: StateInProgress, AfterMove: func(string) error { t.Fatal("later callback ran after failure"); return nil }}}); !errors.Is(err, injected) {
		t.Fatalf("rollback error: %v", err)
	}
	if receipt, ok := taskCompletionReceipt(root, later); !ok || receipt != laterReceipt {
		t.Fatal("rollback lost the untouched later subject's receipt")
	}
	current, _ := mustCurrentTask(t, root, done.ID)
	if current.State != StateDone || pathExists(filepath.Join(current.Dir, "decision.md")) {
		t.Fatal("failed block left task or decision changed")
	}
	if _, ok, err := ReadAuditReopenRecord(root, done.ID); err != nil || ok {
		t.Fatalf("failed block left authority: %v, %v", ok, err)
	}
	if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 1 {
		t.Fatalf("rollback lost receipt/debt: %+v, %v", cohort, err)
	}
	move.AfterMove = nil
	// Rollback restored the original bytes under a new filesystem fingerprint.
	_, move.PendingReview, err = PrepareReviewReopen(repo, root, done.ID, prior.Finding)
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveTrustedTasksFromDoneWith([]TrustedTaskMove{move}); err != nil {
		t.Fatal(err)
	}
	// Simulate a stopped controller immediately after the host move: recovery retires only the
	// exact disputed receipt, and a later human unblock starts with no inherited finding.
	if cohort, err := LoadPendingReviews(repo, []string{root}); err != nil || len(cohort.Subjects) != 0 {
		t.Fatalf("recover dispute: %+v, %v", cohort, err)
	}
	current, _ = mustCurrentTask(t, root, done.ID)
	if err := resolveAndUnblock(repo, root, current, "run the missing checks"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadAuditReopenRecord(root, done.ID)
	if err != nil || !ok || got.Finding != "" || got.DisputedReceipt != "" || got.Repaired {
		t.Fatalf("unblock retained dispute: %+v, %v", got, err)
	}
	current, _ = mustCurrentTask(t, root, done.ID)
	if current.State != StateTodo {
		t.Fatalf("unblocked state: %s", current.State)
	}
}

func TestReviewDisputeKeepsRepairEvidenceAcrossSigning(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound repair", true: "tagged sibling"}[tagged], func(t *testing.T) {
			repo, root, base, assignment, done, plan := pendingReviewTestCompletion(t)
			if err := assignment.Lease.MarkCompletedForReview(repo, done, plan, false); err != nil {
				t.Fatal(err)
			}
			if err := assignment.Lease.Release(); err != nil {
				t.Fatal(err)
			}
			reopen := reopenForRework(t, repo, root, done)
			reopen.Finding = "missing evidence"
			if err := WriteAuditReopenRecord(root, reopen); err != nil {
				t.Fatal(err)
			}
			writeTaskFile(t, filepath.Join(repo, "later.txt"), "later change\n")
			git(t, repo, "add", "later.txt")
			message := "repair"
			if tagged {
				message += "\n\nCoop-Task: sibling"
			}
			git(t, repo, "commit", "-qm", message)
			oldHead := forkspace.GitOut(repo, "rev-parse", "HEAD")
			resignSince(t, repo, base, "2034-05-06T07:08:09Z")
			if err := CarryReopenedReviewsAfterSigning(repo, oldHead); err != nil {
				t.Fatal(err)
			}
			got, ok, err := ReadAuditReopenRecord(root, done.ID)
			if err != nil || !ok || got.Finding != reopen.Finding || got.Repaired == tagged {
				t.Fatalf("carried provenance = %+v, %v", got, err)
			}
		})
	}
}
