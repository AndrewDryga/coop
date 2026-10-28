package sessionsvc

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestCancellationSettlesAfterCallerStopsWaiting(t *testing.T) {
	for _, mode := range []string{"caller", "deadline", "natural"} {
		t.Run(mode, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, func(store *session.Store) Runner {
				return RunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
					if _, err := store.MarkTurnSendIntent(ctx, bound.ID, turn.ID); err != nil {
						return turn, err
					}
					if _, err := store.MarkTurnSent(ctx, bound.ID, turn.ID); err != nil {
						return turn, err
					}
					close(started)
					<-ctx.Done()
					<-release
					if mode == "natural" {
						return store.CompleteTurn(context.Background(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: "natural"})
					}
					return turn, ctx.Err()
				})
			})
			defer service.Stop()
			defer releaseOnce.Do(func() { close(release) })
			if mode == "deadline" {
				service.stopTimeout = 20 * time.Millisecond
			}
			if err := service.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			sess, err := service.CreateRemoteSession(t.Context(), "create", service.request(t, "late-cleanup"))
			if err != nil {
				t.Fatal(err)
			}
			turn, err := service.SubmitTurn(t.Context(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "cancel"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(wait.Deadline):
				t.Fatal("runner did not start")
			}
			req := session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			response := make(chan error, 1)
			go func() { _, err := service.CancelTurn(caller, "cancel-late", req); response <- err }()
			var active *activeSessionTurn
			waitForSessionTest(t, func() bool {
				service.mu.Lock()
				defer service.mu.Unlock()
				active = service.active[turn.ID]
				return active != nil && active.requested
			})
			if mode != "deadline" {
				cancel()
			}
			select {
			case err := <-response:
				if session.CodeOf(err) != session.CodeOperationUncertain {
					t.Fatalf("waiting result: %v", err)
				}
			case <-time.After(wait.Deadline):
				t.Fatal("cancel caller did not return")
			}
			if _, err := service.store.CancelTurn(t.Context(), "cancel-late", req); session.CodeOf(err) != session.CodeOperationUncertain {
				t.Fatalf("ordinary store retry assumed cleanup: %v", err)
			}
			current, err := service.store.GetTurn(t.Context(), sess.ID, turn.ID)
			if err != nil || sessionTurnTerminal(current.State) {
				t.Fatalf("cleanup still held but turn terminal: %+v %v", current, err)
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case <-active.done:
			case <-time.After(wait.Deadline):
				t.Fatal("worker did not settle")
			}
			got, err := service.CancelTurn(t.Context(), "cancel-late", req)
			want := session.TurnCancelled
			if mode == "natural" {
				want = session.TurnCompleted
			}
			if err != nil || got.State != want {
				t.Fatalf("settled replay: %+v %v", got, err)
			}
			op, err := service.store.GetOperation(t.Context(), "cancel-late")
			hash, _ := session.CanonicalRequestHash(req)
			if err != nil || op.State != session.OperationSucceeded || op.ErrorCode != "" || op.ErrorDetail != "" || op.RequestHash != hash {
				t.Fatalf("settled receipt: %+v %v", op, err)
			}
		})
	}
}

func TestCancellationDoesNotCertifyTerminalTurnWithPendingCleanup(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded cleanup", true: "joined failure"}[joined], func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			started := make(chan struct{})
			var service *Service
			fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, func(store *session.Store) Runner {
				return RunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
					close(started)
					<-ctx.Done()
					failed, err := store.FailTurn(context.Background(), session.FailTurnRequest{SessionID: bound.ID, TurnID: turn.ID, ErrorCode: session.CodeInternal, ErrorDetail: "provider failed"})
					if err != nil {
						return failed, err
					}
					if !joined {
						service.runtimeCleanupFailed(bound.ID)
						return failed, nil
					}
					return failed, errors.Join(acpFailure(sessionACPProcessError, "provider failed"), acpFailure(sessionACPCleanupError, "cleanup pending"))
				})
			})
			service = fixture.Service
			defer service.Stop()
			if err := service.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			sess, err := service.CreateRemoteSession(t.Context(), "create", fixture.request(t, "pending-cleanup"))
			if err != nil {
				t.Fatal(err)
			}
			turn, err := service.SubmitTurn(t.Context(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "cancel"})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(wait.Deadline):
				t.Fatal("runner did not start")
			}
			req := session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}
			_, err = service.CancelTurn(t.Context(), "cancel-pending", req)
			if session.CodeOf(err) != session.CodeOperationUncertain {
				t.Fatalf("pending cleanup certified: %v", err)
			}
			op, err := service.store.GetOperation(t.Context(), "cancel-pending")
			if err != nil || op.State != session.OperationUncertain {
				t.Fatalf("pending cleanup receipt: %+v %v", op, err)
			}
		})
	}
}
