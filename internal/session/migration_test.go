package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildLegacyDatabase creates a session.sqlite file shaped exactly like one a real daemon
// would have left behind at the given historical schema version: the literal DDL migrate() has
// always used to build that version (schemaV1..schemaVN, embedded verbatim — never regenerated
// by running today's migrate()), followed by hand-written INSERTs using only the columns that
// existed at that version. A bug in migrate() itself therefore cannot silently launder into a
// "passing" fixture the way replaying Open() to build the fixture could.
func buildLegacyDatabase(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ddl := schemaV1
	if version >= 2 {
		ddl += schemaV2
	}
	if version >= 3 {
		ddl += schemaV3
	}
	if version >= 4 {
		ddl += schemaV4
	}
	if version >= 5 {
		ddl += schemaV5
	}
	if version >= 6 {
		ddl += schemaV6
	}
	if version >= 7 {
		ddl += schemaV7
	}
	if version >= 8 {
		ddl += schemaV8
	}
	if version >= 9 {
		ddl += schemaV9
	}
	if version >= 10 {
		ddl += schemaV10
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("build v%d schema: %v", version, err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("set v%d user_version: %v", version, err)
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

// A database left behind at any historical version must migrate cleanly to the current schema:
// the version pragma lands on SchemaVersion, pre-existing rows stay readable through today's
// scanners (proving no column was renamed or reordered incompatibly along the way), and the
// columns/tables added after that version come back with sane defaults rather than missing data.
func TestMigrationFromEachHistoricalVersionReachesCurrentSchema(t *testing.T) {
	ctx := context.Background()
	for version := 1; version < SchemaVersion; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			buildLegacyDatabase(t, filepath.Join(root, databaseName), version)

			store := openTestStore(t, root)
			defer store.Close()

			var gotVersion int
			if err := store.db.QueryRow("PRAGMA user_version").Scan(&gotVersion); err != nil {
				t.Fatal(err)
			}
			if gotVersion != SchemaVersion {
				t.Fatalf("user_version after v%d migration = %d, want %d", version, gotVersion, SchemaVersion)
			}

			op, err := store.GetOperation(ctx, "legacy-key")
			if err != nil || op.State != OperationSucceeded || op.ResourceID != "legacy-session" {
				t.Fatalf("legacy operation after v%d migration = %+v, err=%v", version, op, err)
			}

			sess, err := store.GetSession(ctx, "legacy-session")
			if err != nil {
				t.Fatalf("legacy session after v%d migration: %v", version, err)
			}
			if sess.Target != "codex:legacy" || sess.State != SessionOpen ||
				sess.MaxPatchBytes != 1048576 || !sess.ProjectEnv || !sess.ProjectMCP {
				t.Fatalf("legacy session after v%d migration = %+v", version, sess)
			}
			wantCompanions := 0
			if version >= 2 {
				wantCompanions = 1
			}
			if len(sess.Companions) != wantCompanions {
				t.Fatalf("v%d migration companions = %+v, want %d entries", version, sess.Companions, wantCompanions)
			}

			turn, err := store.GetTurn(ctx, "legacy-session", "turn-legacy")
			if err != nil {
				t.Fatalf("legacy turn after v%d migration: %v", version, err)
			}
			if turn.Prompt != "legacy prompt" || turn.Usage.Recorded() {
				t.Fatalf("legacy turn after v%d migration = %+v", version, turn)
			}

			// The migrated store is not just readable, it is fully writable under the
			// current schema.
			created, err := store.CreateSession(ctx, fmt.Sprintf("post-migration-v%d", version), CreateSessionRequest{Target: "codex"})
			if err != nil || len(created.Companions) != 0 {
				t.Fatalf("post-migration create after v%d = %+v, err=%v", version, created, err)
			}
		})
	}
}

// migrate() runs every catch-up step in one transaction and only sets user_version at the very
// end, so a failure partway through must roll back everything, including the version pragma —
// otherwise a retried Open would think a step had already applied when it hadn't.
func TestFailedMigrationRollsBackAndLeavesUserVersionUntouched(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, databaseName)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	// Sabotage the v1->v2 step: pre-create the column schemaV2's ALTER TABLE is about to add,
	// so migrate()'s tx.Exec(schemaV2) fails on "duplicate column name" mid-transaction — a
	// deterministic, real SQL failure partway through migrate(), without touching production
	// code to inject one.
	if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN companions TEXT NOT NULL DEFAULT '[]'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(root); err == nil {
		t.Fatal("Open succeeded despite a broken migration step")
	} else if !strings.Contains(err.Error(), "migrate schema v2") {
		t.Fatalf("migration failure = %v, want a v2 migration error", err)
	}

	// Open closes the handle and releases the lock on any error, so the file is safe to
	// inspect directly.
	verify, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	var version int
	if err := verify.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("user_version after failed migration = %d, want unchanged 1", version)
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
	if _, err := Open(root); err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Fatalf("newer schema version error = %v", err)
	}
}
