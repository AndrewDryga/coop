package workerconnector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

type jobSourceGrantFixture struct {
	calls   int
	expired bool
	// public names the repository refs the controller grants as public, without a token.
	public map[string]bool
	// token, when set, replaces whatever the grant would carry.
	token *string
}

func TestSourceStagingRequiresAndMaterializesTheEntireAuthorizedGitlinkTree(t *testing.T) {
	ctx := context.Background()
	leaf, leafGit := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(leaf, "code"), []byte("nested content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	leafGit("add", "code")
	leafGit("commit", "-qm", "leaf")
	value := func(repo, revision string) string {
		t.Helper()
		out, err := sourceGitValue(ctx, repo, "", "file", "rev-parse", revision)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	makeParent := func(childCommit, path string) string {
		t.Helper()
		repo, git := gitrepo.New(t)
		git("update-index", "--add", "--cacheinfo", "160000,"+childCommit+","+path)
		// The worker must not follow this URL or execute its update instruction.
		data := "[submodule \"child\"]\npath = " + path + "\nurl = https://evil.invalid/stolen\nupdate = !exit 99\n"
		if err := os.WriteFile(filepath.Join(repo, ".gitmodules"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", ".gitmodules")
		git("commit", "-qm", "parent")
		return repo
	}
	child := makeParent(value(leaf, "HEAD"), "nested space")
	remote := makeParent(value(child, "HEAD"), "vendor/child")
	source := testJobSource()
	source.Binding.DefaultCommit = value(remote, "HEAD")
	source.Binding.SelectedCommit, source.Binding.BaseCommit = source.Binding.DefaultCommit, source.Binding.DefaultCommit
	source.Binding.AdmittedTree = value(remote, "HEAD^{tree}")
	source.Submodules = []workerproto.JobSubmodule{{Path: "vendor/child", RepositoryRef: "child",
		GitHubRepository: "example/child", GitHubRepositoryID: 18, Commit: value(child, "HEAD"), Tree: value(child, "HEAD^{tree}"),
		Submodules: []workerproto.JobSubmodule{{Path: "nested space", RepositoryRef: "leaf", GitHubRepository: "example/leaf",
			GitHubRepositoryID: 19, Commit: value(leaf, "HEAD"), Tree: value(leaf, "HEAD^{tree}"), Submodules: []workerproto.JobSubmodule{}}}}}
	newStager := func(t *testing.T) (*privateJobSourceStager, *jobSourceGrantFixture) {
		t.Helper()
		root := t.TempDir()
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
		grant := &jobSourceGrantFixture{}
		return &privateJobSourceStager{transport: grant, stateRoot: root, remoteForTest: remote,
			submoduleRemotesForTest: map[string]string{"example/child": child, "example/leaf": leaf}}, grant
	}
	for _, scenario := range []string{"missing", "extra", "wrong-tree", "wrong-commit"} {
		t.Run(scenario, func(t *testing.T) {
			encoded, _ := json.Marshal(source)
			var invalid workerproto.JobSource
			if err := json.Unmarshal(encoded, &invalid); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				invalid.Submodules = []workerproto.JobSubmodule{}
			case "extra":
				invalid.Submodules[0].Path = "undeclared"
			case "wrong-tree":
				invalid.Submodules[0].Submodules[0].Tree = strings.Repeat("f", 40)
			case "wrong-commit":
				invalid.Submodules[0].Commit = strings.Repeat("f", 40)
			}
			stager, _ := newStager(t)
			if err := stager.Stage(ctx, "job:modules", invalid); err == nil {
				t.Fatal("staged a partial or incorrect source")
			}
			key, _ := invalid.StagingKey()
			if _, err := os.Lstat(filepath.Join(stager.stateRoot, "job-sources", key)); !os.IsNotExist(err) {
				t.Fatalf("failed tree published authority: %v", err)
			}
		})
	}
	stager, grant := newStager(t)
	for range 2 {
		if err := stager.Stage(ctx, "job:modules", source); err != nil {
			t.Fatal(err)
		}
	}
	if grant.calls != 3 {
		t.Fatalf("wanted exactly one grant per repository, got %d", grant.calls)
	}
	key, _ := source.StagingKey()
	nested := filepath.Join(stager.stateRoot, "job-sources", key, "repository", "vendor", "child", "nested space")
	if data, err := os.ReadFile(filepath.Join(nested, "code")); err != nil || string(data) != "nested content\n" {
		t.Fatalf("missing nested content: %q, %v", data, err)
	}
	if info, err := os.Lstat(filepath.Join(nested, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("child must own its Git metadata: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "code"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stager.Stage(ctx, "job:modules", source); !errors.Is(err, ErrJobSourceIntegrity) {
		t.Fatalf("warm source adopted modified child: %v", err)
	}
}

func (f *jobSourceGrantFixture) FetchJobSourceGrant(_ context.Context, _ string, source workerproto.RepositoryIdentity) (JobSourceGrant, error) {
	f.calls++
	expires := time.Now().Add(time.Hour)
	if f.expired {
		expires = time.Now().Add(-time.Second)
	}
	grant := JobSourceGrant{RepositoryRef: source.RepositoryRef, GitHubRepository: source.GitHubRepository, GitHubRepositoryID: source.GitHubRepositoryID, Token: "temporary-token", ExpiresAt: expires}
	if f.public[source.RepositoryRef] {
		grant.Public, grant.Token = true, ""
	}
	if f.token != nil {
		grant.Token = *f.token
	}
	return grant, nil
}

// A repository may vendor an open-source library from outside every organization the
// controller's GitHub App reaches (theblitzapp/blitz-core vendors skypjack/entt). The
// controller grants such a public repository without a credential; the worker stages it
// anonymously, and refuses a grant that is neither credentialed nor public, or both.
func TestSourceStagingFetchesAPublicSubmoduleWithoutACredential(t *testing.T) {
	ctx := context.Background()
	library, libraryGit := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(library, "entt.hpp"), []byte("// header\n"), 0600); err != nil {
		t.Fatal(err)
	}
	libraryGit("add", "entt.hpp")
	libraryGit("commit", "-qm", "library")
	value := func(repo, revision string) string {
		t.Helper()
		out, err := sourceGitValue(ctx, repo, "", "file", "rev-parse", revision)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	remote, git := gitrepo.New(t)
	git("update-index", "--add", "--cacheinfo", "160000,"+value(library, "HEAD")+",lib/libentt")
	if err := os.WriteFile(filepath.Join(remote, ".gitmodules"), []byte("[submodule \"entt\"]\npath = lib/libentt\nurl = https://github.com/skypjack/entt.git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitmodules")
	git("commit", "-qm", "parent")
	source := testJobSource()
	source.Binding.DefaultCommit = value(remote, "HEAD")
	source.Binding.SelectedCommit, source.Binding.BaseCommit = source.Binding.DefaultCommit, source.Binding.DefaultCommit
	source.Binding.AdmittedTree = value(remote, "HEAD^{tree}")
	source.Submodules = []workerproto.JobSubmodule{{Path: "lib/libentt", RepositoryRef: "public:skypjack:entt",
		GitHubRepository: "skypjack/entt", GitHubRepositoryID: 2, Commit: value(library, "HEAD"),
		Tree: value(library, "HEAD^{tree}"), Submodules: []workerproto.JobSubmodule{}}}
	stager := func(grant *jobSourceGrantFixture) *privateJobSourceStager {
		t.Helper()
		root := t.TempDir()
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
		return &privateJobSourceStager{transport: grant, stateRoot: root, remoteForTest: remote,
			submoduleRemotesForTest: map[string]string{"skypjack/entt": library}}
	}
	public := map[string]bool{"public:skypjack:entt": true}
	if err := stager(&jobSourceGrantFixture{public: public}).Stage(ctx, "job:public", source); err != nil {
		t.Fatalf("public submodule was not staged: %v", err)
	}
	withToken, none := "temporary-token", ""
	for name, grant := range map[string]*jobSourceGrantFixture{
		"public with a credential":  {public: public, token: &withToken},
		"no credential, not public": {token: &none},
	} {
		if err := stager(grant).Stage(ctx, "job:public", source); !errors.Is(err, ErrJobSourceIntegrity) {
			t.Fatalf("%s: staged with %v", name, err)
		}
	}
}

func TestSourceStagingRefusesChangedOrUnprovenFrozenObjects(t *testing.T) {
	for _, scenario := range []string{"moved-default", "moved-selected", "wrong-tree", "wrong-base", "unavailable", "cancelled", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			remote, git := gitrepo.New(t)
			git("commit", "--allow-empty", "-qm", "default")
			value := func(revision string) string {
				t.Helper()
				value, err := sourceGitValue(context.Background(), remote, "", "file", "rev-parse", revision)
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			base := value("HEAD")
			git("checkout", "-qb", "feature")
			if err := os.WriteFile(filepath.Join(remote, "work.txt"), []byte("selected\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "work.txt")
			git("commit", "-qm", "selected")
			selected := value("HEAD")
			source := testJobSource()
			ref := "refs/heads/feature"
			source.Binding.Kind, source.Binding.Requested = session.SourceBranch, session.SourceSelector{Kind: session.SourceBranch, Name: "feature"}
			source.Binding.DefaultCommit, source.Binding.BaseCommit = base, base
			source.Binding.SelectedRef, source.Binding.SelectedCommit = &ref, selected
			source.Binding.AdmittedTree = value("HEAD^{tree}")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			grant := &jobSourceGrantFixture{expired: scenario == "expired"}
			switch scenario {
			case "moved-default":
				git("checkout", "-q", "main")
				git("commit", "--allow-empty", "-qm", "moved")
			case "moved-selected":
				git("commit", "--allow-empty", "-qm", "moved")
			case "wrong-tree":
				source.Binding.AdmittedTree = value("main^{tree}")
			case "wrong-base":
				source.Binding.BaseCommit = selected
			case "unavailable":
				remote = filepath.Join(t.TempDir(), "absent")
			case "cancelled":
				cancel()
			}
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			stager := &privateJobSourceStager{transport: grant, stateRoot: root, remoteForTest: remote}
			if err := stager.Stage(ctx, "job:frozen-source", source); err == nil {
				t.Fatal("published an unproven source")
			}
			key, err := source.StagingKey()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(root, "job-sources", key)); !os.IsNotExist(err) {
				t.Fatalf("failed fetch published source authority: %v", err)
			}
		})
	}
}

func testJobSource() workerproto.JobSource {
	ref := "refs/heads/main"
	commit := strings.Repeat("a", 40)
	return workerproto.JobSource{
		RepositoryRef: "repo:one", GitHubRepository: "example/repository", GitHubRepositoryID: 17,
		Binding: session.SourceBinding{
			Version: 1, Kind: session.SourceDefault, Requested: session.DefaultSourceSelector(),
			RemoteIdentity: "origin", DefaultRef: ref, SelectedRef: &ref, DefaultCommit: commit, SelectedCommit: commit, BaseCommit: commit,
			AdmittedTree: strings.Repeat("c", 40), ResolvedAt: time.Now().UTC(),
		}, Submodules: []workerproto.JobSubmodule{},
	}
}

func TestJobSourceStagerFetchesPrivateVerifiedRepository(t *testing.T) {
	remote, git := gitrepo.New(t)
	if err := os.WriteFile(filepath.Join(remote, "code"), []byte("full working tree"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "code")
	git("commit", "-qm", "source")
	source := testJobSource()
	commit, err := sourceGitValue(context.Background(), remote, "", "file", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sourceGitValue(context.Background(), remote, "", "file", "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	source.Binding.DefaultCommit, source.Binding.SelectedCommit, source.Binding.BaseCommit, source.Binding.AdmittedTree = commit, commit, commit, tree
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	transport := &jobSourceGrantFixture{}
	stager := &privateJobSourceStager{transport: transport, stateRoot: root, remoteForTest: remote}
	for range 2 {
		if err := stager.Stage(context.Background(), "job:one", source); err != nil {
			t.Fatal(err)
		}
	}
	if transport.calls != 1 {
		t.Fatalf("fetched grant %d times", transport.calls)
	}
	key, _ := source.StagingKey()
	path := filepath.Join(root, "job-sources", key, "repository", "code")
	identity, err := sourceGitValue(context.Background(), filepath.Dir(path), "", "file", "-c", "user.useConfigOnly=true", "var", "GIT_COMMITTER_IDENT")
	if err != nil || !strings.HasPrefix(identity, "Coop <coop@localhost> ") {
		t.Fatalf("staged automation identity = %q, %v", identity, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "full working tree" {
		t.Fatalf("source %q %v", data, err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stager.Stage(context.Background(), "job:one", source); !errors.Is(err, ErrJobSourceIntegrity) {
		t.Fatalf("tampered source = %v", err)
	}
}

// emisar's default branch moved on 2026-10-01, and the next session cloned all 93,414 of its
// objects again at 93 KB/s, past the command's lease, from scratch on every retry, while every
// other worker command, routing's too, waited behind it. A new commit of a repository already
// staged starts from the earlier copy's objects and fetches only what is new.
func TestStagingANewCommitStartsFromTheEarlierCopyOfTheRepository(t *testing.T) {
	ctx := context.Background()
	remote, git := gitrepo.New(t)
	stage := func(content string) (workerproto.JobSource, string) {
		if err := os.WriteFile(filepath.Join(remote, "code"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", "code")
		git("commit", "-qm", content)
		commit, err := sourceGitValue(ctx, remote, "", "file", "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		tree, err := sourceGitValue(ctx, remote, "", "file", "rev-parse", "HEAD^{tree}")
		if err != nil {
			t.Fatal(err)
		}
		source := testJobSource()
		source.Binding.DefaultCommit, source.Binding.SelectedCommit = commit, commit
		source.Binding.BaseCommit, source.Binding.AdmittedTree = commit, tree
		key, err := source.StagingKey()
		if err != nil {
			t.Fatal(err)
		}
		return source, key
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stager := &privateJobSourceStager{transport: &jobSourceGrantFixture{}, stateRoot: root, remoteForTest: remote}

	first, firstKey := stage("first version")
	if err := stager.Stage(ctx, "job:one", first); err != nil {
		t.Fatal(err)
	}
	firstRepository := filepath.Join(root, "job-sources", firstKey, "repository")
	firstBlob, err := sourceGitValue(ctx, firstRepository, "", "file", "rev-parse", "HEAD:code")
	if err != nil {
		t.Fatal(err)
	}

	second, secondKey := stage("second version")
	if err := stager.Stage(ctx, "job:two", second); err != nil {
		t.Fatal(err)
	}
	secondRepository := filepath.Join(root, "job-sources", secondKey, "repository")

	object := filepath.Join(".git", "objects", firstBlob[:2], firstBlob[2:])
	earlier, err := os.Stat(filepath.Join(firstRepository, object))
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := os.Stat(filepath.Join(secondRepository, object))
	if err != nil || !os.SameFile(earlier, seeded) {
		t.Fatalf("the new copy did not start from the earlier one: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(secondRepository, "code")); err != nil || string(data) != "second version" {
		t.Fatalf("staged %q %v", data, err)
	}
	if refs, _ := sourceGitValue(ctx, secondRepository, "", "file", "for-each-ref", "refs/coop"); refs != "" {
		t.Fatalf("the seed ref stayed behind: %q", refs)
	}
}

func TestJobSourceGrantIsJobAndExactRepositoryBound(t *testing.T) {
	source := testJobSource().RepositoryIdentity()
	grant, _ := (&jobSourceGrantFixture{}).FetchJobSourceGrant(context.Background(), "job:one", source)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/coop-workers/jobs/job:one/source-grants" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var requested map[string]any
		expected := map[string]any{"repository_ref": source.RepositoryRef, "github_repository": source.GitHubRepository,
			"github_repository_id": float64(source.GitHubRepositoryID)}
		if err := json.NewDecoder(r.Body).Decode(&requested); err != nil || !reflect.DeepEqual(requested, expected) {
			t.Fatalf("grant request changed source: %+v, %v", requested, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(grant)
	}))
	defer server.Close()
	transport, err := newHTTPTransport(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.FetchJobSourceGrant(context.Background(), "job:one", source); err != nil {
		t.Fatal(err)
	}
	grant.GitHubRepositoryID++
	if _, err := transport.FetchJobSourceGrant(context.Background(), "job:one", source); !errors.Is(err, ErrJobSourceIntegrity) {
		t.Fatalf("wrong repo = %v", err)
	}
	grant.GitHubRepositoryID--
	grant.Public, grant.Token = true, ""
	if fetched, err := transport.FetchJobSourceGrant(context.Background(), "job:one", source); err != nil || !fetched.Public || fetched.Token != "" {
		t.Fatalf("public grant = %+v, %v", fetched, err)
	}
	for _, refused := range []JobSourceGrant{{Public: true, Token: "x"}, {Public: false, Token: ""}} {
		grant.Public, grant.Token = refused.Public, refused.Token
		if _, err := transport.FetchJobSourceGrant(context.Background(), "job:one", source); !errors.Is(err, ErrJobSourceIntegrity) {
			t.Fatalf("grant public=%v token=%q = %v", refused.Public, refused.Token, err)
		}
	}
}

func TestCreateJobCommandForwardsOnlyTheFrozenJob(t *testing.T) {
	job := workerproto.JobSpec{
		Version: 1, JobRef: "job:one", Source: ptrJobSource(testJobSource()),
		Companions: []workerproto.JobCompanion{}, Targets: []string{"codex"}, Mode: "readonly",
		RepositoryReadOnly: true, Egress: workerproto.JobEgress{Mode: "none", Rules: []workerproto.JobRule{}},
		Limits: workerproto.JobLimits{MaxTurns: 1, MaxQueuedTurns: 1, MaxQueuedBytes: 4096,
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
	payload, err := json.Marshal(map[string]any{"task": job.JobRef, "job": json.RawMessage(document), "expected_job_digest": digest})
	if err != nil {
		t.Fatal(err)
	}
	stager := &recordingJobSourceStager{}
	executor := &Executor{jobSourceStager: stager}
	if err := executor.stageCreateJobSources(context.Background(), payload); err != nil || len(stager.sources) != 1 || !slices.Equal(stager.jobs, []string{job.JobRef}) ||
		!reflect.DeepEqual(stager.sources[0], *job.Source) {
		t.Fatalf("staged create sources = %+v, err=%v", stager.sources, err)
	}
}

func ptrJobSource(source workerproto.JobSource) *workerproto.JobSource { return &source }

func TestSourceGitCredentialsAreEphemeralAndScopedToGitHub(t *testing.T) {
	cmd := sourceGitCommand(context.Background(), t.TempDir(), "https", "fetch", "origin", "main")
	for _, expected := range []string{
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_GLOBAL=/dev/null",
	} {
		if !slices.Contains(cmd.Env, expected) {
			t.Errorf("missing host-only Git configuration %s", strings.SplitN(expected, "=", 2)[0])
		}
	}
	var output sourceGitOutput
	if _, err := output.Write(bytes.Repeat([]byte("x"), 4097)); !errors.Is(err, ErrJobSourceIntegrity) || output.Len() != 0 {
		t.Fatal("Git output was retained beyond its bound")
	}
	var streamed sourceGitOutput
	if _, err := io.Copy(&streamed, io.LimitReader(strings.NewReader(strings.Repeat("x", 8192)), 8192)); !errors.Is(err, ErrJobSourceIntegrity) || streamed.Len() > 4096 {
		t.Fatal("subprocess pipe copying bypassed the Git output bound")
	}
}

func TestSourceLFSDiagnosticsCannotPersistTheGrantToken(t *testing.T) {
	repository, _ := gitrepo.New(t)
	private := privateWorkerRoot(t)
	token := "dummy-lfs-log-protection-token"
	if _, err := sourceGitValue(context.Background(), repository, token, "https", "lfs", "env"); err == nil {
		t.Fatal("authenticated Git fell back to system temp")
	}
	ctx, err := withSourceCredentials(context.Background(), private)
	if err != nil {
		t.Fatal(err)
	}
	output, err := sourceGitValue(ctx, repository, token, "https", "lfs", "env")
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	if strings.Contains(output, token) || strings.Contains(output, encoded) {
		t.Fatal("Git LFS diagnostic environment contains the grant credential")
	}
	header, err := sourceGitValue(ctx, repository, token, "https", "config", "--get-urlmatch", "http.extraheader", "https://github.com/example/repo.git")
	if err != nil || header != "Authorization: Basic "+encoded {
		t.Fatalf("GitHub did not receive its scoped credential: %v", err)
	}
	if _, err := sourceGitValue(ctx, repository, token, "https", "config", "--get-urlmatch", "http.extraheader", "https://unrelated.invalid/repo.git"); err == nil {
		t.Fatal("credential matched an unrelated host")
	}
	entries, err := os.ReadDir(filepath.Join(private, "host-git-tmp"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("credential file survived success/failure cleanup: %v", err)
	}
}

type recordingJobSourceStager struct {
	sources []workerproto.JobSource
	jobs    []string
}

func TestEmptyPrimaryDoesNotSkipAuthorizedCompanionStaging(t *testing.T) {
	source := testJobSource()
	job := workerproto.JobSpec{
		Version: 1, JobRef: "job:empty", Source: nil,
		Companions: []workerproto.JobCompanion{{Name: "library", Source: source}},
		Targets:    []string{"codex"}, Mode: "normal", RepositoryReadOnly: true,
		Egress: workerproto.JobEgress{Mode: "none", Rules: []workerproto.JobRule{}},
		Limits: workerproto.JobLimits{MaxTurns: 1, MaxQueuedTurns: 1, MaxQueuedBytes: 4096,
			TurnTimeoutMS: 60_000, MaxPatchBytes: 1024},
	}
	payload, err := json.Marshal(map[string]any{"job": job})
	if err != nil {
		t.Fatal(err)
	}
	stager := &recordingJobSourceStager{}
	executor := &Executor{jobSourceStager: stager}
	if err := executor.stageCreateJobSources(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stager.sources, []workerproto.JobSource{source}) ||
		!reflect.DeepEqual(stager.jobs, []string{job.JobRef}) {
		t.Fatalf("companion source was not staged: %+v", stager)
	}
	executor.jobSourceStager = nil
	if err := executor.stageCreateJobSources(context.Background(), payload); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("missing companion transport admitted: %v", err)
	}
}

func (s *recordingJobSourceStager) Stage(_ context.Context, jobRef string, source workerproto.JobSource) error {
	s.jobs = append(s.jobs, jobRef)
	s.sources = append(s.sources, source)
	return nil
}
