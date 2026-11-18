package workerconnector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestExistingPullPublicationPreservesTheAuthorsWorkAndMetadata(t *testing.T) {
	for _, scenario := range []string{"success", "escaped_body", "new_commit", "deleted_branch", "changed_base", "revoked", "changed_after_push", "unverified_after_push", "unrelated_candidate", "attempted_revoked", "attempted_newer", "marker_failure", "malformed_marker", "missing_marker_version", "unknown_marker_version"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			repo, git := gitrepo.New(t)
			git("commit", "--allow-empty", "-qm", "default")
			base, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
			if err := os.WriteFile(filepath.Join(repo, "human.txt"), []byte("original work\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-qm", "human change")
			expected, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
			remote := filepath.Join(t.TempDir(), "remote.git")
			if err := runSourceGit(ctx, "", "", "file", "init", "--bare", "--quiet", remote); err != nil {
				t.Fatal(err)
			}
			git("push", "-q", remote, "HEAD:refs/heads/human/change")
			git("commit", "--allow-empty", "-qm", "concurrent human change")
			newer, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
			git("push", "-q", remote, "HEAD:refs/heads/other")
			git("checkout", "--detach", expected)
			if scenario == "unrelated_candidate" {
				git("checkout", "--detach", base)
			}
			if err := os.WriteFile(filepath.Join(repo, "fix.txt"), []byte("selected fix\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-qm", "selected fix")
			head, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
			tree, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD^{tree}")
			intent := workerproto.PublishIntent{JobRef: "job:fix", JobDigest: "digest", SessionID: "session", ReviewOperationID: "review", CommandKey: "publish",
				Repository: workerproto.RepositoryIdentity{RepositoryRef: "repo:example", GitHubRepository: "example/repo", GitHubRepositoryID: 42},
				Request: workerproto.PublishRequest{Mode: "existing_pull_request", AuthorizationRef: "approval", CandidateHead: head, CandidateTree: tree,
					Branch: "human/change", BaseBranch: "main", ExpectedHead: expected, PullRequestNumber: 7, Title: "Unused title", Body: "Unused body"}}
			if scenario == "escaped_body" {
				intent.Request.Body = strings.Repeat("<", 32<<10)
			}
			var reads, grants, writes atomic.Int32
			stateRoot := privateWorkerRoot(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method != "GET" {
					writes.Add(1)
					w.WriteHeader(500)
					return
				}
				if r.URL.Path == "/repos/example/repo" {
					_ = json.NewEncoder(w).Encode(map[string]int{"id": 42})
					return
				}
				n := reads.Add(1)
				pull := githubPublicationPull{Number: 7, URL: "https://github.com/example/repo/pull/7", State: "open", Draft: false}
				pull.User.ID, pull.User.Type = 123, "User"
				pull.Head.Ref, pull.Head.Repo.ID = "human/change", 42
				pull.Head.SHA, _ = sourceGitValue(ctx, remote, "", "file", "rev-parse", "refs/heads/human/change")
				pull.Base.Ref, pull.Base.Repo.ID = "main", 42
				if n == 2 {
					switch scenario {
					case "new_commit":
						if err := runSourceGit(ctx, remote, "", "file", "update-ref", "refs/heads/human/change", newer); err != nil {
							t.Error(err)
						}
					case "deleted_branch":
						if err := runSourceGit(ctx, remote, "", "file", "update-ref", "-d", "refs/heads/human/change"); err != nil {
							t.Error(err)
						}
					case "changed_base":
						pull.Base.Ref = "release"
					}
				}
				if n >= 3 && scenario == "changed_after_push" {
					pull.Base.Ref = "release"
				}
				if n >= 3 && scenario == "unverified_after_push" {
					w.WriteHeader(502)
					return
				}
				_ = json.NewEncoder(w).Encode(pull)
			}))
			defer server.Close()
			host := publicationHost{stateRoot: stateRoot, api: server.URL, client: server.Client(), remote: remote, protocol: "file",
				grant: func(context.Context, workerproto.PublishIntent) (publicationGrant, error) {
					n := grants.Add(1)
					if n == 2 && scenario == "marker_failure" {
						path, _, err := (publicationHost{stateRoot: stateRoot}).publicationProgressPath(intent)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
					}
					if n == 2 && scenario == "revoked" || scenario == "attempted_revoked" {
						return publicationGrant{}, publicationRefusal("publication_authorization_revoked")
					}
					return publicationGrant{JobSourceGrant: JobSourceGrant{Token: "scoped", ExpiresAt: time.Now().Add(time.Hour)}, ActorID: 55}, nil
				}}
			if scenario == "attempted_revoked" || scenario == "attempted_newer" {
				if err := host.savePublicationProgress(intent, publicationProgress{Attempted: true}); err != nil {
					t.Fatal(err)
				}
				git("push", "-q", remote, "HEAD:refs/heads/human/change")
				if scenario == "attempted_newer" {
					git("commit", "--allow-empty", "-qm", "human change after the lost receipt")
					git("push", "-q", remote, "HEAD:refs/heads/human/change")
				}
			}
			if scenario == "malformed_marker" || scenario == "missing_marker_version" || scenario == "unknown_marker_version" {
				path, _, err := host.publicationProgressPath(intent)
				if err != nil {
					t.Fatal(err)
				}
				data := []byte(`{"attempted":true,"result":{"status":"published"}}`)
				if scenario != "malformed_marker" {
					if err := host.savePublicationProgress(intent, publicationProgress{Attempted: true}); err != nil {
						t.Fatal(err)
					}
					data, err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var marker map[string]any
					if err := json.Unmarshal(data, &marker); err != nil {
						t.Fatal(err)
					}
					if marker["version"] != float64(publicationProgressVersion) {
						t.Fatalf("unversioned marker: %s", data)
					}
					delete(marker, "version")
					if scenario == "unknown_marker_version" {
						marker["version"] = 2
					}
					data, err = json.Marshal(marker)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := host.publish(ctx, repo, intent)
			if scenario == "marker_failure" || scenario == "malformed_marker" || scenario == "missing_marker_version" || scenario == "unknown_marker_version" {
				remoteHead, _ := sourceGitValue(ctx, remote, "", "file", "rev-parse", "refs/heads/human/change")
				if err == nil || remoteHead != expected {
					t.Fatalf("unrecorded push: %+v %v %s", result, err, remoteHead)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if writes.Load() != 0 {
				t.Fatal("existing PR metadata was written")
			}
			remoteHead, _ := sourceGitValue(ctx, remote, "", "file", "rev-parse", "--verify", "refs/heads/human/change")
			switch scenario {
			case "success", "escaped_body", "changed_after_push", "unverified_after_push":
				status := "published"
				if scenario == "changed_after_push" {
					status = "published_target_changed"
				}
				if scenario == "unverified_after_push" {
					status = "published_target_unverified"
				}
				if result.Status != status || result.Receipt == nil || result.Receipt.CommitSHA != head || remoteHead != head {
					t.Fatalf("publication: %+v remote=%s", result, remoteHead)
				}
				if scenario == "success" || scenario == "escaped_body" || scenario == "changed_after_push" {
					host.grant = func(context.Context, workerproto.PublishIntent) (publicationGrant, error) {
						t.Error("confirmed receipt tried to mint credentials")
						return publicationGrant{}, publicationRefusal("publication_authorization_revoked")
					}
					replayed, err := host.publish(ctx, repo, intent)
					if err != nil || replayed.Status != status || replayed.Receipt.CommitSHA != head {
						t.Fatalf("recovery: %+v %v", replayed, err)
					}
				}
			case "new_commit":
				if remoteHead != newer || result.ErrorCode != "publication_push_outcome_unverified" {
					t.Fatalf("newer commit overwritten: %+v %s", result, remoteHead)
				}
			case "deleted_branch":
				if remoteHead != "" || result.ErrorCode != "publication_push_outcome_unverified" {
					t.Fatalf("deleted branch recreated: %+v %s", result, remoteHead)
				}
			case "attempted_revoked", "attempted_newer":
				if result.Status != "refused" || result.ErrorCode != "publication_push_outcome_unverified" {
					t.Fatalf("lost push evidence: %+v", result)
				}
			default:
				if remoteHead != expected || result.Status != "refused" {
					t.Fatalf("unauthorized update: %+v %s", result, remoteHead)
				}
			}
		})
	}
}
