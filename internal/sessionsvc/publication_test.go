package sessionsvc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestPublicationRetainsIntentAndPublishesReviewedSHAAfterWorkspaceMoves(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	service := newReviewTestService(t, repo, 1024, ReviewGateFunc(func(context.Context, ReviewGateRequest) (ReviewGateResult, error) {
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "publish")
	sessionWorkspaceWrite(t, filepath.Join(sess.Workspace, "result.txt"), "reviewed\n")
	sessionWorkspaceGit(t, sess.Workspace, "add", ".")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "work")
	ctx := context.Background()
	review, err := service.RunReview(ctx, "review", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	req := workerproto.PublishRequest{AuthorizationRef: "approved", CandidateHead: review.CandidateHead, CandidateTree: review.CandidateTree, Branch: "coop/fix", BaseBranch: "main", Title: "Fix", Body: "Reviewed work"}
	calls := 0
	service.reviewPublisher = func(_ context.Context, dir string, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
		calls++
		if intent.Request != req || dir == sess.Workspace || gitOut(dir, "rev-parse", "HEAD") != review.CandidateHead {
			t.Fatal("publisher used live workspace or changed intent")
		}
		if calls == 1 {
			return workerproto.PublishResult{}, errors.New("lost remote response")
		}
		return workerproto.PublishResult{Status: "published", Receipt: &workerproto.PublicationReceipt{
			Repository: intent.Repository.RepositoryRef, BranchRef: "refs/heads/coop/fix", CandidateTree: review.CandidateTree, CommitSHA: review.CandidateHead,
			PullRequestNumber: 7, PullRequestURL: "https://github.com/" + intent.Repository.GitHubRepository + "/pull/7",
		}}, nil
	}
	op, err := service.PublishReview(ctx, "publish", sess.ID, review.OperationID, req)
	if err != nil || op.State != session.OperationRunning {
		t.Fatalf("admit: %+v %v", op, err)
	}
	if err := service.requireNoPendingPublication(ctx, sess.ID); err == nil {
		t.Fatal("pending publication allowed discard")
	}
	saved := sess.Workspace + ".saved"
	if err := os.Rename(sess.Workspace, saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(saved, sess.Workspace) })
	if err := service.runPublishOperation(ctx, op.ID); err == nil {
		t.Fatal("lost response not exercised")
	}
	interrupted, _ := service.Store().GetOperationByID(ctx, op.ID)
	if interrupted.State != session.OperationRunning {
		t.Fatal("publication intent lost")
	}
	if err := service.runPublishOperation(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	_, result, err := service.GetPublication(ctx, sess.ID, op.ID)
	if err != nil || result.Receipt.CommitSHA != review.CandidateHead {
		t.Fatalf("exact result: %+v %v", result, err)
	}
	if err := service.requireNoPendingPublication(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.PublishReview(ctx, "publish", sess.ID, review.OperationID, req)
	if err != nil || replayed.ID != op.ID || replayed.State != session.OperationSucceeded || calls != 2 {
		t.Fatalf("replay: %+v %v calls=%d", replayed, err, calls)
	}
	changed := req
	changed.CandidateHead = review.ParentHead
	if _, err := service.PublishReview(ctx, "publish", sess.ID, review.OperationID, changed); session.CodeOf(err) != session.CodeIdempotencyConflict {
		t.Fatalf("changed publication replay: %v", err)
	}
	if _, err := service.PublishReview(ctx, "different", sess.ID, review.OperationID, changed); err == nil {
		t.Fatal("another commit substituted for approved review")
	}
}
