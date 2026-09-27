package sessionsvc

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/testutil/workertls"
	"github.com/AndrewDryga/coop/internal/workerconnector"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestWorkerActivitySurvivesAsyncCreateAcknowledgementAndRestart(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	service, err := openSessionFixture(t, Config{
		StateRoot: filepath.Join(t.TempDir(), "state"), SourceConfig: &config.Config{},
		Runner: RunnerFunc(func(context.Context, session.Session, session.Turn) (session.Turn, error) {
			t.Error("activity qualification unexpectedly ran a provider")
			return session.Turn{}, nil
		}),
	}, repo)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	service.testBeforeCreatePin = func() error { close(entered); <-release; return nil }
	t.Cleanup(func() { unblock(); _ = service.Stop() })
	ctx := context.Background()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	socketRoot := shortSessionSocketRoot(t)
	socket := filepath.Join(socketRoot, "control.sock")
	listener, cleanup, err := ListenSocket(socketRoot, socket)
	if err != nil {
		t.Fatal(err)
	}
	var creates atomic.Int32
	handler := NewHTTPHandler(service.Service)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sessions" {
			creates.Add(1)
			if r.Header.Get("Prefer") != "respond-async" {
				t.Error("connector did not request asynchronous creation")
			}
		}
		handler.ServeHTTP(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); cleanup() })
	now := time.Now().UTC()
	command := workerproto.Command{
		CommandID: "command:create", WorkerID: "worker-a", SessionRef: "remote-session", PlacementGeneration: 1,
		LeaseRef: "lease:create", LeaseExpiresAt: now.Add(time.Hour), Kind: "api_request", CommandVersion: workerproto.Version,
		Payload:        connectorCreatePayload(t, service.request(t, "test:async-activity")),
		IdempotencyKey: "operation:create",
	}
	dir := t.TempDir()
	ca := workertls.NewCA(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate)
	var enrolls, polls atomic.Int32
	var responseMu sync.Mutex
	reply := workerproto.Response{Commands: []workerproto.Command{command}}
	setResponse := func(value workerproto.Response) {
		responseMu.Lock()
		reply = value
		responseMu.Unlock()
	}
	requests := make(chan workerproto.Poll, 1)
	token := strings.Repeat("t", 43)
	controller := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/v1/coop-workers/enroll" {
			var document map[string]string
			if err := json.NewDecoder(r.Body).Decode(&document); err != nil || document["token"] != token ||
				len(document) != 2 {
				http.Error(w, "invalid enrollment authority", http.StatusForbidden)
				return
			}
			identity, err := ca.Identity(document["public_key_pem"], command.WorkerID, "workspace-main")
			if err != nil {
				t.Error(err)
				http.Error(w, "invalid worker key", http.StatusBadRequest)
				return
			}
			enrolls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(identity)
			return
		}
		// Bootstrap needs anonymous TLS, but no other route may use it.
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 ||
			r.TLS.PeerCertificates[0].Subject.CommonName != command.WorkerID {
			http.Error(w, "verified worker identity required", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/coop-workers/poll" {
			http.NotFound(w, r)
			return
		}
		var request workerproto.Poll
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid poll", http.StatusBadRequest)
			return
		}
		polls.Add(1)
		requests <- request
		responseMu.Lock()
		response := reply
		responseMu.Unlock()
		response.Version, response.PollRef, response.ServerTime = workerproto.Version, request.PollRef, now
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	controller.TLS = &tls.Config{
		Certificates: []tls.Certificate{ca.ServerCertificate(t)}, ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs: roots, MinVersion: tls.VersionTLS13,
	}
	controller.StartTLS()
	t.Cleanup(controller.Close)
	bootstrap := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}, Timeout: time.Second}
	t.Cleanup(bootstrap.CloseIdleConnections)
	for _, route := range []string{"poll", "renew"} {
		response, err := bootstrap.Post(controller.URL+"/v1/coop-workers/"+route, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous %s returned %d", route, response.StatusCode)
		}
	}
	identityPath, tokenPath := filepath.Join(dir, "identity.json"), filepath.Join(dir, "enrollment-token")
	configuration := workerconnector.HTTPTransportConfig{
		BaseURL: controller.URL, CAFile: filepath.Join(dir, "ca.pem"),
		IdentityFile: identityPath, EnrollmentTokenFile: tokenPath, RenewBefore: time.Minute, Timeout: time.Second,
	}
	for path, contents := range map[string][]byte{configuration.CAFile: ca.PEM, tokenPath: []byte(token)} {
		if err := os.WriteFile(path, contents, 0600); err != nil {
			t.Fatal(err)
		}
	}
	newTransport := func() *workerconnector.HTTPTransport {
		t.Helper()
		transport, err := workerconnector.NewHTTPTransport(configuration)
		if err != nil {
			t.Fatal(err)
		}
		return transport
	}
	poll := func() workerproto.Poll {
		t.Helper()
		api, err := workerconnector.NewUnixAPI(socket, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// A new transport reloads the identity from disk; retaining it would not
		// prove enrollment/identity recovery across connector restart.
		transport := newTransport()
		sources, err := workerconnector.NewJobSourceStager(transport, service.stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		executor, err := workerconnector.NewExecutor(workerconnector.ExecutorConfig{
			API: api, BodyTransport: transport, JournalDir: filepath.Join(dir, "journal"),
			JobSourceStager: sources,
			Now:             func() time.Time { return now }, WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		connector, err := workerconnector.NewConnector(workerconnector.ConnectorConfig{
			Executor: executor, Now: func() time.Time { return now },
			Hello: func(ctx context.Context, clock time.Time) workerproto.WorkerHello {
				current := workerproto.WorkerHello{
					ID: "worker-a", WorkspaceRef: "workspace-main", ProtocolVersion: "2", BuildVersion: "test",
					State: "eligible", SandboxDigest: strings.Repeat("a", 64),

					Capabilities: []workerproto.Capability{},
					Capacity:     workerproto.Capacity{SessionSlotsFree: 2, SessionSlotsTotal: 2, TurnSlotsFree: 2, TurnSlotsTotal: 2, WorkspaceSlotsFree: 2, WorkspaceSlotsTotal: 2, State: "eligible"},
				}
				current.ClockAt = clock
				current.Capabilities = workerconnector.LiveCapabilities(ctx, api)
				return current
			},
			Transport: transport,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := connector.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case sent := <-requests:
			return sent
		default:
			t.Fatal("successful poll did not reach the TLS controller")
			return workerproto.Poll{}
		}
	}
	first := poll()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("asynchronous create did not start")
	}
	identity, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(identityPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity is not private: %v, %v", info, err)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("enrollment token was not consumed: %v", err)
	}
	// Redeliver before ACK with a new identity manager and journal reader.
	receipt := poll()
	setResponse(workerproto.Response{AcknowledgedResultCommandIDs: []string{command.CommandID}})
	replayed := poll()
	if !reflect.DeepEqual(receipt.CommandResults, replayed.CommandResults) || creates.Load() != 1 {
		t.Fatalf("restart/redelivery changed receipt or repeated create: before=%+v after=%+v creates=%d", receipt.CommandResults, replayed.CommandResults, creates.Load())
	}
	if len(receipt.CommandResults) != 1 || len(receipt.EventBatches) != 0 {
		t.Fatalf("create settlement = %+v", receipt)
	}
	var accepted struct {
		Operation OperationDTO    `json:"operation"`
		Session   json.RawMessage `json:"session"`
	}
	var createResponse workerproto.APIResponse
	if err := json.Unmarshal(receipt.CommandResults[0].Resource, &createResponse); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(createResponse.Body, &accepted); err != nil ||
		accepted.Operation.State != session.OperationRunning || len(accepted.Session) != 0 {
		t.Fatalf("create was not the real asynchronous response: %+v, %v", accepted, err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "journal", "commands", "*.json"))
	if err != nil || len(files) != 0 {
		t.Fatalf("ACK did not remove command custody before completion: %v, %v", files, err)
	}
	setResponse(workerproto.Response{})
	if got := poll(); len(got.EventBatches) != 0 {
		t.Fatal("activity was bound before the operation completed")
	}
	unblock()
	var completed session.Operation
	waitForSessionTest(t, func() bool {
		completed, err = service.GetOperation(ctx, command.IdempotencyKey)
		return err == nil && sessionOperationReached(t, completed, session.OperationSucceeded)
	})
	if completed.ID != accepted.Operation.ID || completed.IdempotencyKey != command.IdempotencyKey ||
		completed.ResourceType != "session" || completed.ResourceID == "" {
		t.Fatalf("activity resolved a different create operation: accepted=%+v completed=%+v", accepted.Operation, completed)
	}
	var activity workerproto.Poll
	for range 2 { // one bounded scan resolves the origin; the next publishes its events
		activity = poll()
		if len(activity.EventBatches) > 0 {
			break
		}
	}
	if len(activity.EventBatches) != 1 {
		t.Fatalf("ACK-before-completion/restart lost activity: %+v", activity)
	}
	batch := activity.EventBatches[0]
	if batch.SessionRef != command.SessionRef || batch.PlacementGeneration != 1 || len(batch.Events) == 0 {
		t.Fatalf("wrong activity binding: %+v", batch)
	}
	var event workerproto.SessionEvent
	if err := json.Unmarshal(batch.Events[0].Payload, &event); err != nil || event.SessionID != completed.ResourceID || event.Type != "session.created" {
		t.Fatalf("wrong originating session event: %+v, %v", event, err)
	}
	if replay := poll(); !reflect.DeepEqual(replay.EventBatches, activity.EventBatches) {
		t.Fatal("unacknowledged activity did not replay across restart")
	}
	setResponse(workerproto.Response{EventAcknowledgements: []workerproto.EventAcknowledgement{{SessionRef: command.SessionRef, PlacementGeneration: 1, Sequence: batch.Events[len(batch.Events)-1].Sequence}}})
	poll()
	setResponse(workerproto.Response{})
	if replay := poll(); len(replay.EventBatches) != 0 {
		t.Fatal("acknowledged activity replayed across restart")
	}
	if creates.Load() != 1 {
		t.Fatalf("create executed %d times", creates.Load())
	}
	if current, err := os.ReadFile(identityPath); err != nil || !bytes.Equal(current, identity) || enrolls.Load() != 1 {
		t.Fatalf("restart replaced/re-enrolled the identity: enrolls=%d err=%v", enrolls.Load(), err)
	}
	// A malformed saved identity must not fall back to a fresh enrollment,
	// even when the operator has placed a valid bootstrap token beside it.
	if err := os.WriteFile(identityPath, []byte("malformed identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	beforePolls := polls.Load()
	if _, err := newTransport().Poll(ctx, first); err == nil || !strings.Contains(err.Error(), "load worker identity") {
		t.Fatalf("malformed identity did not fail at identity loading: %v", err)
	}
	if enrolls.Load() != 1 || polls.Load() != beforePolls || creates.Load() != 1 {
		t.Fatal("malformed identity enabled enrollment, polling or execution")
	}
	if retained, err := os.ReadFile(tokenPath); err != nil || string(retained) != token {
		t.Fatalf("malformed identity consumed bootstrap authority: %v", err)
	}
	if retained, err := os.ReadFile(identityPath); err != nil || string(retained) != "malformed identity" {
		t.Fatalf("malformed identity was replaced: %v", err)
	}
}

// The real worker-to-private-service path: a worker authorized against policy "responder" keeps
// advertising that policy's digests, the daemon restarts with a same-name policy that resolves
// differently, and the pinned create is refused by the daemon at admission — a definite failure
// on the worker side, a failed operation with no session on the daemon side.
func TestWorkerCannotSendRetiredPolicyCreate(t *testing.T) {
	service := newTestSessionService(t, filepath.Join(t.TempDir(), "state"), "", nil)
	defer service.Stop()
	response := sessionHTTPTestRequest(t, NewHTTPHandler(service.Service), http.MethodPost, "/v1/sessions",
		`{"task":"test:legacy","policy":"responder"}`, "legacy-create", "application/json")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("retired create = %d %s", response.Code, response.Body.String())
	}
	if rows, err := service.ListSessions(context.Background(), 10); err != nil || len(rows) != 0 {
		t.Fatalf("retired create wrote sessions: %v, %v", rows, err)
	}
}

func connectorCreatePayload(t *testing.T, request CreateRemoteSessionRequest) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(workerproto.APIRequest{Method: "POST", Path: "/v1/sessions", Body: mustConnectorJSON(t, request)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func mustConnectorJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
