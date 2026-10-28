package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Before v26 only terminal costs were deltas; pending candidates stored the last
// native-session counter. Translate retained observations once. Historical public
// receipts and terminal rows stay unchanged; overwritten spend is not recoverable.
func backfillTurnCosts(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT t.id, t.session_id, t.usage_cost_usd, t.usage_cost_recorded,
		s.usage_cumulative_cost_usd, s.usage_cost_recorded FROM turns t JOIN sessions s ON s.id = t.session_id
		WHERE t.state IN ('queued', 'starting', 'running', 'awaiting_validation') ORDER BY t.session_id, t.ordinal`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pendingCost struct {
		turn, session string
		raw           float64
		known         bool
		previous      float64
		previousKnown bool
	}
	var pending []pendingCost
	seen := map[string]bool{}
	for rows.Next() {
		var item pendingCost
		if err := rows.Scan(&item.turn, &item.session, &item.raw, &item.known, &item.previous, &item.previousKnown); err != nil {
			return err
		}
		if item.known && seen[item.session] {
			return fmt.Errorf("session %s has multiple pending cost observations", item.session)
		}
		seen[item.session] = seen[item.session] || item.known
		pending = append(pending, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	stale, err := staleSessionCostResets(tx)
	if err != nil {
		return err
	}
	for _, item := range pending {
		before, after, candidate, err := pendingCostResets(tx, item.session, item.turn)
		if err != nil {
			return err
		}
		if reset := stale[item.session]; item.known && reset > 0 {
			if candidate > reset {
				before = true
				delete(stale, item.session)
			} else {
				after = true
			}
		}
		if item.known {
			amount := item.raw
			if !before && item.previousKnown && item.raw >= item.previous {
				amount -= item.previous
			}
			if _, err := tx.Exec(`UPDATE turns SET usage_cost_usd = ? WHERE id = ?`, amount, item.turn); err != nil {
				return err
			}
		}
		if !item.known && !before && !after {
			continue
		}
		cursor, known := item.raw, item.known
		if after || !item.known {
			cursor, known = 0, false
		}
		if _, err := tx.Exec(`UPDATE sessions SET usage_cumulative_cost_usd = ?, usage_cost_recorded = ? WHERE id = ?`, cursor, known, item.session); err != nil {
			return err
		}
	}
	for sessionID := range stale {
		if _, err := tx.Exec(`UPDATE sessions SET usage_cumulative_cost_usd = 0, usage_cost_recorded = 0 WHERE id = ?`, sessionID); err != nil {
			return err
		}
	}

	return nil
}

// Rotation events prove whether the retained candidate and current native ID
// belong to the same epoch. Missing history cannot reconstruct overwritten costs.
func pendingCostResets(tx *sql.Tx, sessionID, turnID string) (before, after bool, candidate int64, err error) {
	rows, err := tx.Query(`SELECT sequence, type, payload FROM events WHERE session_id = ? AND turn_id = ?
		AND type IN ('turn.awaiting_validation', 'session.target_rotated') ORDER BY sequence`, sessionID, turnID)
	if err != nil {
		return false, false, 0, err
	}
	defer rows.Close()
	var resets []int64
	for rows.Next() {
		var sequence int64
		var kind string
		var payload []byte
		if err := rows.Scan(&sequence, &kind, &payload); err != nil {
			return false, false, 0, err
		}
		if kind == string(EventTurnAwaitingValidation) {
			candidate = sequence
			continue
		}
		var rotation struct {
			Reset bool `json:"native_session_reset"`
		}
		if json.Unmarshal(payload, &rotation) == nil && rotation.Reset {
			resets = append(resets, sequence)
		}
	}
	for _, sequence := range resets {
		before = before || sequence < candidate
		after = after || sequence > candidate
	}
	return before, after, candidate, rows.Err()
}

// A reset can belong to an earlier, cost-unknown failed turn even when the
// session is idle now. Only retire a cursor proven to predate its native epoch.
// Later known terminal rows are ambiguous in v25: their cost may be a new report
// or a retained pre-reset candidate. Neither observations nor provenance can be
// reconstructed from those receipts, so leave that historical state unchanged.
func staleSessionCostResets(tx *sql.Tx) (map[string]int64, error) {
	rows, err := tx.Query(`SELECT e.session_id, e.sequence, e.payload FROM events e
 JOIN sessions s ON s.id = e.session_id WHERE e.type = 'session.target_rotated'
 AND s.usage_cost_recorded = 1 ORDER BY e.session_id, e.sequence`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	resets := map[string]int64{}
	for rows.Next() {
		var sessionID string
		var sequence int64
		var payload []byte
		if err := rows.Scan(&sessionID, &sequence, &payload); err != nil {
			return nil, err
		}
		var rotation struct {
			Reset bool `json:"native_session_reset"`
		}
		if json.Unmarshal(payload, &rotation) == nil && rotation.Reset {
			resets[sessionID] = sequence
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for sessionID, sequence := range resets {
		var settled bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM events e JOIN turns t ON t.id = e.turn_id
   WHERE e.session_id = ? AND e.sequence > ? AND t.usage_cost_recorded = 1
   AND e.type IN ('turn.completed', 'turn.failed', 'turn.cancelled', 'turn.interrupted'))`, sessionID, sequence).Scan(&settled); err != nil {
			return nil, err
		}
		if settled {
			delete(resets, sessionID)
		}
	}
	return resets, nil
}
