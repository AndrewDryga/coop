package session

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

var (
	// ErrStoreInUse reports that a running session service owns the state root. Its database
	// could be read without writing, but the read would race the service: one that stops midway
	// leaves SQLite to create a log file beside the database. A conversion's backup needs the
	// service stopped anyway.
	ErrStoreInUse = errors.New("a session service is using this state root")
	// ErrNoStateRoot reports a state root that does not exist. It is an error rather than an empty
	// store, so a mistyped path or an unmounted volume cannot pass a pre-upgrade check.
	ErrNoStateRoot = errors.New("there is no session state root")
	// ErrNoSessionData reports a state root that has neither a database nor the lock every service
	// start creates: it never held session data. A mistyped Docker volume name creates exactly
	// that, so it must not pass as an empty store either.
	ErrNoSessionData = errors.New("there is no session data in this state root")

	errUncleanStop = errors.New("the session service did not stop cleanly, so its database cannot be " +
		"read without changing it; start and stop the service once with the coop build that last ran it, " +
		"then check again")
)

// StoreCheck is what this build would do with the session store under a state root.
type StoreCheck struct {
	Status  StoreStatus
	Version int   // the store's schema version; 0 for StoreEmpty
	Refusal error // what Open would answer for StoreTooOld and StoreTooNew; nil otherwise
}

// HoldStateRoot keeps a session service from starting on root until release, without writing
// there: it takes the service's state lock shared, and returns ErrStoreInUse while a service owns
// the root. A missing lock file stays missing, because creating it would be a write. No service
// has used the root then, but one could still start during the caller's read.
func HoldStateRoot(root string) (release func(), err error) {
	// O_NONBLOCK keeps a FIFO in the lock's place from hanging the open; the type test refuses it.
	lock, err := os.OpenFile(filepath.Join(root, lockName), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, nil
	}
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errors.New("session state lock is a symlink")
		}
		return nil, fmt.Errorf("open session state lock: %w", err)
	}
	info, err := lock.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("session state lock is not a regular file")
	}
	if err == nil {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			err = ErrStoreInUse
		}
	}
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return func() { _ = lock.Close() }, nil // closing the descriptor drops the lock
}

// CheckStore reports what Open would do with the store under root, and changes nothing there: it
// creates no file and leaves the SQLite sidecars as they are. Call it while holding the root with
// HoldStateRoot, so a service cannot start and convert the store during the read.
func CheckStore(root string) (StoreCheck, error) {
	return checkStore(root, supportedSchemas)
}

func checkStore(root string, window schemaWindow) (StoreCheck, error) {
	if root == "" {
		return StoreCheck{}, &Error{Code: CodeInvalidRequest, Detail: "state root is required"}
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return StoreCheck{}, ErrNoStateRoot
	}
	if err != nil {
		return StoreCheck{}, fmt.Errorf("inspect state root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return StoreCheck{}, errors.New("state root is a symlink")
	}
	if !info.IsDir() {
		return StoreCheck{}, errors.New("state root is not a directory")
	}
	version, err := readSchemaVersion(filepath.Join(root, databaseName))
	if errors.Is(err, os.ErrNotExist) {
		if _, lockErr := os.Lstat(filepath.Join(root, lockName)); errors.Is(lockErr, os.ErrNotExist) {
			return StoreCheck{}, ErrNoSessionData
		}
		return StoreCheck{Status: StoreEmpty}, nil
	}
	if err != nil {
		return StoreCheck{}, err
	}
	if version < 0 {
		return StoreCheck{}, window.refusal(version) // damaged, not a verdict about its age
	}
	return StoreCheck{Status: window.status(version), Version: version, Refusal: window.refusal(version)}, nil
}

// readSchemaVersion reads a stopped store's schema version without writing beside it. With no
// write-ahead log the file is complete, and immutable mode reads it without the -wal and -shm
// files an ordinary read-only open creates and leaves behind. A log left by a service that did not
// stop cleanly still holds committed pages, so that read goes through the log with SQLite's
// readonly_shm parameter, which indexes the log privately instead of writing to -shm. That
// parameter is undocumented (SQLite's own read-only tests use it); the crash-leftovers case in
// TestCheckStoreReportsWithoutWriting fails if it ever stops working.
func readSchemaVersion(path string) (int, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if err != nil {
		return 0, fmt.Errorf("inspect session database: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("session database is a symlink")
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("session database is not a regular file")
	}
	query := url.Values{"mode": {"ro"}, "immutable": {"1"}}
	// Immutable mode ignores a rollback journal, so a half-written first page could be read.
	if journal, err := os.Lstat(path + "-journal"); err == nil && journal.Size() > 0 {
		return 0, errUncleanStop
	}
	wal, err := os.Lstat(path + "-wal")
	if err == nil && !wal.Mode().IsRegular() {
		return 0, errors.New("the session database log is not a regular file")
	}
	if err == nil && wal.Size() > 0 {
		// Without the -shm index the read fails; on an empty database SQLite deletes the log. SQLite
		// would open a FIFO in its place and wait for a writer, so only a regular file will do.
		if shm, err := os.Lstat(path + "-shm"); err != nil || !shm.Mode().IsRegular() || info.Size() == 0 {
			return 0, errUncleanStop
		}
		query = url.Values{"mode": {"ro"}, "readonly_shm": {"1"}}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("inspect session database log: %w", err)
	}
	db, err := sql.Open("sqlite", sqliteFileURI(path, query))
	if err != nil {
		return 0, fmt.Errorf("open session database: %w", err)
	}
	defer db.Close()
	var version, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("read session schema version: %w", err)
	}
	if version != 0 {
		return version, nil
	}
	// Version 0 is a database Open has not set up yet, which has no tables.
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema").Scan(&tables); err != nil {
		return 0, fmt.Errorf("read session database tables: %w", err)
	}
	if tables > 0 {
		return 0, errors.New("the session database has tables but no schema version")
	}
	return 0, nil
}
