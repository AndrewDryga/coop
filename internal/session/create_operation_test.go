package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCompleteCreateSessionOperationUsesOnlyTheOuterOperation(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "outer-create")
	req := remoteCreateSessionRequest("remote-session")

	const callers = 2
	results := make(chan Session, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	start := make(chan struct{})
	for range callers {
		go func() {
			ready.Done()
			<-start
			sess, err := store.CompleteCreateSessionOperation(ctx, op, req)
			results <- sess
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if sess := <-results; sess.ID != req.ID || sess.LastEventSequence != 1 {
			t.Fatalf("created session = %+v", sess)
		}
	}
	operation, err := store.GetOperation(ctx, "outer-create")
	if err != nil || operation.State != OperationSucceeded || operation.ResourceID != req.ID {
		t.Fatalf("outer operation = %+v err=%v", operation, err)
	}
	if _, err := store.GetOperation(ctx, "create-session-"+op.ID); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("synthetic inner operation exists: %v", err)
	}
	events, err := store.ListEvents(ctx, req.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Type != EventSessionCreated {
		t.Fatalf("session events = %+v err=%v", events, err)
	}
	var operations, sessions int
	if err := store.db.QueryRow(`SELECT count(*) FROM operations`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if operations != 1 || sessions != 1 {
		t.Fatalf("rows: operations=%d sessions=%d", operations, sessions)
	}
	changed := req
	changed.Target = "claude:model"
	if _, err := store.CompleteCreateSessionOperation(ctx, op, changed); CodeOf(err) != CodeOperationIntentConflict {
		t.Fatalf("conflicting replay error = %v", err)
	}
}

func TestCompleteCreateSessionOperationRollsBackEveryRowWhenOuterCompletionFails(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "atomic-create")
	req := remoteCreateSessionRequest("atomic-session")
	trigger := fmt.Sprintf(`CREATE TRIGGER fail_remote_create_completion
		BEFORE UPDATE OF state ON operations
		WHEN OLD.id = '%s' AND NEW.state = 'succeeded'
		BEGIN SELECT RAISE(ABORT, 'injected completion failure'); END`, op.ID)
	if _, err := store.db.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteCreateSessionOperation(ctx, op, req); err == nil {
		t.Fatal("injected outer completion failure unexpectedly committed")
	}
	if _, err := store.GetSession(ctx, req.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("session survived failed transaction: %v", err)
	}
	var events int
	if err := store.db.QueryRow(`SELECT count(*) FROM events WHERE session_id = ?`, req.ID).Scan(&events); err != nil || events != 0 {
		t.Fatalf("events after rollback = %d err=%v", events, err)
	}
	current, err := store.GetOperationByID(ctx, op.ID)
	if err != nil || !sameOperationSnapshot(current, op) {
		t.Fatalf("outer operation changed after rollback: %+v err=%v", current, err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER fail_remote_create_completion`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteCreateSessionOperation(ctx, op, req); err != nil {
		t.Fatalf("retry after atomic rollback: %v", err)
	}
}

func TestCompleteCreateSessionOperationRejectsAStaleOuterSnapshot(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "stale-create")
	stale := op
	stale.Result = []byte(`{"operation_id":"changed"}`)
	req := remoteCreateSessionRequest("stale-session")
	if _, err := store.CompleteCreateSessionOperation(ctx, stale, req); CodeOf(err) != CodeOperationIntentConflict {
		t.Fatalf("stale outer snapshot error = %v", err)
	}
	if _, err := store.GetSession(ctx, req.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("stale worker created a session: %v", err)
	}
}

func TestCompleteCreateSessionOperationRejectsMissingRepositoryFreshness(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "missing-freshness-create")
	req := remoteCreateSessionRequest("missing-freshness-session")
	req.RepositoryFreshness = nil

	if _, err := store.CompleteCreateSessionOperation(ctx, op, req); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("missing freshness error = %v", err)
	}
	if _, err := store.GetSession(ctx, req.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing freshness created a session: %v", err)
	}
}

func TestCompleteCreateSessionOperationFinishesAnExactHistoricalSplitCreate(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "historical-outer")
	req := remoteCreateSessionRequest("historical-session")
	innerKey := "create-session-" + op.ID
	inner, err := store.CreateSession(ctx, innerKey, req)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteCreateSessionOperation(ctx, op, req)
	if err != nil || completed.ID != inner.ID {
		t.Fatalf("complete historical split create = %+v err=%v", completed, err)
	}
	innerOperation, err := store.GetOperation(ctx, innerKey)
	if err != nil || innerOperation.State != OperationSucceeded || innerOperation.ResourceID != inner.ID {
		t.Fatalf("historical inner operation = %+v err=%v", innerOperation, err)
	}
	var operations, events int
	if err := store.db.QueryRow(`SELECT count(*) FROM operations`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM events WHERE session_id = ?`, req.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if operations != 2 || events != 1 {
		t.Fatalf("historical rows changed: operations=%d events=%d", operations, events)
	}
}

func TestCompleteCreateSessionOperationRejectsAnUnprovenExistingSession(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "unproven-outer")
	req := remoteCreateSessionRequest("unproven-session")
	if _, err := store.CreateSession(ctx, "caller-owned-create", req); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteCreateSessionOperation(ctx, op, req); CodeOf(err) != CodeOperationIntentConflict {
		t.Fatalf("unproven existing session error = %v", err)
	}
	if _, err := store.GetSession(ctx, req.ID); err != nil {
		t.Fatalf("unproven existing session was changed: %v", err)
	}
	outer, err := store.GetOperationByID(ctx, op.ID)
	if err != nil || outer.State != OperationRunning {
		t.Fatalf("outer operation after conflict = %+v err=%v", outer, err)
	}
}

func runningRemoteCreateOperation(t *testing.T, store *Store, key string) Operation {
	t.Helper()
	ctx := context.Background()
	op, replay, err := store.ReserveOperation(ctx, "CreateRemoteSession", key, map[string]string{"task": key})
	if err != nil || replay {
		t.Fatalf("reserve outer operation: %+v replay=%v err=%v", op, replay, err)
	}
	intent := []byte(`{"operation_id":"` + op.ID + `"}`)
	if err := store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
		t.Fatal(err)
	}
	op, err = store.GetOperationByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func remoteCreateSessionRequest(id string) CreateSessionRequest {
	base := "0123456789abcdef0123456789abcdef01234567"
	return CreateSessionRequest{
		ID: id, ExternalRef: "task", Target: "codex:model", Policy: "responder",
		Repository: "/repo", Workspace: "/workspace", ForkName: "remote-fork",
		ForkGeneration: "0123456789abcdef0123456789abcdef",
		BaseCommit:     base,
		RepositoryFreshness: []RepositoryFreshnessReceipt{{
			Version: 2, Name: "primary", RequestedRevision: "HEAD", ResolvedRevision: base,
			WorkspaceBaseRevision: base, RemoteIdentity: "origin", FetchedAt: time.Unix(1, 0).UTC(),
			StaleBaseStatus: "not_applicable",
		}},
		MaxTurns: 3, MaxQueuedTurns: 2, MaxQueuedBytes: 4096,
	}
}
