package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCompactTurnOperationResultsBacksUpRewritesAndReclaims(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	prompt := strings.Repeat("private prompt bytes ", 4096)
	var firstRequest SubmitTurnRequest
	var firstTurn Turn
	for i := 0; i < 80; i++ {
		key := "legacy-submit-" + strings.Repeat("x", i%3) + time.Unix(int64(i), 0).UTC().Format("150405")
		request := SubmitTurnRequest{SessionID: "session-1", ExpectedRevision: 1, Prompt: prompt + key}
		op, replay, err := store.ReserveOperation(ctx, "SubmitTurn", key, request)
		if err != nil || replay {
			t.Fatalf("reserve legacy operation %d: replay=%v err=%v", i, replay, err)
		}
		turn := Turn{
			ID: "turn-" + key, SessionID: request.SessionID, Ordinal: int64(i + 1),
			IdempotencyKey: key, RequestHash: op.RequestHash, State: TurnQueued,
			SendState: SendStateNone, Prompt: request.Prompt,
			QueuedAt: time.Date(2026, 8, 29, 12, 0, i, 0, time.UTC),
		}
		legacy, err := json.Marshal(turn)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteOperation(ctx, op.ID, "turn", turn.ID, legacy); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstRequest, firstTurn = request, turn
		}
	}
	beforeOperation, err := store.GetOperation(ctx, firstTurn.IdempotencyKey)
	if err != nil || !strings.Contains(string(beforeOperation.Result), prompt[:100]) {
		t.Fatalf("legacy operation before compaction = %+v err=%v", beforeOperation, err)
	}
	beforeReplay, err := store.SubmitTurn(ctx, firstTurn.IdempotencyKey, firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	wantPublic := turnOperationResultFromTurn(beforeReplay)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(t.TempDir(), "session-before-compact.sqlite")
	result, err := CompactTurnOperationResults(ctx, root, backup)
	if err != nil {
		t.Fatal(err)
	}
	if result.BackupPath != backup || result.CompactedOperations != 80 ||
		result.BackupBytes == 0 ||
		result.ResultBytesAfter >= result.ResultBytesBefore ||
		result.DatabaseBytesAfter >= result.DatabaseBytesBefore {
		t.Fatalf("compaction result = %+v", result)
	}
	assertMode(t, backup, 0o600)

	backupDB, err := sql.Open("sqlite", sqliteFileURI(backup, nil))
	if err != nil {
		t.Fatal(err)
	}
	var backupResult []byte
	if err := backupDB.QueryRow(`SELECT result FROM operations WHERE idempotency_key = ?`, firstTurn.IdempotencyKey).Scan(&backupResult); err != nil {
		backupDB.Close()
		t.Fatal(err)
	}
	if err := backupDB.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backupResult), prompt[:100]) || strings.Contains(string(backupResult), `"receipt_version"`) {
		t.Fatalf("backup did not retain the legacy receipt: %s", backupResult[:min(len(backupResult), 500)])
	}

	store = openTestStore(t, root)
	defer store.Close()
	afterOperation, err := store.GetOperation(ctx, firstTurn.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(afterOperation.Result), prompt[:100]) || !strings.Contains(string(afterOperation.Result), `"receipt_version":1`) {
		t.Fatalf("source operation was not compacted: %s", afterOperation.Result[:min(len(afterOperation.Result), 500)])
	}
	afterReplay, err := store.SubmitTurn(ctx, firstTurn.IdempotencyKey, firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got := turnOperationResultFromTurn(afterReplay); !reflect.DeepEqual(got, wantPublic) {
		t.Fatalf("public replay changed:\n got  %+v\n want %+v", got, wantPublic)
	}
}

func TestCompactTurnOperationResultsRefusesBackupInsideStateRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "nested")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, backup := range []string{
		filepath.Join(root, "backup.sqlite"),
		filepath.Join(root, databaseName+"-wal"),
		filepath.Join(root, databaseName+"-shm"),
		filepath.Join(inside, "backup.sqlite"),
	} {
		if _, err := CompactTurnOperationResults(context.Background(), root, backup); err == nil ||
			!strings.Contains(err.Error(), "outside the session state root") {
			t.Errorf("inside-root backup %q error = %v", backup, err)
		}
		if _, err := os.Lstat(backup); !os.IsNotExist(err) {
			t.Errorf("inside-root backup %q was created: %v", backup, err)
		}
	}

	aliasRoot := t.TempDir()
	if err := os.Symlink(root, filepath.Join(aliasRoot, "state-link")); err != nil {
		t.Fatal(err)
	}
	aliasedBackup := filepath.Join(aliasRoot, "state-link", "nested", "backup.sqlite")
	if _, err := CompactTurnOperationResults(context.Background(), root, aliasedBackup); err == nil ||
		!strings.Contains(err.Error(), "outside the session state root") {
		t.Fatalf("aliased inside-root backup error = %v", err)
	}
}

func TestCompactTurnOperationResultsNeverOverwritesABackup(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "existing.sqlite")
	if err := os.WriteFile(backup, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CompactTurnOperationResults(ctx, root, backup); err == nil || !strings.Contains(err.Error(), "create new session backup") {
		t.Fatalf("existing-backup error = %v", err)
	}
	data, err := os.ReadFile(backup)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("existing backup = %q err=%v", data, err)
	}
}

func TestCompactTurnOperationResultsRefusesAnActiveStateRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	defer store.Close()
	backup := filepath.Join(t.TempDir(), "backup.sqlite")
	if _, err := CompactTurnOperationResults(context.Background(), root, backup); err == nil ||
		!strings.Contains(err.Error(), "another session daemon owns this state root") {
		t.Fatalf("active-state compaction error = %v", err)
	}
	if _, err := os.Lstat(backup); !os.IsNotExist(err) {
		t.Fatalf("active-state compaction created a backup: %v", err)
	}
}

func TestCompactTurnOperationResultsBacksUpBeforeSchemaMigration(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, databaseName)
	buildLegacyDatabase(t, databasePath, SchemaVersion-1)
	db, err := sql.Open("sqlite", sqliteFileURI(databasePath, nil))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(Turn{
		ID: "turn-compaction", SessionID: "legacy-session", Ordinal: 2,
		State: TurnQueued, SendState: SendStateNone, Prompt: "duplicate prompt",
		QueuedAt: time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO operations
		(id, method, idempotency_key, request_hash, state, resource_type, resource_id, result, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, "op-compaction", "SubmitTurn", "compact-key", "hash",
		"succeeded", "turn", "turn-compaction", legacy, int64(2000), int64(2000)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(t.TempDir(), "before-migration.sqlite")
	result, err := CompactTurnOperationResults(ctx, root, backup)
	if err != nil || result.CompactedOperations != 1 {
		t.Fatalf("compaction = %+v err=%v", result, err)
	}
	backupDB, err := sql.Open("sqlite", sqliteFileURI(backup, nil))
	if err != nil {
		t.Fatal(err)
	}
	var backupVersion int
	if err := backupDB.QueryRow("PRAGMA user_version").Scan(&backupVersion); err != nil {
		backupDB.Close()
		t.Fatal(err)
	}
	backupDB.Close()
	if backupVersion != SchemaVersion-1 {
		t.Fatalf("backup schema = %d, want original %d", backupVersion, SchemaVersion-1)
	}
	store := openTestStore(t, root)
	defer store.Close()
	var currentVersion int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
		t.Fatal(err)
	}
	if currentVersion != SchemaVersion {
		t.Fatalf("compacted schema = %d, want %d", currentVersion, SchemaVersion)
	}
	op, err := store.GetOperation(ctx, "compact-key")
	if err != nil || !strings.Contains(string(op.Result), `"receipt_version":1`) || strings.Contains(string(op.Result), "duplicate prompt") {
		t.Fatalf("compacted legacy operation = %+v err=%v", op, err)
	}
}

func TestCompactTurnOperationResultsRollsBackMalformedLegacyReceipt(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	store := openTestStore(t, root)
	request := SubmitTurnRequest{SessionID: "session-1", ExpectedRevision: 1, Prompt: "prompt"}
	op, _, err := store.ReserveOperation(ctx, "SubmitTurn", "broken", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteOperation(ctx, op.ID, "turn", "turn-broken", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "before.sqlite")
	result, err := CompactTurnOperationResults(ctx, root, backup)
	if err == nil || !strings.Contains(err.Error(), "missing turn id") {
		t.Fatalf("malformed-receipt result = %+v err=%v", result, err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("valid pre-rewrite backup was not retained: %v", err)
	}
	store = openTestStore(t, root)
	defer store.Close()
	current, err := store.GetOperation(ctx, "broken")
	if err != nil || string(current.Result) != `{}` {
		t.Fatalf("malformed source receipt changed: result=%s err=%v", current.Result, err)
	}
}

func TestCompactTurnOperationResultsRequiresExistingState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	backup := filepath.Join(t.TempDir(), "backup.sqlite")
	if _, err := CompactTurnOperationResults(context.Background(), root, backup); err == nil {
		t.Fatal("compaction created a missing state root")
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("missing state root was created: %v", err)
	}
}
