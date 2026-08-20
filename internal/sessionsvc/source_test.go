package sessionsvc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// A production alert spent more than two hours retrying session creation because a repository
// 265 commits behind needed longer to fetch than the cheap remote-ref lookup. The transfer is
// valid work; finishing it after the lookup budget expires must not turn it into twenty retries.
func TestRepositoryFetchMayOutliveRemoteIdentityLookup(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	seed, seedGit := gitrepo.New(t)
	seedGit("commit", "-q", "--allow-empty", "-m", "base")
	seedGit("branch", "-M", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, "", "init", "-q", "--bare", remote)
	seedGit("remote", "add", "origin", remote)
	seedGit("push", "-q", "-u", "origin", "main")

	checkout := filepath.Join(t.TempDir(), "checkout")
	runGitTest(t, "", "clone", "-q", "-b", "main", remote, checkout)
	if err := os.WriteFile(filepath.Join(seed, "later.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGit("add", "later.txt")
	seedGit("commit", "-q", "-m", "remote advance")
	seedGit("push", "-q", "origin", "main")
	want := gitOut(seed, "rev-parse", "HEAD")

	lookupTimeout := 250 * time.Millisecond
	runner := func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "fetch" {
			select {
			case <-time.After(lookupTimeout + 50*time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return runSessionSourceGit(ctx, dir, args...)
	}
	got, err := pinSessionRepositoryWithTimeouts(
		context.Background(),
		sessionRepositorySource{
			label: "primary", repository: checkout, remote: "origin", branch: "main",
		},
		lookupTimeout, 2*time.Second, runner,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("pinned commit = %s, want %s", got, want)
	}
}

// blitz-core was 265 commits behind its remote ref, but the exact remote head
// object was already in the local object database. Every watch-session create
// still fetched that same hash until its deadline, so ordinary alerts queued
// behind workspace preparation even though no object transfer was necessary.
func TestRepositoryRefreshUsesAnExistingVerifiedCommitWithoutFetching(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	commit := gitOut(repo, "rev-parse", "HEAD")
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "ls-remote" {
			return []byte(commit + "\trefs/heads/main\n"), nil
		}
		return nil, errors.New("fetch should not run for an object already present locally")
	}
	got, err := pinSessionRepositoryWithTimeouts(
		context.Background(),
		sessionRepositorySource{
			label: "primary", repository: repo, remote: "origin", branch: "main",
		},
		time.Second, time.Second, runner,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != commit {
		t.Fatalf("pinned commit = %s, want %s", got, commit)
	}
}

func TestRepositoryFetchRemainsCancellableAtItsOwnDeadline(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	commit := strings.Repeat("a", 40)
	runner := func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "ls-remote" {
			return []byte(commit + "\trefs/heads/main\n"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, err := pinSessionRepositoryWithTimeouts(
		context.Background(),
		sessionRepositorySource{
			label: "primary", repository: repo, remote: "origin", branch: "main",
		},
		time.Second, 25*time.Millisecond, runner,
	)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "refresh primary repository") {
		t.Fatalf("fetch deadline error = %v", err)
	}
	if got := string(session.CodeOf(err)); got != "repository_unavailable" {
		t.Fatalf("fetch deadline code = %q, want repository_unavailable", got)
	}
}
