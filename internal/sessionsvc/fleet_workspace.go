package sessionsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/safefile"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/shadowpath"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// CaptureFleetWorkspace writes a complete, content-addressable operator archive.
// No bytes leave the private staging directory until the second capture agrees.
func CaptureFleetWorkspace(ctx context.Context, source, job, repositoryRef, base string, selected []string, out io.Writer) (workerproto.BodyReference, error) {
	return captureFleetWorkspace(ctx, source, job, repositoryRef, base, selected, out, defaultCheckpointReserve)
}

func captureFleetWorkspace(ctx context.Context, source, job, repositoryRef, base string, selected []string, out io.Writer, reserve func(int64) int64) (workerproto.BodyReference, error) {
	var ref workerproto.BodyReference
	source, err := filepath.Abs(source)
	if err != nil {
		return ref, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return ref, err
	}
	if _, home := shadowpath.HomeOrAbove(source); home {
		return ref, errors.New("choose a repository below the home directory or outside it")
	}
	if info, err := os.Lstat(filepath.Join(source, ".git")); err != nil || !(info.IsDir() || info.Mode().IsRegular()) {
		return ref, errors.New("choose the root of a Git working copy")
	}
	if !forkspace.ValidObjectID(base) {
		return ref, errors.New("the fleet input base must be an exact commit ID")
	}
	head, err := sessionWorkspaceCommitContext(ctx, source, "HEAD")
	if err != nil {
		return ref, err
	}
	if _, _, err := runSessionWorkspaceGitWithEnvContext(ctx, source, 128, nil, "merge-base", "--is-ancestor", base, head); err != nil {
		return ref, fmt.Errorf("local HEAD must descend from base %s: %w", base, err)
	}
	stage, err := os.MkdirTemp("", ".coop-fleet-input-")
	if err != nil {
		return ref, err
	}
	defer os.RemoveAll(stage)
	ctx, stop, err := checkpointDiskContext(ctx, []string{stage}, 0, reserve)
	if err != nil {
		return ref, err
	}
	defer stop()
	root, err := os.Open(source)
	if err != nil {
		return ref, err
	}
	defer root.Close()
	selected = slices.Sorted(slices.Values(selected))
	collect := func(copyFiles bool) ([]checkpointBlob, error) {
		return captureFleetUntracked(ctx, root, source, stage, selected, copyFiles)
	}
	files, err := collect(true)
	if err != nil {
		return ref, err
	}
	repository, err := os.CreateTemp(stage, "objects-")
	if err != nil {
		return ref, err
	}
	defer repository.Close()
	hash := sha256.New()
	count := checkpointCountWriter{writer: io.MultiWriter(hash, repository)}
	m := workerproto.FleetWorkspaceInput{Version: 1, JobRef: job, RepositoryRef: repositoryRef, BaseRevision: base, CommittedRevision: head, UntrackedFiles: checkpointEntries(files)}
	if err := validateFleetCaptureState(ctx, source, head, 0); err != nil {
		return ref, err
	}
	checkPath := fleetInputPathPolicy(source)
	checked := make(map[string]struct{})
	allow := func(path string) error {
		if _, seen := checked[path]; seen {
			return nil
		}
		if err := checkPath(path); err != nil {
			return err
		}
		checked[path] = struct{}{}
		return nil
	}
	if err := withSessionTrackedTree(ctx, source, func(tree string, env []string) error {
		m.TrackedTree = tree
		return writePortableRepository(ctx, source, base, "", head, tree, env, &count, allow)
	}); err != nil {
		return ref, err
	}
	m.Repository = workerproto.WorkspaceCheckpointBundleEntry{Entry: "repository.tar", ByteSize: count.bytes, SHA256: fmt.Sprintf("%x", hash.Sum(nil))}
	if err := m.Validate(); err != nil {
		return ref, err
	}
	manifest, err := json.Marshal(m)
	if err != nil {
		return ref, err
	}
	if len(manifest) > workerproto.MaxWorkspaceCheckpointManifest {
		return ref, errors.New("workspace input manifest exceeds its bound")
	}
	archive, body, err := writeWorkspaceCheckpointTar(ctx, manifest, repository, m.Repository, files, nil)
	if err != nil {
		return ref, err
	}
	defer archive.Close()
	if err := validateFleetCaptureState(ctx, source, head, 0); err != nil {
		return ref, err
	}
	if err := withSessionTrackedTree(ctx, source, func(tree string, _ []string) error {
		if tree != m.TrackedTree {
			return errors.New("tracked files changed during capture; retry")
		}
		return nil
	}); err != nil {
		return ref, err
	}
	finalPolicy := fleetInputPathPolicy(source)
	for path := range checked {
		if err := finalPolicy(path); err != nil {
			return ref, err
		}
	}
	after, err := collect(false)
	if err != nil {
		return ref, err
	}
	if !checkpointFileEntriesEqual(checkpointEntries(files), checkpointEntries(after)) {
		return ref, errors.New("selected files changed during capture; retry")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return ref, err
	}
	if _, err := io.Copy(out, reviewScanReader{ctx, archive}); err != nil {
		return ref, err
	}
	return workerproto.BodyReference{ByteSize: body.ByteSize, SHA256: body.SHA256}, nil
}

func fleetInputPathPolicy(source string) func(string) error {
	hidden := shadowpath.NewDecider(source)
	return func(path string) error {
		if !filepath.IsLocal(path) || filepath.Clean(path) != path || path == "." || strings.Contains(path, "\\") {
			return errors.New("invalid workspace input path")
		}
		for _, part := range strings.Split(path, "/") {
			if strings.EqualFold(part, ".git") {
				return errors.New("cannot submit Git metadata as working files")
			}
		}
		if strings.EqualFold(path, forkspace.GenerationMarkerName) || strings.EqualFold(path, tasks.TasksRoot) || strings.HasPrefix(strings.ToLower(path), strings.ToLower(tasks.TasksRoot)+"/") {
			return fmt.Errorf("workspace custody metadata cannot be submitted: %q", path)
		}
		h, err := hidden(path)
		if err != nil {
			return err
		}
		if h {
			return fmt.Errorf("workspace input %q is hidden by Coop's file policy", path)
		}
		return nil
	}
}

func validateFleetCaptureState(ctx context.Context, source, head string, depth int) error {
	current, err := sessionWorkspaceCommitContext(ctx, source, "HEAD")
	if err != nil || current != head {
		return errors.New("source revision changed or submodule is not at its pin")
	}
	if depth == 0 {
		data, truncated, err := runSessionWorkspaceGitWithEnvContext(ctx, source, 1, nil, "ls-files", "--unmerged", "-z")
		if err != nil {
			return err
		}
		if truncated || len(data) > 0 {
			return errors.New("resolve Git conflicts before capturing the workspace")
		}
	} else {
		status, truncated, err := sessionPinnedStatusContext(ctx, session.CompanionRepository{Workspace: source, BaseCommit: head})
		if err != nil {
			return err
		}
		if truncated || len(status) > 0 {
			return errors.New("local submodule changes require separate custody")
		}
	}
	links, err := forkspace.Gitlinks(ctx, source, head)
	if err != nil {
		return err
	}
	if depth >= 16 && len(links) > 0 {
		return errSessionSubmodulesTooDeep
	}
	for path, commit := range links {
		child, err := forkspace.SubmoduleDirectory(source, path)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(child, ".git")); err != nil {
			return fmt.Errorf("initialize submodule %q to capture this workspace: %w", path, err)
		}
		if err := validateFleetCaptureState(ctx, child, commit, depth+1); err != nil {
			return fmt.Errorf("submodule %q: %w", path, err)
		}
	}
	return nil
}

func captureFleetUntracked(ctx context.Context, root *os.File, source, stage string, selected []string, copyFiles bool) ([]checkpointBlob, error) {
	if len(selected) > workerproto.MaxWorkspaceCheckpointFiles {
		return nil, errors.New("too many selected untracked files")
	}
	allow := fleetInputPathPolicy(source)
	files := make([]checkpointBlob, 0, len(selected))
	for i, path := range selected {
		if i > 0 && selected[i-1] == path {
			return nil, fmt.Errorf("duplicate selected path %q", path)
		}
		if err := allow(path); err != nil {
			return nil, err
		}
		listing, truncated, err := runSessionWorkspaceGitWithEnvContext(ctx, source, len(path)+2, []string{"GIT_LITERAL_PATHSPECS=1"}, "ls-files", "--others", "--exclude-standard", "-z", "--", path)
		if err != nil {
			return nil, err
		}
		if truncated || !bytes.Equal(listing, append([]byte(path), 0)) {
			return nil, fmt.Errorf("%q must be an unignored untracked file", path)
		}
		parent, policy, err := shadowpath.OpenTree(root, source, filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		if policy.Shadowed(path) {
			parent.Close()
			return nil, fmt.Errorf("selected file %q is hidden", path)
		}
		file, err := safefile.OpenRegular(parent, filepath.Base(path))
		parent.Close()
		if err != nil {
			return nil, err
		}
		blob, err := copyFleetInputFile(ctx, file, path, stage, i, copyFiles)
		err = errors.Join(err, file.Close())
		if err != nil {
			return nil, err
		}
		files = append(files, blob)
	}
	return files, nil
}

func copyFleetInputFile(ctx context.Context, file *os.File, path, stage string, ordinal int, copyFile bool) (checkpointBlob, error) {
	var blob checkpointBlob
	info, err := file.Stat()
	if err != nil {
		return blob, err
	}
	hash := sha256.New()
	var output io.Writer = hash
	var dst *os.File
	if copyFile {
		dst, err = os.CreateTemp(stage, "untracked-")
		if err != nil {
			return blob, err
		}
		blob.path = dst.Name()
		output = io.MultiWriter(hash, dst)
	}
	err = copyCheckpointContent(ctx, output, io.LimitReader(file, info.Size()+1), info.Size(), "")
	if dst != nil {
		err = errors.Join(err, dst.Close())
	}
	if err != nil {
		return blob, err
	}
	mode := int64(0644)
	if info.Mode()&0111 != 0 {
		mode = 0755
	}
	blob.entry = workerproto.WorkspaceCheckpointFileEntry{Entry: fmt.Sprintf("untracked/%06d", ordinal), PathB64: base64.StdEncoding.EncodeToString([]byte(path)), Mode: mode, ByteSize: info.Size(), SHA256: fmt.Sprintf("%x", hash.Sum(nil))}
	return blob, nil
}
