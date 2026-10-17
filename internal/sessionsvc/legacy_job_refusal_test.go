package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestCreateIntentCannotSilentlyDropRetiredControllerBinding(t *testing.T) {
	for name, corrupt := range map[string]func([]byte) []byte{
		"retired binding": func(data []byte) []byte {
			return []byte(strings.TrimSuffix(string(data), "}") + `,"responder_binding":{"endpoint":"https://controller.example/tools","token":"old-private-token"}}`)
		},
		"trailing document": func(data []byte) []byte { return append(data, []byte(` {"controller_tools":{}}`)...) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", nil)
			defer service.Stop()
			request := service.request(t, "old-controller-intent")
			op, _, err := service.Store().ReserveOperation(ctx, "CreateRemoteSession", "old-controller", request)
			if err != nil {
				t.Fatal(err)
			}
			intent, err := service.captureCreateIntent(ctx, op, request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Store().MarkOperationRunning(ctx, op.ID, corrupt(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := service.CreateRemoteSession(ctx, "old-controller", request); session.CodeOf(err) != session.CodeOperationUncertain {
				t.Fatalf("unproven controller binding replay = %v", err)
			}
			if _, err := service.Store().GetSession(ctx, intent.SessionID); !errors.Is(err, session.ErrSessionNotFound) {
				t.Fatalf("unreadable intent created a session: %v", err)
			}
		})
	}
}

func TestJoblessHistoricalSessionRemainsReadableButCannotExecute(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", func(*session.Store) Runner {
		return RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			runs.Add(1)
			return turn, nil
		})
	})
	defer fixture.Stop()
	legacy, err := fixture.Store().CreateSession(ctx, "legacy-row", session.CreateSessionRequest{JobDocument: storedTestJobDocument, JobDigest: storedTestJobDigest,
		Target: "codex", Mode: "bare", OmitEnv: true, OmitMCP: true, NetworkMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy = clearHistoricalJob(t, fixture.Store(), legacy.ID)
	queued, err := fixture.Store().SubmitTurn(ctx, "legacy-queue", session.SubmitTurnRequest{
		SessionID: legacy.ID, ExpectedRevision: legacy.Revision, Prompt: "old queued work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Start(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.GetSession(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.SubmitTurn(ctx, "new-legacy-turn", session.SubmitTurnRequest{
		SessionID: current.ID, ExpectedRevision: current.Revision, Prompt: "new work",
	}); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("jobless turn admission = %v", err)
	}
	if _, err := fixture.PrepareSession(ctx, current.ID, current.Revision); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("jobless prepare = %v", err)
	}
	fixture.mu.Lock()
	workers := len(fixture.workers)
	fixture.mu.Unlock()
	turn, err := fixture.GetTurn(ctx, current.ID, queued.ID)
	if err != nil || turn.State != session.TurnQueued || workers != 0 || runs.Load() != 0 {
		t.Fatalf("jobless restart executed work: turn=%+v workers=%d runs=%d err=%v", turn, workers, runs.Load(), err)
	}
}

func TestVersionOneHistoricalSessionRemainsReadableButCannotExecute(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", func(*session.Store) Runner {
		return RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			runs.Add(1)
			return turn, nil
		})
	})
	defer fixture.Stop()
	old, err := fixture.Store().CreateSession(ctx, "version-one-row", session.CreateSessionRequest{
		JobDocument: storedTestJobDocument, JobDigest: storedTestJobDigest,
		Target: "codex", Mode: "bare", OmitEnv: true, OmitMCP: true, NetworkMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := fixture.Store().SubmitTurn(ctx, "version-one-queue", session.SubmitTurnRequest{
		SessionID: old.ID, ExpectedRevision: old.Revision, Prompt: "historical work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Start(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.GetSession(ctx, old.ID)
	if err != nil || current.ID != old.ID || current.JobDigest != storedTestJobDigest {
		t.Fatalf("historical inspection = %+v, %v", current, err)
	}
	if _, err := fixture.SubmitTurn(ctx, "version-one-new-turn", session.SubmitTurnRequest{
		SessionID: current.ID, ExpectedRevision: current.Revision, Prompt: "new work",
	}); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("version-one turn admission = %v", err)
	}
	if _, err := fixture.PrepareSession(ctx, current.ID, current.Revision); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("version-one prepare = %v", err)
	}
	turn, err := fixture.GetTurn(ctx, current.ID, queued.ID)
	if err != nil || turn.State != session.TurnQueued || runs.Load() != 0 {
		t.Fatalf("version-one restart executed work: %+v, runs=%d, err=%v", turn, runs.Load(), err)
	}
}

// A session created before the readonly and bare modes were removed keeps its record, but its job
// no longer decodes: a restart starts no worker for it, its queued turn stays queued, and a new
// turn is refused with the reason rather than run as a normal session.
func TestSessionInARemovedModeRemainsReadableButCannotExecute(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", func(*session.Store) Runner {
		return RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			runs.Add(1)
			return turn, nil
		})
	})
	defer fixture.Stop()
	// The job as the previous version froze it: canonical sorted-key JSON, which Digest now refuses.
	job := emptyWorkerJob()
	job.Mode = "bare"
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(document)
	old, err := fixture.Store().CreateSession(ctx, "removed-mode-row", session.CreateSessionRequest{
		JobDocument: document, JobDigest: hex.EncodeToString(sum[:]),
		Target: "codex", Mode: "bare", OmitEnv: true, OmitMCP: true, NetworkMode: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := fixture.Store().SubmitTurn(ctx, "removed-mode-queue", session.SubmitTurnRequest{
		SessionID: old.ID, ExpectedRevision: old.Revision, Prompt: "old queued work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Start(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.GetSession(ctx, old.ID)
	if err != nil || current.Mode != "bare" {
		t.Fatalf("removed-mode inspection = %+v, %v", current, err)
	}
	if _, err := fixture.SubmitTurn(ctx, "removed-mode-new-turn", session.SubmitTurnRequest{
		SessionID: current.ID, ExpectedRevision: current.Revision, Prompt: "new work",
	}); session.CodeOf(err) != session.CodeInvalidSessionState || !strings.Contains(err.Error(), `job mode "bare" was removed`) {
		t.Fatalf("removed-mode turn admission = %v", err)
	}
	fixture.mu.Lock()
	workers := len(fixture.workers)
	fixture.mu.Unlock()
	turn, err := fixture.GetTurn(ctx, current.ID, queued.ID)
	if err != nil || turn.State != session.TurnQueued || workers != 0 || runs.Load() != 0 {
		t.Fatalf("removed-mode restart executed work: turn=%+v workers=%d runs=%d err=%v", turn, workers, runs.Load(), err)
	}
	// What the docs tell its owner to do still works, with no job authority: cancel the queued
	// turn, close the session, then plan and apply its discard.
	if _, err := fixture.CancelTurn(ctx, "removed-mode-cancel", session.CancelTurnRequest{
		SessionID: current.ID, TurnID: queued.ID, ExpectedRevision: current.Revision,
	}); err != nil {
		t.Fatalf("cancel the queued turn: %v", err)
	}
	if current, err = fixture.GetSession(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := fixture.CloseSession(ctx, "removed-mode-close", session.CloseSessionRequest{
		SessionID: current.ID, ExpectedRevision: current.Revision,
	})
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	plan, err := fixture.PlanDiscard(ctx, "removed-mode-plan", PlanDiscardRequest{SessionID: closed.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatalf("discard plan: %v", err)
	}
	if discarded, err := fixture.Discard(ctx, "removed-mode-discard", DiscardRequest{PlanOperationID: plan.OperationID}); err != nil ||
		discarded.State != session.SessionDiscarded || runs.Load() != 0 {
		t.Fatalf("discard = %+v, %v (runs=%d)", discarded, err, runs.Load())
	}
}

func TestRecoveredReviewCannotExecuteAJoblessHistoricalSession(t *testing.T) {
	ctx := context.Background()
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer fixture.Stop()
	bound, err := fixture.CreateRemoteSession(ctx, "legacy-review-create", fixture.request(t, "legacy-review"))
	if err != nil {
		t.Fatal(err)
	}
	jobDigest := bound.JobDigest
	bound = clearHistoricalJob(t, fixture.Store(), bound.ID)
	request := RunReviewRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision}
	op, _, err := fixture.Store().ReserveOperation(ctx, "RunReview", "legacy-review-operation", request)
	if err != nil {
		t.Fatal(err)
	}
	tree := gitOut(bound.Workspace, "rev-parse", "HEAD^{tree}")
	intent := sessionReviewIntent{
		OperationID: op.ID, SessionID: bound.ID, SessionRevision: bound.Revision,
		JobDigest:  jobDigest,
		Repository: bound.Repository, Workspace: bound.Workspace, SourceBranch: bound.ForkName,
		ForkGeneration: bound.ForkGeneration, CreationBase: bound.BaseCommit,
		SourceHead: bound.BaseCommit, SourceTree: tree, ParentHead: bound.BaseCommit, ParentTree: tree,
		MaxPatchBytes: bound.MaxPatchBytes,
	}
	document, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.Store().MarkOperationRunning(ctx, op.ID, document); err != nil {
		t.Fatal(err)
	}
	fixture.reviewGate = ReviewGateFunc(func(context.Context, ReviewGateRequest) (ReviewGateResult, error) {
		t.Error("historical review executed a gate without job authority")
		return ReviewGateResult{}, nil
	})
	if _, err := fixture.executeReviewIntent(ctx, op, intent); session.CodeOf(err) != session.CodeOperationUncertain {
		t.Fatalf("historical review replay = %v", err)
	}
	if head := gitOut(bound.Workspace, "rev-parse", "HEAD"); head != bound.BaseCommit {
		t.Fatalf("historical review mutated workspace: %s", head)
	}
}
