package workerconnector

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/workerproto"
)

var (
	ErrLeaseExpired   = errors.New("worker command placement lease has expired")
	ErrWorkerMismatch = errors.New("worker command belongs to another worker")
)

const maxErrorDetailBytes = 4096

type Request struct {
	Method         string
	Path           string
	Headers        map[string]string
	IdempotencyKey string
	Body           []byte
	BodyBytes      int64
}

type API interface {
	Do(context.Context, Request) (json.RawMessage, error)
}
type streamingAPI interface {
	Forward(context.Context, Request, io.Reader) (*http.Response, error)
}
type APIError struct {
	Status int
	Code   string
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("private Coop API returned %d %s: %s", e.Status, e.Code, e.Detail)
}

type ExecutorConfig struct {
	API             API
	BodyTransport   BodyTransport
	JobSourceStager JobSourceStager
	JournalDir      string
	Now             func() time.Time
	WorkerID        string
}
type Executor struct {
	api             API
	bodyTransport   BodyTransport
	jobSourceStager JobSourceStager
	journal         *journal
	now             func() time.Time
	workerID        string
}

func NewExecutor(config ExecutorConfig) (*Executor, error) {
	if config.API == nil || config.Now == nil || config.WorkerID == "" {
		return nil, errors.New("worker command executor configuration is incomplete")
	}
	journal, err := openJournal(config.JournalDir)
	if err != nil {
		return nil, err
	}
	return &Executor{api: config.API, bodyTransport: config.BodyTransport, jobSourceStager: config.JobSourceStager, journal: journal, now: config.Now, workerID: config.WorkerID}, nil
}

func (e *Executor) Execute(ctx context.Context, command workerproto.Command) (workerproto.CommandResult, error) {
	return e.execute(ctx, command, func() time.Time { return command.LeaseExpiresAt })
}

func (e *Executor) execute(ctx context.Context, command workerproto.Command, expiresAt func() time.Time) (workerproto.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return workerproto.CommandResult{}, err
	}
	if err := command.Validate(); err != nil {
		return workerproto.CommandResult{}, fmt.Errorf("validate worker command: %w", err)
	}
	if command.WorkerID != e.workerID {
		return workerproto.CommandResult{}, ErrWorkerMismatch
	}
	if !e.now().Before(expiresAt()) {
		return workerproto.CommandResult{}, ErrLeaseExpired
	}
	entry, err := e.journal.begin(command)
	if err != nil {
		return workerproto.CommandResult{}, err
	}
	if entry.State == "completed" {
		_ = e.journal.preserveCreateOrigin(entry)
		return *entry.Result, nil
	}
	request, err := workerproto.DecodeAPIRequest(command.Payload)
	if err != nil {
		return e.complete(entry, failureResult(command, "invalid_command", err.Error()))
	}
	if entry.Response == nil {
		if err := e.checkPlacement(command, request); err != nil {
			if errors.Is(err, ErrRequestRejected) {
				return e.complete(entry, failureResult(command, "placement_superseded", err.Error()))
			}
			return workerproto.CommandResult{}, err
		}
	}
	response, err := e.forwardRequest(ctx, entry, command, request, expiresAt)
	if errors.Is(err, errArtifactTransfer) || errors.Is(err, ErrLeaseExpired) {
		return workerproto.CommandResult{}, err
	}
	return e.complete(entry, resultFromCall(command, response, err))
}

func (e *Executor) checkPlacement(command workerproto.Command, request workerproto.APIRequest) error {
	if !needsPlacementFence(request) {
		return nil
	}
	origin, err := e.journal.readCreateOrigin(e.journal.createOriginPath(command.SessionRef))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: read placement authority: %v", errArtifactTransfer, err)
	}
	if origin.PlacementGeneration > command.PlacementGeneration {
		return fmt.Errorf("%w: a newer placement owns this session", ErrRequestRejected)
	}
	return nil
}

func needsPlacementFence(request workerproto.APIRequest) bool {
	if request.Method == "GET" || request.Method == "HEAD" {
		return false
	}
	parsed, err := url.ParseRequestURI(request.Path)
	if err != nil || request.Method != "POST" {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/v1/sessions/"), "/")
	if strings.HasPrefix(parsed.Path, "/v1/sessions/") && len(parts) == 2 {
		switch parts[1] {
		case "close", "discard", "discard-plan", "checkpoint":
			return false
		}
	}
	if strings.HasPrefix(parsed.Path, "/v1/sessions/") && len(parts) == 4 && parts[1] == "turns" && parts[3] == "cancel" {
		return false
	}
	return true
}

func isCreateRequest(command workerproto.Command) bool {
	request, err := workerproto.DecodeAPIRequest(command.Payload)
	if err != nil || request.Method != "POST" {
		return false
	}
	parsed, err := url.ParseRequestURI(request.Path)
	return err == nil && parsed.Path == "/v1/sessions"
}

func (e *Executor) complete(entry journalEntry, result workerproto.CommandResult) (workerproto.CommandResult, error) {
	if err := result.Validate(); err != nil {
		return workerproto.CommandResult{}, fmt.Errorf("validate worker command result: %w", err)
	}
	completed, err := e.journal.complete(entry, result)
	if err != nil {
		return workerproto.CommandResult{}, err
	}
	_ = e.journal.preserveCreateOrigin(completed)
	return *completed.Result, nil
}

var errArtifactTransfer = errors.New("body transfer failed for now")

func classifyArtifactFetch(err error, what string) error {
	var status *BodyStatusError
	if errors.As(err, &status) && status.Status >= 400 && status.Status < 500 && status.Status != 408 && status.Status != 429 {
		return fmt.Errorf("%w: %s: %v", ErrRequestRejected, what, err)
	}
	return fmt.Errorf("%w: %s: %v", errArtifactTransfer, what, err)
}

func resultFromCall(command workerproto.Command, resource json.RawMessage, err error) workerproto.CommandResult {
	if err == nil && jsonObject(resource) {
		return workerproto.CommandResult{
			CommandID: command.CommandID, State: "succeeded", OperationKey: command.IdempotencyKey,
			Resource: resource, Error: json.RawMessage("null"),
		}
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiFailureResult(command, apiErr.Status, apiErr.Code, apiErr.Detail)
	}
	if errors.Is(err, ErrRequestRejected) {
		return failureResult(command, "invalid_command", err.Error()) // nothing was sent: not uncertain
	}
	detail := "private Coop API response was not proven"
	if err != nil {
		detail = err.Error()
	}
	return uncertainResult(command, "transport_uncertain", detail)
}

func failureResult(command workerproto.Command, code, detail string) workerproto.CommandResult {
	return errorResult(command, "failed", code, detail)
}

func uncertainResult(command workerproto.Command, code, detail string) workerproto.CommandResult {
	return errorResult(command, "uncertain", code, detail)
}

func apiFailureResult(command workerproto.Command, status int, code, detail string) workerproto.CommandResult {
	errorBody, _ := json.Marshal(map[string]any{"status": status, "code": bounded(code), "detail": bounded(detail)})
	return workerproto.CommandResult{
		CommandID: command.CommandID, State: "failed", OperationKey: command.IdempotencyKey,
		Resource: json.RawMessage("null"), Error: errorBody,
	}
}

func errorResult(command workerproto.Command, state, code, detail string) workerproto.CommandResult {
	errorBody, _ := json.Marshal(map[string]any{"code": bounded(code), "detail": bounded(detail)})
	return workerproto.CommandResult{
		CommandID: command.CommandID, State: state, OperationKey: command.IdempotencyKey,
		Resource: json.RawMessage("null"), Error: errorBody,
	}
}

func decodePayload(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode worker command payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("worker command payload has trailing data")
	}
	return nil
}

func jsonObject(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var object map[string]any
	return json.Unmarshal(raw, &object) == nil && object != nil
}

func reference(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
		return false
	}
	return true
}

func digest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func bounded(value string) string {
	if len(value) <= maxErrorDetailBytes {
		return value
	}
	return value[:maxErrorDetailBytes]
}
