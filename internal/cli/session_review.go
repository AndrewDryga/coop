package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/session"
)

const (
	sessionReviewFindingLimit      = 64
	sessionReviewFindingBytes      = 1024
	sessionReviewFindingTotalBytes = 32 << 10
)

type SessionReviewGateStatus string

const (
	SessionReviewGateNone         SessionReviewGateStatus = "none"
	SessionReviewGatePassed       SessionReviewGateStatus = "passed"
	SessionReviewGateFailed       SessionReviewGateStatus = "failed"
	SessionReviewGateStartupError SessionReviewGateStatus = "startup_error"
	SessionReviewGateNotRun       SessionReviewGateStatus = "not_run"
)

type SessionReviewRebaseStatus string

const (
	SessionReviewRebaseClean    SessionReviewRebaseStatus = "clean"
	SessionReviewRebaseConflict SessionReviewRebaseStatus = "conflict"
)

type RunReviewRequest struct {
	SessionID        string `json:"session_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type SessionReviewDossier struct {
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
	Publishable           bool                      `json:"publishable"`
	NotPublishableReasons []string                  `json:"not_publishable_reasons,omitempty"`
}

// SessionReviewGateResult is the complete outcome of the trusted parent gate.
// StartupError is a successful review outcome, not a failed operation.
type SessionReviewGateResult struct {
	Configured   bool
	Passed       bool
	StartupError string
}

// SessionReviewGate is the narrow gate seam used by RunReview. Implementations must only
// inspect treeDir; they must not mutate gateRepo or the candidate.
type SessionReviewGate interface {
	Run(context.Context, string, string) (SessionReviewGateResult, error)
}

type SessionReviewGateFunc func(context.Context, string, string) (SessionReviewGateResult, error)

func (f SessionReviewGateFunc) Run(ctx context.Context, gateRepo, treeDir string) (SessionReviewGateResult, error) {
	return f(ctx, gateRepo, treeDir)
}

type defaultSessionReviewGate struct {
	cfg *config.Config
	rt  runtime.Runtime
}

func (g defaultSessionReviewGate) Run(ctx context.Context, gateRepo, treeDir string) (SessionReviewGateResult, error) {
	if err := ctx.Err(); err != nil {
		return SessionReviewGateResult{}, err
	}
	a := &app{cfg: g.cfg, rt: g.rt, rtSet: g.rt.Name != ""}
	image, err := a.mergeGate(gateRepo)
	if err != nil {
		return SessionReviewGateResult{Configured: true, StartupError: sanitizeSessionReviewText(err.Error(), session.MaxErrorDetailBytes)}, nil
	}
	if image == "" {
		return SessionReviewGateResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return SessionReviewGateResult{}, err
	}
	passed, err := a.reviewGatePasses(gateRepo, treeDir, image)
	if err != nil {
		return SessionReviewGateResult{Configured: true, StartupError: sanitizeSessionReviewText(err.Error(), session.MaxErrorDetailBytes)}, nil
	}
	return SessionReviewGateResult{Configured: true, Passed: passed}, nil
}

type sessionReviewIntent struct {
	OperationID        string `json:"operation_id"`
	SessionID          string `json:"session_id"`
	SessionRevision    int64  `json:"session_revision"`
	Repository         string `json:"repository"`
	Workspace          string `json:"workspace"`
	CreationBase       string `json:"creation_base"`
	SourceHead         string `json:"source_head"`
	SourceTree         string `json:"source_tree"`
	SourceBranch       string `json:"source_branch"`
	SourceStatusDigest string `json:"source_status_digest"`
	ParentHead         string `json:"parent_head"`
	ParentTree         string `json:"parent_tree"`
	PolicyDigest       string `json:"policy_digest"`
	MaxPatchBytes      int    `json:"max_patch_bytes"`
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

func (s *SessionService) RunReview(ctx context.Context, key string, req RunReviewRequest) (SessionReviewDossier, error) {
	unlock := s.lockOperation(key)
	defer unlock()

	if req.SessionID == "" || len(req.SessionID) > session.MaxIDBytes || !utf8SessionText(req.SessionID) || req.ExpectedRevision <= 0 {
		return SessionReviewDossier{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "session id and positive expected revision are required"}
	}
	op, replay, err := s.store.ReserveOperation(ctx, "RunReview", key, req)
	if err != nil {
		return SessionReviewDossier{}, err
	}
	if replay {
		switch op.State {
		case session.OperationSucceeded:
			return decodeSessionReviewDossier(op.Result)
		case session.OperationFailed:
			return SessionReviewDossier{}, &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail}
		case session.OperationRunning:
			if err := s.store.MarkOperationUncertain(ctx, op.ID); err != nil {
				return SessionReviewDossier{}, err
			}
			return SessionReviewDossier{}, session.ErrOperationUncertain
		case session.OperationUncertain:
			return SessionReviewDossier{}, session.ErrOperationUncertain
		default:
			return s.executeReview(ctx, op, req)
		}
	}
	return s.executeReview(ctx, op, req)
}

func decodeSessionReviewDossier(data []byte) (SessionReviewDossier, error) {
	var dossier SessionReviewDossier
	if err := json.Unmarshal(data, &dossier); err != nil {
		return SessionReviewDossier{}, fmt.Errorf("decode review operation result: %w", err)
	}
	if dossier.OperationID == "" || dossier.SessionID == "" {
		return SessionReviewDossier{}, errors.New("decode review operation result: missing identity")
	}
	return dossier, nil
}

func (s *SessionService) executeReview(ctx context.Context, op session.Operation, req RunReviewRequest) (SessionReviewDossier, error) {
	intent, err := s.captureReviewIntent(ctx, op.ID, req)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.store.MarkOperationRunning(ctx, op.ID, data); err != nil {
		return SessionReviewDossier{}, err
	}
	return s.executeReviewIntent(ctx, op, intent)
}

func (s *SessionService) captureReviewIntent(ctx context.Context, operationID string, req RunReviewRequest) (sessionReviewIntent, error) {
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return sessionReviewIntent{}, err
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
	if !validExistingForkName(sess.ForkName) || sess.Repository == "" || sess.Workspace == "" || sess.Workspace != forkWorkspace(sess.Repository, sess.ForkName) {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeInvalidRequest, Detail: "session workspace is not its exact bound fork"}
	}
	if forkNeedsStop(sess.Repository, sess.ForkName) {
		return sessionReviewIntent{}, &session.Error{Code: session.CodeInvalidSessionState, Detail: "fork is running or cleanup-pending"}
	}
	base, err := sessionWorkspaceCommit(sess.Repository, sess.BaseCommit)
	if err != nil {
		return sessionReviewIntent{}, fmt.Errorf("resolve creation base: %w", err)
	}
	source, err := captureSessionReviewSource(sess.Repository, sess.Workspace, sess.ForkName)
	if err != nil {
		return sessionReviewIntent{}, fmt.Errorf("inspect review source: %w", err)
	}
	if source.Branch != sess.ForkName {
		return sessionReviewIntent{}, errors.New("review source is not checked out on its bound branch")
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
	parent, err := captureSessionReviewParent(sess.Repository)
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
		CreationBase: base, SourceHead: source.Head, SourceTree: source.Tree,
		SourceBranch: source.Branch, SourceStatusDigest: source.StatusDigest,
		ParentHead: parent.Head, ParentTree: parent.Tree,
		PolicyDigest: sess.PolicyDigest, MaxPatchBytes: sess.MaxPatchBytes,
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
	head, tree, err := sessionReviewGitIdentity(workspace, "HEAD")
	if err != nil {
		return sessionReviewSourceIdentity{}, err
	}
	gotBranch, err := sessionWorkspaceBranch(workspace)
	if err != nil {
		return sessionReviewSourceIdentity{}, err
	}
	status, truncated, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "-z")
	if err != nil {
		return sessionReviewSourceIdentity{}, err
	}
	if truncated {
		return sessionReviewSourceIdentity{}, errors.New("review source status exceeds the bounded limit")
	}
	if len(status) != 0 {
		return sessionReviewSourceIdentity{}, errors.New("review source is not clean")
	}
	if branch != "" && gotBranch != branch {
		return sessionReviewSourceIdentity{}, fmt.Errorf("review source branch is %q, want %q", gotBranch, branch)
	}
	return sessionReviewSourceIdentity{Head: head, Tree: tree, Branch: gotBranch, StatusDigest: sessionWorkspaceStatusDigest(status)}, nil
}

func captureSessionReviewParent(repo string) (sessionReviewParentIdentity, error) {
	head, tree, err := sessionReviewGitIdentity(repo, "HEAD")
	if err != nil {
		return sessionReviewParentIdentity{}, err
	}
	return sessionReviewParentIdentity{Head: head, Tree: tree}, nil
}

func sessionReviewGitIdentity(dir, revision string) (string, string, error) {
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
	cmd := exec.Command("git", gitArgs(dir, []string{"merge-base", "--is-ancestor", base, head})...)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

func (s *SessionService) executeReviewIntent(ctx context.Context, op session.Operation, intent sessionReviewIntent) (SessionReviewDossier, error) {
	if intent.OperationID != op.ID || intent.SessionID == "" || intent.Repository == "" || intent.Workspace == "" || intent.SessionRevision <= 0 || !validSessionReviewObject(intent.CreationBase) || !validSessionReviewObject(intent.SourceHead) || !validSessionReviewObject(intent.SourceTree) || !validSessionReviewObject(intent.ParentHead) || !validSessionReviewObject(intent.ParentTree) || intent.MaxPatchBytes <= 0 || intent.MaxPatchBytes > session.MaxPatchBytesLimit {
		return SessionReviewDossier{}, &session.Error{Code: session.CodeOperationUncertain, Detail: "review operation intent is invalid"}
	}
	candidate, err := prepareForkReviewCandidateFromIntent(intent)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	defer candidate.cleanup()
	dossier := SessionReviewDossier{
		OperationID: op.ID, SessionID: intent.SessionID, SessionRevision: intent.SessionRevision,
		PolicyDigest: intent.PolicyDigest, CreationBase: intent.CreationBase,
		SourceHead: intent.SourceHead, SourceTree: intent.SourceTree,
		ParentHead: intent.ParentHead, ParentTree: intent.ParentTree,
		Rebase: SessionReviewRebaseClean, Gate: SessionReviewGateNotRun,
	}
	if candidate.conflict {
		dossier.Rebase = SessionReviewRebaseConflict
		dossier.NotPublishableReasons = []string{"rebase_conflict"}
		return s.completeReview(ctx, dossier)
	}
	candidateHead, candidateTree, err := sessionReviewGitIdentity(candidate.dir, candidate.name)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("pin review candidate: %w", err))
	}
	candidateParentHead, candidateParentTree, err := sessionReviewGitIdentity(candidate.dir, candidate.base)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("pin review candidate parent: %w", err))
	}
	dossier.ParentHead, dossier.ParentTree = candidateParentHead, candidateParentTree
	dossier.CandidateHead, dossier.CandidateTree = candidateHead, candidateTree
	if candidateParentHead != intent.ParentHead || candidateParentTree != intent.ParentTree {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "parent_moved")
	}
	gateResult, err := s.reviewGate.Run(ctx, intent.Repository, candidate.dir)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("run review gate: %w", err))
	}
	switch {
	case gateResult.StartupError != "":
		dossier.Gate = SessionReviewGateStartupError
		dossier.GateError = sanitizeSessionReviewText(gateResult.StartupError, session.MaxErrorDetailBytes)
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "gate_startup_error")
	case !gateResult.Configured:
		dossier.Gate = SessionReviewGateNone
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "gate_not_configured")
	case gateResult.Passed:
		dossier.Gate = SessionReviewGatePassed
	default:
		dossier.Gate = SessionReviewGateFailed
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "gate_failed")
	}
	if err := candidate.detachBase(); err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("detach review candidate: %w", err))
	}
	dossier.PolicyFindings = boundedSessionReviewFindings(policyScan(candidate.dir, candidate.name))
	if len(dossier.PolicyFindings) > 0 {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "policy_findings")
	}
	patch, truncated, err := sessionReviewPatch(candidate.dir, candidateParentHead, candidateHead, intent.MaxPatchBytes)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("compute review candidate patch: %w", err))
	}
	dossier.Patch, dossier.PatchTruncated = patch, truncated
	if truncated {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "patch_truncated")
	}
	if current, err := captureSessionReviewParent(intent.Repository); err != nil || current.Head != intent.ParentHead || current.Tree != intent.ParentTree {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "parent_moved")
	}
	if current, err := captureSessionReviewSource(intent.Repository, intent.Workspace, intent.SourceBranch); err != nil || current.Head != intent.SourceHead || current.Tree != intent.SourceTree || current.Branch != intent.SourceBranch || current.StatusDigest != intent.SourceStatusDigest {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "source_moved")
	}
	if forkNeedsStop(intent.Repository, intent.SourceBranch) {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "fork_owner_active")
	}
	dossier.NotPublishableReasons = stableSessionReviewReasons(dossier.NotPublishableReasons)
	dossier.Publishable = dossier.Rebase == SessionReviewRebaseClean && dossier.Gate == SessionReviewGatePassed && !dossier.PatchTruncated && len(dossier.PolicyFindings) == 0 && len(dossier.NotPublishableReasons) == 0
	return s.completeReview(ctx, dossier)
}

func prepareForkReviewCandidateFromIntent(intent sessionReviewIntent) (forkReviewCandidate, error) {
	c, err := newForkReviewScratch(intent.Repository)
	if err != nil {
		return c, err
	}
	keep := false
	defer func() {
		if !keep {
			c.cleanup()
		}
	}()
	if !validExistingForkName(intent.SourceBranch) {
		return c, errors.New("review intent has an invalid source branch")
	}
	propagateGitIdentity(intent.Repository, c.dir)
	const capturedParentRef = "refs/coop/session-parent"
	if err := gitRun(c.dir, "fetch", "--quiet", intent.Repository, "+"+intent.ParentHead+":"+capturedParentRef); err != nil {
		return c, fmt.Errorf("fetch captured review parent: %w", err)
	}
	parentHead, parentTree, err := sessionReviewGitIdentity(c.dir, intent.ParentHead)
	if err != nil {
		return c, fmt.Errorf("resolve captured review parent: %w", err)
	}
	if parentHead != intent.ParentHead || parentTree != intent.ParentTree {
		return c, errors.New("captured review parent tree is unavailable")
	}
	c.base = intent.ParentHead
	if err := c.detachBase(); err != nil {
		return c, fmt.Errorf("detach captured review parent: %w", err)
	}
	c.name = intent.SourceBranch
	if err := gitRun(c.dir, "fetch", "--quiet", intent.Workspace, "+"+intent.SourceHead+":refs/heads/"+intent.SourceBranch); err != nil {
		return c, fmt.Errorf("fetch captured review source: %w", err)
	}
	sourceHead, sourceTree, err := sessionReviewGitIdentity(c.dir, intent.SourceHead)
	if err != nil {
		return c, fmt.Errorf("resolve captured review source: %w", err)
	}
	if sourceHead != intent.SourceHead || sourceTree != intent.SourceTree {
		return c, errors.New("captured review source tree is unavailable")
	}
	if err := gitRun(c.dir, "rebase", c.base, c.name); err != nil {
		if abortErr := gitRun(c.dir, "rebase", "--abort"); abortErr != nil {
			return c, fmt.Errorf("rebase captured review scratch failed and abort failed: %v; %w", err, abortErr)
		}
		c.conflict = true
	}
	keep = true
	return c, nil
}

func sessionReviewPatch(dir, parentHead, candidateHead string, maxBytes int) ([]byte, bool, error) {
	patch, truncated, err := runSessionWorkspaceGit(dir, maxBytes,
		"diff", "--binary", "--no-ext-diff", "--no-textconv", parentHead, candidateHead, "--")
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
		finding = sanitizeSessionReviewText(finding, sessionReviewFindingBytes)
		if finding == "" || len(result) == sessionReviewFindingLimit || total+len(finding) > sessionReviewFindingTotalBytes {
			break
		}
		result = append(result, finding)
		total += len(finding)
	}
	return result
}

func sanitizeSessionReviewText(value string, maxBytes int) string {
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

func (s *SessionService) completeReview(ctx context.Context, dossier SessionReviewDossier) (SessionReviewDossier, error) {
	dossier.NotPublishableReasons = stableSessionReviewReasons(dossier.NotPublishableReasons)
	if dossier.Gate != SessionReviewGatePassed || dossier.Rebase != SessionReviewRebaseClean || dossier.PatchTruncated || len(dossier.PolicyFindings) != 0 || len(dossier.NotPublishableReasons) != 0 {
		dossier.Publishable = false
	}
	data, err := json.Marshal(dossier)
	if err != nil {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, dossier.OperationID, err)
	}
	if len(data) > session.MaxOperationResultBytes {
		return SessionReviewDossier{}, s.failServiceOperation(ctx, dossier.OperationID, errors.New("review dossier exceeds the bounded operation result"))
	}
	if err := s.store.CompleteOperation(ctx, dossier.OperationID, "review", dossier.SessionID, data); err != nil {
		return SessionReviewDossier{}, err
	}
	return dossier, nil
}

var _ SessionReviewGate = SessionReviewGateFunc(nil)
