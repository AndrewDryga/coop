package session

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// buildLegacyDatabase creates a session.sqlite file shaped exactly like one a real daemon
// would have left behind at the given historical schema version: test-owned frozen literal DDL,
// never current schema constants or today's migrate(), followed by hand-written INSERTs using only the columns that
// existed at that version. A bug in migrate() itself therefore cannot silently launder into a
// "passing" fixture the way replaying Open() to build the fixture could.
func buildLegacyDatabase(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if version < 1 || version > len(legacySchemas) {
		t.Fatalf("unsupported historical fixture version%d", version)
	}
	ddl := strings.Join(legacySchemas[:version], "")
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("build v%d schema: %v", version, err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set v%d user_version: %v", version, err)
	}
	if version >= 25 {
		if _, err := db.Exec(`INSERT INTO store_identity(slot, id) VALUES (1, 'store_0123456789abcdef0123456789abcdef')`); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.Exec(`INSERT INTO operations
		(id, method, idempotency_key, request_hash, state, resource_type, resource_id, result, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"op-legacy", "CreateSession", "legacy-key", "legacy-hash", "succeeded", "session", "legacy-session",
		[]byte(`{}`), int64(1000), int64(1000),
	); err != nil {
		t.Fatalf("seed v%d operation: %v", version, err)
	}

	sessionCols := []string{"id", "target", "revision", "state", "activity", "max_turns", "max_queued_turns", "max_queued_bytes", "created_at", "updated_at"}
	sessionVals := []any{"legacy-session", "codex:legacy", int64(1), "open", "parked", 100, 20, 1048576, int64(1000), int64(1000)}
	if version >= 2 {
		// companions only exists from v2 on; a real v2+ row would have carried a real value,
		// so seed one to prove migration preserves it rather than merely defaulting it.
		sessionCols = append(sessionCols, "companions")
		sessionVals = append(sessionVals, `[{"name":"docs","repository":"/repo/docs","workspace":"/work/docs","base_commit":"deadbeef"}]`)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sessionCols)), ",")
	query := fmt.Sprintf("INSERT INTO sessions (%s) VALUES (%s)", strings.Join(sessionCols, ", "), placeholders)
	if _, err := db.Exec(query, sessionVals...); err != nil {
		t.Fatalf("seed v%d session: %v", version, err)
	}

	if _, err := db.Exec(`INSERT INTO turns
		(id, session_id, ordinal, idempotency_key, request_hash, state, send_state, prompt, queued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"turn-legacy", "legacy-session", int64(1), "legacy-turn-key", "legacy-turn-hash", "queued", "none", "legacy prompt", int64(1000),
	); err != nil {
		t.Fatalf("seed v%d turn: %v", version, err)
	}
}

// A new store is built from currentSchema in one step, while every store in service reached its
// shape through conversion steps; the frozen fixtures hold the schema 26 those stores have. A
// converted store must end with the same tables, columns, constraints and indexes as a new one,
// keep its rows and identity, and a new store gets exactly one store identity.
func TestNewStoreSchemaMatchesAConvertedStore(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh")
	createStore(t, fresh)
	converted := filepath.Join(t.TempDir(), "converted")
	stoppedHistoricalStore(t, converted, SchemaVersion-1)
	store, err := Open(converted)
	if err != nil {
		t.Fatalf("convert schema %d: %v", SchemaVersion-1, err)
	}
	if store.ID() != "store_0123456789abcdef0123456789abcdef" {
		t.Fatalf("conversion changed the store identity to %q", store.ID())
	}
	if _, err := store.GetSession(context.Background(), "legacy-session"); err != nil {
		t.Fatalf("converted store lost its session: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := immutableUserVersion(t, filepath.Join(converted, databaseName)); got != SchemaVersion {
		t.Fatalf("converted schema = %d, want %d", got, SchemaVersion)
	}
	got, want := schemaShape(t, filepath.Join(fresh, databaseName)), schemaShape(t, filepath.Join(converted, databaseName))
	if !slices.Equal(got, want) {
		t.Errorf("new store schema differs from the converted one:\n got: %q\nwant: %q", got, want)
	}

	ids := map[string]bool{}
	for range 2 {
		root := filepath.Join(t.TempDir(), "state")
		createStore(t, root)
		db, err := sql.Open("sqlite", "file:"+filepath.Join(root, databaseName))
		if err != nil {
			t.Fatal(err)
		}
		var rows, slot int
		var id string
		err = db.QueryRow(`SELECT COUNT(*), MIN(slot), MIN(id) FROM store_identity`).Scan(&rows, &slot, &id)
		db.Close()
		if err != nil || rows != 1 || slot != 1 || !validStoreID(id) {
			t.Fatalf("store identity = %d rows, slot %d, id %q, %v; want one valid row in slot 1", rows, slot, id, err)
		}
		ids[id] = true
	}
	if len(ids) != 2 {
		t.Fatal("two new stores got the same identity")
	}
}

// schemaShape lists every schema object as type, name, table and its SQL with layout whitespace
// removed. SQLite keeps an ALTER TABLE ADD COLUMN as text spliced into the original CREATE TABLE,
// so only spacing and line breaks may differ between a converted store and a new one.
func schemaShape(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(path, url.Values{"mode": {"ro"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var shape []string
	for rows.Next() {
		var kind, name, table, text string
		if err := rows.Scan(&kind, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		text = strings.Join(strings.Fields(text), " ")
		for _, punct := range []string{",", "(", ")"} {
			text = strings.ReplaceAll(text, " "+punct, punct)
			text = strings.ReplaceAll(text, punct+" ", punct)
		}
		shape = append(shape, kind+" "+name+" on "+table+": "+text)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return shape
}

// A store left behind below the oldest schema this build converts is refused by name, with the
// schema whose builds convert it, and Open changes nothing on disk.
func TestOpenRefusesEveryOlderSchemaByNameAndLeavesTheStoreAlone(t *testing.T) {
	for version := 1; version < OldestSchemaVersion; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			stoppedHistoricalStore(t, root, version)
			path := filepath.Join(root, databaseName)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			want := fmt.Sprintf("session schema %d is older than this coop build supports (%d to %d); "+
				"open it once with a coop build that uses session schema %d and converts older stores (v12.1.0, or any build at or before commit 29133b0e), then upgrade, or start fresh with a new --state folder",
				version, OldestSchemaVersion, SchemaVersion, OldestSchemaVersion)
			if store, err := Open(root); err == nil || err.Error() != want {
				if store != nil {
					store.Close()
				}
				t.Fatalf("Open = %v, want %q", err, want)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("the refusal changed the database: %v", err)
			}
			if _, err := os.Lstat(path + "-wal"); err == nil {
				t.Fatal("the refusal left a write-ahead log behind")
			}
			if got := immutableUserVersion(t, path); got != version {
				t.Fatalf("user_version after the refusal = %d, want unchanged %d", got, version)
			}
		})
	}
}

// migrate() builds the whole schema in one transaction and only sets user_version at the very
// end, so a failure partway through must roll back everything, including the version pragma —
// otherwise a retried Open would find half a schema and take it for a store.
func TestFailedMigrationRollsBackAndLeavesUserVersionUntouched(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, databaseName)
	// Sabotage the last table currentSchema creates, so its CREATE TABLE fails after every other
	// table was made in the same transaction: a deterministic, real SQL failure partway through
	// migrate(), without touching production code to inject one.
	execAndClose(t, path, `CREATE TABLE session_owner_bindings (stray INTEGER)`)

	if _, err := Open(root); err == nil {
		t.Fatal("Open succeeded despite a broken schema step")
	} else if !strings.Contains(err.Error(), fmt.Sprintf("create session schema %d", SchemaVersion)) {
		t.Fatalf("migration failure = %v, want a schema creation error", err)
	}

	// Open closes the handle and releases the lock on any error, so the file is safe to
	// inspect directly.
	verify, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var version, tables int
	if err := verify.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("user_version after failed migration = %d, want unchanged 0", version)
	}
	if err := verify.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Fatalf("tables after failed migration = %d, want only the stray one", tables)
	}
}

// migrate() also has to refuse to run backwards: a database stamped with a schema version newer
// than this binary knows about (an older binary opening a newer daemon's state) must fail
// outright rather than silently truncating data it doesn't understand.
func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, databaseName)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("session schema %d is newer than this coop build supports (%d to %d)",
		SchemaVersion+1, OldestSchemaVersion, SchemaVersion)
	if _, err := Open(root); err == nil || err.Error() != want {
		t.Fatalf("newer schema version error = %v, want %q", err, want)
	}
}
