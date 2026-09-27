package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

// Checkpoints allocate new custody even during recovery. Keep the worker's
// reserve available for cleanup; an interrupted live restore stays fenced with
// its original body until space is available. This is pressure protection, not
// a filesystem quota: other host writers still share these volumes.
func (s *Service) checkpointDiskContext(parent context.Context, workspace string, incoming int64) (context.Context, func(), error) {
	check := func() error {
		for _, path := range []string{s.stateRoot, workspace} {
			volume, err := forkspace.MeasureFilesystem(storageMeasurePath(path))
			if err != nil {
				return &session.Error{Code: session.CodeStorageUnavailable, Detail: "cannot measure checkpoint storage"}
			}
			s.storage.mu.Lock()
			reserve := s.storageLimitsLocked(volume.CapacityBytes).ReserveBytes
			s.storage.mu.Unlock()
			available := volume.FreeBytes - reserve
			// Body, extracted members and the immutable recovery artifact. This
			// is only the known minimum: Git history/checkouts are monitored too.
			if available < 0 || (path == s.stateRoot && incoming > available/3) {
				return &session.Error{Code: session.CodeStorageUnavailable,
					Detail: fmt.Sprintf("checkpoint needs more storage; %d bytes available above the worker reserve", max(available, 0))}
			}
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, nil, err
	}
	incoming = 0 // Subsequent measurements account for bytes already written.
	ctx, stop := watchCheckpointDisk(parent, check)
	return ctx, stop, nil
}

func watchCheckpointDisk(parent context.Context, check func() error) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := check(); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return ctx, func() { cancel(nil); <-done }
}

// Called only under the session runtime lock after discard has tombstoned the
// workspace. Do not acquire operation locks here: capture takes them first.
func (s *Service) removeSessionCheckpointArtifacts(ctx context.Context, sessionID string) error {
	sess, err := s.store.GetSession(ctx, sessionID)
	if err != nil || sess.State != session.SessionDiscarded {
		return errors.New("checkpoint cleanup requires a discarded session")
	}
	var ids []string
	for _, method := range []string{"CheckpointWorkspace", "RestoreWorkspaceCheckpoint"} {
		owned, err := s.store.ListSessionArtifactOperationIDs(ctx, method, sessionID)
		if err != nil {
			return err
		}
		ids = append(ids, owned...)
	}
	for _, id := range ids {
		if !validSessionPathComponent(id) {
			return errors.New("invalid checkpoint artifact identity")
		}
		op, err := s.store.GetOperationByID(ctx, id)
		if err != nil {
			return err
		}
		if op.State == session.OperationRunning || op.State == session.OperationUncertain {
			if err := s.store.FailOperation(ctx, id, session.CodeInvalidSessionState, "checkpoint session was discarded"); err != nil {
				return err
			}
		}
		path := filepath.Join(s.stateRoot, "workspace-checkpoints", id+".checkpoint")
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("unsafe checkpoint artifact path")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
