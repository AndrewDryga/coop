package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
)

type semanticSchedulingRunner struct {
	store   *session.Store
	staged  chan session.Turn
	release chan struct{}
	active  atomic.Int32
	overlap atomic.Bool
}

func (*semanticSchedulingRunner) ReapInterruptedTurn(context.Context, session.Session, session.Turn) error {
	return nil
}

func (r *semanticSchedulingRunner) Run(ctx context.Context, sess session.Session, turn session.Turn) (session.Turn, error) {
	if r.active.Add(1) != 1 {
		r.overlap.Store(true)
	}
	defer r.active.Add(-1)
	if _, err := r.store.MarkTurnSendIntent(ctx, sess.ID, turn.ID); err != nil {
		return turn, err
	}
	if _, err := r.store.MarkTurnSent(ctx, sess.ID, turn.ID); err != nil {
		return turn, err
	}
	if turn.OutputContract == nil {
		return r.store.CompleteTurn(ctx, session.CompleteTurnRequest{SessionID: sess.ID, TurnID: turn.ID, Message: "successor"})
	}
	message := `{"reply":"candidate"}`
	digest := sha256.Sum256([]byte(message))
	staged, err := r.store.StageTurnCandidate(ctx, session.StageTurnCandidateRequest{
		SessionID: sess.ID, TurnID: turn.ID, Message: message,
		SHA256: fmt.Sprintf("%x", digest), Attempt: session.MaxOutputContractAttempts,
	})
	if err != nil {
		return turn, err
	}
	r.staged <- staged
	select {
	case <-r.release:
		return staged, nil
	case <-ctx.Done():
		return staged, ctx.Err()
	}
}

func TestSemanticTerminalDecisionsResumeQueuedTurnsWithinBudget(t *testing.T) {
	for _, verdict := range []string{"accept", "reject"} {
		for _, workerState := range []string{"exited", "unwinding"} {
			for _, maxTurns := range []int{1, 3} {
				t.Run(fmt.Sprintf("%s/%s/budget-%d", verdict, workerState, maxTurns), func(t *testing.T) {
					ctx := context.Background()
					runner := &semanticSchedulingRunner{staged: make(chan session.Turn, 1), release: make(chan struct{})}
					service, err := NewService(Config{
						StateRoot: filepath.Join(t.TempDir(), "state"), SourceConfig: &config.Config{},
						Policies: map[string]Policy{"fixture": {Name: "fixture"}}, Runner: runner,
						CleanupInterval: time.Hour,
					})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = service.Stop() })
					runner.store = service.Store()
					if err := service.Start(ctx); err != nil {
						t.Fatal(err)
					}
					sess, err := service.Store().CreateSession(ctx, "create", session.CreateSessionRequest{
						Target: "codex:test", MaxTurns: maxTurns, MaxQueuedTurns: 3, MaxQueuedBytes: 4096,
					})
					if err != nil {
						t.Fatal(err)
					}
					schema := json.RawMessage(`{"type":"object"}`)
					digest := sha256.Sum256(schema)
					var turns []session.Turn
					for index := range 3 {
						req := session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: fmt.Sprintf("turn %d", index)}
						if index == 0 {
							req.OutputContract = &session.OutputContract{
								JSONSchema: schema, SHA256: fmt.Sprintf("%x", digest), RequireSemanticValidation: true,
							}
						}
						turn, err := service.Store().SubmitTurn(ctx, fmt.Sprintf("submit-%d", index), req)
						if err != nil {
							t.Fatal(err)
						}
						turns = append(turns, turn)
					}
					service.mu.Lock()
					worker := service.ensureWorkerLocked(sess.ID)
					service.triggerWorker(worker)
					service.mu.Unlock()
					var candidate session.Turn
					select {
					case candidate = <-runner.staged:
					case <-time.After(3 * time.Second):
						t.Fatal("candidate was not staged")
					}
					decide := func() (session.Turn, error) {
						if verdict == "accept" {
							return service.AcceptTurnCandidate(ctx, "decide", sess.ID, candidate.ID, candidate.CandidateSHA256)
						}
						return service.RejectTurnCandidate(ctx, "decide", session.RejectTurnCandidateRequest{
							SessionID: sess.ID, TurnID: candidate.ID, CandidateSHA256: candidate.CandidateSHA256,
							Violations: []string{"final candidate rejected"},
						})
					}
					decision := make(chan error, 1)
					if workerState == "exited" {
						close(runner.release)
						select {
						case <-worker.done:
						case <-time.After(3 * time.Second):
							t.Fatal("awaiting-candidate worker did not exit")
						}
						_, err := decide()
						decision <- err
					} else {
						func() {
							service.mu.Lock()
							defer service.mu.Unlock()
							close(runner.release)
							go func() { _, err := decide(); decision <- err }()
							// The old worker cannot finish its handoff and scheduling cannot
							// return until this mutex is released. Runtime cleanup and the
							// durable decision must still complete independently of it.
							waitForSessionTest(t, func() bool {
								turn, _ := service.GetTurn(ctx, sess.ID, candidate.ID)
								return turn.State == session.TurnCompleted || turn.State == session.TurnFailed
							})
						}()
					}
					select {
					case err := <-decision:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("semantic decision did not return")
					}
					waitForSessionTest(t, func() bool {
						last, _ := service.GetTurn(ctx, sess.ID, turns[2].ID)
						return last.State == session.TurnCompleted || last.State == session.TurnBudgetExhausted
					})
					waitForSessionTest(t, func() bool {
						service.mu.Lock()
						defer service.mu.Unlock()
						return len(service.workers) == 0
					})
					before := mustSession(t, service, sess.ID)
					for range 2 {
						if _, err := decide(); err != nil {
							t.Fatalf("decision replay: %v", err)
						}
					}
					waitForSessionTest(t, func() bool {
						service.mu.Lock()
						defer service.mu.Unlock()
						return len(service.workers) == 0
					})
					if after := mustSession(t, service, sess.ID); before.Revision != after.Revision || before.LastEventSequence != after.LastEventSequence ||
						after.TurnsUsed != maxTurns || after.State != session.SessionExhausted || after.QueuedTurnCount != 0 || after.QueuedPromptBytes != 0 {
						t.Fatalf("terminal/replayed session = %+v; before=%+v", after, before)
					}
					events, err := service.Store().ListEvents(ctx, sess.ID, 0, 100)
					if err != nil {
						t.Fatal(err)
					}
					for _, successor := range turns[1:] {
						starts := 0
						for _, event := range events {
							if event.TurnID == successor.ID && event.Type == session.EventTurnStarted {
								starts++
							}
						}
						wantState, wantStarts := session.TurnCompleted, 1
						if maxTurns == 1 {
							wantState, wantStarts = session.TurnBudgetExhausted, 0
						}
						got, err := service.GetTurn(ctx, sess.ID, successor.ID)
						if err != nil || got.State != wantState || starts != wantStarts {
							t.Fatalf("successor = %+v, starts=%d, err=%v; want %s/%d", got, starts, err, wantState, wantStarts)
						}
					}
					if runner.overlap.Load() {
						t.Fatal("session turns ran concurrently")
					}
				})
			}
		}
	}
}

func TestSessionServiceDoesNotStartSuccessorWhenAcceptedCandidateRuntimeReapFails(t *testing.T) {
	ctx := context.Background()
	runner := &candidateDecisionCleanupRunner{runStarted: make(chan struct{})}
	runner.setReapError(acpFailure(sessionACPCleanupError, "runtime inventory unavailable"))
	service, sess, candidate := newCandidateDecisionService(t, "failed-accept-candidate", runner)
	successor, err := service.Store().SubmitTurn(ctx, "queued-successor", session.SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "queued behind validation",
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := service.AcceptTurnCandidate(ctx, "failed-accept", sess.ID, candidate.ID, candidate.CandidateSHA256); err == nil {
			t.Fatal("accepted candidate despite failed runtime cleanup")
		}
		got, err := service.GetTurn(ctx, sess.ID, candidate.ID)
		if err != nil || got.State != session.TurnAwaitingValidation || got.CandidateSHA256 != candidate.CandidateSHA256 {
			t.Fatalf("failed acceptance changed candidate: %+v, %v", got, err)
		}
		service.mu.Lock()
		workers := len(service.workers)
		service.mu.Unlock()
		if workers != 0 {
			t.Fatalf("failed acceptance scheduled %d workers", workers)
		}
		select {
		case <-runner.runStarted:
			t.Fatal("successor ran despite cleanup refusal")
		default:
		}
		got, err = service.GetTurn(ctx, sess.ID, successor.ID)
		if err != nil || got.State != session.TurnQueued || !got.StartedAt.IsZero() {
			t.Fatalf("failed acceptance changed successor: %+v, %v", got, err)
		}
	}
	if runner.reapedCount() != 1 {
		t.Fatal("failed-key replay repeated cleanup")
	}
	runner.setReapError(nil)
	if _, err := service.AcceptTurnCandidate(ctx, "accept-after-recovery", sess.ID, candidate.ID, candidate.CandidateSHA256); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.runStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("successor did not run after successful cleanup and acceptance")
	}
}
