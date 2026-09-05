package sessionsvc

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
	"github.com/AndrewDryga/coop/internal/workerconnector"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestWorkerActivitySurvivesAsyncCreateAcknowledgementAndRestart(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	git("commit", "-q", "--allow-empty", "-m", "base")
	policy := testSessionPolicies(repo)["responder"]
	service, err := NewService(Config{
		StateRoot: filepath.Join(t.TempDir(), "state"), SourceConfig: &config.Config{},
		Policies: map[string]Policy{"responder": policy},
		Runner: RunnerFunc(func(context.Context, session.Session, session.Turn) (session.Turn, error) {
			t.Error("activity qualification unexpectedly ran a provider")
			return session.Turn{}, nil
		}),
	})
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
	handler := NewHTTPHandler(service)
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
	api, err := workerconnector.NewUnixAPI(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	command := workerproto.Command{
		CommandID: "command:create", WorkerID: "worker-a", SessionRef: "remote-session", PlacementGeneration: 1,
		LeaseRef: "lease:create", LeaseExpiresAt: now.Add(time.Hour), Kind: "create_session", CommandVersion: workerproto.Version,
		Payload:        json.RawMessage(`{"external_ref":"async activity","policy":"responder","policy_digest":"` + ResolvedPolicyDigest(policy) + `"}`),
		IdempotencyKey: "operation:create",
	}
	dir := t.TempDir()
	var commands = []workerproto.Command{command}
	var resultACKs []string
	var eventACKs []workerproto.EventAcknowledgement
	poll := func() workerproto.Poll {
		t.Helper()
		executor, err := workerconnector.NewExecutor(workerconnector.ExecutorConfig{
			API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		var sent workerproto.Poll
		connector, err := workerconnector.NewConnector(workerconnector.ConnectorConfig{
			Executor: executor, Now: func() time.Time { return now },
			Hello: func(context.Context, time.Time) workerproto.WorkerHello {
				return workerproto.WorkerHello{
					ID: "worker-a", WorkspaceRef: "workspace-main", ProtocolVersion: "1", BuildVersion: "test",
					ClockAt: now, SandboxDigest: strings.Repeat("a", 64), PolicyDigests: map[string]string{"responder": ResolvedPolicyDigest(policy)},
					State: "eligible", Capacity: workerproto.Capacity{State: "eligible"},
				}
			},
			Transport: workerActivityTransport(func(_ context.Context, request workerproto.Poll) (workerproto.Response, error) {
				sent = request
				return workerproto.Response{Version: workerproto.Version, PollRef: request.PollRef, ServerTime: now,
					Commands: commands, AcknowledgedResultCommandIDs: resultACKs, EventAcknowledgements: eventACKs}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := connector.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return sent
	}
	poll()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("asynchronous create did not start")
	}
	commands, resultACKs = nil, []string{command.CommandID}
	receipt := poll()
	if len(receipt.CommandResults) != 1 || len(receipt.EventBatches) != 0 {
		t.Fatalf("create settlement = %+v", receipt)
	}
	var accepted struct {
		Operation OperationDTO    `json:"operation"`
		Session   json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(receipt.CommandResults[0].Resource, &accepted); err != nil ||
		accepted.Operation.State != session.OperationRunning || len(accepted.Session) != 0 {
		t.Fatalf("create was not the real asynchronous response: %+v, %v", accepted, err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "commands", "*.json"))
	if err != nil || len(files) != 0 {
		t.Fatalf("ACK did not remove command custody before completion: %v, %v", files, err)
	}
	resultACKs = nil
	if got := poll(); len(got.EventBatches) != 0 {
		t.Fatal("activity was bound before the operation completed")
	}
	unblock()
	var completed session.Operation
	waitForSessionTest(t, func() bool {
		completed, err = service.GetOperation(ctx, command.IdempotencyKey)
		return err == nil && completed.State == session.OperationSucceeded
	})
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
	if replay := poll(); len(replay.EventBatches) != 1 || replay.EventBatches[0].Events[0].Sequence != batch.Events[0].Sequence {
		t.Fatal("unacknowledged activity did not replay across restart")
	}
	eventACKs = []workerproto.EventAcknowledgement{{SessionRef: command.SessionRef, PlacementGeneration: 1, Sequence: batch.Events[len(batch.Events)-1].Sequence}}
	poll()
	eventACKs = nil
	if replay := poll(); len(replay.EventBatches) != 0 {
		t.Fatal("acknowledged activity replayed across restart")
	}
	if creates.Load() != 1 {
		t.Fatalf("create executed %d times", creates.Load())
	}
}

type workerActivityTransport func(context.Context, workerproto.Poll) (workerproto.Response, error)

func (f workerActivityTransport) Poll(ctx context.Context, poll workerproto.Poll) (workerproto.Response, error) {
	return f(ctx, poll)
}
