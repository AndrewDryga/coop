package sessionsvc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestRunnerKeepsCostReportedBeforeProviderFailure(t *testing.T) {
	for _, scenario := range []string{"usage-error", "usage-cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newSessionACPFixture(t, scenario)
			turn := fixture.submit(t, "report then fail")
			got, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
			if err == nil || got.State != session.TurnFailed || !got.Usage.CostRecorded || got.Usage.CostUSD != 0.375 {
				t.Fatalf("failed usage: %+v %v", got, err)
			}
			if scenario == "usage-cancelled" && got.Usage.InputTokens != 1200 {
				t.Fatalf("failed prompt result lost tokens: %+v", got.Usage)
			}
			persisted, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
			if err != nil || persisted.Usage != got.Usage {
				t.Fatalf("stored usage: %+v %v", persisted.Usage, err)
			}
		})
	}
}

func refuseCostPersistence(t *testing.T, store *session.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(store.Root(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER refuse_reported_cost BEFORE UPDATE OF usage_cumulative_cost_usd ON sessions BEGIN SELECT RAISE(ABORT, 'injected cost persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerDoesNotPublishWhenReportedUsageCannotBePersisted(t *testing.T) {
	fixture := newSessionACPFixture(t, "usage")
	turn := fixture.submit(t, "retain reported spend")
	refuseCostPersistence(t, fixture.store)
	_, err := fixture.runner.Run(contextWithTurnDeadline(t), fixture.session, turn)
	if !errors.Is(err, errTurnUsageUnsettled) || !strings.Contains(err.Error(), "injected cost persistence failure") {
		t.Fatalf("unsettled cost error: %v", err)
	}
	got, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
	if err != nil || got.State != session.TurnRunning || got.SendState != session.SendStateSent || got.AssistantMessage != "" || got.Usage.CostRecorded {
		t.Fatalf("unproven usage was terminalized: %+v %v", got, err)
	}
}

func TestServiceCancellationPreservesCheckpointedUsageAndRequestIdentity(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	started := make(chan struct{})
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, func(store *session.Store) Runner {
		return RunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
			if _, err := store.MarkTurnSendIntent(ctx, bound.ID, turn.ID); err != nil {
				return turn, err
			}
			if _, err := store.MarkTurnSent(ctx, bound.ID, turn.ID); err != nil {
				return turn, err
			}
			if _, err := store.RecordTurnUsage(ctx, session.RecordTurnUsageRequest{SessionID: bound.ID, TurnID: turn.ID, Usage: session.Usage{InputTokens: 10}, CumulativeCostUSD: 0.375, CostRecorded: true}); err != nil {
				return turn, err
			}
			close(started)
			<-ctx.Done()
			lookup := ctx.Value(sessionCancelRequestContextKey{}).(func() (string, session.CancelTurnRequest, bool))
			key, req, ok := lookup()
			if !ok {
				return turn, ctx.Err()
			}
			return store.CancelTurn(context.Background(), key, req)
		})
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create", service.request(t, "cancel-usage"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(wait.Deadline):
		t.Fatal("runner did not report usage")
	}
	req := session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}
	for range 2 {
		got, err := service.CancelTurn(context.Background(), "cancel-usage", req)
		if err != nil || got.State != session.TurnCancelled || got.Usage.CostUSD != 0.375 || !got.Usage.CostRecorded || got.Usage.InputTokens != 10 {
			t.Fatalf("cancel/replay: %+v %v", got, err)
		}
	}
	op, err := service.Store().GetOperation(context.Background(), "cancel-usage")
	hash, hashErr := session.CanonicalRequestHash(req)
	if err != nil || hashErr != nil || op.RequestHash != hash {
		t.Fatalf("cancel identity changed: %+v %v %v", op, err, hashErr)
	}
}

func TestServiceDoesNotSettleAnUnpersistedUsageObservation(t *testing.T) {
	for _, cancelRequested := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelRequested), func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			started, release := make(chan struct{}), make(chan struct{})
			var store *session.Store
			service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, func(s *session.Store) Runner {
				store = s
				return RunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
					if _, err := s.MarkTurnSendIntent(ctx, bound.ID, turn.ID); err != nil {
						return turn, err
					}
					if _, err := s.MarkTurnSent(ctx, bound.ID, turn.ID); err != nil {
						return turn, err
					}
					_, err := s.RecordTurnUsage(ctx, session.RecordTurnUsageRequest{SessionID: bound.ID, TurnID: turn.ID, CumulativeCostUSD: 0.375, CostRecorded: true})
					close(started)
					<-release
					return turn, errors.Join(err, errTurnUsageUnsettled)
				})
			})
			defer service.Stop()
			refuseCostPersistence(t, store)
			if err := service.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			sess, err := service.CreateRemoteSession(context.Background(), "create", service.request(t, "unsettled"))
			if err != nil {
				t.Fatal(err)
			}
			turn, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "observe"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(wait.Deadline):
				t.Fatal("runner did not reach usage checkpoint")
			}
			service.mu.Lock()
			done := service.active[turn.ID].done
			service.mu.Unlock()
			cancelled := make(chan error, 1)
			if cancelRequested {
				go func() {
					_, err := service.CancelTurn(context.Background(), "cancel", session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision})
					cancelled <- err
				}()
				waitForSessionTest(t, func() bool {
					service.mu.Lock()
					defer service.mu.Unlock()
					a := service.active[turn.ID]
					return a != nil && a.requested
				})
			}
			close(release)
			select {
			case <-done:
			case <-time.After(wait.Deadline):
				t.Fatal("runner did not finish")
			}
			if cancelRequested {
				select {
				case err := <-cancelled:
					if err == nil {
						t.Fatal("unsettled cancellation reported success")
					}
				case <-time.After(wait.Deadline):
					t.Fatal("cancellation did not return")
				}
				op, err := store.GetOperation(context.Background(), "cancel")
				if err != nil || op.State != session.OperationUncertain {
					t.Fatalf("cancel operation: %+v %v", op, err)
				}
			}
			got, err := store.GetTurn(context.Background(), sess.ID, turn.ID)
			if err != nil || got.State != session.TurnRunning || got.SendState != session.SendStateSent || got.Usage.CostRecorded {
				t.Fatalf("generic fallback discarded usage uncertainty: %+v %v", got, err)
			}
		})
	}
}

func TestActualRunnerCancellationKeepsReservedRequestIdentity(t *testing.T) {
	fixture := newSessionACPFixture(t, "invalid-contract-once-then-hang")
	turn := fixture.submitContract(t, "repair then cancel")
	req := session.CancelTurnRequest{SessionID: fixture.session.ID, TurnID: turn.ID, ExpectedRevision: fixture.session.Revision}
	op, _, err := fixture.store.ReserveOperation(context.Background(), "CancelTurn", "reserved-cancel", req)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MarkOperationRunning(context.Background(), op.ID, intent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(contextWithTurnDeadline(t))
	defer cancel()
	ctx = context.WithValue(ctx, sessionCancelRequestContextKey{}, func() (string, session.CancelTurnRequest, bool) { return "reserved-cancel", req, true })
	done := make(chan error, 1)
	go func() { _, err := fixture.runner.Run(ctx, fixture.session, turn); done <- err }()
	waitForSessionTest(t, func() bool {
		got, err := fixture.store.GetTurn(context.Background(), fixture.session.ID, turn.ID)
		return err == nil && got.Usage.CostRecorded && got.Usage.CostUSD == 0.25
	})
	if err := fixture.store.MarkOperationUncertain(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("actual runner cancellation: %v", err)
		}
	case <-time.After(wait.Deadline):
		t.Fatal("runner did not settle cancellation")
	}
	got, err := fixture.store.CancelTurn(context.Background(), "reserved-cancel", req)
	if err != nil || got.State != session.TurnCancelled || got.Usage.CostUSD != 0.25 || got.Usage.InputTokens != 10 {
		t.Fatalf("reserved cancel replay: %+v %v", got, err)
	}
}
