package sessionsvc

import (
	"context"
	"encoding/json"
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
		if op.Method == "CreateRemoteSession" && op.State == session.OperationRunning {
			s.scheduleCreateOperation(op.ID)
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

func (s *Service) scheduleCreateOperation(operationID string) {
	s.mu.Lock()
	if !s.started || s.ctx == nil {
		s.mu.Unlock()
		return
	}
	s.operationMu.Lock()
	if s.createActive[operationID] {
		s.operationMu.Unlock()
		s.mu.Unlock()
		return
	}
	select {
	case s.createSlots <- struct{}{}:
		s.createActive[operationID] = true
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
		defer func() {
			<-s.createSlots
			s.operationMu.Lock()
			delete(s.createActive, operationID)
			s.operationMu.Unlock()
			if ctx.Err() == nil {
				s.scheduleWaitingCreateOperations(ctx, operationID)
			}
		}()
		if err := s.runCreateOperation(ctx, operationID); err != nil && ctx.Err() == nil {
			code := session.CodeOf(err)
			if code == "" {
				code = session.CodeInternal
			}
			s.log.Error("asynchronous session creation stopped",
				"operation_id", operationID, "error_code", code,
			)
		}
	}()
}

func (s *Service) scheduleWaitingCreateOperations(ctx context.Context, completedID string) {
	operations, err := s.store.ListIncompleteOperations(ctx)
	if err != nil {
		s.log.Error("list queued session creations", "error", err)
		return
	}
	for _, op := range operations {
		if op.Method == "CreateRemoteSession" && op.State == session.OperationRunning && op.ID != completedID {
			s.scheduleCreateOperation(op.ID)
		}
	}
}
