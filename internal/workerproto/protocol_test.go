package workerproto

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedWorkerGoldenAcceptsOnlyTheVersionedBoundedContract(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "coop-worker-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Poll     json.RawMessage `json:"poll"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(document, &fixture); err != nil {
		t.Fatal(err)
	}

	poll, err := DecodePoll(fixture.Poll)
	if err != nil {
		t.Fatalf("DecodePoll: %v", err)
	}
	if poll.Worker.ID != "worker-a" || len(poll.EventBatches) != 1 {
		t.Fatalf("poll = %+v", poll)
	}

	response, err := DecodeResponse(fixture.Response)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(response.Commands) != 1 || response.Commands[0].Kind != "submit_turn" {
		t.Fatalf("response = %+v", response)
	}
	if len(response.AcknowledgedResultCommandIDs) != 1 || response.AcknowledgedResultCommandIDs[0] != "command:create:1" {
		t.Fatalf("result acknowledgements = %+v", response.AcknowledgedResultCommandIDs)
	}
	if !bytes.Contains(response.Commands[0].Payload, []byte("Continue the selected episode.")) {
		t.Fatal("frozen submission prompt did not cross the worker contract")
	}

	unknown := append([]byte(nil), fixture.Poll...)
	unknown = []byte(strings.Replace(string(unknown), `"version": 1`, `"version": 1, "provider_credentials": ["forbidden"]`, 1))
	if _, err := DecodePoll(unknown); err == nil {
		t.Fatal("unknown authority field was accepted")
	}

	shell := []byte(strings.Replace(string(fixture.Response), `"kind": "submit_turn"`, `"kind": "shell"`, 1))
	if _, err := DecodeResponse(shell); err == nil {
		t.Fatal("generic shell command was accepted")
	}
}

func TestWorkerProtocolRejectsOversizeAndSequenceGaps(t *testing.T) {
	if _, err := DecodePoll([]byte(strings.Repeat("x", MaxDocumentBytes+1))); err == nil {
		t.Fatal("oversized poll was accepted")
	}

	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "coop-worker-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Poll json.RawMessage `json:"poll"`
	}
	if err := json.Unmarshal(document, &fixture); err != nil {
		t.Fatal(err)
	}
	gapped := []byte(strings.Replace(string(fixture.Poll), `"sequence": 2`, `"sequence": 3`, 1))
	if _, err := DecodePoll(gapped); err == nil {
		t.Fatal("gapped event batch was accepted")
	}
}

func TestWorkerProtocolCarriesOneExactPublicSessionEvent(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "coop-worker-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Poll map[string]any `json:"poll"`
	}
	if err := json.Unmarshal(document, &fixture); err != nil {
		t.Fatal(err)
	}
	event := fixture.Poll["event_batches"].([]any)[0].(map[string]any)["events"].([]any)[0].(map[string]any)
	event["kind"] = "session_event"
	event["payload"] = map[string]any{
		"id": "evt-1", "session_id": "coop-session-1", "sequence": 1,
		"turn_id": "turn-1", "type": "tool.started", "version": 1,
		"occurred_at": "2026-08-29T12:00:00Z",
		"payload":     map[string]any{"tool_call_id": "tool-1", "title": "Read repository"},
	}
	encoded, err := json.Marshal(fixture.Poll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePoll(encoded); err != nil {
		t.Fatalf("session event rejected: %v", err)
	}

	event["payload"].(map[string]any)["sequence"] = float64(2)
	mismatched, err := json.Marshal(fixture.Poll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePoll(mismatched); err == nil {
		t.Fatal("mismatched inner session event sequence was accepted")
	}
}

func TestWorkerProtocolRejectsDuplicateAuthorityAdvertisements(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "coop-worker-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Poll map[string]any `json:"poll"`
	}
	if err := json.Unmarshal(document, &fixture); err != nil {
		t.Fatal(err)
	}
	worker := fixture.Poll["worker"].(map[string]any)
	repositories := worker["repositories"].([]any)
	worker["repositories"] = append(repositories, repositories[0])
	duplicate, err := json.Marshal(fixture.Poll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePoll(duplicate); err == nil {
		t.Fatal("duplicate repository advertisement was accepted")
	}
}

func TestWorkerProtocolAllowsOnlyNamedSemanticAndFenceMutations(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "testdata", "protocol", "coop-worker-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Response map[string]any `json:"response"`
	}
	if err := json.Unmarshal(document, &fixture); err != nil {
		t.Fatal(err)
	}
	commands := fixture.Response["commands"].([]any)
	command := commands[0].(map[string]any)
	for _, kind := range []string{"get_session", "get_turn", "validate_candidate", "fence_operation"} {
		command["kind"] = kind
		encoded, err := json.Marshal(fixture.Response)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeResponse(encoded); err != nil {
			t.Fatalf("%s rejected: %v", kind, err)
		}
	}
}
