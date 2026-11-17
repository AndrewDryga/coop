package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

func TestSessionServiceRunReviewCompletesAfterClientCancellation(t *testing.T) {
	repo, _ := gitrepo.NewCommitted(t)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	service := newReviewTestService(t, repo, 1<<20, ReviewGateFunc(func(ctx context.Context, _ ReviewGateRequest) (ReviewGateResult, error) {
		started <- ctx
		<-release
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	sess := createReviewSession(t, service, "client-cancel")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		dossier ReviewDossier
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		dossier, err := service.RunReview(ctx, "review-client-cancel", RunReviewRequest{
			SessionID: sess.ID, ExpectedRevision: sess.Revision,
		})
		resultCh <- result{dossier: dossier, err: err}
	}()
	var gateCtx context.Context
	select {
	case gateCtx = <-started:
	case early := <-resultCh:
		t.Fatalf("review ended before gate entry: %+v, %v", early.dossier, early.err)
	case <-time.After(wait.Deadline):
		t.Fatal("review did not enter gate")
	}
	cancel()
	if err := gateCtx.Err(); err != nil {
		t.Fatalf("review gate inherited client cancellation: %v", err)
	}
	unblock()

	got := <-resultCh
	if got.err != nil || !got.dossier.Publishable {
		t.Fatalf("review after client cancellation = %+v, err=%v", got.dossier, got.err)
	}
	op, err := service.Store().GetOperation(context.Background(), "review-client-cancel")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("review operation after client cancellation = %+v, err=%v", op, err)
	}
}

// A request whose own context ended did not fail: the client left. Recording that as the
// operation's durable failure made every same-key retry replay "internal: context canceled".
func TestCancelledRequestLeavesItsReviewOperationRetryable(t *testing.T) {
	repo, _ := gitrepo.NewCommitted(t)
	service := newReviewTestService(t, repo, 1<<20, ReviewGateFunc(func(context.Context, ReviewGateRequest) (ReviewGateResult, error) {
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "cancelled-retry")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")
	req := RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
	op, _, err := service.Store().ReserveOperation(context.Background(), "RunReview", "review-cancelled-retry", req)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = service.failServiceOperation(ctx, op.ID, fmt.Errorf("read review session: %w", ctx.Err()))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request lost its cause: %v", err)
	}
	if stored, err := service.Store().GetOperationByID(context.Background(), op.ID); err != nil || stored.State != session.OperationReserved {
		t.Fatalf("cancelled request settled its operation: %+v, %v", stored, err)
	}
	dossier, err := service.RunReview(context.Background(), "review-cancelled-retry", req)
	if err != nil || !dossier.Publishable {
		t.Fatalf("same-key retry after a cancelled request = %+v, %v", dossier, err)
	}
	// A failure that is not the request's own cancellation still settles the operation.
	other, _, err := service.Store().ReserveOperation(context.Background(), "RunReview", "review-real-failure",
		RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision + 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = service.failServiceOperation(ctx, other.ID, errors.New("gate exploded"))
	if stored, err := service.Store().GetOperationByID(context.Background(), other.ID); err != nil || stored.State != session.OperationFailed {
		t.Fatalf("a real failure was not recorded: %+v, %v", stored, err)
	}
}
