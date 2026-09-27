package workerconnector

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A command from a superseded placement generation whose lease has not expired must not reach
// the session it targets: the controller has moved the session ref on to a newer generation on
// this worker, so a late submit_turn (or a stale ensure_workspace restore) for the old one is
// refused with a definite failure instead of being executed. Reads and cleanup for the old
// generation — checkpoints, artifacts, discard, close — stay allowed; a move checkpoints the old
// placement after the new one exists.
func TestSupersededPlacementCommandsAreRefusedNotExecuted(t *testing.T) {
	executor, api, first := newOriginFixture(t)
	if _, err := executor.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	origin, _ := executor.journal.readCreateOrigin(executor.journal.createOriginPath(first.SessionRef))
	api.operation.State, api.operation.ResourceType, api.operation.ResourceID = "succeeded", "session", "coop-session-1"
	if err := executor.resolveCreateOrigin(context.Background(), origin); err != nil {
		t.Fatal(err)
	}
	second := first
	second.CommandID, second.IdempotencyKey, second.PlacementGeneration = "create-second", "key-second", 2
	api.operation = createOperation{ID: "create-2", Method: "CreateRemoteSession", State: "running"}
	if _, err := executor.Execute(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	before := len(api.requests)

	payload := apiPayload("POST", "/v1/sessions/coop-session-1/turns", json.RawMessage(`{"expected_revision":2,"prompt":"late work"}`))
	late := first
	late.CommandID, late.IdempotencyKey, late.Kind, late.Payload = "turn-late", "responder:work:turn:late:g1", "api_request", payload
	late.LeaseExpiresAt = time.Date(2026, 9, 5, 19, 0, 0, 0, time.UTC) // still valid
	result, err := executor.Execute(context.Background(), late)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "failed" || !strings.Contains(string(result.Error), "placement_superseded") {
		t.Fatalf("late submit_turn result = %+v; want a definite placement_superseded failure", result)
	}
	if len(api.requests) != before {
		t.Fatalf("late submit_turn reached the session: %d new request(s)", len(api.requests)-before)
	}

	// The current generation's turn is unaffected.
	current := late
	current.CommandID, current.IdempotencyKey, current.PlacementGeneration = "turn-current", "responder:work:turn:current:g2", 2
	if result, err := executor.Execute(context.Background(), current); err != nil || result.State == "failed" {
		t.Fatalf("current-generation submit_turn = %+v, %v; want it executed", result, err)
	}
}

func TestGenericPlacementFenceUsesRouteNotQuerySuffix(t *testing.T) {
	for _, path := range []string{"/v1/sessions/s/workspace/restore?ignored=/cancel", "/v1/sessions/s/other/close", "/v1/sess%69ons/s/turns"} {
		if !needsPlacementFence(workerproto.APIRequest{Method: "POST", Path: path}) {
			t.Errorf("mutation escaped fence: %s", path)
		}
	}
	for _, path := range []string{"/v1/sessions/s/close", "/v1/sessions/s/checkpoint?key=value", "/v1/sessions/s/turns/t/cancel"} {
		if needsPlacementFence(workerproto.APIRequest{Method: "POST", Path: path}) {
			t.Errorf("cleanup blocked: %s", path)
		}
	}
	command := createCommand(time.Now().Add(time.Minute))
	command.Payload = apiPayload("POST", "/v1/sess%69ons", json.RawMessage(`{}`))
	if !isCreateRequest(command) {
		t.Fatal("encoded create bypasses staging")
	}
}

func TestGenericPlacementFenceFailsClosedOnCorruptAuthority(t *testing.T) {
	executor, api, command := newOriginFixture(t)
	if err := os.WriteFile(executor.journal.createOriginPath(command.SessionRef), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), command); err == nil || len(api.requests) != 0 {
		t.Fatalf("corrupt placement allowed mutation: %v calls=%d", err, len(api.requests))
	}
}
