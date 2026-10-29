package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// A successful submit HTTP request did not mean its model turn was finished. The controller
// restarted a live worker in October 2026; this aggregate must see every committed turn.
func TestQuiescenceReadsEveryTurnFromLiveWALWithoutWriting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := openTestStore(t, root)
	defer store.Close()
	for i := 0; i < 1001; i++ {
		_, err := store.CreateSession(ctx, fmt.Sprintf("idle-%d", i), CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest, Target: "codex:model"})
		if err != nil {
			t.Fatal(err)
		}
	}
	sess, err := store.CreateSession(ctx, "last", CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest, Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "last-turn", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []TurnState{TurnQueued, TurnStarting, TurnRunning, TurnAwaitingValidation} {
		if _, err := store.db.Exec("UPDATE turns SET state=? WHERE id=?", state, turn.ID); err != nil {
			t.Fatal(err)
		}
		before := quiescenceFiles(t, root)
		got, err := readQuiescenceProcess(root)
		if err != nil || got.Idle || got.ActiveTurns != 1 {
			t.Fatalf("%s: %+v %v", state, got, err)
		}
		after := quiescenceFiles(t, root)
		for name, data := range before {
			if string(after[name]) != string(data) {
				t.Fatalf("inspection changed %s", name)
			}
		}
	}
	if _, err := store.db.Exec("UPDATE turns SET state='completed'; UPDATE sessions SET queued_turn_count=0, active_turn_id='', activity='parked'"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadQuiescence(ctx, root)
	if err != nil || !got.Idle {
		t.Fatalf("warm idle sessions are not work: %+v %v", got, err)
	}
	if _, err := store.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadQuiescence(ctx, root); err == nil {
		t.Fatal("future schema reported idle")
	}
}

func TestQuiescenceUnavailableNeverMeansIdle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := ReadQuiescence(context.Background(), root); err == nil {
		t.Fatal("missing store reported idle")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("inspection created state", err)
	}
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, databaseName), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadQuiescence(context.Background(), root); err == nil {
		t.Fatal("damaged store reported idle")
	}
}

func quiescenceFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(filepath.Join(root, databaseName+suffix))
		if err != nil {
			t.Fatal(err)
		}
		out[suffix] = data
	}
	return out
}

// A CLI is a separate process. SQLite reuses a writer's shared-memory mapping inside one
// process even for a read-only connection, so exercise the deployed boundary explicitly.
func TestQuiescenceReaderProcess(t *testing.T) {
	root := os.Getenv("COOP_TEST_QUIESCENCE_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	snapshot, err := ReadQuiescence(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(snapshot); err != nil {
		t.Fatal(err)
	}
}

func readQuiescenceProcess(root string) (Quiescence, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestQuiescenceReaderProcess$")
	cmd.Env = append(os.Environ(), "COOP_TEST_QUIESCENCE_ROOT="+root)
	data, err := cmd.Output()
	var snapshot Quiescence
	if err != nil {
		return snapshot, fmt.Errorf("read-only subprocess: %w", err)
	}
	err = json.NewDecoder(bytes.NewReader(data)).Decode(&snapshot)
	return snapshot, err
}

func TestQuiescenceDistinguishesDormantUncertaintyFromActiveCleanup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := openTestStore(t, root)
	defer store.Close()
	op, _, err := store.ReserveOperation(ctx, "CreateRemoteSession", "origin", map[string]string{"external_ref": "retained"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, state string
		busy          bool
	}{
		{"CreateRemoteSession", "reserved", true},
		{"CreateRemoteSession", "running", true},
		{"CreateRemoteSession", "uncertain", false},
		// A retained uncertain discard made an idle production worker unavailable for
		// every upgrade in October 2026. Its unknown outcome must survive inspection.
		{"Discard", "reserved", true},
		{"Discard", "running", true},
		{"Discard", "uncertain", false},
		{"CancelTurn", "uncertain", true},
		{"CancelTurn", "succeeded", false},
		{"CloseSession", "failed", false},
		{"CreateRemoteSession", "unknown", true},
	} {
		if _, err := store.db.Exec("UPDATE operations SET method=?,state=?,error_code='operation_uncertain' WHERE id=?", tc.method, tc.state, op.ID); err != nil {
			t.Fatal(err)
		}
		before, err := store.GetOperation(ctx, "origin")
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := ReadQuiescence(ctx, root)
		if err != nil || snapshot.Idle == tc.busy {
			t.Fatalf("%+v: snapshot=%+v err=%v", tc, snapshot, err)
		}
		after, err := store.GetOperation(ctx, "origin")
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("inspection changed retained operation: before=%+v after=%+v err=%v", before, after, err)
		}
	}
	if _, err := store.db.Exec("UPDATE operations SET state='failed'"); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, "session", CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest, Target: "codex:model"})
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"activity='starting'", "activity='running'", "activity='unknown'", "active_turn_id='unfinished'", "queued_turn_count=1", "state='unknown'"} {
		if _, err := store.db.Exec("UPDATE sessions SET activity='parked',active_turn_id='',queued_turn_count=0 WHERE id=?", sess.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("UPDATE sessions SET "+marker+" WHERE id=?", sess.ID); err != nil {
			t.Fatal(err)
		}
		snapshot, err := ReadQuiescence(ctx, root)
		if err != nil || snapshot.Idle || snapshot.ActiveSessions != 1 {
			t.Fatalf("%s: %+v %v", marker, snapshot, err)
		}
	}
}

func TestQuiescenceRejectsNonregularState(t *testing.T) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			root := t.TempDir()
			if suffix != "" {
				if err := os.WriteFile(filepath.Join(root, databaseName), []byte("database"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(root, databaseName+suffix)); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadQuiescence(context.Background(), root); err == nil {
				t.Fatal("followed linked database state")
			}
		})
	}
}

func TestQuiescenceRequiresLiveSidecarsInsteadOfCreatingThem(t *testing.T) {
	root := t.TempDir()
	store := openTestStore(t, root)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, databaseName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readQuiescenceProcess(root); err == nil {
		t.Fatal("stopped store reported idle without live WAL evidence")
	}
	after, err := os.ReadFile(filepath.Join(root, databaseName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection changed the stopped database", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(root, databaseName+suffix)); !os.IsNotExist(err) {
			t.Fatal("inspection created sidecar", suffix, err)
		}
	}
}
