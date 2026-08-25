package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const (
	databaseName = "session.sqlite"
	lockName     = "lock"
	busyTimeout  = 5000
)

type Store struct {
	db    *sql.DB
	lock  *os.File
	root  string
	clock func() time.Time
	id    func(string) string

	closeOnce sync.Once
	closeErr  error
}

func Open(root string, opts ...Option) (*Store, error) {
	settings := options{clock: func() time.Time { return time.Now().UTC() }, id: randomID}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	if root == "" {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "state root is required"}
	}
	if err := ensureStateRoot(root); err != nil {
		return nil, err
	}
	lock, err := openStateLock(filepath.Join(root, lockName))
	if err != nil {
		return nil, err
	}
	databasePath := filepath.Join(root, databaseName)
	if err := ensureDatabaseFile(databasePath); err != nil {
		return nil, closeStateLock(lock, err)
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, closeStateLock(lock, fmt.Errorf("open session database: %w", err))
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := func(err error) (*Store, error) {
		return nil, closeStateLock(lock, errors.Join(err, db.Close()))
	}
	if err := db.Ping(); err != nil {
		return closeOnError(fmt.Errorf("ping session database: %w", err))
	}
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return closeOnError(fmt.Errorf("enable foreign keys: %w", err))
	}
	if err := verifyWALMode(db); err != nil {
		return closeOnError(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout)); err != nil {
		return closeOnError(fmt.Errorf("set busy timeout: %w", err))
	}
	if err := migrate(db); err != nil {
		return closeOnError(err)
	}
	return &Store{db: db, lock: lock, root: root, clock: settings.clock, id: settings.id}, nil
}

// verifyWALMode switches the connection into WAL mode and confirms the switch actually took.
// SQLite silently keeps the prior journal mode instead of erroring when WAL isn't available
// (an in-memory database, or a connection that asserts the file is immutable), and every
// Store method depends on WAL's concurrent-reader guarantees, so a silent non-WAL file has to
// fail loudly here rather than surface later as a confusing lock error.
func verifyWALMode(db *sql.DB) error {
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("enable WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("enable WAL: got %q", journalMode)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var errs []error
		if s.db != nil {
			errs = append(errs, s.db.Close())
		}
		if s.lock != nil {
			if err := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN); err != nil {
				errs = append(errs, fmt.Errorf("release session state lock: %w", err))
			}
			if err := s.lock.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close session state lock: %w", err))
			}
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

func (s *Store) Root() string { return s.root }

func ensureStateRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create state root: %w", err)
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return fmt.Errorf("inspect state root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("state root is a symlink")
	}
	if !info.IsDir() {
		return fmt.Errorf("state root is not a directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("protect state root: %w", err)
	}
	return nil
}

func ensureDatabaseFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return fmt.Errorf("create session database: %w", createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close new session database: %w", closeErr)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect session database: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("session database is a symlink")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("session database is not a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect session database: %w", err)
	}
	return nil
}

func openStateLock(path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errors.New("session state lock is a symlink")
		}
		if errors.Is(err, syscall.EISDIR) {
			return nil, errors.New("session state lock is not a regular file")
		}
		return nil, fmt.Errorf("open session state lock: %w", err)
	}
	closeOnError := func(err error) (*os.File, error) {
		_ = lock.Close()
		return nil, err
	}
	info, err := lock.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("inspect session state lock: %w", err))
	}
	if !info.Mode().IsRegular() {
		return closeOnError(errors.New("session state lock is not a regular file"))
	}
	if err := lock.Chmod(0o600); err != nil {
		return closeOnError(fmt.Errorf("protect session state lock: %w", err))
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return closeOnError(errors.New("another session daemon owns this state root"))
		}
		return closeOnError(fmt.Errorf("lock session state root: %w", err))
	}
	return lock, nil
}

func closeStateLock(lock *os.File, err error) error {
	if lock == nil {
		return err
	}
	return errors.Join(err, syscall.Flock(int(lock.Fd()), syscall.LOCK_UN), lock.Close())
}

// randomID mints a durable ID for the long-running daemon. The panic below looks alarming for
// a daemon, but converting it to a returned error would be actively misleading: as of Go 1.24
// (go.dev/issue/66821), crypto/rand.Read is documented to "never return an error" and to
// "crash[] the program irrecoverably" via an unrecoverable runtime fatal error — not a
// catchable panic — if its underlying OS entropy source ever fails. On every platform coop
// ships for (getrandom(2) on Linux, arc4random_buf(3) on Darwin) that source is itself
// documented to never fail. So this err != nil branch is unreachable dead code: by the time
// rand.Read could return a non-nil error, the Go runtime has already terminated the process a
// few frames down, and no error handling in any caller of randomID could run, let alone help.
// Threading an error through every caller here would imply a recoverable failure mode that
// cannot occur and cannot be exercised in a test without crashing the test binary itself (verified
// empirically: swapping rand.Reader to a failing io.Reader aborts the process via runtime.fatal,
// not recover()). The panic stays as documentation of that unreachability, not a real code path.
func randomID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func (s *Store) now() time.Time { return s.clock().UTC() }

func (s *Store) begin(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

func validateOperation(method, key, hash string) error {
	if method == "" || len(method) > MaxMethodBytes || strings.IndexByte(method, 0) >= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "invalid operation method"}
	}
	if key == "" || len(key) > MaxIdempotencyKeyBytes || strings.IndexByte(key, 0) >= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "invalid idempotency key"}
	}
	if hash == "" {
		return &Error{Code: CodeInvalidRequest, Detail: "request hash is required"}
	}
	return nil
}

func (s *Store) reserveOperationTx(ctx context.Context, tx *sql.Tx, method, key, hash string) (Operation, bool, error) {
	if err := validateOperation(method, key, hash); err != nil {
		return Operation{}, false, err
	}
	op, err := scanOperation(tx.QueryRowContext(ctx, `
		SELECT id, method, idempotency_key, request_hash, state, resource_type,
		       resource_id, result, error_code, error_detail, created_at, updated_at
		FROM operations WHERE idempotency_key = ?`, key))
	if err == nil {
		if op.Method != method || op.RequestHash != hash {
			return Operation{}, false, &Error{Code: CodeIdempotencyConflict, Detail: "idempotency key is bound to another request"}
		}
		return op, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, false, fmt.Errorf("lookup operation: %w", err)
	}
	now := s.now()
	op = Operation{
		ID:             s.id("op"),
		Method:         method,
		IdempotencyKey: key,
		RequestHash:    hash,
		State:          OperationReserved,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO operations
		(id, method, idempotency_key, request_hash, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, op.ID, op.Method, op.IdempotencyKey, op.RequestHash,
		string(op.State), now.UnixNano(), now.UnixNano()); err != nil {
		return Operation{}, false, fmt.Errorf("reserve operation: %w", err)
	}
	return op, false, nil
}

func (s *Store) ReserveOperation(ctx context.Context, method, key string, request any) (Operation, bool, error) {
	hash, err := CanonicalRequestHash(request)
	if err != nil {
		return Operation{}, false, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Operation{}, false, fmt.Errorf("begin operation: %w", err)
	}
	defer tx.Rollback()
	op, replay, err := s.reserveOperationTx(ctx, tx, method, key, hash)
	if err != nil {
		return Operation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, false, fmt.Errorf("commit operation: %w", err)
	}
	return op, replay, nil
}

// MarkOperationRunning durably records the intent that makes a cross-process operation
// recoverable. The result column is deliberately reused so the reservation and its intent
// commit in one small transaction; a retry must present the exact same bytes.
func (s *Store) MarkOperationRunning(ctx context.Context, operationID string, intent []byte) error {
	if operationID == "" || !validBoundedText(operationID, MaxIDBytes) {
		return &Error{Code: CodeInvalidRequest, Detail: "operation id is required"}
	}
	if len(intent) == 0 || len(intent) > MaxOperationResultBytes {
		return &Error{Code: CodeInvalidRequest, Detail: "operation intent is outside bounds"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operation intent: %w", err)
	}
	defer tx.Rollback()
	var state OperationState
	var current []byte
	if err := tx.QueryRowContext(ctx, "SELECT state, result FROM operations WHERE id = ?", operationID).Scan(&state, &current); errors.Is(err, sql.ErrNoRows) {
		return ErrOperationNotFound
	} else if err != nil {
		return fmt.Errorf("read operation intent: %w", err)
	}
	switch state {
	case OperationReserved:
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, result = ?, updated_at = ? WHERE id = ?`,
			string(OperationRunning), intent, s.now().UnixNano(), operationID); err != nil {
			return fmt.Errorf("mark operation running: %w", err)
		}
	case OperationRunning:
		if !bytes.Equal(current, intent) {
			return &Error{Code: ErrOperationIntentConflict.Code, Detail: "operation intent differs from the recorded intent"}
		}
		return nil
	default:
		return &Error{Code: CodeInvalidRequest, Detail: "operation is already terminal"}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operation intent: %w", err)
	}
	return nil
}

// ReplaceOperationIntent advances a running operation's recoverable intent
// with an optimistic compare-and-swap. It is used when admission is durable
// before a slow external read (for example Git ref resolution), then that read
// supplies immutable pins required by the execution phase.
func (s *Store) ReplaceOperationIntent(
	ctx context.Context,
	operationID string,
	expected []byte,
	intent []byte,
) error {
	if operationID == "" || !validBoundedText(operationID, MaxIDBytes) {
		return &Error{Code: CodeInvalidRequest, Detail: "operation id is required"}
	}
	if len(expected) == 0 || len(intent) == 0 || len(intent) > MaxOperationResultBytes {
		return &Error{Code: CodeInvalidRequest, Detail: "operation intent is outside bounds"}
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE operations SET result = ?, updated_at = ?
		WHERE id = ? AND state = ? AND result = ?`,
		intent, s.now().UnixNano(), operationID, string(OperationRunning), expected,
	)
	if err != nil {
		return fmt.Errorf("replace operation intent: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("replace operation intent: %w", err)
	}
	if rows != 1 {
		return &Error{Code: CodeOperationIntentConflict, Detail: "operation intent changed before it could be advanced"}
	}
	return nil
}

// MarkOperationUncertain preserves the recorded intent while making a running
// operation non-replayable after its external outcome is unknown.
func (s *Store) MarkOperationUncertain(ctx context.Context, operationID string) error {
	if operationID == "" || !validBoundedText(operationID, MaxIDBytes) {
		return &Error{Code: CodeInvalidRequest, Detail: "operation id is required"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin uncertain operation: %w", err)
	}
	defer tx.Rollback()
	var state OperationState
	if err := tx.QueryRowContext(ctx, "SELECT state FROM operations WHERE id = ?", operationID).Scan(&state); errors.Is(err, sql.ErrNoRows) {
		return ErrOperationNotFound
	} else if err != nil {
		return fmt.Errorf("read uncertain operation state: %w", err)
	}
	switch state {
	case OperationUncertain:
		return nil
	case OperationRunning:
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, error_code = ?, error_detail = ?, updated_at = ? WHERE id = ?`,
			string(OperationUncertain), string(CodeOperationUncertain), "operation outcome is unknown", s.now().UnixNano(), operationID); err != nil {
			return fmt.Errorf("mark operation uncertain: %w", err)
		}
	default:
		return &Error{Code: CodeInvalidRequest, Detail: "operation is not running"}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit uncertain operation: %w", err)
	}
	return nil
}

// ReconcileOperation applies a watchdog transition only to the exact snapshot
// the watchdog inspected. Timestamp, intent bytes, and resource identity form
// the mutable snapshot, so a concurrent replay or receipt wins even under a
// fixed/coarse clock and stale reconciliation becomes a harmless no-op.
func (s *Store) ReconcileOperation(
	ctx context.Context,
	operation Operation,
	state OperationState,
	code ErrorCode,
	detail string,
) (bool, error) {
	if operation.ID == "" || (state != OperationFailed && state != OperationUncertain) {
		return false, &Error{Code: CodeInvalidRequest, Detail: "invalid operation reconciliation"}
	}
	detail = boundedErrorDetail(detail)
	result, err := s.db.ExecContext(ctx, `
		UPDATE operations SET state = ?, error_code = ?, error_detail = ?, updated_at = ?
		WHERE id = ? AND state = ? AND updated_at = ?
		  AND resource_type = ? AND resource_id = ? AND result IS ?`,
		string(state), string(code), detail, s.now().UnixNano(),
		operation.ID, string(operation.State), operation.UpdatedAt.UnixNano(),
		operation.ResourceType, operation.ResourceID, operation.Result,
	)
	if err != nil {
		return false, fmt.Errorf("reconcile operation: %w", err)
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *Store) CompleteOperation(ctx context.Context, id, resourceType, resourceID string, result []byte) error {
	if len(result) > MaxOperationResultBytes {
		return &Error{Code: CodeInvalidRequest, Detail: "operation result is too large"}
	}
	return s.updateOperation(ctx, id, OperationSucceeded, resourceType, resourceID, result, "", "")
}

func (s *Store) FailOperation(ctx context.Context, id string, code ErrorCode, detail string) error {
	if code == "" {
		return &Error{Code: CodeInvalidRequest, Detail: "operation error code is required"}
	}
	if len(detail) > MaxErrorDetailBytes || !utf8.ValidString(detail) {
		return &Error{Code: CodeInvalidRequest, Detail: "operation error detail is outside bounds"}
	}
	if id == "" {
		return &Error{Code: CodeInvalidRequest, Detail: "operation id is required"}
	}
	detail = boundedErrorDetail(detail)
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operation failure: %w", err)
	}
	defer tx.Rollback()
	var current OperationState
	if err := tx.QueryRowContext(ctx, "SELECT state FROM operations WHERE id = ?", id).Scan(&current); errors.Is(err, sql.ErrNoRows) {
		return ErrOperationNotFound
	} else if err != nil {
		return fmt.Errorf("read operation state: %w", err)
	}
	if current != OperationReserved && current != OperationRunning && current != OperationUncertain {
		return &Error{Code: CodeInvalidRequest, Detail: "operation is already terminal"}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE operations SET state = ?, error_code = ?, error_detail = ?, updated_at = ? WHERE id = ?`,
		string(OperationFailed), string(code), detail, s.now().UnixNano(), id); err != nil {
		return fmt.Errorf("fail operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operation failure: %w", err)
	}
	return nil
}

func (s *Store) updateOperation(ctx context.Context, id string, state OperationState, resourceType, resourceID string, result []byte, code ErrorCode, detail string) error {
	if id == "" {
		return &Error{Code: CodeInvalidRequest, Detail: "operation id is required"}
	}
	if len(result) > MaxOperationResultBytes {
		return &Error{Code: CodeInvalidRequest, Detail: "operation result is too large"}
	}
	detail = boundedErrorDetail(detail)
	tx, err := s.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operation update: %w", err)
	}
	defer tx.Rollback()
	var current OperationState
	if err := tx.QueryRowContext(ctx, "SELECT state FROM operations WHERE id = ?", id).Scan(&current); errors.Is(err, sql.ErrNoRows) {
		return ErrOperationNotFound
	} else if err != nil {
		return fmt.Errorf("read operation state: %w", err)
	}
	if current != OperationReserved && current != OperationRunning && current != OperationUncertain {
		return &Error{Code: CodeInvalidRequest, Detail: "operation is already terminal"}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE operations SET state = ?, resource_type = ?, resource_id = ?, result = ?,
		       error_code = ?, error_detail = ?, updated_at = ? WHERE id = ?`,
		string(state), resourceType, resourceID, result, string(code), detail, s.now().UnixNano(), id); err != nil {
		return fmt.Errorf("update operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operation update: %w", err)
	}
	return nil
}

func (s *Store) GetOperation(ctx context.Context, key string) (Operation, error) {
	return s.getOperation(ctx, `WHERE idempotency_key = ?`, key)
}

func (s *Store) GetOperationByID(ctx context.Context, id string) (Operation, error) {
	return s.getOperation(ctx, `WHERE id = ?`, id)
}

// ListIncompleteOperations returns every durable mutation that has not reached
// a terminal receipt. Reserved rows are included because a process can crash
// between admission and persisting its recoverable running intent.
func (s *Store) ListIncompleteOperations(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, method, idempotency_key, request_hash, state, resource_type,
		       resource_id, result, error_code, error_detail, created_at, updated_at
		FROM operations WHERE state IN (?, ?) ORDER BY updated_at, id`,
		string(OperationReserved), string(OperationRunning))
	if err != nil {
		return nil, fmt.Errorf("list incomplete operations: %w", err)
	}
	defer rows.Close()
	var operations []Operation
	for rows.Next() {
		op, err := scanOperationRows(rows)
		if err != nil {
			return nil, fmt.Errorf("scan incomplete operation: %w", err)
		}
		operations = append(operations, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list incomplete operations: %w", err)
	}
	return operations, nil
}

func (s *Store) ListOperationIDsForResource(
	ctx context.Context,
	method string,
	resourceType string,
	resourceID string,
) ([]string, error) {
	if method == "" || resourceType == "" || resourceID == "" {
		return nil, errors.New("operation resource query is incomplete")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id
		FROM operations
		WHERE method = ? AND resource_type = ? AND resource_id = ?
		ORDER BY created_at, id`,
		method,
		resourceType,
		resourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list operation resource IDs: %w", err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan operation resource ID: %w", err)
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list operation resource IDs: %w", err)
	}
	return result, nil
}

func (s *Store) getOperation(ctx context.Context, where string, arg string) (Operation, error) {
	op, err := scanOperation(s.db.QueryRowContext(ctx, `
		SELECT id, method, idempotency_key, request_hash, state, resource_type,
		       resource_id, result, error_code, error_detail, created_at, updated_at
		FROM operations `+where, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrOperationNotFound
	}
	if err != nil {
		return Operation{}, fmt.Errorf("get operation: %w", err)
	}
	return op, nil
}

func scanOperation(row *sql.Row) (Operation, error) {
	return scanOperationValue(row.Scan)
}

type operationScanner interface {
	Scan(...any) error
}

func scanOperationRows(row operationScanner) (Operation, error) {
	return scanOperationValue(row.Scan)
}

func scanOperationValue(scan func(...any) error) (Operation, error) {
	var op Operation
	var state, resourceType, resourceID, errorCode, errorDetail string
	var createdAt, updatedAt int64
	if err := scan(&op.ID, &op.Method, &op.IdempotencyKey, &op.RequestHash, &state,
		&resourceType, &resourceID, &op.Result, &errorCode, &errorDetail, &createdAt, &updatedAt); err != nil {
		return Operation{}, err
	}
	op.State = OperationState(state)
	op.ResourceType = resourceType
	op.ResourceID = resourceID
	op.ErrorCode = ErrorCode(errorCode)
	op.ErrorDetail = errorDetail
	op.CreatedAt = fromUnixNano(createdAt)
	op.UpdatedAt = fromUnixNano(updatedAt)
	return op, nil
}

func (s *Store) CreateSession(ctx context.Context, key string, req CreateSessionRequest) (Session, error) {
	req = normalizeCreateRequest(req)
	hash, err := CanonicalRequestHash(req)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin session creation: %w", err)
	}
	defer tx.Rollback()
	op, replay, err := s.reserveOperationTx(ctx, tx, "CreateSession", key, hash)
	if err != nil {
		return Session{}, err
	}
	if replay {
		if err := tx.Commit(); err != nil {
			return Session{}, fmt.Errorf("commit operation replay: %w", err)
		}
		return s.replaySession(op)
	}
	if err := validateCreateRequest(req); err != nil {
		return Session{}, s.failAndCommit(tx, op.ID, err)
	}
	now := s.now()
	sess := Session{
		ID:                 req.ID,
		ExternalRef:        req.ExternalRef,
		Target:             req.Target,
		Policy:             req.Policy,
		PolicyDigest:       req.PolicyDigest,
		ProjectEnv:         !req.OmitEnv,
		ProjectMCP:         !req.OmitMCP,
		RepositoryReadOnly: req.RepositoryReadOnly,
		Repository:         req.Repository,
		Workspace:          req.Workspace,
		ForkName:           req.ForkName,
		BaseCommit:         req.BaseCommit,
		PullRequest:        clonePullRequestBinding(req.PullRequest),
		Companions:         append([]CompanionRepository(nil), req.Companions...),
		TurnTimeout:        req.TurnTimeout,
		MaxPatchBytes:      req.MaxPatchBytes,
		Revision:           1,
		State:              SessionOpen,
		Activity:           ActivityParked,
		MaxTurns:           normalized(req.MaxTurns, DefaultMaxTurns),
		MaxQueuedTurns:     normalized(req.MaxQueuedTurns, DefaultMaxQueuedTurns),
		MaxQueuedBytes:     normalized(req.MaxQueuedBytes, DefaultMaxQueuedBytes),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if sess.ID == "" {
		sess.ID = s.id("ses")
	}
	companions, err := json.Marshal(sess.Companions)
	if err != nil {
		return Session{}, fmt.Errorf("encode companion repositories: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions
		(id, external_ref, target, policy, policy_digest, project_env, project_mcp, repository_read_only, repository, workspace, fork_name, base_commit, companions,
		 pull_request_number, pull_request_ref, pull_request_head_commit,
		 turn_timeout, max_patch_bytes, revision, state, activity, max_turns, max_queued_turns, max_queued_bytes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, sess.ID, sess.ExternalRef, sess.Target,
		sess.Policy, sess.PolicyDigest, sess.ProjectEnv, sess.ProjectMCP, sess.RepositoryReadOnly, sess.Repository, sess.Workspace, sess.ForkName, sess.BaseCommit,
		string(companions), pullRequestNumber(sess.PullRequest), pullRequestRef(sess.PullRequest), pullRequestHead(sess.PullRequest),
		int64(sess.TurnTimeout), sess.MaxPatchBytes, sess.Revision, string(sess.State), string(sess.Activity), sess.MaxTurns,
		sess.MaxQueuedTurns, sess.MaxQueuedBytes, now.UnixNano(), now.UnixNano()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: sessions.id") {
			return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidRequest, Detail: "session id is already in use"})
		}
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	payload := mustJSON(map[string]any{"target": sess.Target})
	if _, err := s.appendEventTx(ctx, tx, sess.ID, "", EventSessionCreated, 1, payload); err != nil {
		return Session{}, fmt.Errorf("append session.created: %w", err)
	}
	sess.LastEventSequence = 1
	result, err := json.Marshal(sess)
	if err != nil {
		return Session{}, fmt.Errorf("encode session result: %w", err)
	}
	if err := s.completeOperationTx(ctx, tx, op.ID, "session", sess.ID, result); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit session creation: %w", err)
	}
	return sess, nil
}

func (s *Store) replaySession(op Operation) (Session, error) {
	if op.State == OperationFailed {
		return Session{}, operationError(op)
	}
	if op.State != OperationSucceeded {
		return Session{}, ErrOperationUncertain
	}
	var sess Session
	if err := json.Unmarshal(op.Result, &sess); err != nil {
		return Session{}, fmt.Errorf("decode session operation result: %w", err)
	}
	if sess.ID == "" {
		return Session{}, errors.New("decode session operation result: missing session id")
	}
	return sess, nil
}

func normalizeCreateRequest(req CreateSessionRequest) CreateSessionRequest {
	if req.TurnTimeout == 0 {
		req.TurnTimeout = DefaultTurnTimeout
	}
	if req.MaxPatchBytes == 0 {
		req.MaxPatchBytes = DefaultMaxPatchBytes
	}
	if req.PolicyDigest == "" {
		companions, _ := json.Marshal(req.Companions)
		bindings := []string{
			req.Policy, req.Target, req.Repository, req.Workspace, req.ForkName, req.BaseCommit,
			string(companions), fmt.Sprintf("%d", pullRequestNumber(req.PullRequest)),
			pullRequestRef(req.PullRequest), pullRequestHead(req.PullRequest),
			fmt.Sprintf("%d", req.TurnTimeout), fmt.Sprintf("%d", req.MaxPatchBytes),
		}
		if req.OmitEnv || req.OmitMCP {
			bindings = append(bindings, fmt.Sprintf("omit-env=%t", req.OmitEnv), fmt.Sprintf("omit-mcp=%t", req.OmitMCP))
		}
		sum := sha256.Sum256([]byte(strings.Join(bindings, "\x00")))
		req.PolicyDigest = hex.EncodeToString(sum[:])
	}
	return req
}

func validateCreateRequest(req CreateSessionRequest) error {
	if req.ID != "" && !validBoundedText(req.ID, MaxIDBytes) {
		return &Error{Code: CodeInvalidRequest, Detail: "session id is outside bounds"}
	}
	if len(req.ExternalRef) > MaxExternalRefBytes || strings.IndexByte(req.ExternalRef, 0) >= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "external reference is too large or contains NUL"}
	}
	if req.Target == "" || len(req.Target) > MaxTargetBytes || strings.IndexByte(req.Target, 0) >= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "exact target is required"}
	}
	if req.MaxTurns < 0 || req.MaxTurns > MaxTurnsLimit || req.MaxQueuedTurns < 0 || req.MaxQueuedTurns > MaxQueuedTurnsLimit || req.MaxQueuedBytes < 0 || req.MaxQueuedBytes > MaxQueuedBytesLimit {
		return &Error{Code: CodeInvalidRequest, Detail: "session limits are outside bounds"}
	}
	if len(req.PolicyDigest) != sha256.Size*2 || !validBoundedText(req.PolicyDigest, sha256.Size*2) {
		return &Error{Code: CodeInvalidRequest, Detail: "policy digest is outside bounds"}
	}
	if _, err := hex.DecodeString(req.PolicyDigest); err != nil {
		return &Error{Code: CodeInvalidRequest, Detail: "policy digest is not hexadecimal"}
	}
	if req.TurnTimeout <= 0 || req.TurnTimeout > MaxTurnTimeout || req.MaxPatchBytes <= 0 || req.MaxPatchBytes > MaxPatchBytesLimit {
		return &Error{Code: CodeInvalidRequest, Detail: "session policy bounds are outside limits"}
	}
	bindings := []string{req.Policy, req.Repository, req.Workspace, req.ForkName, req.BaseCommit}
	boundCount := 0
	for _, binding := range bindings {
		if binding != "" {
			boundCount++
		}
		if !validBoundedText(binding, MaxBindingBytes) {
			return &Error{Code: CodeInvalidRequest, Detail: "session binding is outside bounds"}
		}
	}
	if boundCount != 0 && boundCount != len(bindings) {
		return &Error{Code: CodeInvalidRequest, Detail: "session bindings must be all-or-none"}
	}
	if req.PullRequest != nil {
		if req.PullRequest.Number < 1 || req.PullRequest.Ref == "" || req.PullRequest.HeadCommit == "" ||
			!validBoundedText(req.PullRequest.Ref, MaxBindingBytes) ||
			!validBoundedText(req.PullRequest.HeadCommit, MaxBindingBytes) ||
			req.PullRequest.Ref != fmt.Sprintf("refs/pull/%d/head", req.PullRequest.Number) ||
			!validGitObjectID(req.PullRequest.HeadCommit) || boundCount != len(bindings) {
			return &Error{Code: CodeInvalidRequest, Detail: "pull request binding is invalid"}
		}
	}
	seenCompanions := make(map[string]bool, len(req.Companions))
	for _, companion := range req.Companions {
		if companion.Name == "" || seenCompanions[companion.Name] ||
			!validBoundedText(companion.Name, MaxBindingBytes) ||
			!validBoundedText(companion.Repository, MaxBindingBytes) ||
			!validBoundedText(companion.Workspace, MaxBindingBytes) ||
			!validBoundedText(companion.BaseCommit, MaxBindingBytes) ||
			companion.Repository == "" || companion.Workspace == "" || companion.BaseCommit == "" {
			return &Error{Code: CodeInvalidRequest, Detail: "companion repository binding is invalid"}
		}
		seenCompanions[companion.Name] = true
	}
	return nil
}

func validGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

const MaxBindingBytes = 4096

func validBoundedText(value string, max int) bool {
	return len(value) <= max && utf8.ValidString(value) && strings.IndexByte(value, 0) < 0
}

func normalized(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	sess, err := scanSession(s.db.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 0 || limit > 1000 {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "session list limit is outside bounds"}
	}
	rows, err := s.db.QueryContext(ctx, sessionSelect+" ORDER BY created_at, id LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return sessions, nil
}

// ListSessionsForRecovery returns every durable session for daemon startup reconciliation.
// The public listing stays bounded; startup cannot skip newer sessions merely because old closed
// tombstones filled the first page.
func (s *Store) ListSessionsForRecovery(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, sessionSelect+" ORDER BY created_at, id")
	if err != nil {
		return nil, fmt.Errorf("list sessions for recovery: %w", err)
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan recovery session: %w", err)
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions for recovery: %w", err)
	}
	return sessions, nil
}

// ListInterruptedTurns returns active turns that need runtime cleanup before startup can
// reconcile their durable state. It intentionally does not mutate anything.
func (s *Store) ListInterruptedTurns(ctx context.Context) ([]Turn, error) {
	rows, err := s.db.QueryContext(ctx, turnSelect+` WHERE state IN (?, ?) ORDER BY session_id, ordinal`, string(TurnStarting), string(TurnRunning))
	if err != nil {
		return nil, fmt.Errorf("list interrupted turns: %w", err)
	}
	defer rows.Close()
	var turns []Turn
	for rows.Next() {
		turn, err := scanTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("scan interrupted turn: %w", err)
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read interrupted turns: %w", err)
	}
	return turns, nil
}

const sessionSelect = `SELECT id, external_ref, target, policy, policy_digest, project_env, project_mcp, repository_read_only, repository, workspace, fork_name,
	   base_commit, companions, pull_request_number, pull_request_ref, pull_request_head_commit,
	   native_session_id, turn_timeout, max_patch_bytes, revision, state, activity,
	   max_turns, max_queued_turns, max_queued_bytes, turns_used, queued_turn_count,
	   queued_prompt_bytes, active_turn_id, last_event_sequence, created_at, updated_at
FROM sessions`

type rowScanner interface{ Scan(...any) error }

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	var state, activity, active string
	var companions string
	var pullRequestNumber int
	var pullRequestRef, pullRequestHead string
	var turnTimeout int64
	var createdAt, updatedAt int64
	if err := row.Scan(&sess.ID, &sess.ExternalRef, &sess.Target, &sess.Policy, &sess.PolicyDigest,
		&sess.ProjectEnv, &sess.ProjectMCP, &sess.RepositoryReadOnly, &sess.Repository, &sess.Workspace, &sess.ForkName, &sess.BaseCommit, &companions,
		&pullRequestNumber, &pullRequestRef, &pullRequestHead, &sess.NativeSessionID,
		&turnTimeout, &sess.MaxPatchBytes, &sess.Revision, &state, &activity, &sess.MaxTurns,
		&sess.MaxQueuedTurns, &sess.MaxQueuedBytes, &sess.TurnsUsed, &sess.QueuedTurnCount,
		&sess.QueuedPromptBytes, &active,
		&sess.LastEventSequence, &createdAt, &updatedAt); err != nil {
		return Session{}, err
	}
	if err := json.Unmarshal([]byte(companions), &sess.Companions); err != nil {
		return Session{}, fmt.Errorf("decode companion repositories: %w", err)
	}
	if pullRequestNumber > 0 {
		sess.PullRequest = &PullRequestBinding{Number: pullRequestNumber, Ref: pullRequestRef, HeadCommit: pullRequestHead}
	}
	sess.State = SessionState(state)
	sess.Activity = ActivityState(activity)
	sess.TurnTimeout = time.Duration(turnTimeout)
	sess.ActiveTurnID = active
	sess.CreatedAt = fromUnixNano(createdAt)
	sess.UpdatedAt = fromUnixNano(updatedAt)
	return sess, nil
}

func clonePullRequestBinding(value *PullRequestBinding) *PullRequestBinding {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func pullRequestNumber(value *PullRequestBinding) int {
	if value == nil {
		return 0
	}
	return value.Number
}

func pullRequestRef(value *PullRequestBinding) string {
	if value == nil {
		return ""
	}
	return value.Ref
}

func pullRequestHead(value *PullRequestBinding) string {
	if value == nil {
		return ""
	}
	return value.HeadCommit
}

func (s *Store) SubmitTurn(ctx context.Context, key string, req SubmitTurnRequest) (Turn, error) {
	hash, err := CanonicalRequestHash(req)
	if err != nil {
		return Turn{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("begin turn admission: %w", err)
	}
	defer tx.Rollback()
	op, replay, err := s.reserveOperationTx(ctx, tx, "SubmitTurn", key, hash)
	if err != nil {
		return Turn{}, err
	}
	if replay {
		if err := tx.Commit(); err != nil {
			return Turn{}, fmt.Errorf("commit operation replay: %w", err)
		}
		return s.replayTurn(op)
	}
	if err := validateSubmitRequest(req); err != nil {
		return Turn{}, s.failAndCommit(tx, op.ID, err)
	}
	var sess Session
	if sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID)); errors.Is(err, sql.ErrNoRows) {
		return Turn{}, s.failAndCommit(tx, op.ID, ErrSessionNotFound)
	} else if err != nil {
		return Turn{}, fmt.Errorf("read session for turn: %w", err)
	}
	if err := validateRevision(sess, req.ExpectedRevision); err != nil {
		return Turn{}, s.failAndCommit(tx, op.ID, err)
	}
	if sess.State != SessionOpen {
		return Turn{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidSessionState, Detail: "session does not accept turns"})
	}
	if sess.TurnsUsed >= sess.MaxTurns {
		return Turn{}, s.failAndCommit(tx, op.ID, ErrBudgetExhausted)
	}
	promptBytes := len(req.Prompt)
	if sess.QueuedTurnCount >= sess.MaxQueuedTurns || sess.QueuedPromptBytes > sess.MaxQueuedBytes-promptBytes {
		return Turn{}, s.failAndCommit(tx, op.ID, ErrQueueFull)
	}
	now := s.now()
	var ordinal int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(ordinal), 0) + 1 FROM turns WHERE session_id = ?", req.SessionID).Scan(&ordinal); err != nil {
		return Turn{}, fmt.Errorf("reserve turn ordinal: %w", err)
	}
	turn := Turn{
		ID:             s.id("turn"),
		SessionID:      req.SessionID,
		Ordinal:        ordinal,
		IdempotencyKey: key,
		RequestHash:    hash,
		State:          TurnQueued,
		SendState:      SendStateNone,
		Prompt:         req.Prompt,
		QueuedAt:       now,
		MinTargetIndex: req.MinTargetIndex,
		RewindTarget:   req.RewindTarget,
		OutputContract: cloneOutputContract(req.OutputContract),
	}
	outputSchema := []byte{}
	var outputSchemaSHA256 string
	var outputSemanticValidation bool
	if turn.OutputContract != nil {
		outputSchema = turn.OutputContract.JSONSchema
		outputSchemaSHA256 = turn.OutputContract.SHA256
		outputSemanticValidation = turn.OutputContract.RequireSemanticValidation
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO turns (id, session_id, ordinal, idempotency_key, request_hash, state, send_state, prompt, queued_at, min_target_index, rewind_target, output_schema, output_schema_sha256, output_semantic_validation)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, turn.ID, turn.SessionID, turn.Ordinal, turn.IdempotencyKey,
		turn.RequestHash, string(turn.State), string(turn.SendState), turn.Prompt, now.UnixNano(),
		turn.MinTargetIndex, turn.RewindTarget, outputSchema, outputSchemaSHA256, outputSemanticValidation); err != nil {
		return Turn{}, fmt.Errorf("insert turn: %w", err)
	}
	for ordinal, artifact := range req.Artifacts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO turn_artifacts (turn_id, ordinal, name, media_type, sha256, data)
			VALUES (?, ?, ?, ?, ?, ?)`,
			turn.ID, ordinal, artifact.Name, artifact.MediaType, artifact.SHA256, artifact.Data,
		); err != nil {
			return Turn{}, fmt.Errorf("insert turn artifact: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET next_ordinal = next_ordinal + 1, queued_turn_count = queued_turn_count + 1,
		queued_prompt_bytes = queued_prompt_bytes + ?, updated_at = ? WHERE id = ?`, promptBytes,
		now.UnixNano(), sess.ID); err != nil {
		return Turn{}, fmt.Errorf("reserve turn queue: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, turn.SessionID, turn.ID, EventTurnQueued, 1,
		mustJSON(map[string]any{"ordinal": turn.Ordinal})); err != nil {
		return Turn{}, fmt.Errorf("append turn.queued: %w", err)
	}
	result, err := json.Marshal(turn)
	if err != nil {
		return Turn{}, fmt.Errorf("encode turn result: %w", err)
	}
	if err := s.completeOperationTx(ctx, tx, op.ID, "turn", turn.ID, result); err != nil {
		return Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit turn admission: %w", err)
	}
	return turn, nil
}

func validateSubmitRequest(req SubmitTurnRequest) error {
	if req.SessionID == "" || len(req.SessionID) > MaxIDBytes || strings.IndexByte(req.SessionID, 0) >= 0 || req.ExpectedRevision <= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "invalid session or revision"}
	}
	if req.Prompt == "" || len(req.Prompt) > MaxPromptBytes || !utf8.ValidString(req.Prompt) || strings.IndexByte(req.Prompt, 0) >= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "prompt must be bounded UTF-8 text"}
	}
	// The store owns only the shape of the index. Whether the rung exists is a
	// question about the session's policy ladder, which this layer never sees;
	// the service checks it there, where the answer can name the ladder's size.
	if req.MinTargetIndex < 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "min_target_index must not be negative"}
	}
	if req.RewindTarget && req.MinTargetIndex > 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "rewind_target cannot be combined with min_target_index"}
	}
	if _, err := CompileOutputContract(req.OutputContract); err != nil {
		return err
	}
	if len(req.Artifacts) > MaxTurnArtifacts {
		return &Error{Code: CodeInvalidRequest, Detail: "turn has too many artifacts"}
	}
	total := 0
	for _, artifact := range req.Artifacts {
		if err := validateInputArtifact(artifact); err != nil {
			return err
		}
		if total > MaxTurnArtifactBytes-len(artifact.Data) {
			return &Error{Code: CodeInvalidRequest, Detail: "turn artifact content exceeds its bound"}
		}
		total += len(artifact.Data)
	}
	return nil
}

func validateInputArtifact(artifact InputArtifact) error {
	if artifact.Name == "" || len(artifact.Name) > MaxArtifactNameBytes ||
		filepath.Base(artifact.Name) != artifact.Name || artifact.Name == "." ||
		!utf8.ValidString(artifact.Name) || strings.IndexByte(artifact.Name, 0) >= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "artifact name is invalid"}
	}
	for _, r := range artifact.Name {
		if unicode.IsControl(r) {
			return &Error{Code: CodeInvalidRequest, Detail: "artifact name is invalid"}
		}
	}
	switch artifact.MediaType {
	case "image/png", "image/jpeg", "image/webp", "image/gif",
		"text/plain", "text/markdown", "text/csv", "application/json",
		"application/yaml", "application/x-yaml", "application/pdf":
	default:
		return &Error{Code: CodeInvalidRequest, Detail: "artifact media type is unsupported"}
	}
	if len(artifact.Data) == 0 || len(artifact.Data) > MaxArtifactBytes {
		return &Error{Code: CodeInvalidRequest, Detail: "artifact content exceeds its bound"}
	}
	if !artifactMediaMatches(artifact.MediaType, artifact.Data) {
		return &Error{Code: CodeInvalidRequest, Detail: "artifact content does not match its media type"}
	}
	digest := sha256.Sum256(artifact.Data)
	if artifact.SHA256 != hex.EncodeToString(digest[:]) {
		return &Error{Code: CodeInvalidRequest, Detail: "artifact digest does not match its content"}
	}
	return nil
}

func validateOutputArtifact(artifact OutputArtifact) error {
	if artifact.ID == "" || !validBoundedText(artifact.ID, MaxIDBytes) {
		return &Error{Code: CodeInvalidRequest, Detail: "output artifact id is invalid"}
	}
	input := InputArtifact{Name: artifact.Name, MediaType: artifact.MediaType, SHA256: artifact.SHA256, Data: artifact.Data}
	if err := validateInputArtifact(input); err != nil {
		return err
	}
	if artifact.MediaType != "image/png" && artifact.MediaType != "image/jpeg" &&
		artifact.MediaType != "image/webp" && artifact.MediaType != "image/gif" {
		return &Error{Code: CodeInvalidRequest, Detail: "output artifact media type is unsupported"}
	}
	if artifact.Bytes != int64(len(artifact.Data)) {
		return &Error{Code: CodeInvalidRequest, Detail: "output artifact byte count does not match its content"}
	}
	return nil
}

func artifactMediaMatches(mediaType string, data []byte) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif":
		return http.DetectContentType(data) == mediaType
	case "image/webp":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	case "application/pdf":
		return len(data) >= 5 && string(data[:5]) == "%PDF-"
	default:
		return utf8.Valid(data) && strings.IndexByte(string(data), 0) < 0
	}
}

func validateRevision(sess Session, expected int64) error {
	if expected != sess.Revision {
		return &Error{Code: CodeRevisionConflict, Detail: fmt.Sprintf("expected revision %d, current revision %d", expected, sess.Revision)}
	}
	return nil
}

func (s *Store) replayTurn(op Operation) (Turn, error) {
	if op.State == OperationFailed {
		return Turn{}, operationError(op)
	}
	if op.State != OperationSucceeded {
		return Turn{}, ErrOperationUncertain
	}
	var turn Turn
	if err := json.Unmarshal(op.Result, &turn); err != nil {
		return Turn{}, fmt.Errorf("decode turn operation result: %w", err)
	}
	if turn.ID == "" {
		return Turn{}, errors.New("decode turn operation result: missing turn id")
	}
	return turn, nil
}

func (s *Store) LeaseNextTurn(ctx context.Context, sessionID string) (Turn, bool, error) {
	if sessionID == "" {
		return Turn{}, false, &Error{Code: CodeInvalidRequest, Detail: "session id is required"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, false, fmt.Errorf("begin turn lease: %w", err)
	}
	defer tx.Rollback()
	var sess Session
	if sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", sessionID)); errors.Is(err, sql.ErrNoRows) {
		return Turn{}, false, ErrSessionNotFound
	} else if err != nil {
		return Turn{}, false, fmt.Errorf("read session for lease: %w", err)
	}
	if sess.ActiveTurnID != "" || sess.Activity != ActivityParked {
		return Turn{}, false, nil
	}
	if sess.State == SessionClosed || sess.State == SessionDiscarded {
		return Turn{}, false, nil
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+` WHERE session_id = ? AND state = ? ORDER BY ordinal LIMIT 1`, sessionID, string(TurnQueued)))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, false, nil
	}
	if err != nil {
		return Turn{}, false, fmt.Errorf("find queued turn: %w", err)
	}
	turn.Artifacts, err = loadTurnArtifacts(ctx, tx, turn.ID)
	if err != nil {
		return Turn{}, false, err
	}
	now := s.now()
	nextState, nextSend, nextActivity := TurnStarting, SendStateNone, ActivityStarting
	if turn.OutputContract != nil && turn.OutputContract.RequireSemanticValidation && turn.ValidationAttempt > 0 {
		nextState, nextSend, nextActivity = TurnRunning, SendStateSent, ActivityRunning
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, send_state = ?, started_at = ? WHERE id = ?`, string(nextState), string(nextSend), now.UnixNano(), turn.ID); err != nil {
		return Turn{}, false, fmt.Errorf("lease turn: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_turn_id = ?, activity = ?, queued_turn_count = queued_turn_count - 1, queued_prompt_bytes = queued_prompt_bytes - ?, updated_at = ? WHERE id = ?`, turn.ID, string(nextActivity), len(turn.Prompt), now.UnixNano(), sessionID); err != nil {
		return Turn{}, false, fmt.Errorf("mark active turn: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, sessionID, turn.ID, EventTurnStarted, 1, mustJSON(map[string]any{"ordinal": turn.Ordinal})); err != nil {
		return Turn{}, false, fmt.Errorf("append turn.started: %w", err)
	}
	turn.State = nextState
	turn.SendState = nextSend
	turn.StartedAt = now
	if err := tx.Commit(); err != nil {
		return Turn{}, false, fmt.Errorf("commit turn lease: %w", err)
	}
	return turn, true, nil
}

func (s *Store) MarkTurnSendIntent(ctx context.Context, sessionID, turnID string) (Turn, error) {
	return s.markTurnSendState(ctx, sessionID, turnID, SendStateIntent)
}

func (s *Store) MarkTurnSent(ctx context.Context, sessionID, turnID string) (Turn, error) {
	return s.markTurnSendState(ctx, sessionID, turnID, SendStateSent)
}

func (s *Store) markTurnSendState(ctx context.Context, sessionID, turnID string, next SendState) (Turn, error) {
	if sessionID == "" || turnID == "" || !validBoundedText(sessionID, MaxIDBytes) || !validBoundedText(turnID, MaxIDBytes) {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "session and turn are required"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("begin turn send checkpoint: %w", err)
	}
	defer tx.Rollback()
	var sess Session
	if sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", sessionID)); errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrSessionNotFound
	} else if err != nil {
		return Turn{}, fmt.Errorf("read session for turn send checkpoint: %w", err)
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", sessionID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read turn for send checkpoint: %w", err)
	}
	if sess.ActiveTurnID != turn.ID || sess.Activity != ActivityStarting || turn.State != TurnStarting {
		return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn is not the active starting turn"}
	}
	want := SendStateNone
	if next == SendStateSent {
		want = SendStateIntent
	}
	if turn.SendState != want {
		return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn send checkpoint is out of order"}
	}
	now := s.now()
	if next == SendStateSent {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, send_state = ? WHERE id = ?`, string(TurnRunning), string(next), turn.ID); err != nil {
			return Turn{}, fmt.Errorf("mark turn sent: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET activity = ?, updated_at = ? WHERE id = ?`, string(ActivityRunning), now.UnixNano(), sessionID); err != nil {
			return Turn{}, fmt.Errorf("mark session running: %w", err)
		}
		turn.State = TurnRunning
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET send_state = ? WHERE id = ?`, string(next), turn.ID); err != nil {
			return Turn{}, fmt.Errorf("mark turn send intent: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now.UnixNano(), sessionID); err != nil {
			return Turn{}, fmt.Errorf("record turn send intent: %w", err)
		}
	}
	turn.SendState = next
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit turn send checkpoint: %w", err)
	}
	return turn, nil
}

func (s *Store) BindNativeSession(ctx context.Context, sessionID, nativeID string) (Session, error) {
	if sessionID == "" || nativeID == "" || !validBoundedText(sessionID, MaxIDBytes) || !validBoundedText(nativeID, MaxIDBytes) {
		return Session{}, &Error{Code: CodeInvalidRequest, Detail: "session and native session ids are required"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin native session binding: %w", err)
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("read session for native binding: %w", err)
	}
	if sess.NativeSessionID != "" && sess.NativeSessionID != nativeID {
		return Session{}, &Error{Code: CodeNativeSessionConflict, Detail: "session is already bound to another native session"}
	}
	if sess.NativeSessionID == "" {
		now := s.now()
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET native_session_id = ?, updated_at = ? WHERE id = ?`, nativeID, now.UnixNano(), sessionID); err != nil {
			return Session{}, fmt.Errorf("bind native session: %w", err)
		}
		sess.NativeSessionID = nativeID
		sess.UpdatedAt = now
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit native session binding: %w", err)
	}
	return sess, nil
}

// RotateTurnTarget moves a session onto another rung of its policy's target ladder — because the
// active rung rate limited the in-flight turn, or because that turn was admitted with an
// escalation floor above it — and rewinds the turn so it can be delivered again. It
// compare-and-swaps on the active target, so a rotation can never double-advance past a rung
// something else already moved off.
//
// The rewind is the reason this is one transaction rather than two. SendState is the delivery
// ledger: `sent` means a prompt reached a provider and may have had effects, which is why
// MarkTurnSendIntent refuses a second delivery. A rate-limited prompt is the one refusal that
// provably did no work — no tools ran, no tokens were produced — so the rotation un-sends it
// back to the state LeaseNextTurn leaves behind (a turn escalated before its first delivery has
// nothing to un-send). Splitting the two would let a crash strand a turn that is rotated but
// still marked delivered, or delivered but still on the limited rung.
//
// resetNativeSession drops the bound native session id. The caller decides, because only it knows
// the target grammar: a rung on the same provider keeps its transcript, but a cross-provider hop
// must clear it — the new provider's adapter cannot load the old one's session, so every later
// session/load would fail against an id that outlived its store.
func (s *Store) RotateTurnTarget(ctx context.Context, sessionID, turnID, from, to string, resetNativeSession bool) (Session, Turn, error) {
	if sessionID == "" || turnID == "" ||
		!validBoundedText(sessionID, MaxIDBytes) || !validBoundedText(turnID, MaxIDBytes) {
		return Session{}, Turn{}, &Error{Code: CodeInvalidRequest, Detail: "session and turn are required"}
	}
	if from == to || !validBoundedText(from, MaxTargetBytes) || !validBoundedText(to, MaxTargetBytes) {
		return Session{}, Turn{}, &Error{Code: CodeInvalidRequest, Detail: "rotation requires two different bounded targets"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Session{}, Turn{}, fmt.Errorf("begin target rotation: %w", err)
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, Turn{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, Turn{}, fmt.Errorf("read session for target rotation: %w", err)
	}
	if sess.Target != from {
		return Session{}, Turn{}, &Error{Code: CodeRevisionConflict, Detail: "session is no longer on the rotating target"}
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", sessionID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Session{}, Turn{}, fmt.Errorf("read turn for target rotation: %w", err)
	}
	if sess.ActiveTurnID != turn.ID || (turn.State != TurnStarting && turn.State != TurnRunning) {
		return Session{}, Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn is not the session's in-flight turn"}
	}
	now := s.now()
	native := sess.NativeSessionID
	if resetNativeSession {
		native = ""
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET target = ?, native_session_id = ?, activity = ?, revision = revision + 1, updated_at = ? WHERE id = ?`,
		to, native, string(ActivityStarting), now.UnixNano(), sessionID); err != nil {
		return Session{}, Turn{}, fmt.Errorf("rotate session target: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, send_state = ? WHERE id = ?`,
		string(TurnStarting), string(SendStateNone), turn.ID); err != nil {
		return Session{}, Turn{}, fmt.Errorf("rewind rotated turn: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, sessionID, turn.ID, EventSessionTargetRotated, 1,
		mustJSON(map[string]any{
			"from": from, "to": to, "native_session_reset": resetNativeSession,
		})); err != nil {
		return Session{}, Turn{}, fmt.Errorf("append session.target_rotated: %w", err)
	}
	sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", sessionID))
	if err != nil {
		return Session{}, Turn{}, fmt.Errorf("read rotated session: %w", err)
	}
	turn.State, turn.SendState = TurnStarting, SendStateNone
	if err := tx.Commit(); err != nil {
		return Session{}, Turn{}, fmt.Errorf("commit target rotation: %w", err)
	}
	return sess, turn, nil
}

func (s *Store) ReconcileInterruptedTurns(ctx context.Context) ([]Turn, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin interrupted turn reconciliation: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, turnSelect+` WHERE state IN (?, ?) ORDER BY session_id, ordinal`, string(TurnStarting), string(TurnRunning))
	if err != nil {
		return nil, fmt.Errorf("find interrupted turns: %w", err)
	}
	var turns []Turn
	var sessionIDs []string
	seenSessions := make(map[string]bool)
	for rows.Next() {
		turn, scanErr := scanTurn(rows)
		if scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan interrupted turn: %w", scanErr)
		}
		turns = append(turns, turn)
		if !seenSessions[turn.SessionID] {
			seenSessions[turn.SessionID] = true
			sessionIDs = append(sessionIDs, turn.SessionID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read interrupted turns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close interrupted turn rows: %w", err)
	}
	now := s.now()
	const detail = "daemon restart interrupted active turn"
	requeuedCounts := make(map[string]int)
	requeuedBytes := make(map[string]int)
	for i := range turns {
		turn := &turns[i]
		if turn.SendState == SendStateNone {
			if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, started_at = NULL, finished_at = NULL, stop_reason = '', error_code = '', error_detail = '' WHERE id = ?`, string(TurnQueued), turn.ID); err != nil {
				return nil, fmt.Errorf("requeue interrupted turn: %w", err)
			}
			turn.State = TurnQueued
			turn.StartedAt = time.Time{}
			turn.FinishedAt = time.Time{}
			turn.StopReason = ""
			turn.ErrorCode = ""
			turn.ErrorDetail = ""
			requeuedCounts[turn.SessionID]++
			requeuedBytes[turn.SessionID] += len(turn.Prompt)
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, finished_at = ?, stop_reason = ?, error_code = ?, error_detail = ? WHERE id = ?`, string(TurnInterrupted), now.UnixNano(), string(StopInterrupted), string(CodeTurnInterrupted), detail, turn.ID); err != nil {
			return nil, fmt.Errorf("interrupt turn: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, turn.SessionID, turn.ID, EventTurnInterrupted, 1, mustJSON(map[string]any{
			"ordinal": turn.Ordinal, "send_state": string(turn.SendState), "error_code": string(CodeTurnInterrupted),
		})); err != nil {
			return nil, fmt.Errorf("append turn.interrupted: %w", err)
		}
		if err := deleteTurnArtifacts(ctx, tx, turn.ID); err != nil {
			return nil, err
		}
		turn.State = TurnInterrupted
		turn.FinishedAt = now
		turn.StopReason = StopInterrupted
		turn.ErrorCode = CodeTurnInterrupted
		turn.ErrorDetail = detail
	}
	for _, sessionID := range sessionIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_turn_id = '', activity = ?, queued_turn_count = queued_turn_count + ?, queued_prompt_bytes = queued_prompt_bytes + ?, updated_at = ? WHERE id = ?`, string(ActivityParked), requeuedCounts[sessionID], requeuedBytes[sessionID], now.UnixNano(), sessionID); err != nil {
			return nil, fmt.Errorf("park interrupted session: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, sessionID, "", EventSessionParked, 1, mustJSON(map[string]any{"reason": string(StopInterrupted)})); err != nil {
			return nil, fmt.Errorf("append session.parked: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit interrupted turn reconciliation: %w", err)
	}
	return turns, nil
}

const turnSelect = `SELECT id, session_id, ordinal, idempotency_key, request_hash, state, send_state,
	   prompt, queued_at, started_at, finished_at, stop_reason, assistant_message, error_code, error_detail,
	   usage_input_tokens, usage_cached_input_tokens, usage_output_tokens, usage_reasoning_tokens,
	   usage_cost_usd, usage_cost_recorded, min_target_index, rewind_target,
	   output_schema, output_schema_sha256, output_semantic_validation,
	   candidate_message, candidate_sha256, validation_attempt, validation_error, validation_receipt
FROM turns`

func (s *Store) GetTurn(ctx context.Context, sessionID, turnID string) (Turn, error) {
	turn, err := scanTurn(s.db.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", sessionID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("get turn: %w", err)
	}
	turn.OutputArtifacts, err = readOutputArtifactMetadata(ctx, s.db, turn.ID)
	if err != nil {
		return Turn{}, err
	}
	return turn, nil
}

func (s *Store) ListTurns(ctx context.Context, sessionID string, afterOrdinal int64, limit int) ([]Turn, error) {
	if sessionID == "" || !validBoundedText(sessionID, MaxIDBytes) || afterOrdinal < 0 {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "invalid turn cursor"}
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 0 || limit > 1000 {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "turn list limit is outside bounds"}
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", sessionID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	} else if err != nil {
		return nil, fmt.Errorf("check turn session: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, turnSelect+` WHERE session_id = ? AND ordinal > ? ORDER BY ordinal LIMIT ?`, sessionID, afterOrdinal, limit)
	if err != nil {
		return nil, fmt.Errorf("list turns: %w", err)
	}
	var turns []Turn
	for rows.Next() {
		turn, err := scanTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("scan listed turn: %w", err)
		}
		turns = append(turns, turn)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list turns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close listed turns: %w", err)
	}
	for i := range turns {
		turns[i].OutputArtifacts, err = readOutputArtifactMetadata(ctx, s.db, turns[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return turns, nil
}

func (s *Store) GetOutputArtifact(ctx context.Context, sessionID, turnID, artifactID string) (OutputArtifact, error) {
	if !validBoundedText(sessionID, MaxIDBytes) || !validBoundedText(turnID, MaxIDBytes) || !validBoundedText(artifactID, MaxIDBytes) {
		return OutputArtifact{}, &Error{Code: CodeInvalidRequest, Detail: "invalid output artifact identity"}
	}
	var artifact OutputArtifact
	err := s.db.QueryRowContext(ctx, `SELECT a.id, a.name, a.media_type, a.sha256, length(a.data), a.data
		FROM turn_output_artifacts a JOIN turns t ON t.id = a.turn_id
		WHERE t.session_id = ? AND t.id = ? AND a.id = ?`, sessionID, turnID, artifactID).
		Scan(&artifact.ID, &artifact.Name, &artifact.MediaType, &artifact.SHA256, &artifact.Bytes, &artifact.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return OutputArtifact{}, ErrTurnNotFound
	}
	if err != nil {
		return OutputArtifact{}, fmt.Errorf("get output artifact: %w", err)
	}
	return artifact, nil
}

type queryContexter interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readOutputArtifactMetadata(ctx context.Context, db queryContexter, turnID string) ([]OutputArtifact, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, media_type, sha256, length(data)
		FROM turn_output_artifacts WHERE turn_id = ? ORDER BY ordinal`, turnID)
	if err != nil {
		return nil, fmt.Errorf("read output artifacts: %w", err)
	}
	defer rows.Close()
	var artifacts []OutputArtifact
	for rows.Next() {
		var artifact OutputArtifact
		if err := rows.Scan(&artifact.ID, &artifact.Name, &artifact.MediaType, &artifact.SHA256, &artifact.Bytes); err != nil {
			return nil, fmt.Errorf("scan output artifact: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read output artifacts: %w", err)
	}
	return artifacts, nil
}

func scanTurn(row rowScanner) (Turn, error) {
	var turn Turn
	var state, sendState, stopReason, errorCode string
	var queuedAt, startedAt, finishedAt sql.NullInt64
	var outputSchema []byte
	var outputSchemaSHA256 string
	var outputSemanticValidation bool
	var candidateMessage, candidateSHA256, validationError, validationReceipt string
	if err := row.Scan(&turn.ID, &turn.SessionID, &turn.Ordinal, &turn.IdempotencyKey,
		&turn.RequestHash, &state, &sendState, &turn.Prompt, &queuedAt, &startedAt, &finishedAt,
		&stopReason, &turn.AssistantMessage, &errorCode, &turn.ErrorDetail,
		&turn.Usage.InputTokens, &turn.Usage.CachedInputTokens,
		&turn.Usage.OutputTokens, &turn.Usage.ReasoningTokens,
		&turn.Usage.CostUSD, &turn.Usage.CostRecorded, &turn.MinTargetIndex,
		&turn.RewindTarget, &outputSchema, &outputSchemaSHA256, &outputSemanticValidation,
		&candidateMessage, &candidateSHA256, &turn.ValidationAttempt,
		&validationError, &validationReceipt); err != nil {
		return Turn{}, err
	}
	if len(outputSchema) > 0 || outputSchemaSHA256 != "" {
		turn.OutputContract = &OutputContract{
			JSONSchema:                append(json.RawMessage(nil), outputSchema...),
			SHA256:                    outputSchemaSHA256,
			RequireSemanticValidation: outputSemanticValidation,
		}
	}
	if candidateSHA256 != "" {
		turn.CandidateSHA256 = candidateSHA256
		if TurnState(state) == TurnAwaitingValidation {
			turn.Candidate = &TurnCandidate{Message: candidateMessage, SHA256: candidateSHA256, Attempt: turn.ValidationAttempt}
		}
	}
	turn.ValidationError = validationError
	turn.ValidationReceipt = validationReceipt
	turn.State = TurnState(state)
	turn.SendState = SendState(sendState)
	turn.StopReason = StopReason(stopReason)
	turn.ErrorCode = ErrorCode(errorCode)
	if queuedAt.Valid {
		turn.QueuedAt = fromUnixNano(queuedAt.Int64)
	}
	if startedAt.Valid {
		turn.StartedAt = fromUnixNano(startedAt.Int64)
	}
	if finishedAt.Valid {
		turn.FinishedAt = fromUnixNano(finishedAt.Int64)
	}
	return turn, nil
}

func (s *Store) CancelTurn(ctx context.Context, key string, req CancelTurnRequest) (Turn, error) {
	hash, err := CanonicalRequestHash(req)
	if err != nil {
		return Turn{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("begin turn cancellation: %w", err)
	}
	defer tx.Rollback()
	op, replay, err := s.reserveOperationTx(ctx, tx, "CancelTurn", key, hash)
	if err != nil {
		return Turn{}, err
	}
	if replay && op.State != OperationReserved && op.State != OperationRunning {
		if err := tx.Commit(); err != nil {
			return Turn{}, fmt.Errorf("commit operation replay: %w", err)
		}
		return s.replayTurn(op)
	}
	if req.SessionID == "" || req.TurnID == "" || req.ExpectedRevision <= 0 {
		return Turn{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidRequest, Detail: "session, turn, and revision are required"})
	}
	var sess Session
	if sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID)); errors.Is(err, sql.ErrNoRows) {
		return Turn{}, s.failAndCommit(tx, op.ID, ErrSessionNotFound)
	} else if err != nil {
		return Turn{}, fmt.Errorf("read session for cancellation: %w", err)
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", req.SessionID, req.TurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, s.failAndCommit(tx, op.ID, ErrTurnNotFound)
	} else if err != nil {
		return Turn{}, fmt.Errorf("read turn for cancellation: %w", err)
	}
	if isTerminal(turn.State) {
		if replay && (op.State == OperationReserved || op.State == OperationRunning) {
			result, err := json.Marshal(turn)
			if err != nil {
				return Turn{}, fmt.Errorf("encode observed terminal turn: %w", err)
			}
			if err := s.completeOperationTx(ctx, tx, op.ID, "turn", turn.ID, result); err != nil {
				return Turn{}, err
			}
			if err := tx.Commit(); err != nil {
				return Turn{}, fmt.Errorf("commit observed terminal cancellation: %w", err)
			}
			return turn, nil
		}
		return Turn{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeTurnNotRunnable, Detail: "turn is already terminal"})
	}
	if err := validateRevision(sess, req.ExpectedRevision); err != nil {
		return Turn{}, s.failAndCommit(tx, op.ID, err)
	}
	previousState := turn.State
	now := s.now()
	turn.State = TurnCancelled
	turn.FinishedAt = now
	turn.StopReason = StopCancelled
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, finished_at = ?, stop_reason = ? WHERE id = ?`, string(turn.State), now.UnixNano(), string(turn.StopReason), turn.ID); err != nil {
		return Turn{}, fmt.Errorf("cancel turn: %w", err)
	}
	if err := deleteTurnArtifacts(ctx, tx, turn.ID); err != nil {
		return Turn{}, err
	}
	if previousState == TurnQueued {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET queued_turn_count = queued_turn_count - 1, queued_prompt_bytes = queued_prompt_bytes - ?, revision = revision + 1, updated_at = ? WHERE id = ?`, len(turn.Prompt), now.UnixNano(), sess.ID); err != nil {
			return Turn{}, fmt.Errorf("release cancelled queue slot: %w", err)
		}
	} else {
		if sess.ActiveTurnID == turn.ID {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_turn_id = '', activity = ?, revision = revision + 1, updated_at = ? WHERE id = ?`, string(ActivityParked), now.UnixNano(), sess.ID); err != nil {
				return Turn{}, fmt.Errorf("park cancelled session: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE sessions SET revision = revision + 1, updated_at = ? WHERE id = ?`, now.UnixNano(), sess.ID); err != nil {
			return Turn{}, fmt.Errorf("advance cancelled session revision: %w", err)
		}
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, EventTurnCancelled, 1, mustJSON(map[string]any{"stop_reason": string(turn.StopReason)})); err != nil {
		return Turn{}, fmt.Errorf("append turn.cancelled: %w", err)
	}
	result, err := json.Marshal(turn)
	if err != nil {
		return Turn{}, fmt.Errorf("encode cancelled turn: %w", err)
	}
	if err := s.completeOperationTx(ctx, tx, op.ID, "turn", turn.ID, result); err != nil {
		return Turn{}, err
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit turn cancellation: %w", err)
	}
	return turn, nil
}

func isTerminal(state TurnState) bool {
	switch state {
	case TurnCompleted, TurnFailed, TurnCancelled, TurnInterrupted, TurnBudgetExhausted:
		return true
	default:
		return false
	}
}

// StageTurnCandidate durably separates a schema-valid model result from a
// completed assistant message. Callers that own semantic rules can inspect the
// candidate without any consumer mistaking it for an accepted answer.
func (s *Store) StageTurnCandidate(ctx context.Context, req StageTurnCandidateRequest) (Turn, error) {
	if !validBoundedText(req.SessionID, MaxIDBytes) || !validBoundedText(req.TurnID, MaxIDBytes) ||
		req.Message == "" || len(req.Message) > MaxEventPayloadBytes || !utf8.ValidString(req.Message) ||
		req.Attempt < 1 || req.Attempt > MaxOutputContractAttempts {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "semantic candidate is outside bounds"}
	}
	digest := sha256.Sum256([]byte(req.Message))
	if req.SHA256 != hex.EncodeToString(digest[:]) {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "semantic candidate digest does not match its message"}
	}
	if req.CostRecorded && (req.CumulativeCostUSD < 0 || math.IsNaN(req.CumulativeCostUSD) || math.IsInf(req.CumulativeCostUSD, 0)) {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "turn cost is outside bounds"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("begin semantic candidate staging: %w", err)
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrSessionNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read candidate session: %w", err)
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", req.SessionID, req.TurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read candidate turn: %w", err)
	}
	if turn.OutputContract == nil || !turn.OutputContract.RequireSemanticValidation ||
		turn.State != TurnRunning || turn.SendState != SendStateSent ||
		sess.ActiveTurnID != turn.ID || sess.Activity != ActivityRunning {
		return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn does not await semantic validation"}
	}
	validator, err := CompileOutputContract(turn.OutputContract)
	if err != nil || validator.Validate([]byte(req.Message)) != nil {
		return Turn{}, &Error{Code: CodeOutputContractFailed, Detail: "semantic candidate does not satisfy its durable schema"}
	}
	if err := replaceOutputArtifactsTx(ctx, tx, turn.ID, req.Artifacts); err != nil {
		return Turn{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, candidate_message = ?, candidate_sha256 = ?,
		validation_attempt = ?, validation_error = '', validation_receipt = '',
		usage_input_tokens = ?, usage_cached_input_tokens = ?, usage_output_tokens = ?,
		usage_reasoning_tokens = ?, usage_cost_usd = ?, usage_cost_recorded = ? WHERE id = ?`,
		string(TurnAwaitingValidation), req.Message, req.SHA256, req.Attempt,
		req.Usage.InputTokens, req.Usage.CachedInputTokens, req.Usage.OutputTokens,
		req.Usage.ReasoningTokens, req.CumulativeCostUSD, req.CostRecorded, turn.ID); err != nil {
		return Turn{}, fmt.Errorf("stage semantic candidate: %w", err)
	}
	turn.State = TurnAwaitingValidation
	turn.Candidate = &TurnCandidate{Message: req.Message, SHA256: req.SHA256, Attempt: req.Attempt}
	turn.CandidateSHA256 = req.SHA256
	turn.ValidationAttempt = req.Attempt
	turn.Usage = req.Usage
	turn.Usage.CostUSD, turn.Usage.CostRecorded = req.CumulativeCostUSD, req.CostRecorded
	turn.OutputArtifacts = outputArtifactMetadata(req.Artifacts)
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, EventTurnAwaitingValidation, 1,
		mustJSON(map[string]any{"candidate_sha256": req.SHA256, "attempt": req.Attempt})); err != nil {
		return Turn{}, fmt.Errorf("append turn.awaiting_validation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit semantic candidate: %w", err)
	}
	return turn, nil
}

func replaceOutputArtifactsTx(ctx context.Context, tx *sql.Tx, turnID string, artifacts []OutputArtifact) error {
	if len(artifacts) > MaxTurnArtifacts {
		return &Error{Code: CodeInvalidRequest, Detail: "turn has too many output artifacts"}
	}
	total := 0
	seenIDs := make(map[string]bool, len(artifacts))
	seenDigests := make(map[string]bool, len(artifacts))
	for _, artifact := range artifacts {
		if err := validateOutputArtifact(artifact); err != nil {
			return err
		}
		if seenIDs[artifact.ID] || seenDigests[artifact.SHA256] {
			return &Error{Code: CodeInvalidRequest, Detail: "turn has duplicate output artifacts"}
		}
		seenIDs[artifact.ID], seenDigests[artifact.SHA256] = true, true
		if total > MaxTurnArtifactBytes-len(artifact.Data) {
			return &Error{Code: CodeInvalidRequest, Detail: "turn output artifact content exceeds its bound"}
		}
		total += len(artifact.Data)
	}
	if err := deleteTurnArtifacts(ctx, tx, turnID); err != nil {
		return err
	}
	for i, artifact := range artifacts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO turn_output_artifacts(turn_id, ordinal, id, name, media_type, sha256, data)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, turnID, i, artifact.ID, artifact.Name, artifact.MediaType, artifact.SHA256, artifact.Data); err != nil {
			return fmt.Errorf("store output artifact: %w", err)
		}
	}
	return nil
}

func outputArtifactMetadata(artifacts []OutputArtifact) []OutputArtifact {
	result := make([]OutputArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		artifact.Bytes = int64(len(artifact.Data))
		artifact.Data = nil
		result = append(result, artifact)
	}
	return result
}

// RejectTurnCandidate records caller-owned semantic violations and either
// requeues the same logical turn for repair or fails it at the shared
// three-candidate bound. The rejected bytes never become an assistant message.
func (s *Store) RejectTurnCandidate(ctx context.Context, req RejectTurnCandidateRequest) (Turn, error) {
	if !validBoundedText(req.SessionID, MaxIDBytes) || !validBoundedText(req.TurnID, MaxIDBytes) ||
		len(req.CandidateSHA256) != sha256.Size*2 || len(req.Violations) == 0 || len(req.Violations) > 20 {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "semantic rejection is outside bounds"}
	}
	violations := make([]string, 0, len(req.Violations))
	total := 0
	for _, violation := range req.Violations {
		violation = strings.TrimSpace(strings.ToValidUTF8(violation, "�"))
		if violation == "" || len(violation) > MaxErrorDetailBytes || total > MaxErrorDetailBytes-len(violation)-1 {
			return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "semantic violation is outside bounds"}
		}
		total += len(violation) + 1
		violations = append(violations, violation)
	}
	detail := strings.Join(violations, "\n")
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("begin semantic candidate rejection: %w", err)
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrSessionNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read candidate session for rejection: %w", err)
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", req.SessionID, req.TurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read candidate turn for rejection: %w", err)
	}
	if turn.State == TurnQueued && turn.CandidateSHA256 == req.CandidateSHA256 && turn.ValidationError != "" {
		return turn, nil
	}
	if turn.State != TurnAwaitingValidation || turn.Candidate == nil || sess.ActiveTurnID != turn.ID {
		return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn has no semantic candidate awaiting rejection"}
	}
	if turn.Candidate.SHA256 != req.CandidateSHA256 {
		return Turn{}, &Error{Code: CodeRevisionConflict, Detail: "semantic candidate digest is stale"}
	}
	now := s.now()
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, EventOutputContractRejected, 1, mustJSON(map[string]any{
		"attempt": turn.ValidationAttempt, "candidate_sha256": req.CandidateSHA256,
		"semantic": true, "violations": violations,
	})); err != nil {
		return Turn{}, fmt.Errorf("append semantic output rejection: %w", err)
	}
	if err := deleteTurnArtifacts(ctx, tx, turn.ID); err != nil {
		return Turn{}, err
	}
	turn.Candidate = nil
	turn.ValidationError = detail
	turn.OutputArtifacts = nil
	if turn.ValidationAttempt >= MaxOutputContractAttempts {
		failureDetail := fmt.Sprintf("caller rejected semantic output after %d attempts", turn.ValidationAttempt)
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, finished_at = ?, stop_reason = ?,
			assistant_message = '', candidate_message = '',
			validation_error = ?, error_code = ?, error_detail = ? WHERE id = ?`,
			string(TurnFailed), now.UnixNano(), string(StopError), detail,
			string(CodeOutputContractFailed), failureDetail, turn.ID); err != nil {
			return Turn{}, fmt.Errorf("fail rejected semantic turn: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_turn_id = '', activity = ?,
			turns_used = turns_used + 1, updated_at = ? WHERE id = ?`,
			string(ActivityParked), now.UnixNano(), sess.ID); err != nil {
			return Turn{}, fmt.Errorf("park rejected semantic turn: %w", err)
		}
		turn.State, turn.FinishedAt, turn.StopReason = TurnFailed, now, StopError
		turn.ErrorCode, turn.ErrorDetail = CodeOutputContractFailed, failureDetail
		if _, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, EventTurnFailed, 1,
			mustJSON(map[string]any{"error_code": string(turn.ErrorCode), "detail": failureDetail, "ordinal": turn.Ordinal})); err != nil {
			return Turn{}, fmt.Errorf("append rejected semantic turn failure: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventSessionParked, 1,
			mustJSON(map[string]any{"reason": string(StopError)})); err != nil {
			return Turn{}, fmt.Errorf("append rejected semantic session parked: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, send_state = ?, started_at = NULL,
			candidate_message = '', validation_error = ?, validation_receipt = '' WHERE id = ?`,
			string(TurnQueued), string(SendStateNone), detail, turn.ID); err != nil {
			return Turn{}, fmt.Errorf("requeue rejected semantic turn: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_turn_id = '', activity = ?,
			queued_turn_count = queued_turn_count + 1, queued_prompt_bytes = queued_prompt_bytes + ?, updated_at = ? WHERE id = ?`,
			string(ActivityParked), len(turn.Prompt), now.UnixNano(), sess.ID); err != nil {
			return Turn{}, fmt.Errorf("requeue rejected semantic session: %w", err)
		}
		turn.State, turn.SendState, turn.StartedAt = TurnQueued, SendStateNone, time.Time{}
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit semantic candidate rejection: %w", err)
	}
	return turn, nil
}

func (s *Store) CompleteTurn(ctx context.Context, req CompleteTurnRequest) (Turn, error) {
	if req.SessionID == "" || req.TurnID == "" {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "session and turn are required"}
	}
	if len(req.Message) > MaxEventPayloadBytes || !utf8.ValidString(req.Message) {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "assistant message is outside bounds"}
	}
	if len(req.Artifacts) > MaxTurnArtifacts {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "turn has too many output artifacts"}
	}
	if req.CostRecorded && (req.CumulativeCostUSD < 0 || math.IsNaN(req.CumulativeCostUSD) || math.IsInf(req.CumulativeCostUSD, 0)) {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "turn cost is outside bounds"}
	}
	total := 0
	seenIDs := make(map[string]bool, len(req.Artifacts))
	seenDigests := make(map[string]bool, len(req.Artifacts))
	for _, artifact := range req.Artifacts {
		if err := validateOutputArtifact(artifact); err != nil {
			return Turn{}, err
		}
		if seenIDs[artifact.ID] || seenDigests[artifact.SHA256] {
			return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "turn has duplicate output artifacts"}
		}
		seenIDs[artifact.ID], seenDigests[artifact.SHA256] = true, true
		if total > MaxTurnArtifactBytes-len(artifact.Data) {
			return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "turn output artifact content exceeds its bound"}
		}
		total += len(artifact.Data)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("begin turn completion: %w", err)
	}
	defer tx.Rollback()
	var sess Session
	if sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID)); errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrSessionNotFound
	} else if err != nil {
		return Turn{}, fmt.Errorf("read session for completion: %w", err)
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", req.SessionID, req.TurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	} else if err != nil {
		return Turn{}, fmt.Errorf("read turn for completion: %w", err)
	}
	semanticAcceptance := turn.OutputContract != nil && turn.OutputContract.RequireSemanticValidation
	if semanticAcceptance {
		if turn.State == TurnCompleted && turn.ValidationReceipt != "" {
			digest := sha256.Sum256([]byte(turn.AssistantMessage))
			if req.CandidateSHA256 == hex.EncodeToString(digest[:]) {
				return turn, nil
			}
			return Turn{}, &Error{Code: CodeRevisionConflict, Detail: "semantic candidate digest is stale"}
		}
		if turn.State != TurnAwaitingValidation || turn.Candidate == nil {
			return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn has no semantic candidate awaiting acceptance"}
		}
		if req.CandidateSHA256 == "" || req.CandidateSHA256 != turn.Candidate.SHA256 {
			return Turn{}, &Error{Code: CodeRevisionConflict, Detail: "semantic candidate digest is stale"}
		}
		if req.Message != "" || len(req.Artifacts) != 0 {
			return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "semantic acceptance uses the stored candidate"}
		}
		req.Message = turn.Candidate.Message
		req.Usage = turn.Usage
		req.CumulativeCostUSD, req.CostRecorded = turn.Usage.CostUSD, turn.Usage.CostRecorded
	} else if turn.State != TurnRunning || turn.SendState != SendStateSent || req.CandidateSHA256 != "" {
		return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn is not running after send"}
	}
	if sess.ActiveTurnID != turn.ID || sess.Activity != ActivityRunning {
		return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn is not the session's active turn"}
	}
	now := s.now()
	turn.State = TurnCompleted
	turn.FinishedAt = now
	turn.StopReason = StopEndTurn
	turn.AssistantMessage = req.Message
	if semanticAcceptance {
		turn.ValidationReceipt = s.id("validation")
	}
	turn.Usage = req.Usage
	turn.Usage.CostUSD, turn.Usage.CostRecorded = 0, false
	if req.CostRecorded {
		var previous float64
		var recorded bool
		if err := tx.QueryRowContext(ctx, `SELECT usage_cumulative_cost_usd, usage_cost_recorded FROM sessions WHERE id = ?`, req.SessionID).
			Scan(&previous, &recorded); err != nil {
			return Turn{}, fmt.Errorf("read cumulative session cost: %w", err)
		}
		turn.Usage.CostUSD = req.CumulativeCostUSD
		if recorded && req.CumulativeCostUSD >= previous {
			turn.Usage.CostUSD -= previous
		}
		turn.Usage.CostRecorded = true
	}
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, finished_at = ?, stop_reason = ?,
	    assistant_message = ?, usage_input_tokens = ?, usage_cached_input_tokens = ?,
	    usage_output_tokens = ?, usage_reasoning_tokens = ?, usage_cost_usd = ?,
	    usage_cost_recorded = ?, validation_receipt = ? WHERE id = ?`,
		string(turn.State), now.UnixNano(), string(turn.StopReason), turn.AssistantMessage,
		turn.Usage.InputTokens, turn.Usage.CachedInputTokens,
		turn.Usage.OutputTokens, turn.Usage.ReasoningTokens,
		turn.Usage.CostUSD, turn.Usage.CostRecorded, turn.ValidationReceipt, turn.ID); err != nil {
		return Turn{}, fmt.Errorf("complete turn: %w", err)
	}
	if !semanticAcceptance {
		if err := deleteTurnArtifacts(ctx, tx, turn.ID); err != nil {
			return Turn{}, err
		}
	}
	for i, artifact := range req.Artifacts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO turn_output_artifacts(turn_id, ordinal, id, name, media_type, sha256, data)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, turn.ID, i, artifact.ID, artifact.Name, artifact.MediaType, artifact.SHA256, artifact.Data); err != nil {
			return Turn{}, fmt.Errorf("store output artifact: %w", err)
		}
		artifact.Data = nil
		turn.OutputArtifacts = append(turn.OutputArtifacts, artifact)
	}
	if req.Message != "" {
		if _, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, EventAssistantMessage, 1, mustJSON(map[string]any{"text": req.Message, "final": true})); err != nil {
			return Turn{}, fmt.Errorf("append assistant.message: %w", err)
		}
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, EventTurnCompleted, 1, mustJSON(map[string]any{"stop_reason": string(turn.StopReason)})); err != nil {
		return Turn{}, fmt.Errorf("append turn.completed: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_turn_id = '', activity = ?,
	    turns_used = turns_used + 1,
	    usage_cumulative_cost_usd = CASE WHEN ? THEN ? ELSE usage_cumulative_cost_usd END,
	    usage_cost_recorded = usage_cost_recorded OR ?, updated_at = ? WHERE id = ?`,
		string(ActivityParked), req.CostRecorded, req.CumulativeCostUSD,
		req.CostRecorded, now.UnixNano(), req.SessionID); err != nil {
		return Turn{}, fmt.Errorf("park session after turn: %w", err)
	}
	if sess.TurnsUsed+1 >= sess.MaxTurns {
		if err := s.exhaustQueuedTx(ctx, tx, req.SessionID, now); err != nil {
			return Turn{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, revision = revision + 1 WHERE id = ?`, string(SessionExhausted), req.SessionID); err != nil {
			return Turn{}, fmt.Errorf("mark max-turn budget exhausted: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventSessionStateChanged, 1, mustJSON(map[string]any{"state": string(SessionExhausted)})); err != nil {
			return Turn{}, fmt.Errorf("append max-turn session.state_changed: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventBudgetExhausted, 1, mustJSON(map[string]any{"reason": string(StopBudget)})); err != nil {
			return Turn{}, fmt.Errorf("append max-turn budget.exhausted: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit turn completion: %w", err)
	}
	if semanticAcceptance {
		turn.OutputArtifacts, err = readOutputArtifactMetadata(ctx, s.db, turn.ID)
		if err != nil {
			return Turn{}, err
		}
	}
	return turn, nil
}

func (s *Store) FailTurn(ctx context.Context, req FailTurnRequest) (Turn, error) {
	if req.SessionID == "" || req.TurnID == "" || !validBoundedText(req.SessionID, MaxIDBytes) || !validBoundedText(req.TurnID, MaxIDBytes) {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "session and turn are required"}
	}
	if !validErrorCode(req.ErrorCode) || len(req.ErrorDetail) > MaxErrorDetailBytes || !utf8.ValidString(req.ErrorDetail) {
		return Turn{}, &Error{Code: CodeInvalidRequest, Detail: "turn failure code and detail are outside bounds"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Turn{}, fmt.Errorf("begin turn failure: %w", err)
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrSessionNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read session for turn failure: %w", err)
	}
	turn, err := scanTurn(tx.QueryRowContext(ctx, turnSelect+" WHERE session_id = ? AND id = ?", req.SessionID, req.TurnID))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrTurnNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("read turn for failure: %w", err)
	}
	if sess.ActiveTurnID != turn.ID || (turn.State != TurnStarting && turn.State != TurnRunning) {
		return Turn{}, &Error{Code: CodeTurnNotRunnable, Detail: "turn is not the active starting or running turn"}
	}
	now := s.now()
	turn.State = TurnFailed
	turn.FinishedAt = now
	turn.StopReason = StopError
	turn.ErrorCode = req.ErrorCode
	turn.ErrorDetail = req.ErrorDetail
	if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, finished_at = ?, stop_reason = ?, error_code = ?, error_detail = ? WHERE id = ?`, string(turn.State), now.UnixNano(), string(turn.StopReason), string(turn.ErrorCode), turn.ErrorDetail, turn.ID); err != nil {
		return Turn{}, fmt.Errorf("fail turn: %w", err)
	}
	if err := deleteTurnArtifacts(ctx, tx, turn.ID); err != nil {
		return Turn{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET active_turn_id = '', activity = ?, turns_used = turns_used + 1, updated_at = ? WHERE id = ?`, string(ActivityParked), now.UnixNano(), req.SessionID); err != nil {
		return Turn{}, fmt.Errorf("park failed turn session: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, EventTurnFailed, 1, mustJSON(map[string]any{
		"error_code": string(turn.ErrorCode), "detail": turn.ErrorDetail, "ordinal": turn.Ordinal,
	})); err != nil {
		return Turn{}, fmt.Errorf("append turn.failed: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventSessionParked, 1, mustJSON(map[string]any{"reason": string(turn.StopReason)})); err != nil {
		return Turn{}, fmt.Errorf("append session.parked: %w", err)
	}
	if sess.TurnsUsed+1 >= sess.MaxTurns {
		if err := s.exhaustQueuedTx(ctx, tx, req.SessionID, now); err != nil {
			return Turn{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, revision = revision + 1, updated_at = ? WHERE id = ?`, string(SessionExhausted), now.UnixNano(), req.SessionID); err != nil {
			return Turn{}, fmt.Errorf("exhaust failed-turn budget: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventSessionStateChanged, 1, mustJSON(map[string]any{"state": string(SessionExhausted)})); err != nil {
			return Turn{}, fmt.Errorf("append failed-turn state change: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventBudgetExhausted, 1, mustJSON(map[string]any{"reason": string(StopBudget)})); err != nil {
			return Turn{}, fmt.Errorf("append failed-turn budget exhaustion: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Turn{}, fmt.Errorf("commit turn failure: %w", err)
	}
	return turn, nil
}

func validErrorCode(code ErrorCode) bool {
	return code != "" && validBoundedText(string(code), MaxMethodBytes)
}

func (s *Store) ExhaustBudget(ctx context.Context, key string, req ExhaustBudgetRequest) (Session, error) {
	hash, err := CanonicalRequestHash(req)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin budget exhaustion: %w", err)
	}
	defer tx.Rollback()
	op, replay, err := s.reserveOperationTx(ctx, tx, "ExhaustBudget", key, hash)
	if err != nil {
		return Session{}, err
	}
	if replay {
		if err := tx.Commit(); err != nil {
			return Session{}, fmt.Errorf("commit operation replay: %w", err)
		}
		return s.replaySession(op)
	}
	if req.SessionID == "" || req.ExpectedRevision <= 0 {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidRequest, Detail: "session and revision are required"})
	}
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, s.failAndCommit(tx, op.ID, ErrSessionNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("read session for budget: %w", err)
	}
	if err := validateRevision(sess, req.ExpectedRevision); err != nil {
		return Session{}, s.failAndCommit(tx, op.ID, err)
	}
	if sess.State == SessionClosed || sess.State == SessionDiscarded {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidSessionState, Detail: "terminal session cannot exhaust budget"})
	}
	now := s.now()
	if err := s.exhaustQueuedTx(ctx, tx, req.SessionID, now); err != nil {
		return Session{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, revision = revision + 1, updated_at = ? WHERE id = ?`, string(SessionExhausted), now.UnixNano(), req.SessionID); err != nil {
		return Session{}, fmt.Errorf("exhaust session budget: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventSessionStateChanged, 1, mustJSON(map[string]any{"state": string(SessionExhausted)})); err != nil {
		return Session{}, fmt.Errorf("append session.state_changed: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventBudgetExhausted, 1, mustJSON(map[string]any{"reason": string(StopBudget)})); err != nil {
		return Session{}, fmt.Errorf("append budget.exhausted: %w", err)
	}
	sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if err != nil {
		return Session{}, fmt.Errorf("read exhausted session: %w", err)
	}
	result, err := json.Marshal(sess)
	if err != nil {
		return Session{}, fmt.Errorf("encode exhausted session: %w", err)
	}
	if err := s.completeOperationTx(ctx, tx, op.ID, "session", sess.ID, result); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit budget exhaustion: %w", err)
	}
	return sess, nil
}

func (s *Store) CloseSession(ctx context.Context, key string, req CloseSessionRequest) (Session, error) {
	hash, err := CanonicalRequestHash(req)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin session close: %w", err)
	}
	defer tx.Rollback()
	op, replay, err := s.reserveOperationTx(ctx, tx, "CloseSession", key, hash)
	if err != nil {
		return Session{}, err
	}
	if replay {
		if err := tx.Commit(); err != nil {
			return Session{}, fmt.Errorf("commit close replay: %w", err)
		}
		return s.replaySession(op)
	}
	if req.SessionID == "" || !validBoundedText(req.SessionID, MaxIDBytes) || req.ExpectedRevision <= 0 {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidRequest, Detail: "session and revision are required"})
	}
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, s.failAndCommit(tx, op.ID, ErrSessionNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("read session for close: %w", err)
	}
	if err := validateRevision(sess, req.ExpectedRevision); err != nil {
		return Session{}, s.failAndCommit(tx, op.ID, err)
	}
	if sess.State == SessionClosed || sess.State == SessionDiscarded {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidSessionState, Detail: "session is already terminal"})
	}
	var activeOrQueued int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE session_id = ? AND state IN (?, ?, ?)`, req.SessionID, string(TurnQueued), string(TurnStarting), string(TurnRunning)).Scan(&activeOrQueued); err != nil {
		return Session{}, fmt.Errorf("check session turn queue for close: %w", err)
	}
	if sess.ActiveTurnID != "" || activeOrQueued != 0 {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidSessionState, Detail: "session must have no active or queued turns"})
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, activity = ?, revision = revision + 1, updated_at = ? WHERE id = ?`, string(SessionClosed), string(ActivityParked), now.UnixNano(), req.SessionID); err != nil {
		return Session{}, fmt.Errorf("close session: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventSessionClosed, 1, mustJSON(map[string]any{"state": string(SessionClosed)})); err != nil {
		return Session{}, fmt.Errorf("append session.closed: %w", err)
	}
	sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if err != nil {
		return Session{}, fmt.Errorf("read closed session: %w", err)
	}
	result, err := json.Marshal(sess)
	if err != nil {
		return Session{}, fmt.Errorf("encode closed session: %w", err)
	}
	if err := s.completeOperationTx(ctx, tx, op.ID, "session", sess.ID, result); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit session close: %w", err)
	}
	return sess, nil
}

func (s *Store) ExtendBudget(ctx context.Context, key string, req ExtendBudgetRequest) (Session, error) {
	hash, err := CanonicalRequestHash(req)
	if err != nil {
		return Session{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin budget extension: %w", err)
	}
	defer tx.Rollback()
	op, replay, err := s.reserveOperationTx(ctx, tx, "ExtendBudget", key, hash)
	if err != nil {
		return Session{}, err
	}
	if replay {
		if err := tx.Commit(); err != nil {
			return Session{}, fmt.Errorf("commit budget extension replay: %w", err)
		}
		return s.replaySession(op)
	}
	if req.SessionID == "" || !validBoundedText(req.SessionID, MaxIDBytes) || req.ExpectedRevision <= 0 || req.AdditionalTurns <= 0 || req.AdditionalTurns > MaxTurnsLimit {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidRequest, Detail: "session, revision, and positive budget addition are required"})
	}
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, s.failAndCommit(tx, op.ID, ErrSessionNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("read session for budget extension: %w", err)
	}
	if err := validateRevision(sess, req.ExpectedRevision); err != nil {
		return Session{}, s.failAndCommit(tx, op.ID, err)
	}
	if sess.State == SessionClosed || sess.State == SessionDiscarded {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidSessionState, Detail: "terminal session cannot extend budget"})
	}
	if req.AdditionalTurns > MaxTurnsLimit-sess.MaxTurns {
		return Session{}, s.failAndCommit(tx, op.ID, &Error{Code: CodeInvalidRequest, Detail: "extended budget exceeds the maximum turn limit"})
	}
	newMax := sess.MaxTurns + req.AdditionalTurns
	state := SessionExhausted
	if sess.TurnsUsed < newMax {
		state = SessionOpen
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET max_turns = ?, state = ?, revision = revision + 1, updated_at = ? WHERE id = ?`, newMax, string(state), now.UnixNano(), req.SessionID); err != nil {
		return Session{}, fmt.Errorf("extend session budget: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, req.SessionID, "", EventSessionStateChanged, 1, mustJSON(map[string]any{
		"state": string(state), "max_turns": newMax, "additional_turns": req.AdditionalTurns,
	})); err != nil {
		return Session{}, fmt.Errorf("append budget state change: %w", err)
	}
	sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", req.SessionID))
	if err != nil {
		return Session{}, fmt.Errorf("read extended session: %w", err)
	}
	result, err := json.Marshal(sess)
	if err != nil {
		return Session{}, fmt.Errorf("encode extended session: %w", err)
	}
	if err := s.completeOperationTx(ctx, tx, op.ID, "session", sess.ID, result); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit budget extension: %w", err)
	}
	return sess, nil
}

// MarkSessionDiscarded records the controller tombstone after the workspace removal has been
// proven. It intentionally leaves the session row and its event history available for operation
// replay and audit.
func (s *Store) MarkSessionDiscarded(ctx context.Context, sessionID string) (Session, error) {
	if sessionID == "" || !validBoundedText(sessionID, MaxIDBytes) {
		return Session{}, &Error{Code: CodeInvalidRequest, Detail: "session id is required"}
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin session discard: %w", err)
	}
	defer tx.Rollback()
	sess, err := scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("read session for discard: %w", err)
	}
	if sess.State == SessionDiscarded {
		return sess, nil
	}
	if sess.State != SessionClosed || sess.ActiveTurnID != "" || sess.QueuedTurnCount != 0 {
		return Session{}, &Error{Code: CodeInvalidSessionState, Detail: "session must be closed with no active or queued turns"}
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state = ?, activity = ?, revision = revision + 1, updated_at = ? WHERE id = ?`,
		string(SessionDiscarded), string(ActivityParked), now.UnixNano(), sessionID); err != nil {
		return Session{}, fmt.Errorf("mark session discarded: %w", err)
	}
	if _, err := s.appendEventTx(ctx, tx, sessionID, "", EventWorkspaceDiscarded, 1,
		mustJSON(map[string]any{"state": string(SessionDiscarded)})); err != nil {
		return Session{}, fmt.Errorf("append workspace.discarded: %w", err)
	}
	sess, err = scanSession(tx.QueryRowContext(ctx, sessionSelect+" WHERE id = ?", sessionID))
	if err != nil {
		return Session{}, fmt.Errorf("read discarded session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit session discard: %w", err)
	}
	return sess, nil
}

func (s *Store) exhaustQueuedTx(ctx context.Context, tx *sql.Tx, sessionID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, ordinal FROM turns WHERE session_id = ? AND state = ? ORDER BY ordinal`, sessionID, string(TurnQueued))
	if err != nil {
		return fmt.Errorf("find queued turns for budget: %w", err)
	}
	defer rows.Close()
	type queued struct {
		id      string
		ordinal int64
	}
	var queuedTurns []queued
	for rows.Next() {
		var item queued
		if err := rows.Scan(&item.id, &item.ordinal); err != nil {
			return fmt.Errorf("scan queued turn for budget: %w", err)
		}
		queuedTurns = append(queuedTurns, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read queued turns for budget: %w", err)
	}
	for _, item := range queuedTurns {
		if _, err := tx.ExecContext(ctx, `UPDATE turns SET state = ?, finished_at = ?, stop_reason = ?, error_code = ?, error_detail = ? WHERE id = ?`, string(TurnBudgetExhausted), now.UnixNano(), string(StopBudget), string(CodeBudgetExhausted), "budget exhausted", item.id); err != nil {
			return fmt.Errorf("terminalize queued turn: %w", err)
		}
		if _, err := s.appendEventTx(ctx, tx, sessionID, item.id, EventTurnFailed, 1, mustJSON(map[string]any{"error_code": string(CodeBudgetExhausted), "ordinal": item.ordinal})); err != nil {
			return fmt.Errorf("append queued budget event: %w", err)
		}
		if err := deleteTurnArtifacts(ctx, tx, item.id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET queued_turn_count = 0, queued_prompt_bytes = 0, updated_at = ? WHERE id = ?`, now.UnixNano(), sessionID); err != nil {
		return fmt.Errorf("clear queued budget counters: %w", err)
	}
	return nil
}

func loadTurnArtifacts(ctx context.Context, tx *sql.Tx, turnID string) ([]InputArtifact, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT name, media_type, sha256, data
		FROM turn_artifacts WHERE turn_id = ? ORDER BY ordinal`, turnID)
	if err != nil {
		return nil, fmt.Errorf("read turn artifacts: %w", err)
	}
	defer rows.Close()
	var artifacts []InputArtifact
	for rows.Next() {
		var artifact InputArtifact
		if err := rows.Scan(&artifact.Name, &artifact.MediaType, &artifact.SHA256, &artifact.Data); err != nil {
			return nil, fmt.Errorf("scan turn artifact: %w", err)
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read turn artifacts: %w", err)
	}
	return artifacts, nil
}

func deleteTurnArtifacts(ctx context.Context, tx *sql.Tx, turnID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM turn_artifacts WHERE turn_id = ?`, turnID); err != nil {
		return fmt.Errorf("delete terminal turn artifacts: %w", err)
	}
	return nil
}

func (s *Store) AppendEvent(ctx context.Context, req AppendEventRequest) (Event, error) {
	if err := validateEventRequest(req); err != nil {
		return Event{}, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Event{}, fmt.Errorf("begin event append: %w", err)
	}
	defer tx.Rollback()
	event, err := s.appendEventTx(ctx, tx, req.SessionID, req.TurnID, req.Type, req.Version, req.Payload)
	if err != nil {
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("commit event append: %w", err)
	}
	return event, nil
}

func validateEventRequest(req AppendEventRequest) error {
	if req.SessionID == "" || req.Type == "" || len(req.Type) > MaxMethodBytes || strings.IndexByte(string(req.Type), 0) >= 0 || req.Version <= 0 {
		return &Error{Code: CodeInvalidRequest, Detail: "event session, type, and version are required"}
	}
	if len(req.Payload) == 0 || len(req.Payload) > MaxEventPayloadBytes || !json.Valid(req.Payload) {
		return &Error{Code: CodeEventPayloadTooLarge, Detail: "event payload must be bounded JSON"}
	}
	return nil
}

func (s *Store) appendEventTx(ctx context.Context, tx *sql.Tx, sessionID, turnID string, eventType EventType, version int, payload []byte) (Event, error) {
	if len(payload) == 0 || len(payload) > MaxEventPayloadBytes || !json.Valid(payload) {
		return Event{}, &Error{Code: CodeEventPayloadTooLarge, Detail: "event payload must be bounded JSON"}
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET last_event_sequence = last_event_sequence + 1, updated_at = ? WHERE id = ?`, now.UnixNano(), sessionID)
	if err != nil {
		return Event{}, fmt.Errorf("advance event sequence: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return Event{}, fmt.Errorf("check event sequence: %w", err)
	} else if affected != 1 {
		return Event{}, ErrSessionNotFound
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT last_event_sequence FROM sessions WHERE id = ?", sessionID).Scan(&sequence); err != nil {
		return Event{}, fmt.Errorf("read event sequence: %w", err)
	}
	event := Event{ID: s.id("evt"), SessionID: sessionID, Sequence: sequence, TurnID: turnID, Type: eventType, Version: version, OccurredAt: now, Payload: append([]byte(nil), payload...)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (id, session_id, sequence, turn_id, type, version, occurred_at, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, event.ID, event.SessionID, event.Sequence, event.TurnID, string(event.Type), event.Version, event.OccurredAt.UnixNano(), event.Payload); err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	return event, nil
}

func (s *Store) ListEvents(ctx context.Context, sessionID string, after int64, limit int) ([]Event, error) {
	if sessionID == "" || after < 0 {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "invalid event cursor"}
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 0 || limit > 1000 {
		return nil, &Error{Code: CodeInvalidRequest, Detail: "event replay limit is outside bounds"}
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", sessionID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	} else if err != nil {
		return nil, fmt.Errorf("check event session: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, sequence, turn_id, type, version, occurred_at, payload FROM events WHERE session_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var eventType string
		var occurredAt int64
		if err := rows.Scan(&event.ID, &event.SessionID, &event.Sequence, &event.TurnID, &eventType, &event.Version, &occurredAt, &event.Payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		event.Type = EventType(eventType)
		event.OccurredAt = fromUnixNano(occurredAt)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

func (s *Store) completeOperationTx(ctx context.Context, tx *sql.Tx, id, resourceType, resourceID string, result []byte) error {
	if len(result) > MaxOperationResultBytes {
		return &Error{Code: CodeInternal, Detail: "operation result is too large"}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET state = ?, resource_type = ?, resource_id = ?, result = ?, updated_at = ? WHERE id = ?`, string(OperationSucceeded), resourceType, resourceID, result, s.now().UnixNano(), id); err != nil {
		return fmt.Errorf("complete operation: %w", err)
	}
	return nil
}

func (s *Store) failAndCommit(tx *sql.Tx, id string, err error) error {
	code := CodeOf(err)
	if code == "" {
		code = CodeInternal
	}
	detail := persistedErrorDetail(err)
	if updateErr := s.completeFailureTx(tx, id, code, detail); updateErr != nil {
		return updateErr
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("commit failed operation: %w", commitErr)
	}
	return err
}

func (s *Store) completeFailureTx(tx *sql.Tx, id string, code ErrorCode, detail string) error {
	detail = boundedErrorDetail(detail)
	if _, err := tx.Exec(`UPDATE operations SET state = ?, error_code = ?, error_detail = ?, updated_at = ? WHERE id = ?`, string(OperationFailed), string(code), detail, s.now().UnixNano(), id); err != nil {
		return fmt.Errorf("fail operation: %w", err)
	}
	return nil
}

func persistedErrorDetail(err error) string {
	var storeErr *Error
	if errors.As(err, &storeErr) {
		return boundedErrorDetail(storeErr.Detail)
	}
	return boundedErrorDetail(err.Error())
}

func boundedErrorDetail(detail string) string {
	if !utf8.ValidString(detail) {
		detail = strings.ToValidUTF8(detail, "\uFFFD")
	}
	if len(detail) <= MaxErrorDetailBytes {
		return detail
	}
	detail = detail[:MaxErrorDetailBytes]
	for !utf8.ValidString(detail) {
		detail = detail[:len(detail)-1]
	}
	return detail
}

func operationError(op Operation) error {
	if op.ErrorCode == "" {
		return &Error{Code: CodeInternal, Detail: "operation failed without an error code"}
	}
	return &Error{Code: op.ErrorCode, Detail: op.ErrorDetail}
}

func mustJSON(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

func fromUnixNano(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}
