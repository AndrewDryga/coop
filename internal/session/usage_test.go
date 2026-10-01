package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A turn's cost has to survive the round trip, and "nothing reported" has to be
// distinguishable from "free". A caller that read a zero as a measurement would
// bill itself for a turn nobody measured.
func TestUsageRecordedDistinguishesAbsenceFromZero(t *testing.T) {
	if (Usage{}).Recorded() {
		t.Error("an unreported usage claims to be recorded")
	}
	for _, usage := range []Usage{
		{InputTokens: 1}, {CachedInputTokens: 1}, {OutputTokens: 1}, {ReasoningTokens: 1},
		{CostRecorded: true},
	} {
		if !usage.Recorded() {
			t.Errorf("%+v is a real measurement and reports otherwise", usage)
		}
	}
}

func TestUsageSnapshotReadsLiveWALWithoutMigrationOrCurrentTargetGuess(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := openTestStore(t, root)
	defer store.Close()
	sess, err := store.CreateSession(ctx, "usage-create", CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest, Target: "codex:gpt-6.1-sol@work"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := store.SubmitTurn(ctx, "usage-turn", SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "must not be read by usage snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	started := sess.CreatedAt.Add(time.Hour)
	finished := started.Add(time.Minute)
	if _, err := store.db.Exec(`UPDATE turns SET state='cancelled',started_at=?,finished_at=?,usage_input_tokens=100,usage_output_tokens=20 WHERE id=?`, started.UnixNano(), finished.UnixNano(), turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET target='claude:other@personal' WHERE id=?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadUsageSnapshot(ctx, root, started.Add(-time.Hour), finished)
	if err != nil || len(snapshot.Turns) != 1 || snapshot.Turns[0].Target != "codex:gpt-6.1-sol@work" || snapshot.Turns[0].Usage.InputTokens != 100 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if _, err := store.db.Exec(`INSERT INTO events(id,session_id,sequence,turn_id,type,version,occurred_at,payload) VALUES('rotation',?,(SELECT MAX(sequence)+1 FROM events WHERE session_id=?),?,'session.target_rotated',1,?,?)`, sess.ID, sess.ID, turn.ID, started.Add(time.Second).UnixNano(), `{"from":"codex:gpt-6.1-sol@work","to":"claude:other@personal"}`); err != nil {
		t.Fatal(err)
	}
	snapshot, err = ReadUsageSnapshot(ctx, root, started.Add(-time.Hour), finished)
	if err != nil || len(snapshot.Turns) != 1 || snapshot.Turns[0].Target != "" {
		t.Fatalf("mixed-credential turn was attributed: %+v %v", snapshot, err)
	}
	if _, err := store.db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUsageSnapshot(ctx, root, started.Add(-time.Hour), finished); err == nil {
		t.Fatal("future schema accepted")
	}
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 999 {
		t.Fatal("inspection migrated source database")
	}
}

func TestUsageSnapshotMissingStateDoesNotCreateAnything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	if _, err := ReadUsageSnapshot(context.Background(), root, time.Now().Add(-time.Hour), time.Now()); !os.IsNotExist(err) {
		t.Fatal("missing database did not remain unavailable", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("inspection created state")
	}
}
