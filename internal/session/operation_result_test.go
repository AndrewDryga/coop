package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTurnOperationResultContainsOnlyPublicReplayData(t *testing.T) {
	now := time.Date(2026, 9, 3, 11, 12, 13, 14, time.UTC)
	turn := Turn{
		ID: "turn-1", SessionID: "session-1", Ordinal: 7,
		IdempotencyKey: "private-idempotency", RequestHash: "private-request-hash",
		State: TurnAwaitingValidation, SendState: SendStateSent,
		Prompt: "private-prompt", QueuedAt: now, StartedAt: now.Add(time.Second),
		FinishedAt: now.Add(2 * time.Second), StopReason: StopEndTurn,
		AssistantMessage: "public-answer", ErrorCode: CodeOutputContractFailed,
		ErrorDetail: "public-error", Usage: Usage{InputTokens: 4, OutputTokens: 2},
		MinTargetIndex: 2, RewindTarget: true,
		OutputContract:  &OutputContract{JSONSchema: json.RawMessage(`{"type":"object"}`), SHA256: "private-schema"},
		OutputArtifacts: []OutputArtifact{{ID: "artifact-1", Name: "chart.png", MediaType: "image/png", SHA256: "public-artifact-digest", Bytes: 4, Data: []byte("data")}},
		Candidate:       &TurnCandidate{Message: "public-candidate", SHA256: "public-candidate-digest", Attempt: 2},
		CandidateSHA256: "public-candidate-digest", ValidationAttempt: 2,
		ValidationError: "public-validation-error", ValidationReceipt: "public-validation-receipt",
		RuntimeRunID: "private-runtime", ControllerTools: &ControllerTools{
			Endpoint: "https://private.example/mcp", Token: strings.Repeat("s", 48),
		},
	}

	encoded, err := EncodeTurnOperationResult(turn)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{
		"private-prompt", "private-idempotency", "private-request-hash", "private-schema",
		"private-runtime", "https://private.example/mcp", strings.Repeat("s", 48),
		`"prompt"`, `"idempotency_key"`, `"request_hash"`, `"output_contract"`,
		`"min_target_index"`, `"rewind_target"`, `"runtime_run_id"`, `"data"`,
	} {
		if strings.Contains(string(encoded), private) {
			t.Errorf("compact result contains private value/key %q: %s", private, encoded)
		}
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if got := int(document["receipt_version"].(float64)); got != turnOperationResultVersion {
		t.Fatalf("receipt version = %d, want %d", got, turnOperationResultVersion)
	}

	replayed, err := DecodeTurnOperationResult(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := turnOperationResultFromTurn(replayed), turnOperationResultFromTurn(turn); !reflect.DeepEqual(got, want) {
		t.Fatalf("public replay changed:\n got  %+v\n want %+v", got, want)
	}
	if replayed.Prompt != "" || replayed.IdempotencyKey != "" || replayed.RequestHash != "" ||
		replayed.OutputContract != nil || replayed.RuntimeRunID != "" || replayed.ControllerTools != nil {
		t.Fatalf("compact replay restored private fields: %+v", replayed)
	}
	if replayed.ControllerToolsDigest != ControllerToolsDigest(turn.ControllerTools) {
		t.Fatalf("responder digest = %q, want %q", replayed.ControllerToolsDigest, ControllerToolsDigest(turn.ControllerTools))
	}
}

func TestHistoricalControllerToolsReceiptsKeepTheirDigest(t *testing.T) {
	digest := strings.Repeat("b", 64)
	data := []byte(`{"receipt_version":1,"id":"old","responder_binding_digest":"` + digest + `"}`)
	sess, err := DecodeSessionOperationResult(data)
	if err != nil || sess.ControllerToolsDigest != digest || sess.ControllerTools != nil {
		t.Fatalf("historical session receipt: %+v, %v", sess, err)
	}
	turn, err := DecodeTurnOperationResult(data)
	if err != nil || turn.ControllerToolsDigest != digest || turn.ControllerTools != nil {
		t.Fatalf("historical turn receipt: %+v, %v", turn, err)
	}
	encoded, err := EncodeTurnOperationResult(turn)
	if err != nil || strings.Contains(string(encoded), "responder_binding") || !strings.Contains(string(encoded), digest) {
		t.Fatalf("new receipt must use only the neutral name: %s, %v", encoded, err)
	}
	conflict := []byte(`{"receipt_version":1,"id":"old","responder_binding_digest":"old","controller_tools_digest":"different"}`)
	if _, err := DecodeSessionOperationResult(conflict); err == nil {
		t.Fatal("conflicting session receipt digests accepted")
	}
	if _, err := DecodeTurnOperationResult(conflict); err == nil {
		t.Fatal("conflicting turn receipt digests accepted")
	}
}

func TestTurnOperationResultReadsLegacyFullTurnJSON(t *testing.T) {
	legacy := Turn{
		ID: "legacy-turn", SessionID: "session-1", Ordinal: 3,
		IdempotencyKey: "legacy-key", RequestHash: "legacy-hash",
		State: TurnQueued, SendState: SendStateNone, Prompt: "legacy prompt",
		QueuedAt: time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC),
	}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	replayed, old, err := decodeTurnOperationResult(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !old || !reflect.DeepEqual(replayed, legacy) {
		t.Fatalf("legacy replay = %+v old=%v, want %+v old=true", replayed, old, legacy)
	}
}

func TestTurnOperationResultOmitsCandidateAfterValidation(t *testing.T) {
	turn := Turn{
		ID: "accepted-turn", State: TurnCompleted, AssistantMessage: "accepted",
		Candidate:       &TurnCandidate{Message: "accepted", SHA256: "candidate-digest", Attempt: 1},
		CandidateSHA256: "candidate-digest", ValidationAttempt: 1,
		ValidationReceipt: "validation-receipt",
	}
	encoded, err := EncodeTurnOperationResult(turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"candidate"`) {
		t.Fatalf("accepted turn receipt retained candidate: %s", encoded)
	}
	replayed, err := DecodeTurnOperationResult(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Candidate != nil || replayed.CandidateSHA256 != turn.CandidateSHA256 ||
		replayed.ValidationReceipt != turn.ValidationReceipt {
		t.Fatalf("accepted turn replay = %+v", replayed)
	}
}

func TestTurnOperationResultRejectsUnknownOrEmptyReceipts(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"receipt_version":99,"id":"turn-1"}`),
		[]byte(`{"receipt_version":"one","id":"turn-1"}`),
	} {
		if _, err := DecodeTurnOperationResult(data); err == nil {
			t.Fatalf("DecodeTurnOperationResult(%s) unexpectedly succeeded", data)
		}
	}
	if _, err := EncodeTurnOperationResult(Turn{}); err == nil {
		t.Fatal("EncodeTurnOperationResult accepted an empty turn")
	}
}

func TestTurnOperationResultDoesNotRetainPrivateErrorDetails(t *testing.T) {
	for _, test := range []struct {
		code ErrorCode
		want string
	}{
		{CodeInternal, "internal operation failure"},
		{CodeDiscardPlanStale, "discard plan no longer matches workspace state"},
		{CodeSessionCleanupError, "session runtime cleanup is temporarily unavailable"},
	} {
		encoded, err := EncodeTurnOperationResult(Turn{
			ID: "turn-1", ErrorCode: test.code, ErrorDetail: "/private/path/provider.log",
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "/private/path") || !strings.Contains(string(encoded), test.want) {
			t.Fatalf("%s receipt = %s", test.code, encoded)
		}
		replayed, err := DecodeTurnOperationResult(encoded)
		if err != nil || replayed.ErrorDetail != test.want {
			t.Fatalf("%s replay = %+v err=%v", test.code, replayed, err)
		}
	}
}
