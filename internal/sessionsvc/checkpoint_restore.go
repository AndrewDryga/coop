package sessionsvc

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

func (s *Service) restoreStreamedCheckpoint(ctx context.Context, op session.Operation, req RestoreWorkspaceCheckpointRequest) (session.Session, error) {
	// Once mutation has begun, a refusal cannot release the durable runtime fence.
	// The exact body is retained for restart/retry, never reconstructed from settings.
	fail := func(err error) (session.Session, error) {
		if op.State == session.OperationReserved {
			return session.Session{}, s.failServiceOperation(ctx, op.ID, err)
		}
		return session.Session{}, wrapServiceOperationError(op.ID, &session.Error{
			Code: session.CodeOperationUncertain, Detail: "checkpoint restore must finish before execution: " + err.Error(),
		})
	}
	if req.SessionID == "" || req.ExpectedRevision <= 0 {
		return fail(errors.New("restore session and revision are required"))
	}
	if err := req.Checkpoint.Validate(); err != nil {
		return fail(err)
	}
	release, ok := s.beginWorkspaceRestore(req.SessionID)
	if !ok {
		return fail(&session.Error{Code: session.CodeInvalidSessionState, Detail: "restore requires a parked runtime"})
	}
	defer release()
	if pending, err := s.store.HasPendingWorkspaceRestore(ctx, req.SessionID, op.ID); err != nil || pending {
		return fail(errors.Join(err, errors.New("another checkpoint restore must finish first")))
	}
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return fail(err)
	}
	if err := s.validateRestoreWorkspaceSession(ctx, sess, req, op.State != session.OperationReserved); err != nil {
		return fail(err)
	}
	ctx, stopDiskWatch, err := s.checkpointDiskContext(ctx, sess.Workspace, req.Checkpoint.Bundle.ByteSize)
	if err != nil {
		return fail(err)
	}
	defer stopDiskWatch()
	if evicter, ok := s.runner.(sessionRunnerWarmEvicter); ok {
		if err := evicter.EvictWarmSession(sess.ID); err != nil {
			return fail(err)
		}
	}
	root := filepath.Join(s.stateRoot, "workspace-checkpoints", ".staging")
	if err := os.MkdirAll(root, 0700); err != nil {
		return fail(err)
	}
	permit, err := s.admitNewFork(root, "")
	if err != nil {
		return fail(err)
	}
	defer permit()
	stage, err := os.MkdirTemp(root, op.ID+"-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(stage)
	repository := filepath.Join(stage, "repository")
	if err := initializeCheckpointRepository(ctx, sess.Repository, repository, req.Checkpoint.BaseRevision); err != nil {
		return fail(err)
	}
	body, err := os.OpenFile(filepath.Join(stage, "body.tar"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	defer body.Close()
	members := make(map[string]string)
	manifest, err := workerproto.ReadWorkspaceCheckpointBundle(req.Checkpoint, io.TeeReader(reviewScanReader{ctx, req.Stream}, body), func(header *tar.Header, input io.Reader) error {
		name := filepath.Join(stage, "members", header.Name)
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		err = copyCheckpointContent(ctx, file, input, header.Size, "")
		members[header.Name] = name
		return errors.Join(err, file.Close())
	})
	if err != nil {
		return fail(err)
	}
	archive, err := os.Open(members[manifest.Repository.Entry])
	if err != nil {
		return fail(err)
	}
	err = readCheckpointRepository(ctx, repository, archive)
	if err := errors.Join(err, archive.Close()); err != nil {
		return fail(err)
	}
	if err := validateCheckpointRepository(ctx, repository, req.Checkpoint, manifest); err != nil {
		return fail(err)
	}
	if err := materializeCheckpoint(ctx, sess.Repository, repository, repository, req.Checkpoint, manifest, members); err != nil {
		return fail(err)
	}
	verification := sess
	verification.Workspace = repository
	binding, err := verifyStreamedCheckpoint(ctx, verification, req.Checkpoint, manifest)
	if err != nil {
		return fail(err)
	}
	if sess.WorkspaceTask != nil && *sess.WorkspaceTask != binding {
		return fail(errors.New("session is already bound to another restored workspace task"))
	}
	if op.State == session.OperationReserved {
		descriptor, _ := json.Marshal(req.Checkpoint)
		if err := s.writeWorkspaceCheckpointArtifact(ctx, op.ID, checkpointPrivateArtifact{descriptor: descriptor, path: body.Name()}); err != nil {
			return fail(err)
		}
		intent, _ := json.Marshal(req)
		if err := s.store.MarkOperationRunning(ctx, op.ID, intent); err != nil {
			return fail(err)
		}
		op.State = session.OperationRunning
	}
	if s.testDuringRestoreFiles != nil {
		s.testDuringRestoreFiles()
	}
	branch, err := sessionWorkspaceGitText(sess.Workspace, 257, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) != sess.ForkName {
		return fail(errors.New("replacement workspace branch authority changed"))
	}
	if err := materializeCheckpoint(ctx, sess.Repository, repository, sess.Workspace, req.Checkpoint, manifest, members); err != nil {
		return fail(err)
	}
	if err := s.validateSessionForkAuthority(ctx, sess); err != nil {
		return fail(err)
	}
	if _, err := verifyStreamedCheckpoint(ctx, sess, req.Checkpoint, manifest); err != nil {
		return fail(err)
	}
	bound, err := s.store.RestoreWorkspaceTask(ctx, sess.ID, req.ExpectedRevision, req.Checkpoint.BaseRevision, binding)
	if err != nil {
		return fail(err)
	}
	result, _ := json.Marshal(bound)
	if err := s.store.CompleteOperation(ctx, op.ID, "session", bound.ID, result); err != nil {
		return fail(err)
	}
	// A succeeded receipt replays from the journal, not the now-unused body.
	_ = os.Remove(filepath.Join(s.stateRoot, "workspace-checkpoints", op.ID+".checkpoint"))
	return bound, nil
}

func initializeCheckpointRepository(ctx context.Context, source, repository, base string) error {
	format := "sha1"
	if len(base) == 64 {
		format = "sha256"
	}
	command := exec.CommandContext(ctx, "git", append(append([]string{}, forkspace.GitHardening...),
		"init", "--quiet", "--template=", "--object-format="+format, repository)...)
	command.Env = sessionCompanionGitEnv()
	if err := command.Run(); err != nil {
		return err
	}
	if err := forkspace.GitFetchPinnedContext(ctx, source, repository, base); err != nil {
		return err
	}
	// Create the initial ref and reflog on the real Git directory before a
	// worktree command runs through the trusted view.
	command = forkspace.GitRefCommand(ctx, repository, "update-ref", "HEAD", base)
	command.Env = sessionCompanionGitEnv()
	return command.Run()
}

func validateCheckpointRepository(ctx context.Context, repository string, checkpoint workerproto.WorkspaceCheckpoint, manifest workerproto.WorkspaceCheckpointBundleManifest) error {
	for _, args := range [][]string{
		{"merge-base", "--is-ancestor", checkpoint.BaseRevision, checkpoint.CommittedRevision},
		{"fsck", "--full", "--strict", "--unreachable", "--no-progress", checkpoint.CommittedRevision, manifest.TrackedTree},
	} {
		output, truncated, err := runSessionCompanionGitContext(ctx, repository, 4096, args...)
		if err != nil || truncated || len(output) != 0 {
			return errors.New("checkpoint repository object closure is invalid")
		}
	}
	wantLinks, err := forkspace.Gitlinks(ctx, repository, checkpoint.BaseRevision)
	if err != nil {
		return err
	}
	seen := filepath.Join(repository, ".git", "checkpoint-seen")
	err = visitCheckpointTrees(ctx, repository, checkpoint.BaseRevision, checkpoint.CommittedRevision, manifest.TrackedTree, func(commit string) error {
		links, err := forkspace.Gitlinks(ctx, repository, commit)
		if err != nil || !maps.Equal(wantLinks, links) {
			return errors.New("checkpoint contains submodule changes without separate custody")
		}
		if _, _, err := runSessionCompanionGitContext(ctx, repository, 128, "read-tree", commit); err != nil {
			return err
		}
		if err := forkspace.CopyLFSObjects(ctx, repository, commit); err != nil {
			return err
		}
		return forkspace.VisitLFSPointers(ctx, repository, commit, func(pointer forkspace.LFSPointer) error {
			err := os.Remove(filepath.Join(seen, "lfs-"+pointer.OID))
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		})
	})
	if err != nil {
		return err
	}
	dir, err := os.Open(seen)
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(128)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "lfs-") {
				return errors.New("checkpoint contains an unused LFS object")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
	}
}

func materializeCheckpoint(ctx context.Context, source, objects, workspace string, checkpoint workerproto.WorkspaceCheckpoint, manifest workerproto.WorkspaceCheckpointBundleManifest, members map[string]string) error {
	if objects != workspace {
		for _, oid := range []string{checkpoint.CommittedRevision, manifest.TrackedTree} {
			if err := forkspace.GitFetchPinnedContext(ctx, objects, workspace, oid); err != nil {
				return err
			}
		}
		if err := visitCheckpointTrees(ctx, workspace, checkpoint.BaseRevision, checkpoint.CommittedRevision, manifest.TrackedTree, func(commit string) error {
			if _, _, err := runSessionCompanionGitContext(ctx, workspace, 128, "read-tree", commit); err != nil {
				return err
			}
			return forkspace.CopyLFSObjects(ctx, workspace, commit, objects)
		}); err != nil {
			return err
		}
	}
	for _, args := range [][]string{
		{"reset", "--hard", checkpoint.CommittedRevision},
		{"clean", "-qfdx", "-e", "/" + forkspace.GenerationMarkerName},
		{"read-tree", "--reset", "-u", manifest.TrackedTree},
	} {
		if _, _, err := runSessionCompanionGitContext(ctx, workspace, sessionWorkspaceGitOutputLimit, args...); err != nil {
			return err
		}
	}
	if err := materializeSessionSubmodules(ctx, source, workspace, manifest.TrackedTree, objects); err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	write := func(entry workerproto.WorkspaceCheckpointFileEntry, path []byte) error {
		if string(path) == forkspace.GenerationMarkerName {
			return errors.New("checkpoint cannot replace fork authority")
		}
		file, err := os.Open(members[entry.Entry])
		if err != nil {
			return err
		}
		defer file.Close()
		return writeRestoredCheckpointStream(root, path, entry.Mode, reviewScanReader{ctx, file})
	}
	for _, entry := range manifest.UntrackedFiles {
		if err := write(entry, entry.PathBytes); err != nil {
			return err
		}
	}
	for _, entry := range manifest.TaskProjection.Files {
		if err := write(entry, append([]byte(tasks.TasksRoot+"/"), entry.PathBytes...)); err != nil {
			return err
		}
	}
	return nil
}

func verifyStreamedCheckpoint(ctx context.Context, sess session.Session, checkpoint workerproto.WorkspaceCheckpoint, manifest workerproto.WorkspaceCheckpointBundleManifest) (session.WorkspaceTaskBinding, error) {
	var binding session.WorkspaceTaskBinding
	head, err := sessionWorkspaceCommitContext(ctx, sess.Workspace, "HEAD")
	if err != nil || head != checkpoint.CommittedRevision {
		return binding, errors.New("restored workspace HEAD does not match")
	}
	err = withSessionTrackedTree(ctx, sess.Workspace, func(tree string, _ []string) error {
		if tree != manifest.TrackedTree {
			return errors.New("restored workspace tracked tree does not match")
		}
		return nil
	})
	if err != nil {
		return binding, err
	}
	untracked, err := checkpointUntrackedFiles(ctx, sess.Workspace)
	if err != nil || !checkpointFileEntriesEqual(checkpointEntries(untracked), manifest.UntrackedFiles) {
		return binding, errors.New("restored workspace untracked files do not match")
	}
	recovered, err := tasks.ReadControllerTaskBinding(sess.Workspace, checkpoint.Task.ID)
	if err != nil || recovered.QueueID != checkpoint.Task.QueueID || recovered.TaskID != checkpoint.Task.TaskID ||
		recovered.ID != checkpoint.Task.ID || recovered.OfferRef != sess.ExternalRef {
		return binding, errors.New("restored workspace task authority does not match")
	}
	binding = session.WorkspaceTaskBinding{QueueID: recovered.QueueID, TaskID: recovered.TaskID, ID: recovered.ID,
		OfferRef: recovered.OfferRef, DraftSHA256: recovered.DraftSHA256}
	sess.WorkspaceTask = &binding
	projection, _, subtasks, err := checkpointTaskProjection(ctx, sess)
	if err != nil || projection.StateSHA256 != manifest.TaskProjection.StateSHA256 ||
		projection.State != manifest.TaskProjection.State || !slices.Equal(subtasks, checkpoint.Task.Subtasks) {
		return binding, errors.New("restored workspace task projection does not match")
	}
	candidate := checkpointSHA256(mustCheckpointJSON(struct {
		Base      string                                     `json:"base"`
		Committed string                                     `json:"committed"`
		Tree      string                                     `json:"tree"`
		Untracked []workerproto.WorkspaceCheckpointFileEntry `json:"untracked"`
	}{checkpoint.BaseRevision, checkpoint.CommittedRevision, manifest.TrackedTree, manifest.UntrackedFiles}))
	if candidate != checkpoint.CandidateTreeSHA256 {
		return binding, fmt.Errorf("restored workspace candidate identity does not match")
	}
	return binding, nil
}
