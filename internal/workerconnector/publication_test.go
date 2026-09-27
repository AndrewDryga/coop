package workerconnector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestPublicationPushesExactReviewedCommitAndRecoversLostPRResponse(t *testing.T) {
	ctx := context.Background()
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	base, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "result.txt"), []byte("reviewed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "reviewed")
	head, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
	tree, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD^{tree}")
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := runSourceGit(ctx, "", "", "file", "init", "--bare", "--quiet", remote); err != nil {
		t.Fatal(err)
	}
	intent := workerproto.PublishIntent{JobRef: "job:publish", JobDigest: "digest", SessionID: "session", ReviewOperationID: "review", CommandKey: "publish",
		Repository: workerproto.RepositoryIdentity{RepositoryRef: "repo:example", GitHubRepository: "example/repo", GitHubRepositoryID: 42},
		Request:    workerproto.PublishRequest{AuthorizationRef: "approval", CandidateHead: head, CandidateTree: tree, Branch: "coop/fix", BaseBranch: "main", Title: "Fix", Body: "Reviewed work"}}
	var mu sync.Mutex
	creates, updates, grants := 0, 0, 0
	staleUpdate := true
	var pull *githubPublicationPull
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer host-only" {
			t.Error("missing scoped credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/example/repo":
			_ = json.NewEncoder(w).Encode(map[string]int{"id": 42})
		case r.Method == "GET":
			if r.URL.Query().Get("state") != "all" {
				t.Error("closed PRs must participate in recovery")
			}
			pulls := []githubPublicationPull{}
			if pull != nil {
				pull.Head.SHA, _ = sourceGitValue(ctx, remote, "", "file", "rev-parse", "refs/heads/coop/fix")
				pulls = append(pulls, *pull)
			}
			_ = json.NewEncoder(w).Encode(pulls)
		case r.Method == "POST":
			creates++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["draft"] != true {
				t.Error("publication did not create a draft")
			}
			pull = &githubPublicationPull{Number: 7, URL: "https://github.com/example/repo/pull/7", State: "open", Draft: true}
			pull.User.ID, pull.User.Type = 55, "Bot"
			pull.Head.Ref, pull.Head.SHA, pull.Head.Repo.ID = "coop/fix", head, 42
			pull.Base.Ref, pull.Base.Repo.ID = "main", 42
			w.WriteHeader(http.StatusBadGateway) // GitHub accepted; its response was lost.
		case r.Method == "PATCH":
			updates++
			response := *pull
			if staleUpdate {
				response.Head.SHA = base
				staleUpdate = false
			}
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	defer server.Close()
	host := publicationHost{stateRoot: privateWorkerRoot(t), api: server.URL, client: server.Client(), remote: remote, protocol: "file", grant: func(_ context.Context, got workerproto.PublishIntent) (publicationGrant, error) {
		grants++
		if got != intent {
			t.Fatal("grant request drifted")
		}
		return publicationGrant{JobSourceGrant: JobSourceGrant{Token: "host-only", ExpiresAt: time.Now().Add(time.Hour)}, ActorID: 55}, nil
	}}
	if _, err := host.publish(ctx, repo, intent); err == nil {
		t.Fatal("lost PR response was not exercised")
	}
	if result, err := host.publish(ctx, repo, intent); err == nil || result.Status == "refused" {
		t.Fatalf("stale PR projection must remain retryable: %+v %v", result, err)
	}
	result, err := host.publish(ctx, repo, intent)
	if err != nil || result.Receipt == nil || result.Receipt.CommitSHA != head || result.Receipt.CandidateTree != tree {
		t.Fatalf("recovery: %+v %v", result, err)
	}
	mu.Lock()
	if creates != 1 || updates != 2 || grants != 3 {
		t.Fatalf("duplicate PR or reused grant: creates=%d updates=%d grants=%d", creates, updates, grants)
	}
	mu.Unlock()
	// A moved remote branch is reported with evidence and never overwritten.
	if err := runSourceGit(ctx, remote, "", "file", "update-ref", "refs/heads/coop/fix", base); err != nil {
		t.Fatal(err)
	}
	result, err = host.publish(ctx, repo, intent)
	if err != nil || result.Status != "conflict" || result.Conflict.ObservedHeadSHA != base || result.Conflict.PullRequestNumber != 7 {
		t.Fatalf("CAS conflict: %+v %v", result, err)
	}
	mu.Lock()
	pull.State = "closed"
	mu.Unlock()
	if result, err := host.publish(ctx, repo, intent); err != nil || result.Status != "refused" {
		t.Fatal("closed PR was recreated")
	}
	mu.Lock()
	pull.State = "open"
	pull.User.ID = 999
	mu.Unlock()
	if result, err := host.publish(ctx, repo, intent); err != nil || result.Status != "refused" {
		t.Fatal("another author's PR was updated")
	}
	host.grant = func(context.Context, workerproto.PublishIntent) (publicationGrant, error) {
		return publicationGrant{}, errors.New("revoked")
	}
	if _, err := host.publish(ctx, repo, intent); err == nil {
		t.Fatal("revoked publication was attempted")
	}
}

func TestPublicationUploadsLargeGitAndLFSObjectsAndUpdatesTheExactReadyPR(t *testing.T) {
	ctx := context.Background()
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-qm", "base")
	base, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "remote.git")
	if err := runSourceGit(ctx, "", "", "file", "init", "--bare", "--quiet", remote); err != nil {
		t.Fatal(err)
	}
	git("push", "-q", remote, "HEAD:refs/heads/coop/fix")
	const size = 70 << 20
	file, err := os.Create(filepath.Join(repo, "large.dat"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	chunk := bytes.Repeat([]byte{0, 1, 2, 3}, (1<<20)/4)
	for range 70 {
		if _, err := io.MultiWriter(file, hash).Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	oid := fmt.Sprintf("%x", hash.Sum(nil))
	object := filepath.Join(repo, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	if err := os.MkdirAll(filepath.Dir(object), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(repo, "large.dat"), object); err != nil {
		t.Fatal(err)
	}
	var uploads, creates, updates atomic.Int32
	var endpoint string
	var head string
	pull := func() githubPublicationPull {
		p := githubPublicationPull{Number: 7, URL: "https://github.com/example/repo/pull/7", State: "open", Draft: false}
		p.User.ID, p.User.Type = 55, "Bot"
		p.Head.Ref, p.Head.Repo.ID = "coop/fix", 42
		p.Head.SHA, _ = sourceGitValue(ctx, remote, "", "file", "rev-parse", "refs/heads/coop/fix")
		p.Base.Ref, p.Base.Repo.ID = "main", 42
		return p
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/lfs/objects/batch":
			if r.Header.Get("Authorization") != "" {
				t.Error("repository token crossed to an unrelated LFS endpoint")
			}
			var batch struct {
				Operation string `json:"operation"`
				Objects   []struct {
					OID  string `json:"oid"`
					Size int64  `json:"size"`
				} `json:"objects"`
			}
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || batch.Operation != "upload" || len(batch.Objects) != 1 || batch.Objects[0].OID != oid || batch.Objects[0].Size != size {
				t.Errorf("unexpected LFS upload: %+v %v", batch, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": []any{map[string]any{"oid": oid, "size": size, "actions": map[string]any{"upload": map[string]any{"href": endpoint + "/upload"}}}}})
		case "/upload":
			h := sha256.New()
			n, err := io.Copy(h, r.Body)
			if r.Method != "PUT" || n != size || err != nil || fmt.Sprintf("%x", h.Sum(nil)) != oid {
				t.Errorf("incomplete LFS object: %d %v", n, err)
			}
			uploads.Add(1)
		case "/repos/example/repo":
			_ = json.NewEncoder(w).Encode(map[string]int{"id": 42})
		case "/repos/example/repo/pulls/7":
			if r.Method == "PATCH" {
				updates.Add(1)
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if len(body) != 2 || body["title"] != "Fix" || body["body"] != "Reviewed work" {
					t.Errorf("changed a human's ready-for-review choice: %+v", body)
				}
			}
			_ = json.NewEncoder(w).Encode(pull())
		case "/repos/example/repo/pulls":
			creates.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		default:
			t.Errorf("untrusted endpoint followed: %s", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	endpoint = server.URL
	for path, data := range map[string]string{
		"large.bin":      fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size),
		".gitattributes": "*.bin filter=lfs -text\n",
		".lfsconfig":     "[lfs]\nurl = " + endpoint + "/untrusted\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "reviewed large result")
	head, _ = sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD")
	tree, _ := sourceGitValue(ctx, repo, "", "", "rev-parse", "HEAD^{tree}")
	intent := workerproto.PublishIntent{
		Repository: workerproto.RepositoryIdentity{RepositoryRef: "repo", GitHubRepository: "example/repo", GitHubRepositoryID: 42},
		Request:    workerproto.PublishRequest{AuthorizationRef: "approval", CandidateHead: head, CandidateTree: tree, Branch: "coop/fix", BaseBranch: "main", ExpectedHead: base, PullRequestNumber: 7, Title: "Fix", Body: "Reviewed work"},
	}
	host := publicationHost{stateRoot: privateWorkerRoot(t), api: endpoint, client: server.Client(), remote: remote, protocol: "file", lfsEndpointForTest: endpoint + "/lfs", grant: func(context.Context, workerproto.PublishIntent) (publicationGrant, error) {
		return publicationGrant{JobSourceGrant: JobSourceGrant{Token: "host-only", ExpiresAt: time.Now().Add(time.Hour)}, ActorID: 55}, nil
	}}
	result, err := host.publish(ctx, repo, intent)
	if err != nil || result.Receipt == nil || result.Receipt.CommitSHA != head || uploads.Load() != 1 || updates.Load() != 1 || creates.Load() != 0 {
		t.Fatalf("large publication: %+v %v uploads=%d updates=%d creates=%d", result, err, uploads.Load(), updates.Load(), creates.Load())
	}
	actualSize, err := sourceGitValue(ctx, remote, "", "file", "cat-file", "-s", head+":large.dat")
	if err != nil || actualSize != fmt.Sprint(size) {
		t.Fatalf("large Git blob missing from pushed commit: %s %v", actualSize, err)
	}
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	if _, err := host.publish(ctx, repo, intent); err == nil || updates.Load() != 1 {
		t.Fatal("missing LFS custody was silently accepted on retry")
	}
}
