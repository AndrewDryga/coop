package networkstate

import (
	"context"
	"errors"
	"time"
)

// ExecutionRetention is how long a finished run's record is kept after the run
// ended. Every settle pass reads every record, so keeping them all made each
// filtered launch slower forever. A run still owed cleanup has no limit: its
// record is the only map of what is left to remove.
const ExecutionRetention = 90 * 24 * time.Hour

// Expired reports whether retention may remove this run's record: its receipt
// is sealed, nothing it created is left to clean up, and it ended more than
// ExecutionRetention before now.
func (s ExecutionSummary) Expired(now time.Time) bool {
	return s.Final && !s.CleanupPending && now.Sub(s.EndedAt) > ExecutionRetention
}

// RemoveExpired deletes one run's record once Expired allows it, and reports
// whether it did. The summary that named the run may be stale, so the record is
// read again under the run's own lock, where no writer can change it: one that
// does not read and validate, is owed cleanup or is not old enough is kept. One
// already gone was taken by another settle pass.
func (e *Evidence) RemoveExpired(ctx context.Context, id string, now time.Time) (bool, error) {
	removed := false
	err := e.files.lockExecution(ctx, id, func() error {
		// read opens without following links, so a link planted under a record's
		// name fails here and is never removed.
		record, err := e.files.execution(id)
		if errors.Is(err, ErrEvidenceUnavailable) {
			return nil
		}
		if err != nil {
			return err
		}
		if !summarize(record).Expired(now) {
			return nil
		}
		// os.Root unlinks the name itself. No directory sync: a removal a crash
		// undoes leaves an expired record that the next pass removes again.
		if err := e.files.root.Remove("execution-" + id + ".json"); err != nil {
			return err
		}
		removed = true
		return nil
	})
	return removed, err
}
