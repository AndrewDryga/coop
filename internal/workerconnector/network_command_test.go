package workerconnector

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The two networking reads are plain owner-private GETs. The connector chooses the path and
// forwards the daemon's answer verbatim: it holds no policy, no projection and no destination —
// the daemon already decided what this session's authority discloses.
func TestExecutorMapsTheNetworkReadCommands(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for kind, wantPath := range map[string]string{
		"get_network":             "/v1/sessions/coop%2Fsession-1/network",
		"get_network_receipt":     "/v1/sessions/coop%2Fsession-1/network/receipt",
		"get_network_connections": "/v1/sessions/coop%2Fsession-1/network/connections",
	} {
		t.Run(kind, func(t *testing.T) {
			api := &fakeAPI{response: json.RawMessage(`{"mode":"filtered"}`)}
			executor, err := NewExecutor(ExecutorConfig{
				API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
			})
			if err != nil {
				t.Fatal(err)
			}
			command := createCommand(now.Add(time.Minute))
			command.Kind, command.Payload = kind, json.RawMessage(`{"coop_session_id":"coop/session-1"}`)
			result, err := executor.Execute(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			if result.State != "succeeded" {
				t.Fatalf("%s result = %+v", kind, result)
			}
			if len(api.requests) != 1 || api.requests[0].Method != "GET" || api.requests[0].Path != wantPath {
				t.Fatalf("%s requests = %+v, want one GET %s", kind, api.requests, wantPath)
			}
			// A read carries no idempotency key and no body: it changes nothing.
			if api.requests[0].IdempotencyKey != "" || len(api.requests[0].Body) != 0 {
				t.Fatalf("%s sent a mutation: %+v", kind, api.requests[0])
			}
			if string(result.Resource) != `{"mode":"filtered"}` {
				t.Fatalf("%s forwarded %s", kind, result.Resource)
			}
		})
	}
}

func TestExecutorRefusesNetworkReadsWithoutASessionIdentity(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"get_network", "get_network_receipt", "get_network_connections", "get_network_explanation"} {
		api := &fakeAPI{}
		executor, err := NewExecutor(ExecutorConfig{
			API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		command := createCommand(now.Add(time.Minute))
		command.Kind, command.Payload = kind, json.RawMessage(`{"coop_session_id":""}`)
		result, err := executor.Execute(context.Background(), command)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != "failed" || len(api.requests) != 0 {
			t.Fatalf("%s with no identity = %+v (requests %d)", kind, result, len(api.requests))
		}
	}
}

// The protocol allowlist is the only gate on command kinds, so a kind that is not in it is not a
// command at all — and one that is must decode from a real controller response.
func TestWorkerProtocolAdmitsTheNetworkReadCommands(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"get_network", "get_network_receipt", "get_network_connections"} {
		command := createCommand(now.Add(time.Minute))
		command.Kind, command.Payload = kind, json.RawMessage(`{"coop_session_id":"coop-session-1"}`)
		if err := command.Validate(); err != nil {
			t.Fatalf("%s rejected by the protocol: %v", kind, err)
		}
	}
	unknown := createCommand(now.Add(time.Minute))
	unknown.Kind = "get_network_secrets"
	if err := unknown.Validate(); err == nil {
		t.Fatal("an unlisted command kind was admitted")
	}
	_ = workerproto.Version
}

// An explanation names one retained event of one session. The connector escapes both references
// and forwards the daemon's answer; an event id that could reach another path is not a reference.
func TestExecutorMapsTheNetworkExplanationCommand(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	api := &fakeAPI{response: json.RawMessage(`{"available":true}`)}
	executor, err := NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command := createCommand(now.Add(time.Minute))
	command.Kind = "get_network_explanation"
	command.Payload = json.RawMessage(`{"coop_session_id":"coop/session-1","event_id":"ab/cd"}`)
	result, err := executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	want := "/v1/sessions/coop%2Fsession-1/network/explanations/ab%2Fcd"
	if result.State != "succeeded" || len(api.requests) != 1 || api.requests[0].Path != want {
		t.Fatalf("explanation request = %+v (result %+v), want one GET %s", api.requests, result, want)
	}
	if api.requests[0].IdempotencyKey != "" || len(api.requests[0].Body) != 0 {
		t.Fatalf("a read sent a mutation: %+v", api.requests[0])
	}
	// Without an event there is nothing to explain, and no request is made.
	api = &fakeAPI{}
	executor, err = NewExecutor(ExecutorConfig{
		API: api, JournalDir: t.TempDir(), Now: func() time.Time { return now }, WorkerID: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	command = createCommand(now.Add(time.Minute))
	command.Kind, command.Payload = "get_network_explanation", json.RawMessage(`{"coop_session_id":"coop/session-1"}`)
	result, err = executor.Execute(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "failed" || len(api.requests) != 0 {
		t.Fatalf("an explanation with no event = %+v (requests %d)", result, len(api.requests))
	}
}
