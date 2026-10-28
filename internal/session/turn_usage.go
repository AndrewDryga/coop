package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RecordTurnUsage checkpoints a delivered attempt before its result is validated
// or its provider is replaced. Repeating the same observation adds no spend.
func (s *Store) RecordTurnUsage(ctx context.Context, req RecordTurnUsageRequest) (Usage, error) {
	if req.SessionID == "" || req.TurnID == "" || !validBoundedText(req.SessionID, MaxIDBytes) || !validBoundedText(req.TurnID, MaxIDBytes) {
		return Usage{}, &Error{Code: CodeInvalidRequest, Detail: "session and turn are required"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("begin turn usage checkpoint: %w", err)
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Usage{}, ErrSessionNotFound
	}
	if err != nil {
		return Usage{}, err
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", req.SessionID, req.TurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Usage{}, ErrTurnNotFound
	}
	if err != nil {
		return Usage{}, err
	}
	if sess.ActiveTurnID != turn.ID || sess.Activity != ActivityRunning || turn.State != TurnRunning || turn.SendState != SendStateSent {
		return Usage{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn is not running after send"}
	}
	if err := s.recordTurnUsageTx(ctx, tx, req.SessionID, &turn, req.Usage, req.CumulativeCostUSD, req.CostRecorded); err != nil {
		return Usage{}, err
	}
	if err := tx.Commit(); err != nil {
		return Usage{}, fmt.Errorf("commit turn usage checkpoint: %w", err)
	}
	return turn.Usage, nil
}
