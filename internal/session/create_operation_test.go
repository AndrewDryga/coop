package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRemoteCreatePersistsPrivateJobAuthorityAndRejectsChangedReplay(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "job-create")
	document := json.RawMessage(`{"job_ref":"job:one","version":1}`)
	sum := sha256.Sum256(document)
	req := CreateSessionRequest{
		ID: "job-session", ExternalRef: "job:one", Target: "codex", Mode: "bare",
		OmitEnv: true, OmitMCP: true, MaxTurns: 1, MaxQueuedTurns: 1, MaxQueuedBytes: 4096,
		JobDocument: document, JobDigest: hex.EncodeToString(sum[:]),
	}
	created, err := store.CompleteCreateSessionOperation(ctx, op, req)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.GetSession(ctx, created.ID)
	if err != nil || !bytes.Equal(stored.JobDocument, document) || stored.JobDigest != req.JobDigest {
		t.Fatalf("stored job authority = %q/%q, err=%v", stored.JobDocument, stored.JobDigest, err)
	}
	public, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(public, document) || bytes.Contains(public, []byte(`"job_document"`)) {
		t.Fatalf("public session leaked private job document: %s", public)
	}
	for _, obsolete := range []string{"policy", "policy_digest", "authority_digest"} {
		if bytes.Contains(public, []byte(`"`+obsolete+`"`)) {
			t.Fatalf("new session still exposes obsolete %s: %s", obsolete, public)
		}
	}
	if stored.JobRef != "job:one" || stored.Policy != "" || stored.PolicyDigest != "" || stored.AuthorityDigest != "" {
		t.Fatalf("new session did not use job-only authority: %+v", stored)
	}
	if _, err := store.CompleteCreateSessionOperation(ctx, op, req); err != nil {
		t.Fatalf("same job did not replay: %v", err)
	}
	changed := req
	changed.JobDocument = json.RawMessage(`{"job_ref":"job:other","version":1}`)
	sum = sha256.Sum256(changed.JobDocument)
	changed.JobDigest = hex.EncodeToString(sum[:])
	if _, err := store.CompleteCreateSessionOperation(ctx, op, changed); CodeOf(err) != CodeOperationIntentConflict {
		t.Fatalf("changed job replay = %v, want intent conflict", err)
	}
	invalid := req
	invalid.JobDigest = strings.Repeat("0", 64)
	if _, err := store.CompleteCreateSessionOperation(ctx, op, invalid); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("mismatched job digest = %v, want invalid request", err)
	}
}

func TestStoredJobIdentityRejectsCorruptionButKeepsHistoricalRowsReadable(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	req := remoteCreateSessionRequest("identity-row")
	created, err := store.CreateSession(ctx, "identity-create", req)
	if err != nil {
		t.Fatal(err)
	}
	for name, document := range map[string]string{
		"missing document": "", "changed document": `{"job_ref":"other","version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.db.Exec("UPDATE sessions SET job_document = ? WHERE id = ?", document, created.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.GetSession(ctx, created.ID); err == nil {
				t.Fatal("corrupt job became a public session identity")
			}
		})
	}
	if _, err := store.db.Exec("UPDATE sessions SET job_document = '', job_digest = '', policy = 'historical', policy_digest = ? WHERE id = ?", strings.Repeat("a", 64), created.ID); err != nil {
		t.Fatal(err)
	}
	historical, err := store.GetSession(ctx, created.ID)
	if err != nil || historical.JobRef != "" || historical.JobDigest != "" || historical.Policy != "historical" {
		t.Fatalf("historical row was reinterpreted: %+v, %v", historical, err)
	}
	if _, err := store.CloseSession(ctx, "historical-close", CloseSessionRequest{SessionID: created.ID, ExpectedRevision: historical.Revision}); err != nil {
		t.Fatalf("historical row cannot be cleaned up: %v", err)
	}
}

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

// A bare create binds a policy and nothing repository-shaped: no fork, no pins, no freshness
// receipt — and it is stored and replayed under exactly that mode. A bare request that carries
// a repository binding, or a normal request that carries none, is refused before any row.
func TestCompleteCreateSessionOperationAcceptsABareSessionWithoutARepository(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "bare-create")
	req := CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest,
		ID: "bare-session", ExternalRef: "route", Target: "claude", Mode: "bare",
		OmitEnv: true, OmitMCP: true, MaxTurns: 1, MaxQueuedTurns: 1, MaxQueuedBytes: 4096,
	}
	sess, err := store.CompleteCreateSessionOperation(ctx, op, req)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Mode != "bare" || sess.Repository != "" || sess.Workspace != "" || sess.ForkName != "" ||
		sess.ForkGeneration != "" || sess.BaseCommit != "" || len(sess.RepositoryFreshness) != 0 {
		t.Fatalf("bare session = %+v", sess)
	}
	stored, err := store.GetSession(ctx, sess.ID)
	if err != nil || stored.Mode != "bare" {
		t.Fatalf("stored bare session = %+v err=%v", stored, err)
	}
	if replayed, err := store.CompleteCreateSessionOperation(ctx, op, req); err != nil || replayed.ID != sess.ID {
		t.Fatalf("bare replay = %+v err=%v", replayed, err)
	}
	// A replay that relabels the mode never gets the bare session back: without a repository it
	// is not even a valid normal request, and the identity check refuses it regardless.
	relabeled := req
	relabeled.Mode = "normal"
	if replayed, err := store.CompleteCreateSessionOperation(ctx, op, relabeled); err == nil || replayed.ID != "" {
		t.Fatalf("a replay under another mode was answered: %+v, %v", replayed, err)
	}
	if !initialSessionMatchesRequest(stored, normalizeCreateRequest(req)) ||
		initialSessionMatchesRequest(stored, normalizeCreateRequest(relabeled)) {
		t.Fatal("the create identity check must bind the mode")
	}

	for name, edit := range map[string]func(*CreateSessionRequest){
		"repository": func(r *CreateSessionRequest) { r.Repository = "/repo" },
		"companion": func(r *CreateSessionRequest) {
			r.Companions = []CompanionRepository{{Name: "docs", Repository: "/d", Workspace: "/w", BaseCommit: "c"}}
		},
		"source": func(r *CreateSessionRequest) {
			binding := testSourceBinding(SourceDefault)
			r.Source = &binding
		},
		"freshness": func(r *CreateSessionRequest) {
			r.RepositoryFreshness = remoteCreateSessionRequest("x").RepositoryFreshness
		},
	} {
		bad := req
		bad.ID = "bare-" + strings.ReplaceAll(name, " ", "-")
		edit(&bad)
		if _, err := store.CompleteCreateSessionOperation(ctx, runningRemoteCreateOperation(t, store, "bare-"+name), bad); CodeOf(err) != CodeInvalidRequest {
			t.Errorf("bare request with a %s was accepted: %v", name, err)
		}
	}
	// The normal shape still needs its receipt: only a bare session has no repository to be fresh about.
	normal := remoteCreateSessionRequest("normal-session")
	normal.RepositoryFreshness = nil
	if _, err := store.CompleteCreateSessionOperation(ctx, runningRemoteCreateOperation(t, store, "normal-create"), normal); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("normal request without freshness was accepted: %v", err)
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
	return CreateSessionRequest{JobDocument: testJobDocument, JobDigest: testJobDigest,
		ID: id, ExternalRef: "task", Target: "codex:model", Repository: "/repo", Workspace: "/workspace", ForkName: "remote-fork",
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

func TestCompleteCreateSessionOperationRefusesMissingJobAuthority(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "outer-create")
	req := remoteCreateSessionRequest("remote-session")
	req.JobDocument, req.JobDigest = nil, ""
	if _, err := store.CompleteCreateSessionOperation(ctx, op, req); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("create without a job = %v, want invalid request", err)
	}
	if _, err := store.GetSession(ctx, req.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("refused create persisted a session: %v", err)
	}
}

func TestCompleteCreateSessionOperationReplaysJobIdentity(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	defer store.Close()
	ctx := context.Background()
	op := runningRemoteCreateOperation(t, store, "outer-create")
	req := remoteCreateSessionRequest("remote-session")
	if _, err := store.CompleteCreateSessionOperation(ctx, op, req); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.CompleteCreateSessionOperation(ctx, op, req)
	if err != nil || replayed.ID != req.ID || replayed.JobRef != "job:test" || replayed.JobDigest != req.JobDigest {
		t.Fatalf("job replay = %+v, %v; want the stored session back", replayed, err)
	}
}
