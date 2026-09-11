package sessionsvc

import (
	"context"

	"github.com/AndrewDryga/coop/internal/session"
)

// CompactSessionState is the transport-neutral operator entrypoint for the
// session store's explicit, backup-first retry-receipt compaction.
func CompactSessionState(ctx context.Context, stateRoot, backupPath string) (CompactionResult, error) {
	return session.CompactTurnOperationResults(ctx, stateRoot, backupPath)
}

// CompactionResult and ErrCompactionUnfinished are re-exported so a caller can read the stage a
// compaction reached without importing internal/session, which the frozen import graph keeps below
// this package. The values are the store's own — this is a name, not a second projection.
type CompactionResult = session.TurnOperationCompactionResult

// ErrCompactionUnfinished marks a failure AFTER the retry-receipt rewrite committed.
var ErrCompactionUnfinished = session.ErrCompactionUnfinished
