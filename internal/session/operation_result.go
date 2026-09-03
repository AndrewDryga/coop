package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const turnOperationResultVersion = 1

// turnOperationResult is the exact public turn snapshot needed for an
// idempotent mutation replay. Execution inputs and authority stay only in the
// canonical turn row: operation receipts must not duplicate prompts, request
// hashes, schemas, runtime IDs, responder tokens, or artifact contents.
type turnOperationResult struct {
	ReceiptVersion            int              `json:"receipt_version"`
	ID                        string           `json:"id"`
	SessionID                 string           `json:"session_id"`
	Ordinal                   int64            `json:"ordinal"`
	State                     TurnState        `json:"state"`
	SendState                 SendState        `json:"send_state"`
	AssistantMessage          string           `json:"assistant_message,omitempty"`
	StopReason                StopReason       `json:"stop_reason,omitempty"`
	ErrorCode                 ErrorCode        `json:"error_code,omitempty"`
	ErrorDetail               string           `json:"error_detail,omitempty"`
	QueuedAt                  time.Time        `json:"queued_at"`
	StartedAt                 time.Time        `json:"started_at,omitempty"`
	FinishedAt                time.Time        `json:"finished_at,omitempty"`
	OutputArtifacts           []OutputArtifact `json:"output_artifacts,omitempty"`
	Usage                     Usage            `json:"usage,omitzero"`
	Candidate                 *TurnCandidate   `json:"candidate,omitempty"`
	ValidationCandidateSHA256 string           `json:"validation_candidate_sha256,omitempty"`
	ValidationAttempt         int              `json:"validation_attempt,omitempty"`
	ValidationError           string           `json:"validation_error,omitempty"`
	ValidationReceipt         string           `json:"validation_receipt,omitempty"`
	ResponderBindingDigest    string           `json:"responder_binding_digest,omitempty"`
}

// EncodeTurnOperationResult records only the immutable public result of one
// turn mutation. The returned shape is deliberately independent of Turn's
// private persistence fields.
func EncodeTurnOperationResult(turn Turn) ([]byte, error) {
	if turn.ID == "" {
		return nil, errors.New("encode turn operation result: missing turn id")
	}
	return json.Marshal(turnOperationResultFromTurn(turn))
}

// DecodeTurnOperationResult reads both current compact receipts and the old
// root-level Turn JSON. Keeping the legacy decoder lets an upgraded daemon
// replay operations before an operator chooses to compact its database.
func DecodeTurnOperationResult(data []byte) (Turn, error) {
	turn, _, err := decodeTurnOperationResult(data)
	return turn, err
}

func decodeTurnOperationResult(data []byte) (Turn, bool, error) {
	var header struct {
		ReceiptVersion json.RawMessage `json:"receipt_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Turn{}, false, fmt.Errorf("decode turn operation result: %w", err)
	}
	if len(header.ReceiptVersion) == 0 {
		var legacy Turn
		if err := json.Unmarshal(data, &legacy); err != nil {
			return Turn{}, false, fmt.Errorf("decode legacy turn operation result: %w", err)
		}
		if legacy.ID == "" {
			return Turn{}, false, errors.New("decode turn operation result: missing turn id")
		}
		return legacy, true, nil
	}
	var version int
	if err := json.Unmarshal(header.ReceiptVersion, &version); err != nil {
		return Turn{}, false, errors.New("decode turn operation result: invalid receipt version")
	}
	if version != turnOperationResultVersion {
		return Turn{}, false, fmt.Errorf("decode turn operation result: unsupported receipt version %d", version)
	}
	var result turnOperationResult
	if err := json.Unmarshal(data, &result); err != nil {
		return Turn{}, false, fmt.Errorf("decode turn operation result: %w", err)
	}
	if result.ID == "" {
		return Turn{}, false, errors.New("decode turn operation result: missing turn id")
	}
	return result.turn(), false, nil
}

func turnOperationResultFromTurn(turn Turn) turnOperationResult {
	digest := turn.ResponderBindingDigest
	if digest == "" {
		digest = ResponderBindingDigest(turn.ResponderBinding)
	}
	artifacts := make([]OutputArtifact, 0, len(turn.OutputArtifacts))
	for _, artifact := range turn.OutputArtifacts {
		artifact.Data = nil
		artifacts = append(artifacts, artifact)
	}
	return turnOperationResult{
		ReceiptVersion: turnOperationResultVersion,
		ID:             turn.ID, SessionID: turn.SessionID, Ordinal: turn.Ordinal,
		State: turn.State, SendState: turn.SendState,
		AssistantMessage: turn.AssistantMessage, StopReason: turn.StopReason,
		ErrorCode: turn.ErrorCode, ErrorDetail: PublicErrorDetail(turn.ErrorCode, turn.ErrorDetail),
		QueuedAt: turn.QueuedAt, StartedAt: turn.StartedAt, FinishedAt: turn.FinishedAt,
		OutputArtifacts: artifacts, Usage: turn.Usage,
		Candidate:                 cloneTurnCandidate(turn.Candidate),
		ValidationCandidateSHA256: turn.CandidateSHA256,
		ValidationAttempt:         turn.ValidationAttempt, ValidationError: turn.ValidationError,
		ValidationReceipt: turn.ValidationReceipt, ResponderBindingDigest: digest,
	}
}

func (result turnOperationResult) turn() Turn {
	return Turn{
		ID: result.ID, SessionID: result.SessionID, Ordinal: result.Ordinal,
		State: result.State, SendState: result.SendState,
		AssistantMessage: result.AssistantMessage, StopReason: result.StopReason,
		ErrorCode: result.ErrorCode, ErrorDetail: result.ErrorDetail,
		QueuedAt: result.QueuedAt, StartedAt: result.StartedAt, FinishedAt: result.FinishedAt,
		OutputArtifacts: append([]OutputArtifact(nil), result.OutputArtifacts...), Usage: result.Usage,
		Candidate: cloneTurnCandidate(result.Candidate), CandidateSHA256: result.ValidationCandidateSHA256,
		ValidationAttempt: result.ValidationAttempt, ValidationError: result.ValidationError,
		ValidationReceipt: result.ValidationReceipt, ResponderBindingDigest: result.ResponderBindingDigest,
	}
}

func cloneTurnCandidate(value *TurnCandidate) *TurnCandidate {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
