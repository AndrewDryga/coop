package session

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// This task retires nothing: every schema a released or untagged build ever wrote still converts.
func TestSupportedSchemasStillConvertEveryHistoricalStore(t *testing.T) {
	for version := 1; version < SchemaVersion; version++ {
		if got := supportedSchemas.status(version); got != StoreConverts {
			t.Errorf("schema %d status = %v, want StoreConverts", version, got)
		}
	}
	for version, want := range map[int]StoreStatus{0: StoreEmpty, SchemaVersion: StoreCurrent, SchemaVersion + 1: StoreTooNew} {
		if got := supportedSchemas.status(version); got != want {
			t.Errorf("schema %d status = %v, want %v", version, got, want)
		}
	}
}

// Once a waypoint is set, the refusal names the schema whose builds convert the store, because
// the state version, not a release tag, is what an untagged deployment can be checked against.
func TestSchemaWindowRefusesOutsideItAndNamesTheConvertingSchema(t *testing.T) {
	window := schemaWindow{oldest: 25, current: 26}
	cases := []struct {
		version int
		status  StoreStatus
		refusal string
	}{
		{0, StoreEmpty, ""},
		{24, StoreTooOld, "session schema 24 is older than this coop build supports (25 to 26); " +
			"open it once with a coop build that uses session schema 25, then upgrade"},
		{25, StoreConverts, ""},
		{26, StoreCurrent, ""},
		{27, StoreTooNew, "session schema 27 is newer than this coop build supports (25 to 26)"},
	}
	for _, tc := range cases {
		if got := window.status(tc.version); got != tc.status {
			t.Errorf("schema %d status = %v, want %v", tc.version, got, tc.status)
		}
		got := ""
		if err := window.refusal(tc.version); err != nil {
			got = err.Error()
		}
		if got != tc.refusal {
			t.Errorf("schema %d refusal = %q, want %q", tc.version, got, tc.refusal)
		}
	}
}

// The check answers what Open would do and proves it wrote nothing: no file, no SQLite sidecar
// and no directory entry appears, changes or disappears under the state root.
func TestCheckStoreReportsWithoutWriting(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, root string)
		window  schemaWindow
		status  StoreStatus
		version int
	}{
		// A service that started holds the lock; until its first store it has no database yet.
		{"no database yet", func(t *testing.T, root string) {
			mkdirPrivate(t, root)
			if err := os.WriteFile(filepath.Join(root, lockName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, supportedSchemas, StoreEmpty, 0},
		{"current", func(t *testing.T, root string) { createStore(t, root) }, supportedSchemas, StoreCurrent, SchemaVersion},
		{"converts", func(t *testing.T, root string) {
			stoppedHistoricalStore(t, root, SchemaVersion-1)
		}, supportedSchemas, StoreConverts, SchemaVersion - 1},
		{"too old", func(t *testing.T, root string) {
			stoppedHistoricalStore(t, root, 24)
		}, schemaWindow{oldest: 25, current: 26}, StoreTooOld, 24},
		{"too new", func(t *testing.T, root string) {
			createStore(t, root)
			setUserVersion(t, filepath.Join(root, databaseName), SchemaVersion+1)
		}, supportedSchemas, StoreTooNew, SchemaVersion + 1},
		// A service that did not stop cleanly leaves committed pages in the log: here the main
		// file still says the old schema, and only the log knows the store was converted.
		{"crash leftovers", func(t *testing.T, root string) {
			copyLiveStore(t, convertingStore(t), root, true)
			if got := immutableUserVersion(t, filepath.Join(root, databaseName)); got != SchemaVersion-1 {
				t.Fatalf("fixture main file schema = %d, want %d left behind by the conversion", got, SchemaVersion-1)
			}
		}, supportedSchemas, StoreCurrent, SchemaVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			tc.prepare(t, root)
			before := stateSnapshot(t, root)
			check, err := checkStore(root, tc.window)
			assertSameSnapshot(t, before, stateSnapshot(t, root))
			if err != nil || check.Status != tc.status || check.Version != tc.version {
				t.Fatalf("check = %+v, %v; want status %v at schema %d", check, err, tc.status, tc.version)
			}
			if want := tc.window.refusal(tc.version); fmt.Sprint(check.Refusal) != fmt.Sprint(want) {
				t.Fatalf("refusal = %v, want %v", check.Refusal, want)
			}
		})
	}
}

// The refusal the check reports is the exact error a service start would hit.
func TestCheckStoreRefusalIsWhatOpenReturns(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	createStore(t, root)
	setUserVersion(t, filepath.Join(root, databaseName), SchemaVersion+1)
	check, err := CheckStore(root)
	if err != nil || check.Refusal == nil {
		t.Fatalf("check = %+v, %v; want a refusal", check, err)
	}
	if _, err := Open(root); err == nil || err.Error() != check.Refusal.Error() {
		t.Fatalf("Open error = %v, check refusal = %v", err, check.Refusal)
	}
}

// What the check cannot read without writing, or what Open itself would refuse to touch, is an
// error rather than a verdict, and it still leaves the root exactly as it found it.
func TestCheckStoreFailsWithoutWriting(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, root string) (cleanup func())
		want    string
	}{
		// A mistyped path, an unmounted volume or a new empty one must not pass a pre-upgrade check.
		{"no state root", func(*testing.T, string) func() { return nil }, ErrNoStateRoot.Error()},
		{"never used", func(t *testing.T, root string) func() { mkdirPrivate(t, root); return nil }, ErrNoSessionData.Error()},
		// SQLite opens a FIFO in place of the -shm index in blocking mode: the check would hang.
		{"index is a FIFO", func(t *testing.T, root string) func() {
			copyLiveStore(t, convertingStore(t), root, false)
			if err := syscall.Mkfifo(filepath.Join(root, databaseName+"-shm"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}, "the session service did not stop cleanly"},
		{"service running", func(t *testing.T, root string) func() {
			store, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			return func() { _ = store.Close() }
		}, ErrStoreInUse.Error()},
		{"log without its index", func(t *testing.T, root string) func() {
			copyLiveStore(t, convertingStore(t), root, false)
			return nil
		}, "the session service did not stop cleanly"},
		{"symlinked state root", func(t *testing.T, root string) func() {
			target := filepath.Join(t.TempDir(), "real")
			createStore(t, target)
			if err := os.Symlink(target, root); err != nil {
				t.Fatal(err)
			}
			return nil
		}, "state root is a symlink"},
		{"symlinked lock", func(t *testing.T, root string) func() {
			createStore(t, root)
			relink(t, filepath.Join(root, lockName))
			return nil
		}, "session state lock is a symlink"},
		{"symlinked database", func(t *testing.T, root string) func() {
			createStore(t, root)
			relink(t, filepath.Join(root, databaseName))
			return nil
		}, "session database is a symlink"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if cleanup := tc.prepare(t, root); cleanup != nil {
				defer cleanup()
			}
			before := stateSnapshot(t, root)
			// The order coop sessions check uses: hold the root, then read the store.
			var check StoreCheck
			release, err := HoldStateRoot(root)
			if err == nil {
				check, err = CheckStore(root)
				release()
			}
			assertSameSnapshot(t, before, stateSnapshot(t, root))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("check = %+v, %v; want an error containing %q", check, err, tc.want)
			}
			if tc.want == ErrStoreInUse.Error() && !errors.Is(err, ErrStoreInUse) {
				t.Fatalf("a running service must be reported as ErrStoreInUse, got %v", err)
			}
		})
	}
}

// While the check holds the state lock, a service cannot start and convert the store under it.
func TestOpenFailsFastWhileACheckHoldsTheLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	createStore(t, root)
	release, err := HoldStateRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil || !strings.Contains(err.Error(), "another session daemon owns this state root") {
		t.Fatalf("Open during a check = %v", err)
	}
	release()
	createStore(t, root)
}

func mkdirPrivate(t *testing.T, root string) {
	t.Helper()
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
}

// createStore leaves a current store under root the way a stopped service does.
func createStore(t *testing.T, root string) {
	t.Helper()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// stoppedHistoricalStore leaves a store at an older schema the way its stopped service did: in
// write-ahead-log mode, with the log checkpointed and removed by the clean close.
func stoppedHistoricalStore(t *testing.T, root string, version int) {
	t.Helper()
	mkdirPrivate(t, root)
	path := filepath.Join(root, databaseName)
	buildLegacyDatabase(t, path, version)
	execAndClose(t, path, "PRAGMA journal_mode = WAL")
	if _, err := os.Lstat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a clean close left the log behind: %v", err)
	}
}

func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	execAndClose(t, path, fmt.Sprintf("PRAGMA user_version = %d", version))
}

func execAndClose(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// convertingStore opens a store one schema behind, so the conversion Open commits is still only
// in the write-ahead log when the returned store is copied; the store closes with the test.
func convertingStore(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "live")
	stoppedHistoricalStore(t, root, SchemaVersion-1)
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return root
}

// copyLiveStore copies an open store's files the way a crash would leave them: the database, its
// log and (when withIndex) the log's shared-memory index, plus the unlocked lock file.
func copyLiveStore(t *testing.T, live, root string, withIndex bool) {
	t.Helper()
	mkdirPrivate(t, root)
	names := []string{lockName, databaseName, databaseName + "-wal"}
	if withIndex {
		names = append(names, databaseName+"-shm")
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(live, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func immutableUserVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(path, url.Values{"mode": {"ro"}, "immutable": {"1"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// relink moves a file aside and leaves a symlink to it under its old name.
func relink(t *testing.T, path string) {
	t.Helper()
	target := path + ".real"
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// stateSnapshot records everything a write could change under root, the root itself included:
// each entry's type and permissions, size, modification time and contents. A sidecar created and
// removed again still shows, as a changed directory modification time.
func stateSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return map[string]string{".": "absent"}
	}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		entry := fmt.Sprintf("%v size=%d mtime=%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += fmt.Sprintf(" sha256=%x", sha256.Sum256(data))
		}
		rel, err := filepath.Rel(root, path)
		snapshot[rel] = entry
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertSameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	if maps.Equal(before, after) {
		return
	}
	names := slices.Concat(slices.Collect(maps.Keys(before)), slices.Collect(maps.Keys(after)))
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		if before[name] != after[name] {
			t.Errorf("the check changed %s:\n before: %q\n  after: %q", name, before[name], after[name])
		}
	}
	t.FailNow()
}
