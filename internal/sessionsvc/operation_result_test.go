package sessionsvc

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
)

func TestCompactTurnResultReplaysTheExactPublicProjection(t *testing.T) {
	turn := session.Turn{
		ID: "turn-1", SessionID: "session-1", Ordinal: 2,
		State: session.TurnAwaitingValidation, SendState: session.SendStateSent,
		Prompt: "never persist this twice", QueuedAt: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC),
		ErrorCode: session.CodeSessionCleanupError, ErrorDetail: "runtime cleanup failed: /secret/runtime/path",
		AssistantMessage: "answer", Candidate: &session.TurnCandidate{Message: "candidate", SHA256: "candidate-sha", Attempt: 1},
		CandidateSHA256: "candidate-sha", ValidationAttempt: 1,
		OutputArtifacts:  []session.OutputArtifact{{ID: "artifact", Name: "file.txt", MediaType: "text/plain", SHA256: "artifact-sha", Bytes: 3}},
		ResponderBinding: &session.ResponderBinding{Endpoint: "https://responder.example/mcp", Token: strings.Repeat("t", 48)},
	}
	want := publicTurn(turn)
	result, err := session.EncodeTurnOperationResult(turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "/secret/runtime/path") {
		t.Fatalf("compact receipt retained private cleanup detail: %s", result)
	}
	replayed, err := session.DecodeTurnOperationResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if got := publicTurn(replayed); !reflect.DeepEqual(got, want) {
		t.Fatalf("public replay changed:\n got  %+v\n want %+v", got, want)
	}
}

func TestPublicTurnHidesHistoricalCandidateAfterValidation(t *testing.T) {
	legacy := session.Turn{
		ID: "accepted-turn", State: session.TurnCompleted, AssistantMessage: "accepted",
		Candidate:       &session.TurnCandidate{Message: "accepted", SHA256: "candidate-sha", Attempt: 1},
		CandidateSHA256: "candidate-sha", ValidationAttempt: 1,
		ValidationReceipt: "validation-receipt",
	}
	got := publicTurn(legacy)
	if got.Candidate != nil || got.ValidationCandidateSHA256 != legacy.CandidateSHA256 ||
		got.ValidationReceipt != legacy.ValidationReceipt {
		t.Fatalf("completed historical turn projection = %+v", got)
	}
}

func TestCompactTurnResultOmitsCandidateAfterValidation(t *testing.T) {
	turn := session.Turn{
		ID: "accepted-turn", State: session.TurnCompleted, AssistantMessage: "accepted",
		Candidate:       &session.TurnCandidate{Message: "accepted", SHA256: "candidate-sha", Attempt: 1},
		CandidateSHA256: "candidate-sha", ValidationAttempt: 1,
		ValidationReceipt: "validation-receipt",
	}
	encoded, err := session.EncodeTurnOperationResult(turn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"candidate"`) {
		t.Fatalf("accepted turn receipt retained candidate: %s", encoded)
	}
}
