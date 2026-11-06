package tasks

import (
	"errors"
	"fmt"
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

// PrepareReviewReopen captures the next host authority without changing task state. The caller
// supplies the returned pending record to the move transaction to reject intervening changes.
// Oversized findings still reopen normally; a truncated display must never identify a dispute.
func PrepareReviewReopen(repo, root, id, finding string) (AuditReopenRecord, *PendingReviewRecord, error) {
	audit, err := CaptureAuditReopen(repo, id)
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
