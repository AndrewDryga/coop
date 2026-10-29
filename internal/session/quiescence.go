package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// Quiescence counts native work rather than HTTP requests or warm runtime slots. A controller
// must stop new submissions before using this snapshot to decide whether to restart its worker.
type Quiescence struct {
	ActiveTurns      int  `json:"active_turns"`
	ActiveOperations int  `json:"active_operations"`
	ActiveSessions   int  `json:"active_sessions"`
	Idle             bool `json:"idle"`
}

// ReadQuiescence observes the running service's committed WAL without acquiring its lifetime
// lock, migration or checkpoint. readonly_shm keeps a separate process's WAL index private;
// immutable=1 would silently ignore work accepted since the last checkpoint. A read-only
// filesystem mount also prevents sidecar creation if the daemon stops during this read.
func ReadQuiescence(ctx context.Context, root string) (Quiescence, error) {
	var out Quiescence
	path := filepath.Join(root, databaseName)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if suffix == "-journal" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return out, err
		}
		if !info.Mode().IsRegular() {
			return out, fmt.Errorf("session database%s is not a regular file", suffix)
		}
		if suffix == "-journal" && info.Size() > 0 {
			return out, errors.New("session database has an unfinished rollback journal")
		}
	}
	db, err := sql.Open("sqlite", sqliteFileURI(path, url.Values{"mode": {"ro"}, "readonly_shm": {"1"}, "_pragma": {"busy_timeout(1000)"}}))
	if err != nil {
		return out, fmt.Errorf("open session database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, fmt.Errorf("read session work: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return out, fmt.Errorf("read session schema: %w", err)
	}
	if version < OldestSchemaVersion || version > SchemaVersion {
		return out, fmt.Errorf("unsupported session schema %d", version)
	}
	// Unknown states stay busy rather than becoming a false idle result after a schema change.
	// Stored uncertain creates/discards cannot replay. A discard becomes uncertain only after
	// recovery acquires its operation lock; an executing discard remains reserved/running.
	if err := tx.QueryRowContext(ctx, `SELECT
  (SELECT COUNT(*) FROM turns WHERE state NOT IN ('completed','failed','cancelled','interrupted','budget_exhausted')),
  (SELECT COUNT(*) FROM operations WHERE state NOT IN ('succeeded','failed') AND NOT (state='uncertain' AND method IN ('CreateRemoteSession','Discard'))),
  (SELECT COUNT(*) FROM sessions WHERE active_turn_id<>'' OR queued_turn_count<>0 OR activity<>'parked' OR state NOT IN ('open','exhausted','closed','discarded'))
 `).Scan(&out.ActiveTurns, &out.ActiveOperations, &out.ActiveSessions); err != nil {
		return out, fmt.Errorf("read active session work: %w", err)
	}
	out.Idle = out.ActiveTurns == 0 && out.ActiveOperations == 0 && out.ActiveSessions == 0
	return out, nil
}
