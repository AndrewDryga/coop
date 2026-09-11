package sessionsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestCompletedReviewCanBeReadWithoutRunningTheGateAgain(t *testing.T) {
	// A real 54-second review outlived the worker's 30-second HTTP request.
	// The saved failed gate must reach the controller, not replay uncertainty forever.
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	var gateCalls atomic.Int32
	service := newReviewTestService(t, repo, 1<<20, ReviewGateFunc(func(context.Context, string, string) (ReviewGateResult, error) {
		gateCalls.Add(1)
		return ReviewGateResult{Configured: true, Passed: false}, nil
	}))
	defer service.Stop()
	sess := createReviewSession(t, service, "lookup")
	if err := os.WriteFile(filepath.Join(sess.Workspace, "change.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionWorkspaceGit(t, sess.Workspace, "add", "change.txt")
	sessionWorkspaceGit(t, sess.Workspace, "commit", "-qm", "review change")
	dossier, err := service.RunReview(context.Background(), "review-lookup", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(service)
	path := "/v1/sessions/" + sess.ID + "/reviews/" + dossier.OperationID
	for range 2 {
		response := sessionHTTPTestRequest(t, handler, http.MethodGet, path, "", "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("saved review status=%d body=%s", response.Code, response.Body.String())
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		expected, err := json.Marshal(publicReview(dossier))
		if err != nil || string(got["review"]) != string(expected) {
			t.Fatalf("saved review changed: %s, want %s, err=%v", got["review"], expected, err)
		}
		var operation OperationDTO
		if err := json.Unmarshal(got["operation"], &operation); err != nil ||
			operation.ID != dossier.OperationID || operation.Method != "RunReview" ||
			operation.ResourceID != sess.ID || operation.State != "succeeded" {
			t.Fatalf("saved review operation=%+v, err=%v", operation, err)
		}
	}
	if gateCalls.Load() != 1 || dossier.Publishable || dossier.Gate != ReviewGateFailed {
		t.Fatalf("lookup reran or rewrote the failed gate: calls=%d dossier=%+v", gateCalls.Load(), dossier)
	}

	created, err := service.GetOperation(context.Background(), "create-lookup")
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := service.Store().ReserveOperation(context.Background(), "RunReview", "pending-lookup", RunReviewRequest{SessionID: sess.ID, ExpectedRevision: sess.Revision})
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []struct {
		name, path, method string
	}{
		{"another session", "/v1/sessions/another-session/reviews/" + dossier.OperationID, http.MethodGet},
		{"non-review", "/v1/sessions/" + sess.ID + "/reviews/" + created.ID, http.MethodGet},
		{"unfinished review", "/v1/sessions/" + sess.ID + "/reviews/" + pending.ID, http.MethodGet},
		{"mutation", path, http.MethodPost},
		{"extra query", path + "?other=true", http.MethodGet},
	} {
		t.Run(denied.name, func(t *testing.T) {
			response := sessionHTTPTestRequest(t, handler, denied.method, denied.path, "", "", "")
			if response.Code < 400 || response.Code >= 500 {
				t.Fatalf("unsafe review lookup status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestReviewEvidenceCollectionsRemainArraysWhenEmpty(t *testing.T) {
	// The controller requires both arrays. A real completed review omitted
	// policy_findings; an all-green result also lost not_publishable_reasons.
	encoded, err := json.Marshal(publicReview(ReviewDossier{}))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"policy_findings", "not_publishable_reasons"} {
		if string(document[field]) != "[]" {
			t.Errorf("empty %s = %s, want []", field, document[field])
		}
	}
}
