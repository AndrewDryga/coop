package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// RetainedUsage is a turn aggregate, not a priced native request. Old records combine cache
// categories and cannot identify the served model or the request times inside a turn.
type RetainedUsage struct {
	SessionID, TurnID, Target, Mode string
	Started, Finished               time.Time
	Usage                           Usage
}

type UsageSnapshot struct {
	Turns     []RetainedUsage
	Truncated bool
}

// ReadUsageSnapshot does not create, migrate, checkpoint or rewrite session state. Read the live
// WAL rather than using immutable=1, which would silently miss newly committed usage.
func ReadUsageSnapshot(ctx context.Context, root string, since, until time.Time) (UsageSnapshot, error) {
	var out UsageSnapshot
	path := filepath.Join(root, databaseName)
	info, err := os.Lstat(path)
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() {
		return out, errors.New("session usage database is not a regular file")
	}
	db, err := sql.Open("sqlite", sqliteFileURI(path, url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1000)"}}))
	if err != nil {
		return out, errors.New("session usage database is unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, errors.New("retained session usage is unavailable")
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version > SchemaVersion || version < 5 {
		return out, errors.New("retained session usage schema is unavailable")
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.id,t.session_id,COALESCE(t.started_at,t.queued_at),t.finished_at,s.mode,
		       t.usage_input_tokens,t.usage_cached_input_tokens,t.usage_output_tokens,
		       t.usage_reasoning_tokens,t.usage_cost_recorded,
		       COALESCE((SELECT CASE WHEN length(e.payload)<=4096 THEN e.payload ELSE '' END FROM events e
		          WHERE e.session_id=t.session_id AND e.type IN ('session.created','session.target_rotated')
		          AND e.occurred_at<=COALESCE(t.started_at,t.queued_at) ORDER BY e.sequence DESC LIMIT 1),''),
		       (SELECT COUNT(*) FROM events e WHERE e.session_id=t.session_id
		          AND e.type='session.target_rotated' AND e.occurred_at>COALESCE(t.started_at,t.queued_at) AND e.occurred_at<=t.finished_at)
		FROM turns t JOIN sessions s ON s.id=t.session_id
		WHERE t.finished_at>=? AND t.finished_at<=?
		ORDER BY t.finished_at,t.id LIMIT 10001`, since.UnixNano(), until.UnixNano())
	if err != nil {
		return out, errors.New("retained session usage schema is unavailable")
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		seen++
		if seen > 10000 {
			out.Truncated = true
			break
		}
		var row RetainedUsage
		var started, finished int64
		var payload string
		var rotations int
		if err := rows.Scan(&row.TurnID, &row.SessionID, &started, &finished, &row.Mode,
			&row.Usage.InputTokens, &row.Usage.CachedInputTokens, &row.Usage.OutputTokens,
			&row.Usage.ReasoningTokens, &row.Usage.CostRecorded, &payload, &rotations); err != nil {
			return out, errors.New("retained session usage cannot be decoded")
		}
		if !row.Usage.Recorded() {
			continue
		}
		row.Started, row.Finished = time.Unix(0, started), time.Unix(0, finished)
		if rotations == 0 {
			var target struct {
				Target string `json:"target"`
				To     string `json:"to"`
			}
			if json.Unmarshal([]byte(payload), &target) == nil {
				row.Target = target.Target
				if target.To != "" {
					row.Target = target.To
				}
			}
		}
		out.Turns = append(out.Turns, row)
	}
	if err := rows.Err(); err != nil {
		return out, errors.New("retained session usage read was interrupted")
	}
	return out, nil
}
