package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

func (s *Service) reconcileInterruptedOperations(ctx context.Context, startup bool) error {
	operations, err := s.store.ListIncompleteOperations(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, op := range operations {
		if backgroundOperation(op) {
			s.scheduleBackgroundOperation(op.ID)
			continue
		}
		if !startup && now.Sub(op.UpdatedAt) < s.operationStaleAfter {
			continue
		}
		unlock, claimed := s.tryLockOperation(op.IdempotencyKey)
		if !claimed {
			continue
		}
		latest, getErr := s.store.GetOperationByID(ctx, op.ID)
		if getErr != nil {
			unlock()
			return getErr
		}
		if latest.State != op.State || !latest.UpdatedAt.Equal(op.UpdatedAt) {
			unlock()
			continue
		}
		if op.Method == "RestoreWorkspaceCheckpoint" && op.State == session.OperationRunning {
			var req RestoreWorkspaceCheckpointRequest
			artifact, restoreErr := s.readWorkspaceCheckpointArtifact(ctx, op.ID)
			if restoreErr == nil {
				restoreErr = json.Unmarshal(op.Result, &req)
			}
			if restoreErr == nil {
				var file *os.File
				file, restoreErr = os.Open(artifact.path)
				if restoreErr == nil {
					_, restoreErr = file.Seek(artifact.offset, io.SeekStart)
					if restoreErr == nil {
						req.Stream = file
						_, restoreErr = s.executeRestoreWorkspaceCheckpoint(ctx, op, req)
					}
					restoreErr = errors.Join(restoreErr, file.Close())
				}
			}
			unlock()
			if restoreErr != nil {
				s.log.Warn("checkpoint restore remains fenced", "operation_id", op.ID, "error", restoreErr)
			}
			continue
		}
		if op.Method == "RunReview" && op.State == session.OperationRunning {
			if _, _, err := s.retainedReviewCandidate(ctx, op.ID); err == nil {
				_, err := s.resumeReview(ctx, op)
				unlock()
				if err != nil {
					return err
				}
				continue
			}
		}
		if op.Method == "CancelTurn" && op.State == session.OperationRunning {
			handled, err := s.reconcileCancelOperation(ctx, op)
			if err != nil {
				unlock()
				return err
			}
			if handled {
				unlock()
				continue
			}
		}
		if op.State == session.OperationReserved {
			detail := "operation admission was interrupted before execution"
			changed, err := s.store.ReconcileOperation(
				ctx, op, session.OperationFailed, session.CodeOperationUncertain,
				detail,
			)
			unlock()
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			s.log.Warn("reserved session operation reconciled",
				"operation_id", op.ID, "method", op.Method,
				"resource_type", op.ResourceType, "resource_id", op.ResourceID,
				"error_code", session.CodeOperationUncertain, "error_detail", detail,
			)
			continue
		}
		detail := "operation outcome is unknown"
		changed, err := s.store.ReconcileOperation(
			ctx, op, session.OperationUncertain, session.CodeOperationUncertain, detail,
		)
		unlock()
		if err != nil {
			return err
		}
		if !changed {
			continue
		}
		s.log.Warn("stale session operation made uncertain",
			"operation_id", op.ID, "method", op.Method,
			"resource_type", op.ResourceType, "resource_id", op.ResourceID,
			"error_code", session.CodeOperationUncertain, "error_detail", detail,
		)
	}
	return nil
}

func (s *Service) reconcileCancelOperation(ctx context.Context, op session.Operation) (bool, error) {
	var req session.CancelTurnRequest
	if err := json.Unmarshal(op.Result, &req); err != nil || req.SessionID == "" || req.TurnID == "" {
		return false, nil
	}
	bound, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return false, err
	}
	if err := requireSessionForkAuthority(bound); err != nil {
		if errors.Is(err, errLegacySessionForkUnproven) {
			return true, nil
		}
		return false, err
	}
	turn, err := s.store.GetTurn(ctx, req.SessionID, req.TurnID)
	if err != nil {
		return false, err
	}
	if sessionTurnTerminal(turn.State) {
		_, err := s.completeObservedCancel(ctx, op, turn)
		return true, err
	}
	if turn.State == session.TurnQueued {
		_, err := s.store.CancelTurn(ctx, op.IdempotencyKey, req)
		return true, err
	}
	s.mu.Lock()
	pending := s.pendingCancels[req.TurnID]
	active := pending != nil && pending.key == op.IdempotencyKey && pending.request == req
	s.mu.Unlock()
	return active, nil
}

func backgroundOperation(op session.Operation) bool {
	return op.State == session.OperationRunning && (op.Method == "CreateRemoteSession" || op.Method == "PublishReview")
}

func (s *Service) scheduleBackgroundOperation(operationID string) {
	op, err := s.store.GetOperationByID(context.Background(), operationID)
	if err != nil || !backgroundOperation(op) {
		return
	}
	if op.Method == "CreateRemoteSession" &&
		s.sessionQuarantined(deterministicSessionID(op.ID)) {
		return
	}
	s.mu.Lock()
	if !s.started || s.ctx == nil {
		s.mu.Unlock()
		return
	}
	s.operationMu.Lock()
	if s.backgroundActive[operationID] {
		s.operationMu.Unlock()
		s.mu.Unlock()
		return
	}
	select {
	case s.backgroundSlots <- struct{}{}:
		s.backgroundActive[operationID] = true
	default:
		s.operationMu.Unlock()
		s.mu.Unlock()
		return
	}
	s.operationMu.Unlock()
	ctx := s.ctx
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		completed := false
		defer func() {
			<-s.backgroundSlots
			s.operationMu.Lock()
			delete(s.backgroundActive, operationID)
			s.operationMu.Unlock()
			if completed && ctx.Err() == nil {
				s.scheduleWaitingBackgroundOperations(ctx, operationID)
			}
		}()
		var err error
		if op.Method == "PublishReview" {
			err = s.runPublishOperation(ctx, operationID)
		} else {
			err = s.runCreateOperation(ctx, operationID)
		}
		completed = err == nil || op.Method == "CreateRemoteSession"
		if err != nil && ctx.Err() == nil {
			code := session.CodeOf(err)
			if code == "" {
				code = session.CodeInternal
			}
			s.log.Error("background session operation stopped",
				"operation_id", operationID, "method", op.Method, "error_code", code,
			)
		}
	}()
}

func (s *Service) scheduleWaitingBackgroundOperations(ctx context.Context, completedID string) {
	operations, err := s.store.ListIncompleteOperations(ctx)
	if err != nil {
		s.log.Error("list queued session operations", "error", err)
		return
	}
	for _, op := range operations {
		if backgroundOperation(op) && op.ID != completedID {
			s.scheduleBackgroundOperation(op.ID)
		}
	}
}
