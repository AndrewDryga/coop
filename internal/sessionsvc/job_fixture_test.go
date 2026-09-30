package sessionsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The fixture authors the same job document a controller sends. It does not intercept
// service calls or admit the retired policy-name request format.
type sessionFixture struct {
	*Service
	Job workerproto.JobSpec
}

func (f *sessionFixture) companion(t *testing.T, name, repository string) {
	t.Helper()
	source := stageTestJobSource(t, f.stateRoot, repository)
	source.RepositoryRef, source.GitHubRepository = "test:"+name, "example/"+name
	source.GitHubRepositoryID = int64(18 + len(f.Job.Companions))
	stageTestJobBinding(t, f.stateRoot, repository, source)
	f.Job.Companions = append(f.Job.Companions, workerproto.JobCompanion{Name: name, Source: source})
}

func (f *sessionFixture) pullSource(t *testing.T, upstream string, number int, selected, defaultCommit, base string) {
	t.Helper()
	source := *f.Job.Source
	ref := fmt.Sprintf("refs/pull/%d/head", number)
	source.Binding.Kind = session.SourcePullRequest
	source.Binding.Requested = session.SourceSelector{Kind: session.SourcePullRequest, Number: number, ExpectedHeadCommit: selected}
	source.Binding.SelectedRef, source.Binding.SelectedCommit = &ref, selected
	source.Binding.DefaultCommit, source.Binding.BaseCommit = defaultCommit, base
	source.Binding.PullRequestNumber, source.Binding.PullRequestExpectedHead = number, selected
	tree, err := sessionWorkspaceTree(upstream, selected)
	if err != nil {
		t.Fatal(err)
	}
	source.Binding.AdmittedTree = tree
	stageTestJobBinding(t, f.stateRoot, upstream, source)
	f.Job.Source = &source
}

func (f *sessionFixture) request(t *testing.T, task string) CreateRemoteSessionRequest {
	t.Helper()
	return jobRequest(t, f.Job, task)
}

func (f *sessionFixture) body(t *testing.T, task string) string {
	t.Helper()
	document, err := json.Marshal(f.request(t, task))
	if err != nil {
		t.Fatal(err)
	}
	return string(document)
}

func jobRequest(t *testing.T, job workerproto.JobSpec, task string) CreateRemoteSessionRequest {
	t.Helper()
	job.JobRef = task
	document, err := job.CanonicalDocument()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := job.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return CreateRemoteSessionRequest{Task: task, Job: document, ExpectedJobDigest: digest}
}

func newSessionFixture(t *testing.T, cfg Config, repository string) *sessionFixture {
	t.Helper()
	fixture, err := openSessionFixture(t, cfg, repository)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func openSessionFixture(t *testing.T, cfg Config, repository string) (*sessionFixture, error) {
	t.Helper()
	if repository != "" && cfg.SourceRefresher == nil {
		cfg.SourceRefresher = func(ctx context.Context, _ string, source workerproto.JobSource, staged string) (string, error) {
			const trackingRef = "refs/coop/current-default"
			command := exec.CommandContext(ctx, "git", "-C", staged, "fetch", "--quiet", "--no-tags", repository, "+"+source.Binding.DefaultRef+":"+trackingRef)
			if output, err := command.CombinedOutput(); err != nil {
				return "", fmt.Errorf("refresh fixture: %s: %w", output, err)
			}
			return sessionWorkspaceCommitContext(ctx, staged, trackingRef)
		}
	}
	service, err := newSessionServiceWithTestStorage(t, cfg)
	if err != nil {
		return nil, err
	}
	job := workerproto.JobSpec{
		Version: 2, JobRef: "test:job", Companions: []workerproto.JobCompanion{},
		Targets: []string{"codex@work"}, Mode: "normal",
		Egress: workerproto.JobEgress{Mode: "open", Rules: []workerproto.JobRule{}},
		Limits: workerproto.JobLimits{MaxTurns: 10, MaxQueuedTurns: 5,
			MaxQueuedBytes: 1 << 20, MaxPatchBytes: 1 << 20, TurnTimeoutMS: 1000},
		Environment: map[string]string{}, Check: workerproto.JobCheck{Argv: []string{}, Environment: map[string]string{}},
		Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256},
	}
	if repository == "" {
		job.Mode = "bare"
	} else {
		source := stageTestJobSource(t, cfg.StateRoot, repository)
		job.Source = &source
	}
	return &sessionFixture{Service: service, Job: job}, nil
}

func stageTestJobSource(t *testing.T, stateRoot, upstream string) workerproto.JobSource {
	t.Helper()
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
		RepositoryRef: "test:repository", GitHubRepository: "example/repository", GitHubRepositoryID: 17,
		Binding: session.SourceBinding{Version: 1, Kind: session.SourceDefault,
			Requested: session.DefaultSourceSelector(), RemoteIdentity: "origin",
			DefaultRef: ref, SelectedRef: &ref, DefaultCommit: commit, SelectedCommit: commit,
			BaseCommit: commit, AdmittedTree: tree, ResolvedAt: time.Unix(1, 0).UTC()},
		Submodules: testJobSubmodules(t, upstream, commit),
	}
	stageTestJobBinding(t, stateRoot, upstream, source)
	return source
}

func stageTestJobBinding(t *testing.T, stateRoot, upstream string, source workerproto.JobSource) {
	t.Helper()
	key, err := source.StagingKey()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(stateRoot, "job-sources", key)
	repository := filepath.Join(directory, "repository")
	if _, err := os.Lstat(directory); os.IsNotExist(err) {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("git", "clone", "--quiet", "--no-local", upstream, repository)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("stage fixture source: %s %v", output, err)
		}
		for _, setting := range [][2]string{{"user.name", "Coop"}, {"user.email", "coop@localhost"}} {
			if output, err := exec.Command("git", "-C", repository, "config", setting[0], setting[1]).CombinedOutput(); err != nil {
				t.Fatalf("stage automation identity: %s %v", output, err)
			}
		}
		command = exec.Command("git", "-C", repository, "checkout", "--quiet", "--detach", source.Binding.SelectedCommit)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("select fixture source: %s %v", output, err)
		}
		copyTestJobSubmodules(t, upstream, repository, source.Submodules)
		if err := forkspace.HydrateLFS(context.Background(), repository, source.Binding.SelectedCommit, upstream); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(repository, 0700); err != nil {
			t.Fatal(err)
		}
		document, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "source.json"), document, 0600); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err := stagedJobRepository(context.Background(), stateRoot, source); err != nil {
		t.Fatal(err)
	}
}

func testJobSubmodules(t *testing.T, repository, commit string) []workerproto.JobSubmodule {
	t.Helper()
	links, err := forkspace.Gitlinks(context.Background(), repository, commit)
	if err != nil {
		t.Fatal(err)
	}
	modules := []workerproto.JobSubmodule{}
	for path, head := range links {
		child := filepath.Join(repository, path)
		tree, err := sessionWorkspaceTree(child, head)
		if err != nil {
			t.Fatal(err)
		}
		modules = append(modules, workerproto.JobSubmodule{Path: path, RepositoryRef: "test:child",
			GitHubRepository: "example/child", GitHubRepositoryID: 19, Commit: head, Tree: tree,
			Submodules: testJobSubmodules(t, child, head)})
	}
	return modules
}

func copyTestJobSubmodules(t *testing.T, source, destination string, modules []workerproto.JobSubmodule) {
	t.Helper()
	for _, module := range modules {
		child := filepath.Join(destination, module.Path)
		command := exec.Command("git", "clone", "--quiet", "--no-local", filepath.Join(source, module.Path), child)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("clone fixture child: %s %v", output, err)
		}
		command = exec.Command("git", "-C", child, "checkout", "--quiet", "--detach", module.Commit)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("pin fixture child: %s %v", output, err)
		}
		copyTestJobSubmodules(t, filepath.Join(source, module.Path), child, module.Submodules)
		if err := forkspace.HydrateLFS(context.Background(), child, module.Commit, filepath.Join(source, module.Path)); err != nil {
			t.Fatal(err)
		}
	}
}
