package sessionsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const (
	sessionReviewFindingLimit      = 64
	sessionReviewFindingBytes      = 1024
	sessionReviewFindingTotalBytes = 32 << 10
)

var errSessionReviewSourceBranch = errors.New("review source is not on its bound branch")

var (
	errSessionReviewSourceDirty       = errors.New("review source is not clean")
	errSessionReviewSourceStatusLarge = errors.New("review source status exceeds the bounded limit")
)

type ReviewGateStatus string

const (
	ReviewGateNone         ReviewGateStatus = "none"
	ReviewGatePassed       ReviewGateStatus = "passed"
	ReviewGateFailed       ReviewGateStatus = "failed"
	ReviewGateStartupError ReviewGateStatus = "startup_error"
	ReviewGateNotRun       ReviewGateStatus = "not_run"
)

type ReviewRebaseStatus string

const (
	ReviewRebaseClean    ReviewRebaseStatus = "clean"
	ReviewRebaseConflict ReviewRebaseStatus = "conflict"
)

type RunReviewRequest struct {
	SessionID        string `json:"session_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type ReviewDossier struct {
	OperationID           string                 `json:"operation_id"`
	SessionID             string                 `json:"session_id"`
	SessionRevision       int64                  `json:"session_revision"`
	JobDigest             string                 `json:"job_digest"`
	Source                *session.SourceBinding `json:"source,omitempty"`
	CreationBase          string                 `json:"creation_base"`
	SourceHead            string                 `json:"source_head"`
	SourceTree            string                 `json:"source_tree"`
	ParentHead            string                 `json:"parent_head"`
	ParentTree            string                 `json:"parent_tree"`
	CandidateHead         string                 `json:"candidate_head"`
	CandidateTree         string                 `json:"candidate_tree"`
	CandidateRetained     bool                   `json:"candidate_retained"`
	Rebase                ReviewRebaseStatus     `json:"rebase"`
	Gate                  ReviewGateStatus       `json:"gate"`
	GateError             string                 `json:"gate_error,omitempty"`
	GateOutput            *ReviewGateOutput      `json:"gate_output,omitempty"`
	PolicyFindings        []string               `json:"policy_findings,omitempty"`
	Patch                 []byte                 `json:"patch,omitempty"`
	PatchTruncated        bool                   `json:"patch_truncated"`
	Publishable           bool                   `json:"publishable"`
	NotPublishableReasons []string               `json:"not_publishable_reasons,omitempty"`
}

// ReviewGateResult is the complete outcome of the trusted parent gate.
// StartupError is a successful review outcome, not a failed operation.
// Command and ExitCode say what ran and how it ended, when the gate knows.
type ReviewGateResult struct {
	Configured   bool
	Passed       bool
	StartupError string
	Command      []string
	ExitCode     *int
}

// ReviewGateRequest carries the saved job authority to the host-owned checker. The candidate is
// disposable; the repository and network binding come from the authenticated session, not it.
// Output receives everything the gate prints, stdout and stderr alike, for the review to keep.
type ReviewGateRequest struct {
	Repository, Candidate, StateRoot, OperationID         string
	SessionID, JobDigest                                  string
	NetworkMode, NetworkFingerprint, NetworkQualification string
	Command                                               []string
	Environment                                           map[string]string
	Resources                                             workerproto.JobResources
	Output                                                io.Writer
}

// ReviewGate is the narrow gate seam used by RunReview. Implementations must not mutate
// Repository. A gate may create ignored build output in the disposable candidate; RunReview rejects
// any change to its pinned commit, tree, branch, tracked files, or non-ignored untracked files.
type ReviewGate interface {
	Run(context.Context, ReviewGateRequest) (ReviewGateResult, error)
}

type ReviewGateFunc func(context.Context, ReviewGateRequest) (ReviewGateResult, error)

func (f ReviewGateFunc) Run(ctx context.Context, request ReviewGateRequest) (ReviewGateResult, error) {
	return f(ctx, request)
}

// MaxReviewErrorBytes bounds a gate's startup-error prose, so a host implementing
// ReviewGate truncates the same way the service's own paths do.
const MaxReviewErrorBytes = session.MaxErrorDetailBytes

type sessionReviewIntent struct {
	OperationID        string                 `json:"operation_id"`
	SessionID          string                 `json:"session_id"`
	SessionRevision    int64                  `json:"session_revision"`
	Repository         string                 `json:"repository"`
	Workspace          string                 `json:"workspace"`
	ForkGeneration     string                 `json:"fork_generation"`
	CreationBase       string                 `json:"creation_base"`
	SourceHead         string                 `json:"source_head"`
	SourceTree         string                 `json:"source_tree"`
	SourceBranch       string                 `json:"source_branch"`
	SourceStatusDigest string                 `json:"source_status_digest"`
	ParentHead         string                 `json:"parent_head"`
	ParentTree         string                 `json:"parent_tree"`
	JobDigest          string                 `json:"job_digest"`
	Source             *session.SourceBinding `json:"source,omitempty"`
	MaxPatchBytes      int                    `json:"max_patch_bytes"`
}

type sessionReviewSourceIdentity struct {
	Head         string
	Tree         string
	Branch       string
	StatusDigest string
}

type sessionReviewParentIdentity struct {
	Head string
	Tree string
}

func (s *Service) RunReview(ctx context.Context, key string, req RunReviewRequest) (ReviewDossier, error) {
	unlock := s.lockOperation(key)
	defer unlock()

	if req.SessionID == "" || len(req.SessionID) > session.MaxIDBytes || !utf8SessionText(req.SessionID) || req.ExpectedRevision <= 0 {
		return ReviewDossier{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "session id and positive expected revision are required"}
	}
	op, replay, err := s.store.ReserveOperation(ctx, "RunReview", key, req)
	if err != nil {
		return ReviewDossier{}, err
	}
	if replay {
		switch op.State {
		case session.OperationSucceeded:
			dossier, decodeErr := decodeSessionReviewDossier(op.Result)
			return dossier, wrapServiceOperationError(op.ID, decodeErr)
		case session.OperationFailed:
			return ReviewDossier{}, wrapServiceOperationError(op.ID,
				&session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail})
		case session.OperationRunning, session.OperationUncertain:
			return s.resumeReview(ctx, op)
		default:
			return s.executeReview(ctx, op, req)
		}
	}
	return s.executeReview(ctx, op, req)
}

func decodeSessionReviewDossier(data []byte) (ReviewDossier, error) {
	var dossier ReviewDossier
	if err := json.Unmarshal(data, &dossier); err != nil {
		return ReviewDossier{}, fmt.Errorf("decode review operation result: %w", err)
	}
	if dossier.OperationID == "" || dossier.SessionID == "" {
		return ReviewDossier{}, errors.New("decode review operation result: missing identity")
	}
	return dossier, nil
}

// GetReview reads an immutable completed result. It never starts or resumes a gate.
func (s *Service) GetReview(ctx context.Context, sessionID, operationID string) (session.Operation, ReviewDossier, error) {
	op, err := s.store.GetOperationByID(ctx, operationID)
	if err != nil {
		return op, ReviewDossier{}, err
	}
	if op.Method != "RunReview" || op.State != session.OperationSucceeded ||
		op.ResourceType != "review" || op.ResourceID != sessionID {
		return op, ReviewDossier{}, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "operation is not a completed review for this session",
		}
	}
	dossier, err := decodeSessionReviewDossier(op.Result)
	if err != nil {
		return op, ReviewDossier{}, err
	}
	if dossier.OperationID != operationID || dossier.SessionID != sessionID {
		return op, ReviewDossier{}, errors.New("stored review identity does not match its operation")
	}
	return op, dossier, nil
}

func (s *Service) resumeReview(
	ctx context.Context,
	op session.Operation,
) (ReviewDossier, error) {
	var intent sessionReviewIntent
	if err := json.Unmarshal(op.Result, &intent); err != nil {
		if op.State == session.OperationRunning {
			return ReviewDossier{}, s.makeOperationUncertain(
				ctx, op, "review operation intent is unreadable",
			)
		}
		return ReviewDossier{}, &serviceOperationError{
			operationID: op.ID,
			err: &session.Error{
				Code: session.CodeOperationUncertain, Detail: "review operation intent is unreadable",
			},
		}
	}
	unlock, ok := s.tryLockSessionRuntime(intent.SessionID)
	if !ok {
		return ReviewDossier{}, wrapServiceOperationError(op.ID, session.ErrOperationUncertain)
	}
	defer unlock()
	bound, err := s.store.GetSession(ctx, intent.SessionID)
	if err != nil || bound.State == session.SessionDiscarded {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: "review session is unavailable",
		})
	}
	dossier, err := s.executeReviewIntent(s.reviewExecutionContext(), op, intent)
	if err != nil && session.CodeOf(err) == session.CodeOperationUncertain &&
		op.State == session.OperationRunning {
		var correlated interface{ OperationID() string }
		if errors.As(err, &correlated) {
			return ReviewDossier{}, err
		}
		return ReviewDossier{}, s.makeOperationUncertain(ctx, op, boundedSessionServiceError(err))
	}
	return dossier, err
}

func (s *Service) executeReview(ctx context.Context, op session.Operation, req RunReviewRequest) (ReviewDossier, error) {
	// Review requires a parked session, so a runtime already owned by a running turn, another
	// review, or cleanup is the documented state conflict — reported now, not after waiting up to
	// a turn timeout for the lock. Check the cheap durable preconditions before evicting a
	// healthy warm child, then remove that last possible workspace writer before reading Git.
	unlock, ok := s.tryLockSessionRuntime(req.SessionID)
	if !ok {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code:   session.CodeInvalidSessionState,
			Detail: "review requires a parked session; a turn or another runtime operation is in progress",
		})
	}
	defer unlock()
	bound, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID,
			&session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()})
	}
	if bound.Revision == req.ExpectedRevision &&
		(bound.State == session.SessionOpen || bound.State == session.SessionExhausted) &&
		bound.Activity == session.ActivityParked && bound.ActiveTurnID == "" &&
		bound.QueuedTurnCount == 0 && bound.QueuedPromptBytes == 0 {
		if evicter, ok := s.runner.(sessionRunnerWarmEvicter); ok {
			if err := evicter.EvictWarmSession(bound.ID); err != nil {
				return ReviewDossier{}, s.failServiceOperation(ctx, op.ID,
					fmt.Errorf("stop warm session before review: %w", err))
			}
		}
	}
	intent, err := s.captureReviewIntent(ctx, op.ID, req)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.store.MarkOperationRunning(ctx, op.ID, data); err != nil {
		return ReviewDossier{}, err
	}
	return s.executeReviewIntent(s.reviewExecutionContext(), op, intent)
}

// Reviews run from a frozen intent and may outlive an HTTP request. Use the service context for
// durable writes and cancellable review implementations so a client timeout cannot discard the
// result. Tests and direct local callers that do not Start the service retain synchronous behavior.
func (s *Service) reviewExecutionContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *Service) captureReviewIntent(ctx context.Context, operationID string, req RunReviewRequest) (sessionReviewIntent, error) {
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return sessionReviewIntent{}, err
	}
	if err := s.validateSessionForkAuthority(ctx, sess); err != nil {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if sess.Revision != req.ExpectedRevision {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeRevisionConflict, Detail: fmt.Sprintf("expected revision %d, current revision %d", req.ExpectedRevision, sess.Revision)}
	}
	if sess.State != session.SessionOpen && sess.State != session.SessionExhausted {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "review requires an open or exhausted session"}
	}
	if sess.Activity != session.ActivityParked || sess.ActiveTurnID != "" || sess.QueuedTurnCount != 0 || sess.QueuedPromptBytes != 0 {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "review requires a parked session with no active or queued turns"}
	}
	if !forkspace.ValidExistingName(sess.ForkName) || sess.Repository == "" || sess.Workspace == "" || sess.Workspace != forkspace.Workspace(sess.Repository, sess.ForkName) {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "session workspace is not its exact bound fork"}
	}
	if forkspace.NeedsStop(sess.Repository, sess.ForkName) {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "fork is running or cleanup-pending"}
	}
	unlockFork, err := forkspace.LockStateContext(ctx, sess.Repository, sess.ForkName)
	if err != nil {
		return sessionReviewIntent{}, err
	}
	identity, hasGeneration, authorityErr := forkspace.ReadGeneration(sess.Repository, sess.ForkName)
	if authorityErr == nil && (!hasGeneration || sess.ForkGeneration == "" ||
		identity.Generation != forkspace.Generation(sess.ForkGeneration)) {
		authorityErr = errors.New("session workspace generation changed")
	}
	if authorityErr == nil {
		authorityErr = forkspace.ValidateGenerationWorkspace(sess.Repository, identity)
	}
	if authorityErr == nil {
		reservation, reserved, readErr := forkspace.ReadWorkspaceReservation(sess.Repository, identity)
		if readErr != nil {
			authorityErr = readErr
		} else if !reserved || !reservation.MatchesSessionOwner(s.store.ID(), sess.ID) {
			authorityErr = errors.New("session workspace reservation changed")
		}
	}
	if authorityErr == nil {
		authorityErr = forkspace.RequireNoForkExecutionsLocked(sess.Repository, identity)
	}
	unlockFork()
	if authorityErr != nil {
		return sessionReviewIntent{}, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: "session workspace authority is not idle and exact",
		}
	}
	base, err := sessionWorkspaceCommit(sess.Repository, sess.BaseCommit)
	if err != nil {
		return sessionReviewIntent{}, fmt.Errorf("resolve creation base: %w", err)
	}
	source, err := captureSessionReviewSource(sess.Repository, sess.Workspace, sess.ForkName)
	if err != nil {
		if errors.Is(err, errSessionReviewSourceBranch) {
			return sessionReviewIntent{}, &session.Error{
				Code: session.CodeInvalidSessionState,
				Detail: fmt.Sprintf(
					"review requires a clean task workspace checked out on its bound branch %q; ask the agent to restore that branch before retrying",
					sess.ForkName,
				),
			}
		}
		if errors.Is(err, errSessionReviewSourceDirty) ||
			errors.Is(err, errSessionReviewSourceStatusLarge) {
			return sessionReviewIntent{}, &session.Error{
				Code: session.CodeInvalidSessionState,
				Detail: fmt.Sprintf(
					"review requires a clean committed task workspace on bound branch %q; commit or remove workspace changes before retrying",
					sess.ForkName,
				),
			}
		}
		return sessionReviewIntent{}, fmt.Errorf("inspect review source: %w", err)
	}
	if source.Branch != sess.ForkName {
		return sessionReviewIntent{}, &session.Error{
			Code: session.CodeInvalidSessionState,
			Detail: fmt.Sprintf(
				"review source is not checked out on its bound branch %q; ask the agent to restore that branch before retrying",
				sess.ForkName,
			),
		}
	}
	if source.StatusDigest == "" {
		return sessionReviewIntent{}, errors.New("review source status digest is empty")
	}
	ancestor, err := sessionReviewIsAncestor(sess.Workspace, base, source.Head)
	if err != nil {
		return sessionReviewIntent{}, fmt.Errorf("check creation base ancestry: %w", err)
	}
	if !ancestor {
		return sessionReviewIntent{}, errors.New("creation base is not an ancestor of source HEAD")
	}
	if sess.Source != nil && sess.Source.SelectedCommit != base {
		// Whatever source the session was admitted on — branch, pull request or exact commit —
		// the work under review must still contain it. Losing it means the branch was rewritten
		// out from under the admitted source. A default selection needs no second check: its
		// selected commit IS the creation base the ancestry check above just proved.
		admitted, err := sessionWorkspaceCommit(sess.Repository, sess.Source.SelectedCommit)
		if err != nil {
			return sessionReviewIntent{}, fmt.Errorf("resolve admitted source head: %w", err)
		}
		containsSource, err := sessionReviewIsAncestor(sess.Workspace, admitted, source.Head)
		if err != nil {
			return sessionReviewIntent{}, fmt.Errorf("check admitted source ancestry: %w", err)
		}
		if !containsSource {
			return sessionReviewIntent{}, &session.Error{
				Code:   session.CodeInvalidSessionState,
				Detail: "review source no longer contains the admitted source commit; restore the task branch before retrying",
			}
		}
	}
	parentHead, err := s.pinCurrentSessionParent(ctx, sess)
	if err != nil {
		return sessionReviewIntent{}, fmt.Errorf("refresh current parent: %w", err)
	}
	parent, err := captureSessionReviewParent(sess.Repository, parentHead)
	if err != nil {
		return sessionReviewIntent{}, fmt.Errorf("inspect current parent: %w", err)
	}
	origin, err := sessionWorkspaceGitText(sess.Workspace, 4<<10, "remote", "get-url", "origin")
	if err != nil || !sameRealPath(strings.TrimSpace(string(origin)), sess.Repository) {
		if err != nil {
			return sessionReviewIntent{}, fmt.Errorf("verify review source origin: %w", err)
		}
		return sessionReviewIntent{}, errors.New("review source origin is not the bound parent repository")
	}
	return sessionReviewIntent{
		OperationID: operationID, SessionID: sess.ID, SessionRevision: sess.Revision,
		Repository: sess.Repository, Workspace: sess.Workspace,
		ForkGeneration: sess.ForkGeneration,
		CreationBase:   base, SourceHead: source.Head, SourceTree: source.Tree,
		SourceBranch: source.Branch, SourceStatusDigest: source.StatusDigest,
		ParentHead: parent.Head, ParentTree: parent.Tree,
		JobDigest: sess.JobDigest, Source: session.CloneSourceBinding(sess.Source),
		MaxPatchBytes: sess.MaxPatchBytes,
	}, nil
}

func captureSessionReviewSource(repo, workspace, branch string) (sessionReviewSourceIdentity, error) {
	info, err := os.Lstat(workspace)
	if err != nil {
		return sessionReviewSourceIdentity{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return sessionReviewSourceIdentity{}, errors.New("review source is not a real directory")
	}
	head, tree, err := ReviewGitIdentity(workspace, "HEAD")
	if err != nil {
		return sessionReviewSourceIdentity{}, err
	}
	if err := verifySessionSubmodules(context.Background(), workspace, head); err != nil {
		return sessionReviewSourceIdentity{}, fmt.Errorf("%w: %v", errSessionReviewSourceDirty, err)
	}
	gotBranch, err := sessionWorkspaceBranch(workspace)
	if err != nil {
		if errors.Is(err, errSessionWorkspaceDetachedHead) {
			return sessionReviewSourceIdentity{}, fmt.Errorf("%w: %v", errSessionReviewSourceBranch, err)
		}
		return sessionReviewSourceIdentity{}, err
	}
	status, truncated, err := sessionWorkspaceStatusContext(context.Background(), workspace)
	if err != nil {
		return sessionReviewSourceIdentity{}, err
	}
	if truncated {
		return sessionReviewSourceIdentity{}, errSessionReviewSourceStatusLarge
	}
	if len(status) != 0 {
		return sessionReviewSourceIdentity{}, errSessionReviewSourceDirty
	}
	if branch != "" && gotBranch != branch {
		return sessionReviewSourceIdentity{}, fmt.Errorf("%w: got %q, want %q", errSessionReviewSourceBranch, gotBranch, branch)
	}
	return sessionReviewSourceIdentity{Head: head, Tree: tree, Branch: gotBranch, StatusDigest: sessionWorkspaceStatusDigest(status)}, nil
}

func captureSessionReviewParent(repo, revision string) (sessionReviewParentIdentity, error) {
	head, tree, err := ReviewGitIdentity(repo, revision)
	if err != nil {
		return sessionReviewParentIdentity{}, err
	}
	return sessionReviewParentIdentity{Head: head, Tree: tree}, nil
}

func ReviewGitIdentity(dir, revision string) (string, string, error) {
	head, err := sessionWorkspaceCommit(dir, revision)
	if err != nil {
		return "", "", err
	}
	treeRaw, err := sessionWorkspaceGitText(dir, 4<<10, "rev-parse", "--verify", "--end-of-options", head+"^{tree}")
	if err != nil {
		return "", "", fmt.Errorf("resolve tree for %s: %w", head, err)
	}
	tree := strings.TrimSpace(string(treeRaw))
	if !validSessionReviewObject(tree) {
		return "", "", fmt.Errorf("resolve tree for %s: malformed identity %q", head, tree)
	}
	return head, tree, nil
}

func validSessionReviewObject(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func sessionReviewIsAncestor(dir, base, head string) (bool, error) {
	cmd, err := forkspace.GitCommand(context.Background(), dir, "merge-base", "--is-ancestor", base, head)
	if err != nil {
		return false, err
	}
	err = cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

func (s *Service) executeReviewIntent(ctx context.Context, op session.Operation, intent sessionReviewIntent) (ReviewDossier, error) {
	if intent.OperationID != op.ID || intent.SessionID == "" || intent.Repository == "" || intent.Workspace == "" ||
		!forkspace.ValidGeneration(forkspace.Generation(intent.ForkGeneration)) || intent.SessionRevision <= 0 ||
		!validSessionReviewObject(intent.CreationBase) || !validSessionReviewObject(intent.SourceHead) ||
		!validSessionReviewObject(intent.SourceTree) || !validSessionReviewObject(intent.ParentHead) ||
		!validSessionReviewObject(intent.ParentTree) || intent.MaxPatchBytes <= 0 || intent.MaxPatchBytes > session.MaxPatchBytesLimit {
		return ReviewDossier{}, s.makeOperationUncertain(ctx, op, "review operation intent is invalid")
	}
	if intent.Source != nil && session.ValidateSourceBinding(*intent.Source) != nil {
		return ReviewDossier{}, s.makeOperationUncertain(ctx, op, "review source binding is invalid")
	}
	if _, retained, err := s.retainedReviewCandidate(ctx, op.ID); err == nil {
		if retained.SessionID != intent.SessionID || retained.SessionRevision != intent.SessionRevision ||
			retained.JobDigest != intent.JobDigest || retained.ParentHead != intent.ParentHead ||
			retained.ParentTree != intent.ParentTree || retained.SourceHead != intent.SourceHead ||
			retained.SourceTree != intent.SourceTree || retained.CreationBase != intent.CreationBase {
			return ReviewDossier{}, s.makeOperationUncertain(ctx, op, "retained review authority changed")
		}
		return s.completeReview(ctx, retained)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ReviewDossier{}, s.makeOperationUncertain(ctx, op, "retained review custody is invalid")
	}
	bound, err := s.store.GetSession(ctx, intent.SessionID)
	if err != nil || intent.JobDigest == "" || bound.JobDigest != intent.JobDigest ||
		bound.Revision != intent.SessionRevision || bound.Repository != intent.Repository ||
		bound.Workspace != intent.Workspace || bound.ForkName != intent.SourceBranch ||
		bound.ForkGeneration != intent.ForkGeneration {
		return ReviewDossier{}, s.makeOperationUncertain(ctx, op, "review workspace authority changed")
	}
	if err := s.validateSessionForkAuthority(ctx, bound); err != nil {
		return ReviewDossier{}, s.makeOperationUncertain(ctx, op, "review workspace authority changed")
	}
	policy, err := s.sessionExecution(ctx, bound)
	if err != nil {
		return ReviewDossier{}, s.makeOperationUncertain(ctx, op, "review has no valid controller job authority")
	}
	candidate, err := s.prepareForkReviewCandidateFromIntent(ctx, op.ID, intent)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	defer candidate.cleanup()
	dossier := ReviewDossier{
		OperationID: op.ID, SessionID: intent.SessionID, SessionRevision: intent.SessionRevision,
		JobDigest: intent.JobDigest, Source: session.CloneSourceBinding(intent.Source),
		CreationBase: intent.CreationBase,
		SourceHead:   intent.SourceHead, SourceTree: intent.SourceTree,
		ParentHead: intent.ParentHead, ParentTree: intent.ParentTree,
		Rebase: ReviewRebaseClean, Gate: ReviewGateNotRun,
	}
	if candidate.conflict {
		dossier.Rebase = ReviewRebaseConflict
		dossier.NotPublishableReasons = []string{"rebase_conflict"}
		return s.completeReview(ctx, dossier)
	}
	candidateHead, candidateTree, err := freezeReviewCommit(ctx, candidate, op)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("pin review candidate: %w", err))
	}
	if err := materializeSessionSubmodules(ctx, intent.Repository, candidate.dir, candidateHead, intent.Workspace); err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	candidateParentHead, candidateParentTree, err := ReviewGitIdentity(candidate.dir, candidate.base)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("pin review candidate parent: %w", err))
	}
	dossier.ParentHead, dossier.ParentTree = candidateParentHead, candidateParentTree
	dossier.CandidateHead, dossier.CandidateTree = candidateHead, candidateTree
	if candidateParentHead != intent.ParentHead || candidateParentTree != intent.ParentTree {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "parent_moved")
	}
	var gateResult ReviewGateResult
	if s.reviewGate != nil {
		gateEnvironment := maps.Clone(policy.Environment)
		for key, value := range policy.Check.Environment {
			gateEnvironment[key] = value
		}
		gateLog, openErr := s.openReviewGateOutput(op.ID)
		gateResult, err = s.reviewGate.Run(ctx, ReviewGateRequest{
			Repository: intent.Repository, Candidate: candidate.dir, StateRoot: s.stateRoot, OperationID: op.ID,
			SessionID: bound.ID, JobDigest: bound.JobDigest, NetworkMode: string(policy.Egress.Mode),
			NetworkFingerprint: bound.NetworkFingerprint, NetworkQualification: bound.NetworkQualification,
			Command: append([]string(nil), policy.Check.Argv...), Environment: gateEnvironment, Resources: policy.Resources,
			Output: gateLog,
		})
		output := gateLog.finish(gateResult, openErr)
		if err == nil && (gateResult.Configured || gateResult.StartupError != "" || output.Bytes > 0) {
			dossier.GateOutput = output
		} else if path, pathErr := s.reviewGateOutputPath(op.ID); pathErr == nil {
			_ = os.Remove(path) // no gate ran: there is nothing to keep
		}
	}
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("run review gate: %w", err))
	}
	if !ReviewCandidateUnchanged(candidate.dir, candidate.name, candidateHead, candidateTree) {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "gate_modified_candidate")
	}
	switch {
	case gateResult.StartupError != "":
		dossier.Gate = ReviewGateStartupError
		dossier.GateError = SanitizeReviewText(gateResult.StartupError, session.MaxErrorDetailBytes)
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "gate_startup_error")
	case !gateResult.Configured:
		dossier.Gate = ReviewGateNone
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "gate_not_configured")
	case gateResult.Passed:
		dossier.Gate = ReviewGatePassed
	default:
		dossier.Gate = ReviewGateFailed
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "gate_failed")
	}
	dossier.PolicyFindings, err = scanReviewCandidate(ctx, candidate.dir, candidateParentHead, candidateHead)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("scan review candidate: %w", err))
	}
	if len(dossier.PolicyFindings) > 0 {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "policy_findings")
	}
	patch, truncated, err := sessionReviewPatch(candidate.dir, candidateParentHead, candidateHead, intent.MaxPatchBytes)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("compute review candidate patch: %w", err))
	}
	dossier.Patch, dossier.PatchTruncated = patch, truncated
	currentParentMatches := false
	if sess, err := s.store.GetSession(ctx, intent.SessionID); err == nil {
		if currentHead, err := s.pinCurrentSessionParent(ctx, sess); err == nil {
			if current, err := captureSessionReviewParent(intent.Repository, currentHead); err == nil {
				currentParentMatches = current.Head == intent.ParentHead && current.Tree == intent.ParentTree
			}
		}
	}
	if !currentParentMatches {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "parent_moved")
	}
	if current, err := captureSessionReviewSource(intent.Repository, intent.Workspace, intent.SourceBranch); err != nil || current.Head != intent.SourceHead || current.Tree != intent.SourceTree || current.Branch != intent.SourceBranch || current.StatusDigest != intent.SourceStatusDigest {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "source_moved")
	}
	if forkspace.NeedsStop(intent.Repository, intent.SourceBranch) {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "fork_owner_active")
	}
	if candidateTree == candidateParentTree {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "no_changes")
	}
	dossier.NotPublishableReasons = stableSessionReviewReasons(dossier.NotPublishableReasons)
	// Missing/failed gates remain useful review evidence. Only an exact, unchanged,
	// security-clean candidate may be retained for a later controller decision.
	dossier.CandidateRetained = len(dossier.PolicyFindings) == 0
	for _, reason := range dossier.NotPublishableReasons {
		if reason != "gate_not_configured" && reason != "gate_startup_error" && reason != "gate_failed" {
			dossier.CandidateRetained = false
		}
	}
	dossier.Publishable = dossier.CandidateRetained && dossier.Gate == ReviewGatePassed && len(dossier.NotPublishableReasons) == 0
	if dossier.CandidateRetained {
		if err := s.retainReviewCandidate(ctx, candidate.dir, dossier); err != nil {
			return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("retain reviewed candidate: %w", err))
		}
	}
	return s.completeReview(ctx, dossier)
}

func ReviewCandidateUnchanged(dir, branch, head, tree string) bool {
	current, err := captureSessionReviewSource("", dir, branch)
	return err == nil && current.Head == head && current.Tree == tree &&
		current.Branch == branch && current.StatusDigest == sessionWorkspaceStatusDigest(nil)
}

// reviewScratch is a disposable, rebased view of a session's workspace. base remains the parent
// commit the clone captured; name is the candidate branch. The caller owns cleanup whenever dir is
// non-empty. It is a value, not a seam: prepareForkReviewCandidateFromIntent mutates base, name,
// and conflict as it builds the candidate.
//
// forkctl.forkReviewCandidate is its deliberate near-twin — same scaffold, opposite anchor: that
// one PREVIEWS against the parent's current HEAD, this one rebuilds a CAPTURED intent and refuses
// unless every captured head/tree still resolves. Assessed and kept separate; read
// .agent/kb/fork-review-scratch-two-copies.md before merging them.
type reviewScratch struct {
	dir      string
	base     string
	name     string
	conflict bool
}

func (c reviewScratch) cleanup() { _ = os.RemoveAll(c.dir) }

// A CI checkout and a developer's clone name the default branch as origin/<branch>, and gates
// compare against it: emisar's dependency-age check refused to run without it (2026-09-30).
// The candidate is a clone of the staged source, which keeps what it fetched as origin/* that a
// clone never copies. It names the branch at the default commit the job pinned when that commit
// is in the candidate, and otherwise at the trusted parent the review was rebased onto.
func nameReviewDefaultBranch(ctx context.Context, dir string, source *session.SourceBinding, parent string) error {
	if source == nil || !strings.HasPrefix(source.DefaultRef, "refs/heads/") {
		return nil
	}
	commit := source.DefaultCommit
	if !validSessionReviewObject(commit) || forkspace.GitRefCommand(ctx, dir, "cat-file", "-e", commit+"^{commit}").Run() != nil {
		commit = parent
	}
	branch := strings.TrimPrefix(source.DefaultRef, "refs/heads/")
	return forkspace.GitRefCommand(ctx, dir, "update-ref", "refs/remotes/origin/"+branch, commit).Run()
}

func (s *Service) newReviewScratch(ctx context.Context, operationID, repo, commit string) (reviewScratch, error) {
	path, err := s.reviewCandidatePath(operationID)
	if err != nil {
		return reviewScratch{}, err
	}
	staging := filepath.Join(filepath.Dir(path), ".staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		return reviewScratch{}, err
	}
	dir, err := os.MkdirTemp(staging, operationID+"-")
	if err != nil {
		return reviewScratch{}, err
	}
	c := reviewScratch{dir: dir}
	if err := forkspace.GitClonePinnedContext(ctx, repo, dir, commit); err != nil {
		c.cleanup()
		return reviewScratch{}, fmt.Errorf("clone parent into review scratch: %w", err)
	}
	return c, nil
}

func (s *Service) prepareForkReviewCandidateFromIntent(ctx context.Context, operationID string, intent sessionReviewIntent) (c reviewScratch, err error) {
	if !forkspace.ValidExistingName(intent.SourceBranch) {
		return c, errors.New("review intent has an invalid source branch")
	}
	c, err = s.newReviewScratch(ctx, operationID, intent.Repository, intent.ParentHead)
	if err != nil {
		return c, err
	}
	keep := false
	defer func() {
		if !keep {
			c.cleanup()
			c = reviewScratch{}
		}
	}()
	if err := forkspace.PropagateGitIdentityContext(ctx, intent.Repository, c.dir); err != nil {
		return c, fmt.Errorf("prepare session review Git identity: %w", err)
	}
	parentHead, parentTree, err := ReviewGitIdentity(c.dir, intent.ParentHead)
	if err != nil {
		return c, fmt.Errorf("resolve captured review parent: %w", err)
	}
	if parentHead != intent.ParentHead || parentTree != intent.ParentTree {
		return c, errors.New("captured review parent tree is unavailable")
	}
	c.base = intent.ParentHead
	if err := forkspace.GitRefCommand(ctx, c.dir, "update-ref", "--no-deref", "HEAD", c.base).Run(); err != nil {
		return c, fmt.Errorf("detach captured review parent: %w", err)
	}
	// The worker-owned checker hands the gate this commit as COOP_REVIEW_BASE,
	// and reads it from the candidate itself (forkctl.ReviewControllerJob).
	if err := forkspace.GitRefCommand(ctx, c.dir, "update-ref", "refs/coop/session-parent", c.base).Run(); err != nil {
		return c, fmt.Errorf("name captured review parent: %w", err)
	}
	if err := nameReviewDefaultBranch(ctx, c.dir, intent.Source, c.base); err != nil {
		return c, fmt.Errorf("name captured default branch: %w", err)
	}
	if _, _, err := runSessionCompanionGitContext(ctx, c.dir, sessionWorkspaceGitOutputLimit, "reset", "--hard", "--quiet", c.base); err != nil {
		return c, fmt.Errorf("checkout captured review parent: %w", err)
	}
	c.name = intent.SourceBranch
	if err := forkspace.GitFetchPinnedContext(ctx, intent.Workspace, c.dir, intent.SourceHead); err != nil {
		return c, fmt.Errorf("fetch captured review source: %w", err)
	}
	sourceHead, sourceTree, err := ReviewGitIdentity(c.dir, intent.SourceHead)
	if err != nil {
		return c, fmt.Errorf("resolve captured review source: %w", err)
	}
	if sourceHead != intent.SourceHead || sourceTree != intent.SourceTree {
		return c, errors.New("captured review source tree is unavailable")
	}
	creationBase, _, err := ReviewGitIdentity(c.dir, intent.CreationBase)
	if err != nil {
		return c, fmt.Errorf("resolve captured creation base: %w", err)
	}
	creationBaseIsAncestor, err := sessionReviewIsAncestor(c.dir, creationBase, sourceHead)
	if err != nil {
		return c, fmt.Errorf("verify captured creation base: %w", err)
	}
	if !creationBaseIsAncestor {
		return c, errors.New("captured creation base is not an ancestor of the review source")
	}
	// A detached clone may have no logs/refs. Create branch metadata on the real
	// Git directory so a first reflog cannot replace the trusted view's link.
	if err := forkspace.GitRefCommand(ctx, c.dir, "update-ref", "refs/heads/"+c.name, sourceHead).Run(); err != nil {
		return c, fmt.Errorf("create captured review branch: %w", err)
	}
	// Replay exactly the task-local commits captured at session creation. Inferring the upstream
	// from the current parent would also replay rewritten parent history after a force-push/rebase.
	if _, _, err := runSessionCompanionGitContext(ctx, c.dir, sessionWorkspaceGitOutputLimit, "-c", "user.useConfigOnly=true", "rebase", "--onto", c.base, creationBase, c.name); err != nil {
		unmerged, _, inspectErr := runSessionCompanionGitContext(ctx, c.dir, 4096, "ls-files", "--unmerged", "-z")
		_, _, abortErr := runSessionCompanionGitContext(ctx, c.dir, sessionWorkspaceGitOutputLimit, "rebase", "--abort")
		if inspectErr != nil || abortErr != nil || len(unmerged) == 0 {
			return c, fmt.Errorf("rebase captured review scratch failed: %w", errors.Join(err, inspectErr, abortErr))
		}
		c.conflict = true
	}
	keep = true
	return c, nil
}

func sessionReviewPatch(dir, parentHead, candidateHead string, maxBytes int) ([]byte, bool, error) {
	patch, truncated, err := runSessionWorkspaceGit(dir, maxBytes,
		"diff", "--binary", "--no-ext-diff", "--no-textconv", "--ignore-submodules=dirty", "--submodule=short", parentHead, candidateHead, "--")
	if err != nil {
		return nil, false, err
	}
	return patch, truncated, nil
}

func boundedSessionReviewFindings(findings []string) []string {
	if len(findings) == 0 {
		return nil
	}
	result := make([]string, 0, min(len(findings), sessionReviewFindingLimit))
	total := 0
	for _, finding := range findings {
		finding = SanitizeReviewText(finding, sessionReviewFindingBytes)
		if finding == "" || len(result) == sessionReviewFindingLimit || total+len(finding) > sessionReviewFindingTotalBytes {
			break
		}
		result = append(result, finding)
		total += len(finding)
	}
	return result
}

func SanitizeReviewText(value string, maxBytes int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, value)
	value = strings.TrimSpace(value)
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

func stableSessionReviewReasons(reasons []string) []string {
	if len(reasons) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(reasons))
	result := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if reason == "" {
			continue
		}
		if _, ok := seen[reason]; ok {
			continue
		}
		seen[reason] = struct{}{}
		result = append(result, reason)
	}
	sort.Strings(result)
	return result
}

func (s *Service) completeReview(ctx context.Context, dossier ReviewDossier) (ReviewDossier, error) {
	dossier.NotPublishableReasons = stableSessionReviewReasons(dossier.NotPublishableReasons)
	if dossier.Gate != ReviewGatePassed ||
		dossier.Rebase != ReviewRebaseClean ||
		!dossier.CandidateRetained || dossier.CandidateTree == dossier.ParentTree ||
		len(dossier.PolicyFindings) != 0 ||
		len(dossier.NotPublishableReasons) != 0 {
		dossier.Publishable = false
	}
	data, err := json.Marshal(dossier)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, dossier.OperationID, err)
	}
	if len(data) > session.MaxOperationResultBytes {
		return ReviewDossier{}, s.failServiceOperation(ctx, dossier.OperationID, errors.New("review dossier exceeds the bounded operation result"))
	}
	if err := s.store.CompleteOperation(ctx, dossier.OperationID, "review", dossier.SessionID, data); err != nil {
		return ReviewDossier{}, err
	}
	return dossier, nil
}

var _ ReviewGate = ReviewGateFunc(nil)
