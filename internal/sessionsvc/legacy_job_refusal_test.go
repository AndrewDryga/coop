package sessionsvc

import (
	"context"
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
	fixture.reviewGate = ReviewGateFunc(func(context.Context, string, string) (ReviewGateResult, error) {
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
