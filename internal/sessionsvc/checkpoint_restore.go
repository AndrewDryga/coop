package sessionsvc

import (
	"archive/tar"
	"bytes"
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
		return fail(&session.Error{Code: session.CodeInvalidRequest, Detail: "restore session and revision are required"})
	}
	if err := req.Checkpoint.Validate(); err != nil {
		return fail(&session.Error{Code: session.CodeInvalidRequest, Detail: "checkpoint descriptor is invalid: " + err.Error()})
	}
	release, ok := s.beginWorkspaceMutation(ctx, req.SessionID)
	if !ok {
		return fail(&session.Error{Code: session.CodeInvalidSessionState, Detail: "restore requires a parked runtime"})
	}
	defer release()
	if pending, err := s.store.HasPendingWorkspaceRestore(ctx, req.SessionID, op.ID); err != nil {
		return fail(err)
	} else if pending {
		return fail(checkpointRefusal("another checkpoint restore must finish first"))
	}
	sess, err := s.store.GetSession(ctx, req.SessionID)
	if err != nil {
		return fail(err)
	}
	if err := s.validateRestoreWorkspaceSession(ctx, sess, req, op.State != session.OperationReserved); err != nil {
		return fail(err)
	}
	// A refusal returns no context, and fail records it with the caller's own.
	watched, stopDiskWatch, err := s.checkpointDiskContext(ctx, sess.Workspace, req.Checkpoint.Bundle.ByteSize)
	if err != nil {
		return fail(err)
	}
	defer stopDiskWatch()
	ctx = watched
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
	if manifest.UpstreamRevision != "" {
		recovering := op.State == session.OperationRunning || op.State == session.OperationUncertain
		if err := s.fetchCheckpointUpstream(ctx, sess, repository, req.Checkpoint.BaseRevision, manifest.UpstreamRevision, recovering); err != nil {
			return fail(err)
		}
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
		return fail(checkpointRefusal("session is already bound to another restored workspace task"))
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
		return fail(checkpointRefusal("replacement workspace branch authority changed"))
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
	if err := excludeWorkspaceTaskMetadata(sess.Workspace, binding.ID); err != nil {
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

// fetchCheckpointUpstream obtains the upstream commit a checkpoint left out, from this worker's
// own copy of the job's source and only after the controller-granted refresh proves the commit is
// in the source's default history now. A commit the source no longer has, or a refresh that
// cannot run, refuses the restore before any workspace file changes. Only a refresh that could
// not run, or a copy Git could not read, is worth a retry: once the refresh brought the current
// default into the copy (full history, never shallow), a commit missing from it or outside the
// default's history is gone from that branch, and retrying would only spend another refresh until
// the branch holds it again; then a new key restores into the same unused session.
//
// A resumed restore (recovering) proved all of that before its operation was marked running, and
// the journaled request pins the same bundle. It checks only that the private copy still holds the
// commit on top of the base: requiring the network again would fence the session for good if the
// default branch were force-pushed between the crash and the retry.
func (s *Service) fetchCheckpointUpstream(ctx context.Context, sess session.Session, repository, base, upstream string, recovering bool) error {
	unavailable := func(why string) error {
		if err := context.Cause(ctx); err != nil {
			return err // the caller left; the operation stays unsettled for a same-key retry
		}
		return &session.Error{Code: session.CodeRepositoryUnavailable,
			Detail: "checkpoint needs upstream commit " + upstream + " from the job's source, " + why}
	}
	gone := func(why string) error {
		if recovering {
			return unavailable(why)
		}
		return &session.Error{Code: session.CodeInvalidSessionState,
			Detail: "checkpoint needs upstream commit " + upstream + ", which the job's default branch no longer holds; this key keeps that answer, and a new key can restore into the same unused session once the branch holds the commit again"}
	}
	pairs := [][2]string{{base, upstream}}
	if !recovering {
		job, err := sessionJobSpec(sess)
		if err != nil {
			return &session.Error{Code: session.CodeInvalidSessionState, Detail: "checkpoint needs upstream commit " + upstream + ", but the session has no job authority"}
		}
		head, err := s.refreshSessionSource(ctx, job, sess.Repository)
		var refusal *session.Error
		if errors.As(err, &refusal) && refusal.Code == session.CodeInvalidSessionState {
			// The same final answer the service gives wherever the grant is refused.
			return &session.Error{Code: session.CodeInvalidSessionState, Detail: "checkpoint needs upstream commit " + upstream + ": " + refusal.Detail}
		}
		if err != nil {
			return unavailable("which could not be refreshed")
		}
		pairs = append(pairs, [2]string{upstream, head})
	}
	// Only git's own clean "no such object" answer is final; a lookup that failed for any other
	// reason (no git, a full descriptor table, a corrupt pack) proves nothing about the source.
	if checkpointCommitMissing(ctx, sess.Repository, upstream) {
		return gone("which does not have it")
	}
	if actual, err := sessionWorkspaceCommitContext(ctx, sess.Repository, upstream); err != nil || actual != upstream {
		return unavailable("whose copy could not be read")
	}
	for _, pair := range pairs {
		check, err := forkspace.GitCommandWithEnv(ctx, sess.Repository, sessionCompanionGitEnv(), "merge-base", "--is-ancestor", pair[0], pair[1])
		if err != nil {
			return unavailable("whose copy could not be read")
		}
		var exit *exec.ExitError
		if err := check.Run(); errors.As(err, &exit) && exit.ExitCode() == 1 {
			return gone("whose default history no longer contains it")
		} else if err != nil {
			return unavailable("whose copy could not be read")
		}
	}
	if err := forkspace.GitFetchPinnedContext(ctx, sess.Repository, repository, upstream); err != nil {
		return unavailable("whose copy could not be read")
	}
	return nil
}

// checkpointCommitMissing reports git's answer for an object the repository does not hold:
// `cat-file -e` exits 1 and prints nothing. A failed start, a signal, exit 128 or any message
// (a corrupt pack, an unreadable object) is not that answer.
func checkpointCommitMissing(ctx context.Context, repository, commit string) bool {
	check, err := forkspace.GitCommandWithEnv(ctx, repository, append(sessionCompanionGitEnv(), "LC_ALL=C"), "cat-file", "-e", commit)
	if err != nil {
		return false
	}
	var stderr bytes.Buffer
	check.Stdout, check.Stderr = nil, &stderr
	var exit *exec.ExitError
	return errors.As(check.Run(), &exit) && exit.ExitCode() == 1 && stderr.Len() == 0 && ctx.Err() == nil
}

func validateCheckpointRepository(ctx context.Context, repository string, checkpoint workerproto.WorkspaceCheckpoint, manifest workerproto.WorkspaceCheckpointBundleManifest) error {
	checks := [][]string{
		{"merge-base", "--is-ancestor", checkpoint.BaseRevision, checkpoint.CommittedRevision},
		{"fsck", "--full", "--strict", "--unreachable", "--no-progress", checkpoint.CommittedRevision, manifest.TrackedTree},
	}
	if manifest.UpstreamRevision != "" {
		checks = append(checks, []string{"merge-base", "--is-ancestor", manifest.UpstreamRevision, checkpoint.CommittedRevision})
	}
	for _, args := range checks {
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
			return &session.Error{Code: session.CodeInvalidRequest, Detail: "checkpoint contains submodule changes without separate custody"}
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
				return &session.Error{Code: session.CodeInvalidRequest, Detail: "checkpoint contains an unused LFS object"}
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
			return &session.Error{Code: session.CodeInvalidRequest, Detail: "checkpoint cannot replace fork authority"}
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
	return restoreTaskStateDirs(root)
}

// restoreTaskStateDirs puts back the empty state folders the clean above removed: the bundle
// carries files only, and a queue needs all four, or moving a task into a missing one renames it
// and `coop tasks lint` flags the queue (see tasks.ScaffoldStateDirs). Session create made them;
// whatever stands at one of those paths now (a restored file, a link) is the workspace's own and
// is left as it is.
func restoreTaskStateDirs(root *os.Root) error {
	for _, state := range tasks.TaskStates {
		path := filepath.Join(tasks.TasksRoot, state)
		if _, err := root.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		// MkdirAll: a bundle with no queue file at all leaves no parent folder either.
		if err := root.MkdirAll(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
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
	untracked, err := checkpointUntrackedFiles(ctx, sess.Workspace, checkpoint.Task.ID)
	if err != nil || !checkpointFileEntriesEqual(checkpointEntries(untracked), manifest.UntrackedFiles) {
		return binding, errors.New("restored workspace untracked files do not match")
	}
	recovered, err := tasks.ReadControllerTaskBinding(sess.Workspace, checkpoint.Task.ID)
	if err != nil || recovered.QueueID != checkpoint.Task.QueueID || recovered.TaskID != checkpoint.Task.TaskID ||
		recovered.ID != checkpoint.Task.ID {
		return binding, errors.New("restored workspace task authority does not match")
	}
	if recovered.OfferRef != sess.ExternalRef {
		// The caller created the replacement session for another task: their mistake, not ours.
		return binding, &session.Error{Code: session.CodeInvalidRequest,
			Detail: "checkpoint belongs to another task offer; create the replacement session with the checkpoint's task offer as its task"}
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
