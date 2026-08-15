package sessionsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestSessionHTTPUnixSocketOwnershipAndStalePaths(t *testing.T) {
	root := shortSessionSocketRoot(t)
	listener, cleanup, err := ListenSocket(root, filepath.Join(root, "nested", "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "nested", "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode = %v, want socket", info.Mode())
	}
	if got, err := os.Stat(filepath.Join(root, "nested")); err != nil || got.Mode().Perm() != 0o700 {
		t.Fatalf("socket parent = %v, err=%v, want 700 directory", got, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(listener) }()
	response, err := sessionUnixHTTPClient(filepath.Join(root, "nested", "control.sock")).Get("http://unix/healthz")
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("Unix HTTP request status=%v err=%v", responseStatus(response), err)
	}
	_ = response.Body.Close()
	_ = server.Close()
	cleanup()
	if _, err := os.Lstat(filepath.Join(root, "nested", "control.sock")); !os.IsNotExist(err) {
		t.Fatalf("owned socket after cleanup: err=%v", err)
	}

	staleListener, staleCleanup, err := ListenSocket(root, filepath.Join(root, "stale.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := staleListener.Close(); err != nil {
		t.Fatal(err)
	}
	// Keep the exact stale socket in place; the next owner may unlink a socket only.
	_ = staleCleanup
	newListener, newCleanup, err := ListenSocket(root, filepath.Join(root, "stale.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = newListener.Close()
	newCleanup()

	symlink := filepath.Join(root, "symlink.sock")
	if err := os.Symlink(filepath.Join(root, "missing.sock"), symlink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ListenSocket(root, symlink); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink socket error = %v", err)
	}

	nonSocket := filepath.Join(root, "regular.sock")
	if err := os.WriteFile(nonSocket, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ListenSocket(root, nonSocket); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("non-socket error = %v", err)
	}

	realRoot := t.TempDir()
	symlinkRoot := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ListenSocket(symlinkRoot, filepath.Join(symlinkRoot, "control.sock")); err == nil || !strings.Contains(err.Error(), "state root") {
		t.Fatalf("symlink state root error = %v", err)
	}
}

func TestSessionHTTPStrictBodiesAndRedaction(t *testing.T) {
	service, repo := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := NewHTTPHandler(service)

	response := sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder","task":"task"}`, "", "application/json")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("missing idempotency response = %d %s", response.Code, response.Body.String())
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder","task":"task","extra":true}`, "create-unknown", "application/json")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown body field status = %d body=%s", response.Code, response.Body.String())
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder"}`, "create-content", "text/plain")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("content type status = %d body=%s", response.Code, response.Body.String())
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder","task":"task"} {}`, "create-two", "application/json")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("multiple values status = %d body=%s", response.Code, response.Body.String())
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", strings.Repeat("x", sessionHTTPMaxBody+1), "create-large", "application/json")
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d body=%s", response.Code, response.Body.String())
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions?unexpected=true", `{"policy":"responder","task":"task"}`, "create-query", "application/json")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("mutation query status = %d body=%s", response.Code, response.Body.String())
	}
	duplicate := httptest.NewRequest(http.MethodPost, "/v1/sessions", strings.NewReader(`{"policy":"responder","task":"task"}`))
	duplicate.Header.Add("Idempotency-Key", "first")
	duplicate.Header.Add("Idempotency-Key", "second")
	duplicate.Header.Set("Content-Type", "application/json")
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicate)
	if duplicateResponse.Code != http.StatusBadRequest {
		t.Fatalf("duplicate idempotency status = %d body=%s", duplicateResponse.Code, duplicateResponse.Body.String())
	}
	response = sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions",
		`{"policy":"responder","task":"pull request","pull_request":{"number":514,"head_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
		"create-pull-request", "application/json",
	)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "operator-configured remote") {
		t.Fatalf("pull request body status = %d body=%s", response.Code, response.Body.String())
	}

	response = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder","task":"task"}`, "create", "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Session.ID == "" || created.Operation.ID == "" {
		t.Fatalf("create response = %+v", created)
	}
	if strings.Contains(response.Body.String(), repo) || strings.Contains(response.Body.String(), "workspace") {
		t.Fatalf("create response leaked host data: %s", response.Body.String())
	}
	redacted, err := json.Marshal(struct {
		Operation OperationDTO     `json:"operation"`
		Turn      TurnDTO          `json:"turn"`
		Review    SessionReviewDTO `json:"review"`
	}{
		Operation: publicOperation(session.Operation{ErrorCode: session.CodeInternal, ErrorDetail: "/secret/host/path"}),
		Turn:      publicTurn(session.Turn{ErrorCode: session.CodeInternal, ErrorDetail: "/secret/host/path"}),
		Review:    publicReview(ReviewDossier{GateError: "/secret/host/path"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), "/secret/host/path") {
		t.Fatalf("public error DTO leaked host path: %s", redacted)
	}

	if _, err := service.Store().BindNativeSession(context.Background(), created.Session.ID, "native-secret"); err != nil {
		t.Fatal(err)
	}
	response = sessionHTTPTestRequest(t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/turns", `{"expected_revision":1,"prompt":"secret prompt"}`, "turn", "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("turn status = %d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret prompt") || strings.Contains(response.Body.String(), "turn") && strings.Contains(response.Body.String(), "idempotency") {
		t.Fatalf("turn response leaked private data: %s", response.Body.String())
	}
	var turnResponse sessionMutationTurnResponse
	if err := json.Unmarshal(response.Body.Bytes(), &turnResponse); err != nil {
		t.Fatal(err)
	}
	invalidTurn := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/turns",
		`{"expected_revision":999,"prompt":"stale"}`, "turn-invalid", "application/json",
	)
	if invalidTurn.Code != http.StatusConflict ||
		!strings.Contains(invalidTurn.Body.String(), `"operation_id":"`) ||
		!strings.Contains(invalidTurn.Body.String(), `"code":"revision_conflict"`) {
		t.Fatalf("correlated turn failure status=%d body=%s", invalidTurn.Code, invalidTurn.Body.String())
	}
	operationResponse := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/operations/"+created.Operation.ID, "", "", "")
	if operationResponse.Code != http.StatusOK {
		t.Fatalf("operation status = %d body=%s", operationResponse.Code, operationResponse.Body.String())
	}
	if strings.Contains(operationResponse.Body.String(), "native-secret") || strings.Contains(operationResponse.Body.String(), "secret prompt") || strings.Contains(operationResponse.Body.String(), "request_hash") || strings.Contains(operationResponse.Body.String(), "result") {
		t.Fatalf("operation response leaked private data: %s", operationResponse.Body.String())
	}
	operationByKey := sessionHTTPTestRequest(
		t, handler, http.MethodGet, "/v1/operations?key=create", "", "", "",
	)
	if operationByKey.Code != http.StatusOK ||
		!strings.Contains(operationByKey.Body.String(), created.Operation.ID) ||
		strings.Contains(operationByKey.Body.String(), "idempotency_key") {
		t.Fatalf("operation-by-key status=%d body=%s", operationByKey.Code, operationByKey.Body.String())
	}
	getResponse := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+created.Session.ID, "", "", "")
	if getResponse.Code != http.StatusOK || strings.Contains(getResponse.Body.String(), repo) || strings.Contains(getResponse.Body.String(), "native-secret") {
		t.Fatalf("session response leaked private data: %d %s", getResponse.Code, getResponse.Body.String())
	}
}

func TestSessionHTTPAsyncCreateReturnsOperationAndCoalescesReplay(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	service, _ := newHTTPTestSessionService(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	service.testBeforeCreatePin = func() error {
		close(entered)
		<-release
		return nil
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	handler := NewHTTPHandler(service)

	requestAsync := func(key, policy string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost, "http://unix/v1/sessions",
			strings.NewReader(fmt.Sprintf(`{"policy":%q,"task":"cold session"}`, policy)),
		)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Prefer", "wait=1, RESPOND-ASYNC")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	response := requestAsync("async-http-create", "responder")
	if response.Code != http.StatusAccepted {
		t.Fatalf("async create status=%d body=%s", response.Code, response.Body.String())
	}
	var admitted sessionAsyncOperationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if admitted.Operation.ID == "" || admitted.Operation.State != session.OperationRunning {
		t.Fatalf("async create response = %+v", admitted)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("asynchronous create did not begin")
	}
	replay := requestAsync("async-http-create", "responder")
	var replayed sessionAsyncOperationResponse
	if replay.Code != http.StatusAccepted || json.Unmarshal(replay.Body.Bytes(), &replayed) != nil ||
		replayed.Operation.ID != admitted.Operation.ID {
		t.Fatalf("async replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	close(release)
	var operation OperationDTO
	waitForSessionTest(t, func() bool {
		got := sessionHTTPTestRequest(
			t, handler, http.MethodGet, "/v1/operations/"+admitted.Operation.ID, "", "", "",
		)
		if got.Code != http.StatusOK {
			return false
		}
		return json.Unmarshal(got.Body.Bytes(), &operation) == nil &&
			operation.State == session.OperationSucceeded
	})
	if operation.ResourceType != "session" || operation.ResourceID == "" {
		t.Fatalf("completed async operation = %+v", operation)
	}

	failure := requestAsync("async-http-invalid", "missing")
	if failure.Code != http.StatusBadRequest ||
		!strings.Contains(failure.Body.String(), `"operation_id":"`) ||
		!strings.Contains(failure.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("correlated failure status=%d body=%s", failure.Code, failure.Body.String())
	}
}

func TestSessionHTTPExistingPullRequestBindingSurvivesCreateAndReview(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	seed, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("branch", "-M", "main")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTest(t, "", "init", "-q", "--bare", remote)
	git("remote", "add", "origin", remote)
	git("push", "-q", "-u", "origin", "main")
	git("checkout", "-qb", "pull-514")
	if err := os.WriteFile(filepath.Join(seed, "pull.txt"), []byte("pull request\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "pull.txt")
	git("commit", "-qm", "pull request")
	pullHead := gitOut(seed, "rev-parse", "HEAD")
	git("push", "-q", "origin", "HEAD:refs/pull/514/head")

	checkout := filepath.Join(t.TempDir(), "checkout")
	runGitTest(t, "", "clone", "-q", "-b", "main", remote, checkout)
	policies := testSessionPolicies(checkout)
	policy := policies["responder"]
	policy.Remote, policy.Branch = "origin", "main"
	policies["responder"] = policy
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), policies, nil)
	service.reviewGate = ReviewGateFunc(func(context.Context, string, string) (ReviewGateResult, error) {
		return ReviewGateResult{Configured: true, Passed: true}, nil
	})
	defer service.Stop()
	handler := NewHTTPHandler(service)
	create := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions",
		fmt.Sprintf(`{"policy":"responder","task":"pull request","pull_request":{"number":514,"head_commit":%q}}`, pullHead),
		"create-pull-http", "application/json",
	)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Session.PullRequest == nil || created.Session.PullRequest.Number != 514 ||
		created.Session.PullRequest.Ref != "refs/pull/514/head" ||
		created.Session.PullRequest.HeadCommit != pullHead {
		t.Fatalf("created pull request binding = %+v", created.Session.PullRequest)
	}
	review := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/review",
		fmt.Sprintf(`{"expected_revision":%d}`, created.Session.Revision),
		"review-pull-http", "application/json",
	)
	if review.Code != http.StatusOK {
		t.Fatalf("review status=%d body=%s", review.Code, review.Body.String())
	}
	var reviewed sessionMutationReviewResponse
	if err := json.Unmarshal(review.Body.Bytes(), &reviewed); err != nil {
		t.Fatal(err)
	}
	if reviewed.Review.PullRequest == nil || reviewed.Review.PullRequest.Number != 514 ||
		reviewed.Review.PullRequest.Ref != "refs/pull/514/head" ||
		reviewed.Review.PullRequest.HeadCommit != pullHead {
		t.Fatalf("reviewed pull request binding = %+v", reviewed.Review.PullRequest)
	}
}

func TestSessionHTTPRouteWiring(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := NewHTTPHandler(service)
	post := func(path, body, key string) *httptest.ResponseRecorder {
		return sessionHTTPTestRequest(t, handler, http.MethodPost, path, body, key, "application/json")
	}
	get := func(path string) *httptest.ResponseRecorder {
		return sessionHTTPTestRequest(t, handler, http.MethodGet, path, "", "", "")
	}

	createdResponse := post("/v1/sessions", `{"policy":"responder","task":"routes"}`, "route-create")
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	sessionID := created.Session.ID
	if response := get("/v1/sessions"); response.Code != http.StatusOK {
		t.Fatalf("list sessions status = %d body=%s", response.Code, response.Body.String())
	}
	if response := get("/v1/sessions/" + sessionID); response.Code != http.StatusOK {
		t.Fatalf("get session status = %d body=%s", response.Code, response.Body.String())
	}

	turnResponse := post("/v1/sessions/"+sessionID+"/turns", `{"expected_revision":1,"prompt":"queued"}`, "route-turn")
	var turnCreated sessionMutationTurnResponse
	if err := json.Unmarshal(turnResponse.Body.Bytes(), &turnCreated); err != nil {
		t.Fatal(err)
	}
	turnID := turnCreated.Turn.ID
	if response := get("/v1/sessions/" + sessionID + "/turns"); response.Code != http.StatusOK {
		t.Fatalf("list turns status = %d body=%s", response.Code, response.Body.String())
	}
	if response := get("/v1/sessions/" + sessionID + "/turns/" + turnID); response.Code != http.StatusOK {
		t.Fatalf("get turn status = %d body=%s", response.Code, response.Body.String())
	}
	if response := get("/v1/sessions/" + sessionID + "/events?after=0&limit=20"); response.Code != http.StatusOK {
		t.Fatalf("list events status = %d body=%s", response.Code, response.Body.String())
	}

	cancelResponse := post("/v1/sessions/"+sessionID+"/turns/"+turnID+"/cancel", `{"expected_revision":1}`, "route-cancel")
	if cancelResponse.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", cancelResponse.Code, cancelResponse.Body.String())
	}
	budgetResponse := post("/v1/sessions/"+sessionID+"/budget", `{"expected_revision":2,"additional_turns":1}`, "route-budget")
	if budgetResponse.Code != http.StatusOK {
		t.Fatalf("budget status = %d body=%s", budgetResponse.Code, budgetResponse.Body.String())
	}
	if response := get("/v1/sessions/" + sessionID + "/changes"); response.Code != http.StatusOK {
		t.Fatalf("changes status = %d body=%s", response.Code, response.Body.String())
	}
	reviewResponse := post("/v1/sessions/"+sessionID+"/review", `{"expected_revision":3}`, "route-review")
	if reviewResponse.Code != http.StatusOK {
		t.Fatalf("review status = %d body=%s", reviewResponse.Code, reviewResponse.Body.String())
	}
	closeResponse := post("/v1/sessions/"+sessionID+"/close", `{"expected_revision":3}`, "route-close")
	if closeResponse.Code != http.StatusOK {
		t.Fatalf("close status = %d body=%s", closeResponse.Code, closeResponse.Body.String())
	}
	var closed sessionMutationSessionResponse
	if err := json.Unmarshal(closeResponse.Body.Bytes(), &closed); err != nil {
		t.Fatal(err)
	}
	planResponse := post("/v1/sessions/"+sessionID+"/discard-plan", `{"expected_revision":4}`, "route-plan")
	if planResponse.Code != http.StatusOK {
		t.Fatalf("discard plan status = %d body=%s", planResponse.Code, planResponse.Body.String())
	}
	var planned sessionMutationPlanResponse
	if err := json.Unmarshal(planResponse.Body.Bytes(), &planned); err != nil {
		t.Fatal(err)
	}
	if planned.Plan.OperationID == "" || planned.Plan.Plan.Workspace.Branch == "" {
		t.Fatalf("discard plan = %+v", planned)
	}
	discardResponse := post("/v1/sessions/"+sessionID+"/discard", fmt.Sprintf(`{"plan_operation_id":%q}`, planned.Plan.OperationID), "route-discard")
	if discardResponse.Code != http.StatusOK {
		t.Fatalf("discard status = %d body=%s", discardResponse.Code, discardResponse.Body.String())
	}
}

func TestSessionHTTPGeneratedArtifactIsSeparateFromTurnJSON(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := NewHTTPHandler(service)
	createdResponse := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder","task":"image"}`,
		"image-create", "application/json",
	)
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	turnResponse := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/turns",
		`{"expected_revision":1,"prompt":"draw"}`, "image-turn", "application/json",
	)
	var submitted sessionMutationTurnResponse
	if err := json.Unmarshal(turnResponse.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	turn, found, err := service.store.LeaseNextTurn(context.Background(), created.Session.ID)
	if err != nil || !found {
		t.Fatalf("lease generated-image turn = %+v, %v, %v", turn, found, err)
	}
	if _, err := service.store.MarkTurnSendIntent(context.Background(), created.Session.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.MarkTurnSent(context.Background(), created.Session.ID, turn.ID); err != nil {
		t.Fatal(err)
	}
	data := []byte("\x89PNG\r\n\x1a\nchart")
	digest := sha256.Sum256(data)
	digestHex := hex.EncodeToString(digest[:])
	completed, err := service.store.CompleteTurn(context.Background(), session.CompleteTurnRequest{
		SessionID: created.Session.ID, TurnID: submitted.Turn.ID, Message: "Chart attached.",
		Artifacts: []session.OutputArtifact{{
			ID: "artifact_chart", Name: "load.png", MediaType: "image/png",
			SHA256: digestHex, Bytes: int64(len(data)), Data: data,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	turnGet := sessionHTTPTestRequest(
		t, handler, http.MethodGet,
		"/v1/sessions/"+created.Session.ID+"/turns/"+completed.ID, "", "", "",
	)
	if turnGet.Code != http.StatusOK || !strings.Contains(turnGet.Body.String(), `"name":"load.png"`) ||
		strings.Contains(turnGet.Body.String(), "Y2hhcnQ=") {
		t.Fatalf("turn metadata response = %d %s", turnGet.Code, turnGet.Body.String())
	}
	artifactGet := sessionHTTPTestRequest(
		t, handler, http.MethodGet,
		"/v1/sessions/"+created.Session.ID+"/turns/"+completed.ID+"/artifacts/artifact_chart",
		"", "", "",
	)
	if artifactGet.Code != http.StatusOK || artifactGet.Header().Get("Content-Type") != "image/png" ||
		artifactGet.Header().Get("ETag") != `"`+digestHex+`"` ||
		!strings.Contains(artifactGet.Header().Get("Content-Disposition"), "load.png") ||
		!bytes.Equal(artifactGet.Body.Bytes(), data) {
		t.Fatalf("artifact response = %d headers=%v bytes=%q", artifactGet.Code, artifactGet.Header(), artifactGet.Body.Bytes())
	}
}

// The store has recorded per-turn usage since schema v5, but TurnDTO had no field for it and
// publicTurn copied none, so every caller of the session API read a completed turn and got no
// numbers at all — the store was measuring turns nobody could see. Every commit this feature has
// had landed without touching this seam, because nothing in this file ever looked at it.
func TestSessionHTTPTurnPublishesRecordedUsage(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := NewHTTPHandler(service)
	createdResponse := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder","task":"usage"}`,
		"usage-create", "application/json",
	)
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	turnResponse := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/turns",
		`{"expected_revision":1,"prompt":"measure me"}`, "usage-turn", "application/json",
	)
	var submitted sessionMutationTurnResponse
	if err := json.Unmarshal(turnResponse.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	leased, found, err := service.store.LeaseNextTurn(context.Background(), created.Session.ID)
	if err != nil || !found {
		t.Fatalf("lease measured turn = %+v, %v, %v", leased, found, err)
	}
	if _, err := service.store.MarkTurnSendIntent(context.Background(), created.Session.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.MarkTurnSent(context.Background(), created.Session.ID, leased.ID); err != nil {
		t.Fatal(err)
	}
	want := session.Usage{InputTokens: 8, CachedInputTokens: 979866, OutputTokens: 10939, ReasoningTokens: 512, CostUSD: 0.375, CostRecorded: true}
	completed, err := service.store.CompleteTurn(context.Background(), session.CompleteTurnRequest{
		SessionID: created.Session.ID, TurnID: submitted.Turn.ID, Message: "Measured.",
		Usage:             session.Usage{InputTokens: 8, CachedInputTokens: 979866, OutputTokens: 10939, ReasoningTokens: 512},
		CumulativeCostUSD: 0.375, CostRecorded: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Usage != want {
		t.Fatalf("stored usage = %+v, want %+v", completed.Usage, want)
	}

	// Decoded off the wire rather than through TurnDTO, because what broke was the wire: a
	// client reading these tags found nothing while the database held the numbers.
	var fetched struct {
		Usage session.Usage `json:"usage"`
	}
	turnGet := sessionHTTPTestRequest(
		t, handler, http.MethodGet,
		"/v1/sessions/"+created.Session.ID+"/turns/"+completed.ID, "", "", "",
	)
	if turnGet.Code != http.StatusOK {
		t.Fatalf("get turn status = %d body=%s", turnGet.Code, turnGet.Body.String())
	}
	if err := json.Unmarshal(turnGet.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Usage != want {
		t.Fatalf("turn usage on the wire = %+v, want %+v\nbody=%s", fetched.Usage, want, turnGet.Body.String())
	}

	// The list is the same projection, and a caller totalling what a session spent reads it
	// instead of fetching every turn one at a time.
	var listed []struct {
		Usage session.Usage `json:"usage"`
	}
	listGet := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+created.Session.ID+"/turns", "", "", "")
	if err := json.Unmarshal(listGet.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Usage != want {
		t.Fatalf("listed turn usage = %+v, want one turn with %+v\nbody=%s", listed, want, listGet.Body.String())
	}

	// A turn nobody measured publishes no usage object at all. Zero is a real answer for a
	// trivial turn, so a caller pricing spend has to keep absence distinct from free.
	unmeasured, err := json.Marshal(publicTurn(session.Turn{ID: "turn_unmeasured"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unmeasured), "usage") {
		t.Fatalf("unmeasured turn published a usage object: %s", unmeasured)
	}
}

// The escalation floor is a request field, and the turn body decoder rejects
// unknown fields — so a parameter Coop's struct does not name is a 400 rather
// than a silently ignored hint, and "the wire accepts it" is a claim that has to
// be proven on the wire. It is deliberately not published back on the turn: a
// public turn carries no request data, and what a client can observe instead is
// the session's target and its session.target_rotated event.
func TestATurnSubmittedWithAnEscalationFloorCarriesItOffTheWire(t *testing.T) {
	service, _ := newHTTPTestSessionService(t, "codex@work", "codex:fallback-model@work")
	defer service.Stop()
	handler := NewHTTPHandler(service)
	createdResponse := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions", `{"policy":"responder","task":"escalation"}`,
		"floor-create", "application/json",
	)
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	turnResponse := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/turns",
		`{"expected_revision":1,"prompt":"re-deliver higher","min_target_index":1}`,
		"floor-turn", "application/json",
	)
	if turnResponse.Code != http.StatusOK {
		t.Fatalf("floored turn status = %d body=%s", turnResponse.Code, turnResponse.Body.String())
	}
	var submitted sessionMutationTurnResponse
	if err := json.Unmarshal(turnResponse.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	stored, err := service.store.GetTurn(context.Background(), created.Session.ID, submitted.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MinTargetIndex != 1 {
		t.Fatalf("turn admitted over HTTP has floor %d, want the requested 1", stored.MinTargetIndex)
	}

	offLadder := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/turns",
		`{"expected_revision":1,"prompt":"re-deliver higher","min_target_index":9}`,
		"floor-off-ladder", "application/json",
	)
	if offLadder.Code != http.StatusBadRequest ||
		!strings.Contains(offLadder.Body.String(), "2-rung") {
		t.Fatalf("off-ladder floor status = %d body=%s", offLadder.Code, offLadder.Body.String())
	}
}

type preparingHTTPRunner struct{ calls atomic.Int32 }

func (*preparingHTTPRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	return turn, nil
}

func (r *preparingHTTPRunner) PrepareSession(_ context.Context, _ session.Session, _ time.Duration) error {
	r.calls.Add(1)
	return nil
}

func TestSessionHTTPPreparesPolicyOptedWarmExecution(t *testing.T) {
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	policies := testSessionPolicies(repo)
	policy := policies["responder"]
	policy.WarmIdleTimeout = 15 * time.Minute
	policies["responder"] = policy
	runner := &preparingHTTPRunner{}
	service, err := NewService(Config{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: policies, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	handler := NewHTTPHandler(service)
	createdResponse := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions",
		`{"policy":"responder","task":"warm route"}`, "warm-create", "application/json",
	)
	var created sessionMutationSessionResponse
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	prepared := sessionHTTPTestRequest(
		t, handler, http.MethodPost, "/v1/sessions/"+created.Session.ID+"/prepare",
		fmt.Sprintf(`{"expected_revision":%d}`, created.Session.Revision),
		"warm-prepare", "application/json",
	)
	if prepared.Code != http.StatusOK || runner.calls.Load() != 1 {
		t.Fatalf("prepare status=%d calls=%d body=%s", prepared.Code, runner.calls.Load(), prepared.Body.String())
	}
}

// newHTTPTestSessionService builds the handler's service on the one-rung test policy, or on the
// ladder given — which a test needs before it can name a rung above the first.
func newHTTPTestSessionService(t *testing.T, ladder ...string) (*Service, string) {
	t.Helper()
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	policies := testSessionPolicies(repo)
	if len(ladder) > 0 {
		policy := policies["responder"]
		policy.Targets = mustTargets(ladder...)
		policies["responder"] = policy
	}
	service, err := NewService(Config{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: policies,
		Runner: RunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			return turn, nil
		}),
		ReviewGate: ReviewGateFunc(func(context.Context, string, string) (ReviewGateResult, error) {
			return ReviewGateResult{Configured: true, Passed: true}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

func sessionHTTPTestRequest(t *testing.T, handler http.Handler, method, path, body, key, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://unix"+path, bytes.NewBufferString(body))
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func responseStatus(response *http.Response) any {
	if response == nil {
		return nil
	}
	return response.StatusCode
}

func shortSessionSocketRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "coop-session-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}
