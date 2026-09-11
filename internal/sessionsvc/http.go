package sessionsvc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	sessionHTTPMaxBody      = 128 << 10
	sessionHTTPTurnMaxBody  = 12 << 20
	sessionHTTPFenceMaxBody = sessionHTTPTurnMaxBody + sessionHTTPMaxBody
	sessionHTTPDefaultMax   = 100
	sessionHTTPMaxList      = 1000
	// sessionEventPageBytes caps the payload bytes one event page may carry.
	// Chosen well under the 3 MiB a client reasonably allows for a whole
	// response, since the DTO envelope and JSON escaping both add to it.
	sessionEventPageBytes = 1 << 20
)

// These DTOs are the public v1 wire types. They deliberately do not mirror the durable records:
// the latter contain prompts, operation secrets, native provider identities, and host paths.
type SessionDTO struct {
	ID                        string                               `json:"id"`
	ExternalRef               string                               `json:"external_ref"`
	Target                    string                               `json:"target"`
	Policy                    string                               `json:"policy"`
	PolicyDigest              string                               `json:"policy_digest"`
	AuthorityDigest           string                               `json:"authority_digest"`
	ProjectEnv                bool                                 `json:"project_env"`
	ProjectMCP                bool                                 `json:"project_mcp"`
	ResponderBindingDigest    string                               `json:"responder_binding_digest,omitempty"`
	WorkspaceTask             *SessionWorkspaceTaskDTO             `json:"workspace_task,omitempty"`
	Mode                      string                               `json:"mode"`
	RepositoryReadOnly        bool                                 `json:"repository_read_only"`
	BaseCommit                string                               `json:"base_commit"`
	RepositoryFreshnessStatus string                               `json:"repository_freshness_status"`
	RepositoryFreshness       []session.RepositoryFreshnessReceipt `json:"repository_freshness"`
	PullRequest               *session.PullRequestBinding          `json:"pull_request,omitempty"`
	Companions                []SessionCompanionDTO                `json:"companions,omitempty"`
	Network                   SessionNetworkSummaryDTO             `json:"network"`
	ForkName                  string                               `json:"fork_name"`
	Revision                  int64                                `json:"revision"`
	State                     session.SessionState                 `json:"state"`
	Activity                  session.ActivityState                `json:"activity"`
	MaxTurns                  int                                  `json:"max_turns"`
	MaxQueuedTurns            int                                  `json:"max_queued_turns"`
	MaxQueuedBytes            int                                  `json:"max_queued_bytes"`
	TurnsUsed                 int                                  `json:"turns_used"`
	QueuedTurnCount           int                                  `json:"queued_turn_count"`
	QueuedPromptBytes         int                                  `json:"queued_prompt_bytes"`
	ActiveTurnID              string                               `json:"active_turn_id,omitempty"`
	LastEventSequence         int64                                `json:"last_event_sequence"`
	CreatedAt                 time.Time                            `json:"created_at"`
	UpdatedAt                 time.Time                            `json:"updated_at"`
}

type SessionWorkspaceTaskDTO struct {
	QueueID     string `json:"queue_id"`
	TaskID      string `json:"task_id"`
	ID          string `json:"id"`
	OfferRef    string `json:"offer_ref"`
	DraftSHA256 string `json:"draft_sha256"`
}

type SessionCompanionDTO struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	BaseCommit string `json:"base_commit"`
}

// SessionNetworkSummaryDTO is the two facts every session view carries: the posture its boxes run
// under, and — for a filtered one — the fingerprint of the exact policy they enforce. The live
// view and the receipt live behind their own routes; this stays small enough to appear in a list.
type SessionNetworkSummaryDTO struct {
	Mode        string `json:"mode"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type TurnDTO struct {
	ID                        string                 `json:"id"`
	SessionID                 string                 `json:"session_id"`
	Ordinal                   int64                  `json:"ordinal"`
	State                     session.TurnState      `json:"state"`
	SendState                 session.SendState      `json:"send_state"`
	AssistantMessage          string                 `json:"assistant_message,omitempty"`
	StopReason                session.StopReason     `json:"stop_reason,omitempty"`
	ErrorCode                 session.ErrorCode      `json:"error_code,omitempty"`
	ErrorDetail               string                 `json:"error_detail,omitempty"`
	QueuedAt                  time.Time              `json:"queued_at"`
	StartedAt                 time.Time              `json:"started_at,omitempty"`
	FinishedAt                time.Time              `json:"finished_at,omitempty"`
	OutputArtifacts           []TurnArtifactDTO      `json:"output_artifacts,omitempty"`
	Usage                     session.Usage          `json:"usage,omitzero"`
	Candidate                 *session.TurnCandidate `json:"candidate,omitempty"`
	ValidationCandidateSHA256 string                 `json:"validation_candidate_sha256,omitempty"`
	ValidationAttempt         int                    `json:"validation_attempt,omitempty"`
	ValidationError           string                 `json:"validation_error,omitempty"`
	ValidationReceipt         string                 `json:"validation_receipt,omitempty"`
	ResponderBindingDigest    string                 `json:"responder_binding_digest,omitempty"`
}

type TurnArtifactDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
}

type EventDTO struct {
	ID         string            `json:"id"`
	SessionID  string            `json:"session_id"`
	Sequence   int64             `json:"sequence"`
	TurnID     string            `json:"turn_id,omitempty"`
	Type       session.EventType `json:"type"`
	Version    int               `json:"version"`
	OccurredAt time.Time         `json:"occurred_at"`
	// Payload is the event's own record of what happened. It was withheld for
	// a long time, which left a caller able to count that a turn failed but
	// not to say why, and able to see that a turn ran without seeing any of
	// the work inside it.
	Payload json.RawMessage `json:"payload,omitempty"`
}

type OperationDTO struct {
	ID           string                 `json:"id"`
	Method       string                 `json:"method"`
	State        session.OperationState `json:"state"`
	ResourceType string                 `json:"resource_type,omitempty"`
	ResourceID   string                 `json:"resource_id,omitempty"`
	ErrorCode    session.ErrorCode      `json:"error_code,omitempty"`
	ErrorDetail  string                 `json:"error_detail,omitempty"`
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time              `json:"updated_at"`
}

type operationFenceEnvelope struct {
	Method  string          `json:"method"`
	Request json.RawMessage `json:"request"`
}

type operationFenceSubmitTurnRequest struct {
	SessionID        string                    `json:"session_id"`
	ExpectedRevision int64                     `json:"expected_revision"`
	Prompt           string                    `json:"prompt"`
	Artifacts        []session.InputArtifact   `json:"artifacts,omitempty"`
	MinTargetIndex   int                       `json:"min_target_index,omitempty"`
	RewindTarget     bool                      `json:"rewind_target,omitempty"`
	OutputContract   *session.OutputContract   `json:"output_contract,omitempty"`
	ResponderBinding *session.ResponderBinding `json:"responder_binding,omitempty"`
}

type SessionChangeDTO struct {
	Path         string `json:"path,omitempty"`
	PathBytes    []byte `json:"path_bytes,omitempty"`
	OldPath      string `json:"old_path,omitempty"`
	OldPathBytes []byte `json:"old_path_bytes,omitempty"`
	Status       string `json:"status"`
}

type SessionParentDivergenceDTO struct {
	Ahead        int  `json:"ahead"`
	Behind       int  `json:"behind"`
	BaseToFork   int  `json:"base_to_fork"`
	BaseToParent int  `json:"base_to_parent"`
	Diverged     bool `json:"diverged"`
}

type SessionChangesDTO struct {
	BaseCommit       string                     `json:"base_commit"`
	ForkHead         string                     `json:"fork_head"`
	ForkTree         string                     `json:"fork_tree"`
	PullRequestTree  string                     `json:"pull_request_tree,omitempty"`
	ParentHead       string                     `json:"parent_head"`
	Committed        []SessionChangeDTO         `json:"committed"`
	Staged           []SessionChangeDTO         `json:"staged"`
	Unstaged         []SessionChangeDTO         `json:"unstaged"`
	Untracked        []SessionChangeDTO         `json:"untracked"`
	Conflicts        []SessionChangeDTO         `json:"conflicts"`
	ParentDivergence SessionParentDivergenceDTO `json:"parent_divergence"`
	Patch            []byte                     `json:"patch,omitempty"`
	Truncated        bool                       `json:"truncated"`
	PatchDigest      string                     `json:"patch_digest,omitempty"`
	PatchBytes       int64                      `json:"patch_bytes"`
	PatchOffset      int64                      `json:"patch_offset"`
	PatchNextOffset  int64                      `json:"patch_next_offset"`
	PatchHasMore     bool                       `json:"patch_has_more"`
}

type SessionReviewDTO struct {
	OperationID           string                      `json:"operation_id"`
	SessionID             string                      `json:"session_id"`
	SessionRevision       int64                       `json:"session_revision"`
	PolicyDigest          string                      `json:"policy_digest"`
	PullRequest           *session.PullRequestBinding `json:"pull_request,omitempty"`
	CreationBase          string                      `json:"creation_base"`
	SourceHead            string                      `json:"source_head"`
	SourceTree            string                      `json:"source_tree"`
	ParentHead            string                      `json:"parent_head"`
	ParentTree            string                      `json:"parent_tree"`
	CandidateHead         string                      `json:"candidate_head"`
	CandidateTree         string                      `json:"candidate_tree"`
	Rebase                ReviewRebaseStatus          `json:"rebase"`
	Gate                  ReviewGateStatus            `json:"gate"`
	GateError             string                      `json:"gate_error,omitempty"`
	PolicyFindings        []string                    `json:"policy_findings"`
	Patch                 []byte                      `json:"patch,omitempty"`
	PatchTruncated        bool                        `json:"patch_truncated"`
	PatchArtifactID       string                      `json:"patch_artifact_id,omitempty"`
	PatchDigest           string                      `json:"patch_digest,omitempty"`
	PatchBytes            int64                       `json:"patch_bytes"`
	Publishable           bool                        `json:"publishable"`
	NotPublishableReasons []string                    `json:"not_publishable_reasons"`
}

type SessionDiscardWorkspaceDTO struct {
	Branch           string `json:"branch"`
	Head             string `json:"head"`
	StatusDigest     string `json:"status_digest"`
	Dirty            bool   `json:"dirty"`
	Unmerged         bool   `json:"unmerged"`
	Running          bool   `json:"running"`
	AcceptedDirty    bool   `json:"accepted_dirty,omitempty"`
	AcceptedUnmerged bool   `json:"accepted_unmerged,omitempty"`
}

type SessionDiscardPlanDTO struct {
	SessionID string                     `json:"session_id"`
	Revision  int64                      `json:"revision"`
	Workspace SessionDiscardWorkspaceDTO `json:"workspace"`
}

type SessionPlanDiscardDTO struct {
	OperationID string                `json:"operation_id"`
	Plan        SessionDiscardPlanDTO `json:"plan"`
}

type sessionMutationSessionResponse struct {
	Operation OperationDTO `json:"operation"`
	Session   SessionDTO   `json:"session"`
}

type sessionMutationCheckpointResponse struct {
	Operation  OperationDTO                    `json:"operation"`
	Checkpoint workerproto.WorkspaceCheckpoint `json:"checkpoint"`
}

type sessionAsyncOperationResponse struct {
	Operation OperationDTO `json:"operation"`
}

type sessionMutationTurnResponse struct {
	Operation OperationDTO `json:"operation"`
	Turn      TurnDTO      `json:"turn"`
}

type sessionMutationReviewResponse struct {
	Operation OperationDTO     `json:"operation"`
	Review    SessionReviewDTO `json:"review"`
}

type sessionMutationPlanResponse struct {
	Operation OperationDTO          `json:"operation"`
	Plan      SessionPlanDiscardDTO `json:"plan"`
}

type sessionHealthDTO struct {
	Healthy bool `json:"healthy"`
}

type sessionReadyDTO struct {
	Ready bool `json:"ready"`
}

type sessionCapabilitiesDTO struct {
	RepositoryFreshnessReceiptVersions []int `json:"repository_freshness_receipt_versions"`
	// Policies is each served policy's network reach as this daemon resolved it: the mode, and for
	// a filtered policy the fingerprint a create may pin. It is the published half of the network
	// fence — a caller cannot compute it, because host approval feeds it.
	Policies map[string]PolicyNetwork `json:"policies,omitempty"`
}

type sessionHTTPErrorBody struct {
	Code        string `json:"code"`
	Detail      string `json:"detail,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
}

type sessionHTTPErrorResponse struct {
	Error sessionHTTPErrorBody `json:"error"`
}

type sessionHTTPHandler struct {
	service *Service
	ready   bool
}

func NewHTTPHandler(service *Service) http.Handler {
	return &sessionHTTPHandler{service: service, ready: service != nil}
}

func (h *sessionHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeSessionHTTPError(w, http.StatusInternalServerError, "internal_error", "session service is unavailable")
		return
	}
	switch r.URL.Path {
	case "/healthz":
		if !sessionHTTPMethod(w, r, http.MethodGet) {
			return
		}
		if !sessionQueryOnly(w, r) {
			return
		}
		writeSessionJSON(w, http.StatusOK, sessionHealthDTO{Healthy: true})
		return
	case "/readyz":
		if !sessionHTTPMethod(w, r, http.MethodGet) {
			return
		}
		if !h.ready {
			writeSessionHTTPError(w, http.StatusServiceUnavailable, "not_ready", "session service is not ready")
			return
		}
		if !sessionQueryOnly(w, r) {
			return
		}
		writeSessionJSON(w, http.StatusOK, sessionReadyDTO{Ready: true})
		return
	case "/v1/capabilities":
		if !sessionHTTPMethod(w, r, http.MethodGet) {
			return
		}
		if !sessionQueryOnly(w, r) {
			return
		}
		writeSessionJSON(w, http.StatusOK, sessionCapabilitiesDTO{
			RepositoryFreshnessReceiptVersions: []int{2},
			Policies:                           h.service.PolicyNetworks(),
		})
		return
	}
	if r.URL.Path == "/v1/sessions" || strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		h.serveSessionPath(w, r)
		return
	}
	if r.URL.Path == "/v1/operations/fence" {
		h.fenceOperation(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/operations/") {
		h.serveOperationPath(w, r)
		return
	}
	if r.URL.Path == "/v1/operations" {
		h.getOperationByKey(w, r)
		return
	}
	writeSessionHTTPError(w, http.StatusNotFound, "not_found", "resource not found")
}

func (h *sessionHTTPHandler) fenceOperation(w http.ResponseWriter, r *http.Request) {
	if !h.requirePost(w, r) {
		return
	}
	var envelope operationFenceEnvelope
	if !decodeSessionJSONLimit(w, r, &envelope, sessionHTTPFenceMaxBody) {
		return
	}
	// Only a request that cannot be decoded is the caller's fault. A fence that decoded but failed
	// inside the service (a store or transaction error) is reported as the internal failure it is,
	// so a controller revoking authority keeps retrying instead of concluding its request was wrong.
	var (
		op  session.Operation
		err error
	)
	switch envelope.Method {
	case "CreateRemoteSession":
		var request CreateRemoteSessionRequest
		if err = decodeSessionJSONValue(envelope.Request, &request); err != nil {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "fenced request is invalid")
			return
		}
		op, err = h.service.FenceCreateRemoteSession(r.Context(), sessionIdempotencyKey(r), request)
	case "SubmitTurn":
		var request operationFenceSubmitTurnRequest
		if err = decodeSessionJSONValue(envelope.Request, &request); err != nil {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "fenced request is invalid")
			return
		}
		op, err = h.service.FenceSubmitTurn(
			r.Context(), sessionIdempotencyKey(r), session.SubmitTurnRequest{
				SessionID: request.SessionID, ExpectedRevision: request.ExpectedRevision,
				Prompt: request.Prompt, Artifacts: request.Artifacts,
				MinTargetIndex: request.MinTargetIndex, RewindTarget: request.RewindTarget,
				OutputContract: request.OutputContract, ResponderBinding: request.ResponderBinding,
			},
		)
	default:
		err = &session.Error{Code: session.CodeInvalidRequest, Detail: "operation method cannot be fenced"}
	}
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, publicOperation(op))
}

func (h *sessionHTTPHandler) getOperationByKey(w http.ResponseWriter, r *http.Request) {
	if !sessionHTTPMethod(w, r, http.MethodGet) || !sessionQueryOnly(w, r, "key") {
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" || len(key) > session.MaxIdempotencyKeyBytes || !utf8SessionText(key) {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid idempotency key")
		return
	}
	op, err := h.service.GetOperation(r.Context(), key)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, publicOperation(op))
}

func (h *sessionHTTPHandler) serveSessionPath(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/sessions" {
		switch r.Method {
		case http.MethodGet:
			h.listSessions(w, r)
		case http.MethodPost:
			h.createSession(w, r)
		default:
			sessionHTTPMethod(w, r, http.MethodGet, http.MethodPost)
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/")
	if len(parts) == 0 || parts[0] == "" || strings.Contains(r.URL.Path, "//") {
		writeSessionHTTPError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	sessionID := parts[0]
	if !validSessionHTTPPathID(sessionID) {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid session id")
		return
	}
	switch {
	case len(parts) == 1:
		if !sessionHTTPMethod(w, r, http.MethodGet) {
			return
		}
		h.getSession(w, r, sessionID)
	case len(parts) == 2 && parts[1] == "turns":
		switch r.Method {
		case http.MethodGet:
			h.listTurns(w, r, sessionID)
		case http.MethodPost:
			h.submitTurn(w, r, sessionID)
		default:
			sessionHTTPMethod(w, r, http.MethodGet, http.MethodPost)
		}
	case len(parts) == 2 && parts[1] == "events":
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.listEvents(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "changes":
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.getChanges(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "network":
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.getNetwork(w, r, sessionID)
		}
	case len(parts) == 3 && parts[1] == "network" && parts[2] == "receipt":
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.getNetworkReceipt(w, r, sessionID)
		}
	case len(parts) == 3 && parts[1] == "network" && parts[2] == "connections":
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.getNetworkConnections(w, r, sessionID)
		}
	case len(parts) == 4 && parts[1] == "network" && parts[2] == "explanations" && parts[3] != "":
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.getNetworkExplanation(w, r, sessionID, parts[3])
		}
	case len(parts) == 2 && parts[1] == "budget":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.extendBudget(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "prepare":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.prepareSession(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "workspace":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.ensureWorkspaceTask(w, r, sessionID)
		}
	case len(parts) == 3 && parts[1] == "workspace" && parts[2] == "restore":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.restoreWorkspaceCheckpoint(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "checkpoint":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.checkpointWorkspace(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "review":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.review(w, r, sessionID)
		}
	case len(parts) == 3 && parts[1] == "reviews" && validSessionHTTPPathID(parts[2]):
		if sessionHTTPMethod(w, r, http.MethodGet) && sessionQueryOnly(w, r) {
			h.getReview(w, r, sessionID, parts[2])
		}
	case len(parts) == 2 && parts[1] == "close":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.closeSession(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "discard-plan":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.planDiscard(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "discard":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.discard(w, r, sessionID)
		}
	case len(parts) == 3 && parts[1] == "turns" && parts[2] != "":
		turnID := parts[2]
		if !validSessionHTTPPathID(turnID) {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid turn id")
			return
		}
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.getTurn(w, r, sessionID, turnID)
		}
	case len(parts) == 5 && parts[1] == "turns" && parts[3] == "artifacts" && parts[2] != "" && parts[4] != "":
		turnID, artifactID := parts[2], parts[4]
		if !validSessionHTTPPathID(turnID) || !validSessionHTTPPathID(artifactID) {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid artifact identity")
			return
		}
		if sessionHTTPMethod(w, r, http.MethodGet) {
			h.getTurnArtifact(w, r, sessionID, turnID, artifactID)
		}
	case len(parts) == 4 && parts[1] == "turns" && parts[3] == "cancel" && parts[2] != "":
		turnID := parts[2]
		if !validSessionHTTPPathID(turnID) {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid turn id")
			return
		}
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.cancelTurn(w, r, sessionID, turnID)
		}
	case len(parts) == 4 && parts[1] == "turns" && parts[3] == "validation" && parts[2] != "":
		turnID := parts[2]
		if !validSessionHTTPPathID(turnID) {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid turn id")
			return
		}
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.validateTurnCandidate(w, r, sessionID, turnID)
		}
	default:
		writeSessionHTTPError(w, http.StatusNotFound, "not_found", "resource not found")
	}
}

func (h *sessionHTTPHandler) checkpointWorkspace(w http.ResponseWriter, r *http.Request, sessionID string) {
	var body struct {
		SessionRef          string `json:"session_ref"`
		ExpectedRevision    int64  `json:"expected_revision"`
		PlacementGeneration int    `json:"placement_generation"`
		RepositoryRef       string `json:"repository_ref"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	result, err := h.service.CheckpointWorkspace(
		r.Context(), sessionIdempotencyKey(r),
		CheckpointWorkspaceRequest{
			SessionID: sessionID, SessionRef: body.SessionRef,
			ExpectedRevision:    body.ExpectedRevision,
			PlacementGeneration: body.PlacementGeneration, RepositoryRef: body.RepositoryRef,
		},
	)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationCheckpointResponse{
		Operation: publicOperation(op), Checkpoint: result.Checkpoint,
	})
}

func (h *sessionHTTPHandler) restoreWorkspaceCheckpoint(w http.ResponseWriter, r *http.Request, sessionID string) {
	key := sessionIdempotencyKey(r)
	if key == "" {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "idempotency key is required")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != workerproto.WorkspaceCheckpointBundleMediaType {
		writeSessionHTTPError(w, http.StatusUnsupportedMediaType, "invalid_request", "workspace checkpoint media type is invalid")
		return
	}
	revision, err := strconv.ParseInt(r.Header.Get("X-Coop-Expected-Revision"), 10, 64)
	if err != nil || revision <= 0 {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "workspace checkpoint revision is invalid")
		return
	}
	descriptor, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Coop-Workspace-Checkpoint"))
	if err != nil || len(descriptor) == 0 || len(descriptor) > sessionHTTPMaxBody {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "workspace checkpoint descriptor is invalid")
		return
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(descriptor)
	if err != nil {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "workspace checkpoint descriptor is invalid")
		return
	}
	if r.ContentLength <= 0 || r.ContentLength > workerproto.MaxWorkspaceCheckpointBundleBytes {
		writeSessionHTTPError(w, http.StatusRequestEntityTooLarge, "invalid_request", "workspace checkpoint bundle length is invalid")
		return
	}
	bundle, err := io.ReadAll(io.LimitReader(r.Body, workerproto.MaxWorkspaceCheckpointBundleBytes+1))
	if err != nil || int64(len(bundle)) != r.ContentLength || len(bundle) > workerproto.MaxWorkspaceCheckpointBundleBytes {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "workspace checkpoint bundle is invalid")
		return
	}
	sess, err := h.service.RestoreWorkspaceCheckpoint(r.Context(), key, RestoreWorkspaceCheckpointRequest{
		SessionID: sessionID, ExpectedRevision: revision, Checkpoint: checkpoint, Bundle: bundle,
	})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, key)
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationSessionResponse{
		Operation: publicOperation(op), Session: publicSession(sess),
	})
}

func (h *sessionHTTPHandler) ensureWorkspaceTask(w http.ResponseWriter, r *http.Request, sessionID string) {
	var body struct {
		ExpectedRevision int64                     `json:"expected_revision"`
		Task             tasks.ControllerTaskDraft `json:"task"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	sess, err := h.service.EnsureWorkspaceTask(
		r.Context(), sessionIdempotencyKey(r),
		EnsureWorkspaceTaskRequest{SessionID: sessionID, ExpectedRevision: body.ExpectedRevision, Task: body.Task},
	)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationSessionResponse{
		Operation: publicOperation(op), Session: publicSession(sess),
	})
}

func (h *sessionHTTPHandler) serveOperationPath(w http.ResponseWriter, r *http.Request) {
	if !sessionHTTPMethod(w, r, http.MethodGet) {
		return
	}
	if !sessionQueryOnly(w, r) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/operations/"), "/")
	if len(parts) == 2 && parts[1] == "review-patch" &&
		validSessionHTTPPathID(parts[0]) {
		h.reviewPatch(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "checkpoint-bundle" &&
		validSessionHTTPPathID(parts[0]) {
		h.workspaceCheckpointBundle(w, r, parts[0])
		return
	}
	if len(parts) != 1 || !validSessionHTTPPathID(parts[0]) {
		writeSessionHTTPError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	}
	op, err := h.service.GetOperationByID(r.Context(), parts[0])
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, publicOperation(op))
}

func (h *sessionHTTPHandler) workspaceCheckpointBundle(
	w http.ResponseWriter,
	r *http.Request,
	operationID string,
) {
	bundle, err := h.service.OpenWorkspaceCheckpointBundle(r.Context(), operationID)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", workerproto.WorkspaceCheckpointBundleMediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(bundle)))
	w.Header().Set("ETag", `"`+checkpointSHA256(bundle)+`"`)
	w.Header().Set("Content-Disposition", `attachment; filename="workspace-checkpoint.tar"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bundle)
}

func (h *sessionHTTPHandler) reviewPatch(
	w http.ResponseWriter,
	r *http.Request,
	operationID string,
) {
	file, dossier, err := h.service.OpenReviewPatch(r.Context(), operationID)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(dossier.PatchBytes, 10))
	w.Header().Set("ETag", `"`+dossier.PatchDigest+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func (h *sessionHTTPHandler) createSession(w http.ResponseWriter, r *http.Request) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		Policy                     string                    `json:"policy"`
		Task                       string                    `json:"task"`
		PullRequest                *RemotePullRequestBinding `json:"pull_request,omitempty"`
		ResponderBinding           *session.ResponderBinding `json:"responder_binding,omitempty"`
		ExpectedPolicyDigest       string                    `json:"expected_policy_digest,omitempty"`
		ExpectedAuthorityDigest    string                    `json:"expected_authority_digest,omitempty"`
		ExpectedNetworkFingerprint string                    `json:"expected_network_fingerprint,omitempty"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	request := CreateRemoteSessionRequest{
		Policy: body.Policy, Task: body.Task, PullRequest: body.PullRequest,
		ResponderBinding:     body.ResponderBinding,
		ExpectedPolicyDigest: body.ExpectedPolicyDigest, ExpectedAuthorityDigest: body.ExpectedAuthorityDigest,
		ExpectedNetworkFingerprint: body.ExpectedNetworkFingerprint,
	}
	if sessionPreferAsync(r) {
		op, err := h.service.CreateRemoteSessionAsync(
			r.Context(), sessionIdempotencyKey(r), request,
		)
		if err != nil {
			writeSessionServiceError(w, err)
			return
		}
		writeSessionJSON(w, http.StatusAccepted, sessionAsyncOperationResponse{
			Operation: publicOperation(op),
		})
		return
	}
	sess, err := h.service.CreateRemoteSession(r.Context(), sessionIdempotencyKey(r), request)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationSessionResponse{Operation: publicOperation(op), Session: publicSession(sess)})
}

func sessionPreferAsync(r *http.Request) bool {
	for _, value := range r.Header.Values("Prefer") {
		for _, preference := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(preference), "respond-async") {
				return true
			}
		}
	}
	return false
}

func (h *sessionHTTPHandler) listSessions(w http.ResponseWriter, r *http.Request) {
	limit, ok := sessionListQuery(w, r)
	if !ok {
		return
	}
	sessions, err := h.service.ListSessions(r.Context(), limit)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	result := make([]SessionDTO, 0, len(sessions))
	for _, sess := range sessions {
		result = append(result, publicSession(sess))
	}
	writeSessionJSON(w, http.StatusOK, result)
}

func (h *sessionHTTPHandler) getSession(w http.ResponseWriter, r *http.Request, id string) {
	if !sessionQueryOnly(w, r) {
		return
	}
	if !sessionHTTPMethod(w, r, http.MethodGet) {
		return
	}
	sess, err := h.service.GetSession(r.Context(), id)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, publicSession(sess))
}

// getNetwork and getNetworkReceipt are READS. They project retained evidence and never probe a
// gateway, so a denial storm cannot make them slow, and they cannot grant, approve, or widen
// anything — the session API has no path to network authority at all, by design.
func (h *sessionHTTPHandler) getNetwork(w http.ResponseWriter, r *http.Request, id string) {
	if !sessionQueryOnly(w, r) {
		return
	}
	view, err := h.service.SessionNetwork(r.Context(), id)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, view)
}

func (h *sessionHTTPHandler) getNetworkConnections(w http.ResponseWriter, r *http.Request, id string) {
	if !sessionQueryOnly(w, r) {
		return
	}
	view, err := h.service.SessionNetworkConnections(r.Context(), id)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, view)
}

func (h *sessionHTTPHandler) getNetworkExplanation(w http.ResponseWriter, r *http.Request, id, eventID string) {
	if !sessionQueryOnly(w, r) {
		return
	}
	if !validSessionHTTPPathID(eventID) {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid event id")
		return
	}
	view, err := h.service.SessionNetworkExplanation(r.Context(), id, eventID)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, view)
}

func (h *sessionHTTPHandler) getNetworkReceipt(w http.ResponseWriter, r *http.Request, id string) {
	if !sessionQueryOnly(w, r) {
		return
	}
	receipt, err := h.service.SessionNetworkReceipt(r.Context(), id)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, receipt)
}

func (h *sessionHTTPHandler) submitTurn(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64                     `json:"expected_revision"`
		Prompt           string                    `json:"prompt"`
		Artifacts        []session.InputArtifact   `json:"artifacts,omitempty"`
		MinTargetIndex   int                       `json:"min_target_index,omitempty"`
		RewindTarget     bool                      `json:"rewind_target,omitempty"`
		OutputContract   *session.OutputContract   `json:"output_contract,omitempty"`
		ResponderBinding *session.ResponderBinding `json:"responder_binding,omitempty"`
	}
	if !decodeSessionJSONLimit(w, r, &body, sessionHTTPTurnMaxBody) {
		return
	}
	turn, err := h.service.SubmitTurn(r.Context(), sessionIdempotencyKey(r), session.SubmitTurnRequest{
		SessionID: sessionID, ExpectedRevision: body.ExpectedRevision, Prompt: body.Prompt,
		Artifacts: body.Artifacts, MinTargetIndex: body.MinTargetIndex,
		RewindTarget: body.RewindTarget, OutputContract: body.OutputContract,
		ResponderBinding: body.ResponderBinding,
	})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationTurnResponse{Operation: publicOperation(op), Turn: publicTurn(turn)})
}

func (h *sessionHTTPHandler) prepareSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	sess, err := h.service.PrepareSession(r.Context(), sessionID, body.ExpectedRevision)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, publicSession(sess))
}

func (h *sessionHTTPHandler) listTurns(w http.ResponseWriter, r *http.Request, sessionID string) {
	after, limit, ok := sessionCursorQuery(w, r)
	if !ok {
		return
	}
	turns, err := h.service.ListTurns(r.Context(), sessionID, after, limit)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	result := make([]TurnDTO, 0, len(turns))
	for _, turn := range turns {
		result = append(result, publicTurn(turn))
	}
	writeSessionJSON(w, http.StatusOK, result)
}

func (h *sessionHTTPHandler) getTurn(w http.ResponseWriter, r *http.Request, sessionID, turnID string) {
	if !sessionQueryOnly(w, r) {
		return
	}
	turn, err := h.service.GetTurn(r.Context(), sessionID, turnID)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, publicTurn(turn))
}

func (h *sessionHTTPHandler) validateTurnCandidate(w http.ResponseWriter, r *http.Request, sessionID, turnID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		CandidateSHA256 string   `json:"candidate_sha256"`
		Verdict         string   `json:"verdict"`
		Violations      []string `json:"violations,omitempty"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	var (
		turn session.Turn
		err  error
	)
	switch body.Verdict {
	case "accept":
		if len(body.Violations) != 0 {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "accepted candidates cannot carry violations")
			return
		}
		turn, err = h.service.AcceptTurnCandidate(r.Context(), sessionIdempotencyKey(r), sessionID, turnID, body.CandidateSHA256)
	case "reject":
		turn, err = h.service.RejectTurnCandidate(r.Context(), sessionIdempotencyKey(r), session.RejectTurnCandidateRequest{
			SessionID: sessionID, TurnID: turnID,
			CandidateSHA256: body.CandidateSHA256, Violations: body.Violations,
		})
	default:
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "validation verdict must be accept or reject")
		return
	}
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationTurnResponse{Operation: publicOperation(op), Turn: publicTurn(turn)})
}

func (h *sessionHTTPHandler) getTurnArtifact(w http.ResponseWriter, r *http.Request, sessionID, turnID, artifactID string) {
	if !sessionQueryOnly(w, r) {
		return
	}
	artifact, err := h.service.GetOutputArtifact(r.Context(), sessionID, turnID, artifactID)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", artifact.MediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(artifact.Bytes, 10))
	w.Header().Set("ETag", `"`+artifact.SHA256+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.Name}))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(artifact.Data)
}

func (h *sessionHTTPHandler) listEvents(w http.ResponseWriter, r *http.Request, sessionID string) {
	after, limit, ok := sessionCursorQuery(w, r)
	if !ok {
		return
	}
	events, err := h.service.ListEvents(r.Context(), sessionID, after, limit)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	result := make([]EventDTO, 0, len(events))
	// Payloads are what make an event readable rather than merely countable,
	// but they are individually bounded at 256 KiB and a page holds up to a
	// thousand of them, so a page has to be bounded by bytes as well as by
	// count. Returning a short page is correct: the caller pages by sequence
	// and the next request resumes exactly where this one stopped. Always
	// return at least one event, or a single large payload would wedge the
	// cursor forever.
	budget := sessionEventPageBytes
	for _, event := range events {
		if len(result) > 0 && budget-len(event.Payload) < 0 {
			break
		}
		budget -= len(event.Payload)
		result = append(result, publicEvent(event))
	}
	writeSessionJSON(w, http.StatusOK, result)
}

func (h *sessionHTTPHandler) cancelTurn(w http.ResponseWriter, r *http.Request, sessionID, turnID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	turn, err := h.service.CancelTurn(r.Context(), sessionIdempotencyKey(r), session.CancelTurnRequest{
		SessionID: sessionID, TurnID: turnID, ExpectedRevision: body.ExpectedRevision,
	})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationTurnResponse{Operation: publicOperation(op), Turn: publicTurn(turn)})
}

func (h *sessionHTTPHandler) extendBudget(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
		AdditionalTurns  int   `json:"additional_turns"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	sess, err := h.service.ExtendBudget(r.Context(), sessionIdempotencyKey(r), session.ExtendBudgetRequest{
		SessionID: sessionID, ExpectedRevision: body.ExpectedRevision, AdditionalTurns: body.AdditionalTurns,
	})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationSessionResponse{Operation: publicOperation(op), Session: publicSession(sess)})
}

func (h *sessionHTTPHandler) getChanges(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !sessionQueryOnly(w, r, "patch_offset", "patch_limit") {
		return
	}
	query := r.URL.Query()
	patchOffset := int64(0)
	if raw := query.Get("patch_offset"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 || parsed > 1<<30 {
			writeSessionHTTPError(
				w,
				http.StatusBadRequest,
				"invalid_request",
				"patch_offset is outside bounds",
			)
			return
		}
		patchOffset = parsed
	}
	patchLimit := 0
	if raw := query.Get("patch_limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > session.MaxPatchBytesLimit {
			writeSessionHTTPError(
				w,
				http.StatusBadRequest,
				"invalid_request",
				"patch_limit is outside bounds",
			)
			return
		}
		patchLimit = parsed
	}
	var changes WorkspaceChanges
	var err error
	if patchLimit == 0 && patchOffset == 0 {
		changes, err = h.service.GetChanges(r.Context(), sessionID)
	} else if patchLimit == 0 {
		writeSessionHTTPError(
			w,
			http.StatusBadRequest,
			"invalid_request",
			"patch_limit is required when patch_offset is set",
		)
		return
	} else {
		changes, err = h.service.GetChangesPage(
			r.Context(),
			sessionID,
			patchOffset,
			patchLimit,
		)
	}
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, publicChanges(changes))
}

func (h *sessionHTTPHandler) getReview(w http.ResponseWriter, r *http.Request, sessionID, operationID string) {
	op, dossier, err := h.service.GetReview(r.Context(), sessionID, operationID)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationReviewResponse{Operation: publicOperation(op), Review: publicReview(dossier)})
}

func (h *sessionHTTPHandler) review(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	dossier, err := h.service.RunReview(r.Context(), sessionIdempotencyKey(r), RunReviewRequest{SessionID: sessionID, ExpectedRevision: body.ExpectedRevision})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationReviewResponse{Operation: publicOperation(op), Review: publicReview(dossier)})
}

func (h *sessionHTTPHandler) closeSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	sess, err := h.service.Close(r.Context(), sessionIdempotencyKey(r), session.CloseSessionRequest{SessionID: sessionID, ExpectedRevision: body.ExpectedRevision})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationSessionResponse{Operation: publicOperation(op), Session: publicSession(sess)})
}

func (h *sessionHTTPHandler) planDiscard(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
		AcceptDirty      bool  `json:"accept_dirty"`
		AcceptUnmerged   bool  `json:"accept_unmerged"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	planned, err := h.service.PlanDiscard(r.Context(), sessionIdempotencyKey(r), PlanDiscardRequest{
		SessionID: sessionID, ExpectedRevision: body.ExpectedRevision, AcceptDirty: body.AcceptDirty, AcceptUnmerged: body.AcceptUnmerged,
	})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	publicPlan := publicDiscardPlan(planned)
	writeSessionJSON(w, http.StatusOK, sessionMutationPlanResponse{Operation: publicOperation(op), Plan: publicPlan})
}

func (h *sessionHTTPHandler) discard(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		PlanOperationID   string `json:"plan_operation_id"`
		RetireQuarantined bool   `json:"retire_quarantined"`
		ExpectedRevision  int64  `json:"expected_revision"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	if body.RetireQuarantined {
		if body.PlanOperationID != "" || body.ExpectedRevision <= 0 {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "retire_quarantined takes expected_revision and no plan")
			return
		}
		sess, err := h.service.Discard(r.Context(), sessionIdempotencyKey(r), DiscardRequest{
			RetireQuarantined: true, SessionID: sessionID, ExpectedRevision: body.ExpectedRevision,
		})
		if err != nil {
			writeSessionServiceError(w, err)
			return
		}
		op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
		if !ok {
			return
		}
		writeSessionJSON(w, http.StatusOK, sessionMutationSessionResponse{Operation: publicOperation(op), Session: publicSession(sess)})
		return
	}
	if !validSessionHTTPPathID(body.PlanOperationID) {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "discard plan operation id is required")
		return
	}
	planOp, err := h.service.GetOperationByID(r.Context(), body.PlanOperationID)
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	if planOp.Method != "PlanDiscard" || planOp.State != session.OperationSucceeded {
		writeSessionServiceError(w, session.ErrOperationUncertain)
		return
	}
	var planned PlanDiscardResult
	if err := json.Unmarshal(planOp.Result, &planned); err != nil || planned.Plan.SessionID != sessionID {
		writeSessionHTTPError(w, http.StatusConflict, string(session.CodeDiscardPlanStale), "discard plan does not belong to this session")
		return
	}
	sess, err := h.service.Discard(r.Context(), sessionIdempotencyKey(r), DiscardRequest{PlanOperationID: body.PlanOperationID})
	if err != nil {
		writeSessionServiceError(w, err)
		return
	}
	op, ok := h.operationForKey(w, r, sessionIdempotencyKey(r))
	if !ok {
		return
	}
	writeSessionJSON(w, http.StatusOK, sessionMutationSessionResponse{Operation: publicOperation(op), Session: publicSession(sess)})
}

func (h *sessionHTTPHandler) requirePost(w http.ResponseWriter, r *http.Request) bool {
	if !sessionHTTPMethod(w, r, http.MethodPost) {
		return false
	}
	if !sessionQueryOnly(w, r) {
		return false
	}
	if len(r.Header.Values("Idempotency-Key")) != 1 {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key must be specified once")
		return false
	}
	if sessionIdempotencyKey(r) == "" {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key is required")
		return false
	}
	if len(r.Header.Values("Content-Type")) != 1 {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "Content-Type must be specified once")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "Content-Type must be application/json")
		return false
	}
	return true
}

func decodeSessionJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	return decodeSessionJSONLimit(w, r, value, sessionHTTPMaxBody)
}

func decodeSessionJSONLimit(w http.ResponseWriter, r *http.Request, value any, limit int64) bool {
	detail := fmt.Sprintf("request body exceeds %d bytes", limit)
	if r.ContentLength > limit {
		writeSessionHTTPError(w, http.StatusRequestEntityTooLarge, "request_too_large", detail)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		if isSessionBodyTooLarge(err) {
			writeSessionHTTPError(w, http.StatusRequestEntityTooLarge, "request_too_large", detail)
		} else if errors.Is(err, io.EOF) {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON value")
		} else {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "request body is invalid JSON")
		}
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if isSessionBodyTooLarge(err) {
			writeSessionHTTPError(w, http.StatusRequestEntityTooLarge, "request_too_large", detail)
		} else {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON value")
		}
		return false
	}
	return true
}

func decodeSessionJSONValue(data []byte, value any) error {
	if len(data) == 0 {
		return errors.New("request value is required")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("request value must contain exactly one JSON value")
	}
	return nil
}

func isSessionBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func sessionIdempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

func (h *sessionHTTPHandler) operationForKey(w http.ResponseWriter, r *http.Request, key string) (session.Operation, bool) {
	op, err := h.service.GetOperation(r.Context(), key)
	if err != nil {
		writeSessionServiceError(w, err)
		return session.Operation{}, false
	}
	return op, true
}

func sessionHTTPMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	writeSessionHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed for this resource")
	return false
}

func validSessionHTTPPathID(value string) bool {
	return value != "" && len(value) <= session.MaxIDBytes && !strings.ContainsAny(value, "/\\\x00")
}

func sessionListQuery(w http.ResponseWriter, r *http.Request) (int, bool) {
	if !sessionQueryOnly(w, r, "limit") {
		return 0, false
	}
	limit := sessionHTTPDefaultMax
	if r.URL.Query().Has("limit") {
		raw := r.URL.Query().Get("limit")
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > sessionHTTPMaxList {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "limit is outside bounds")
			return 0, false
		}
		limit = parsed
	}
	return limit, true
}

func sessionCursorQuery(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	if !sessionQueryOnly(w, r, "after", "limit") {
		return 0, 0, false
	}
	var after int64
	var limit = sessionHTTPDefaultMax
	query := r.URL.Query()
	if query.Has("after") {
		raw := query.Get("after")
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "after is outside bounds")
			return 0, 0, false
		}
		after = parsed
	}
	if query.Has("limit") {
		raw := query.Get("limit")
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > sessionHTTPMaxList {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "limit is outside bounds")
			return 0, 0, false
		}
		limit = parsed
	}
	return after, limit, true
}

func sessionQueryOnly(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	valid := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		valid[name] = true
	}
	for name := range r.URL.Query() {
		if !valid[name] {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "unknown query parameter")
			return false
		}
		if len(r.URL.Query()[name]) != 1 {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "query parameter may be specified once")
			return false
		}
	}
	return true
}

func publicSession(value session.Session) SessionDTO {
	companions := make([]SessionCompanionDTO, 0, len(value.Companions))
	for _, companion := range value.Companions {
		companions = append(companions, SessionCompanionDTO{
			Name: companion.Name, Path: sessionCompanionBoxPath(companion.Name),
			BaseCommit: companion.BaseCommit,
		})
	}
	return SessionDTO{
		ID: value.ID, ExternalRef: value.ExternalRef, Target: value.Target, Policy: value.Policy,
		PolicyDigest: value.PolicyDigest, AuthorityDigest: value.AuthorityDigest,
		ProjectEnv: value.ProjectEnv, ProjectMCP: value.ProjectMCP,
		Mode:                      normalizedSessionMode(value.Mode),
		RepositoryReadOnly:        value.RepositoryReadOnly,
		ResponderBindingDigest:    sessionResponderBindingDigest(value),
		WorkspaceTask:             publicWorkspaceTask(value.WorkspaceTask),
		BaseCommit:                value.BaseCommit,
		RepositoryFreshnessStatus: repositoryFreshnessStatus(value.RepositoryFreshness),
		RepositoryFreshness:       append([]session.RepositoryFreshnessReceipt(nil), value.RepositoryFreshness...),
		PullRequest:               cloneSessionPullRequestBinding(value.PullRequest),
		Companions:                companions,
		Network: SessionNetworkSummaryDTO{
			Mode: normalizedSessionNetworkMode(value.NetworkMode), Fingerprint: value.NetworkFingerprint,
		},
		ForkName: value.ForkName,
		Revision: value.Revision, State: value.State, Activity: value.Activity, MaxTurns: value.MaxTurns,
		MaxQueuedTurns: value.MaxQueuedTurns, MaxQueuedBytes: value.MaxQueuedBytes, TurnsUsed: value.TurnsUsed,
		QueuedTurnCount: value.QueuedTurnCount, QueuedPromptBytes: value.QueuedPromptBytes,
		ActiveTurnID: value.ActiveTurnID, LastEventSequence: value.LastEventSequence,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func repositoryFreshnessStatus(receipts []session.RepositoryFreshnessReceipt) string {
	if len(receipts) == 0 {
		return "unavailable"
	}
	return "recorded"
}

func publicWorkspaceTask(value *session.WorkspaceTaskBinding) *SessionWorkspaceTaskDTO {
	if value == nil {
		return nil
	}
	return &SessionWorkspaceTaskDTO{
		QueueID: value.QueueID, TaskID: value.TaskID, ID: value.ID,
		OfferRef: value.OfferRef, DraftSHA256: value.DraftSHA256,
	}
}

// sessionResponderBindingDigest reads the digest from the private binding when the session came
// from its canonical row, and from the receipt field when it was replayed from an operation
// result, which carries the digest and never the bearer.
func sessionResponderBindingDigest(value session.Session) string {
	if value.ResponderBinding != nil {
		return session.ResponderBindingDigest(value.ResponderBinding)
	}
	return value.ResponderBindingDigest
}

func publicTurn(value session.Turn) TurnDTO {
	responderBindingDigest := value.ResponderBindingDigest
	if responderBindingDigest == "" {
		responderBindingDigest = session.ResponderBindingDigest(value.ResponderBinding)
	}
	artifacts := make([]TurnArtifactDTO, 0, len(value.OutputArtifacts))
	for _, artifact := range value.OutputArtifacts {
		artifacts = append(artifacts, TurnArtifactDTO{
			ID: artifact.ID, Name: artifact.Name, MediaType: artifact.MediaType,
			SHA256: artifact.SHA256, Bytes: artifact.Bytes,
		})
	}
	var candidate *session.TurnCandidate
	if value.State == session.TurnAwaitingValidation {
		candidate = value.Candidate
	}
	return TurnDTO{
		ID: value.ID, SessionID: value.SessionID, Ordinal: value.Ordinal, State: value.State,
		SendState: value.SendState, AssistantMessage: value.AssistantMessage, StopReason: value.StopReason,
		ErrorCode: value.ErrorCode, ErrorDetail: publicSessionErrorDetail(value.ErrorCode, value.ErrorDetail), QueuedAt: value.QueuedAt,
		StartedAt: value.StartedAt, FinishedAt: value.FinishedAt, OutputArtifacts: artifacts,
		Usage: value.Usage, Candidate: candidate,
		ValidationCandidateSHA256: value.CandidateSHA256,
		ValidationAttempt:         value.ValidationAttempt,
		ValidationError:           value.ValidationError,
		ValidationReceipt:         value.ValidationReceipt,
		ResponderBindingDigest:    responderBindingDigest,
	}
}

func publicEvent(value session.Event) EventDTO {
	return EventDTO{ID: value.ID, SessionID: value.SessionID, Sequence: value.Sequence, TurnID: value.TurnID, Type: value.Type, Version: value.Version, OccurredAt: value.OccurredAt, Payload: json.RawMessage(value.Payload)}
}

func publicOperation(value session.Operation) OperationDTO {
	return OperationDTO{
		ID: value.ID, Method: value.Method, State: value.State, ResourceType: value.ResourceType,
		ResourceID: value.ResourceID, ErrorCode: value.ErrorCode, ErrorDetail: publicSessionErrorDetail(value.ErrorCode, value.ErrorDetail),
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func publicChange(value sessionWorkspaceChange) SessionChangeDTO {
	return SessionChangeDTO{Path: value.Path, PathBytes: append([]byte(nil), value.PathBytes...), OldPath: value.OldPath, OldPathBytes: append([]byte(nil), value.OldPathBytes...), Status: value.Status}
}

func publicChanges(value WorkspaceChanges) SessionChangesDTO {
	convert := func(values []sessionWorkspaceChange) []SessionChangeDTO {
		out := make([]SessionChangeDTO, 0, len(values))
		for _, item := range values {
			out = append(out, publicChange(item))
		}
		return out
	}
	return SessionChangesDTO{
		BaseCommit: value.BaseCommit, ForkHead: value.ForkHead, ForkTree: value.ForkTree,
		PullRequestTree: value.PullRequestTree, ParentHead: value.ParentHead,
		Committed: convert(value.Committed), Staged: convert(value.Staged), Unstaged: convert(value.Unstaged),
		Untracked: convert(value.Untracked), Conflicts: convert(value.Conflicts),
		ParentDivergence: SessionParentDivergenceDTO{Ahead: value.ParentDivergence.Ahead, Behind: value.ParentDivergence.Behind, BaseToFork: value.ParentDivergence.BaseToFork, BaseToParent: value.ParentDivergence.BaseToParent, Diverged: value.ParentDivergence.Diverged},
		Patch:            []byte(value.Patch), Truncated: value.Truncated,
		PatchDigest: value.PatchDigest, PatchBytes: value.PatchBytes,
		PatchOffset: value.PatchOffset, PatchNextOffset: value.PatchNextOffset,
		PatchHasMore: value.PatchHasMore,
	}
}

func publicReview(value ReviewDossier) SessionReviewDTO {
	result := SessionReviewDTO{
		OperationID: value.OperationID, SessionID: value.SessionID, SessionRevision: value.SessionRevision,
		PolicyDigest: value.PolicyDigest, PullRequest: cloneSessionPullRequestBinding(value.PullRequest),
		CreationBase: value.CreationBase, SourceHead: value.SourceHead,
		SourceTree: value.SourceTree, ParentHead: value.ParentHead, ParentTree: value.ParentTree,
		CandidateHead: value.CandidateHead, CandidateTree: value.CandidateTree, Rebase: value.Rebase,
		Gate: value.Gate, PolicyFindings: append([]string{}, value.PolicyFindings...),
		Patch: append([]byte(nil), value.Patch...), PatchTruncated: value.PatchTruncated,
		PatchArtifactID: value.PatchArtifactID, PatchDigest: value.PatchDigest,
		PatchBytes: value.PatchBytes, Publishable: value.Publishable,
		NotPublishableReasons: append([]string{}, value.NotPublishableReasons...),
	}
	if value.GateError != "" {
		result.GateError = "review gate could not start"
	}
	return result
}

func publicSessionErrorDetail(code session.ErrorCode, detail string) string {
	return BoundedDetail(session.PublicErrorDetail(code, detail))
}

func publicDiscardPlan(value PlanDiscardResult) SessionPlanDiscardDTO {
	workspace := value.Plan.Workspace
	return SessionPlanDiscardDTO{
		OperationID: value.OperationID,
		Plan: SessionDiscardPlanDTO{SessionID: value.Plan.SessionID, Revision: value.Plan.Revision, Workspace: SessionDiscardWorkspaceDTO{
			Branch: workspace.Branch, Head: workspace.Head, StatusDigest: workspace.StatusDigest,
			Dirty: workspace.Dirty, Unmerged: workspace.Unmerged, Running: workspace.Running,
			AcceptedDirty: workspace.AcceptedDirty, AcceptedUnmerged: workspace.AcceptedUnmerged,
		}},
	}
}

func BoundedDetail(value string) string {
	if len(value) > session.MaxErrorDetailBytes {
		return value[:session.MaxErrorDetailBytes]
	}
	return value
}

func writeSessionJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeSessionHTTPError(w http.ResponseWriter, status int, code, detail string) {
	writeSessionJSON(w, status, sessionHTTPErrorResponse{Error: sessionHTTPErrorBody{Code: code, Detail: BoundedDetail(detail)}})
}

func writeSessionServiceError(w http.ResponseWriter, err error) {
	code, status, detail := sessionHTTPError(err)
	var operationID string
	var operationErr interface{ OperationID() string }
	if errors.As(err, &operationErr) {
		operationID = operationErr.OperationID()
	}
	writeSessionJSON(w, status, sessionHTTPErrorResponse{Error: sessionHTTPErrorBody{
		Code: code, Detail: BoundedDetail(detail), OperationID: operationID,
	}})
}

func sessionHTTPError(err error) (string, int, string) {
	if err == nil {
		return "internal_error", http.StatusInternalServerError, "internal server error"
	}
	code := session.CodeOf(err)
	if code == "" {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "internal_error", http.StatusInternalServerError, "internal server error"
		}
		return "internal_error", http.StatusInternalServerError, "internal server error"
	}
	status := http.StatusBadRequest
	switch code {
	case session.CodeSessionNotFound, session.CodeTurnNotFound, session.CodeOperationNotFound:
		status = http.StatusNotFound
	case session.CodeIdempotencyConflict, session.CodeOperationIntentConflict, session.CodeOperationUncertain,
		session.CodeOperationFenced,
		session.CodeRevisionConflict, session.CodeInvalidSessionState, session.CodeQueueFull,
		session.CodeBudgetExhausted, session.CodeTurnNotRunnable, session.CodeNativeSessionConflict,
		session.CodeDiscardPlanStale, session.CodePolicyDigestMismatch, session.CodeNetworkFingerprintMismatch:
		status = http.StatusConflict
	case session.CodeInternal:
		status = http.StatusInternalServerError
	case session.CodeRepositoryUnavailable, session.CodeNetworkUnavailable, sessionACPCleanupError:
		status = http.StatusServiceUnavailable
	}
	var typed *session.Error
	if errors.As(err, &typed) {
		detail := publicSessionErrorDetail(code, typed.Detail)
		if detail == "" {
			detail = strings.ReplaceAll(string(code), "_", " ")
		}
		return string(code), status, detail
	}
	return string(code), status, "request failed"
}

// EnsureAncestors creates missing parents of the state root without weakening permissions on an
// existing home/config directory. The state root itself is hardened by session.Open.
func EnsureAncestors(target string) error {
	target, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(target)
	current := volume + string(filepath.Separator)
	rel := strings.TrimPrefix(target, current)
	if rel == "" {
		return nil
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return fmt.Errorf("create session state parent: %w", err)
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return fmt.Errorf("inspect session state parent: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("session state parent is not a real directory")
		}
	}
	return nil
}

type sessionSocketOwner struct {
	path string
	info os.FileInfo
	once sync.Once
}

func ListenSocket(stateRoot, socketPath string) (net.Listener, func(), error) {
	root, err := filepath.Abs(filepath.Clean(stateRoot))
	if err != nil {
		return nil, nil, err
	}
	socketPath, err = filepath.Abs(filepath.Clean(socketPath))
	if err != nil {
		return nil, nil, err
	}
	rel, err := filepath.Rel(root, socketPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, nil, errors.New("session socket must be contained beneath the state root")
	}
	if err := ensureSessionPrivatePath(root, filepath.Dir(socketPath)); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, errors.New("session socket is a symlink")
		}
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, errors.New("session socket path is not a socket")
		}
		if err := os.Remove(socketPath); err != nil {
			return nil, nil, fmt.Errorf("remove stale session socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("inspect session socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on session socket: %w", err)
	}
	closeOnError := func(err error) (net.Listener, func(), error) {
		_ = listener.Close()
		return nil, nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return closeOnError(fmt.Errorf("protect session socket: %w", err))
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		return closeOnError(fmt.Errorf("inspect owned session socket: %w", err))
	}
	owner := &sessionSocketOwner{path: socketPath, info: info}
	cleanup := func() {
		owner.once.Do(func() {
			if current, err := os.Lstat(owner.path); err == nil && os.SameFile(current, owner.info) {
				_ = os.Remove(owner.path)
			}
		})
	}
	return listener, cleanup, nil
}

func ensureSessionPrivatePath(root, target string) error {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect session state root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("session state root is not a real directory")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("protect session state root: %w", err)
	}
	target, err = filepath.Abs(filepath.Clean(target))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("session socket parent escaped state root")
	}
	parts := []string{}
	if rel != "." {
		parts = strings.Split(rel, string(filepath.Separator))
	}
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		path := current
		info, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(path, 0o700); err != nil {
				return fmt.Errorf("create session socket parent: %w", err)
			}
			info, statErr = os.Lstat(path)
		}
		if statErr != nil {
			return fmt.Errorf("inspect session socket parent: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("session socket parent is not a real directory")
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("protect session socket parent: %w", err)
		}
	}
	return nil
}
