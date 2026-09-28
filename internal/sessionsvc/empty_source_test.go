package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/mcp"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestEmptySourceJobOwnsPrivateWorkspaceAndSurvivesRestart(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "state")
	open := func() *Service {
		t.Helper()
		s, err := newSessionServiceWithTestStorage(t, Config{StateRoot: root, SourceConfig: &config.Config{ConfigDir: t.TempDir()},
			Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
				return turn, nil
			})})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	ctx := context.Background()
	job := bareWorkerJob()
	job.Mode, job.RepositoryReadOnly, job.Egress.Mode = "normal", true, "open"
	document, _ := json.Marshal(job)
	digest, _ := job.Digest()
	req := CreateRemoteSessionRequest{Task: "empty-workspace", Job: document, ExpectedJobDigest: digest}
	s := open()
	defer func() { _ = s.Stop() }()
	first, err := s.CreateRemoteSession(ctx, "empty-create", req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Source != nil || first.Workspace == "" || first.Repository == "" ||
		first.Workspace == first.Repository || !validSessionWorkspaceCommit(first.BaseCommit) {
		t.Fatalf("empty workspace authority = %+v", first)
	}
	entries, err := os.ReadDir(first.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".git" && entry.Name() != forkspace.GenerationMarkerName {
			t.Fatalf("empty workspace contains %s", entry.Name())
		}
	}
	if len(first.RepositoryFreshness) != 1 {
		t.Fatalf("empty source freshness = %+v", first.RepositoryFreshness)
	}
	fresh := first.RepositoryFreshness[0]
	if fresh.Version != 2 || fresh.Name != "primary" || fresh.RemoteIdentity != "local" ||
		fresh.RequestedRevision != "HEAD" || fresh.ResolvedRevision != first.BaseCommit ||
		fresh.WorkspaceBaseRevision != first.BaseCommit || fresh.FetchedAt.IsZero() ||
		fresh.StaleBaseStatus != "not_applicable" {
		t.Fatalf("empty source freshness = %+v", fresh)
	}
	second, err := s.CreateRemoteSession(ctx, "empty-create-two", req)
	if err != nil || second.Workspace == first.Workspace || second.Repository != first.Repository {
		t.Fatalf("separate session workspace = %+v, %v", second, err)
	}
	if _, err := s.sessionExecution(ctx, first); err != nil {
		t.Fatal(err)
	}
	if head, err := s.pinCurrentSessionParent(ctx, first); err != nil || head != first.BaseCommit {
		t.Fatalf("empty baseline refresh = %q, %v", head, err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	s = open()
	replay, err := s.CreateRemoteSession(ctx, "empty-create", req)
	if err != nil || replay.ID != first.ID || !reflect.DeepEqual(replay.RepositoryFreshness, first.RepositoryFreshness) {
		t.Fatalf("empty create replay = %+v, %v", replay, err)
	}
	stored, err := s.store.GetSession(ctx, replay.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessionExecution(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if changes, err := s.GetChanges(ctx, second.ID); err != nil || changes.BaseCommit != first.BaseCommit {
		t.Fatalf("empty workspace changes = %+v, %v", changes, err)
	}
	s.reviewGate = ReviewGateFunc(func(context.Context, string, string) (ReviewGateResult, error) {
		return ReviewGateResult{Configured: true, Passed: true}, nil
	})
	if review, err := s.RunReview(ctx, "empty-review", RunReviewRequest{SessionID: second.ID, ExpectedRevision: second.Revision}); err != nil || review.ParentHead != first.BaseCommit {
		t.Fatalf("empty workspace review = %+v, %v", review, err)
	}
	closed, err := s.Close(ctx, "empty-close", session.CloseSessionRequest{SessionID: second.ID, ExpectedRevision: second.Revision})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanDiscard(ctx, "empty-plan", PlanDiscardRequest{SessionID: second.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Discard(ctx, "empty-discard", DiscardRequest{PlanOperationID: plan.OperationID}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Repository, "unexpected"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessionExecution(ctx, stored); err == nil {
		t.Fatal("adopted changed empty baseline")
	}
}

func TestEmptySourceJobRetainsCompanionsToolsAndSemanticValidation(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "companion")
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer fixture.Stop()
	fixture.Job.Source = nil
	fixture.Job.RepositoryReadOnly = true
	fixture.Job.Targets = []string{"codex@work"}
	fixture.companion(t, "library", repo)
	ctx := context.Background()
	req := fixture.request(t, "empty-with-tools")
	req.ControllerTools = &session.ControllerTools{Endpoint: "https://controller.example/mcp", Token: strings.Repeat("b", 48)}
	created, err := fixture.CreateRemoteSession(ctx, "empty-tools-create", req)
	if err != nil {
		t.Fatal(err)
	}
	if created.ControllerToolsDigest != session.ControllerToolsDigest(req.ControllerTools) ||
		len(created.Companions) != 1 || created.Companions[0].Name != "library" ||
		len(created.RepositoryFreshness) != 2 || created.RepositoryFreshness[1].Name != "library" {
		t.Fatalf("empty source lost authorized tools or companions: %+v", created)
	}
	schema := json.RawMessage(`{"type":"object"}`)
	digest := sha256.Sum256(schema)
	_, err = fixture.SubmitTurn(ctx, "empty-semantic", session.SubmitTurnRequest{
		SessionID: created.ID, ExpectedRevision: created.Revision, Prompt: "inspect the companion",
		OutputContract: &session.OutputContract{JSONSchema: schema, SHA256: fmt.Sprintf("%x", digest), RequireSemanticValidation: true},
	})
	if err != nil {
		t.Fatalf("repository-free job cannot request semantic validation: %v", err)
	}
	acp := newSessionACPFixture(t, "normal")
	target, err := agents.ParseTarget(created.Target)
	if err != nil {
		t.Fatal(err)
	}
	agent, ok := agents.Get(target.Provider)
	if !ok {
		t.Fatal("test agent unavailable")
	}
	projection, err := acp.runner.projectCredentials(created, target, agent, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer projection.remove()
	env := readFile(t, filepath.Join(projection.privateRoot, "env"))
	mcpConfig := readFile(t, filepath.Join(projection.privateRoot, "mcp.json"))
	if env != mcp.ControllerToolsTokenEnv+"="+req.ControllerTools.Token+"\n" ||
		!strings.Contains(mcpConfig, req.ControllerTools.Endpoint) || strings.Contains(mcpConfig, req.ControllerTools.Token) {
		t.Fatal("empty-workspace job lost isolated controller-tool projection")
	}
}

func TestEmptySourceConcurrentInitializationAndTamperRefusal(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	root := t.TempDir()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := emptyJobRepository(context.Background(), root); err != nil {
				t.Errorf("initialize shared baseline: %v", err)
			}
		})
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	repository, err := emptyJobRepository(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := forkspace.GitRefCommand(context.Background(), repository, "remote", "add", "origin", "https://example.invalid/changed").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := emptyJobRepository(context.Background(), root); err == nil {
		t.Fatal("adopted empty baseline with a remote")
	}
}
