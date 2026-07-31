package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
)

func TestParseSessionPoliciesIsStrictAndPinsOneTarget(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	companion, companionGit := gitRepo(t)
	companionGit("commit", "-q", "--allow-empty", "-m", "companion base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	companion, err = filepath.EvalSymlinks(companion)
	if err != nil {
		t.Fatal(err)
	}
	configRoot := t.TempDir()
	profile := filepath.Join(configRoot, "codex", "profiles", "work")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: configRoot}
	valid := []byte("version: 1\npolicies:\n  responder:\n    repository: " + repo +
		"\n    companions:\n      - name: application\n        repository: " + companion +
		"\n    target: codex:model/high@work\n    max_turns: 100\n    max_queued_turns: 20\n    max_queued_bytes: 1048576\n    max_patch_bytes: 1048576\n    turn_timeout: 1h\n    warm_idle_timeout: 15m\n")
	policies, err := parseSessionPolicies(valid, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := policies["responder"]; got.Target != "codex:model/high@work" ||
		got.Repository != repo || got.TurnTimeout != time.Hour ||
		got.WarmIdleTimeout != 15*time.Minute ||
		len(got.Companions) != 1 ||
		got.Companions[0] != (SessionCompanionPolicy{
			Name: "application", Repository: companion,
		}) {
		t.Fatalf("parsed policy = %+v", got)
	}
	for name, body := range map[string]string{
		"unknown":  string(valid) + "    typo: true\n",
		"preset":   string(valid[:len(valid)-1]) + "    target: codex@work,other\n",
		"relative": "version: 1\npolicies:\n  p:\n    repository: repo\n    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n",
	} {
		if _, err := parseSessionPolicies([]byte(body), cfg); err == nil {
			t.Fatalf("%s policy unexpectedly accepted", name)
		}
	}
	tooLong := strings.Replace(string(valid), "warm_idle_timeout: 15m", "warm_idle_timeout: 61m", 1)
	if _, err := parseSessionPolicies([]byte(tooLong), cfg); err == nil ||
		!strings.Contains(err.Error(), "warm_idle_timeout") {
		t.Fatalf("oversized warm idle timeout error = %v", err)
	}
}

func TestWarmIdleTimeoutIsBoundIntoPolicyDigest(t *testing.T) {
	policy := SessionPolicy{Name: "conversation", Repository: "/repo", Target: "codex@work", TurnTimeout: time.Hour}
	cold := resolvedSessionPolicyDigest(policy)
	if want := "0f7066c5d36ac4cfd709ce3908092be4f92f8ee2b93d4d6983cea527e8bc2ddb"; cold != want {
		t.Fatalf("cold policy digest = %q, want backward-compatible %q", cold, want)
	}
	policy.WarmIdleTimeout = 15 * time.Minute
	if warm := resolvedSessionPolicyDigest(policy); warm == cold {
		t.Fatal("warm idle timeout did not change the immutable policy digest")
	}
}

func TestParseSessionPoliciesRejectsUnsafeCompanions(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "version: 1\npolicies:\n  responder:\n    repository: " + repo + "\n"
	suffix := "    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n" +
		"    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n"
	for name, companions := range map[string]string{
		"primary alias":    "    companions:\n      - name: primary\n        repository: " + repo + "\n",
		"uppercase alias":  "    companions:\n      - name: Application\n        repository: " + repo + "\n",
		"duplicate source": "    companions:\n      - name: application\n        repository: " + repo + "\n",
	} {
		if _, err := parseSessionPolicies(
			[]byte(prefix+companions+suffix), nil,
		); err == nil {
			t.Fatalf("%s companion unexpectedly accepted", name)
		}
	}
}

func TestParseSessionPoliciesBoundsCompanionCount(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	var companions string
	for index := 0; index <= sessionPolicyMaxCompanions; index++ {
		companion, companionGit := gitRepo(t)
		companionGit("commit", "-q", "--allow-empty", "-m", "base")
		companion, err = filepath.EvalSymlinks(companion)
		if err != nil {
			t.Fatal(err)
		}
		companions += fmt.Sprintf(
			"      - name: repo%d\n        repository: %s\n",
			index, companion,
		)
	}
	body := "version: 1\npolicies:\n  responder:\n    repository: " + repo +
		"\n    companions:\n" + companions +
		"    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n" +
		"    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n"
	if _, err := parseSessionPolicies([]byte(body), nil); err == nil ||
		!strings.Contains(err.Error(), "limited to 32") {
		t.Fatalf("oversized companion set error = %v", err)
	}
}

func TestLoadSessionPoliciesRejectsUnsafeFileAndAncestry(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	body := "version: 1\npolicies:\n  responder:\n    repository: " + repo + "\n    target: codex@work\n    max_turns: 1\n    max_queued_turns: 1\n    max_queued_bytes: 1\n    max_patch_bytes: 1\n    turn_timeout: 1s\n"
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session-policies.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(path, nil); err != nil {
		t.Fatalf("normal policy file rejected: %v", err)
	}
	symlink := filepath.Join(root, "policy-link.yaml")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(symlink, nil); err == nil {
		t.Fatal("policy symlink was accepted")
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(path, nil); err == nil {
		t.Fatal("group/world-writable policy file was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	unsafeDir := filepath.Join(root, "unsafe")
	if err := os.Mkdir(unsafeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	unsafePath := filepath.Join(unsafeDir, "session-policies.yaml")
	if err := os.WriteFile(unsafePath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafeDir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPolicies(unsafePath, nil); err == nil {
		t.Fatal("group/world-writable policy ancestry was accepted")
	}
}

func TestSessionServiceCreateReplayUsesPersistedIntentAndWorkspaceBase(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	root := t.TempDir()
	policies := testSessionPolicies(repo)
	service := newTestSessionService(t, filepath.Join(root, "state"), policies, nil)
	defer service.Stop()
	request := CreateRemoteSessionRequest{Policy: "responder", Task: "task-1"}
	op, replay, err := service.Store().ReserveOperation(context.Background(), "CreateRemoteSession", "create-1", request)
	if err != nil || replay {
		t.Fatalf("reserve create = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent := sessionCreateIntent{
		OperationID: op.ID, Policy: policies["responder"], Task: request.Task,
		SessionID: deterministicSessionID(op.ID), ForkName: deterministicForkName(op.ID), BaseCommit: base,
	}
	intentBytes, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store().MarkOperationRunning(context.Background(), op.ID, intentBytes); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "--allow-empty", "-m", "parent advanced")
	sess, err := service.CreateRemoteSession(context.Background(), "create-1", request)
	if err != nil {
		t.Fatal(err)
	}
	policy := policies["responder"]
	if sess.BaseCommit != base || sess.ID != intent.SessionID || sess.ForkName != intent.ForkName ||
		sess.PolicyDigest != resolvedSessionPolicyDigest(policy) || sess.TurnTimeout != policy.TurnTimeout || sess.MaxPatchBytes != policy.MaxPatchBytes {
		t.Fatalf("replayed session = %+v, intent=%+v", sess, intent)
	}
	if got := gitOut(sess.Workspace, "rev-parse", "HEAD"); got != base {
		t.Fatalf("workspace HEAD = %s, want persisted base %s", got, base)
	}
	replayed, err := service.CreateRemoteSession(context.Background(), "create-1", request)
	if err != nil || replayed.ID != sess.ID || replayed.Workspace != sess.Workspace {
		t.Fatalf("create replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServicePinsPersistsAndDiscardsCompanionRepositories(t *testing.T) {
	primary, primaryGit := gitRepo(t)
	primaryGit("commit", "-q", "--allow-empty", "-m", "primary base")
	companion, companionGit := gitRepo(t)
	if err := os.WriteFile(
		filepath.Join(companion, "topology.txt"), []byte("v1\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	companionGit("add", "topology.txt")
	companionGit("commit", "-qm", "companion base")
	companionBase := gitOut(companion, "rev-parse", "HEAD")
	policies := testSessionPolicies(primary)
	policy := policies["responder"]
	policy.Companions = []SessionCompanionPolicy{{
		Name: "topology", Repository: companion,
	}}
	policies["responder"] = policy
	service := newTestSessionService(
		t, filepath.Join(t.TempDir(), "state"), policies, nil,
	)
	defer service.Stop()

	created, err := service.CreateRemoteSession(
		context.Background(), "create-companion",
		CreateRemoteSessionRequest{Policy: "responder", Task: "multi-repo"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Companions) != 1 ||
		created.Companions[0].Name != "topology" ||
		created.Companions[0].BaseCommit != companionBase ||
		created.Companions[0].Workspace == companion {
		t.Fatalf("created companion binding = %+v", created.Companions)
	}
	if branch := gitOut(created.Companions[0].Workspace, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
		t.Fatalf("companion snapshot branch = %q, want detached HEAD", branch)
	}
	if got := readFile(
		t, filepath.Join(created.Companions[0].Workspace, "topology.txt"),
	); got != "v1\n" {
		t.Fatalf("companion snapshot = %q", got)
	}
	if err := os.WriteFile(
		filepath.Join(companion, "topology.txt"), []byte("v2\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	companionGit("commit", "-qam", "advance companion")
	if got := readFile(
		t, filepath.Join(created.Companions[0].Workspace, "topology.txt"),
	); got != "v1\n" {
		t.Fatalf("pinned companion changed with source = %q", got)
	}
	persisted, err := service.GetSession(context.Background(), created.ID)
	if err != nil || len(persisted.Companions) != 1 ||
		persisted.Companions[0] != created.Companions[0] {
		t.Fatalf("persisted companion = %+v, %v", persisted.Companions, err)
	}
	public := publicSession(persisted)
	if len(public.Companions) != 1 ||
		public.Companions[0].Path != "/coop/repositories/topology" ||
		public.Companions[0].BaseCommit != companionBase {
		t.Fatalf("public companion = %+v", public.Companions)
	}

	closed, err := service.Close(
		context.Background(), "close-companion",
		session.CloseSessionRequest{
			SessionID: created.ID, ExpectedRevision: created.Revision,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(
		context.Background(), "plan-companion",
		PlanDiscardRequest{
			SessionID: created.ID, ExpectedRevision: closed.Revision,
		},
	)
	if err != nil || len(plan.Plan.Companions) != 1 {
		t.Fatalf("companion discard plan = %+v, %v", plan, err)
	}
	discarded, err := service.Discard(
		context.Background(), "discard-companion",
		DiscardRequest{PlanOperationID: plan.OperationID},
	)
	if err != nil || discarded.State != session.SessionDiscarded ||
		pathExists(created.Companions[0].Workspace) {
		t.Fatalf("companion discard = %+v, %v", discarded, err)
	}
}

func TestSessionServiceFIFOOneWorkerAndCancel(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var mu sync.Mutex
	var prompts []string
	var running, maxRunning atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var fakeStore *session.Store
	runner := SessionRunnerFunc(func(ctx context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
		n := running.Add(1)
		for {
			old := maxRunning.Load()
			if n <= old || maxRunning.CompareAndSwap(old, n) {
				break
			}
		}
		mu.Lock()
		prompts = append(prompts, turn.Prompt)
		mu.Unlock()
		if turn.Prompt == "cancel" {
			started <- struct{}{}
			<-ctx.Done()
		} else if turn.Prompt == "first" {
			started <- struct{}{}
			<-release
		}
		defer running.Add(-1)
		if err := ctx.Err(); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSendIntent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		return fakeStore.CompleteTurn(context.Background(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: turn.Prompt})
	})
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), func(store *session.Store) SessionRunner {
		fakeStore = store
		return runner
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "fifo"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SubmitTurn(context.Background(), "turn-1", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SubmitTurn(context.Background(), "turn-2", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "second"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if got := service.Store(); got == nil {
		t.Fatal("service lost its store")
	}
	close(release)
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, second.ID)
		return err == nil && got.State == session.TurnCompleted
	})
	if maxRunning.Load() != 1 {
		t.Fatalf("maximum concurrent workers = %d", maxRunning.Load())
	}
	mu.Lock()
	if len(prompts) != 2 || prompts[0] != "first" || prompts[1] != "second" {
		t.Fatalf("FIFO prompts = %v", prompts)
	}
	mu.Unlock()

	third, err := service.SubmitTurn(context.Background(), "turn-3", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancelled, err := service.CancelTurn(context.Background(), "cancel-3", session.CancelTurnRequest{SessionID: sess.ID, TurnID: third.ID, ExpectedRevision: sess.Revision})
	if err != nil || cancelled.State != session.TurnCancelled {
		t.Fatalf("active cancellation = %+v, err=%v", cancelled, err)
	}
	replayedCancel, err := service.CancelTurn(context.Background(), "cancel-3", session.CancelTurnRequest{SessionID: sess.ID, TurnID: third.ID, ExpectedRevision: sess.Revision})
	if err != nil || replayedCancel.ID != cancelled.ID || replayedCancel.State != session.TurnCancelled {
		t.Fatalf("active cancellation replay = %+v, err=%v", replayedCancel, err)
	}
}

func TestSessionServiceQueuedCancelReplaysAfterRevisionChange(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "queued-cancel"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	req := session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}
	cancelled, err := service.CancelTurn(context.Background(), "cancel-queued", req)
	if err != nil || cancelled.State != session.TurnCancelled {
		t.Fatalf("queued cancellation = %+v, err=%v", cancelled, err)
	}
	replayed, err := service.CancelTurn(context.Background(), "cancel-queued", req)
	if err != nil || replayed.ID != cancelled.ID || replayed.State != session.TurnCancelled {
		t.Fatalf("queued cancellation replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceCancelNaturalCompletionReplaysObservedTerminalTurn(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	started := make(chan struct{})
	release := make(chan struct{})
	var fakeStore *session.Store
	runner := SessionRunnerFunc(func(_ context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
		if _, err := fakeStore.MarkTurnSendIntent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		close(started)
		<-release
		return fakeStore.CompleteTurn(context.Background(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: "natural"})
	})
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), func(store *session.Store) SessionRunner {
		fakeStore = store
		return runner
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "natural-race"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "natural"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	req := session.CancelTurnRequest{SessionID: sess.ID, TurnID: turn.ID, ExpectedRevision: sess.Revision}
	result := make(chan struct {
		turn session.Turn
		err  error
	}, 1)
	go func() {
		cancelled, err := service.CancelTurn(context.Background(), "cancel-natural", req)
		result <- struct {
			turn session.Turn
			err  error
		}{cancelled, err}
	}()
	waitForSessionTest(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		active := service.active[turn.ID]
		return active != nil && active.requested
	})
	close(release)
	response := <-result
	if response.err != nil || response.turn.State != session.TurnCompleted || response.turn.AssistantMessage != "natural" {
		t.Fatalf("natural completion won cancellation = %+v, err=%v", response.turn, response.err)
	}
	replayed, err := service.CancelTurn(context.Background(), "cancel-natural", req)
	if err != nil || replayed.ID != response.turn.ID || replayed.State != session.TurnCompleted || replayed.AssistantMessage != "natural" {
		t.Fatalf("natural completion cancellation replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceStopWaitsForWorkerBeforeClosingStore(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	started := make(chan struct{})
	release := make(chan struct{})
	runner := SessionRunnerFunc(func(ctx context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
		close(started)
		<-ctx.Done()
		<-release
		return turn, ctx.Err()
	})
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: testSessionPolicies(repo),
		Runner: runner, StopTimeout: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SubmitTurn(context.Background(), "turn", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "stop"}); err != nil {
		t.Fatal(err)
	}
	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- service.Stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned while worker was still blocked: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.workers) != 0 || len(service.active) != 0 {
		t.Fatalf("stopped service retained workers=%d active=%d", len(service.workers), len(service.active))
	}
}

type startupCleaningRunner struct {
	cleaned []string
	reaped  []string
	err     error
	reapErr error
}

func (*startupCleaningRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	return turn, nil
}

func (r *startupCleaningRunner) CleanupSession(_ context.Context, sess session.Session) error {
	r.cleaned = append(r.cleaned, sess.ID)
	return r.err
}

func (r *startupCleaningRunner) ReapInterruptedTurn(_ context.Context, _ session.Session, turn session.Turn) error {
	r.reaped = append(r.reaped, turn.ID)
	return r.reapErr
}

func TestSessionServiceRunsStartupCleanupBeforeWorkers(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &startupCleaningRunner{}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Policies:  testSessionPolicies(repo),
		Runner:    runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	var newest string
	for i := 0; i < 105; i++ {
		sess, err := service.Store().CreateSession(context.Background(), fmt.Sprintf("create-%d", i), session.CreateSessionRequest{Target: "codex@work"})
		if err != nil {
			t.Fatal(err)
		}
		newest = sess.ID
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.cleaned) != 105 || runner.cleaned[len(runner.cleaned)-1] != newest {
		t.Fatalf("startup cleanup count = %d newest=%q, want 105 and %q", len(runner.cleaned), runner.cleaned[len(runner.cleaned)-1], newest)
	}
}

func TestSessionServiceCleanupFailureDoesNotBrickStartup(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &startupCleaningRunner{err: errors.New("old provider is unavailable")}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Policies:  testSessionPolicies(repo),
		Runner:    runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	if _, err := service.Store().CreateSession(context.Background(), "create", session.CreateSessionRequest{Target: "removed-provider@old"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("startup failed because one historical cleanup failed: %v", err)
	}
}

type closedCleaningRunner struct {
	startupCalls atomic.Int32
	closedCalls  atomic.Int32
}

func (*closedCleaningRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	return turn, nil
}

func (r *closedCleaningRunner) CleanupSession(_ context.Context, _ session.Session) error {
	r.startupCalls.Add(1)
	return nil
}

func (r *closedCleaningRunner) CleanupClosedSession(_ context.Context, _ session.Session) error {
	r.closedCalls.Add(1)
	return nil
}

func TestSessionServiceCloseUsesKnownRuntimeCleanupWithoutStartupScan(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &closedCleaningRunner{}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"),
		Policies:  testSessionPolicies(repo),
		Runner:    runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create-close-cleanup", CreateRemoteSessionRequest{
		Policy: "responder", Task: "close cleanup",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision}
	closed, err := service.Close(context.Background(), "close-cleanup", req)
	if err != nil || closed.State != session.SessionClosed {
		t.Fatalf("close = %+v, err=%v", closed, err)
	}
	if _, err := service.Close(context.Background(), "close-cleanup", req); err != nil {
		t.Fatalf("close replay = %v", err)
	}
	if got := runner.closedCalls.Load(); got != 1 {
		t.Fatalf("closed cleanup calls = %d, want 1", got)
	}
	if got := runner.startupCalls.Load(); got != 0 {
		t.Fatalf("startup cleanup calls during close = %d, want 0", got)
	}
}

type periodicCleanupRunner struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (r *periodicCleanupRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	close(r.started)
	<-r.release
	return turn, errors.New("test turn complete")
}

func (r *periodicCleanupRunner) CleanupSession(_ context.Context, _ session.Session) error {
	r.calls.Add(1)
	return nil
}

func TestSessionServiceRetriesParkedCleanupWithoutRacingActiveTurn(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &periodicCleanupRunner{started: make(chan struct{}), release: make(chan struct{})}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot:       filepath.Join(t.TempDir(), "state"),
		Policies:        testSessionPolicies(repo),
		Runner:          runner,
		CleanupInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create-periodic-cleanup", CreateRemoteSessionRequest{Policy: "responder", Task: "periodic cleanup"})
	if err != nil {
		t.Fatal(err)
	}
	waitForSessionTest(t, func() bool { return runner.calls.Load() > 0 })

	turn, err := service.SubmitTurn(context.Background(), "run-during-cleanup", session.SubmitTurnRequest{
		SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "run",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-runner.started
	during := runner.calls.Load()
	time.Sleep(40 * time.Millisecond)
	if got := runner.calls.Load(); got != during {
		t.Fatalf("parked cleanup raced active turn: calls changed from %d to %d", during, got)
	}
	close(runner.release)
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, turn.ID)
		return err == nil && got.State == session.TurnFailed
	})
	waitForSessionTest(t, func() bool { return runner.calls.Load() > during })
}

func TestSessionServiceWorkerRetiresParkedSessionAndStartsAgain(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var fakeStore *session.Store
	var calls atomic.Int32
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	runner := SessionRunnerFunc(func(_ context.Context, bound session.Session, turn session.Turn) (session.Turn, error) {
		calls.Add(1)
		if _, err := fakeStore.MarkTurnSendIntent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if _, err := fakeStore.MarkTurnSent(context.Background(), bound.ID, turn.ID); err != nil {
			return turn, err
		}
		if turn.Prompt == "second" {
			close(secondStarted)
			<-releaseSecond
		}
		return fakeStore.CompleteTurn(context.Background(), session.CompleteTurnRequest{SessionID: bound.ID, TurnID: turn.ID, Message: turn.Prompt})
	})
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), func(store *session.Store) SessionRunner {
		fakeStore = store
		return runner
	})
	defer service.Stop()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(context.Background(), "create-worker-lifecycle", CreateRemoteSessionRequest{Policy: "responder", Task: "worker-lifecycle"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.SubmitTurn(context.Background(), "worker-first", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, first.ID)
		return err == nil && got.State == session.TurnCompleted
	})
	waitForSessionTest(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return len(service.workers) == 0
	})
	current, err := service.GetSession(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SubmitTurn(context.Background(), "worker-second", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: current.Revision, Prompt: "second"})
	if err != nil {
		t.Fatal(err)
	}
	<-secondStarted
	service.mu.Lock()
	workerCount := len(service.workers)
	service.mu.Unlock()
	if workerCount != 1 {
		t.Fatalf("later turn worker count = %d, want a new worker", workerCount)
	}
	close(releaseSecond)
	waitForSessionTest(t, func() bool {
		got, err := service.GetTurn(context.Background(), sess.ID, second.ID)
		return err == nil && got.State == session.TurnCompleted
	})
	waitForSessionTest(t, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return len(service.workers) == 0
	})
	if calls.Load() != 2 {
		t.Fatalf("runner calls = %d, want 2", calls.Load())
	}
}

func TestSessionServiceRecoveryCleanupFailureLeavesTurnActiveForRetry(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	runner := &startupCleaningRunner{reapErr: errors.New("runtime unavailable")}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: testSessionPolicies(repo), Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	sess, err := service.Store().CreateSession(context.Background(), "create-recovery", session.CreateSessionRequest{Target: "codex@work"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := service.Store().SubmitTurn(context.Background(), "turn-recovery", session.SubmitTurnRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision, Prompt: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	leased, ok, err := service.Store().LeaseNextTurn(context.Background(), sess.ID)
	if err != nil || !ok {
		t.Fatalf("lease recovery turn = %+v, ok=%v, err=%v", leased, ok, err)
	}
	if _, err := service.Store().MarkTurnSendIntent(context.Background(), sess.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background()); err == nil {
		t.Fatal("startup succeeded despite interrupted runtime cleanup failure")
	}
	active, err := service.Store().GetTurn(context.Background(), sess.ID, turn.ID)
	if err != nil || active.State != session.TurnStarting {
		t.Fatalf("turn after failed recovery = %+v, err=%v", active, err)
	}
	runner.reapErr = nil
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("startup retry = %v", err)
	}
	recovered, err := service.Store().GetTurn(context.Background(), sess.ID, turn.ID)
	if err != nil || recovered.State != session.TurnInterrupted {
		t.Fatalf("recovered sent-intent turn = %+v, err=%v", recovered, err)
	}
	if len(runner.reaped) != 2 || runner.reaped[0] != turn.ID || runner.reaped[1] != turn.ID {
		t.Fatalf("reaped turns = %v, want two retries for %s", runner.reaped, turn.ID)
	}
}

func TestSessionServiceDiscardPlanAndReplay(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "discard"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(context.Background(), "close", session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(context.Background(), "plan", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if plan.OperationID == "" || plan.Plan.Workspace.Head == "" {
		t.Fatalf("discard plan = %+v", plan)
	}
	if _, err := service.PlanDiscard(context.Background(), "plan", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision}); err != nil {
		t.Fatalf("plan replay = %v", err)
	}
	sessionWorkspaceGit(t, plan.Plan.Workspace.Workspace, "commit", "--allow-empty", "-qm", "head changed")
	if _, err := service.Discard(context.Background(), "stale-discard", DiscardRequest{PlanOperationID: plan.OperationID}); session.CodeOf(err) != session.CodeDiscardPlanStale {
		t.Fatalf("stale discard error = %v", err)
	}
	plan, err = service.PlanDiscard(context.Background(), "plan-2", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision, AcceptUnmerged: true})
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := service.Discard(context.Background(), "discard", DiscardRequest{PlanOperationID: plan.OperationID})
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("discard = %+v, err=%v", discarded, err)
	}
	replayed, err := service.Discard(context.Background(), "discard", DiscardRequest{PlanOperationID: plan.OperationID})
	if err != nil || replayed.ID != discarded.ID || pathExists(plan.Plan.Workspace.Workspace) {
		t.Fatalf("discard replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceDiscardReplayAfterWorkspaceRemovalBeforeTombstone(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create", CreateRemoteSessionRequest{Policy: "responder", Task: "discard-crash"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(context.Background(), "close", session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(context.Background(), "plan", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	discardRequest := DiscardRequest{PlanOperationID: plan.OperationID}
	op, replay, err := service.Store().ReserveOperation(context.Background(), "Discard", "discard-crash", discardRequest)
	if err != nil || replay {
		t.Fatalf("reserve discard = %+v, replay=%v, err=%v", op, replay, err)
	}
	intent, err := json.Marshal(discardIntent{Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store().MarkOperationRunning(context.Background(), op.ID, intent); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(plan.Plan.Workspace.Workspace); err != nil {
		t.Fatal(err)
	}
	discarded, err := service.Discard(context.Background(), "discard-crash", discardRequest)
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("replayed missing-workspace discard = %+v, err=%v", discarded, err)
	}
	replayed, err := service.Discard(context.Background(), "discard-crash", discardRequest)
	if err != nil || replayed.State != session.SessionDiscarded {
		t.Fatalf("discard tombstone replay = %+v, err=%v", replayed, err)
	}
}

func TestSessionServiceDiscardReplaysAfterPostDeleteCleanupFailure(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	stateRoot := filepath.Join(t.TempDir(), "state")
	service := newTestSessionService(t, stateRoot, testSessionPolicies(repo), nil)
	defer service.Stop()
	sess, err := service.CreateRemoteSession(context.Background(), "create-post-delete", CreateRemoteSessionRequest{Policy: "responder", Task: "post-delete"})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := service.Close(context.Background(), "close-post-delete", session.CloseSessionRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanDiscard(context.Background(), "plan-post-delete", PlanDiscardRequest{SessionID: sess.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	privateRoot := filepath.Join(stateRoot, "acp")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	privateState := filepath.Join(privateRoot, sess.ID)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outside, privateState); err != nil {
		t.Fatal(err)
	}
	request := DiscardRequest{PlanOperationID: plan.OperationID}
	if _, err := service.Discard(context.Background(), "discard-post-delete", request); !errors.Is(err, session.ErrOperationUncertain) {
		t.Fatalf("post-delete cleanup error = %v, want operation_uncertain", err)
	}
	op, err := service.Store().GetOperation(context.Background(), "discard-post-delete")
	if err != nil || op.State != session.OperationRunning {
		t.Fatalf("post-delete operation = %+v, err=%v, want running", op, err)
	}
	if pathExists(plan.Plan.Workspace.Workspace) {
		t.Fatal("workspace survived before cleanup repair")
	}
	if err := os.Remove(privateState); err != nil {
		t.Fatal(err)
	}
	discarded, err := service.Discard(context.Background(), "discard-post-delete", request)
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("repaired discard replay = %+v, err=%v", discarded, err)
	}
	op, err = service.Store().GetOperation(context.Background(), "discard-post-delete")
	if err != nil || op.State != session.OperationSucceeded {
		t.Fatalf("repaired discard operation = %+v, err=%v", op, err)
	}
}

func testSessionPolicies(repo string) map[string]SessionPolicy {
	return map[string]SessionPolicy{"responder": {
		Name: "responder", Repository: repo, Target: "codex@work", MaxTurns: 10,
		MaxQueuedTurns: 5, MaxQueuedBytes: 1 << 20, MaxPatchBytes: 1 << 20, TurnTimeout: time.Second,
	}}
}

func newTestSessionService(t *testing.T, stateRoot string, policies map[string]SessionPolicy, factory SessionRunnerFactory) *SessionService {
	t.Helper()
	cfg := SessionServiceConfig{StateRoot: stateRoot, Policies: policies, RunnerFactory: factory}
	if factory == nil {
		cfg.Runner = SessionRunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			return turn, nil
		})
	}
	service, err := NewSessionService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func waitForSessionTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session condition did not become true")
}
