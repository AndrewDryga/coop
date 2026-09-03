package session

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// TurnOperationCompactionResult describes the durable work performed by an
// explicit retry-receipt compaction. Byte counts are logical operation-result
// bytes; database counts are allocated bytes after WAL checkpoints.
type TurnOperationCompactionResult struct {
	BackupPath          string
	BackupBytes         int64
	CompactedOperations int
	ResultBytesBefore   int64
	ResultBytesAfter    int64
	DatabaseBytesBefore int64
	DatabaseBytesAfter  int64
}

// CompactTurnOperationResults opens an existing session database exclusively,
// creates and verifies a new backup, rewrites legacy full-turn receipts, checks
// database integrity, and only then vacuums reclaimed pages. It deliberately is
// not part of Open: an operator chooses the backup capacity and maintenance
// window for this potentially large one-time rewrite.
func CompactTurnOperationResults(ctx context.Context, root, backupPath string) (TurnOperationCompactionResult, error) {
	if root == "" {
		return TurnOperationCompactionResult{}, &Error{Code: CodeInvalidRequest, Detail: "state root is required"}
	}
	if _, err := os.Lstat(root); err != nil {
		return TurnOperationCompactionResult{}, fmt.Errorf("inspect existing session state root: %w", err)
	}
	databasePath := filepath.Join(root, databaseName)
	if _, err := os.Lstat(databasePath); err != nil {
		return TurnOperationCompactionResult{}, fmt.Errorf("inspect existing session database: %w", err)
	}
	store, err := openStore(root, options{clock: func() time.Time { return time.Now().UTC() }, id: randomID}, false)
	if err != nil {
		return TurnOperationCompactionResult{}, err
	}
	result, compactErr := store.compactTurnOperationResults(ctx, backupPath)
	return result, errors.Join(compactErr, store.Close())
}

func (s *Store) compactTurnOperationResults(ctx context.Context, backupPath string) (TurnOperationCompactionResult, error) {
	backupPath, err := validateBackupPath(s.root, backupPath)
	if err != nil {
		return TurnOperationCompactionResult{}, err
	}
	databasePath := filepath.Join(s.root, databaseName)
	before, err := regularFileSize(databasePath)
	if err != nil {
		return TurnOperationCompactionResult{}, err
	}
	sourceVersion, err := schemaVersion(s.db)
	if err != nil {
		return TurnOperationCompactionResult{}, err
	}
	if err := backupSQLiteDatabase(ctx, s.db, backupPath, sourceVersion); err != nil {
		return TurnOperationCompactionResult{}, err
	}
	backupBytes, err := regularFileSize(backupPath)
	if err != nil {
		return TurnOperationCompactionResult{}, fmt.Errorf("measure verified session backup: %w", err)
	}
	result := TurnOperationCompactionResult{
		BackupPath: backupPath, BackupBytes: backupBytes,
		DatabaseBytesBefore: before, DatabaseBytesAfter: before,
	}
	if err := checkpointWALIfNeeded(ctx, s.db); err != nil {
		return result, fmt.Errorf("backup retained at %s, but checkpoint session database: %w", backupPath, err)
	}
	if err := verifyWALMode(s.db); err != nil {
		return result, fmt.Errorf("backup retained at %s, but prepare session database: %w", backupPath, err)
	}
	if err := migrate(s.db); err != nil {
		return result, fmt.Errorf("backup retained at %s, but migrate session database: %w", backupPath, err)
	}
	if err := s.rewriteLegacyTurnOperationResults(ctx, &result); err != nil {
		return result, fmt.Errorf("compact retry receipts (backup retained at %s): %w", backupPath, err)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return result, fmt.Errorf("retry receipts compacted and backup retained at %s, but reclaim database space: %w", backupPath, err)
	}
	if err := checkpointWAL(ctx, s.db); err != nil {
		return result, fmt.Errorf("retry receipts compacted and backup retained at %s, but checkpoint compacted database: %w", backupPath, err)
	}
	result.DatabaseBytesAfter, err = regularFileSize(databasePath)
	if err != nil {
		return result, fmt.Errorf("retry receipts compacted and backup retained at %s, but measure compacted database: %w", backupPath, err)
	}
	if err := verifySQLiteDatabase(databasePath, SchemaVersion); err != nil {
		return result, fmt.Errorf("retry receipts compacted and backup retained at %s, but verify compacted database: %w", backupPath, err)
	}
	return result, nil
}

func (s *Store) rewriteLegacyTurnOperationResults(ctx context.Context, result *TurnOperationCompactionResult) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin retry receipt compaction: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM operations
		WHERE state = ? AND method IN ('SubmitTurn', 'ValidateTurnCandidate', 'CancelTurn')
		ORDER BY id`, string(OperationSucceeded))
	if err != nil {
		return fmt.Errorf("list turn operation receipts: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan turn operation receipt: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list turn operation receipts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close turn operation receipt list: %w", err)
	}
	for _, id := range ids {
		var method, resourceType, resourceID string
		var data []byte
		if err := tx.QueryRowContext(ctx, `
			SELECT method, resource_type, resource_id, result FROM operations WHERE id = ?`, id).
			Scan(&method, &resourceType, &resourceID, &data); err != nil {
			return fmt.Errorf("read turn operation receipt %s: %w", id, err)
		}
		turn, legacy, err := decodeTurnOperationResult(data)
		if err != nil {
			return fmt.Errorf("decode turn operation receipt %s: %w", id, err)
		}
		if !legacy {
			continue
		}
		if turn.ID != resourceID || !validTurnOperationResource(method, resourceType) {
			return fmt.Errorf("turn operation receipt %s has inconsistent resource identity", id)
		}
		compact, err := EncodeTurnOperationResult(turn)
		if err != nil {
			return fmt.Errorf("encode turn operation receipt %s: %w", id, err)
		}
		replayed, compactLegacy, err := decodeTurnOperationResult(compact)
		if err != nil || compactLegacy || !bytes.Equal(mustJSON(turnOperationResultFromTurn(replayed)), mustJSON(turnOperationResultFromTurn(turn))) {
			return fmt.Errorf("verify public replay for turn operation receipt %s", id)
		}
		updated, err := tx.ExecContext(ctx, `UPDATE operations SET result = ? WHERE id = ? AND result = ?`, compact, id, data)
		if err != nil {
			return fmt.Errorf("rewrite turn operation receipt %s: %w", id, err)
		}
		count, err := updated.RowsAffected()
		if err != nil || count != 1 {
			return fmt.Errorf("rewrite turn operation receipt %s: result changed during compaction", id)
		}
		result.CompactedOperations++
		result.ResultBytesBefore += int64(len(data))
		result.ResultBytesAfter += int64(len(compact))
	}
	if err := integrityCheck(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit retry receipt compaction: %w", err)
	}
	return nil
}

func validTurnOperationResource(method, resourceType string) bool {
	switch method {
	case "SubmitTurn", "CancelTurn":
		return resourceType == "turn"
	case "ValidateTurnCandidate":
		return resourceType == "turn_validation"
	default:
		return false
	}
}

func validateBackupPath(root, backupPath string) (string, error) {
	if backupPath == "" {
		return "", &Error{Code: CodeInvalidRequest, Detail: "backup path is required"}
	}
	absolute, err := filepath.Abs(filepath.Clean(backupPath))
	if err != nil {
		return "", fmt.Errorf("resolve backup path: %w", err)
	}
	databasePath, err := filepath.Abs(filepath.Join(root, databaseName))
	if err != nil {
		return "", fmt.Errorf("resolve session database path: %w", err)
	}
	if absolute == databasePath {
		return "", errors.New("backup path must differ from the session database")
	}
	parent, err := os.Lstat(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("inspect backup directory: %w", err)
	}
	if parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() {
		return "", errors.New("backup directory is not a real directory")
	}
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolve session state root: %w", err)
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve backup directory: %w", err)
	}
	realBackup := filepath.Join(realParent, filepath.Base(absolute))
	if pathWithin(realRoot, realBackup) {
		return "", errors.New("backup path must be outside the session state root")
	}
	return absolute, nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func backupSQLiteDatabase(ctx context.Context, db *sql.DB, path string, sourceVersion int) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create new session backup: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close new session backup: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("write session database backup: %w", err)
	}
	if err := syncRegularFile(path); err != nil {
		return err
	}
	if err := verifySQLiteDatabase(path, sourceVersion); err != nil {
		return fmt.Errorf("verify session database backup: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync session backup directory: %w", err)
	}
	complete = true
	return nil
}

func sqliteFileURI(path string, query url.Values) string {
	value := &url.URL{Scheme: "file", Path: path}
	value.RawQuery = query.Encode()
	return value.String()
}

func syncRegularFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open session backup for sync: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect session backup: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("session backup is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect session backup: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync session backup: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func checkpointWALIfNeeded(ctx context.Context, db *sql.DB) error {
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return fmt.Errorf("read session database journal mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return nil
	}
	return checkpointWAL(ctx, db)
}

func checkpointWAL(ctx context.Context, db *sql.DB) error {
	var busy, logPages, checkpointed int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logPages, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint session database: %w", err)
	}
	if busy != 0 {
		return errors.New("checkpoint session database: database is busy")
	}
	return nil
}

func integrityCheck(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) error {
	var result string
	if err := query.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("check session database integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("check session database integrity: %s", result)
	}
	return nil
}

func verifySQLiteDatabase(path string, wantVersion int) error {
	query := url.Values{"immutable": {"1"}, "mode": {"ro"}}
	db, err := sql.Open("sqlite", sqliteFileURI(path, query))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := integrityCheck(context.Background(), db); err != nil {
		return err
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read session backup schema version: %w", err)
	}
	if version != wantVersion {
		return fmt.Errorf("session backup schema version = %d, want %d", version, wantVersion)
	}
	return nil
}

func schemaVersion(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read session database schema version: %w", err)
	}
	if version > SchemaVersion {
		return 0, fmt.Errorf("unsupported schema version %d", version)
	}
	return version, nil
}

func regularFileSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("inspect session database size: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, errors.New("session database is not a regular file")
	}
	return info.Size(), nil
}
