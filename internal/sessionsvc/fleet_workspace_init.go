package sessionsvc

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// The ordinary workspace-task operation holds the session's mutation fence.
// Its durable task binding is written only after the complete input is installed.
func (s *Service) initializeFleetWorkspace(ctx context.Context, sess session.Session) error {
	job, err := sessionJobSpec(sess)
	if err != nil {
		return err
	}
	if s.workspaceInputFetcher == nil || job.WorkspaceInput == nil || job.Source == nil {
		return checkpointRefusal("the worker cannot fetch the submitted workspace")
	}
	ref := *job.WorkspaceInput
	watched, stop, err := s.checkpointDiskContext(ctx, sess.Workspace, ref.ByteSize)
	if err != nil {
		return err
	}
	defer stop()
	ctx = watched
	root := filepath.Join(s.stateRoot, "workspace-checkpoints", ".staging")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, "initial-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	body, err := os.CreateTemp(stage, "input-")
	if err != nil {
		return err
	}
	defer body.Close()
	if err := s.workspaceInputFetcher(ctx, job.JobRef, ref, body); err != nil {
		return err
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	members := map[string]string{}
	m, err := workerproto.ReadFleetWorkspaceInput(ref, body, func(header *tar.Header, input io.Reader) error {
		f, err := os.CreateTemp(stage, "member-")
		if err != nil {
			return err
		}
		members[header.Name] = f.Name()
		return errors.Join(copyCheckpointContent(ctx, f, input, header.Size, ""), f.Close())
	})
	if err != nil {
		return err
	}
	if m.JobRef != sess.ExternalRef || m.RepositoryRef != job.Source.RepositoryRef || m.BaseRevision != job.Source.Binding.SelectedCommit {
		return checkpointRefusal("submitted workspace does not match the task, repository and base")
	}
	objects := filepath.Join(stage, "repository")
	if err := initializeCheckpointRepository(ctx, sess.Repository, objects, m.BaseRevision); err != nil {
		return err
	}
	archive, err := os.Open(members[m.Repository.Entry])
	if err != nil {
		return err
	}
	err = readCheckpointRepository(ctx, objects, archive)
	if err := errors.Join(err, archive.Close()); err != nil {
		return err
	}
	if err := validatePortableRepository(ctx, objects, m.BaseRevision, m.CommittedRevision, m.TrackedTree, ""); err != nil {
		return err
	}
	if err := validateFleetInputPaths(ctx, job, sess.Repository, objects, m); err != nil {
		return err
	}
	// Reconstruct and verify in quarantine first. A corrupt tree, a conflicting
	// untracked path or an unavailable LFS object cannot alter the session.
	for _, workspace := range []string{objects, sess.Workspace} {
		if workspace == sess.Workspace {
			branch, err := sessionWorkspaceBranchContext(ctx, workspace)
			if err != nil || branch != sess.ForkName {
				return checkpointRefusal("workspace input requires its assigned Git branch")
			}
		}
		if err := materializePortableTree(ctx, sess.Repository, objects, workspace, m.BaseRevision, m.CommittedRevision, m.TrackedTree, managedWorkerSource); err != nil {
			return err
		}
		if err := writeFleetSelected(ctx, workspace, m.UntrackedFiles, members); err != nil {
			return err
		}
		if err := withSessionTrackedTree(ctx, workspace, func(tree string, _ []string) error {
			if tree != m.TrackedTree {
				return errors.New("restored workspace input tree differs from its manifest")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return s.validateSessionForkAuthority(ctx, sess)
}

func validateFleetInputPaths(ctx context.Context, job workerproto.JobSpec, source, objects string, m workerproto.FleetWorkspaceInput) error {
	policy := fleetInputPathPolicy(source)
	allow := func(path string) error {
		if err := policy(path); err != nil {
			return err
		}
		if path == ".agent/project.yaml" || path == ".tool-versions" || job.Runtime != nil && (path == job.Runtime.Dockerfile || path == job.Runtime.Compose) {
			return fmt.Errorf("local change to %q requires the project setup to be approved at the source commit", path)
		}
		return nil
	}
	for _, f := range m.UntrackedFiles {
		if err := allow(string(f.PathBytes)); err != nil {
			return err
		}
	}
	return visitCheckpointTrees(ctx, objects, m.BaseRevision, m.CommittedRevision, m.TrackedTree, func(tree string) error {
		raw, truncated, err := runSessionCompanionGitContext(ctx, objects, sessionWorkspaceGitOutputLimit, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", m.BaseRevision, tree, "--")
		if err != nil {
			return err
		}
		if truncated {
			return errors.New("changed workspace input paths exceed their bound")
		}
		for path := range bytes.SplitSeq(raw, []byte{0}) {
			if len(path) > 0 {
				if err := allow(string(path)); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func writeFleetSelected(ctx context.Context, workspace string, files []workerproto.WorkspaceCheckpointFileEntry, members map[string]string) error {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range files {
		// A selected file must live in the main workspace, never inside a
		// submodule's independent repository or under a symlink to one.
		for parent := filepath.Dir(string(f.PathBytes)); parent != "."; parent = filepath.Dir(parent) {
			if _, err := root.Lstat(filepath.Join(parent, ".git")); !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("workspace input path crosses a submodule: %q", f.PathBytes)
			}
		}
		src, err := os.Open(members[f.Entry])
		if err != nil {
			return err
		}
		err = writeRestoredCheckpointStream(root, f.PathBytes, f.Mode, reviewScanReader{ctx, src})
		if err := errors.Join(err, src.Close()); err != nil {
			return err
		}
	}
	return restoreTaskStateDirs(root)
}

// Compare the submitted HEAD, index, working tree and untracked files. Task
// metadata and its artifacts have separate custody in the checkpoint.
func fleetWorkingSourceDigest(ctx context.Context, sess session.Session, head string) (string, error) {
	if sess.WorkspaceTask == nil || !strings.HasPrefix(sess.WorkspaceTask.OfferRef, "operator-fleet-job:") {
		return "", checkpointRefusal("verification requires a bound operator workspace task")
	}
	if err := verifyUnchangedSessionSubmodules(ctx, sess.Repository, sess.Workspace, head); err != nil {
		return "", err
	}
	index, truncated, err := runSessionCompanionGitContext(ctx, sess.Workspace, 128, "write-tree")
	if err != nil || truncated {
		return "", checkpointRefusal("cannot verify the workspace index")
	}
	item, found, err := tasks.CurrentTask(filepath.Join(sess.Workspace, tasks.TasksRoot), sess.WorkspaceTask.ID)
	if err != nil || !found {
		return "", checkpointRefusal("the verification task is missing")
	}
	prefix := []byte(filepath.ToSlash(filepath.Join(tasks.TasksRoot, item.State, item.ID, "artifacts")) + "/")
	files, err := workspaceUntrackedFiles(ctx, sess.Workspace, func(path []byte) bool { return bytes.HasPrefix(path, prefix) })
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	err = withSessionTrackedTree(ctx, sess.Workspace, func(tree string, _ []string) error {
		fmt.Fprintf(digest, "%s\n%s\n%s\n", head, strings.TrimSpace(string(index)), tree)
		for _, file := range files {
			fmt.Fprintf(digest, "%s %d %s\n", file.entry.PathB64, file.entry.Mode, file.entry.SHA256)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}
