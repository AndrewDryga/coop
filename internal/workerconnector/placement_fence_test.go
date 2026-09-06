package workerconnector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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

	submission := map[string]any{
		"contract_version":    "work-final-v1",
		"context":             map[string]any{"mode": "full"},
		"input_artifact_refs": []any{},
		"output_schema":       map[string]any{"type": "object"},
		"prompt":              "late work for the old placement",
	}
	payload, _ := json.Marshal(map[string]any{
		"coop_session_id": "coop-session-1", "expected_revision": 2,
		"submission": submission, "submission_sha256": canonicalDigest(t, submission), "turn_ref": "turn-late",
		"responder_binding": map[string]any{
			"endpoint": "https://responder.example/v1/state-tools/mcp",
			"token":    strings.Repeat("t", 48),
		},
	})
	late := first
	late.CommandID, late.IdempotencyKey, late.Kind, late.Payload = "turn-late", "responder:work:turn:late:g1", "submit_turn", payload
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
