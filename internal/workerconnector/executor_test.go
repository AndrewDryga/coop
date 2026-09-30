package workerconnector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

func TestDurableCommandReceiptMakesRedeliveryOneIdempotentLocalOperation(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-1","method":"CreateRemoteSession","state":"running"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))

	first, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	if first.State != "succeeded" || len(api.requests) != 1 {
		t.Fatalf("first = %+v, requests = %d", first, len(api.requests))
	}
	if got := api.requests[0]; got.Method != "POST" || got.Path != "/v1/sessions" || got.IdempotencyKey != command.IdempotencyKey {
		t.Fatalf("request = %+v", got)
	}

	redelivered := command
	redelivered.LeaseExpiresAt = now.Add(2 * time.Minute)
	second, err := executor.Execute(context.Background(), redelivered)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if string(second.Resource) != string(first.Resource) || len(api.requests) != 1 {
		t.Fatalf("redelivery = %+v, requests = %d", second, len(api.requests))
	}

	changed := redelivered
	changed.Payload = json.RawMessage(`{"external_ref":"episode-1","policy":"writable","policy_digest":"` + repeatedDigest("b") + `"}`)
	if _, err := executor.Execute(context.Background(), changed); !errors.Is(err, ErrCommandConflict) {
		t.Fatalf("changed payload error = %v, want command conflict", err)
	}
	if len(api.requests) != 1 {
		t.Fatalf("changed payload issued %d requests", len(api.requests))
	}
}

func TestReceivedBeforeCrashIsSafelyResumedUnderTheSameOperationKey(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	journal, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	if _, err := journal.begin(command); err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-after-crash","method":"CreateRemoteSession","state":"running"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: dir, Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), command)
	if err != nil || result.State != "succeeded" || len(api.requests) != 1 {
		t.Fatalf("resume = %+v, %v, requests = %d", result, err, len(api.requests))
	}
}

func TestLeaseAndWorkerFencesFailBeforeTheUnixAPI(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}

	expired := createCommand(now)
	if _, err := executor.Execute(context.Background(), expired); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired error = %v", err)
	}

	wrongWorker := createCommand(now.Add(time.Minute))
	wrongWorker.WorkerID = "worker-b"
	if _, err := executor.Execute(context.Background(), wrongWorker); !errors.Is(err, ErrWorkerMismatch) {
		t.Fatalf("wrong-worker error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executor.Execute(ctx, createCommand(now.Add(time.Minute))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	if receipts, err := executor.journal.pending(); err != nil || len(receipts) != 0 {
		t.Fatalf("fenced commands created receipts: %+v, %v", receipts, err)
	}

	if len(api.requests) != 0 {
		t.Fatalf("fenced commands issued %d requests", len(api.requests))
	}
}

func TestConfirmedPrivateAPIErrorKeepsItsHTTPStatusForResponder(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{err: &APIError{Status: 409, Code: "revision_conflict", Detail: "expected revision 1"}}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), createCommand(now.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Error, &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != float64(409) || body["code"] != "revision_conflict" {
		t.Fatalf("error = %s", result.Error)
	}
}

func TestConnectorKeepsAResultUntilResponderAcknowledgesIt(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"operation":{"id":"operation-create-1","method":"CreateRemoteSession","state":"running"}}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	transport := &scriptedTransport{responses: []workerproto.Response{
		{Version: workerproto.Version, PollRef: "poll:worker-a:1", ServerTime: now, Commands: []workerproto.Command{command}},
		{Version: workerproto.Version, PollRef: "poll:worker-a:2", ServerTime: now, AcknowledgedResultCommandIDs: []string{command.CommandID}},
		{Version: workerproto.Version, PollRef: "poll:worker-a:3", ServerTime: now},
	}}
	connector, err := NewConnector(ConnectorConfig{
		Executor: executor, Hello: func(_ context.Context, clock time.Time) workerproto.WorkerHello { return hello(clock) },
		Now: func() time.Time { return now }, Transport: transport,
	})
	if err != nil {
		t.Fatal(err)
	}

	for index := 0; index < 3; index++ {
		if err := connector.PollOnce(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", index+1, err)
		}
	}
	if len(transport.polls) != 3 {
		t.Fatalf("polls = %d", len(transport.polls))
	}
	if len(transport.polls[1].AcknowledgedCommandIDs) != 1 || len(transport.polls[1].CommandResults) != 1 {
		t.Fatalf("second poll = %+v", transport.polls[1])
	}
	if len(transport.polls[2].AcknowledgedCommandIDs) != 0 || len(transport.polls[2].CommandResults) != 0 {
		t.Fatalf("acknowledged result was retained: %+v", transport.polls[2])
	}
	if len(api.requests) != 1 {
		t.Fatalf("private API requests = %d", len(api.requests))
	}
}

func hello(clock time.Time) workerproto.WorkerHello {
	return workerproto.WorkerHello{
		ID: "worker-a", WorkspaceRef: "workspace-main", ProtocolVersion: "2", BuildVersion: "coop-test",
		ClockAt: clock, SandboxDigest: repeatedDigest("a"), Capabilities: []workerproto.Capability{},
		Capacity: workerproto.Capacity{SessionSlotsFree: 1, SessionSlotsTotal: 1, TurnSlotsFree: 1, TurnSlotsTotal: 1, WorkspaceSlotsFree: 1, WorkspaceSlotsTotal: 1, State: "eligible"},
		State:    "eligible",
	}
}

func createCommand(expires time.Time) workerproto.Command {
	return workerproto.Command{
		CommandID: "018f04f4-1111-7000-8000-000000000001", WorkerID: "worker-a",
		SessionRef: "018f04f4-2222-7000-8000-000000000001", PlacementGeneration: 1,
		LeaseRef: "placement-lease:1", LeaseExpiresAt: expires, Kind: "api_request",
		CommandVersion: workerproto.Version,
		Payload:        json.RawMessage(`{"method":"POST","path":"/v1/sessions","body":{"task":"episode-1","policy":"work-read-only"}}`),
		IdempotencyKey: "responder:work:create:session-1:g1",
	}
}

func repeatedDigest(character string) string {
	value := ""
	for len(value) < 64 {
		value += character
	}
	return value
}

type scriptedTransport struct {
	polls     []workerproto.Poll
	responses []workerproto.Response
}

func (s *scriptedTransport) Poll(_ context.Context, poll workerproto.Poll) (workerproto.Response, error) {
	s.polls = append(s.polls, poll)
	if len(s.responses) == 0 {
		return workerproto.Response{}, errors.New("unexpected poll")
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

type fakeAPI struct {
	requests []Request
	response json.RawMessage
	err      error
}

func (f *fakeAPI) Do(_ context.Context, request Request) (json.RawMessage, error) {
	f.requests = append(f.requests, request)
	return f.response, f.err
}
func (f *fakeAPI) Forward(ctx context.Context, request Request, body io.Reader) (*http.Response, error) {
	return forwardTestAPI(ctx, f, request, body)
}
func forwardTestAPI(ctx context.Context, api API, request Request, body io.Reader) (*http.Response, error) {
	var err error
	request.Body, err = io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	raw, err := api.Do(ctx, request)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
}
func apiPayload(method, path string, body json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(workerproto.APIRequest{Method: method, Path: path, Body: body})
	if err != nil {
		panic(err)
	}
	return raw
}
func responsePayload(body json.RawMessage) json.RawMessage {
	raw, err := encodeWireJSON(workerproto.APIResponse{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: body})
	if err != nil {
		panic(err)
	}
	return raw
}
