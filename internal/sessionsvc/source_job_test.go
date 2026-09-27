package sessionsvc

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestCreatePreservesTheControllersExactSourceBinding(t *testing.T) {
	for _, kind := range []session.SourceKind{session.SourceBranch, session.SourcePullRequest, session.SourceCommit} {
		t.Run(string(kind), func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "--allow-empty", "-qm", "default")
			base := gitOut(repo, "rev-parse", "HEAD")
			git("checkout", "-qb", "feature")
			if err := os.WriteFile(filepath.Join(repo, "selected.txt"), []byte("selected\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", "selected.txt")
			git("commit", "-qm", "selected")
			fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
			defer fixture.Stop()
			source := *fixture.Job.Source
			binding := &source.Binding
			binding.Kind, binding.DefaultCommit, binding.BaseCommit = kind, base, base
			binding.Requested = session.SourceSelector{Kind: kind}
			switch kind {
			case session.SourceBranch:
				ref := "refs/heads/feature"
				binding.SelectedRef, binding.Requested.Name = &ref, "feature"
			case session.SourcePullRequest:
				ref := "refs/pull/42/head"
				binding.SelectedRef = &ref
				binding.PullRequestNumber, binding.Requested.Number = 42, 42
				binding.PullRequestExpectedHead, binding.Requested.ExpectedHeadCommit = binding.SelectedCommit, binding.SelectedCommit
			case session.SourceCommit:
				binding.SelectedRef, binding.Requested.SHA = nil, binding.SelectedCommit
			}
			stageTestJobBinding(t, fixture.stateRoot, repo, source)
			fixture.Job.Source = &source
			request := fixture.request(t, "test:exact-source")
			created, err := fixture.CreateRemoteSession(context.Background(), "create-exact-source", request)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(created.Source, binding) || created.BaseCommit != base ||
				gitOut(created.Workspace, "rev-parse", "HEAD") != binding.SelectedCommit {
				t.Fatalf("create changed frozen source: %+v; want %+v", created, binding)
			}
			// Advancing the controller's source cannot rewrite an admitted session or retry.
			git("checkout", "-q", "main")
			git("commit", "--allow-empty", "-qm", "default moved")
			replayed, err := fixture.CreateRemoteSession(context.Background(), "create-exact-source", request)
			if err != nil || replayed.ID != created.ID || !reflect.DeepEqual(replayed.Source, binding) {
				t.Fatalf("frozen source replay = %+v, %v", replayed, err)
			}
		})
	}
}

func TestUpstreamMergedJobWorkCanBeDiscardedWithoutAcceptUnmerged(t *testing.T) {
	ctx := context.Background()
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	fixture := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), repo, nil)
	defer fixture.Stop()
	created, err := fixture.CreateRemoteSession(ctx, "create-merged-work", fixture.request(t, "test:merged-work"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Workspace, "work.txt"), []byte("merged work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, created.Workspace, "add", "work.txt")
	sessionWorkspaceGit(t, created.Workspace, "commit", "-qm", "work")
	git("fetch", "--quiet", created.Workspace, "HEAD")
	git("merge", "--ff-only", "FETCH_HEAD")
	closed, err := fixture.Close(ctx, "close-merged-work", session.CloseSessionRequest{SessionID: created.ID, ExpectedRevision: created.Revision})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := fixture.pinCurrentSessionParent(ctx, closed)
	if err != nil || parent != gitOut(repo, "rev-parse", "HEAD") {
		t.Fatalf("refresh merged default = %s, %v", parent, err)
	}
	plan, err := fixture.PlanDiscard(ctx, "plan-merged-work", PlanDiscardRequest{SessionID: created.ID, ExpectedRevision: closed.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOut(created.Repository, "rev-parse", "HEAD"); got != created.Source.SelectedCommit {
		t.Fatalf("refresh changed frozen source HEAD: %s", got)
	}
	discarded, err := fixture.Discard(ctx, "discard-merged-work", DiscardRequest{PlanOperationID: plan.OperationID})
	if err != nil || discarded.State != session.SessionDiscarded {
		t.Fatalf("unforced discard of merged work = %+v, %v", discarded, err)
	}
}
