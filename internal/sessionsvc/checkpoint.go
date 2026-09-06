package sessionsvc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const workspaceCheckpointPrivateMagic = "COOP-CHECKPOINT-1\n"

type CheckpointWorkspaceRequest struct {
	SessionID           string `json:"session_id"`
	SessionRef          string `json:"session_ref"`
	ExpectedRevision    int64  `json:"expected_revision"`
	PlacementGeneration int    `json:"placement_generation"`
	RepositoryRef       string `json:"repository_ref"`
}

type CheckpointWorkspaceResult struct {
	OperationID string                          `json:"operation_id"`
	Checkpoint  workerproto.WorkspaceCheckpoint `json:"checkpoint"`
}

type RestoreWorkspaceCheckpointRequest struct {
	SessionID        string                          `json:"session_id"`
	ExpectedRevision int64                           `json:"expected_revision"`
	Checkpoint       workerproto.WorkspaceCheckpoint `json:"checkpoint"`
	Bundle           []byte                          `json:"-"`
}

type checkpointBlob struct {
	path  []byte
	entry workerproto.WorkspaceCheckpointFileEntry
}

type checkpointPrivateArtifact struct {
	descriptor []byte
	bundle     []byte
}

func (s *Service) CheckpointWorkspace(
	ctx context.Context,
	key string,
	req CheckpointWorkspaceRequest,
) (CheckpointWorkspaceResult, error) {
	unlock := s.lockOperation(key)
	defer unlock()
	op, replay, err := s.store.ReserveOperation(ctx, "CheckpointWorkspace", key, req)
	if err != nil {
		return CheckpointWorkspaceResult{}, err
	}
	if replay && op.State == session.OperationSucceeded {
		return replayCheckpointWorkspace(op)
	}
	if replay && op.State == session.OperationFailed {
		return CheckpointWorkspaceResult{}, wrapServiceOperationError(op.ID,
			&session.Error{Code: op.ErrorCode, Detail: op.ErrorDetail})
	}
	return s.executeCheckpointWorkspace(ctx, op, req)
}

// RestoreWorkspaceCheckpoint reconstructs one verified portable checkpoint into an unused
// writable replacement session. The checkpoint digest is part of the journaled request while the
// bounded binary bundle travels only on the private request body; exact replay is safe after a
// crash at any point before the operation receipt is completed.
func (s *Service) RestoreWorkspaceCheckpoint(
	ctx context.Context,
	key string,
	req RestoreWorkspaceCheckpointRequest,
) (session.Session, error) {
	unlock := s.lockOperation(key)
	defer unlock()
	op, replay, err := s.store.ReserveOperation(ctx, "RestoreWorkspaceCheckpoint", key, req)
	if err != nil {
		return session.Session{}, err
	}
	if replay && op.State != session.OperationReserved && op.State != session.OperationRunning {
		return replaySessionOperation(op)
	}
	return s.executeRestoreWorkspaceCheckpoint(ctx, op, req)
}

func (s *Service) executeRestoreWorkspaceCheckpoint(
	ctx context.Context,
	op session.Operation,
	req RestoreWorkspaceCheckpointRequest,
) (session.Session, error) {
	if req.SessionID == "" || req.ExpectedRevision <= 0 {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "restore session and revision are required",
		})
	}
	manifest, err := workerproto.ValidateWorkspaceCheckpointBundle(req.Checkpoint, req.Bundle)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: err.Error(),
		})
	}
	members, err := workspaceCheckpointMembers(req.Bundle)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	intent, _ := json.Marshal(req)
	if op.State == session.OperationReserved {
		if err := s.store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
			return session.Session{}, err
		}
	}
	// Hold the session's runtime from validation through the binding: a turn cannot start on a
	// workspace mid-rewrite, and one cannot be queued into that window either.
	release, ok := s.beginWorkspaceRestore(req.SessionID)
	if !ok {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code:   session.CodeInvalidSessionState,
			Detail: "restore requires a parked session; a turn or another runtime operation is in progress",
		})
	}
	defer release()
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := validateRestoreWorkspaceSession(ctx, sess, req); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if s.testDuringRestoreFiles != nil {
		s.testDuringRestoreFiles()
	}
	if err := restoreWorkspaceCheckpointFiles(sess.Workspace, req.Checkpoint, manifest, members); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: err.Error(),
		})
	}
	recovered, err := tasks.ReadControllerTaskBinding(sess.Workspace, req.Checkpoint.Task.ID)
	if err != nil || recovered.QueueID != req.Checkpoint.Task.QueueID ||
		recovered.TaskID != req.Checkpoint.Task.TaskID || recovered.ID != req.Checkpoint.Task.ID ||
		recovered.OfferRef != sess.ExternalRef {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: "restored workspace task authority does not match",
		})
	}
	binding := session.WorkspaceTaskBinding{
		QueueID: recovered.QueueID, TaskID: recovered.TaskID, ID: recovered.ID,
		OfferRef: recovered.OfferRef, DraftSHA256: recovered.DraftSHA256,
	}
	verification := sess
	verification.BaseCommit = req.Checkpoint.BaseRevision
	verification.WorkspaceTask = &binding
	if err := verifyRestoredWorkspaceCheckpoint(verification, req.Checkpoint, manifest, members); err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: err.Error(),
		})
	}
	bound, err := s.store.RestoreWorkspaceTask(
		ctx, req.SessionID, req.ExpectedRevision, req.Checkpoint.BaseRevision, binding,
	)
	if err != nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
	}
	result, err := json.Marshal(bound)
	if err != nil {
		return session.Session{}, err
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "session", bound.ID, result); err != nil {
		return session.Session{}, err
	}
	return bound, nil
}

func validateRestoreWorkspaceSession(
	ctx context.Context,
	sess session.Session,
	req RestoreWorkspaceCheckpointRequest,
) error {
	if err := validateSessionForkAuthority(ctx, sess); err != nil {
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if sess.State != session.SessionOpen || sess.RepositoryReadOnly ||
		sess.Activity != session.ActivityParked || sess.ActiveTurnID != "" ||
		sess.QueuedTurnCount != 0 || sess.TurnsUsed != 0 {
		return &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "restore requires an unused writable open session"}
	}
	if sess.WorkspaceTask == nil && sess.Revision != req.ExpectedRevision {
		return &session.Error{Code: session.CodeRevisionConflict, Detail: "session revision changed"}
	}
	if bound := sess.WorkspaceTask; bound != nil {
		// A bound session takes only its own checkpoint again (an exact re-restore). Anything else
		// is refused here, before reset --hard and clean would replace the files it already holds:
		// the store's own refusal comes after the workspace is rewritten, so it alone would leave the
		// files from one checkpoint under a session record bound to another.
		task := req.Checkpoint.Task
		if task.QueueID != bound.QueueID || task.TaskID != bound.TaskID || task.ID != bound.ID ||
			req.Checkpoint.BaseRevision != sess.BaseCommit {
			return &session.Error{Code: session.CodeInvalidSessionState,
				Detail: "session is already bound to another restored workspace task"}
		}
	}
	base, err := sessionWorkspaceCommit(sess.Repository, req.Checkpoint.BaseRevision)
	if err != nil || base != req.Checkpoint.BaseRevision {
		return &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "checkpoint base revision is unavailable in the authorized repository"}
	}
	return nil
}

func workspaceCheckpointMembers(bundle []byte) (map[string][]byte, error) {
	reader := tar.NewReader(bytes.NewReader(bundle))
	members := make(map[string][]byte)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(reader, workerproto.MaxWorkspaceCheckpointBundleBytes+1))
		if err != nil || len(body) > workerproto.MaxWorkspaceCheckpointBundleBytes {
			return nil, errors.New("workspace checkpoint member exceeds its bound")
		}
		members[header.Name] = body
	}
	return members, nil
}

func restoreWorkspaceCheckpointFiles(
	workspace string,
	checkpoint workerproto.WorkspaceCheckpoint,
	manifest workerproto.WorkspaceCheckpointBundleManifest,
	members map[string][]byte,
) error {
	if _, _, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"reset", "--hard", checkpoint.BaseRevision); err != nil {
		return fmt.Errorf("reset replacement workspace: %w", err)
	}
	if _, _, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit, "clean", "-qfdx"); err != nil {
		return fmt.Errorf("clean replacement workspace: %w", err)
	}
	patch := members[manifest.TrackedPatch.Entry]
	if len(patch) > 0 {
		stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
		command := exec.Command("git", gitArgs(workspace, []string{"apply", "--index", "--binary", "--whitespace=nowarn", "-"})...)
		command.Stdin = bytes.NewReader(patch)
		command.Stderr = stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("apply checkpoint patch: %w: %s", err, strings.TrimSpace(stderr.buf.String()))
		}
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range manifest.UntrackedFiles {
		if err := writeRestoredCheckpointFile(root, file.PathBytes, file.Mode, members[file.Entry]); err != nil {
			return err
		}
	}
	for _, file := range manifest.TaskProjection.Files {
		path := append([]byte(tasks.TasksRoot+"/"), file.PathBytes...)
		if err := writeRestoredCheckpointFile(root, path, file.Mode, members[file.Entry]); err != nil {
			return err
		}
	}
	return nil
}

func writeRestoredCheckpointFile(root *os.Root, pathBytes []byte, mode int64, body []byte) error {
	if len(pathBytes) == 0 || pathBytes[0] == '/' || bytes.Contains(pathBytes, []byte{0}) {
		return errors.New("restored checkpoint path is invalid")
	}
	name := string(pathBytes)
	for _, part := range strings.Split(name, "/") {
		// The manifest decoder already refuses these; repeating the check here keeps the
		// filesystem write safe on its own, since a hook planted under .git/ survives the
		// post-restore verification failure and `git clean -x` alike.
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return errors.New("restored checkpoint path is invalid")
		}
	}
	// os.Root confines the final path to the workspace but follows links inside it, and the
	// tracked patch applied just before this may legitimately have created one — so a member
	// under a linked directory could still land in .git/. Only components that already exist can
	// be links; refuse them before MkdirAll creates anything beneath.
	for i, b := range pathBytes {
		if b != '/' {
			continue
		}
		info, err := root.Lstat(string(pathBytes[:i]))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return fmt.Errorf("inspect restored checkpoint directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("restored checkpoint path crosses a symbolic link")
		}
	}
	parent := "."
	if index := bytes.LastIndexByte(pathBytes, '/'); index >= 0 {
		parent = string(pathBytes[:index])
	}
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create restored checkpoint directory: %w", err)
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(mode))
	if err != nil {
		return fmt.Errorf("create restored checkpoint file: %w", err)
	}
	_, writeErr := file.Write(body)
	chmodErr := file.Chmod(os.FileMode(mode))
	closeErr := file.Close()
	if writeErr != nil || chmodErr != nil || closeErr != nil {
		return errors.Join(writeErr, chmodErr, closeErr)
	}
	return nil
}

func verifyRestoredWorkspaceCheckpoint(
	sess session.Session,
	checkpoint workerproto.WorkspaceCheckpoint,
	manifest workerproto.WorkspaceCheckpointBundleManifest,
	members map[string][]byte,
) error {
	patch, truncated, err := runSessionWorkspaceGit(sess.Workspace, workerproto.MaxWorkspaceCheckpointBundleBytes+1,
		"diff", "--no-ext-diff", "--no-textconv", "--binary", checkpoint.BaseRevision, "--")
	if err != nil || truncated || !bytes.Equal(patch, members[manifest.TrackedPatch.Entry]) {
		return errors.New("restored workspace tracked patch does not match")
	}
	untracked, err := checkpointUntrackedFiles(sess.Workspace)
	if err != nil || !checkpointFileEntriesEqual(checkpointEntries(untracked), manifest.UntrackedFiles) {
		return errors.New("restored workspace untracked files do not match")
	}
	projection, _, subtasks, err := checkpointTaskProjection(sess)
	if err != nil || projection.QueueID != manifest.TaskProjection.QueueID ||
		projection.TaskID != manifest.TaskProjection.TaskID || projection.ID != manifest.TaskProjection.ID ||
		projection.State != manifest.TaskProjection.State || projection.StateSHA256 != manifest.TaskProjection.StateSHA256 ||
		!slices.Equal(subtasks, checkpoint.Task.Subtasks) {
		return errors.New("restored workspace task projection does not match")
	}
	candidate := checkpointSHA256(mustCheckpointJSON(struct {
		Base      string                                     `json:"base"`
		Committed string                                     `json:"committed"`
		Patch     workerproto.WorkspaceCheckpointBundleEntry `json:"patch"`
		Untracked []workerproto.WorkspaceCheckpointFileEntry `json:"untracked"`
	}{checkpoint.BaseRevision, checkpoint.CommittedRevision, manifest.TrackedPatch, manifest.UntrackedFiles}))
	if candidate != checkpoint.CandidateTreeSHA256 {
		return errors.New("restored workspace candidate identity does not match")
	}
	return nil
}

func checkpointFileEntriesEqual(left, right []workerproto.WorkspaceCheckpointFileEntry) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (s *Service) executeCheckpointWorkspace(
	ctx context.Context,
	op session.Operation,
	req CheckpointWorkspaceRequest,
) (CheckpointWorkspaceResult, error) {
	if req.SessionID == "" || req.SessionRef == "" || req.ExpectedRevision <= 0 ||
		req.PlacementGeneration <= 0 || req.RepositoryRef == "" {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "checkpoint identity is incomplete",
		})
	}
	if stored, err := s.readWorkspaceCheckpointArtifact(op.ID); err == nil {
		return s.completeWorkspaceCheckpoint(ctx, op, stored)
	} else if !errors.Is(err, os.ErrNotExist) {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := validateCheckpointSession(ctx, sess, req.ExpectedRevision); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	checkpoint, manifest, bundle, err := buildWorkspaceCheckpoint(
		sess, req, op.ID, time.Now().UTC(),
	)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := workerproto.ValidateWorkspaceCheckpointPair(checkpoint, manifest); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	descriptor, err := json.Marshal(checkpoint)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	artifact := checkpointPrivateArtifact{descriptor: descriptor, bundle: bundle}
	if err := s.writeWorkspaceCheckpointArtifact(op.ID, artifact); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	return s.completeWorkspaceCheckpoint(ctx, op, artifact)
}

func validateCheckpointSession(ctx context.Context, sess session.Session, expectedRevision int64) error {
	if err := validateSessionForkAuthority(ctx, sess); err != nil {
		return &session.Error{Code: session.CodeInvalidSessionState, Detail: err.Error()}
	}
	if sess.Revision != expectedRevision || sess.RepositoryReadOnly || sess.WorkspaceTask == nil ||
		(sess.State != session.SessionOpen && sess.State != session.SessionExhausted) ||
		sess.Activity != session.ActivityParked ||
		sess.ActiveTurnID != "" || sess.QueuedTurnCount != 0 {
		return &session.Error{
			Code:   session.CodeInvalidSessionState,
			Detail: "checkpoint requires an idle writable open session with one bound task",
		}
	}
	return nil
}

func buildWorkspaceCheckpoint(
	sess session.Session,
	req CheckpointWorkspaceRequest,
	operationID string,
	createdAt time.Time,
) (workerproto.WorkspaceCheckpoint, workerproto.WorkspaceCheckpointBundleManifest, []byte, error) {
	base, err := sessionWorkspaceCommit(sess.Repository, sess.BaseCommit)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	committed, err := sessionWorkspaceCommit(sess.Workspace, "HEAD")
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	branchBytes, err := sessionWorkspaceGitText(sess.Workspace, 257, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	branch := strings.TrimSpace(string(branchBytes))
	conflicts, truncated, err := runSessionWorkspaceGit(sess.Workspace, 1, "ls-files", "-u", "-z")
	if err != nil || truncated || len(conflicts) != 0 {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil,
			errors.New("checkpoint workspace contains unresolved Git conflicts")
	}
	patch, truncated, err := runSessionWorkspaceGit(
		sess.Workspace, workerproto.MaxWorkspaceCheckpointBundleBytes+1,
		"diff", "--no-ext-diff", "--no-textconv", "--binary", base, "--",
	)
	if err != nil || truncated {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil,
			errors.New("checkpoint tracked patch exceeds its bound")
	}
	untracked, err := checkpointUntrackedFiles(sess.Workspace)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	taskProjection, taskBlobs, subtasks, err := checkpointTaskProjection(sess)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	patchEntry := workerproto.WorkspaceCheckpointBundleEntry{
		Entry: "workspace.patch", SHA256: checkpointSHA256(patch), ByteSize: int64(len(patch)),
	}
	candidate := checkpointSHA256(mustCheckpointJSON(struct {
		Base      string                                     `json:"base"`
		Committed string                                     `json:"committed"`
		Patch     workerproto.WorkspaceCheckpointBundleEntry `json:"patch"`
		Untracked []workerproto.WorkspaceCheckpointFileEntry `json:"untracked"`
	}{Base: base, Committed: committed, Patch: patchEntry, Untracked: checkpointEntries(untracked)}))
	checkpointRef := "checkpoint:" + checkpointSHA256([]byte(operationID + "\x00" + candidate + "\x00" + taskProjection.StateSHA256))[:32]
	manifest := workerproto.WorkspaceCheckpointBundleManifest{
		Version: workerproto.WorkspaceCheckpointVersion, CheckpointRef: checkpointRef,
		RepositoryRef: req.RepositoryRef, BaseRevision: base, BranchRef: branch,
		CommittedRevision: committed, CandidateTreeSHA256: candidate, TrackedPatch: patchEntry,
		UntrackedFiles: checkpointEntries(untracked), TaskProjection: taskProjection,
	}
	if err := manifest.ValidateForCapture(); err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	bundle, err := writeWorkspaceCheckpointTar(manifestBytes, patch, untracked, taskBlobs)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	checkpoint := workerproto.WorkspaceCheckpoint{
		Version: workerproto.WorkspaceCheckpointVersion, CheckpointRef: checkpointRef,
		SessionRef: req.SessionRef, PlacementGeneration: req.PlacementGeneration,
		RepositoryRef: req.RepositoryRef, BaseRevision: base, BranchRef: branch,
		CommittedRevision: committed, CandidateTreeSHA256: candidate,
		Task: workerproto.WorkspaceCheckpointTask{
			QueueID: taskProjection.QueueID, TaskID: taskProjection.TaskID, ID: taskProjection.ID,
			State: taskProjection.State, Subtasks: subtasks, StateSHA256: taskProjection.StateSHA256,
		},
		Gate: workerproto.WorkspaceCheckpointGate{Status: "not_run"},
		Bundle: workerproto.WorkspaceCheckpointBundle{
			MediaType: workerproto.WorkspaceCheckpointBundleMediaType,
			SHA256:    checkpointSHA256(bundle), ByteSize: int64(len(bundle)),
		},
		CreatedAt: createdAt,
	}
	if err := checkpoint.Validate(); err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	return checkpoint, manifest, bundle, nil
}

func checkpointUntrackedFiles(workspace string) ([]checkpointBlob, error) {
	limit := workerproto.MaxWorkspaceCheckpointFiles*(workerproto.MaxWorkspaceCheckpointPathBytes+1) + 1
	raw, truncated, err := runSessionWorkspaceGit(workspace, limit, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil || truncated {
		return nil, errors.New("checkpoint untracked path list exceeds its bound")
	}
	paths := bytes.Split(raw, []byte{0})
	if len(paths) > 0 && len(paths[len(paths)-1]) == 0 {
		paths = paths[:len(paths)-1]
	}
	filtered := make([][]byte, 0, len(paths))
	for _, value := range paths {
		if bytes.HasPrefix(value, []byte(tasks.TasksRoot+"/")) {
			continue
		}
		filtered = append(filtered, append([]byte(nil), value...))
	}
	if len(filtered) > workerproto.MaxWorkspaceCheckpointFiles {
		return nil, errors.New("checkpoint contains too many untracked files")
	}
	sort.Slice(filtered, func(i, j int) bool { return bytes.Compare(filtered[i], filtered[j]) < 0 })
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return checkpointFilesFromPaths(root, filtered, "untracked", workerproto.MaxWorkspaceCheckpointBundleBytes)
}

func checkpointTaskProjection(
	sess session.Session,
) (workerproto.WorkspaceCheckpointTaskProjection, []checkpointBlob, []bool, error) {
	queue := filepath.Join(sess.Workspace, tasks.TasksRoot)
	item, ok, err := tasks.CurrentTask(queue, sess.WorkspaceTask.ID)
	if err != nil {
		return workerproto.WorkspaceCheckpointTaskProjection{}, nil, nil, err
	}
	if !ok {
		return workerproto.WorkspaceCheckpointTaskProjection{}, nil, nil, errors.New("bound workspace task is missing")
	}
	instance, err := tasks.ReadTaskInstance(queue, item)
	if err != nil || instance.Ref.QueueID != sess.WorkspaceTask.QueueID ||
		instance.Ref.TaskID != sess.WorkspaceTask.TaskID || instance.Ref.ID != sess.WorkspaceTask.ID {
		return workerproto.WorkspaceCheckpointTaskProjection{}, nil, nil, errors.New("workspace task authority changed")
	}
	paths := [][]byte{[]byte(tasks.QueueIdentityFile)}
	err = filepath.WalkDir(item.Dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == item.Dir {
			return nil
		}
		rel, err := filepath.Rel(queue, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "tmp" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("workspace task projection contains a non-regular file")
		}
		paths = append(paths, []byte(filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		return workerproto.WorkspaceCheckpointTaskProjection{}, nil, nil, err
	}
	sort.Slice(paths, func(i, j int) bool { return bytes.Compare(paths[i], paths[j]) < 0 })
	if len(paths) == 0 || len(paths) > workerproto.MaxWorkspaceCheckpointTaskFiles {
		return workerproto.WorkspaceCheckpointTaskProjection{}, nil, nil, errors.New("workspace task file count exceeds its bound")
	}
	root, err := os.OpenRoot(queue)
	if err != nil {
		return workerproto.WorkspaceCheckpointTaskProjection{}, nil, nil, err
	}
	defer root.Close()
	blobs, err := checkpointFilesFromPaths(root, paths, "task", workerproto.MaxWorkspaceCheckpointBundleBytes)
	if err != nil {
		return workerproto.WorkspaceCheckpointTaskProjection{}, nil, nil, err
	}
	state := tasks.StateLabel(item.State)
	stateSHA := checkpointSHA256(mustCheckpointJSON(struct {
		QueueID  string                                     `json:"queue_id"`
		TaskID   string                                     `json:"task_id"`
		ID       string                                     `json:"id"`
		State    string                                     `json:"state"`
		Subtasks []bool                                     `json:"subtasks"`
		Files    []workerproto.WorkspaceCheckpointFileEntry `json:"files"`
	}{instance.Ref.QueueID, instance.Ref.TaskID, instance.Ref.ID, state, item.Subtasks, checkpointEntries(blobs)}))
	projection := workerproto.WorkspaceCheckpointTaskProjection{
		QueueID: instance.Ref.QueueID, TaskID: instance.Ref.TaskID, ID: instance.Ref.ID,
		State: state, StateSHA256: stateSHA, Files: checkpointEntries(blobs),
	}
	return projection, blobs, append([]bool(nil), item.Subtasks...), nil
}

func checkpointFilesFromPaths(root *os.Root, paths [][]byte, prefix string, maximum int64) ([]checkpointBlob, error) {
	result := make([]checkpointBlob, 0, len(paths))
	var total int64
	for index, rawPath := range paths {
		if len(rawPath) == 0 || len(rawPath) > workerproto.MaxWorkspaceCheckpointPathBytes || bytes.IndexByte(rawPath, 0) >= 0 {
			return nil, errors.New("checkpoint file path is invalid")
		}
		name := string(rawPath)
		before, err := root.Lstat(name)
		if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("checkpoint member is not a regular file")
		}
		if before.Size() < 0 || before.Size() > maximum-total {
			return nil, errors.New("checkpoint file bytes exceed their bound")
		}
		file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, maximum-total+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(before, after) || int64(len(body)) != before.Size() {
			return nil, errors.New("checkpoint file changed while reading")
		}
		total += int64(len(body))
		mode := int64(0o644)
		if before.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		result = append(result, checkpointBlob{
			path: body,
			entry: workerproto.WorkspaceCheckpointFileEntry{
				PathB64: base64.StdEncoding.EncodeToString(rawPath),
				Entry:   fmt.Sprintf("%s/%06d", prefix, index), Mode: mode,
				SHA256: checkpointSHA256(body), ByteSize: int64(len(body)),
			},
		})
	}
	return result, nil
}

func checkpointEntries(blobs []checkpointBlob) []workerproto.WorkspaceCheckpointFileEntry {
	result := make([]workerproto.WorkspaceCheckpointFileEntry, len(blobs))
	for index := range blobs {
		result[index] = blobs[index].entry
	}
	return result
}

func writeWorkspaceCheckpointTar(
	manifest []byte,
	patch []byte,
	untracked []checkpointBlob,
	taskFiles []checkpointBlob,
) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	write := func(name string, mode int64, body []byte) error {
		header := &tar.Header{
			Name: name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
			ModTime: time.Unix(0, 0).UTC(),
			Format:  tar.FormatUSTAR,
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		_, err := writer.Write(body)
		return err
	}
	if err := write("manifest.json", 0o644, manifest); err != nil {
		return nil, err
	}
	if err := write("workspace.patch", 0o644, patch); err != nil {
		return nil, err
	}
	for _, blob := range append(append([]checkpointBlob(nil), untracked...), taskFiles...) {
		if err := write(blob.entry.Entry, blob.entry.Mode, blob.path); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if output.Len() == 0 || output.Len() > workerproto.MaxWorkspaceCheckpointBundleBytes {
		return nil, errors.New("workspace checkpoint tar exceeds its bound")
	}
	return output.Bytes(), nil
}

func (s *Service) completeWorkspaceCheckpoint(
	ctx context.Context,
	op session.Operation,
	artifact checkpointPrivateArtifact,
) (CheckpointWorkspaceResult, error) {
	var checkpoint workerproto.WorkspaceCheckpoint
	if err := json.Unmarshal(artifact.descriptor, &checkpoint); err != nil {
		return CheckpointWorkspaceResult{}, err
	}
	result := CheckpointWorkspaceResult{OperationID: op.ID, Checkpoint: checkpoint}
	encoded, err := json.Marshal(result)
	if err != nil {
		return CheckpointWorkspaceResult{}, err
	}
	if op.State == session.OperationReserved {
		if err := s.store.MarkOperationRunning(ctx, op.ID, encoded); err != nil {
			return CheckpointWorkspaceResult{}, err
		}
	}
	if err := s.store.CompleteOperation(ctx, op.ID, "workspace_checkpoint", checkpoint.CheckpointRef, encoded); err != nil {
		return CheckpointWorkspaceResult{}, err
	}
	return result, nil
}

func replayCheckpointWorkspace(op session.Operation) (CheckpointWorkspaceResult, error) {
	if op.State != session.OperationSucceeded {
		return CheckpointWorkspaceResult{}, session.ErrOperationUncertain
	}
	var result CheckpointWorkspaceResult
	if err := json.Unmarshal(op.Result, &result); err != nil || result.OperationID != op.ID {
		return CheckpointWorkspaceResult{}, errors.New("stored workspace checkpoint result is invalid")
	}
	return result, nil
}

func (s *Service) writeWorkspaceCheckpointArtifact(operationID string, artifact checkpointPrivateArtifact) error {
	if !validSessionPathComponent(operationID) {
		return errors.New("checkpoint operation identity is invalid")
	}
	root := filepath.Join(s.stateRoot, "workspace-checkpoints")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	var body bytes.Buffer
	body.WriteString(workspaceCheckpointPrivateMagic)
	if len(artifact.descriptor) == 0 || len(artifact.descriptor) > workerproto.MaxWorkspaceCheckpointManifest {
		return errors.New("workspace checkpoint descriptor exceeds its bound")
	}
	_ = binary.Write(&body, binary.BigEndian, uint32(len(artifact.descriptor)))
	body.Write(artifact.descriptor)
	body.Write(artifact.bundle)
	tmp, err := os.CreateTemp(root, ".checkpoint-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	destination := filepath.Join(root, operationID+".checkpoint")
	if existing, err := s.readWorkspaceCheckpointArtifact(operationID); err == nil {
		if !bytes.Equal(existing.descriptor, artifact.descriptor) || !bytes.Equal(existing.bundle, artifact.bundle) {
			return errors.New("workspace checkpoint artifact conflicts with its operation")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tmpName, destination)
}

func (s *Service) readWorkspaceCheckpointArtifact(operationID string) (checkpointPrivateArtifact, error) {
	if !validSessionPathComponent(operationID) {
		return checkpointPrivateArtifact{}, errors.New("checkpoint operation identity is invalid")
	}
	path := filepath.Join(s.stateRoot, "workspace-checkpoints", operationID+".checkpoint")
	body, err := os.ReadFile(path)
	if err != nil {
		return checkpointPrivateArtifact{}, err
	}
	maximum := len(workspaceCheckpointPrivateMagic) + 4 + workerproto.MaxWorkspaceCheckpointManifest + workerproto.MaxWorkspaceCheckpointBundleBytes
	if len(body) > maximum || !bytes.HasPrefix(body, []byte(workspaceCheckpointPrivateMagic)) {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint artifact is invalid")
	}
	offset := len(workspaceCheckpointPrivateMagic)
	if len(body) < offset+4 {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint artifact is truncated")
	}
	descriptorBytes := int(binary.BigEndian.Uint32(body[offset : offset+4]))
	offset += 4
	if descriptorBytes <= 0 || descriptorBytes > workerproto.MaxWorkspaceCheckpointManifest || len(body) <= offset+descriptorBytes {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint descriptor is invalid")
	}
	artifact := checkpointPrivateArtifact{
		descriptor: append([]byte(nil), body[offset:offset+descriptorBytes]...),
		bundle:     append([]byte(nil), body[offset+descriptorBytes:]...),
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(artifact.descriptor)
	if err != nil || int64(len(artifact.bundle)) != checkpoint.Bundle.ByteSize ||
		checkpointSHA256(artifact.bundle) != checkpoint.Bundle.SHA256 {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint identity does not match")
	}
	return artifact, nil
}

func (s *Service) OpenWorkspaceCheckpointBundle(ctx context.Context, operationID string) ([]byte, error) {
	op, err := s.store.GetOperationByID(ctx, operationID)
	if err != nil || op.Method != "CheckpointWorkspace" || op.State != session.OperationSucceeded ||
		op.ResourceType != "workspace_checkpoint" || op.ResourceID == "" {
		return nil, session.ErrOperationNotFound
	}
	artifact, err := s.readWorkspaceCheckpointArtifact(operationID)
	if err != nil {
		return nil, err
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(artifact.descriptor)
	if err != nil || checkpoint.CheckpointRef != op.ResourceID {
		return nil, errors.New("workspace checkpoint operation identity does not match")
	}
	return append([]byte(nil), artifact.bundle...), nil
}

func checkpointSHA256(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func mustCheckpointJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
