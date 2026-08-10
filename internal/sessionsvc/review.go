package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

const (
	sessionReviewFindingLimit      = 64
	sessionReviewFindingBytes      = 1024
	sessionReviewFindingTotalBytes = 32 << 10
	sessionReviewArtifactMaxBytes  = 64 << 20
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
	OperationID           string             `json:"operation_id"`
	SessionID             string             `json:"session_id"`
	SessionRevision       int64              `json:"session_revision"`
	PolicyDigest          string             `json:"policy_digest"`
	CreationBase          string             `json:"creation_base"`
	SourceHead            string             `json:"source_head"`
	SourceTree            string             `json:"source_tree"`
	ParentHead            string             `json:"parent_head"`
	ParentTree            string             `json:"parent_tree"`
	CandidateHead         string             `json:"candidate_head"`
	CandidateTree         string             `json:"candidate_tree"`
	Rebase                ReviewRebaseStatus `json:"rebase"`
	Gate                  ReviewGateStatus   `json:"gate"`
	GateError             string             `json:"gate_error,omitempty"`
	PolicyFindings        []string           `json:"policy_findings,omitempty"`
	Patch                 []byte             `json:"patch,omitempty"`
	PatchTruncated        bool               `json:"patch_truncated"`
	PatchArtifactID       string             `json:"patch_artifact_id,omitempty"`
	PatchDigest           string             `json:"patch_digest,omitempty"`
	PatchBytes            int64              `json:"patch_bytes"`
	Publishable           bool               `json:"publishable"`
	NotPublishableReasons []string           `json:"not_publishable_reasons,omitempty"`
}

// ReviewGateResult is the complete outcome of the trusted parent gate.
// StartupError is a successful review outcome, not a failed operation.
type ReviewGateResult struct {
	Configured   bool
	Passed       bool
	StartupError string
}

// ReviewGate is the narrow gate seam used by RunReview. Implementations must not mutate
// gateRepo. A gate may create ignored build output in the disposable candidate; RunReview rejects
// any change to its pinned commit, tree, branch, tracked files, or non-ignored untracked files.
type ReviewGate interface {
	Run(context.Context, string, string) (ReviewGateResult, error)
}

type ReviewGateFunc func(context.Context, string, string) (ReviewGateResult, error)

func (f ReviewGateFunc) Run(ctx context.Context, gateRepo, treeDir string) (ReviewGateResult, error) {
	return f(ctx, gateRepo, treeDir)
}

// MaxReviewErrorBytes bounds a gate's startup-error prose, so a host implementing
// ReviewGate truncates the same way the service's own paths do.
const MaxReviewErrorBytes = session.MaxErrorDetailBytes

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
			return decodeSessionReviewDossier(op.Result)
		case session.OperationFailed:
			return ReviewDossier{}, &session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail}
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

func (s *Service) resumeReview(
	ctx context.Context,
	op session.Operation,
) (ReviewDossier, error) {
	var intent sessionReviewIntent
	if err := json.Unmarshal(op.Result, &intent); err != nil {
		if op.State == session.OperationRunning {
			_ = s.store.MarkOperationUncertain(ctx, op.ID)
		}
		return ReviewDossier{}, &session.Error{
			Code: session.CodeOperationUncertain, Detail: "review operation intent is unreadable",
		}
	}
	dossier, err := s.executeReviewIntent(s.reviewExecutionContext(), op, intent)
	if err != nil && session.CodeOf(err) == session.CodeOperationUncertain &&
		op.State == session.OperationRunning {
		_ = s.store.MarkOperationUncertain(ctx, op.ID)
	}
	return dossier, err
}

func (s *Service) executeReview(ctx context.Context, op session.Operation, req RunReviewRequest) (ReviewDossier, error) {
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
	head, tree, err := ReviewGitIdentity(workspace, "HEAD")
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

func (s *Service) executeReviewIntent(ctx context.Context, op session.Operation, intent sessionReviewIntent) (ReviewDossier, error) {
	if intent.OperationID != op.ID || intent.SessionID == "" || intent.Repository == "" || intent.Workspace == "" || intent.SessionRevision <= 0 || !validSessionReviewObject(intent.CreationBase) || !validSessionReviewObject(intent.SourceHead) || !validSessionReviewObject(intent.SourceTree) || !validSessionReviewObject(intent.ParentHead) || !validSessionReviewObject(intent.ParentTree) || intent.MaxPatchBytes <= 0 || intent.MaxPatchBytes > session.MaxPatchBytesLimit {
		return ReviewDossier{}, &session.Error{Code: session.CodeOperationUncertain, Detail: "review operation intent is invalid"}
	}
	candidate, err := prepareForkReviewCandidateFromIntent(intent)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, err)
	}
	defer candidate.cleanup()
	dossier := ReviewDossier{
		OperationID: op.ID, SessionID: intent.SessionID, SessionRevision: intent.SessionRevision,
		PolicyDigest: intent.PolicyDigest, CreationBase: intent.CreationBase,
		SourceHead: intent.SourceHead, SourceTree: intent.SourceTree,
		ParentHead: intent.ParentHead, ParentTree: intent.ParentTree,
		Rebase: ReviewRebaseClean, Gate: ReviewGateNotRun,
	}
	if candidate.conflict {
		dossier.Rebase = ReviewRebaseConflict
		dossier.NotPublishableReasons = []string{"rebase_conflict"}
		return s.completeReview(ctx, dossier)
	}
	candidateHead, candidateTree, err := ReviewGitIdentity(candidate.dir, candidate.name)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("pin review candidate: %w", err))
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
	gateResult, err := s.reviewGate.Run(ctx, intent.Repository, candidate.dir)
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
	dossier.PolicyFindings = boundedSessionReviewFindings(s.host.policyScan(candidate.dir, candidateHead))
	if len(dossier.PolicyFindings) > 0 {
		dossier.NotPublishableReasons = append(dossier.NotPublishableReasons, "policy_findings")
	}
	patch, truncated, err := sessionReviewPatch(candidate.dir, candidateParentHead, candidateHead, intent.MaxPatchBytes)
	if err != nil {
		return ReviewDossier{}, s.failServiceOperation(ctx, op.ID, fmt.Errorf("compute review candidate patch: %w", err))
	}
	dossier.Patch, dossier.PatchTruncated = patch, truncated
	artifact, err := s.writeReviewPatchArtifact(
		candidate.dir,
		candidateParentHead,
		candidateHead,
		op.ID,
	)
	if err != nil {
		dossier.NotPublishableReasons = append(
			dossier.NotPublishableReasons,
			"patch_artifact_unavailable",
		)
	} else {
		dossier.PatchArtifactID = op.ID
		dossier.PatchDigest = artifact.Digest
		dossier.PatchBytes = artifact.Bytes
	}
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
	dossier.NotPublishableReasons = stableSessionReviewReasons(dossier.NotPublishableReasons)
	dossier.Publishable = dossier.Rebase == ReviewRebaseClean &&
		dossier.Gate == ReviewGatePassed &&
		dossier.PatchArtifactID != "" &&
		dossier.PatchDigest != "" &&
		dossier.PatchBytes > 0 &&
		len(dossier.PolicyFindings) == 0 &&
		len(dossier.NotPublishableReasons) == 0
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

func (c reviewScratch) detachBase() error {
	return gitRun(c.dir, "checkout", "--quiet", "--detach", c.base)
}

func newReviewScratch(repo string) (reviewScratch, error) {
	dir, err := os.MkdirTemp("", "coop-fork-review-")
	if err != nil {
		return reviewScratch{}, err
	}
	c := reviewScratch{dir: dir}
	if err := forkspace.GitClone(repo, dir); err != nil {
		c.cleanup()
		return reviewScratch{}, fmt.Errorf("clone parent into review scratch: %w", err)
	}
	return c, nil
}

func prepareForkReviewCandidateFromIntent(intent sessionReviewIntent) (reviewScratch, error) {
	c, err := newReviewScratch(intent.Repository)
	if err != nil {
		return c, err
	}
	keep := false
	defer func() {
		if !keep {
			c.cleanup()
		}
	}()
	if !forkspace.ValidExistingName(intent.SourceBranch) {
		return c, errors.New("review intent has an invalid source branch")
	}
	forkspace.PropagateGitIdentity(intent.Repository, c.dir)
	const capturedParentRef = "refs/coop/session-parent"
	if err := gitRun(c.dir, "fetch", "--quiet", intent.Repository, "+"+intent.ParentHead+":"+capturedParentRef); err != nil {
		return c, fmt.Errorf("fetch captured review parent: %w", err)
	}
	parentHead, parentTree, err := ReviewGitIdentity(c.dir, intent.ParentHead)
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
	// Replay exactly the task-local commits captured at session creation. Inferring the upstream
	// from the current parent would also replay rewritten parent history after a force-push/rebase.
	if err := gitRun(c.dir, "rebase", "--onto", c.base, creationBase, c.name); err != nil {
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

type sessionReviewPatchArtifact struct {
	Digest string
	Bytes  int64
}

type sessionReviewArtifactWriter struct {
	file     *os.File
	digest   hashWriter
	written  int64
	maxBytes int64
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

func (w *sessionReviewArtifactWriter) Write(p []byte) (int, error) {
	if w.written+int64(len(p)) > w.maxBytes {
		return 0, fmt.Errorf(
			"review patch exceeds %d bytes",
			w.maxBytes,
		)
	}
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.digest.Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

func (s *Service) writeReviewPatchArtifact(
	dir string,
	parentHead string,
	candidateHead string,
	operationID string,
) (sessionReviewPatchArtifact, error) {
	var artifact sessionReviewPatchArtifact
	if s.stateRoot == "" || !validSessionPathComponent(operationID) {
		return artifact, errors.New("review artifact identity is invalid")
	}
	root := filepath.Join(s.stateRoot, "review-artifacts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return artifact, fmt.Errorf("create review artifact root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return artifact, fmt.Errorf("protect review artifact root: %w", err)
	}
	file, err := os.CreateTemp(root, "."+operationID+"-*.tmp")
	if err != nil {
		return artifact, fmt.Errorf("create review patch artifact: %w", err)
	}
	temp := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temp)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return artifact, fmt.Errorf("protect review patch artifact: %w", err)
	}
	writer := &sessionReviewArtifactWriter{
		file: file, digest: sha256.New(), maxBytes: sessionReviewArtifactMaxBytes,
	}
	stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	args := []string{
		"diff", "--binary", "--no-ext-diff", "--no-textconv",
		parentHead, candidateHead, "--",
	}
	cmd := exec.Command("git", gitArgs(dir, args)...)
	cmd.Stdout = writer
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.buf.String())
		if detail != "" {
			return artifact, fmt.Errorf("write review patch artifact: %w: %s", err, detail)
		}
		return artifact, fmt.Errorf("write review patch artifact: %w", err)
	}
	if writer.written == 0 {
		return artifact, errors.New("review patch artifact is empty")
	}
	if err := file.Sync(); err != nil {
		return artifact, fmt.Errorf("sync review patch artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		return artifact, fmt.Errorf("close review patch artifact: %w", err)
	}
	target := filepath.Join(root, operationID+".diff")
	if err := os.Rename(temp, target); err != nil {
		return artifact, fmt.Errorf("publish review patch artifact: %w", err)
	}
	keep = true
	return sessionReviewPatchArtifact{
		Digest: fmt.Sprintf("%x", writer.digest.Sum(nil)),
		Bytes:  writer.written,
	}, nil
}

func (s *Service) OpenReviewPatch(
	ctx context.Context,
	operationID string,
) (*os.File, ReviewDossier, error) {
	var dossier ReviewDossier
	if !validSessionPathComponent(operationID) {
		return nil, dossier, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "invalid review operation id",
		}
	}
	op, err := s.store.GetOperationByID(ctx, operationID)
	if err != nil {
		return nil, dossier, err
	}
	if op.Method != "RunReview" || op.State != session.OperationSucceeded {
		return nil, dossier, &session.Error{
			Code:   session.CodeInvalidRequest,
			Detail: "operation is not a completed review",
		}
	}
	dossier, err = decodeSessionReviewDossier(op.Result)
	if err != nil {
		return nil, dossier, err
	}
	if dossier.OperationID != operationID ||
		dossier.PatchArtifactID != operationID ||
		dossier.PatchDigest == "" ||
		dossier.PatchBytes < 1 ||
		dossier.PatchBytes > sessionReviewArtifactMaxBytes {
		return nil, dossier, errors.New("review patch artifact metadata is invalid")
	}
	path := filepath.Join(s.stateRoot, "review-artifacts", operationID+".diff")
	lstat, err := os.Lstat(path)
	if err != nil {
		return nil, dossier, fmt.Errorf("inspect review patch artifact path: %w", err)
	}
	if lstat.Mode()&os.ModeSymlink != 0 || !lstat.Mode().IsRegular() {
		return nil, dossier, errors.New("review patch artifact path is unsafe")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, dossier, fmt.Errorf("open review patch artifact: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, dossier, errors.New("open review patch artifact returned no file")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, dossier, fmt.Errorf("inspect review patch artifact: %w", err)
	}
	owner, ownerOK := sessionFileOwner(info)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		!ownerOK || (owner != uint64(os.Geteuid()) && owner != 0) ||
		info.Size() != dossier.PatchBytes {
		_ = file.Close()
		return nil, dossier, errors.New("review patch artifact does not match its metadata")
	}
	return file, dossier, nil
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
		dossier.PatchArtifactID == "" ||
		dossier.PatchDigest == "" ||
		dossier.PatchBytes < 1 ||
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
