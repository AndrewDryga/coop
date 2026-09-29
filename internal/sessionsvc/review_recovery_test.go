package sessionsvc

import (
	"context"
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
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
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
