package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestSessionServiceRunReviewCleanGreenReplayAndIsolation(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var gateCalls atomic.Int32
	service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(_ context.Context, gateRepo, candidate string) (SessionReviewGateResult, error) {
		gateCalls.Add(1)
		if gateRepo != repo || candidate == repo || !pathExists(candidate) {
			t.Errorf("gate inputs = (%q, %q)", gateRepo, candidate)
		}
		return SessionReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "clean")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")
	parentBefore, sourceBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(sess.Workspace)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	dossier, err := service.RunReview(context.Background(), "review-clean", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if !dossier.Publishable || dossier.Rebase != SessionReviewRebaseClean || dossier.Gate != SessionReviewGatePassed || dossier.CandidateHead == "" || dossier.CandidateTree == "" || len(dossier.Patch) == 0 || dossier.PatchTruncated {
		t.Fatalf("green review dossier = %+v", dossier)
	}
	if !bytes.Contains(dossier.Patch, []byte("change.txt")) {
		t.Fatalf("candidate patch = %q", dossier.Patch)
	}
	if got := reviewSourceSnapshot(repo); got != parentBefore {
		t.Fatalf("parent changed during review:\nbefore=%s\nafter=%s", parentBefore, got)
	}
	if got := reviewSourceSnapshot(sess.Workspace); got != sourceBefore {
		t.Fatalf("source changed during review:\nbefore=%s\nafter=%s", sourceBefore, got)
	}
	if got := gitOut(repo, "show-ref", "--verify", "refs/heads/review/"+sess.ForkName); got != "" {
		t.Fatalf("review ref created: %s", got)
	}
	assertReviewScratchEmpty(t, tmp)

	replayed, err := service.RunReview(context.Background(), "review-clean", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil || !reflect.DeepEqual(replayed, dossier) {
		t.Fatalf("review replay = %+v, err=%v; original=%+v", replayed, err, dossier)
	}
	if got := gateCalls.Load(); got != 1 {
		t.Fatalf("gate calls after replay = %d, want 1", got)
	}
	if _, err := service.RunReview(context.Background(), "review-clean", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision + 1}); session.CodeOf(err) != session.CodeIdempotencyConflict {
		t.Fatalf("idempotency conflict = %v", err)
	}
}

func TestSessionServiceRunReviewGateOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		gate          SessionReviewGateResult
		wantGate      SessionReviewGateStatus
		wantPublish   bool
		wantReason    string
		wantGateError string
	}{
		{name: "none", gate: SessionReviewGateResult{}, wantGate: SessionReviewGateNone, wantReason: "gate_not_configured"},
		{name: "red", gate: SessionReviewGateResult{Configured: true}, wantGate: SessionReviewGateFailed, wantReason: "gate_failed"},
		{name: "startup", gate: SessionReviewGateResult{Configured: true, StartupError: "runtime unavailable\nsecret"}, wantGate: SessionReviewGateStartupError, wantReason: "gate_startup_error", wantGateError: "runtime unavailable secret"},
		{name: "green", gate: SessionReviewGateResult{Configured: true, Passed: true}, wantGate: SessionReviewGatePassed, wantPublish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitRepo(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
				return tc.gate, nil
			}))
			defer service.Stop()
			sess := createReviewSession(t, service, tc.name)
			if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
			sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")
			dossier, err := service.RunReview(context.Background(), "review-"+tc.name, RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
			if err != nil {
				t.Fatal(err)
			}
			if dossier.Gate != tc.wantGate || dossier.Publishable != tc.wantPublish {
				t.Fatalf("review dossier = %+v", dossier)
			}
			if tc.wantReason != "" && !containsReviewReason(dossier.NotPublishableReasons, tc.wantReason) {
				t.Fatalf("reasons = %v, want %q", dossier.NotPublishableReasons, tc.wantReason)
			}
			if dossier.GateError != tc.wantGateError {
				t.Fatalf("gate error = %q, want %q", dossier.GateError, tc.wantGateError)
			}
		})
	}
}

func TestSessionServiceRunReviewConflictAndDirtyReject(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		repo, git := gitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("base\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-qm", "base")
		service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
			t.Fatal("gate ran on rebase conflict")
			return SessionReviewGateResult{}, nil
		}))
		defer service.Stop()
		sess := createReviewSession(t, service, "conflict")
		if err := os.WriteFile(filepath.Join(sess.Workspace, "shared.txt"), []byte("fork\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sessionWorkspaceGit(t, sess.Workspace, "add", "shared.txt")
		sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "fork conflict")
		if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("parent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "shared.txt")
		git("commit", "-qm", "parent conflict")
		parentBefore, sourceBefore := reviewSourceSnapshot(repo), reviewSourceSnapshot(sess.Workspace)
		dossier, err := service.RunReview(context.Background(), "review-conflict", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
		if err != nil || dossier.Rebase != SessionReviewRebaseConflict || dossier.Gate != SessionReviewGateNotRun || dossier.CandidateHead != "" || dossier.CandidateTree != "" || dossier.Publishable {
			t.Fatalf("conflict review = %+v, err=%v", dossier, err)
		}
		if got := reviewSourceSnapshot(repo); got != parentBefore {
			t.Fatalf("parent changed on conflict:\nbefore=%s\nafter=%s", parentBefore, got)
		}
		if got := reviewSourceSnapshot(sess.Workspace); got != sourceBefore {
			t.Fatalf("source changed on conflict:\nbefore=%s\nafter=%s", sourceBefore, got)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		repo, git := gitRepo(t)
		git("commit", "-q", "--allow-empty", "-m", "base")
		service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
			t.Fatal("gate ran for dirty source")
			return SessionReviewGateResult{}, nil
		}))
		defer service.Stop()
		sess := createReviewSession(t, service, "dirty")
		if err := os.WriteFile(filepath.Join(sess.Workspace, "untracked.txt"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := service.RunReview(context.Background(), "review-dirty", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
		if err == nil {
			t.Fatalf("dirty review err = %v", err)
		}
		op, err := service.Store().GetOperation(context.Background(), "review-dirty")
		if err != nil || op.State != session.OperationFailed {
			t.Fatalf("dirty operation = %+v, err=%v", op, err)
		}
	})
}

func TestSessionServiceRunReviewMovementMakesDossierNonPublishable(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var moved atomic.Bool
	service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
		if moved.Swap(true) {
			return SessionReviewGateResult{Configured: true, Passed: true}, nil
		}
		if err := os.WriteFile(filepath.Join(repo, "parent-moved.txt"), []byte("parent\n"), 0o644); err != nil {
			return SessionReviewGateResult{}, err
		}
		git("add", "parent-moved.txt")
		git("commit", "-qm", "parent moved")
		return SessionReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "movement")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "fork-moved.txt"), []byte("fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "fork-moved.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "fork moved")
	// The source move is made by the gate callback through the candidate's bound source path
	// after the service has captured its immutable intent.
	gate := service.reviewGate
	service.reviewGate = SessionReviewGateFunc(func(ctx context.Context, gateRepo, candidate string) (SessionReviewGateResult, error) {
		result, err := gate.Run(ctx, gateRepo, candidate)
		if err != nil {
			return result, err
		}
		if err := os.WriteFile(filepath.Join(sess.Workspace, "source-moved.txt"), []byte("source\n"), 0o644); err != nil {
			return SessionReviewGateResult{}, err
		}
		sessionWorkspaceGit(t, sess.Workspace, "add", "source-moved.txt")
		sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "source moved")
		return result, nil
	})
	dossier, err := service.RunReview(context.Background(), "review-movement", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil || dossier.Publishable || !containsReviewReason(dossier.NotPublishableReasons, "parent_moved") || !containsReviewReason(dossier.NotPublishableReasons, "source_moved") {
		t.Fatalf("movement review = %+v, err=%v", dossier, err)
	}
}

func TestSessionReviewIntentUsesCapturedObjectsBeforePublishabilityCheck(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
		return SessionReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "captured-objects")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "captured.txt"), []byte("captured\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "captured.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "captured source")
	op, replay, err := service.Store().ReserveOperation(context.Background(), "RunReview", "review-captured", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil || replay {
		t.Fatalf("reserve review = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent, err := service.captureReviewIntent(context.Background(), op.ID, RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	intentData, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store().MarkOperationRunning(context.Background(), op.ID, intentData); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "parent-moved.txt"), []byte("parent moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "parent-moved.txt")
	git("commit", "-qm", "parent moved")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "source-moved.txt"), []byte("source moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "source-moved.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "source moved")

	dossier, err := service.executeReviewIntent(context.Background(), op, intent)
	if err != nil {
		t.Fatal(err)
	}
	if dossier.ParentHead != intent.ParentHead || dossier.ParentTree != intent.ParentTree || dossier.SourceHead != intent.SourceHead || dossier.SourceTree != intent.SourceTree {
		t.Fatalf("review evidence moved with refs: dossier=%+v intent=%+v", dossier, intent)
	}
	if !bytes.Contains(dossier.Patch, []byte("captured.txt")) || bytes.Contains(dossier.Patch, []byte("source-moved.txt")) {
		t.Fatalf("captured candidate patch = %q", dossier.Patch)
	}
	if dossier.Publishable || !containsReviewReason(dossier.NotPublishableReasons, "parent_moved") || !containsReviewReason(dossier.NotPublishableReasons, "source_moved") {
		t.Fatalf("captured movement publishability = %+v", dossier)
	}
}

func TestSessionServiceRunReviewResumesFrozenRunningAndUncertainIntent(t *testing.T) {
	for _, initialState := range []session.OperationState{
		session.OperationRunning,
		session.OperationUncertain,
	} {
		t.Run(string(initialState), func(t *testing.T) {
			repo, git := gitRepo(t)
			git("commit", "-q", "--allow-empty", "-m", "base")
			var gateCalls atomic.Int32
			service := newReviewTestService(
				t,
				repo,
				1<<20,
				SessionReviewGateFunc(
					func(context.Context, string, string) (SessionReviewGateResult, error) {
						gateCalls.Add(1)
						return SessionReviewGateResult{Configured: true, Passed: true}, nil
					},
				),
			)
			defer service.Stop()
			sess := createReviewSession(t, service, string(initialState))
			if err := os.WriteFile(
				filepath.Join(sess.Workspace, "change.txt"),
				[]byte("reviewed\n"),
				0o644,
			); err != nil {
				t.Fatal(err)
			}
			sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
			sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")
			req := RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
			key := "review-" + string(initialState)
			op, replay, err := service.Store().ReserveOperation(
				context.Background(),
				"RunReview",
				key,
				req,
			)
			if err != nil || replay {
				t.Fatalf("reserve review = %+v, replay=%v, err=%v", op, replay, err)
			}
			intent, err := service.captureReviewIntent(context.Background(), op.ID, req)
			if err != nil {
				t.Fatal(err)
			}
			intentData, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.Store().MarkOperationRunning(
				context.Background(),
				op.ID,
				intentData,
			); err != nil {
				t.Fatal(err)
			}
			if initialState == session.OperationUncertain {
				if err := service.Store().MarkOperationUncertain(
					context.Background(),
					op.ID,
				); err != nil {
					t.Fatal(err)
				}
			}

			dossier, err := service.RunReview(context.Background(), key, req)
			if err != nil || !dossier.Publishable || dossier.OperationID != op.ID ||
				gateCalls.Load() != 1 {
				t.Fatalf(
					"resumed review = %+v, err=%v, gate calls=%d",
					dossier,
					err,
					gateCalls.Load(),
				)
			}
			got, err := service.Store().GetOperation(context.Background(), key)
			if err != nil || got.State != session.OperationSucceeded ||
				got.ID != op.ID {
				t.Fatalf("resumed operation = %+v, err=%v", got, err)
			}
		})
	}
}

func TestSessionServiceRunReviewMalformedRunningIntentBecomesUncertain(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var gateCalls atomic.Int32
	service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
		gateCalls.Add(1)
		return SessionReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "malformed")
	req := RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
	op, replay, err := service.Store().ReserveOperation(
		context.Background(),
		"RunReview",
		"review-malformed",
		req,
	)
	if err != nil || replay {
		t.Fatalf("reserve malformed review = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent := []byte(`{"session_id":`)
	if err := service.Store().MarkOperationRunning(context.Background(), op.ID, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunReview(
		context.Background(),
		"review-malformed",
		req,
	); !errors.Is(err, session.ErrOperationUncertain) {
		t.Fatalf("malformed replay err = %v", err)
	}
	got, err := service.Store().GetOperation(context.Background(), "review-malformed")
	if err != nil || got.State != session.OperationUncertain ||
		string(got.Result) != string(intent) || gateCalls.Load() != 0 {
		t.Fatalf(
			"malformed replay operation = %+v, err=%v, gate calls=%d",
			got,
			err,
			gateCalls.Load(),
		)
	}
}

func TestSessionServiceRunReviewConcurrentReplayWaitsForFirst(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	started := make(chan struct{})
	release := make(chan struct{})
	var gateCalls atomic.Int32
	service := newReviewTestService(t, repo, 1<<20, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
		if gateCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return SessionReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "concurrent")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")

	type result struct {
		dossier SessionReviewDossier
		err     error
	}
	req := RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
	results := make(chan result, 2)
	var callers sync.WaitGroup
	callers.Add(2)
	go func() {
		defer callers.Done()
		dossier, err := service.RunReview(context.Background(), "review-concurrent", req)
		results <- result{dossier: dossier, err: err}
	}()
	<-started
	go func() {
		defer callers.Done()
		dossier, err := service.RunReview(context.Background(), "review-concurrent", req)
		results <- result{dossier: dossier, err: err}
	}()
	select {
	case got := <-results:
		t.Fatalf("duplicate review returned before the first completed: %+v", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	callers.Wait()
	close(results)

	var dossiers []SessionReviewDossier
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		dossiers = append(dossiers, got.dossier)
	}
	if len(dossiers) != 2 || !reflect.DeepEqual(dossiers[0], dossiers[1]) || gateCalls.Load() != 1 {
		t.Fatalf("concurrent review results = %+v, gate calls=%d", dossiers, gateCalls.Load())
	}
}

func TestSessionServiceRunReviewTruncatesBoundedPatch(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newReviewTestService(t, repo, 48, SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
		return SessionReviewGateResult{Configured: true, Passed: true}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "truncated")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "large.txt"), []byte(strings.Repeat("large review line\n", 32)), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "large.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "large review")
	dossier, err := service.RunReview(context.Background(), "review-truncated", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil || !dossier.PatchTruncated || dossier.Publishable || !containsReviewReason(dossier.NotPublishableReasons, "patch_truncated") || len(dossier.Patch) > 48 {
		t.Fatalf("truncated review = %+v, err=%v", dossier, err)
	}
	if data, err := json.Marshal(dossier); err != nil || len(data) > session.MaxOperationResultBytes {
		t.Fatalf("truncated dossier size = %d, err=%v", len(data), err)
	}
}

func newReviewTestService(t *testing.T, repo string, maxPatchBytes int, gate SessionReviewGate) *SessionService {
	t.Helper()
	policies := testSessionPolicies(repo)
	policies["responder"] = SessionPolicy{
		Name: "responder", Repository: repo, Target: "codex@work", MaxTurns: 10,
		MaxQueuedTurns: 5, MaxQueuedBytes: 1 << 20, MaxPatchBytes: maxPatchBytes, TurnTimeout: time.Second,
	}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: policies, ReviewGate: gate,
		Runner: SessionRunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) { return turn, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func createReviewSession(t *testing.T, service *SessionService, task string) session.Session {
	t.Helper()
	sess, err := service.CreateRemoteSession(context.Background(), "create-"+task, CreateRemoteSessionRequest{Policy: "responder", Task: task})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func containsReviewReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
