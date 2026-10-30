package sessionsvc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	// An authorized alerting task could not open its PR because its required replay
	// setup changed CI. Publication must keep that workflow in the exact reviewed tree.
	workflow := "name: replay\non: push\njobs:\n  replay:\n    runs-on: ubuntu-latest\n    steps:\n      - run: ./scripts/replay.sh\n"
	if err := os.MkdirAll(filepath.Join(sess.Workspace, ".github/workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceWrite(t, filepath.Join(sess.Workspace, ".github/workflows/replay.yml"), workflow)
	sessionWorkspaceGit(t, sess.Workspace, "add", ".")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "work")
	ctx := context.Background()
	review, err := service.RunReview(ctx, "review", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !review.Publishable || !review.CandidateRetained || len(review.PolicyFindings) != 0 {
		t.Fatalf("authorized workflow was not retained for publication: %+v", review)
	}
	req := workerproto.PublishRequest{AuthorizationRef: "approved", CandidateHead: review.CandidateHead, CandidateTree: review.CandidateTree, Branch: "coop/fix", BaseBranch: "main", Title: "Fix", Body: "Reviewed work"}
	calls := 0
	service.reviewPublisher = func(_ context.Context, dir string, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
		calls++
		if intent.Request != req || dir == sess.Workspace || gitOut(dir, "rev-parse", "HEAD") != review.CandidateHead {
			t.Fatal("publisher used live workspace or changed intent")
		}
		if got := gitOut(dir, "show", "HEAD:.github/workflows/replay.yml"); got != strings.TrimSpace(workflow) {
			t.Fatalf("published workflow = %q, want reviewed bytes", got)
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

func TestPublicationResultMustMatchRetainedIntent(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	service := newReviewTestService(t, repo, 1024, ReviewGateFunc(func(context.Context, ReviewGateRequest) (ReviewGateResult, error) {
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "receipt-boundary")
	sessionWorkspaceWrite(t, filepath.Join(sess.Workspace, "result.txt"), "reviewed\n")
	sessionWorkspaceGit(t, sess.Workspace, "add", "result.txt")
	sessionWorkspaceGit(t, sess.Workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "work")
	ctx := t.Context()
	review, err := service.RunReview(ctx, "receipt-review", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	req := workerproto.PublishRequest{AuthorizationRef: "approved", CandidateHead: review.CandidateHead, CandidateTree: review.CandidateTree, Branch: "coop/fix", BaseBranch: "main", Title: "Fix", Body: "Reviewed work"}
	published := func(intent workerproto.PublishIntent) workerproto.PublishResult {
		return workerproto.PublishResult{Status: "published", Receipt: &workerproto.PublicationReceipt{Repository: intent.Repository.RepositoryRef, BranchRef: "refs/heads/" + intent.Request.Branch, CandidateTree: intent.Request.CandidateTree, CommitSHA: intent.Request.CandidateHead, PullRequestNumber: 7, PullRequestURL: "https://github.com/" + intent.Repository.GitHubRepository + "/pull/7"}}
	}
	cases := []struct {
		name   string
		valid  bool
		change func(*workerproto.PublishResult, workerproto.PublishIntent)
	}{
		{"commit", false, func(r *workerproto.PublishResult, _ workerproto.PublishIntent) {
			r.Receipt.CommitSHA = review.ParentHead
		}},
		{"repository", false, func(r *workerproto.PublishResult, _ workerproto.PublishIntent) {
			r.Receipt.Repository = "foreign-repository"
		}},
		{"branch", false, func(r *workerproto.PublishResult, _ workerproto.PublishIntent) {
			r.Receipt.BranchRef = "refs/heads/foreign"
		}},
		{"receipt-and-conflict", false, func(r *workerproto.PublishResult, _ workerproto.PublishIntent) {
			r.Conflict = &workerproto.PublicationConflict{}
		}},
		{"conflict", true, func(r *workerproto.PublishResult, i workerproto.PublishIntent) {
			*r = workerproto.PublishResult{Status: "conflict", ErrorCode: "publication_branch_changed", Conflict: &workerproto.PublicationConflict{Repository: i.Repository.RepositoryRef, GitHubRepository: i.Repository.GitHubRepository, BranchRef: "refs/heads/" + i.Request.Branch, CandidateCommitSHA: i.Request.CandidateHead, ObservedHeadSHA: review.ParentHead, PullRequestNumber: 7, PullRequestURL: "https://github.com/" + i.Repository.GitHubRepository + "/pull/7"}}
		}},
		{"refused", true, func(r *workerproto.PublishResult, _ workerproto.PublishIntent) {
			*r = workerproto.PublishResult{Status: "refused", ErrorCode: "publication_authorization_revoked"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want workerproto.PublishResult
			service.reviewPublisher = func(_ context.Context, _ string, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
				want = published(intent)
				tc.change(&want, intent)
				return want, nil
			}
			op, err := service.PublishReview(ctx, "receipt-"+tc.name, sess.ID, review.OperationID, req)
			if err != nil || op.State != session.OperationRunning {
				t.Fatalf("admit: %+v %v", op, err)
			}
			runErr := service.runPublishOperation(ctx, op.ID)
			if !tc.valid {
				if runErr == nil || !strings.Contains(runErr.Error(), "mismatched receipt") {
					t.Fatalf("bad receipt result: %v", runErr)
				}
				retained, err := service.Store().GetOperationByID(ctx, op.ID)
				if err != nil || retained.State != session.OperationRunning || !bytes.Equal(retained.Result, op.Result) {
					t.Fatalf("lost running intent: %+v %v", retained, err)
				}
				if _, _, err := service.GetPublication(ctx, sess.ID, op.ID); session.CodeOf(err) != session.CodeInvalidRequest {
					t.Fatalf("completed bad publication: %v", err)
				}
				if err := service.requireNoPendingPublication(ctx, sess.ID); err == nil {
					t.Fatal("unresolved publication allowed discard")
				}
				service.reviewPublisher = func(_ context.Context, _ string, intent workerproto.PublishIntent) (workerproto.PublishResult, error) {
					want = published(intent)
					return want, nil
				}
				runErr = service.runPublishOperation(ctx, op.ID)
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			completed, got, err := service.GetPublication(ctx, sess.ID, op.ID)
			if err != nil || completed.State != session.OperationSucceeded || !reflect.DeepEqual(got, want) {
				t.Fatalf("projection: %+v %+v %v want %+v", completed, got, err, want)
			}
			if err := service.requireNoPendingPublication(ctx, sess.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
