package workerconnector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// newPullFixture is a reviewed commit, an empty remote and a GitHub stand-in for a new-PR
// publish to coop/fix. pulls is what GitHub lists for the branch; writes counts PR writes.
type newPullFixture struct {
	repo, remote, head string
	intent             workerproto.PublishIntent
	host               publicationHost

	mu     sync.Mutex
	pulls  []githubPublicationPull
	writes int
}

func newNewPullFixture(t *testing.T) *newPullFixture {
	t.Helper()
	ctx := context.Background()
	f := &newPullFixture{}
	var git func(...string)
	f.repo, git = gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	if err := os.WriteFile(filepath.Join(f.repo, "result.txt"), []byte("reviewed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "reviewed")
	f.head, _ = sourceGitValue(ctx, f.repo, "", "", "rev-parse", "HEAD")
	tree, _ := sourceGitValue(ctx, f.repo, "", "", "rev-parse", "HEAD^{tree}")
	f.remote = filepath.Join(t.TempDir(), "remote.git")
	if err := runSourceGit(ctx, "", "", "file", "init", "--bare", "--quiet", f.remote); err != nil {
		t.Fatal(err)
	}
	f.intent = workerproto.PublishIntent{JobRef: "job:publish", JobDigest: "digest", SessionID: "session", ReviewOperationID: "review", CommandKey: "publish",
		Repository: workerproto.RepositoryIdentity{RepositoryRef: "repo:example", GitHubRepository: "example/repo", GitHubRepositoryID: 42},
		Request:    workerproto.PublishRequest{AuthorizationRef: "approval", CandidateHead: f.head, CandidateTree: tree, Branch: "coop/fix", BaseBranch: "main", Title: "Fix", Body: "Reviewed work"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/example/repo":
			_ = json.NewEncoder(w).Encode(map[string]int{"id": 42})
		case r.Method == "GET":
			_ = json.NewEncoder(w).Encode(f.pulls)
		default:
			f.writes++
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	t.Cleanup(server.Close)
	f.host = publicationHost{stateRoot: privateWorkerRoot(t), api: server.URL, client: server.Client(), remote: f.remote, protocol: "file",
		grant: func(context.Context, workerproto.PublishIntent) (publicationGrant, error) {
			return publicationGrant{JobSourceGrant: JobSourceGrant{Token: "host-only", ExpiresAt: time.Now().Add(time.Hour)}, ActorID: 55}, nil
		}}
	return f
}

func (f *newPullFixture) pull(number int, state string, author int64, kind string) githubPublicationPull {
	pull := githubPublicationPull{Number: number, State: state, Draft: true,
		URL: fmt.Sprintf("https://github.com/example/repo/pull/%d", number)}
	pull.User.ID, pull.User.Type = author, kind
	pull.Head.Ref, pull.Head.SHA, pull.Head.Repo.ID = "coop/fix", f.head, 42
	pull.Base.Ref, pull.Base.Repo.ID = "main", 42
	return pull
}

func (f *newPullFixture) remoteHead(t *testing.T) string {
	t.Helper()
	head, _ := sourceGitValue(context.Background(), f.remote, "", "file", "rev-parse", "--verify", "--quiet", "refs/heads/coop/fix")
	return head
}

// A closed PR the App does not own keeps a new-PR publish off its branch: the worker neither
// pushes nor opens a second PR, and the refusal names the newest such PR so the controller can
// say which one and publish to a new branch. A closed PR of the App's own, or any open PR the App
// does not own, still refuses as changed.
func TestANewPullPublishNamesTheClosedPullRequestOnItsBranch(t *testing.T) {
	for name, want := range map[string]struct {
		pulls  func(*newPullFixture) []githubPublicationPull
		code   string
		closed int
	}{
		"closed by a person": {func(f *newPullFixture) []githubPublicationPull {
			return []githubPublicationPull{f.pull(3, "closed", 999, "User")}
		}, workerproto.PublicationClosedPullRequest, 3},
		"two closed, another app's newest": {func(f *newPullFixture) []githubPublicationPull {
			return []githubPublicationPull{f.pull(5, "closed", 77, "Bot"), f.pull(3, "closed", 999, "User")}
		}, workerproto.PublicationClosedPullRequest, 5},
		"the App's own, closed": {func(f *newPullFixture) []githubPublicationPull {
			return []githubPublicationPull{f.pull(3, "closed", 55, "Bot")}
		}, "publication_existing_pull_request_changed", 0},
		"the App's own closed and a person's closed": {func(f *newPullFixture) []githubPublicationPull {
			return []githubPublicationPull{f.pull(4, "closed", 999, "User"), f.pull(3, "closed", 55, "Bot")}
		}, "publication_existing_pull_request_changed", 0},
		// An open PR may sit on a later page, never read.
		"a full page of closed": {func(f *newPullFixture) []githubPublicationPull {
			var pulls []githubPublicationPull
			for number := pullLookupPage; number > 0; number-- {
				pulls = append(pulls, f.pull(number, "closed", 999, "User"))
			}
			return pulls
		}, "publication_existing_pull_request_changed", 0},
		"a person's, open": {func(f *newPullFixture) []githubPublicationPull {
			return []githubPublicationPull{f.pull(3, "open", 999, "User")}
		}, "publication_existing_pull_request_changed", 0},
		"a person's closed and another open": {func(f *newPullFixture) []githubPublicationPull {
			return []githubPublicationPull{f.pull(4, "open", 999, "User"), f.pull(3, "closed", 999, "User")}
		}, "publication_existing_pull_request_changed", 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newNewPullFixture(t)
			f.pulls = want.pulls(f)
			result, err := f.host.publish(context.Background(), f.repo, f.intent)
			if err != nil || result.Status != "refused" || result.ErrorCode != want.code {
				t.Fatalf("publish = %+v %v, want refused %s", result, err, want.code)
			}
			if want.closed == 0 && result.ClosedPullRequest != nil {
				t.Fatalf("named a closed PR for %s: %+v", want.code, result.ClosedPullRequest)
			}
			if want.closed != 0 && (result.ClosedPullRequest == nil || result.ClosedPullRequest.Number != want.closed ||
				result.ClosedPullRequest.URL != fmt.Sprintf("https://github.com/example/repo/pull/%d", want.closed)) {
				t.Fatalf("closed PR = %+v, want #%d", result.ClosedPullRequest, want.closed)
			}
			if head := f.remoteHead(t); head != "" || f.writes != 0 {
				t.Fatalf("refused publish wrote: branch %q, %d PR writes", head, f.writes)
			}
		})
	}
}
