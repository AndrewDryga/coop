package sessionsvc

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

// A stored session this build no longer reads is quarantined at start, refused by name, and
// changed in no way: no worker, its queued turn stays queued, the ordinary discard is refused, and
// only the explicit retire_quarantined discard tombstones the record.
func TestOldSessionRowsAreQuarantinedByNameAndLeftAlone(t *testing.T) {
	removedModeJob := func() ([]byte, string) {
		job := emptyWorkerJob()
		job.Mode = "bare"
		raw, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		// Canonical sorted-key JSON, as the job was frozen, which Digest now refuses.
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		document, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(document)
		return document, hex.EncodeToString(sum[:])
	}
	cases := []struct {
		name, want string
		create     func(t *testing.T, store *session.Store) session.Session
	}{
		{"no saved job", "has no saved job", func(t *testing.T, store *session.Store) session.Session {
			created, err := store.CreateSession(context.Background(), "old-row", session.CreateSessionRequest{
				JobDocument: storedTestJobDocument, JobDigest: storedTestJobDigest, Target: "codex", OmitEnv: true, OmitMCP: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			return clearHistoricalJob(t, store, created.ID)
		}},
		{"removed bare mode", `was created in the removed "bare" mode`, func(t *testing.T, store *session.Store) session.Session {
			document, digest := removedModeJob()
			created, err := store.CreateSession(context.Background(), "old-row", session.CreateSessionRequest{
				JobDocument: document, JobDigest: digest, Target: "codex", Mode: "bare", OmitEnv: true, OmitMCP: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			return created
		}},
		{"blank mode", "has no execution mode", func(t *testing.T, store *session.Store) session.Session {
			created, err := store.CreateSession(context.Background(), "old-row", session.CreateSessionRequest{
				JobDocument: storedTestJobDocument, JobDigest: storedTestJobDigest, Target: "codex", OmitEnv: true, OmitMCP: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			return setHistoricalColumn(t, store, created.ID, "mode")
		}},
		{"blank network mode", "has no network mode", func(t *testing.T, store *session.Store) session.Session {
			created, err := store.CreateSession(context.Background(), "old-row", session.CreateSessionRequest{
				JobDocument: storedTestJobDocument, JobDigest: storedTestJobDigest, Target: "codex", OmitEnv: true, OmitMCP: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			return setHistoricalColumn(t, store, created.ID, "network_mode")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var runs atomic.Int32
			fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", func(*session.Store) Runner {
				return RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
					runs.Add(1)
					return turn, nil
				})
			})
			defer fixture.Stop()
			old := tc.create(t, fixture.Store())
			queued, err := fixture.Store().SubmitTurn(ctx, "old-queue", session.SubmitTurnRequest{
				SessionID: old.ID, ExpectedRevision: old.Revision, Prompt: "old queued work",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := oldSessionRowRefusal(old); err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), old.ID) || !strings.Contains(err.Error(), "29133b0e") {
				t.Fatalf("refusal = %v, want it to name the session, %q and the converting build", err, tc.want)
			}
			if err := fixture.Start(ctx); err != nil {
				t.Fatal(err)
			}
			if !fixture.sessionQuarantined(old.ID) {
				t.Fatal("old session row was not quarantined")
			}
			current, err := fixture.GetSession(ctx, old.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.SubmitTurn(ctx, "old-new-turn", session.SubmitTurnRequest{
				SessionID: current.ID, ExpectedRevision: current.Revision, Prompt: "new work",
			}); session.CodeOf(err) != session.CodeInvalidSessionState {
				t.Fatalf("turn admission on a quarantined old row = %v", err)
			}
			fixture.mu.Lock()
			workers := len(fixture.workers)
			fixture.mu.Unlock()
			turn, err := fixture.GetTurn(ctx, current.ID, queued.ID)
			if err != nil || turn.State != session.TurnQueued || workers != 0 || runs.Load() != 0 {
				t.Fatalf("old row ran work: turn=%+v workers=%d runs=%d err=%v", turn, workers, runs.Load(), err)
			}
			if _, err := fixture.PlanDiscard(ctx, "old-plan", PlanDiscardRequest{SessionID: current.ID, ExpectedRevision: current.Revision}); session.CodeOf(err) != session.CodeInvalidSessionState ||
				!strings.Contains(err.Error(), "quarantined") {
				t.Fatalf("ordinary discard of a quarantined old row = %v", err)
			}
			stalePlan, _, err := fixture.Store().ReserveOperation(ctx, "Discard", "old-stale-plan", DiscardRequest{PlanOperationID: "plan-from-before-quarantine"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.executeDiscard(ctx, stalePlan, PlanDiscardResult{Plan: DiscardPlan{SessionID: current.ID, Revision: current.Revision}}); session.CodeOf(err) != session.CodeInvalidSessionState ||
				!strings.Contains(err.Error(), "quarantined") {
				t.Fatalf("discard by a plan recorded before quarantine = %v", err)
			}
			retired, err := fixture.Discard(ctx, "old-retire", DiscardRequest{RetireQuarantined: true, SessionID: current.ID, ExpectedRevision: current.Revision})
			if err != nil || retired.State != session.SessionDiscarded || runs.Load() != 0 {
				t.Fatalf("retire = %+v, %v (runs=%d)", retired, err, runs.Load())
			}
		})
	}
}

// setHistoricalColumn blanks one text column of a stored session, the shape an older build left.
func setHistoricalColumn(t *testing.T, store *session.Store, id, column string) session.Session {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(store.Root(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE sessions SET "+column+" = '' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return value
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
