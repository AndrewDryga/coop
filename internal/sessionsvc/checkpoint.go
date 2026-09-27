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
	"path/filepath"
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
	SessionID   string                          `json:"session_id,omitempty"` // private custody; omitted by the HTTP projection
	Checkpoint  workerproto.WorkspaceCheckpoint `json:"checkpoint"`
}

type RestoreWorkspaceCheckpointRequest struct {
	SessionID        string                          `json:"session_id"`
	ExpectedRevision int64                           `json:"expected_revision"`
	Checkpoint       workerproto.WorkspaceCheckpoint `json:"checkpoint"`
	Stream           io.Reader                       `json:"-"`
}

type checkpointBlob struct {
	path  string
	entry workerproto.WorkspaceCheckpointFileEntry
}

type checkpointPrivateArtifact struct {
	descriptor []byte
	path       string
	offset     int64
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
	if replay && op.State != session.OperationReserved && op.State != session.OperationRunning && op.State != session.OperationUncertain {
		return replaySessionOperation(op)
	}
	return s.executeRestoreWorkspaceCheckpoint(ctx, op, req)
}

func (s *Service) executeRestoreWorkspaceCheckpoint(ctx context.Context, op session.Operation, req RestoreWorkspaceCheckpointRequest) (session.Session, error) {
	if req.Checkpoint.Version != workerproto.WorkspaceCheckpointVersion {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "historical checkpoint v1 is read-only; restore requires complete repository custody in v2",
		})
	}
	if req.Stream == nil {
		return session.Session{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidRequest, Detail: "checkpoint body is required",
		})
	}
	return s.restoreStreamedCheckpoint(ctx, op, req)
}

func (s *Service) validateRestoreWorkspaceSession(
	ctx context.Context,
	sess session.Session,
	req RestoreWorkspaceCheckpointRequest,
	recovering bool,
) error {
	if err := requireSessionWorkspace(sess); err != nil {
		return err
	}
	if err := s.validateSessionForkAuthority(ctx, sess); err != nil {
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
		// Only the same journaled operation can finish a partially completed restore.
		// Its immutable request hash pins the whole checkpoint, not merely the task ID.
		// A new key must never reset files in an already-bound session.
		task := req.Checkpoint.Task
		if !recovering || task.QueueID != bound.QueueID || task.TaskID != bound.TaskID || task.ID != bound.ID ||
			req.Checkpoint.BaseRevision != sess.BaseCommit {
			return &session.Error{Code: session.CodeInvalidSessionState,
				Detail: "session is already bound to another restored workspace task"}
		}
	}
	base, err := sessionWorkspaceCommitContext(ctx, sess.Repository, req.Checkpoint.BaseRevision)
	if err != nil || base != req.Checkpoint.BaseRevision || base != sess.BaseCommit {
		return &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "checkpoint base revision is unavailable in the authorized repository"}
	}
	return nil
}

func writeRestoredCheckpointStream(root *os.Root, pathBytes []byte, mode int64, body io.Reader) error {
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
	_, writeErr := io.Copy(file, body)
	chmodErr := file.Chmod(os.FileMode(mode))
	closeErr := file.Close()
	if writeErr != nil || chmodErr != nil || closeErr != nil {
		return errors.Join(writeErr, chmodErr, closeErr)
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
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.validateCheckpointSession(ctx, sess, req.ExpectedRevision); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	release, ok := s.beginWorkspaceRestore(req.SessionID)
	if !ok {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, &session.Error{
			Code: session.CodeInvalidSessionState, Detail: "checkpoint requires a parked runtime",
		})
	}
	defer release()
	if stored, err := s.readWorkspaceCheckpointArtifact(ctx, op.ID); err == nil {
		return s.completeWorkspaceCheckpoint(ctx, op, req.SessionID, stored)
	} else if !errors.Is(err, os.ErrNotExist) {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if pending, err := s.store.HasPendingWorkspaceRestore(ctx, req.SessionID, ""); err != nil || pending {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID,
			errors.Join(err, errors.New("workspace restore must finish before capture")))
	}
	// Re-read under the runtime hold: no queued turn may race this snapshot.
	sess, err = s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if err := s.validateCheckpointSession(ctx, sess, req.ExpectedRevision); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	if evicter, ok := s.runner.(sessionRunnerWarmEvicter); ok {
		if err := evicter.EvictWarmSession(sess.ID); err != nil {
			return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
		}
	}
	ctx, stopDiskWatch, err := s.checkpointDiskContext(ctx, sess.Workspace, 0)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	defer stopDiskWatch()
	if op.State == session.OperationReserved {
		intent, _ := json.Marshal(req)
		if err := s.store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
			return CheckpointWorkspaceResult{}, err
		}
		op.State = session.OperationRunning
	}
	stageRoot := filepath.Join(s.stateRoot, "workspace-checkpoints", ".staging")
	if err := os.MkdirAll(stageRoot, 0700); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	stage, err := os.MkdirTemp(stageRoot, op.ID+"-")
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	defer os.RemoveAll(stage)
	checkpoint, manifest, bundle, err := buildWorkspaceCheckpoint(
		ctx, stage, sess, req, op.ID, time.Now().UTC(),
	)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	defer os.Remove(bundle.Name())
	defer bundle.Close()
	if err := workerproto.ValidateWorkspaceCheckpointPair(checkpoint, manifest); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	descriptor, err := json.Marshal(checkpoint)
	if err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	artifact := checkpointPrivateArtifact{descriptor: descriptor, path: bundle.Name()}
	if err := s.writeWorkspaceCheckpointArtifact(ctx, op.ID, artifact); err != nil {
		return CheckpointWorkspaceResult{}, s.failServiceOperation(ctx, op.ID, err)
	}
	return s.completeWorkspaceCheckpoint(ctx, op, req.SessionID, artifact)
}

func (s *Service) validateCheckpointSession(ctx context.Context, sess session.Session, expectedRevision int64) error {
	if err := requireSessionWorkspace(sess); err != nil {
		return err
	}
	if err := s.validateSessionForkAuthority(ctx, sess); err != nil {
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
	ctx context.Context,
	stage string,
	sess session.Session,
	req CheckpointWorkspaceRequest,
	operationID string,
	createdAt time.Time,
) (workerproto.WorkspaceCheckpoint, workerproto.WorkspaceCheckpointBundleManifest, *os.File, error) {
	base, err := sessionWorkspaceCommitContext(ctx, sess.Repository, sess.BaseCommit)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	committed, err := sessionWorkspaceCommitContext(ctx, sess.Workspace, "HEAD")
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	if err := verifyUnchangedSessionSubmodules(ctx, sess.Repository, sess.Workspace, committed); err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil,
			fmt.Errorf("nested work needs separate custody before checkpoint: %w", err)
	}
	branchBytes, truncated, err := runSessionWorkspaceGitWithEnvContext(ctx, sess.Workspace, 257, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if truncated {
		err = errors.New("checkpoint branch exceeds its bound")
	}
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	branch := strings.TrimSpace(string(branchBytes))
	conflicts, truncated, err := runSessionWorkspaceGitWithEnvContext(ctx, sess.Workspace, 1, nil, "ls-files", "-u", "-z")
	if err != nil || truncated || len(conflicts) != 0 {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil,
			errors.New("checkpoint workspace contains unresolved Git conflicts")
	}
	repository, err := os.CreateTemp(stage, "repository-")
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	defer os.Remove(repository.Name())
	defer repository.Close()
	var tree string
	digest := sha256.New()
	err = withSessionTrackedTree(ctx, sess.Workspace, func(captured string, env []string) error {
		tree = captured
		return writeCheckpointRepository(ctx, sess.Workspace, base, committed, tree, env, io.MultiWriter(repository, digest))
	})
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	size, err := repository.Seek(0, io.SeekCurrent)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	content := workerproto.WorkspaceCheckpointBundleEntry{Entry: "repository.tar", SHA256: fmt.Sprintf("%x", digest.Sum(nil)), ByteSize: size}
	untracked, err := checkpointUntrackedFiles(ctx, sess.Workspace)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	taskProjection, taskBlobs, subtasks, err := checkpointTaskProjection(ctx, sess)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	candidate := checkpointSHA256(mustCheckpointJSON(struct {
		Base      string                                     `json:"base"`
		Committed string                                     `json:"committed"`
		Tree      string                                     `json:"tree"`
		Untracked []workerproto.WorkspaceCheckpointFileEntry `json:"untracked"`
	}{Base: base, Committed: committed, Tree: tree, Untracked: checkpointEntries(untracked)}))
	checkpointRef := "checkpoint:" + checkpointSHA256([]byte(operationID + "\x00" + candidate + "\x00" + taskProjection.StateSHA256))[:32]
	manifest := workerproto.WorkspaceCheckpointBundleManifest{
		Version: workerproto.WorkspaceCheckpointVersion, CheckpointRef: checkpointRef,
		RepositoryRef: req.RepositoryRef, BaseRevision: base, BranchRef: branch,
		CommittedRevision: committed, CandidateTreeSHA256: candidate, Repository: &content, TrackedTree: tree,
		UntrackedFiles: checkpointEntries(untracked), TaskProjection: taskProjection,
	}
	if err := manifest.ValidateForCapture(); err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	bundle, bundleIdentity, err := writeWorkspaceCheckpointTar(ctx, manifestBytes, repository, content, untracked, taskBlobs)
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
		Gate:      workerproto.WorkspaceCheckpointGate{Status: "not_run"},
		Bundle:    bundleIdentity,
		CreatedAt: createdAt,
	}
	if err := checkpoint.Validate(); err != nil {
		_ = bundle.Close()
		_ = os.Remove(bundle.Name())
		return workerproto.WorkspaceCheckpoint{}, workerproto.WorkspaceCheckpointBundleManifest{}, nil, err
	}
	return checkpoint, manifest, bundle, nil
}

func checkpointUntrackedFiles(ctx context.Context, workspace string) ([]checkpointBlob, error) {
	limit := workerproto.MaxWorkspaceCheckpointFiles*(workerproto.MaxWorkspaceCheckpointPathBytes+1) + 1
	raw, truncated, err := runSessionWorkspaceGitWithEnvContext(ctx, workspace, limit, nil, "ls-files", "--others", "--exclude-standard", "-z")
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
	return checkpointFilesFromPaths(ctx, root, filtered, "untracked", workerproto.MaxWorkspaceCheckpointStreamBytes)
}

func checkpointTaskProjection(
	ctx context.Context,
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
	blobs, err := checkpointFilesFromPaths(ctx, root, paths, "task", workerproto.MaxWorkspaceCheckpointStreamBytes)
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

func checkpointFilesFromPaths(ctx context.Context, root *os.Root, paths [][]byte, prefix string, maximum int64) ([]checkpointBlob, error) {
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
		hash := sha256.New()
		written, readErr := io.Copy(hash, io.LimitReader(reviewScanReader{ctx, file}, before.Size()+1))
		after, statErr := file.Stat()
		closeErr := file.Close()
		if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(before, after) || written != before.Size() {
			return nil, errors.New("checkpoint file changed while reading")
		}
		total += written
		mode := int64(0o644)
		if before.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		result = append(result, checkpointBlob{
			path: file.Name(),
			entry: workerproto.WorkspaceCheckpointFileEntry{
				PathB64: base64.StdEncoding.EncodeToString(rawPath),
				Entry:   fmt.Sprintf("%s/%06d", prefix, index), Mode: mode,
				SHA256: fmt.Sprintf("%x", hash.Sum(nil)), ByteSize: written,
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
	ctx context.Context,
	manifest []byte,
	repository *os.File,
	content workerproto.WorkspaceCheckpointBundleEntry,
	untracked []checkpointBlob,
	taskFiles []checkpointBlob,
) (output *os.File, identity workerproto.WorkspaceCheckpointBundle, returnErr error) {
	output, err := os.CreateTemp(filepath.Dir(repository.Name()), ".checkpoint-bundle-")
	if err != nil {
		return nil, identity, err
	}
	defer func() {
		if returnErr != nil {
			_ = output.Close()
			_ = os.Remove(output.Name())
		}
	}()
	hash := sha256.New()
	counter := &checkpointCountWriter{writer: io.MultiWriter(output, hash)}
	writer := tar.NewWriter(counter)
	write := func(name string, mode, size int64, digest string, input io.Reader) error {
		if err := checkpointTarHeader(writer, name, mode, size); err != nil {
			return err
		}
		return copyCheckpointContent(ctx, writer, input, size, digest)
	}
	if err := write("manifest.json", 0644, int64(len(manifest)), "", bytes.NewReader(manifest)); err != nil {
		return output, identity, err
	}
	if _, err := repository.Seek(0, io.SeekStart); err != nil {
		return output, identity, err
	}
	if err := write(content.Entry, 0644, content.ByteSize, content.SHA256, repository); err != nil {
		return output, identity, err
	}
	for _, blob := range append(append([]checkpointBlob(nil), untracked...), taskFiles...) {
		file, err := os.OpenFile(blob.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return output, identity, err
		}
		err = write(blob.entry.Entry, blob.entry.Mode, blob.entry.ByteSize, blob.entry.SHA256, file)
		if err = errors.Join(err, file.Close()); err != nil {
			return output, identity, err
		}
	}
	if err := errors.Join(writer.Close(), output.Sync()); err != nil {
		return output, identity, err
	}
	identity = workerproto.WorkspaceCheckpointBundle{MediaType: workerproto.WorkspaceCheckpointBundleMediaType,
		SHA256: fmt.Sprintf("%x", hash.Sum(nil)), ByteSize: counter.bytes}
	return output, identity, nil
}

func (s *Service) completeWorkspaceCheckpoint(
	ctx context.Context,
	op session.Operation,
	sessionID string,
	artifact checkpointPrivateArtifact,
) (CheckpointWorkspaceResult, error) {
	var checkpoint workerproto.WorkspaceCheckpoint
	if err := json.Unmarshal(artifact.descriptor, &checkpoint); err != nil {
		return CheckpointWorkspaceResult{}, err
	}
	result := CheckpointWorkspaceResult{OperationID: op.ID, SessionID: sessionID, Checkpoint: checkpoint}
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

func (s *Service) writeWorkspaceCheckpointArtifact(ctx context.Context, operationID string, artifact checkpointPrivateArtifact) error {
	if !validSessionPathComponent(operationID) {
		return errors.New("checkpoint operation identity is invalid")
	}
	root := filepath.Join(s.stateRoot, "workspace-checkpoints")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	var header bytes.Buffer
	header.WriteString(workspaceCheckpointPrivateMagic)
	if len(artifact.descriptor) == 0 || len(artifact.descriptor) > workerproto.MaxWorkspaceCheckpointManifest {
		return errors.New("workspace checkpoint descriptor exceeds its bound")
	}
	_ = binary.Write(&header, binary.BigEndian, uint32(len(artifact.descriptor)))
	header.Write(artifact.descriptor)
	tmp, err := os.CreateTemp(filepath.Dir(artifact.path), "artifact-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(header.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	input, err := os.Open(artifact.path)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	defer input.Close()
	if _, err := input.Seek(artifact.offset, io.SeekStart); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := io.Copy(tmp, reviewScanReader{ctx, input}); err != nil {
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
	if existing, err := s.readWorkspaceCheckpointArtifact(ctx, operationID); err == nil {
		if !bytes.Equal(existing.descriptor, artifact.descriptor) {
			return errors.New("workspace checkpoint artifact conflicts with its operation")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmpName, destination); err != nil {
		return err
	}
	return syncReviewPath(root)
}

func (s *Service) readWorkspaceCheckpointArtifact(ctx context.Context, operationID string) (checkpointPrivateArtifact, error) {
	if !validSessionPathComponent(operationID) {
		return checkpointPrivateArtifact{}, errors.New("checkpoint operation identity is invalid")
	}
	path := filepath.Join(s.stateRoot, "workspace-checkpoints", operationID+".checkpoint")
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return checkpointPrivateArtifact{}, err
	}
	defer file.Close()
	header := make([]byte, len(workspaceCheckpointPrivateMagic)+4)
	if _, err := io.ReadFull(file, header); err != nil || !bytes.HasPrefix(header, []byte(workspaceCheckpointPrivateMagic)) {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint artifact is invalid")
	}
	descriptorBytes := int(binary.BigEndian.Uint32(header[len(workspaceCheckpointPrivateMagic):]))
	if descriptorBytes <= 0 || descriptorBytes > workerproto.MaxWorkspaceCheckpointManifest {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint descriptor is invalid")
	}
	artifact := checkpointPrivateArtifact{
		descriptor: make([]byte, descriptorBytes), path: path,
		offset: int64(len(header) + descriptorBytes),
	}
	if _, err := io.ReadFull(file, artifact.descriptor); err != nil {
		return checkpointPrivateArtifact{}, err
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(artifact.descriptor)
	if err != nil {
		return checkpointPrivateArtifact{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size()-artifact.offset != checkpoint.Bundle.ByteSize {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint length does not match")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, reviewScanReader{ctx, file}); err != nil || fmt.Sprintf("%x", hash.Sum(nil)) != checkpoint.Bundle.SHA256 {
		return checkpointPrivateArtifact{}, errors.New("private workspace checkpoint identity does not match")
	}
	return artifact, nil
}

func (s *Service) OpenWorkspaceCheckpointBundle(ctx context.Context, operationID string) (*os.File, workerproto.WorkspaceCheckpoint, error) {
	op, err := s.store.GetOperationByID(ctx, operationID)
	if err != nil || op.Method != "CheckpointWorkspace" || op.State != session.OperationSucceeded ||
		op.ResourceType != "workspace_checkpoint" || op.ResourceID == "" {
		return nil, workerproto.WorkspaceCheckpoint{}, session.ErrOperationNotFound
	}
	artifact, err := s.readWorkspaceCheckpointArtifact(ctx, operationID)
	if err != nil {
		return nil, workerproto.WorkspaceCheckpoint{}, err
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(artifact.descriptor)
	if err != nil || checkpoint.CheckpointRef != op.ResourceID {
		return nil, workerproto.WorkspaceCheckpoint{}, errors.New("workspace checkpoint operation identity does not match")
	}
	file, err := os.OpenFile(artifact.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, checkpoint, err
	}
	if _, err := file.Seek(artifact.offset, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, checkpoint, err
	}
	return file, checkpoint, nil
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
