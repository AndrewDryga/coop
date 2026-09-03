package sessionsvc

import (
	"context"

	"github.com/AndrewDryga/coop/internal/session"
)

// CompactSessionState is the transport-neutral operator entrypoint for the
// session store's explicit, backup-first retry-receipt compaction.
func CompactSessionState(ctx context.Context, stateRoot, backupPath string) (session.TurnOperationCompactionResult, error) {
	return session.CompactTurnOperationResults(ctx, stateRoot, backupPath)
}
