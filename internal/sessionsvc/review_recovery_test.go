package sessionsvc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestSessionServiceRunReviewCompletesAfterClientCancellation(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	service := newReviewTestService(t, repo, 1<<20, ReviewGateFunc(func(ctx context.Context, _ string, _ string) (ReviewGateResult, error) {
		started <- ctx
		<-release
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	sess := createReviewSession(t, service, "client-cancel")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")

	ctx, cancel := context.WithCancel(context.Background())
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
	gateCtx := <-started
	cancel()
	if err := gateCtx.Err(); err != nil {
		t.Fatalf("review gate inherited client cancellation: %v", err)
	}
	close(release)

	got := <-resultCh
	if got.err != nil || !got.dossier.Publishable {
		t.Fatalf("review after client cancellation = %+v, err=%v", got.dossier, got.err)
	}
	op, err := service.Store().GetOperation(context.Background(), "review-client-cancel")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("review operation after client cancellation = %+v, err=%v", op, err)
	}
}
