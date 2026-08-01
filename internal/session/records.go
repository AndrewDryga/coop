// Package session contains the durable, transport-neutral remote-session core.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	SchemaVersion = 4

	MaxIDBytes              = 256
	MaxMethodBytes          = 128
	MaxIdempotencyKeyBytes  = 256
	MaxTargetBytes          = 512
	MaxExternalRefBytes     = 512
	MaxPromptBytes          = 64 << 10
	MaxTurnArtifacts        = 4
	MaxArtifactBytes        = 8 << 20
	MaxTurnArtifactBytes    = 8 << 20
	MaxArtifactNameBytes    = 255
	MaxEventPayloadBytes    = 256 << 10
	MaxOperationResultBytes = 2 << 20
	MaxErrorDetailBytes     = 4 << 10

	MaxTurnsLimit         = 10000
	MaxQueuedTurnsLimit   = 1000
	MaxQueuedBytesLimit   = 64 << 20
	DefaultMaxTurns       = 100
	DefaultMaxQueuedTurns = 20
	DefaultMaxQueuedBytes = 1 << 20
	DefaultTurnTimeout    = time.Hour
	MaxTurnTimeout        = 24 * time.Hour
	DefaultMaxPatchBytes  = 1 << 20
	MaxPatchBytesLimit    = 1 << 20
)

type OperationState string

const (
	OperationReserved  OperationState = "reserved"
	OperationRunning   OperationState = "running"
	OperationSucceeded OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
	OperationUncertain OperationState = "uncertain"
)

type SessionState string

const (
	SessionOpen      SessionState = "open"
	SessionExhausted SessionState = "exhausted"
	SessionClosed    SessionState = "closed"
	SessionDiscarded SessionState = "discarded"
)

type ActivityState string

const (
	ActivityParked     ActivityState = "parked"
	ActivityStarting   ActivityState = "starting"
	ActivityRunning    ActivityState = "running"
	ActivityCancelling ActivityState = "cancelling"
)

type TurnState string

const (
	TurnQueued          TurnState = "queued"
	TurnStarting        TurnState = "starting"
	TurnRunning         TurnState = "running"
	TurnCompleted       TurnState = "completed"
	TurnFailed          TurnState = "failed"
	TurnCancelled       TurnState = "cancelled"
	TurnInterrupted     TurnState = "interrupted"
	TurnBudgetExhausted TurnState = "budget_exhausted"
)

type SendState string

const (
	SendStateNone   SendState = "none"
	SendStateIntent SendState = "intent"
	SendStateSent   SendState = "sent"

	TurnSendNone   = SendStateNone
	TurnSendIntent = SendStateIntent
	TurnSendSent   = SendStateSent
)

type StopReason string

const (
	StopEndTurn     StopReason = "end_turn"
	StopCancelled   StopReason = "cancelled"
	StopError       StopReason = "error"
	StopInterrupted StopReason = "interrupted"
	StopBudget      StopReason = "budget"
)

type EventType string

const (
	EventSessionCreated      EventType = "session.created"
	EventSessionStateChanged EventType = "session.state_changed"
	EventTurnQueued          EventType = "turn.queued"
	EventTurnStarted         EventType = "turn.started"
	EventActivityChanged     EventType = "activity.changed"
	EventAssistantMessage    EventType = "assistant.message"
	EventTurnCompleted       EventType = "turn.completed"
	EventTurnFailed          EventType = "turn.failed"
	EventTurnCancelled       EventType = "turn.cancelled"
	EventTurnInterrupted     EventType = "turn.interrupted"
	EventBudgetExhausted     EventType = "budget.exhausted"
	EventSessionParked       EventType = "session.parked"
	EventSessionClosed       EventType = "session.closed"
	EventWorkspaceDiscarded  EventType = "workspace.discarded"
)

type ErrorCode string

const (
	CodeInvalidRequest          ErrorCode = "invalid_request"
	CodeIdempotencyConflict     ErrorCode = "idempotency_conflict"
	CodeOperationNotFound       ErrorCode = "operation_not_found"
	CodeOperationUncertain      ErrorCode = "operation_uncertain"
	CodeOperationIntentConflict ErrorCode = "operation_intent_conflict"
	CodeSessionNotFound         ErrorCode = "session_not_found"
	CodeTurnNotFound            ErrorCode = "turn_not_found"
	CodeRevisionConflict        ErrorCode = "revision_conflict"
	CodeInvalidSessionState     ErrorCode = "invalid_session_state"
	CodeQueueFull               ErrorCode = "queue_full"
	CodeBudgetExhausted         ErrorCode = "budget_exhausted"
	CodeTurnNotRunnable         ErrorCode = "turn_not_runnable"
	CodeNativeSessionConflict   ErrorCode = "native_session_conflict"
	CodeTurnInterrupted         ErrorCode = "turn_interrupted"
	CodeEventPayloadTooLarge    ErrorCode = "event_payload_too_large"
	CodeDiscardPlanStale        ErrorCode = "discard_plan_stale"
	CodeInternal                ErrorCode = "internal_error"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Detail
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && e.Code == t.Code
}

func CodeOf(err error) ErrorCode {
	var storeErr *Error
	if errors.As(err, &storeErr) {
		return storeErr.Code
	}
	return ""
}

var (
	ErrIdempotencyConflict     = &Error{Code: CodeIdempotencyConflict}
	ErrOperationNotFound       = &Error{Code: CodeOperationNotFound}
	ErrOperationUncertain      = &Error{Code: CodeOperationUncertain}
	ErrOperationIntentConflict = &Error{Code: CodeOperationIntentConflict}
	ErrSessionNotFound         = &Error{Code: CodeSessionNotFound}
	ErrTurnNotFound            = &Error{Code: CodeTurnNotFound}
	ErrRevisionConflict        = &Error{Code: CodeRevisionConflict}
	ErrQueueFull               = &Error{Code: CodeQueueFull}
	ErrBudgetExhausted         = &Error{Code: CodeBudgetExhausted}
)

type Operation struct {
	ID             string
	Method         string
	IdempotencyKey string
	RequestHash    string
	State          OperationState
	ResourceType   string
	ResourceID     string
	Result         []byte
	ErrorCode      ErrorCode
	ErrorDetail    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Session struct {
	ID                string                `json:"id"`
	ExternalRef       string                `json:"external_ref"`
	Target            string                `json:"target"`
	Policy            string                `json:"policy"`
	PolicyDigest      string                `json:"policy_digest"`
	Repository        string                `json:"repository"`
	Workspace         string                `json:"workspace"`
	ForkName          string                `json:"fork_name"`
	BaseCommit        string                `json:"base_commit"`
	Companions        []CompanionRepository `json:"companions,omitempty"`
	NativeSessionID   string                `json:"native_session_id"`
	TurnTimeout       time.Duration         `json:"turn_timeout"`
	MaxPatchBytes     int                   `json:"max_patch_bytes"`
	Revision          int64                 `json:"revision"`
	State             SessionState          `json:"state"`
	Activity          ActivityState         `json:"activity"`
	MaxTurns          int                   `json:"max_turns"`
	MaxQueuedTurns    int                   `json:"max_queued_turns"`
	MaxQueuedBytes    int                   `json:"max_queued_bytes"`
	TurnsUsed         int                   `json:"turns_used"`
	QueuedTurnCount   int                   `json:"queued_turn_count"`
	QueuedPromptBytes int                   `json:"queued_prompt_bytes"`
	ActiveTurnID      string                `json:"active_turn_id"`
	LastEventSequence int64                 `json:"last_event_sequence"`
	CreatedAt         time.Time             `json:"created_at"`
	UpdatedAt         time.Time             `json:"updated_at"`
}

// CompanionRepository is an operator-policy-selected repository snapshot available read-only
// during a session. Repository and Workspace are durable host bindings and are omitted from the
// public HTTP DTO; BaseCommit is the immutable identity presented to clients.
type CompanionRepository struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Workspace  string `json:"workspace"`
	BaseCommit string `json:"base_commit"`
}

type Turn struct {
	ID               string           `json:"id"`
	SessionID        string           `json:"session_id"`
	Ordinal          int64            `json:"ordinal"`
	IdempotencyKey   string           `json:"idempotency_key"`
	RequestHash      string           `json:"request_hash"`
	State            TurnState        `json:"state"`
	SendState        SendState        `json:"send_state"`
	Prompt           string           `json:"prompt"`
	Artifacts        []InputArtifact  `json:"-"`
	OutputArtifacts  []OutputArtifact `json:"output_artifacts,omitempty"`
	QueuedAt         time.Time        `json:"queued_at"`
	StartedAt        time.Time        `json:"started_at"`
	FinishedAt       time.Time        `json:"finished_at"`
	StopReason       StopReason       `json:"stop_reason"`
	AssistantMessage string           `json:"assistant_message"`
	ErrorCode        ErrorCode        `json:"error_code"`
	ErrorDetail      string           `json:"error_detail"`
}

// OutputArtifact is a bounded generated file produced by one completed turn. Public turn
// responses expose metadata only; Data is available through the authenticated artifact endpoint.
type OutputArtifact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	Data      []byte `json:"-"`
}

// InputArtifact is opaque user-supplied context attached to one turn. Data is accepted only on
// turn admission and never appears in the public session API or durable operation result.
type InputArtifact struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Data      []byte `json:"data"`
}

type Event struct {
	ID         string
	SessionID  string
	Sequence   int64
	TurnID     string
	Type       EventType
	Version    int
	OccurredAt time.Time
	Payload    []byte
}

type CreateSessionRequest struct {
	ID             string                `json:"id"`
	ExternalRef    string                `json:"external_ref"`
	Target         string                `json:"target"`
	Policy         string                `json:"policy"`
	PolicyDigest   string                `json:"policy_digest"`
	Repository     string                `json:"repository"`
	Workspace      string                `json:"workspace"`
	ForkName       string                `json:"fork_name"`
	BaseCommit     string                `json:"base_commit"`
	Companions     []CompanionRepository `json:"companions,omitempty"`
	TurnTimeout    time.Duration         `json:"turn_timeout"`
	MaxPatchBytes  int                   `json:"max_patch_bytes"`
	MaxTurns       int                   `json:"max_turns"`
	MaxQueuedTurns int                   `json:"max_queued_turns"`
	MaxQueuedBytes int                   `json:"max_queued_bytes"`
}

type SubmitTurnRequest struct {
	SessionID        string
	ExpectedRevision int64
	Prompt           string
	Artifacts        []InputArtifact
}

type CancelTurnRequest struct {
	SessionID        string
	TurnID           string
	ExpectedRevision int64
}

type ExhaustBudgetRequest struct {
	SessionID        string
	ExpectedRevision int64
}

type CompleteTurnRequest struct {
	SessionID string
	TurnID    string
	Message   string
	Artifacts []OutputArtifact
}

type FailTurnRequest struct {
	SessionID   string
	TurnID      string
	ErrorCode   ErrorCode
	ErrorDetail string
}

type CloseSessionRequest struct {
	SessionID        string
	ExpectedRevision int64
}

type ExtendBudgetRequest struct {
	SessionID        string
	ExpectedRevision int64
	AdditionalTurns  int
}

type AppendEventRequest struct {
	SessionID string
	TurnID    string
	Type      EventType
	Version   int
	Payload   []byte
}

type Option func(*options)

type options struct {
	clock func() time.Time
	id    func(string) string
}

func WithClock(clock func() time.Time) Option {
	return func(o *options) {
		if clock != nil {
			o.clock = clock
		}
	}
}

func WithIDGenerator(id func(string) string) Option {
	return func(o *options) {
		if id != nil {
			o.id = id
		}
	}
}

func CanonicalRequestHash(request any) (string, error) {
	b, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("canonical request: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
