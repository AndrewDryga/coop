package sessionsvc

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestRetainedReviewSurvivesLostCompletionWithoutRebuildingWorkspace(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	gateCalls := 0
	service := newReviewTestService(t, repo, 1024, ReviewGateFunc(func(context.Context, string, string) (ReviewGateResult, error) {
		gateCalls++
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "retained-recovery")
	for _, content := range []string{"intermediate\n", "final\n"} {
		sessionWorkspaceWrite(t, filepath.Join(sess.Workspace, "work.txt"), content)
		sessionWorkspaceGit(t, sess.Workspace, "add", ".")
		sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "model work")
	}
	db, err := sql.Open("sqlite", filepath.Join(service.Store().Root(), "session.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER interrupt_review_completion BEFORE UPDATE ON operations
		WHEN NEW.method = 'RunReview' AND NEW.state = 'succeeded'
		BEGIN SELECT RAISE(ABORT, 'injected completion loss'); END`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
	if _, err := service.RunReview(ctx, "retained-review", req); err == nil || !strings.Contains(err.Error(), "injected completion loss") {
		t.Fatalf("completion interruption was not exercised: %v", err)
	}
	op, err := service.Store().GetOperation(ctx, "retained-review")
	if err != nil || op.State != session.OperationRunning {
		t.Fatalf("interrupted operation: %+v, %v", op, err)
	}
	directory, retained, err := service.retainedReviewCandidate(ctx, op.ID)
	if err != nil || !retained.Publishable {
		t.Fatalf("lost reviewed candidate: %v", err)
	}
	if count := gitOut(directory, "rev-list", "--count", retained.ParentHead+".."+retained.CandidateHead); count != "1" {
		t.Fatalf("publication includes intermediate model commits: %s", count)
	}
	intent, err := service.captureReviewIntent(ctx, op.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.prepareForkReviewCandidateFromIntent(ctx, op.ID, intent)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.cleanup()
	head, tree, err := freezeReviewCommit(ctx, candidate, op)
	if err != nil || head != retained.CandidateHead || tree != retained.CandidateTree {
		t.Fatalf("review commit is not deterministic: %s %s %v", head, tree, err)
	}
	// No original Git objects or working files are needed to finish this receipt.
	saved := sess.Workspace + ".saved"
	if err := os.Rename(sess.Workspace, saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(saved, sess.Workspace) })
	if _, err := db.Exec("DROP TRIGGER interrupt_review_completion"); err != nil {
		t.Fatal(err)
	}
	unlock, locked := service.tryLockSessionRuntime(sess.ID)
	if !locked {
		t.Fatal("could not hold the session runtime")
	}
	_, busyErr := service.RunReview(ctx, "retained-review", req)
	unlock()
	if session.CodeOf(busyErr) != session.CodeOperationUncertain {
		t.Fatalf("recovery bypassed an active runtime operation: %v", busyErr)
	}
	replayed, err := service.RunReview(ctx, "retained-review", req)
	if err != nil || replayed.CandidateHead != retained.CandidateHead || gateCalls != 1 {
		t.Fatalf("replay rebuilt the reviewed result: %v, gates=%d", err, gateCalls)
	}
	if err := forkspace.GitRefCommand(ctx, directory, "update-ref", "refs/coop/reviews/"+op.ID, retained.ParentHead).Run(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.retainedReviewCandidate(ctx, op.ID); err == nil {
		t.Fatal("retained candidate tampering was accepted")
	}
}

func TestRetainedReviewOwnsLFSPayloadWithoutWorkingFiles(t *testing.T) {
	repo, git := gitrepo.New(t)
	writeSessionLFSSource(t, repo, []byte("original\n"))
	git("add", ".")
	git("commit", "-qm", "base")
	service := newReviewTestService(t, repo, 1024, ReviewGateFunc(func(context.Context, string, string) (ReviewGateResult, error) {
		return ReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "retained-lfs")
	writeSessionLFSSource(t, sess.Workspace, []byte("model binary\x00data\n"))
	sessionWorkspaceGit(t, sess.Workspace, "add", ".")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "updated asset")
	dossier, err := service.RunReview(context.Background(), "retained-lfs-review", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil || !dossier.CandidateRetained {
		t.Fatalf("review: %v", err)
	}
	directory, _, err := service.retainedReviewCandidate(context.Background(), dossier.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(directory, "asset.bin")) {
		t.Fatal("retention duplicated the LFS working file")
	}
	err = forkspace.VisitLFSPointers(context.Background(), directory, dossier.CandidateHead, func(pointer forkspace.LFSPointer) error {
		object := filepath.Join(directory, ".git/lfs/objects", pointer.OID[:2], pointer.OID[2:4], pointer.OID)
		return os.WriteFile(object, []byte("corrupt"), 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.retainedReviewCandidate(context.Background(), dossier.OperationID); err == nil {
		t.Fatal("corrupt retained LFS payload was accepted")
	}
}
