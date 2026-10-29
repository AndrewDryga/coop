package sessionsvc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

type quarantineCloseRunner struct {
	proof            func(session.Session) error
	proofs, ordinary int
}

func (r *quarantineCloseRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	return turn, errors.New("must not run")
}
func (r *quarantineCloseRunner) CleanupUnprovenSessionRuntime(_ context.Context, bound session.Session) error {
	r.proofs++
	if r.proof != nil {
		return r.proof(bound)
	}
	return nil
}
func (r *quarantineCloseRunner) CleanupClosedSession(context.Context, session.Session) error {
	r.ordinary++
	return errors.New("ordinary cleanup must not touch an unproven workspace")
}

func quarantinedCloseFixture(t *testing.T, runner Runner) (*sessionFixture, session.Session) {
	t.Helper()
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, func(*session.Store) Runner { return runner })
	t.Cleanup(func() { _ = service.Stop() })
	bound, _ := createLegacyBoundSession(t, service, repo, "retained-close", "retained-close-session", "")
	// The startup quarantine decision is already established; keep its janitor out
	// of this deterministic close test so only the close owns the runtime proof.
	service.quarantined[bound.ID] = struct{}{}
	service.historicalPending[bound.ID] = struct{}{}
	service.runtimeCleanupFailed(bound.ID)
	return service, bound
}

// A production stop spent seventy attempts behind workspace validation even
// though stopping its private runtime needs no authority over that workspace.
func TestQuarantinedCloseRequiresExactRuntimeProofAndPreservesWorkspace(t *testing.T) {
	runner := &quarantineCloseRunner{}
	service, bound := quarantinedCloseFixture(t, runner)
	path := filepath.Join(bound.Workspace, "unpublished.txt")
	if err := os.WriteFile(path, []byte("retained candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	req := session.CloseSessionRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision}
	closed, err := service.Close(context.Background(), "close-retained", req)
	if err != nil || closed.State != session.SessionClosed {
		t.Fatalf("close = %+v, %v", closed, err)
	}
	if runner.proofs != 1 || runner.ordinary != 0 {
		t.Fatalf("proofs=%d ordinary=%d", runner.proofs, runner.ordinary)
	}
	if !service.sessionQuarantined(bound.ID) || service.historicalRuntimeNeedsCleanup(bound.ID) || service.runtimeCleanupPending(bound.ID) {
		t.Fatal("close must retain quarantine and release only proven runtime custody")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "retained candidate" {
		t.Fatalf("unpublished work changed: %q, %v", body, err)
	}
	runner.proof = func(session.Session) error { return errors.New("later inventory failure") }
	again, err := service.Close(context.Background(), "close-retained", req)
	if err != nil || again.ID != closed.ID || again.Revision != closed.Revision {
		t.Fatalf("saved close replay = %+v, %v", again, err)
	}
	if runner.proofs != 1 {
		t.Fatal("receipt replay ran cleanup again")
	}
}

func TestQuarantinedCloseRefusesMissingOrFailedProofAndBusyOrStaleState(t *testing.T) {
	for _, kind := range []string{"missing-proof", "failed-proof", "queued", "active", "stale", "not-quarantined", "racing-admission"} {
		t.Run(kind, func(t *testing.T) {
			runner := &quarantineCloseRunner{}
			var actual Runner = runner
			if kind == "missing-proof" {
				actual = RunnerFunc(runner.Run)
			}
			service, bound := quarantinedCloseFixture(t, actual)
			if kind == "failed-proof" {
				runner.proof = func(session.Session) error { return errors.New("inventory unavailable") }
			}
			if kind == "not-quarantined" {
				delete(service.quarantined, bound.ID)
			}
			submit := func(current session.Session) error {
				_, err := service.Store().SubmitTurn(context.Background(), "racing-turn", session.SubmitTurnRequest{SessionID: current.ID, ExpectedRevision: current.Revision, Prompt: "preserved queue"})
				return err
			}
			if kind == "queued" || kind == "active" {
				if err := submit(bound); err != nil {
					t.Fatal(err)
				}
				if kind == "active" {
					if _, ok, err := service.Store().LeaseNextTurn(context.Background(), bound.ID); err != nil || !ok {
						t.Fatalf("lease active fixture: ok=%v, %v", ok, err)
					}
				}
				bound = mustSession(t, service, bound.ID)
			}
			if kind == "racing-admission" {
				runner.proof = submit
			}
			revision := bound.Revision
			if kind == "stale" {
				revision++
			}
			_, err := service.Close(context.Background(), "refused-close", session.CloseSessionRequest{SessionID: bound.ID, ExpectedRevision: revision})
			if err == nil {
				t.Fatal("unproven or busy close succeeded")
			}
			current := mustSession(t, service, bound.ID)
			if current.State == session.SessionClosed {
				t.Fatal("refused close terminalized the session")
			}
			if _, err := os.Stat(bound.Workspace); err != nil {
				t.Fatalf("workspace changed: %v", err)
			}
			if runner.ordinary != 0 {
				t.Fatal("ordinary workspace cleanup ran")
			}
			if kind != "failed-proof" && kind != "racing-admission" && runner.proofs != 0 {
				t.Fatal("invalid request reached runtime cleanup")
			}
			if kind == "failed-proof" && !service.runtimeCleanupPending(bound.ID) {
				t.Fatal("failed proof released runtime custody")
			}
		})
	}
}

func TestClosedReceiptSurvivesLaterWorkspaceLoss(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer service.Stop()
	bound, err := service.CreateRemoteSession(context.Background(), "create-replay-close", service.request(t, "replay-close"))
	if err != nil {
		t.Fatal(err)
	}
	req := session.CloseSessionRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision}
	closed, err := service.Close(context.Background(), "saved-close", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(bound.Workspace); err != nil {
		t.Fatal(err)
	}
	again, err := service.Close(context.Background(), "saved-close", req)
	if err != nil || again.ID != closed.ID || again.Revision != closed.Revision {
		t.Fatalf("immutable close receipt = %+v, %v", again, err)
	}
	if _, err := service.Close(context.Background(), "new-close", session.CloseSessionRequest{SessionID: bound.ID, ExpectedRevision: closed.Revision}); err == nil {
		t.Fatal("new close of terminal session succeeded")
	}
}

func TestQuarantinedExhaustedSessionClosesAfterRuntimeProof(t *testing.T) {
	runner := &quarantineCloseRunner{}
	service, bound := quarantinedCloseFixture(t, runner)
	exhausted, err := service.Store().ExhaustBudget(context.Background(), "exhaust-retained", session.ExhaustBudgetRequest{SessionID: bound.ID, ExpectedRevision: bound.Revision})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(context.Background(), "close-exhausted-retained", session.CloseSessionRequest{SessionID: bound.ID, ExpectedRevision: exhausted.Revision})
	if err != nil || closed.State != session.SessionClosed || runner.proofs != 1 {
		t.Fatalf("exhausted close = %+v, %v; proofs=%d", closed, err, runner.proofs)
	}
}
