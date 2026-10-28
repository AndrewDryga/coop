package workerconnector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestHTTPPublicationGrantBindsIdentityAndWithholdsInvalidSecrets(t *testing.T) {
	intent := workerproto.PublishIntent{JobRef: "job-publish", JobDigest: "digest", SessionID: "session", ReviewOperationID: "review", CommandKey: "publish", Repository: workerproto.RepositoryIdentity{RepositoryRef: "repo:example", GitHubRepository: "example/repo", GitHubRepositoryID: 42}, Request: workerproto.PublishRequest{AuthorizationRef: "approval", CandidateHead: strings.Repeat("a", 40), CandidateTree: strings.Repeat("b", 40), Branch: "coop/fix", BaseBranch: "main", Title: "Fix", Body: "Reviewed work"}}
	fresh := func() publicationGrant {
		return publicationGrant{JobSourceGrant: JobSourceGrant{RepositoryRef: intent.Repository.RepositoryRef, GitHubRepository: intent.Repository.GitHubRepository, GitHubRepositoryID: 42, Token: "PRIVATE_GRANT_FIXTURE", ExpiresAt: time.Now().Add(2 * time.Minute).UTC()}, ActorID: 55}
	}
	marshal := func(grant publicationGrant) string {
		raw, err := json.Marshal(grant)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, test := range []struct {
		name, media string
		status      int
		mutate      func(*publicationGrant)
		raw         func(string) string
	}{
		{name: "valid", media: "application/json"}, {name: "charset", media: "application/json; charset=utf-8"},
		{name: "repository ref", mutate: func(g *publicationGrant) { g.RepositoryRef = "another" }},
		{name: "repository name", mutate: func(g *publicationGrant) { g.GitHubRepository = "other/repo" }},
		{name: "repository id", mutate: func(g *publicationGrant) { g.GitHubRepositoryID = 43 }},
		{name: "actor zero", mutate: func(g *publicationGrant) { g.ActorID = 0 }}, {name: "actor negative", mutate: func(g *publicationGrant) { g.ActorID = -1 }},
		{name: "empty token", mutate: func(g *publicationGrant) { g.Token = "" }}, {name: "long token", mutate: func(g *publicationGrant) { g.Token = strings.Repeat("x", 4097) }},
		{name: "expired", mutate: func(g *publicationGrant) { g.ExpiresAt = time.Now().Add(-time.Minute) }}, {name: "zero expiry", mutate: func(g *publicationGrant) { g.ExpiresAt = time.Time{} }}, {name: "short expiry", mutate: func(g *publicationGrant) { g.ExpiresAt = time.Now().Add(20 * time.Second) }},
		{name: "media", media: "text/plain"}, {name: "unknown field", raw: func(s string) string { return strings.TrimSuffix(s, "}") + `,"secret_extra":"PRIVATE_GRANT_FIXTURE"}` }},
		{name: "wrong type", raw: func(string) string { return `{"actor_id":"PRIVATE_GRANT_FIXTURE"}` }}, {name: "trailing", raw: func(s string) string { return s + ` {}` }}, {name: "oversize", raw: func(s string) string { return s + strings.Repeat(" ", 8193) }},
		{name: "revoked", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			grant := fresh()
			if test.mutate != nil {
				test.mutate(&grant)
			}
			body := marshal(grant)
			if test.raw != nil {
				body = test.raw(body)
			}
			media := test.media
			if media == "" {
				media = "application/json"
			}
			status := test.status
			if status == 0 {
				status = 200
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/coop-workers/jobs/"+intent.JobRef+"/publication-grants" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
					t.Error("wrong grant request")
				}
				var received workerproto.PublishIntent
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil || received != intent {
					t.Errorf("publication identity changed: %v", err)
				}
				w.Header().Set("Content-Type", media)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			transport, err := newHTTPTransport(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			got, err := transport.publicationGrant(t.Context(), intent)
			if test.name == "valid" || test.name == "charset" {
				if err != nil || got != grant {
					t.Fatalf("valid grant changed: %v", err)
				}
				return
			}
			if err == nil || got.Token != "" || strings.Contains(err.Error(), "PRIVATE_GRANT_FIXTURE") || strings.Contains(err.Error(), body) {
				t.Fatalf("invalid grant not safely refused: %v", err)
			}
			if test.name == "revoked" {
				transport.stateRoot = privateWorkerRoot(t)
				result, err := transport.PublishReview(t.Context(), filepath.Join(t.TempDir(), "missing-repository"), intent)
				if err != nil || result.Status != "refused" || result.ErrorCode != "publication_authorization_revoked" {
					t.Fatalf("revoked publication: %+v %v", result, err)
				}
			}
		})
	}
}
