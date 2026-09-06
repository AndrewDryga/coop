package workerconnector

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/secretscan"
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
	IdempotencyKey string
	Body           []byte
}

type API interface {
	Do(context.Context, Request) (json.RawMessage, error)
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
	API               API
	ArtifactTransport ArtifactTransport
	JournalDir        string
	Now               func() time.Time
	WorkerID          string
}

type Executor struct {
	api               API
	artifactTransport ArtifactTransport
	journal           *journal
	now               func() time.Time
	workerID          string
}

func NewExecutor(config ExecutorConfig) (*Executor, error) {
	if config.API == nil || config.Now == nil || config.WorkerID == "" {
		return nil, errors.New("worker command executor configuration is incomplete")
	}
	journal, err := openJournal(config.JournalDir)
	if err != nil {
		return nil, err
	}
	return &Executor{
		api: config.API, artifactTransport: config.ArtifactTransport,
		journal: journal, now: config.Now, workerID: config.WorkerID,
	}, nil
}

func (e *Executor) Execute(ctx context.Context, command workerproto.Command) (workerproto.CommandResult, error) {
	if err := command.Validate(); err != nil {
		return workerproto.CommandResult{}, fmt.Errorf("validate worker command: %w", err)
	}
	if command.WorkerID != e.workerID {
		return workerproto.CommandResult{}, ErrWorkerMismatch
	}
	if !e.now().Before(command.LeaseExpiresAt) {
		return workerproto.CommandResult{}, ErrLeaseExpired
	}

	entry, err := e.journal.begin(command)
	if err != nil {
		return workerproto.CommandResult{}, err
	}
	if entry.State == "completed" {
		// A crash may have landed the command receipt before the activity
		// binding. Reconstruct it only from the same successful create receipt;
		// later commands must never invent a remote session identity.
		_ = e.journal.preserveCreateOrigin(entry)
		return *entry.Result, nil
	}

	if command.Kind == "submit_turn" || command.Kind == "ensure_workspace" {
		// The target-side placement fence: once this worker holds a newer generation for the
		// session ref, a still-leased command from an older one must not mutate its session — a
		// late turn or a stale restore gets a definite failure the controller stops redelivering.
		// Reads and cleanup for the old generation stay allowed; a move checkpoints the old
		// placement after the new one exists.
		if origin, err := e.journal.readCreateOrigin(e.journal.createOriginPath(command.SessionRef)); err == nil &&
			origin.PlacementGeneration > command.PlacementGeneration {
			return e.complete(entry, failureResult(command, "placement_superseded",
				fmt.Sprintf("placement generation %d was superseded by %d on this worker", command.PlacementGeneration, origin.PlacementGeneration)))
		}
	}
	if command.Kind == "get_output_artifact" {
		result := e.transferOutputArtifact(ctx, command)
		return e.complete(entry, result)
	}
	if command.Kind == "get_review_patch" {
		result := e.transferReviewPatch(ctx, command)
		return e.complete(entry, result)
	}
	if command.Kind == "checkpoint_workspace" {
		result := e.transferWorkspaceCheckpoint(ctx, command)
		return e.complete(entry, result)
	}
	if command.Kind == "ensure_workspace" {
		var payload ensureWorkspacePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return e.complete(entry, failureResult(command, "invalid_command", err.Error()))
		}
		if payload.Checkpoint != nil {
			result, err := e.restoreWorkspaceCheckpoint(ctx, command, payload)
			if err != nil {
				return workerproto.CommandResult{}, err // transient fetch: receipt stays received
			}
			return e.complete(entry, result)
		}
	}

	request, err := prepareRequest(ctx, command, e.artifactTransport)
	if errors.Is(err, errArtifactTransfer) {
		return workerproto.CommandResult{}, err // receipt stays received; redelivery retries the fetch
	}
	var status *ArtifactStatusError
	if errors.As(err, &status) {
		return e.complete(entry, failureResult(command, "artifact_transfer_failed", err.Error()))
	}
	if err != nil {
		return e.complete(entry, failureResult(command, "invalid_command", err.Error()))
	}
	resource, callErr := e.api.Do(ctx, request)
	result := resultFromCall(command, resource, callErr)
	return e.complete(entry, result)
}

func (e *Executor) complete(entry journalEntry, result workerproto.CommandResult) (workerproto.CommandResult, error) {
	if err := result.Validate(); err != nil {
		return workerproto.CommandResult{}, fmt.Errorf("validate worker command result: %w", err)
	}
	completed, err := e.journal.complete(entry, result)
	if err != nil {
		return workerproto.CommandResult{}, err
	}
	// Narration remains best effort, but its identity comes from the validated
	// create result rather than an arbitrary later command payload.
	_ = e.journal.preserveCreateOrigin(completed)
	return *completed.Result, nil
}

type createSessionPayload struct {
	ExternalRef      string            `json:"external_ref"`
	Policy           string            `json:"policy"`
	PolicyDigest     string            `json:"policy_digest"`
	AuthorityDigest  string            `json:"authority_digest,omitempty"`
	ResponderBinding *responderBinding `json:"responder_binding,omitempty"`
}

type responderBinding struct {
	Endpoint string `json:"endpoint"`
	Token    string `json:"token"`
}

type getSessionPayload struct {
	CoopSessionID string `json:"coop_session_id"`
}

type getTurnPayload struct {
	CoopSessionID string `json:"coop_session_id"`
	CoopTurnID    string `json:"coop_turn_id"`
}

type getOutputArtifactPayload struct {
	CoopSessionID string `json:"coop_session_id"`
	CoopTurnID    string `json:"coop_turn_id"`
	ArtifactRef   string `json:"artifact_ref"`
}

type getChangesPayload struct {
	CoopSessionID string `json:"coop_session_id"`
}

type getChangesPagePayload struct {
	CoopSessionID string `json:"coop_session_id"`
	PatchOffset   int    `json:"patch_offset"`
	PatchLimit    int    `json:"patch_limit"`
}

type runReviewPayload struct {
	CoopSessionID    string `json:"coop_session_id"`
	ExpectedRevision int    `json:"expected_revision"`
}

type planDiscardPayload struct {
	CoopSessionID    string `json:"coop_session_id"`
	ExpectedRevision int    `json:"expected_revision"`
	AcceptDirty      bool   `json:"accept_dirty"`
	AcceptUnmerged   bool   `json:"accept_unmerged"`
}

type discardSessionPayload struct {
	CoopSessionID   string `json:"coop_session_id"`
	PlanOperationID string `json:"plan_operation_id"`
}

type getReviewPatchPayload struct {
	CoopSessionID  string `json:"coop_session_id"`
	ArtifactID     string `json:"artifact_id"`
	ExpectedSHA256 string `json:"expected_sha256"`
	ExpectedBytes  int64  `json:"expected_bytes"`
}

type frozenSubmission struct {
	ContractVersion   string          `json:"contract_version"`
	Context           json.RawMessage `json:"context"`
	InputArtifactRefs []string        `json:"input_artifact_refs"`
	OutputSchema      json.RawMessage `json:"output_schema"`
	Prompt            string          `json:"prompt"`
}

type submitTurnPayload struct {
	CoopSessionID    string            `json:"coop_session_id"`
	ExpectedRevision int               `json:"expected_revision"`
	Submission       json.RawMessage   `json:"submission"`
	SubmissionSHA256 string            `json:"submission_sha256"`
	TurnRef          string            `json:"turn_ref"`
	ResponderBinding *responderBinding `json:"responder_binding,omitempty"`
}

type cancelTurnPayload struct {
	CoopSessionID    string `json:"coop_session_id"`
	CoopTurnID       string `json:"coop_turn_id"`
	ExpectedRevision int    `json:"expected_revision"`
}

type validateCandidatePayload struct {
	CoopSessionID    string   `json:"coop_session_id"`
	CoopTurnID       string   `json:"coop_turn_id"`
	CandidateAttempt int      `json:"candidate_attempt"`
	CandidateSHA256  string   `json:"candidate_sha256"`
	Verdict          string   `json:"verdict"`
	Violations       []string `json:"violations"`
}

type fenceOperationPayload struct {
	InputArtifactRefs []string        `json:"input_artifact_refs"`
	Method            string          `json:"method"`
	Request           json.RawMessage `json:"request"`
}

type closeSessionPayload struct {
	CoopSessionID    string `json:"coop_session_id"`
	ExpectedRevision int    `json:"expected_revision"`
}

type reconcileOperationPayload struct {
	OperationKey string `json:"operation_key"`
}

type workspaceTaskDraft struct {
	OfferRef        string   `json:"offer_ref"`
	Title           string   `json:"title"`
	Prompt          string   `json:"prompt"`
	SuccessChecks   []string `json:"success_checks"`
	AuthorityLimits []string `json:"authority_limits"`
	InstructionRef  string   `json:"instruction_ref,omitempty"`
	SourceRefs      []string `json:"source_refs"`
}

type ensureWorkspacePayload struct {
	CoopSessionID    string             `json:"coop_session_id"`
	ExpectedRevision int                `json:"expected_revision"`
	Task             workspaceTaskDraft `json:"task"`
	Checkpoint       *workspaceRestore  `json:"checkpoint,omitempty"`
}

type workspaceRestore struct {
	TransferID                string `json:"transfer_id"`
	CheckpointRef             string `json:"checkpoint_ref"`
	SHA256                    string `json:"sha256"`
	ByteSize                  int64  `json:"byte_size"`
	SourceSessionRef          string `json:"source_session_ref"`
	SourcePlacementGeneration int    `json:"source_placement_generation"`
}

type checkpointWorkspacePayload struct {
	CoopSessionID    string `json:"coop_session_id"`
	SessionRef       string `json:"session_ref"`
	ExpectedRevision int    `json:"expected_revision"`
	RepositoryRef    string `json:"repository_ref"`
}

func prepareRequest(ctx context.Context, command workerproto.Command, artifacts ArtifactTransport) (Request, error) {
	switch command.Kind {
	case "create_session":
		var payload createSessionPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.ExternalRef, 1024) || !reference(payload.Policy, 1024) || !digest(payload.PolicyDigest) ||
			(payload.AuthorityDigest != "" && !digest(payload.AuthorityDigest)) {
			return Request{}, errors.New("create_session payload identity is invalid")
		}
		// The digests the controller pinned this command to travel with the create, so the daemon
		// refuses (before any workspace exists) when its same-name policy has changed since this
		// worker was authorized — a stale hello or a later session response can never vouch for it.
		bodyDocument := map[string]any{
			"policy": payload.Policy, "task": payload.ExternalRef,
			"expected_policy_digest": payload.PolicyDigest,
		}
		if payload.AuthorityDigest != "" {
			bodyDocument["expected_authority_digest"] = payload.AuthorityDigest
		}
		if payload.ResponderBinding != nil {
			if err := validateResponderBinding(*payload.ResponderBinding); err != nil {
				return Request{}, err
			}
			bodyDocument["responder_binding"] = payload.ResponderBinding
		}
		body, _ := json.Marshal(bodyDocument)
		return Request{Method: "POST", Path: "/v1/sessions", IdempotencyKey: command.IdempotencyKey, Body: body}, nil

	case "submit_turn":
		var payload submitTurnPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || !reference(payload.TurnRef, 1024) ||
			payload.ExpectedRevision <= 0 || !digest(payload.SubmissionSHA256) {
			return Request{}, errors.New("submit_turn payload identity is invalid")
		}
		if payload.ResponderBinding != nil {
			if err := validateResponderBinding(*payload.ResponderBinding); err != nil {
				return Request{}, err
			}
		}
		submission, err := validateSubmission(payload.Submission, payload.SubmissionSHA256)
		if err != nil {
			return Request{}, err
		}
		outputSchema, err := canonicalJSON(submission.OutputSchema)
		if err != nil {
			return Request{}, errors.New("frozen output schema cannot be encoded")
		}
		outputSchemaDigest := sha256.Sum256(outputSchema)
		inputArtifacts, err := fetchInputArtifacts(ctx, artifacts, command.CommandID, submission.InputArtifactRefs)
		if err != nil {
			return Request{}, err
		}
		bodyDocument := map[string]any{
			"expected_revision": payload.ExpectedRevision,
			"artifacts":         inputArtifacts,
			"output_contract": map[string]any{
				"json_schema": json.RawMessage(outputSchema), "require_semantic_validation": true,
				"sha256": hex.EncodeToString(outputSchemaDigest[:]),
			},
			"prompt": submission.Prompt,
		}
		if payload.ResponderBinding != nil {
			bodyDocument["responder_binding"] = payload.ResponderBinding
		}
		body, _ := json.Marshal(bodyDocument)
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/turns",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "get_session":
		var payload getSessionPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) {
			return Request{}, errors.New("get_session payload identity is invalid")
		}
		return Request{Method: "GET", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID)}, nil

	case "get_turn":
		var payload getTurnPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || !reference(payload.CoopTurnID, 1024) {
			return Request{}, errors.New("get_turn payload identity is invalid")
		}
		return Request{
			Method: "GET", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/turns/" + url.PathEscape(payload.CoopTurnID),
		}, nil

	case "get_output_artifact":
		return Request{}, errors.New("output artifacts use the bounded binary transport")

	case "get_changes":
		var payload getChangesPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) {
			return Request{}, errors.New("get_changes payload identity is invalid")
		}
		query := url.Values{"patch_limit": {"1"}}
		return Request{Method: "GET", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/changes?" + query.Encode()}, nil

	case "get_changes_page":
		var payload getChangesPagePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || payload.PatchOffset < 0 || payload.PatchOffset > 1<<30 || payload.PatchLimit <= 0 || payload.PatchLimit > 512<<10 {
			return Request{}, errors.New("get_changes_page payload is invalid")
		}
		query := url.Values{
			"patch_limit":  {fmt.Sprintf("%d", payload.PatchLimit)},
			"patch_offset": {fmt.Sprintf("%d", payload.PatchOffset)},
		}
		return Request{Method: "GET", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/changes?" + query.Encode()}, nil

	case "run_review":
		var payload runReviewPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || payload.ExpectedRevision <= 0 {
			return Request{}, errors.New("run_review payload is invalid")
		}
		body, _ := json.Marshal(map[string]any{"expected_revision": payload.ExpectedRevision})
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/review",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "plan_discard":
		var payload planDiscardPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || payload.ExpectedRevision <= 0 {
			return Request{}, errors.New("plan_discard payload is invalid")
		}
		body, _ := json.Marshal(map[string]any{
			"accept_dirty": payload.AcceptDirty, "accept_unmerged": payload.AcceptUnmerged,
			"expected_revision": payload.ExpectedRevision,
		})
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/discard-plan",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "discard_session":
		var payload discardSessionPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || !reference(payload.PlanOperationID, 1024) {
			return Request{}, errors.New("discard_session payload is invalid")
		}
		body, _ := json.Marshal(map[string]any{"plan_operation_id": payload.PlanOperationID})
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/discard",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "get_review_patch":
		return Request{}, errors.New("review patches use the bounded binary transport")

	case "cancel_turn":
		var payload cancelTurnPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || !reference(payload.CoopTurnID, 1024) || payload.ExpectedRevision <= 0 {
			return Request{}, errors.New("cancel_turn payload identity is invalid")
		}
		body, _ := json.Marshal(map[string]any{"expected_revision": payload.ExpectedRevision})
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/turns/" + url.PathEscape(payload.CoopTurnID) + "/cancel",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "validate_candidate":
		var payload validateCandidatePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || !reference(payload.CoopTurnID, 1024) ||
			payload.CandidateAttempt <= 0 || !digest(payload.CandidateSHA256) || !validValidation(payload.Verdict, payload.Violations) {
			return Request{}, errors.New("validate_candidate payload is invalid")
		}
		body := map[string]any{"candidate_sha256": payload.CandidateSHA256, "verdict": payload.Verdict}
		if payload.Verdict == "reject" {
			body["violations"] = payload.Violations
		}
		encoded, _ := json.Marshal(body)
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/turns/" + url.PathEscape(payload.CoopTurnID) + "/validation",
			IdempotencyKey: command.IdempotencyKey, Body: encoded,
		}, nil

	case "fence_operation":
		var payload fenceOperationPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		request, err := expandFenceRequest(ctx, artifacts, command.CommandID, payload)
		if err != nil {
			return Request{}, err
		}
		body, _ := json.Marshal(map[string]any{"method": payload.Method, "request": request})
		return Request{
			Method: "POST", Path: "/v1/operations/fence", IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "close_session":
		var payload closeSessionPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || payload.ExpectedRevision <= 0 {
			return Request{}, errors.New("close_session payload identity is invalid")
		}
		body, _ := json.Marshal(map[string]any{"expected_revision": payload.ExpectedRevision})
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/close",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "reconcile_operation":
		var payload reconcileOperationPayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.OperationKey, 512) {
			return Request{}, errors.New("reconcile_operation key is invalid")
		}
		return Request{
			Method: "GET", Path: "/v1/operations?" + url.Values{"key": {payload.OperationKey}}.Encode(),
		}, nil

	case "ensure_workspace":
		var payload ensureWorkspacePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || payload.ExpectedRevision <= 0 ||
			!validWorkspaceTask(payload.Task) {
			return Request{}, errors.New("ensure_workspace payload is invalid")
		}
		body, _ := json.Marshal(map[string]any{
			"expected_revision": payload.ExpectedRevision,
			"task":              payload.Task,
		})
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/workspace",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	case "checkpoint_workspace":
		var payload checkpointWorkspacePayload
		if err := decodePayload(command.Payload, &payload); err != nil {
			return Request{}, err
		}
		if !reference(payload.CoopSessionID, 1024) || payload.SessionRef != command.SessionRef ||
			payload.ExpectedRevision <= 0 || !reference(payload.RepositoryRef, 256) {
			return Request{}, errors.New("checkpoint_workspace payload identity is invalid")
		}
		body, _ := json.Marshal(map[string]any{
			"expected_revision": payload.ExpectedRevision, "placement_generation": command.PlacementGeneration,
			"repository_ref": payload.RepositoryRef, "session_ref": payload.SessionRef,
		})
		return Request{
			Method: "POST", Path: "/v1/sessions/" + url.PathEscape(payload.CoopSessionID) + "/checkpoint",
			IdempotencyKey: command.IdempotencyKey, Body: body,
		}, nil

	default:
		return Request{}, errors.New("worker command kind is not executable")
	}
}

func (e *Executor) restoreWorkspaceCheckpoint(
	ctx context.Context,
	command workerproto.Command,
	payload ensureWorkspacePayload,
) (workerproto.CommandResult, error) {
	checkpointRef := payload.Checkpoint
	if !reference(payload.CoopSessionID, 1024) || payload.ExpectedRevision <= 0 ||
		!validWorkspaceTask(payload.Task) || !reference(checkpointRef.TransferID, 256) ||
		!reference(checkpointRef.CheckpointRef, 256) || !digest(checkpointRef.SHA256) ||
		checkpointRef.ByteSize <= 0 || checkpointRef.ByteSize > workerproto.MaxWorkspaceCheckpointBundleBytes ||
		!reference(checkpointRef.SourceSessionRef, 256) || checkpointRef.SourcePlacementGeneration <= 0 {
		return failureResult(command, "invalid_command", "ensure_workspace checkpoint identity is invalid"), nil
	}
	if e.artifactTransport == nil {
		return failureResult(command, "artifact_transport_unavailable", "workspace checkpoint transport is unavailable"), nil
	}
	checkpoint, bundle, err := e.artifactTransport.FetchWorkspaceCheckpoint(
		ctx, command.CommandID, checkpointRef.TransferID,
	)
	if err != nil {
		if classified := classifyArtifactFetch(err, "fetch workspace checkpoint"); errors.Is(classified, errArtifactTransfer) {
			return workerproto.CommandResult{}, classified
		}
		return failureResult(command, "artifact_transfer_failed", err.Error()), nil
	}
	if checkpoint.CheckpointRef != checkpointRef.CheckpointRef ||
		checkpoint.Bundle.SHA256 != checkpointRef.SHA256 || checkpoint.Bundle.ByteSize != checkpointRef.ByteSize ||
		checkpoint.SessionRef != checkpointRef.SourceSessionRef ||
		checkpoint.PlacementGeneration != checkpointRef.SourcePlacementGeneration {
		return failureResult(command, "artifact_identity_mismatch", "workspace checkpoint does not match the restore command"), nil
	}
	if _, err := workerproto.ValidateWorkspaceCheckpointBundle(checkpoint, bundle); err != nil {
		return failureResult(command, "artifact_identity_mismatch", err.Error()), nil
	}
	if err := rejectWorkspaceCheckpointSecrets(bundle); err != nil {
		return failureResult(command, "checkpoint_secret_detected", err.Error()), nil
	}
	api, ok := e.api.(WorkspaceRestoreAPI)
	if !ok {
		return failureResult(command, "unsupported_command", "private Coop API cannot restore workspace checkpoints"), nil
	}
	resource, callErr := api.RestoreWorkspaceCheckpoint(
		ctx, payload.CoopSessionID, command.IdempotencyKey, payload.ExpectedRevision, checkpoint, bundle,
	)
	return resultFromCall(command, resource, callErr), nil
}

func validWorkspaceTask(task workspaceTaskDraft) bool {
	return reference(task.OfferRef, 256) && reference(task.Title, 120) &&
		!strings.ContainsAny(task.Title, "\r\n") && reference(task.Prompt, 12_000) &&
		boundedUniqueTexts(task.SuccessChecks, 1, 20, 1_000) &&
		boundedUniqueTexts(task.AuthorityLimits, 0, 20, 500) &&
		(task.InstructionRef == "" || reference(task.InstructionRef, 256)) &&
		boundedUniqueTexts(task.SourceRefs, 0, 20, 256)
}

func boundedUniqueTexts(values []string, minimum, maximum, itemMaximum int) bool {
	if len(values) < minimum || len(values) > maximum {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !reference(value, itemMaximum) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validValidation(verdict string, violations []string) bool {
	if verdict == "accept" {
		return len(violations) == 0
	}
	if verdict != "reject" || len(violations) == 0 || len(violations) > 20 {
		return false
	}
	total := 0
	for _, violation := range violations {
		if strings.TrimSpace(violation) != violation || violation == "" || len(violation) > 4096 {
			return false
		}
		total += len(violation) + 1
		if total > 4096 {
			return false
		}
	}
	return true
}

func validateSubmission(raw json.RawMessage, expectedDigest string) (frozenSubmission, error) {
	var submission frozenSubmission
	if err := decodePayload(raw, &submission); err != nil {
		return frozenSubmission{}, err
	}
	if !reference(submission.ContractVersion, 128) || strings.TrimSpace(submission.Prompt) == "" ||
		len(submission.Prompt) > 256<<10 || !jsonObject(submission.Context) || !jsonObject(submission.OutputSchema) {
		return frozenSubmission{}, errors.New("frozen submission is invalid")
	}
	if len(submission.InputArtifactRefs) > 5 || !uniqueArtifactRefs(submission.InputArtifactRefs) {
		return frozenSubmission{}, errors.New("frozen input artifact references are invalid")
	}
	encoded, err := canonicalJSON(raw)
	if err != nil {
		return frozenSubmission{}, errors.New("frozen submission cannot be encoded")
	}
	sum := sha256.Sum256(encoded)
	if hex.EncodeToString(sum[:]) != expectedDigest {
		return frozenSubmission{}, errors.New("frozen submission digest does not match")
	}
	return submission, nil
}

// errArtifactTransfer marks an artifact fetch that failed for a reason the next delivery may not
// see again — a network error, a timeout, a server-side failure. Such a command must keep its
// receipt in "received" so redelivery retries the fetch; only a client status from the
// controller, which says this artifact is gone or the request is wrong, is a permanent answer.
var errArtifactTransfer = errors.New("artifact transfer failed for now")

func classifyArtifactFetch(err error, what string) error {
	var status *ArtifactStatusError
	if errors.As(err, &status) && status.Status >= 400 && status.Status < 500 {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %v", errArtifactTransfer, what, err)
}

func fetchInputArtifacts(ctx context.Context, transport ArtifactTransport, commandID string, refs []string) ([]map[string]any, error) {
	if len(refs) == 0 {
		return []map[string]any{}, nil
	}
	if transport == nil {
		return nil, errors.New("input artifact transport is not configured")
	}
	result := make([]map[string]any, 0, len(refs))
	total := 0
	for _, ref := range refs {
		artifact, err := transport.FetchInputArtifact(ctx, commandID, ref)
		if err != nil {
			return nil, classifyArtifactFetch(err, "fetch input artifact")
		}
		if artifact.ID == "" {
			artifact.ID = ref
		}
		if artifact.ID != ref || validateArtifact(artifact, true) != nil {
			return nil, errors.New("input artifact identity is invalid")
		}
		total += len(artifact.Data)
		if total > maxArtifactBytes {
			return nil, errors.New("input artifacts exceed their total bound")
		}
		result = append(result, map[string]any{
			"data": artifact.Data, "media_type": artifact.MediaType,
			"name": artifact.Name, "sha256": artifact.SHA256,
		})
	}
	return result, nil
}

func expandFenceRequest(
	ctx context.Context,
	transport ArtifactTransport,
	commandID string,
	payload fenceOperationPayload,
) (json.RawMessage, error) {
	if !jsonObject(payload.Request) || len(payload.InputArtifactRefs) > 5 ||
		!uniqueArtifactRefs(payload.InputArtifactRefs) {
		return nil, errors.New("fence_operation payload is invalid")
	}
	if payload.Method == "CreateRemoteSession" {
		if len(payload.InputArtifactRefs) != 0 {
			return nil, errors.New("fence_operation payload is invalid")
		}
		return payload.Request, nil
	}
	if payload.Method != "SubmitTurn" {
		return nil, errors.New("fence_operation payload is invalid")
	}

	var request map[string]json.RawMessage
	if err := json.Unmarshal(payload.Request, &request); err != nil || request == nil {
		return nil, errors.New("fence_operation payload is invalid")
	}
	if _, inline := request["artifacts"]; inline {
		return nil, errors.New("fence_operation input artifacts must use authenticated references")
	}
	inputArtifacts, err := fetchInputArtifacts(ctx, transport, commandID, payload.InputArtifactRefs)
	if err != nil {
		return nil, err
	}
	encodedArtifacts, _ := json.Marshal(inputArtifacts)
	request["artifacts"] = encodedArtifacts
	return json.Marshal(request)
}

func uniqueArtifactRefs(refs []string) bool {
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !reference(ref, 256) || seen[ref] {
			return false
		}
		seen[ref] = true
	}
	return true
}

func (e *Executor) transferOutputArtifact(ctx context.Context, command workerproto.Command) workerproto.CommandResult {
	var payload getOutputArtifactPayload
	if err := decodePayload(command.Payload, &payload); err != nil ||
		!reference(payload.CoopSessionID, 1024) || !reference(payload.CoopTurnID, 1024) || !reference(payload.ArtifactRef, 256) {
		return failureResult(command, "invalid_command", "get_output_artifact payload is invalid")
	}
	api, ok := e.api.(OutputArtifactAPI)
	if !ok || e.artifactTransport == nil {
		return failureResult(command, "artifact_transport_unavailable", "output artifact transport is not configured")
	}
	artifact, err := api.FetchOutputArtifact(ctx, payload.CoopSessionID, payload.CoopTurnID, payload.ArtifactRef)
	if err != nil {
		return resultFromCall(command, nil, err)
	}
	if artifact.ID != payload.ArtifactRef || validateArtifact(artifact, true) != nil || !outputArtifactMediaType(artifact.MediaType) {
		return failureResult(command, "invalid_output_artifact", "output artifact identity is invalid")
	}
	resource, err := e.artifactTransport.UploadOutputArtifact(ctx, command.CommandID, artifact)
	return resultFromCall(command, resource, err)
}

func (e *Executor) transferReviewPatch(ctx context.Context, command workerproto.Command) workerproto.CommandResult {
	var payload getReviewPatchPayload
	if err := decodePayload(command.Payload, &payload); err != nil ||
		!reference(payload.CoopSessionID, 1024) || !reference(payload.ArtifactID, 256) ||
		!digest(payload.ExpectedSHA256) || payload.ExpectedBytes <= 0 || payload.ExpectedBytes > maxReviewPatchBytes {
		return failureResult(command, "invalid_command", "get_review_patch payload is invalid")
	}
	api, ok := e.api.(ReviewPatchAPI)
	if !ok || e.artifactTransport == nil {
		return failureResult(command, "artifact_transport_unavailable", "review patch transport is not configured")
	}
	patch, err := api.FetchReviewPatch(ctx, payload.ArtifactID, payload.ExpectedSHA256, payload.ExpectedBytes)
	if err != nil {
		return resultFromCall(command, nil, err)
	}
	resource, err := e.artifactTransport.UploadReviewPatch(
		ctx, command.CommandID, payload.ArtifactID, payload.ExpectedSHA256, patch,
	)
	return resultFromCall(command, resource, err)
}

func (e *Executor) transferWorkspaceCheckpoint(ctx context.Context, command workerproto.Command) workerproto.CommandResult {
	request, err := prepareRequest(ctx, command, e.artifactTransport)
	if err != nil {
		return failureResult(command, "invalid_command", err.Error())
	}
	api, ok := e.api.(WorkspaceCheckpointAPI)
	if !ok || e.artifactTransport == nil {
		return failureResult(command, "artifact_transport_unavailable", "workspace checkpoint transport is not configured")
	}
	resource, err := e.api.Do(ctx, request)
	if err != nil {
		return resultFromCall(command, nil, err)
	}
	operationID, checkpoint, err := decodeWorkspaceCheckpointResponse(resource, command)
	if err != nil {
		return uncertainResult(command, "transport_uncertain", err.Error())
	}
	bundle, err := api.FetchWorkspaceCheckpointBundle(ctx, operationID, checkpoint)
	if err != nil {
		return resultFromCall(command, nil, err)
	}
	if _, err := workerproto.ValidateWorkspaceCheckpointBundle(checkpoint, bundle); err != nil {
		return failureResult(command, "invalid_checkpoint", err.Error())
	}
	if err := rejectWorkspaceCheckpointSecrets(bundle); err != nil {
		return failureResult(command, "checkpoint_secret_detected", err.Error())
	}
	uploaded, err := e.artifactTransport.UploadWorkspaceCheckpoint(ctx, command.CommandID, checkpoint, bundle)
	return resultFromCall(command, uploaded, err)
}

func rejectWorkspaceCheckpointSecrets(bundle []byte) error {
	reader := tar.NewReader(bytes.NewReader(bundle))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read workspace checkpoint for secret scan: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(reader, workerproto.MaxWorkspaceCheckpointBundleBytes+1))
		if err != nil || len(body) > workerproto.MaxWorkspaceCheckpointBundleBytes {
			return errors.New("workspace checkpoint member exceeds the secret-scan bound")
		}
		if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
			continue
		}
		if findings := secretscan.ScanSecrets(string(body)); len(findings) > 0 {
			return fmt.Errorf(
				"workspace checkpoint member %q contains a likely %s on line %d",
				header.Name, findings[0].Kind, findings[0].Line,
			)
		}
	}
}

func decodeWorkspaceCheckpointResponse(
	resource json.RawMessage,
	command workerproto.Command,
) (string, workerproto.WorkspaceCheckpoint, error) {
	var response struct {
		Operation struct {
			ID           string `json:"id"`
			Method       string `json:"method"`
			State        string `json:"state"`
			ResourceType string `json:"resource_type"`
			ResourceID   string `json:"resource_id"`
		} `json:"operation"`
		Checkpoint json.RawMessage `json:"checkpoint"`
	}
	if err := json.Unmarshal(resource, &response); err != nil {
		return "", workerproto.WorkspaceCheckpoint{}, errors.New("private Coop checkpoint response is invalid")
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(response.Checkpoint)
	if err != nil || response.Operation.Method != "CheckpointWorkspace" || response.Operation.State != "succeeded" ||
		response.Operation.ResourceType != "workspace_checkpoint" ||
		!reference(response.Operation.ID, 256) || response.Operation.ResourceID != checkpoint.CheckpointRef ||
		checkpoint.SessionRef != command.SessionRef || checkpoint.PlacementGeneration != command.PlacementGeneration {
		return "", workerproto.WorkspaceCheckpoint{}, errors.New("private Coop checkpoint identity is invalid")
	}
	var payload checkpointWorkspacePayload
	if decodePayload(command.Payload, &payload) != nil || checkpoint.RepositoryRef != payload.RepositoryRef {
		return "", workerproto.WorkspaceCheckpoint{}, errors.New("private Coop checkpoint authority does not match")
	}
	return response.Operation.ID, checkpoint, nil
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

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("JSON has trailing data")
	}
	return json.Marshal(value)
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

func validateResponderBinding(binding responderBinding) error {
	if len(binding.Endpoint) == 0 || len(binding.Endpoint) > 2048 ||
		len(binding.Token) < 32 || len(binding.Token) > 256 {
		return errors.New("create_session Responder binding is invalid")
	}
	for _, char := range binding.Token {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' {
			return errors.New("create_session Responder binding is invalid")
		}
	}
	endpoint, err := url.Parse(binding.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/v1/state-tools/mcp" {
		return errors.New("create_session Responder binding is invalid")
	}
	return nil
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
