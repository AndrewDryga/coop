package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestSessionHTTPUnixSocketOwnershipAndStalePaths(t *testing.T) {
	root := shortSessionSocketRoot(t)
	listener, cleanup, err := listenSessionSocket(root, filepath.Join(root, "nested", "control.sock"))
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

	staleListener, staleCleanup, err := listenSessionSocket(root, filepath.Join(root, "stale.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := staleListener.Close(); err != nil {
		t.Fatal(err)
	}
	// Keep the exact stale socket in place; the next owner may unlink a socket only.
	_ = staleCleanup
	newListener, newCleanup, err := listenSessionSocket(root, filepath.Join(root, "stale.sock"))
	if err != nil {
		t.Fatal(err)
	}
	_ = newListener.Close()
	newCleanup()

	symlink := filepath.Join(root, "symlink.sock")
	if err := os.Symlink(filepath.Join(root, "missing.sock"), symlink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenSessionSocket(root, symlink); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink socket error = %v", err)
	}

	nonSocket := filepath.Join(root, "regular.sock")
	if err := os.WriteFile(nonSocket, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenSessionSocket(root, nonSocket); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("non-socket error = %v", err)
	}

	realRoot := t.TempDir()
	symlinkRoot := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(realRoot, symlinkRoot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenSessionSocket(symlinkRoot, filepath.Join(symlinkRoot, "control.sock")); err == nil || !strings.Contains(err.Error(), "state root") {
		t.Fatalf("symlink state root error = %v", err)
	}
}

func TestSessionHTTPStrictBodiesAndRedaction(t *testing.T) {
	service, repo := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := newSessionHTTPHandler(service)

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
		Review:    publicReview(SessionReviewDossier{GateError: "/secret/host/path"}),
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
	operationResponse := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/operations/"+created.Operation.ID, "", "", "")
	if operationResponse.Code != http.StatusOK {
		t.Fatalf("operation status = %d body=%s", operationResponse.Code, operationResponse.Body.String())
	}
	if strings.Contains(operationResponse.Body.String(), "native-secret") || strings.Contains(operationResponse.Body.String(), "secret prompt") || strings.Contains(operationResponse.Body.String(), "request_hash") || strings.Contains(operationResponse.Body.String(), "result") {
		t.Fatalf("operation response leaked private data: %s", operationResponse.Body.String())
	}
	getResponse := sessionHTTPTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+created.Session.ID, "", "", "")
	if getResponse.Code != http.StatusOK || strings.Contains(getResponse.Body.String(), repo) || strings.Contains(getResponse.Body.String(), "native-secret") {
		t.Fatalf("session response leaked private data: %d %s", getResponse.Code, getResponse.Body.String())
	}
}

func TestSessionHTTPRouteWiring(t *testing.T) {
	service, _ := newHTTPTestSessionService(t)
	defer service.Stop()
	handler := newSessionHTTPHandler(service)
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

type preparingHTTPRunner struct{ calls atomic.Int32 }

func (*preparingHTTPRunner) Run(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
	return turn, nil
}

func (r *preparingHTTPRunner) PrepareSession(_ context.Context, _ session.Session, _ time.Duration) error {
	r.calls.Add(1)
	return nil
}

func TestSessionHTTPPreparesPolicyOptedWarmExecution(t *testing.T) {
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	policies := testSessionPolicies(repo)
	policy := policies["responder"]
	policy.WarmIdleTimeout = 15 * time.Minute
	policies["responder"] = policy
	runner := &preparingHTTPRunner{}
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: policies, Runner: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Stop()
	handler := newSessionHTTPHandler(service)
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

func newHTTPTestSessionService(t *testing.T) (*SessionService, string) {
	t.Helper()
	repo, git := gitRepo(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service, err := NewSessionService(SessionServiceConfig{
		StateRoot: filepath.Join(t.TempDir(), "state"), Policies: testSessionPolicies(repo),
		Runner: SessionRunnerFunc(func(_ context.Context, _ session.Session, turn session.Turn) (session.Turn, error) {
			return turn, nil
		}),
		ReviewGate: SessionReviewGateFunc(func(context.Context, string, string) (SessionReviewGateResult, error) {
			return SessionReviewGateResult{Configured: true, Passed: true}, nil
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

func TestSessionHTTPDoctorDialerUsesUnixSocket(t *testing.T) {
	root := shortSessionSocketRoot(t)
	socket := filepath.Join(root, "control.sock")
	listener, cleanup, err := listenSessionSocket(root, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"healthy":true,"ready":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	client := sessionUnixHTTPClient(socket)
	var health sessionHealthDTO
	if err := sessionDoctorGet(client, "/healthz", &health); err != nil || !health.Healthy {
		t.Fatalf("Unix doctor health err=%v health=%+v", err, health)
	}
	var ready sessionReadyDTO
	if err := sessionDoctorGet(client, "/readyz", &ready); err != nil || !ready.Ready {
		t.Fatalf("Unix doctor ready err=%v ready=%+v", err, ready)
	}
	code, output := captureSessionDoctorJSON(t, socket)
	if code != 0 {
		t.Fatalf("doctor success code = %d output=%s", code, output)
	}
	var success sessionDoctorResult
	if err := json.Unmarshal([]byte(output), &success); err != nil || !success.Healthy || !success.Ready || success.Error != "" {
		t.Fatalf("doctor success = %+v err=%v output=%s", success, err, output)
	}
	_ = server.Close()
	cleanup()
	code, output = captureSessionDoctorJSON(t, socket)
	if code == 0 {
		t.Fatalf("doctor failure code = %d output=%s", code, output)
	}
	var failure sessionDoctorResult
	if err := json.Unmarshal([]byte(output), &failure); err != nil || failure.Healthy || failure.Ready || failure.Error == "" {
		t.Fatalf("doctor failure = %+v err=%v output=%s", failure, err, output)
	}
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

func captureSessionDoctorJSON(t *testing.T, socket string) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	code, runErr := runSessionDoctor(socket, true)
	_ = writer.Close()
	os.Stdout = previous
	data, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	return code, string(data)
}

func TestSessionCLIPathsUseRealHomeDefaults(t *testing.T) {
	state, policy, socket, err := sessionCLIPaths("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(state, filepath.Join(home, ".local", "state", "coop", "sessions")) || policy != filepath.Join(home, ".config", "coop", "session-policies.yaml") || socket != filepath.Join(state, "control.sock") {
		t.Fatalf("defaults = state %q policy %q socket %q", state, policy, socket)
	}
}
