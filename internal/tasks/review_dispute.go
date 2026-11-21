package tasks

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
)

func validateReclosedFinding(finding string, unchanged bool) error {
	if finding != "" && !ValidTaskText(finding, false, TaskLineLimit) || unchanged && finding == "" {
		return errors.New("invalid pending-review finding")
	}
	return nil
}

func retireDisputedReview(root string, expected PendingReviewRecord, audit AuditReopenRecord) (err error) {
	id := expected.Task.Ref.ID
	authority, err := lockLeaseAuthority(root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlockLeaseFile(authority)) }()
	current, ok, err := CurrentTask(root, id)
	if err != nil || !ok || current.State != StateBlocked {
		return errors.Join(err, fmt.Errorf("task %s changed before retiring disputed review", id))
	}
	instance, err := ReadTaskInstance(root, current)
	if err != nil || !sameTaskInstance(instance, expected.Task) {
		return errors.Join(err, fmt.Errorf("task %s changed identity before retiring disputed review", id))
	}
	prior, ok, err := readPendingReviewRecord(root, id)
	if err != nil || !ok || !pendingReviewRecordMatchesExpected(prior, expected) {
		return errors.Join(err, fmt.Errorf("task %s pending dispute changed", id))
	}
	reopen, ok, err := ReadAuditReopenRecord(root, id)
	if err != nil || !ok || !AuditReopenRecordsEqual(reopen, audit) || reopen.DisputedReceipt != expected.Fingerprint.Receipt {
		return errors.Join(err, fmt.Errorf("task %s dispute authority changed", id))
	}
	return removePendingReviewRecord(root, id)
}

func normalizedReviewFinding(finding string) string {
	return strings.TrimRight(strings.ToLower(strings.Join(strings.Fields(finding), " ")), ". ")
}

// HandOverUnreadableReviews hands each of ids, the subjects of a final-review round, whose review
// window holds a merge commit to a person, and returns them. Coop reviews only a straight line of
// commits, so that review can never run, and every later loop would stop at it (2026-11-21: a review
// left on 2026-11-05 met merge 0c6400a3 among 867 later commits). Coop keeps no record of where a
// run's own attempts ended, so it cannot tell a person's merge made after the run from one a box made
// inside its attempt to keep work from review, and a narrower window, such as the task's commit
// alone, could leave that box's other commits unreviewed. So no such subject keeps its done state: it
// is reopened into 50_blocked/ the way a disputed review is, its decision.md says why, and its record
// is retired with a note in log.md. A box cannot complete, claim or unblock a blocked task, and an
// agent's shell is refused --force: a person closes it with coop tasks done --force after checking
// it, or unblocks it to have it worked again. A box that adds a merge on purpose only hands every
// subject of that window to a person. Any other window error is returned, as
// BeginPendingReviewRound would.
func HandOverUnreadableReviews(repo string, hosts, ids []string) (handed []string, err error) {
	head, err := pendingReviewHead(repo)
	if err != nil {
		return nil, err
	}
	type unreadable struct {
		task   QueuedTask
		record PendingReviewRecord
		merge  string
	}
	var found []unreadable
	merges := map[string]string{} // by window base, which subjects of one run share
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		task, err := uniqueTaskAcrossRoots(hosts, id)
		if err != nil {
			return nil, err
		}
		record, ok, err := readPendingReviewRecord(task.Root, id)
		if err == nil && !ok {
			err = fmt.Errorf("pending-review record for %s is missing", id)
		}
		if err != nil {
			return nil, err
		}
		base := pendingReviewSubjectBase(record)
		merge, read := merges[base]
		if !read {
			_, err := readPendingReviewHistory(repo, base, head)
			var mergeErr *auditMergeCommitError
			switch {
			case errors.As(err, &mergeErr):
				merge = mergeErr.sha
			case err != nil:
				return nil, err
			}
			merges[base] = merge
		}
		if merge != "" {
			found = append(found, unreadable{task: task, record: record, merge: merge})
		}
	}
	if len(found) == 0 {
		return nil, nil
	}
	moves := make([]TrustedTaskMove, 0, len(found))
	for _, subject := range found {
		id, title := subject.task.Item.ID, subject.task.Item.Title
		cause := fmt.Sprintf("final review could not run: merge commit %s is in the history it covers, after %s, and Coop reviews only a straight line of commits",
			shortCommit(subject.merge), shortCommit(pendingReviewSubjectBase(subject.record)))
		// Review authority lets an unblock send the task back to work that leaves the later commits
		// alone. A commit it cannot capture, such as a task commit that is itself a merge, still goes
		// to a person, without that authority: a capture failure must not wedge every loop again.
		var reopen *AuditReopenRecord
		pending := &subject.record
		if audit, prior, err := PrepareReviewReopen(repo, subject.task.Root, id, cause); err == nil && prior != nil {
			// A disputed receipt is what retires the record of a subject blocked for a person
			// (retireDisputedReview here, or the next loop start's load).
			audit.Finding, audit.DisputedReceipt = cause, prior.Fingerprint.Receipt
			reopen, pending = &audit, prior
		}
		decision := Decision{
			Question: "Close this task as it stands, or have it worked again?",
			Why: "Its final review could not run. Merge commit " + shortCommit(subject.merge) + " is in the history the review covers, " +
				"and Coop reviews only a straight line of commits. Coop does not record where a run's own work ended, so it cannot tell " +
				"a merge someone made after the run from one made during it to keep work out of review. Coop dropped that review instead " +
				"of stopping every later loop at it, and leaves the call to you.",
			Options: []string{
				"If you have checked the work, close it: coop tasks done " + id + " --force",
				"To have it worked again, unblock it: coop tasks unblock " + id + " \"<what to check or fix>\". " +
					"Its next review covers the same history, so it comes back here while that merge is in it.",
			},
			Recommendation: "Check the task's commit and every commit after " + shortCommit(pendingReviewSubjectBase(subject.record)) +
				" that you did not make yourself, including what the merge brought in. Close it with --force only once they are checked.",
		}
		moves = append(moves, TrustedTaskMove{
			Root: subject.task.Root, Task: subject.task.Item, NewState: StateBlocked, Reopen: reopen, PendingReview: pending,
			MetadataNames: []string{"log.md", "state.md", "decision.md"},
			AfterMove: func(dir string) error {
				if _, err := ReplaceDecision(dir, id, title, decision); err != nil {
					return err
				}
				return errors.Join(
					AppendTaskLogStrict(dir, cause+"; Coop dropped that review and blocked this task for a person (decision.md)"),
					NormalizeTaskState(id, dir, "blocked — final review could not run", "human decision required; see decision.md",
						"its final review met a merge commit it cannot review", "—"))
			},
		})
	}
	if err := MoveTrustedTasksFromDoneWith(moves); err != nil {
		return nil, fmt.Errorf("hand final-review tasks to a person: %w", err)
	}
	// Each record is retired as soon as its task is blocked. If that fails, the next loop start's
	// load retires it the same way: a disputed one by its receipt, one without authority as a task
	// that left the archive (dropDepartedPendingReview), which keeps it blocked.
	var errs []error
	for _, move := range moves {
		handed = append(handed, move.Task.ID)
		if move.Reopen != nil {
			errs = append(errs, retireDisputedReview(move.Root, *move.PendingReview, *move.Reopen))
		} else {
			_, err := retireDepartedPendingReview(move.Root, *move.PendingReview)
			errs = append(errs, err)
		}
	}
	return handed, errors.Join(errs...)
}

// PrepareReviewReopen captures the next host authority without changing task state. The caller
// supplies the returned pending record to the move transaction to reject intervening changes.
// Oversized findings still reopen normally; a truncated display must never identify a dispute.
func PrepareReviewReopen(repo, root, id, finding string) (AuditReopenRecord, *PendingReviewRecord, error) {
	audit, err := captureReviewReopen(repo, id)
	if err != nil {
		return audit, nil, err
	}
	finding = strings.Join(strings.Fields(finding), " ")
	if ValidTaskText(finding, false, TaskLineLimit) {
		audit.Finding = finding
	}
	prior, ok, err := readPendingReviewRecord(root, id)
	if err != nil || !ok {
		return audit, nil, err
	}
	if prior.Prepared || prior.Phase == PendingReviewReopened {
		return audit, nil, fmt.Errorf("task %s is not ready for a review verdict", id)
	}
	if audit.Finding != "" && prior.ReclosedWithoutCommit && normalizedReviewFinding(prior.Finding) == normalizedReviewFinding(audit.Finding) {
		audit.DisputedReceipt = prior.Fingerprint.Receipt
	}
	return audit, &prior, nil
}
