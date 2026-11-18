package sessionsvc

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// failOperationReceipt makes the next receipt of method fail, the way a crash or a full disk
// loses it after the effect committed. The returned func drops the trigger.
func failOperationReceipt(t *testing.T, service interface{ Store() *session.Store }, method string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(service.Store().Root(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER lose_receipt BEFORE UPDATE ON operations
		WHEN NEW.method = '` + method + `' AND NEW.state = 'succeeded'
		BEGIN SELECT RAISE(ABORT, 'injected receipt loss'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := db.Exec("DROP TRIGGER lose_receipt"); err != nil {
			t.Fatal(err)
		}
	}
}

// A controller that lost an accept or reject receipt used to get "turn does not await semantic
// validation" on its retry, a failure for a decision that had taken effect.
func TestSemanticDecisionRetryReturnsTheAppliedTurnAfterALostReceipt(t *testing.T) {
	for _, verdict := range []string{"accept", "reject"} {
		t.Run(verdict, func(t *testing.T) {
			ctx := context.Background()
			runner := &candidateDecisionCleanupRunner{runStarted: make(chan struct{})}
			service, sess, turn := newCandidateDecisionService(t, verdict+"-lost-receipt", runner)
			digest := turn.Candidate.SHA256
			decide := func(key, verdict, digest string) (session.Turn, error) {
				if verdict == "accept" {
					return service.AcceptTurnCandidate(ctx, key, sess.ID, turn.ID, digest)
				}
				return service.RejectTurnCandidate(ctx, key, session.RejectTurnCandidateRequest{
					SessionID: sess.ID, TurnID: turn.ID, CandidateSHA256: digest, Violations: []string{"needs repair"},
				})
			}
			settled := func(got session.Turn) bool {
				if verdict == "accept" {
					return got.State == session.TurnCompleted && got.SemanticAcceptanceApplied(digest)
				}
				return got.SemanticRejectionApplied(digest)
			}
			restore := failOperationReceipt(t, service, "ValidateTurnCandidate")
			if _, err := decide("decide", verdict, digest); err == nil || !strings.Contains(err.Error(), "injected receipt loss") {
				t.Fatalf("receipt loss was not exercised: %v", err)
			}
			op, err := service.Store().GetOperation(ctx, "decide")
			if err != nil || op.State != session.OperationRunning {
				t.Fatalf("a decision that may have applied must be recorded as running: %+v, %v", op, err)
			}
			restore()
			got, err := decide("decide", verdict, digest)
			if err != nil || got.ID != turn.ID || !settled(got) {
				t.Fatalf("same-key retry = %+v, %v; want the settled turn", got, err)
			}
			if op, err := service.Store().GetOperation(ctx, "decide"); err != nil || op.State != session.OperationSucceeded {
				t.Fatalf("retry did not settle the receipt: %+v, %v", op, err)
			}
			if got, err := decide("decide-again", verdict, digest); err != nil || !settled(got) {
				t.Fatalf("new-key repeat = %+v, %v; want the settled turn", got, err)
			}
			other := "accept"
			if verdict == "accept" {
				other = "reject"
			}
			if _, err := decide("other-verdict", other, digest); err == nil {
				t.Fatalf("a turn settled by %s accepted a %s", verdict, other)
			}
			if _, err := decide("other-digest", verdict, strings.Repeat("0", 64)); err == nil {
				t.Fatal("a settled turn accepted another candidate digest")
			}
		})
	}
}

// Retirement leaves quarantine before its receipt is written, so a retry after a lost receipt
// used to be refused with "session is not quarantined".
func TestRetireQuarantinedRetryReturnsTheRetiredSessionAfterALostReceipt(t *testing.T) {
	ctx := context.Background()
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer service.Stop()
	sess, _ := createLegacyBoundSession(t, service, repo, "legacy-lost-retire", "legacy-lost-retire-session", "")
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !service.sessionQuarantined(sess.ID) {
		t.Fatal("legacy session must be quarantined")
	}
	current := mustSession(t, service, sess.ID)
	retire := DiscardRequest{RetireQuarantined: true, SessionID: sess.ID, ExpectedRevision: current.Revision}
	restore := failOperationReceipt(t, service, "Discard")
	if _, err := service.Discard(ctx, "retire", retire); err == nil || !strings.Contains(err.Error(), "injected receipt loss") {
		t.Fatalf("receipt loss was not exercised: %v", err)
	}
	restore()
	retired, err := service.Discard(ctx, "retire", retire)
	if err != nil || retired.ID != sess.ID || retired.State != session.SessionDiscarded || retired.Revision != current.Revision+1 {
		t.Fatalf("same-key retry = %+v, %v; want the retired session", retired, err)
	}
	if again, err := service.Discard(ctx, "retire-again", retire); err != nil || again.State != session.SessionDiscarded {
		t.Fatalf("new-key repeat = %+v, %v", again, err)
	}

	// An ordinary discard is not a retirement, whatever revision the request names.
	healthy, err := service.CreateRemoteSession(ctx, "create-healthy", service.request(t, "healthy"))
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(ctx, "close-healthy", session.CloseSessionRequest{SessionID: healthy.ID, ExpectedRevision: healthy.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(ctx, "plan-healthy", PlanDiscardRequest{SessionID: healthy.ID, ExpectedRevision: closed.Revision, AcceptUnmerged: true})
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := service.Discard(ctx, "discard-healthy", DiscardRequest{PlanOperationID: plan.OperationID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Discard(ctx, "retire-discarded", DiscardRequest{RetireQuarantined: true, SessionID: healthy.ID, ExpectedRevision: discarded.Revision - 1}); session.CodeOf(err) != session.CodeInvalidSessionState {
		t.Fatalf("retire of an ordinary discarded session = %v, want invalid_session_state", err)
	}
}
