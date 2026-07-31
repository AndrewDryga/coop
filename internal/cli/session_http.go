package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	sessionHTTPMaxBody     = 128 << 10
	sessionHTTPTurnMaxBody = 12 << 20
	sessionHTTPDefaultMax  = 100
	sessionHTTPMaxList     = 1000
)

// These DTOs are the public v1 wire types. They deliberately do not mirror the durable records:
// the latter contain prompts, operation secrets, native provider identities, and host paths.
type SessionDTO struct {
	ID                string                `json:"id"`
	ExternalRef       string                `json:"external_ref"`
	Target            string                `json:"target"`
	Policy            string                `json:"policy"`
	PolicyDigest      string                `json:"policy_digest"`
	BaseCommit        string                `json:"base_commit"`
	Companions        []SessionCompanionDTO `json:"companions,omitempty"`
	ForkName          string                `json:"fork_name"`
	Revision          int64                 `json:"revision"`
	State             session.SessionState  `json:"state"`
	Activity          session.ActivityState `json:"activity"`
	MaxTurns          int                   `json:"max_turns"`
	MaxQueuedTurns    int                   `json:"max_queued_turns"`
	MaxQueuedBytes    int                   `json:"max_queued_bytes"`
	TurnsUsed         int                   `json:"turns_used"`
	QueuedTurnCount   int                   `json:"queued_turn_count"`
	QueuedPromptBytes int                   `json:"queued_prompt_bytes"`
	ActiveTurnID      string                `json:"active_turn_id,omitempty"`
	LastEventSequence int64                 `json:"last_event_sequence"`
	CreatedAt         time.Time             `json:"created_at"`
	UpdatedAt         time.Time             `json:"updated_at"`
}

type SessionCompanionDTO struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	BaseCommit string `json:"base_commit"`
}

type TurnDTO struct {
	ID               string             `json:"id"`
	SessionID        string             `json:"session_id"`
	Ordinal          int64              `json:"ordinal"`
	State            session.TurnState  `json:"state"`
	SendState        session.SendState  `json:"send_state"`
	AssistantMessage string             `json:"assistant_message,omitempty"`
	StopReason       session.StopReason `json:"stop_reason,omitempty"`
	ErrorCode        session.ErrorCode  `json:"error_code,omitempty"`
	ErrorDetail      string             `json:"error_detail,omitempty"`
	QueuedAt         time.Time          `json:"queued_at"`
	StartedAt        time.Time          `json:"started_at,omitempty"`
	FinishedAt       time.Time          `json:"finished_at,omitempty"`
}

type EventDTO struct {
	ID         string            `json:"id"`
	SessionID  string            `json:"session_id"`
	Sequence   int64             `json:"sequence"`
	TurnID     string            `json:"turn_id,omitempty"`
	Type       session.EventType `json:"type"`
	Version    int               `json:"version"`
	OccurredAt time.Time         `json:"occurred_at"`
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
	OperationID           string                    `json:"operation_id"`
	SessionID             string                    `json:"session_id"`
	SessionRevision       int64                     `json:"session_revision"`
	PolicyDigest          string                    `json:"policy_digest"`
	CreationBase          string                    `json:"creation_base"`
	SourceHead            string                    `json:"source_head"`
	SourceTree            string                    `json:"source_tree"`
	ParentHead            string                    `json:"parent_head"`
	ParentTree            string                    `json:"parent_tree"`
	CandidateHead         string                    `json:"candidate_head"`
	CandidateTree         string                    `json:"candidate_tree"`
	Rebase                SessionReviewRebaseStatus `json:"rebase"`
	Gate                  SessionReviewGateStatus   `json:"gate"`
	GateError             string                    `json:"gate_error,omitempty"`
	PolicyFindings        []string                  `json:"policy_findings,omitempty"`
	Patch                 []byte                    `json:"patch,omitempty"`
	PatchTruncated        bool                      `json:"patch_truncated"`
	PatchArtifactID       string                    `json:"patch_artifact_id,omitempty"`
	PatchDigest           string                    `json:"patch_digest,omitempty"`
	PatchBytes            int64                     `json:"patch_bytes"`
	Publishable           bool                      `json:"publishable"`
	NotPublishableReasons []string                  `json:"not_publishable_reasons,omitempty"`
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

type sessionHTTPErrorBody struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

type sessionHTTPErrorResponse struct {
	Error sessionHTTPErrorBody `json:"error"`
}

type sessionHTTPHandler struct {
	service *SessionService
	ready   bool
}

func newSessionHTTPHandler(service *SessionService) http.Handler {
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
	}
	if r.URL.Path == "/v1/sessions" || strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		h.serveSessionPath(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/operations/") {
		h.serveOperationPath(w, r)
		return
	}
	writeSessionHTTPError(w, http.StatusNotFound, "not_found", "resource not found")
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
	case len(parts) == 2 && parts[1] == "budget":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.extendBudget(w, r, sessionID)
		}
	case len(parts) == 2 && parts[1] == "review":
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.review(w, r, sessionID)
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
	case len(parts) == 4 && parts[1] == "turns" && parts[3] == "cancel" && parts[2] != "":
		turnID := parts[2]
		if !validSessionHTTPPathID(turnID) {
			writeSessionHTTPError(w, http.StatusBadRequest, "invalid_request", "invalid turn id")
			return
		}
		if sessionHTTPMethod(w, r, http.MethodPost) {
			h.cancelTurn(w, r, sessionID, turnID)
		}
	default:
		writeSessionHTTPError(w, http.StatusNotFound, "not_found", "resource not found")
	}
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
		Policy string `json:"policy"`
		Task   string `json:"task"`
	}
	if !decodeSessionJSON(w, r, &body) {
		return
	}
	sess, err := h.service.CreateRemoteSession(r.Context(), sessionIdempotencyKey(r), CreateRemoteSessionRequest{Policy: body.Policy, Task: body.Task})
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

func (h *sessionHTTPHandler) submitTurn(w http.ResponseWriter, r *http.Request, sessionID string) {
	if !h.requirePost(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64                   `json:"expected_revision"`
		Prompt           string                  `json:"prompt"`
		Artifacts        []session.InputArtifact `json:"artifacts,omitempty"`
	}
	if !decodeSessionJSONLimit(w, r, &body, sessionHTTPTurnMaxBody) {
		return
	}
	turn, err := h.service.SubmitTurn(r.Context(), sessionIdempotencyKey(r), session.SubmitTurnRequest{
		SessionID: sessionID, ExpectedRevision: body.ExpectedRevision, Prompt: body.Prompt,
		Artifacts: body.Artifacts,
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
	for _, event := range events {
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
	var changes SessionWorkspaceChanges
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
		PlanOperationID string `json:"plan_operation_id"`
	}
	if !decodeSessionJSON(w, r, &body) {
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
		PolicyDigest: value.PolicyDigest, BaseCommit: value.BaseCommit,
		Companions: companions, ForkName: value.ForkName,
		Revision: value.Revision, State: value.State, Activity: value.Activity, MaxTurns: value.MaxTurns,
		MaxQueuedTurns: value.MaxQueuedTurns, MaxQueuedBytes: value.MaxQueuedBytes, TurnsUsed: value.TurnsUsed,
		QueuedTurnCount: value.QueuedTurnCount, QueuedPromptBytes: value.QueuedPromptBytes,
		ActiveTurnID: value.ActiveTurnID, LastEventSequence: value.LastEventSequence,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func publicTurn(value session.Turn) TurnDTO {
	return TurnDTO{
		ID: value.ID, SessionID: value.SessionID, Ordinal: value.Ordinal, State: value.State,
		SendState: value.SendState, AssistantMessage: value.AssistantMessage, StopReason: value.StopReason,
		ErrorCode: value.ErrorCode, ErrorDetail: publicSessionErrorDetail(value.ErrorCode, value.ErrorDetail), QueuedAt: value.QueuedAt,
		StartedAt: value.StartedAt, FinishedAt: value.FinishedAt,
	}
}

func publicEvent(value session.Event) EventDTO {
	return EventDTO{ID: value.ID, SessionID: value.SessionID, Sequence: value.Sequence, TurnID: value.TurnID, Type: value.Type, Version: value.Version, OccurredAt: value.OccurredAt}
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

func publicChanges(value SessionWorkspaceChanges) SessionChangesDTO {
	convert := func(values []sessionWorkspaceChange) []SessionChangeDTO {
		out := make([]SessionChangeDTO, 0, len(values))
		for _, item := range values {
			out = append(out, publicChange(item))
		}
		return out
	}
	return SessionChangesDTO{
		BaseCommit: value.BaseCommit, ForkHead: value.ForkHead, ParentHead: value.ParentHead,
		Committed: convert(value.Committed), Staged: convert(value.Staged), Unstaged: convert(value.Unstaged),
		Untracked: convert(value.Untracked), Conflicts: convert(value.Conflicts),
		ParentDivergence: SessionParentDivergenceDTO{Ahead: value.ParentDivergence.Ahead, Behind: value.ParentDivergence.Behind, BaseToFork: value.ParentDivergence.BaseToFork, BaseToParent: value.ParentDivergence.BaseToParent, Diverged: value.ParentDivergence.Diverged},
		Patch:            []byte(value.Patch), Truncated: value.Truncated,
		PatchDigest: value.PatchDigest, PatchBytes: value.PatchBytes,
		PatchOffset: value.PatchOffset, PatchNextOffset: value.PatchNextOffset,
		PatchHasMore: value.PatchHasMore,
	}
}

func publicReview(value SessionReviewDossier) SessionReviewDTO {
	result := SessionReviewDTO{
		OperationID: value.OperationID, SessionID: value.SessionID, SessionRevision: value.SessionRevision,
		PolicyDigest: value.PolicyDigest, CreationBase: value.CreationBase, SourceHead: value.SourceHead,
		SourceTree: value.SourceTree, ParentHead: value.ParentHead, ParentTree: value.ParentTree,
		CandidateHead: value.CandidateHead, CandidateTree: value.CandidateTree, Rebase: value.Rebase,
		Gate: value.Gate, PolicyFindings: append([]string(nil), value.PolicyFindings...),
		Patch: append([]byte(nil), value.Patch...), PatchTruncated: value.PatchTruncated,
		PatchArtifactID: value.PatchArtifactID, PatchDigest: value.PatchDigest,
		PatchBytes: value.PatchBytes, Publishable: value.Publishable,
		NotPublishableReasons: append([]string(nil), value.NotPublishableReasons...),
	}
	if value.GateError != "" {
		result.GateError = "review gate could not start"
	}
	return result
}

func publicSessionErrorDetail(code session.ErrorCode, detail string) string {
	switch code {
	case "":
		return ""
	case session.CodeInternal:
		return "internal operation failure"
	case session.CodeDiscardPlanStale:
		return "discard plan no longer matches workspace state"
	default:
		return boundedPublicDetail(detail)
	}
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

func boundedPublicDetail(value string) string {
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
	writeSessionJSON(w, status, sessionHTTPErrorResponse{Error: sessionHTTPErrorBody{Code: code, Detail: boundedPublicDetail(detail)}})
}

func writeSessionServiceError(w http.ResponseWriter, err error) {
	code, status, detail := sessionHTTPError(err)
	writeSessionHTTPError(w, status, code, detail)
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
		session.CodeRevisionConflict, session.CodeInvalidSessionState, session.CodeQueueFull,
		session.CodeBudgetExhausted, session.CodeTurnNotRunnable, session.CodeNativeSessionConflict,
		session.CodeDiscardPlanStale:
		status = http.StatusConflict
	case session.CodeInternal:
		status = http.StatusInternalServerError
	}
	var typed *session.Error
	if errors.As(err, &typed) {
		return string(code), status, publicSessionErrorDetail(code, typed.Detail)
	}
	return string(code), status, "request failed"
}

func defaultSessionStateRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "coop", "sessions"), nil
}

func defaultSessionPolicyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".config", "coop", "session-policies.yaml"), nil
}

func sessionCLIPaths(state, policy, socket string) (string, string, string, error) {
	if state == "" {
		var err error
		state, err = defaultSessionStateRoot()
		if err != nil {
			return "", "", "", err
		}
	}
	if policy == "" {
		var err error
		policy, err = defaultSessionPolicyPath()
		if err != nil {
			return "", "", "", err
		}
	}
	state, err := filepath.Abs(filepath.Clean(state))
	if err != nil {
		return "", "", "", fmt.Errorf("resolve session state path: %w", err)
	}
	if socket == "" {
		socket = filepath.Join(state, "control.sock")
	} else {
		socket, err = filepath.Abs(filepath.Clean(socket))
		if err != nil {
			return "", "", "", fmt.Errorf("resolve session socket path: %w", err)
		}
	}
	policy, err = filepath.Abs(filepath.Clean(policy))
	if err != nil {
		return "", "", "", fmt.Errorf("resolve session policy path: %w", err)
	}
	return state, policy, socket, nil
}

func parseSessionsFlags(args []string, command string) (state, policy, socket string, jsonOutput bool, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" && command == "doctor" {
			if jsonOutput {
				return "", "", "", false, errors.New("sessions doctor: --json may be specified once")
			}
			jsonOutput = true
			continue
		}
		var target *string
		switch arg {
		case "--state":
			target = &state
		case "--policies":
			target = &policy
		case "--socket":
			target = &socket
		default:
			return "", "", "", false, fmt.Errorf("sessions %s: unknown flag %q", command, arg)
		}
		if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
			return "", "", "", false, fmt.Errorf("sessions %s: flag %s requires a value", command, arg)
		}
		i++
		if *target != "" {
			return "", "", "", false, fmt.Errorf("sessions %s: flag %s may be specified once", command, arg)
		}
		*target = args[i]
	}
	if command == "doctor" && (state != "" || policy != "") {
		return "", "", "", false, errors.New("sessions doctor: only --socket and --json are supported")
	}
	return state, policy, socket, jsonOutput, nil
}

func (a *app) cmdSessions(args []string) (int, error) {
	if len(args) == 0 {
		return groupHelp("sessions")
	}
	switch args[0] {
	case "serve":
		state, policy, socket, _, err := parseSessionsFlags(args[1:], "serve")
		if err != nil {
			return 2, err
		}
		return runSessionServe(state, policy, socket)
	case "doctor":
		_, _, socket, jsonOutput, err := parseSessionsFlags(args[1:], "doctor")
		if err != nil {
			return 2, err
		}
		return runSessionDoctor(socket, jsonOutput)
	default:
		return 2, fmt.Errorf("sessions: unknown command %q", args[0])
	}
}

func runSessionServe(state, policy, socket string) (int, error) {
	state, policy, socket, err := sessionCLIPaths(state, policy, socket)
	if err != nil {
		return 2, err
	}
	if err := ensureSessionAncestors(filepath.Dir(state)); err != nil {
		return 1, err
	}
	service, err := NewSessionService(SessionServiceConfig{StateRoot: state, PolicyPath: policy, SourceConfig: config.Load(), Executable: os.Args[0]})
	if err != nil {
		return 1, err
	}
	defer service.Stop()
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := service.Start(ctx); err != nil {
		return 1, err
	}
	listener, cleanup, err := listenSessionSocket(state, socket)
	if err != nil {
		return 1, err
	}
	defer cleanup()
	server := &http.Server{Handler: newSessionHTTPHandler(service)}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	select {
	case err := <-serveDone:
		if errors.Is(err, http.ErrServerClosed) {
			return 0, nil
		}
		return 1, err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), sessionServiceDefaultStopTimeout)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancel()
		if shutdownErr != nil {
			_ = server.Close()
			<-serveDone
			return 1, shutdownErr
		}
		if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return 1, err
		}
		return 0, nil
	}
}

// ensureSessionAncestors creates missing parents without weakening permissions on an existing
// home/config directory. The state root itself is hardened by session.Open.
func ensureSessionAncestors(target string) error {
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

func listenSessionSocket(stateRoot, socketPath string) (net.Listener, func(), error) {
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

type sessionDoctorResult struct {
	Socket  string `json:"socket"`
	Healthy bool   `json:"healthy"`
	Ready   bool   `json:"ready"`
	Error   string `json:"error,omitempty"`
}

func runSessionDoctor(socket string, jsonOutput bool) (int, error) {
	_, _, socket, err := sessionCLIPaths("", "", socket)
	if err != nil {
		return 2, err
	}
	result := sessionDoctorResult{Socket: socket}
	client := sessionUnixHTTPClient(socket)
	healthErr := sessionDoctorGet(client, "/healthz", &result)
	if healthErr == nil {
		result.Healthy = true
	}
	var ready sessionReadyDTO
	readyErr := sessionDoctorGet(client, "/readyz", &ready)
	if readyErr == nil {
		result.Ready = ready.Ready
	}
	if healthErr != nil {
		result.Error = boundedPublicDetail(healthErr.Error())
	}
	if readyErr != nil {
		if result.Error != "" {
			result.Error += "; "
		}
		result.Error += boundedPublicDetail(readyErr.Error())
	}
	if result.Error == "" && !result.Ready {
		result.Error = "session service is not ready"
	}
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return 1, err
		}
	} else if result.Error != "" {
		ui.Error("session service is unavailable or unready: %s", result.Error)
	} else {
		ui.OK("session service is healthy and ready at %s", result.Socket)
	}
	if result.Error != "" || !result.Healthy || !result.Ready {
		return 1, nil
	}
	return 0, nil
}

func sessionUnixHTTPClient(socket string) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func sessionDoctorGet(client *http.Client, path string, value any) error {
	request, err := http.NewRequest(http.MethodGet, "http://unix"+path, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("connect to session socket: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("session endpoint returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(value); err != nil {
		return fmt.Errorf("decode session endpoint: %w", err)
	}
	return nil
}
