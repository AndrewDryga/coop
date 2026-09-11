package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// selectorFixture is one operator-configured repository: a seed worktree that publishes to a bare
// remote, and the policy-owned cache clone every session forks from. Nothing here is a mock — the
// remote is contacted over git's own local transport, so freshness, movement, fetch-by-OID and
// cache immutability are observed rather than asserted about a double.
type selectorFixture struct {
	t        *testing.T
	seed     string
	seedGit  func(...string)
	remote   string
	cache    string
	policies map[string]Policy
}

func newSelectorFixture(t *testing.T) *selectorFixture {
	t.Helper()
	globalConfig := filepath.Join(t.TempDir(), "global")
	if err := os.WriteFile(globalConfig, []byte("[user]\n\tuseConfigOnly = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	seed, seedGit := gitrepo.New(t)
	seedGit("commit", "-q", "--allow-empty", "-m", "base")
	seedGit("branch", "-M", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, "", "init", "-q", "--bare", remote)
	// Hosted Git serves reachable objects that are not ref tips; a plain server refuses them.
	// Sessions that select an exact commit depend on that behavior, so the fixture states it.
	runGitTest(t, "", "-C", remote, "config", "uploadpack.allowReachableSHA1InWant", "true")
	seedGit("remote", "add", "origin", remote)
	seedGit("push", "-q", "-u", "origin", "main")

	cache := filepath.Join(t.TempDir(), "cache")
	runGitTest(t, "", "clone", "-q", "-b", "main", remote, cache)
	runGitTest(t, cache, "config", "user.email", "t@t")
	runGitTest(t, cache, "config", "user.name", "T")
	policies := testSessionPolicies(cache)
	policy := policies["responder"]
	policy.Remote, policy.Branch = "origin", "main"
	policies["responder"] = policy
	return &selectorFixture{t: t, seed: seed, seedGit: seedGit, remote: remote, cache: cache, policies: policies}
}

func (f *selectorFixture) service() *Service {
	f.t.Helper()
	service := newTestSessionService(f.t, filepath.Join(f.t.TempDir(), "state"), f.policies, nil)
	f.t.Cleanup(func() { _ = service.Stop() })
	return service
}

// advanceMain publishes one new commit on the configured default branch and returns its id.
func (f *selectorFixture) advanceMain(name string) string {
	f.t.Helper()
	f.seedGit("checkout", "-q", "main")
	f.write(name)
	f.seedGit("push", "-q", "origin", "main")
	return gitOut(f.seed, "rev-parse", "HEAD")
}

func (f *selectorFixture) write(name string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.seed, name+".txt"), []byte(name+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.seedGit("add", name+".txt")
	f.seedGit("commit", "-q", "-m", name)
}

// cacheState is everything about the policy-owned checkout a session must never disturb: its
// HEAD, its checked-out branch, every ref, and the cleanliness of its working tree.
func (f *selectorFixture) cacheState() string {
	f.t.Helper()
	return strings.Join([]string{
		gitOut(f.cache, "rev-parse", "HEAD"),
		gitOut(f.cache, "symbolic-ref", "HEAD"),
		gitOut(f.cache, "for-each-ref"),
		gitOut(f.cache, "status", "--porcelain"),
		gitOut(f.cache, "config", "--local", "--list"),
	}, "\x00")
}

// workspaceEntries lists the fork directories the policy repository owns. A refused source must
// add none of them.
func workspaceEntries(t *testing.T, repository string) []string {
	t.Helper()
	entries, err := os.ReadDir(forkspace.Home(repository))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		// `.coop` is the fork home's own bookkeeping directory, not a workspace.
		if entry.IsDir() && entry.Name() != ".coop" {
			names = append(names, entry.Name())
		}
	}
	return names
}

func selector(kind session.SourceKind) *session.SourceSelector {
	return &session.SourceSelector{Kind: kind}
}

// A session that names no source still starts from the CURRENT configured remote head, not from
// whatever the shared cache happens to be checked out at. The cache here is deliberately stale in
// both ways a real one goes stale: its working tree and its remote-tracking ref.
func TestDefaultSourceStartsAtTheCurrentRemoteHeadDespiteAStaleCache(t *testing.T) {
	fixture := newSelectorFixture(t)
	head := fixture.advanceMain("remote-advance")
	service := fixture.service()

	before := fixture.cacheState()
	sess, err := service.CreateRemoteSession(context.Background(), "default-source", CreateRemoteSessionRequest{
		Policy: "responder", Task: "no source named", Source: selector(session.SourceDefault),
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := sess.Source
	if binding == nil {
		t.Fatal("a repository-backed session carries no source binding")
	}
	if binding.Version != session.SourceBindingVersion || binding.Kind != session.SourceDefault ||
		binding.RemoteIdentity != "origin" || binding.DefaultRef != "refs/heads/main" ||
		binding.DefaultCommit != head || binding.SelectedRefValue() != "refs/heads/main" ||
		binding.SelectedCommit != head || binding.BaseCommit != head ||
		binding.AdmittedTree != gitOut(fixture.seed, "rev-parse", head+"^{tree}") ||
		binding.ResolvedAt.IsZero() || binding.Requested != (session.SourceSelector{Kind: session.SourceDefault}) {
		t.Fatalf("default binding = %+v, want the current %s", binding, head)
	}
	if got := gitOut(sess.Workspace, "rev-parse", "HEAD"); got != head {
		t.Fatalf("workspace HEAD = %s, want the pinned %s", got, head)
	}
	if got := gitOut(sess.Workspace, "symbolic-ref", "--short", "HEAD"); got != sess.ForkName {
		t.Fatalf("workspace branch = %q, want the generated %q", got, sess.ForkName)
	}
	if after := fixture.cacheState(); after != before {
		t.Fatalf("resolution moved the policy-owned cache:\n%q\nwant\n%q", after, before)
	}
	// An omitted selector is the same request as an explicit default one: the host supplies it.
	implicit, err := service.CreateRemoteSession(context.Background(), "implicit-default", CreateRemoteSessionRequest{
		Policy: "responder", Task: "implicit default",
	})
	if err != nil || implicit.Source == nil || implicit.Source.Kind != session.SourceDefault ||
		implicit.Source.SelectedCommit != head {
		t.Fatalf("implicit default binding = %+v, err=%v", implicit.Source, err)
	}
}

// A named branch resolves through its derived ref and starts at that exact tip, with the review
// baseline set to the merge base with the pinned default head. A branch the remote does not
// publish is refused before any workspace exists.
func TestBranchSourceStartsAtItsExactRemoteTip(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "feature/payments")
	fixture.write("payments")
	branchHead := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "feature/payments")
	defaultHead := fixture.advanceMain("main-moves-on")
	service := fixture.service()

	before := fixture.cacheState()
	sess, err := service.CreateRemoteSession(context.Background(), "branch-source", CreateRemoteSessionRequest{
		Policy: "responder", Task: "review the branch",
		Source: &session.SourceSelector{Kind: session.SourceBranch, Name: "feature/payments"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mergeBase := gitOut(fixture.seed, "merge-base", defaultHead, branchHead)
	binding := sess.Source
	if binding == nil || binding.Kind != session.SourceBranch ||
		binding.SelectedRefValue() != "refs/heads/feature/payments" ||
		binding.SelectedCommit != branchHead || binding.DefaultCommit != defaultHead ||
		binding.BaseCommit != mergeBase || binding.Requested.Name != "feature/payments" {
		t.Fatalf("branch binding = %+v, want %s based at %s", binding, branchHead, mergeBase)
	}
	if sess.BaseCommit != mergeBase || gitOut(sess.Workspace, "rev-parse", "HEAD") != branchHead {
		t.Fatalf("session base = %s, workspace HEAD = %s", sess.BaseCommit, gitOut(sess.Workspace, "rev-parse", "HEAD"))
	}
	if after := fixture.cacheState(); after != before {
		t.Fatal("branch resolution moved the policy-owned cache")
	}

	beforeForks := workspaceEntries(t, fixture.cache)
	for name, request := range map[string]session.SourceSelector{
		"unknown branch":   {Kind: session.SourceBranch, Name: "no-such-branch"},
		"a tag namespace":  {Kind: session.SourceBranch, Name: "../tags/v1"},
		"an option":        {Kind: session.SourceBranch, Name: "--upload-pack=touch"},
		"an invalid ref":   {Kind: session.SourceBranch, Name: "bad..name"},
		"a trailing slash": {Kind: session.SourceBranch, Name: "feature/"},
	} {
		source := request
		_, err := service.CreateRemoteSession(context.Background(), "reject-"+name, CreateRemoteSessionRequest{
			Policy: "responder", Task: "bad branch", Source: &source,
		})
		if session.CodeOf(err) != session.CodeInvalidRequest {
			t.Fatalf("%s error = %v (code %s), want invalid_request", name, err, session.CodeOf(err))
		}
	}
	if after := workspaceEntries(t, fixture.cache); len(after) != len(beforeForks) {
		t.Fatalf("refused sources left %d new workspaces behind", len(after)-len(beforeForks))
	}
}

// Movement BEFORE resolution selects the new tip; movement AFTER the durable intent was replaced
// changes nothing a replay, a restart or the workspace sees. This is the whole reason resolution
// journals its exact pins before the fork exists.
func TestBranchMovementBeforeResolutionSelectsTheNewTipAndAfterThePinDoesNot(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "moving")
	fixture.write("first")
	fixture.seedGit("push", "-q", "origin", "moving")
	second := func() string {
		fixture.seedGit("checkout", "-q", "moving")
		fixture.write("second")
		fixture.seedGit("push", "-q", "origin", "moving")
		return gitOut(fixture.seed, "rev-parse", "HEAD")
	}()
	service := fixture.service()
	request := CreateRemoteSessionRequest{
		Policy: "responder", Task: "moving branch",
		Source: &session.SourceSelector{Kind: session.SourceBranch, Name: "moving"},
	}
	sess, err := service.CreateRemoteSession(context.Background(), "moving-branch", request)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source.SelectedCommit != second {
		t.Fatalf("pinned %s, want the tip at resolution %s", sess.Source.SelectedCommit, second)
	}

	fixture.seedGit("checkout", "-q", "moving")
	fixture.write("third")
	fixture.seedGit("push", "-q", "origin", "moving")
	third := gitOut(fixture.seed, "rev-parse", "HEAD")
	if third == second {
		t.Fatal("fixture did not move the branch")
	}
	replayed, err := service.CreateRemoteSession(context.Background(), "moving-branch", request)
	if err != nil || replayed.ID != sess.ID || replayed.Source.SelectedCommit != second {
		t.Fatalf("replay after movement = %+v, err=%v, want the original %s", replayed.Source, err, second)
	}
	if got := gitOut(sess.Workspace, "rev-parse", "HEAD"); got != second {
		t.Fatalf("workspace moved to %s after the branch advanced", got)
	}
	// The same idempotency key with a DIFFERENT selector is a different request, not a retry.
	switched := request
	switched.Source = &session.SourceSelector{Kind: session.SourceDefault}
	if _, err := service.CreateRemoteSession(context.Background(), "moving-branch", switched); session.CodeOf(err) != session.CodeIdempotencyConflict {
		t.Fatalf("switched selector error = %v, want idempotency_conflict", err)
	}
}

// A pull request resolves through its derived ref, keeps its number and the trusted head the host
// observed, and refuses when that head moved between ingress and creation.
func TestPullRequestSourceRecordsItsNumberAndFailsClosedOnAChangedHead(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "pull-514")
	fixture.write("pull-change")
	pullHead := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "HEAD:refs/pull/514/head")
	defaultHead := fixture.advanceMain("main-after-pull")
	service := fixture.service()

	sess, err := service.CreateRemoteSession(context.Background(), "pull-source", CreateRemoteSessionRequest{
		Policy: "responder", Task: "fix the pull request",
		Source: &session.SourceSelector{
			Kind: session.SourcePullRequest, Number: 514, ExpectedHeadCommit: pullHead,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := sess.Source
	if binding == nil || binding.Kind != session.SourcePullRequest ||
		binding.SelectedRefValue() != "refs/pull/514/head" || binding.SelectedCommit != pullHead ||
		binding.PullRequestNumber != 514 || binding.PullRequestExpectedHead != pullHead ||
		binding.DefaultCommit != defaultHead ||
		binding.BaseCommit != gitOut(fixture.seed, "merge-base", defaultHead, pullHead) {
		t.Fatalf("pull request binding = %+v", binding)
	}
	if gitOut(sess.Workspace, "rev-parse", "HEAD") != pullHead {
		t.Fatal("workspace did not start at the pull request head")
	}

	_, err = service.CreateRemoteSession(context.Background(), "stale-pull", CreateRemoteSessionRequest{
		Policy: "responder", Task: "stale head",
		Source: &session.SourceSelector{
			Kind: session.SourcePullRequest, Number: 514, ExpectedHeadCommit: strings.Repeat("a", 40),
		},
	})
	if session.CodeOf(err) != session.CodeInvalidRequest || !strings.Contains(err.Error(), "changed before session creation") {
		t.Fatalf("changed head error = %v", err)
	}
	fixture.seedGit("push", "-q", "origin", ":refs/pull/514/head")
	_, err = service.CreateRemoteSession(context.Background(), "closed-pull", CreateRemoteSessionRequest{
		Policy: "responder", Task: "closed pull request",
		Source: &session.SourceSelector{Kind: session.SourcePullRequest, Number: 514},
	})
	if session.CodeOf(err) != session.CodeInvalidRequest || !strings.Contains(err.Error(), "refs/pull/514/head") {
		t.Fatalf("missing pull request error = %v", err)
	}
	// A pull request that moved after the pin keeps the session it created.
	fixture.seedGit("checkout", "-q", "pull-514")
	fixture.write("pull-moves")
	fixture.seedGit("push", "-q", "origin", "HEAD:refs/pull/514/head")
	current, err := service.GetSession(context.Background(), sess.ID)
	if err != nil || current.Source.SelectedCommit != pullHead {
		t.Fatalf("session after the pull request moved = %+v, err=%v", current.Source, err)
	}
}

// An exact commit has no advertised ref to prove it, so the remote is contacted every time. A
// commit the remote serves succeeds even when the cache has never seen it; a commit that exists
// ONLY in the local cache fails, because being cached is not evidence of anything.
func TestCommitSourceRequiresRemoteProofEvenWhenCached(t *testing.T) {
	fixture := newSelectorFixture(t)
	// A commit published only as a branch tip the cache has never fetched.
	fixture.seedGit("checkout", "-qb", "remote-only")
	fixture.write("remote-only-change")
	remoteOnly := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "remote-only")
	defaultHead := fixture.advanceMain("main-advance")
	service := fixture.service()

	if gitOut(fixture.cache, "cat-file", "-t", remoteOnly) == "commit" {
		t.Fatal("fixture cached the remote-only commit; the test would prove nothing")
	}
	sess, err := service.CreateRemoteSession(context.Background(), "commit-source", CreateRemoteSessionRequest{
		Policy: "responder", Task: "review the exact commit",
		Source: &session.SourceSelector{Kind: session.SourceCommit, SHA: remoteOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := sess.Source
	if binding == nil || binding.Kind != session.SourceCommit || binding.SelectedRef != nil ||
		binding.SelectedCommit != remoteOnly || binding.Requested.SHA != remoteOnly ||
		binding.DefaultCommit != defaultHead ||
		binding.BaseCommit != gitOut(fixture.seed, "merge-base", defaultHead, remoteOnly) {
		t.Fatalf("commit binding = %+v", binding)
	}
	if gitOut(sess.Workspace, "rev-parse", "HEAD") != remoteOnly {
		t.Fatal("workspace did not start at the selected commit")
	}

	// A commit that exists only in the policy-owned cache is refused: proof contacts the remote.
	runGitTest(t, fixture.cache, "commit", "-q", "--allow-empty", "-m", "local only")
	localOnly := gitOut(fixture.cache, "rev-parse", "HEAD")
	runGitTest(t, fixture.cache, "reset", "-q", "--hard", "origin/main")
	if gitOut(fixture.cache, "cat-file", "-t", localOnly) != "commit" {
		t.Fatal("fixture did not leave the local-only object in the cache")
	}
	_, err = service.CreateRemoteSession(context.Background(), "local-only-commit", CreateRemoteSessionRequest{
		Policy: "responder", Task: "cached but unpublished",
		Source: &session.SourceSelector{Kind: session.SourceCommit, SHA: localOnly},
	})
	if err == nil || !strings.Contains(err.Error(), localOnly) {
		t.Fatalf("local-only commit error = %v, want a refusal naming the object", err)
	}

	// A tree object the remote really does serve is still not a commit.
	tree := gitOut(fixture.cache, "rev-parse", "origin/main^{tree}")
	_, err = service.CreateRemoteSession(context.Background(), "tree-commit", CreateRemoteSessionRequest{
		Policy: "responder", Task: "not a commit",
		Source: &session.SourceSelector{Kind: session.SourceCommit, SHA: tree},
	})
	if session.CodeOf(err) != session.CodeInvalidRequest {
		t.Fatalf("tree-as-commit error = %v, want invalid_request", err)
	}

	for name, sha := range map[string]string{
		"abbreviated": remoteOnly[:12],
		"uppercase":   strings.ToUpper(remoteOnly),
		"symbolic":    "HEAD",
		"absent":      strings.Repeat("f", 40),
	} {
		_, err := service.CreateRemoteSession(context.Background(), "bad-commit-"+name, CreateRemoteSessionRequest{
			Policy: "responder", Task: "bad commit",
			Source: &session.SourceSelector{Kind: session.SourceCommit, SHA: sha},
		})
		if err == nil {
			t.Fatalf("%s object id was accepted", name)
		}
		if name != "absent" && session.CodeOf(err) != session.CodeInvalidRequest {
			t.Fatalf("%s object id error = %v, want invalid_request", name, err)
		}
	}
}

// Hosted Git serves an object id only when it is a ref tip or, with
// uploadpack.allowReachableSHA1InWant (which GitHub sets), reachable from one; anything else is
// refused. Git's LOCAL transport enforces none of that, so a temporary bare remote cannot stage a
// server refusal — the refusal is staged at the boundary instead, with the exact message a real
// upload-pack sends. What matters is that Coop fails closed rather than accepting whatever the
// object database happens to hold.
func TestCommitSourceFailsClosedWhenTheRemoteRefusesObjectFetch(t *testing.T) {
	fixture := newSelectorFixture(t)
	head := gitOut(fixture.cache, "rev-parse", "HEAD")
	refusals := 0
	refusing := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "fetch" {
			refusals++
			return nil, fmt.Errorf("git fetch: exit status 128: %s",
				"fatal: remote error: upload-pack: not our ref "+head)
		}
		return nil, fmt.Errorf("unexpected git %v", args)
	}
	_, err := pinSessionSourceCommitWithTimeouts(
		context.Background(),
		sessionRepositorySource{
			label: sessionSourceLabel, repository: fixture.cache, remote: "origin", branch: "main",
		},
		head, wait.Deadline, wait.Deadline, refusing,
	)
	if session.CodeOf(err) != session.CodeRepositoryUnavailable {
		t.Fatalf("refused object fetch error = %v (code %s), want repository_unavailable", err, session.CodeOf(err))
	}
	if refusals != 1 {
		t.Fatalf("object fetch ran %d times, want exactly one bounded attempt", refusals)
	}
	// The object is in the cache and the remote really does serve it, yet the refusal above still
	// failed: being cached is never the evidence.
	if gitOut(fixture.cache, "cat-file", "-t", head) != "commit" {
		t.Fatal("fixture did not leave the object cached")
	}
}

// A source with no common ancestry has no honest review baseline, so it is refused before the
// fork exists rather than reviewed against an invented one.
func TestUnrelatedSourceFailsBeforeAnyWorkspaceIsCreated(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-q", "--orphan", "unrelated")
	fixture.seedGit("rm", "-rq", "--cached", "-r", "--ignore-unmatch", ".")
	fixture.write("unrelated-root")
	fixture.seedGit("push", "-q", "origin", "unrelated")
	service := fixture.service()

	before := workspaceEntries(t, fixture.cache)
	_, err := service.CreateRemoteSession(context.Background(), "unrelated-source", CreateRemoteSessionRequest{
		Policy: "responder", Task: "unrelated history",
		Source: &session.SourceSelector{Kind: session.SourceBranch, Name: "unrelated"},
	})
	if session.CodeOf(err) != session.CodeInvalidRequest ||
		!strings.Contains(err.Error(), "no common") {
		t.Fatalf("unrelated source error = %v, want an actionable invalid_request", err)
	}
	if after := workspaceEntries(t, fixture.cache); len(after) != len(before) {
		t.Fatalf("a refused source created %d workspaces", len(after)-len(before))
	}
}

// Nothing in a selector may fall back to local state when the remote cannot answer: not the
// cache's HEAD, not its remote-tracking ref, not an object it already holds.
func TestSelectorRemoteFailureNeverFallsBackToLocalState(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "tracked")
	fixture.write("tracked-change")
	tracked := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "tracked")
	runGitTest(t, fixture.cache, "fetch", "-q", "origin", "tracked:refs/remotes/origin/tracked")
	if gitOut(fixture.cache, "rev-parse", "refs/remotes/origin/tracked") != tracked {
		t.Fatal("fixture did not leave a stale tracking ref behind")
	}
	if err := os.RemoveAll(fixture.remote); err != nil {
		t.Fatal(err)
	}
	service := fixture.service()

	for name, source := range map[string]session.SourceSelector{
		"default": {Kind: session.SourceDefault},
		"branch":  {Kind: session.SourceBranch, Name: "tracked"},
		"pull":    {Kind: session.SourcePullRequest, Number: 7},
		"commit":  {Kind: session.SourceCommit, SHA: tracked},
	} {
		requested := source
		request := CreateRemoteSessionRequest{
			Policy: "responder", Task: "remote is gone", Source: &requested,
		}
		_, err := service.CreateRemoteSession(context.Background(), "unreachable-"+name, request)
		if err == nil {
			t.Fatalf("%s selector produced a session with no reachable remote", name)
		}
		if session.CodeOf(err) != session.CodeRepositoryUnavailable {
			t.Fatalf("%s selector error = %v (code %s), want repository_unavailable", name, err, session.CodeOf(err))
		}
		// A retry is the same request, so it keeps one operation identity rather than minting a
		// second attempt for the operator to reconcile.
		first, err := service.store.GetOperation(context.Background(), "unreachable-"+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateRemoteSession(context.Background(), "unreachable-"+name, request); session.CodeOf(err) != session.CodeRepositoryUnavailable {
			t.Fatalf("%s retry error = %v", name, err)
		}
		retried, err := service.store.GetOperation(context.Background(), "unreachable-"+name)
		if err != nil || retried.ID != first.ID {
			t.Fatalf("%s retry minted operation %+v, want the original %s", name, retried, first.ID)
		}
	}
	if forks := workspaceEntries(t, fixture.cache); len(forks) != 0 {
		t.Fatalf("an unreachable remote created %d workspaces", len(forks))
	}
}

// Four sessions resolving four different sources share one object database. None of them moves
// the policy-owned checkout, reconfigures it, or lands on another session's source.
func TestConcurrentSelectorsShareOneObjectDatabaseWithoutMovingTheCache(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "concurrent")
	fixture.write("concurrent-change")
	branchHead := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "concurrent")
	fixture.seedGit("checkout", "-qb", "concurrent-pull", "main")
	fixture.write("pull-change")
	pullHead := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "HEAD:refs/pull/99/head")
	fixture.seedGit("checkout", "-qb", "concurrent-commit", "main")
	fixture.write("commit-change")
	exactCommit := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "concurrent-commit")
	defaultHead := fixture.advanceMain("concurrent-main")
	service := fixture.service()

	before := fixture.cacheState()
	want := map[string]string{
		"default": defaultHead, "branch": branchHead, "pull": pullHead, "commit": exactCommit,
	}
	sources := map[string]session.SourceSelector{
		"default": {Kind: session.SourceDefault},
		"branch":  {Kind: session.SourceBranch, Name: "concurrent"},
		"pull":    {Kind: session.SourcePullRequest, Number: 99},
		"commit":  {Kind: session.SourceCommit, SHA: exactCommit},
	}
	var mu sync.Mutex
	got := map[string]session.Session{}
	var wg sync.WaitGroup
	for name, source := range sources {
		name, source := name, source
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := service.CreateRemoteSession(context.Background(), "concurrent-"+name, CreateRemoteSessionRequest{
				Policy: "responder", Task: "concurrent " + name, Source: &source,
			})
			if err != nil {
				t.Errorf("%s create: %v", name, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			got[name] = sess
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	seen := map[string]string{}
	for name, sess := range got {
		if sess.Source.SelectedCommit != want[name] {
			t.Fatalf("%s selected %s, want %s", name, sess.Source.SelectedCommit, want[name])
		}
		if head := gitOut(sess.Workspace, "rev-parse", "HEAD"); head != want[name] {
			t.Fatalf("%s workspace HEAD = %s, want %s", name, head, want[name])
		}
		if other, clash := seen[sess.Workspace]; clash {
			t.Fatalf("%s and %s share workspace %s", name, other, sess.Workspace)
		}
		seen[sess.Workspace] = name
	}
	if after := fixture.cacheState(); after != before {
		t.Fatalf("concurrent resolution disturbed the policy-owned cache:\n%q\nwant\n%q", after, before)
	}
}

// A policy with no operator-configured remote is intentionally local. It keeps local semantics
// for its own default and refuses every selector whose proof it cannot obtain, instead of
// silently reinterpreting a request as "whatever this checkout is at".
func TestLocalPolicyRefusesEverySelectorItCannotProve(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "scratch base")
	head := gitOut(repo, "rev-parse", "HEAD")
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), testSessionPolicies(repo), nil)
	defer service.Stop()

	sess, err := service.CreateRemoteSession(context.Background(), "local-default", CreateRemoteSessionRequest{
		Policy: "responder", Task: "local scratch", Source: selector(session.SourceDefault),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source != nil {
		t.Fatalf("a local policy published a remote source binding: %+v", sess.Source)
	}
	if sess.BaseCommit != head {
		t.Fatalf("local session base = %s, want local HEAD %s", sess.BaseCommit, head)
	}
	for name, source := range map[string]session.SourceSelector{
		"branch": {Kind: session.SourceBranch, Name: "main"},
		"pull":   {Kind: session.SourcePullRequest, Number: 1},
		"commit": {Kind: session.SourceCommit, SHA: head},
	} {
		requested := source
		_, err := service.CreateRemoteSession(context.Background(), "local-"+name, CreateRemoteSessionRequest{
			Policy: "responder", Task: "local " + name, Source: &requested,
		})
		if session.CodeOf(err) != session.CodeInvalidRequest ||
			!strings.Contains(err.Error(), "operator-configured remote") {
			t.Fatalf("local %s selector error = %v, want an actionable invalid_request", name, err)
		}
	}
}

// Selecting a primary source changes nothing about a companion: it stays pinned to its own
// operator-configured default branch, read-only.
func TestCompanionsKeepTheirConfiguredDefaultWhenAPrimarySourceIsSelected(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "primary-branch")
	fixture.write("primary-change")
	branchHead := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "primary-branch")

	companionSeed, companionGit := gitrepo.New(t)
	companionGit("commit", "-q", "--allow-empty", "-m", "companion base")
	companionGit("branch", "-M", "main")
	companionRemote := filepath.Join(t.TempDir(), "companion.git")
	runGitTest(t, "", "init", "-q", "--bare", companionRemote)
	companionGit("remote", "add", "origin", companionRemote)
	companionGit("push", "-q", "-u", "origin", "main")
	companionHead := gitOut(companionSeed, "rev-parse", "HEAD")
	companionCache := filepath.Join(t.TempDir(), "companion-cache")
	runGitTest(t, "", "clone", "-q", "-b", "main", companionRemote, companionCache)

	policy := fixture.policies["responder"]
	policy.Companions = []CompanionPolicy{{
		Name: "docs", Repository: companionCache, Remote: "origin", Branch: "main",
	}}
	fixture.policies["responder"] = policy
	service := fixture.service()

	sess, err := service.CreateRemoteSession(context.Background(), "companion-source", CreateRemoteSessionRequest{
		Policy: "responder", Task: "primary branch with companions",
		Source: &session.SourceSelector{Kind: session.SourceBranch, Name: "primary-branch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source.SelectedCommit != branchHead {
		t.Fatalf("primary selected %s, want %s", sess.Source.SelectedCommit, branchHead)
	}
	if len(sess.Companions) != 1 || sess.Companions[0].BaseCommit != companionHead {
		t.Fatalf("companions = %+v, want the configured default %s", sess.Companions, companionHead)
	}
	for _, receipt := range sess.RepositoryFreshness {
		if receipt.Name == "docs" && receipt.RequestedRevision != "refs/heads/main" {
			t.Fatalf("companion receipt = %+v, want its configured branch", receipt)
		}
	}
}

// The admitted source tree is what completion is judged against for EVERY kind, not only a pull
// request: a session that committed nothing still shows the tree it inherited, and one that
// committed work no longer matches it.
func TestAdmittedSourceTreeIsPublishedForEverySelectedSource(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "admitted")
	fixture.write("inherited")
	branchHead := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "admitted")
	fixture.advanceMain("main-advance")
	service := fixture.service()

	for name, source := range map[string]session.SourceSelector{
		"default": {Kind: session.SourceDefault},
		"branch":  {Kind: session.SourceBranch, Name: "admitted"},
	} {
		requested := source
		sess, err := service.CreateRemoteSession(context.Background(), "admitted-"+name, CreateRemoteSessionRequest{
			Policy: "responder", Task: "admitted tree " + name, Source: &requested,
		})
		if err != nil {
			t.Fatal(err)
		}
		changes, err := service.GetChanges(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if changes.AdmittedSourceTree == "" || changes.AdmittedSourceTree != sess.Source.AdmittedTree {
			t.Fatalf("%s admitted tree = %q, want the binding's %q", name, changes.AdmittedSourceTree, sess.Source.AdmittedTree)
		}
		if changes.ForkTree != changes.AdmittedSourceTree {
			t.Fatalf("%s untouched fork tree = %q, admitted %q", name, changes.ForkTree, changes.AdmittedSourceTree)
		}
		if err := os.WriteFile(filepath.Join(sess.Workspace, "task-change.txt"), []byte("work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sessionWorkspaceGit(t, sess.Workspace, "add", "task-change.txt")
		sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "task change")
		changes, err = service.GetChanges(context.Background(), sess.ID)
		if err != nil || changes.ForkTree == changes.AdmittedSourceTree {
			t.Fatalf("%s changed fork tree = %q, admitted %q, err=%v", name, changes.ForkTree, changes.AdmittedSourceTree, err)
		}
		if name == "branch" && gitOut(sess.Workspace, "rev-parse", "HEAD~1") != branchHead {
			t.Fatal("branch workspace lost its inherited commit")
		}
	}
}

// A crash between the reservation and the pin retries resolution under the SAME operation. A
// crash after the durable intent was replaced replays the exact commits instead of resolving
// again onto a branch that moved in between. Neither duplicates a fork.
func TestCreateCrashBeforeAndAfterTheSourcePinKeepsOneOperationIdentity(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "crash")
	fixture.write("first")
	first := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "crash")
	stateRoot := filepath.Join(t.TempDir(), "state")
	request := CreateRemoteSessionRequest{
		Policy: "responder", Task: "crash window",
		Source: &session.SourceSelector{Kind: session.SourceBranch, Name: "crash"},
	}

	// The daemon goes away between the durable reservation and the pin: the operation stays
	// running with its captured request and no resolved objects, exactly as a crash leaves it.
	crashed := newTestSessionService(t, stateRoot, fixture.policies, nil)
	crashCtx, stopDaemon := context.WithCancel(context.Background())
	entered, release := make(chan struct{}), make(chan struct{})
	crashed.testBeforeCreatePin = func() error { close(entered); <-release; return crashCtx.Err() }
	crashErr := make(chan error, 1)
	go func() {
		_, err := crashed.CreateRemoteSession(crashCtx, "crash-key", request)
		crashErr <- err
	}()
	<-entered
	stopDaemon()
	close(release)
	if err := <-crashErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted create = %v, want the cancellation", err)
	}
	interrupted, err := crashed.store.GetOperation(context.Background(), "crash-key")
	if err != nil || interrupted.State != session.OperationRunning {
		t.Fatalf("interrupted operation = %+v, err=%v, want a running intent", interrupted, err)
	}
	if err := crashed.Stop(); err != nil {
		t.Fatal(err)
	}

	// The branch moved while the daemon was down; a retry before any pin existed takes the tip
	// it can actually prove now.
	fixture.seedGit("checkout", "-q", "crash")
	fixture.write("second")
	second := gitOut(fixture.seed, "rev-parse", "HEAD")
	fixture.seedGit("push", "-q", "origin", "crash")

	resumed := newTestSessionService(t, stateRoot, fixture.policies, nil)
	defer resumed.Stop()
	sess, err := resumed.CreateRemoteSession(context.Background(), "crash-key", request)
	if err != nil {
		t.Fatalf("retry after a crash before the pin: %v", err)
	}
	if sess.Source.SelectedCommit != second || first == second {
		t.Fatalf("retried pin = %s, want the current tip %s", sess.Source.SelectedCommit, second)
	}

	// After the pin, the branch moves again and a replay is answered with the pinned session.
	fixture.seedGit("checkout", "-q", "crash")
	fixture.write("third")
	fixture.seedGit("push", "-q", "origin", "crash")
	replayed, err := resumed.CreateRemoteSession(context.Background(), "crash-key", request)
	if err != nil || replayed.ID != sess.ID || replayed.Source.SelectedCommit != second {
		t.Fatalf("replay after the pin = %+v, err=%v", replayed.Source, err)
	}
	if forks := workspaceEntries(t, fixture.cache); len(forks) != 1 {
		t.Fatalf("crash window produced %d workspaces, want exactly one", len(forks))
	}
}

// A bare session has no repository to select a source in, and says so before anything is
// journaled.
func TestBareSessionRefusesASourceSelector(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	policies := testSessionPolicies(repo)
	bare := policies["responder"]
	bare.Name, bare.Mode, bare.Repository = "bare", "bare", ""
	bare.OmitEnv, bare.OmitMCP = true, true
	policies["bare"] = bare
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), policies, nil)
	defer service.Stop()

	_, err := service.CreateRemoteSession(context.Background(), "bare-source", CreateRemoteSessionRequest{
		Policy: "bare", Task: "bare with a source", Source: selector(session.SourceDefault),
	})
	if session.CodeOf(err) != session.CodeInvalidRequest ||
		!strings.Contains(err.Error(), "bare session") {
		t.Fatalf("bare selector error = %v", err)
	}
}

// A create intent written by a binary that only knew the pull-request dialect cannot be replayed
// under the generic contract: resolving it as "default" would silently move the session onto the
// default branch. It fails closed and the operator re-requests it.
func TestReplayRefusesARetiredPullRequestCreateIntent(t *testing.T) {
	fixture := newSelectorFixture(t)
	service := fixture.service()
	ctx := context.Background()
	op, _, err := service.store.ReserveOperation(ctx, "CreateRemoteSession", "legacy-intent", map[string]any{
		"policy": "responder", "task": "legacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := fmt.Sprintf(
		`{"operation_id":%q,"policy":{"Name":"responder","Repository":%q,"Remote":"origin","Branch":"main","Targets":["codex@work"],"MaxTurns":10,"TurnTimeout":1000000000,"MaxPatchBytes":1048576},"task":"legacy","session_id":%q,"fork_name":%q,"pull_request":{"number":514,"ref":"refs/pull/514/head","head_commit":%q}}`,
		op.ID, fixture.cache, deterministicSessionID(op.ID), deterministicForkName(op.ID), strings.Repeat("a", 40),
	)
	if err := service.store.MarkOperationRunning(ctx, op.ID, []byte(intent)); err != nil {
		t.Fatal(err)
	}
	running, err := service.store.GetOperationByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.replayCreateOperation(ctx, running); session.CodeOf(err) != session.CodeOperationUncertain {
		t.Fatalf("retired dialect replay = %v, want operation_uncertain", err)
	}
}

// A fence occupies the target create's exact ledger identity, so the selector must hash into a
// fenced request exactly as it does into the create. Proving that with equal digests would only
// prove the arithmetic; this proves the CONSEQUENCE: a create that arrives after its fence is
// answered with the fence, and a create carrying another selector is refused as the different
// request it is.
func TestFenceAndCreateBindTheSameSourceSelector(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.seedGit("checkout", "-qb", "fenced")
	fixture.write("fenced-change")
	fixture.seedGit("push", "-q", "origin", "fenced")
	service := fixture.service()
	handler := NewHTTPHandler(service)

	const request = `{"policy":"responder","task":"fenced work","source":{"kind":"branch","name":"fenced"}}`
	fence := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/operations/fence",
		`{"method":"CreateRemoteSession","request":`+request+`}`, "fenced-create", "application/json")
	if fence.Code != http.StatusOK {
		t.Fatalf("fence status=%d body=%s", fence.Code, fence.Body.String())
	}
	replayed := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions",
		request, "fenced-create", "application/json")
	if !strings.Contains(replayed.Body.String(), "operation_fenced") {
		t.Fatalf("create after its fence status=%d body=%s", replayed.Code, replayed.Body.String())
	}
	if forks := workspaceEntries(t, fixture.cache); len(forks) != 0 {
		t.Fatalf("a fenced create produced %d workspaces", len(forks))
	}

	fence = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/operations/fence",
		`{"method":"CreateRemoteSession","request":`+request+`}`, "switched-create", "application/json")
	if fence.Code != http.StatusOK {
		t.Fatalf("second fence status=%d body=%s", fence.Code, fence.Body.String())
	}
	switched := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions",
		`{"policy":"responder","task":"fenced work","source":{"kind":"default"}}`,
		"switched-create", "application/json")
	if !strings.Contains(switched.Body.String(), "idempotency_conflict") {
		t.Fatalf("create with another selector status=%d body=%s", switched.Code, switched.Body.String())
	}
}

// A create intent that was already PINNED by a binary without source selection carries exact
// commits but no binding a controller could validate. Materializing it would produce a session
// nobody can check the source of, so the replay fails closed and the operator requests it again.
// An unpinned intent of the same age is different: it asked for the policy's own configured
// branch, which the default selector reproduces exactly, so it resolves normally.
func TestReplayRefusesAPinnedIntentThatResolvedNoSourceBinding(t *testing.T) {
	fixture := newSelectorFixture(t)
	head := fixture.advanceMain("pinned-legacy")
	service := fixture.service()
	ctx := context.Background()
	request := CreateRemoteSessionRequest{Policy: "responder", Task: "pinned legacy"}

	op, _, err := service.store.ReserveOperation(ctx, "CreateRemoteSession", "pinned-legacy", request)
	if err != nil {
		t.Fatal(err)
	}
	pins, err := pinSessionPolicySources(ctx, fixture.policies["responder"], session.DefaultSourceSelector())
	if err != nil {
		t.Fatal(err)
	}
	intent := sessionCreateIntent{
		OperationID: op.ID, Policy: fixture.policies["responder"], Task: request.Task,
		SessionID: deterministicSessionID(op.ID), ForkName: deterministicForkName(op.ID),
		BaseCommit: pins.creationBase, WorkspaceCommit: pins.workspaceHead,
		RepositoryFreshness: pins.receipts,
	}
	if intent.BaseCommit != head {
		t.Fatalf("fixture pinned %s, want %s", intent.BaseCommit, head)
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.store.MarkOperationRunning(ctx, op.ID, encoded); err != nil {
		t.Fatal(err)
	}
	running, err := service.store.GetOperationByID(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.replayCreateOperation(ctx, running); session.CodeOf(err) != session.CodeOperationUncertain {
		t.Fatalf("pinned intent without a binding = %v, want operation_uncertain", err)
	}
	if forks := workspaceEntries(t, fixture.cache); len(forks) != 0 {
		t.Fatalf("a refused pinned intent created %d workspaces", len(forks))
	}

	// The same-age intent that never got as far as pinning resolves under the default selector.
	unpinned, _, err := service.store.ReserveOperation(ctx, "CreateRemoteSession", "unpinned-legacy", request)
	if err != nil {
		t.Fatal(err)
	}
	intent = sessionCreateIntent{
		OperationID: unpinned.ID, Policy: fixture.policies["responder"], Task: request.Task,
		SessionID: deterministicSessionID(unpinned.ID), ForkName: deterministicForkName(unpinned.ID),
	}
	if encoded, err = json.Marshal(intent); err != nil {
		t.Fatal(err)
	}
	if err := service.store.MarkOperationRunning(ctx, unpinned.ID, encoded); err != nil {
		t.Fatal(err)
	}
	sess, err := service.CreateRemoteSession(ctx, "unpinned-legacy", request)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Source == nil || sess.Source.Kind != session.SourceDefault || sess.Source.SelectedCommit != head {
		t.Fatalf("normalized legacy intent binding = %+v", sess.Source)
	}
}
