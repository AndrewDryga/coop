package sessionsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestControllerJobUsesAnExactPrivateSourceWithoutSharedRepositoryPaths(t *testing.T) {
	upstream, git := gitrepo.New(t)
	if err := os.MkdirAll(filepath.Join(upstream, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstream, ".agent", "project.yaml"), []byte("box: [invalid local policy]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".agent/project.yaml")
	git("commit", "-q", "-m", "base")
	commit, err := sessionWorkspaceCommitContext(context.Background(), upstream, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sessionWorkspaceTree(upstream, commit)
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/main"
	source := workerproto.JobSource{
		RepositoryRef: "repo:one", GitHubRepository: "example/repository", GitHubRepositoryID: 17,
		Binding: session.SourceBinding{Version: 1, Kind: session.SourceDefault, Requested: session.DefaultSourceSelector(), RemoteIdentity: "origin",
			DefaultRef: ref, SelectedRef: &ref, DefaultCommit: commit, SelectedCommit: commit, BaseCommit: commit, AdmittedTree: tree, ResolvedAt: time.Now().UTC()},
		Submodules: []workerproto.JobSubmodule{},
	}
	stateRoot := filepath.Join(t.TempDir(), "worker-state")
	service, err := newSessionServiceWithTestStorage(t, Config{
		StateRoot:    stateRoot,
		SourceConfig: &config.Config{ConfigDir: t.TempDir()},
		Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			return turn, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	key, err := source.StagingKey()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(stateRoot, "job-sources", key)
	repository := filepath.Join(directory, "repository")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "clone", "--quiet", "--no-local", upstream, repository)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("clone: %s %v", output, err)
	}
	if err := os.Chmod(repository, 0700); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(source)
	if err := os.WriteFile(filepath.Join(directory, "source.json"), receipt, 0600); err != nil {
		t.Fatal(err)
	}
	job := workerproto.JobSpec{
		Version: 1, JobRef: "job:source", Source: &source, Companions: []workerproto.JobCompanion{},
		Targets: []string{"codex"}, Mode: "normal", RepositoryReadOnly: true,
		Egress: workerproto.JobEgress{Mode: "open", Rules: []workerproto.JobRule{}},
		Limits: workerproto.JobLimits{MaxTurns: 2, MaxQueuedTurns: 1, MaxQueuedBytes: 4096,
			TurnTimeoutMS: 60_000, MaxPatchBytes: 1024},
	}
	document, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := job.Digest()
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateRemoteSession(context.Background(), "source-job-create", CreateRemoteSessionRequest{
		Task: job.JobRef, Job: document, ExpectedJobDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.BaseCommit != commit || created.Repository == upstream || created.Repository == "" ||
		created.Workspace == "" || created.JobDigest != digest {
		t.Fatalf("imported job session = %+v", created)
	}
	if resolved, err := sessionWorkspaceCommitContext(context.Background(), created.Repository, "HEAD"); err != nil || resolved != commit {
		t.Fatalf("private imported repository commit = %s, err=%v", resolved, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "source.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	job.JobRef = "job:tampered"
	document, _ = json.Marshal(job)
	digest, _ = job.Digest()
	if _, err := service.CreateRemoteSession(context.Background(), "tampered-source-create", CreateRemoteSessionRequest{
		Task: job.JobRef, Job: document, ExpectedJobDigest: digest,
	}); err == nil {
		t.Fatal("created a session from changed staged source bytes")
	}
}

func bareWorkerJob() workerproto.JobSpec {
	return workerproto.JobSpec{
		Version: 1, JobRef: "job:route", Companions: []workerproto.JobCompanion{},
		Targets: []string{"codex"}, Mode: "bare",
		Egress: workerproto.JobEgress{Mode: "none", Rules: []workerproto.JobRule{}},
		Limits: workerproto.JobLimits{
			MaxTurns: 2, MaxQueuedTurns: 1, MaxQueuedBytes: 4096,
			TurnTimeoutMS: 60_000, MaxPatchBytes: 1024,
		},
	}
}

func TestSessionExecutionRejectsChangedFrozenSourceProjections(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer fixture.Stop()
	fixture.companion(t, "docs", repo)
	ctx := context.Background()
	created, err := fixture.CreateRemoteSession(ctx, "create-projections", fixture.request(t, "test:projections"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.sessionExecution(ctx, created); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*session.Session){
		"source":              func(s *session.Session) { s.Source.SelectedCommit = strings.Repeat("a", 40) },
		"missing source":      func(s *session.Session) { s.Source = nil },
		"base":                func(s *session.Session) { s.BaseCommit = strings.Repeat("b", 40) },
		"job ref":             func(s *session.Session) { s.JobRef = "other" },
		"job digest":          func(s *session.Session) { s.JobDigest = "other" },
		"job document":        func(s *session.Session) { s.JobDocument = nil },
		"missing companion":   func(s *session.Session) { s.Companions = nil },
		"companion name":      func(s *session.Session) { s.Companions[0].Name = "other" },
		"companion source":    func(s *session.Session) { s.Companions[0].Repository = repo },
		"companion workspace": func(s *session.Session) { s.Companions[0].Workspace = repo },
		"companion commit":    func(s *session.Session) { s.Companions[0].BaseCommit = strings.Repeat("c", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			bound := created
			bound.Source = session.CloneSourceBinding(created.Source)
			bound.Companions = append([]session.CompanionRepository(nil), created.Companions...)
			mutate(&bound)
			if _, err := fixture.sessionExecution(ctx, bound); err == nil {
				t.Fatal("changed source projection authorized execution")
			}
		})
	}
	created.MaxTurns++ // Budget extension is mutable, unlike execution authority.
	if _, err := fixture.sessionExecution(ctx, created); err != nil {
		t.Fatalf("extended budget refused: %v", err)
	}
}

func TestControllerJobCreatesWithoutLocalPolicyAndReplaysAfterRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	open := func() *Service {
		t.Helper()
		service, err := newSessionServiceWithTestStorage(t, Config{
			StateRoot:    root,
			SourceConfig: &config.Config{ConfigDir: t.TempDir()},
			Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
				return turn, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	job := bareWorkerJob()
	document, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := job.Digest()
	if err != nil {
		t.Fatal(err)
	}
	req := CreateRemoteSessionRequest{Task: "workspace:reusable-offer", Job: document, ExpectedJobDigest: digest}
	service := open()
	created, err := service.CreateRemoteSession(context.Background(), "job-create", req)
	if err != nil {
		t.Fatal(err)
	}
	if created.JobDigest != digest || created.JobRef != job.JobRef || created.ExternalRef != req.Task || created.Mode != "bare" ||
		created.NetworkMode != "none" || created.ProjectEnv || created.ProjectMCP {
		t.Fatalf("controller job session = %+v", created)
	}
	childEnv, err := networkChildEnvironment(created, "turn-one")
	if err != nil || len(childEnv) != 2 || childEnv[0] != "COOP_CONTROLLER_JOB="+digest || childEnv[1] != "COOP_EGRESS=none" {
		t.Fatalf("controller child environment = %v, %v", childEnv, err)
	}
	stored, err := service.store.GetSession(context.Background(), created.ID)
	if err != nil || len(stored.JobDocument) == 0 {
		t.Fatalf("private job authority = %q, err=%v", stored.JobDocument, err)
	}
	public, err := json.Marshal(created)
	if err != nil || bytes.Contains(public, []byte(`"job_document"`)) {
		t.Fatalf("public session leaked job authority: %s, %v", public, err)
	}
	if err := service.Stop(); err != nil {
		t.Fatal(err)
	}
	service = open()
	defer service.Stop()
	replayed, err := service.CreateRemoteSession(context.Background(), "job-create", req)
	if err != nil || replayed.ID != created.ID || replayed.JobDigest != digest {
		t.Fatalf("job replay after restart = %+v, %v", replayed, err)
	}
	stored, err = service.store.GetSession(context.Background(), created.ID)
	if err != nil || len(stored.JobDocument) == 0 {
		t.Fatalf("restarted worker lost private job authority: %q, %v", stored.JobDocument, err)
	}
	if _, err := service.sessionExecution(context.Background(), stored); err != nil {
		t.Fatalf("independent job and task identities refused execution after restart: %v", err)
	}
	changedTask := req
	changedTask.Task = "workspace:another-offer"
	if _, err := service.CreateRemoteSession(context.Background(), "job-create", changedTask); session.CodeOf(err) != session.CodeIdempotencyConflict {
		t.Fatalf("changed task replay = %v, want idempotency conflict", err)
	}
	changedRef := job
	changedRef.JobRef = "job:another-generation"
	refDocument, _ := json.Marshal(changedRef)
	refDigest, _ := changedRef.Digest()
	if _, err := service.CreateRemoteSession(context.Background(), "job-create", CreateRemoteSessionRequest{
		Task: req.Task, Job: refDocument, ExpectedJobDigest: refDigest,
	}); session.CodeOf(err) != session.CodeIdempotencyConflict {
		t.Fatalf("changed job identity replay = %v, want idempotency conflict", err)
	}
	changed := bareWorkerJob()
	changed.Limits.MaxTurns++
	changedDocument, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	changedDigest, err := changed.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateRemoteSession(context.Background(), "job-create", CreateRemoteSessionRequest{
		Task: req.Task, Job: changedDocument, ExpectedJobDigest: changedDigest,
	}); session.CodeOf(err) != session.CodeIdempotencyConflict {
		t.Fatalf("changed job replay = %v, want idempotency conflict", err)
	}
}

func TestControllerJobRejectsMissingAndMismatchedAuthorityBeforeJournal(t *testing.T) {
	service, err := newSessionServiceWithTestStorage(t, Config{
		StateRoot:    filepath.Join(t.TempDir(), "state"),
		SourceConfig: &config.Config{ConfigDir: t.TempDir()},
		Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			return turn, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	job := bareWorkerJob()
	document, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := job.Digest()
	if err != nil {
		t.Fatal(err)
	}
	unsupported := bareWorkerJob()
	unsupported.Mode = "normal"
	ref := "refs/heads/main"
	commit := strings.Repeat("a", 40)
	unsupported.Source = &workerproto.JobSource{RepositoryRef: "repo:one", GitHubRepository: "example/repository", GitHubRepositoryID: 17,
		Binding: session.SourceBinding{Version: 1, Kind: session.SourceDefault, Requested: session.DefaultSourceSelector(),
			RemoteIdentity: "origin", DefaultRef: ref, SelectedRef: &ref, DefaultCommit: commit, SelectedCommit: commit, BaseCommit: commit,
			AdmittedTree: strings.Repeat("c", 40), ResolvedAt: time.Now().UTC()}, Submodules: []workerproto.JobSubmodule{}}
	unsupported.RepositoryReadOnly = true
	unsupported.Egress = workerproto.JobEgress{Mode: "filtered", Rules: []workerproto.JobRule{{
		To: workerproto.JobDestination{IP: "203.0.113.7"}, Protocol: "tls", Ports: []int{443},
	}}}
	unsupportedDocument, err := json.Marshal(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	unsupportedDigest, err := unsupported.Digest()
	if err != nil {
		t.Fatal(err)
	}
	badTarget := bareWorkerJob()
	badTarget.Targets = []string{"not-an-agent"}
	badTargetDocument, err := json.Marshal(badTarget)
	if err != nil {
		t.Fatal(err)
	}
	badTargetDigest, err := badTarget.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]CreateRemoteSessionRequest{
		"missing job":      {Task: job.JobRef, ExpectedJobDigest: digest},
		"wrong digest":     {Task: job.JobRef, Job: document, ExpectedJobDigest: "bad"},
		"missing task":     {Job: document, ExpectedJobDigest: digest},
		"unsupported rule": {Task: unsupported.JobRef, Job: unsupportedDocument, ExpectedJobDigest: unsupportedDigest},
		"invalid target":   {Task: badTarget.JobRef, Job: badTargetDocument, ExpectedJobDigest: badTargetDigest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.CreateRemoteSessionAsync(context.Background(), name, req); session.CodeOf(err) != session.CodeInvalidRequest {
				t.Fatalf("invalid job request = %v, want invalid request", err)
			}
			if _, err := service.store.GetOperation(context.Background(), name); !errors.Is(err, session.ErrOperationNotFound) {
				t.Fatalf("invalid job request was journaled: %v", err)
			}
		})
	}
}

func TestControllerJobHTTPCreateCarriesAuthorityAndRedactsDocument(t *testing.T) {
	service, err := newSessionServiceWithTestStorage(t, Config{
		StateRoot:    filepath.Join(t.TempDir(), "state"),
		SourceConfig: &config.Config{ConfigDir: t.TempDir()},
		Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			return turn, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	job := bareWorkerJob()
	document, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := job.Digest()
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(CreateRemoteSessionRequest{
		Task: job.JobRef, Job: document, ExpectedJobDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service)
	created := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", string(request), "job-http-create", "application/json")
	if created.Code != http.StatusOK || bytes.Contains(created.Body.Bytes(), document) || bytes.Contains(created.Body.Bytes(), []byte("job_document")) {
		t.Fatalf("job HTTP create = %d %s", created.Code, created.Body.String())
	}
	for _, obsolete := range []string{"policy", "policy_digest", "authority_digest"} {
		if bytes.Contains(created.Body.Bytes(), []byte(`"`+obsolete+`"`)) {
			t.Fatalf("HTTP create still exposes obsolete %s: %s", obsolete, created.Body.String())
		}
	}
	var response sessionMutationSessionResponse
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil || response.Session.ID == "" || response.Session.JobRef != job.JobRef || response.Session.JobDigest != digest {
		t.Fatalf("job HTTP response = %+v, %v", response, err)
	}
	replayed := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", string(request), "job-http-create", "application/json")
	if replayed.Code != http.StatusOK || !bytes.Equal(replayed.Body.Bytes(), created.Body.Bytes()) {
		t.Fatalf("job identity changed on HTTP replay: %d %s", replayed.Code, replayed.Body.String())
	}
	wrong := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", string(bytes.Replace(request, []byte(digest), []byte("wrong"), 1)), "job-http-wrong", "application/json")
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong job HTTP digest = %d %s", wrong.Code, wrong.Body.String())
	}
	if _, err := service.store.GetOperation(context.Background(), "job-http-wrong"); !errors.Is(err, session.ErrOperationNotFound) {
		t.Fatalf("wrong digest was journaled: %v", err)
	}
}
