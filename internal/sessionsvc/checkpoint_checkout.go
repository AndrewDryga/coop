package sessionsvc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// CheckoutFleetResult verifies a controller-exported checkpoint and creates an independent
// repository. No files, refs or index entries of the operator's source repository are written.
func CheckoutFleetResult(ctx context.Context, offer, source, parent string, wire io.Reader) (string, error) {
	return checkoutFleetResult(ctx, offer, source, parent, wire, defaultCheckpointReserve)
}

func checkoutFleetResult(ctx context.Context, offer, source, parent string, wire io.Reader, reserve func(int64) int64) (string, error) {
	checkpoint, input, err := readFleetCheckpoint(wire)
	if err != nil {
		return "", err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return "", err
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return "", err
	}
	watched, stop, err := checkpointDiskContext(ctx, []string{parent}, checkpoint.Bundle.ByteSize, reserve)
	if err != nil {
		return "", err
	}
	defer stop()
	ctx = watched
	stage, err := os.MkdirTemp(parent, ".coop-result-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	manifest, members, err := stageCheckpointMembers(ctx, stage, checkpoint, input)
	if err != nil {
		return "", err
	}
	if manifest.Repository == nil {
		return "", errors.New("this checkpoint has no portable Git repository")
	}

	repository, err := os.MkdirTemp(parent, "coop-result-")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(repository)
		}
	}()
	if err := initializeCheckpointRepository(ctx, source, repository, checkpoint.BaseRevision); err != nil {
		return "", fmt.Errorf("initializing from local base commit %s: %w", checkpoint.BaseRevision, err)
	}
	if upstream := manifest.UpstreamRevision; upstream != "" {
		if err := forkspace.GitFetchPinnedContext(ctx, source, repository, upstream); err != nil {
			return "", fmt.Errorf("fetch upstream commit %s into the local source and retry checkout: %w", upstream, err)
		}
	}
	archive, err := os.Open(members[manifest.Repository.Entry])
	if err != nil {
		return "", err
	}
	err = readCheckpointRepository(ctx, repository, archive)
	if err := errors.Join(err, archive.Close()); err != nil {
		return "", err
	}
	if err := validateCheckpointRepository(ctx, repository, checkpoint, manifest); err != nil {
		return "", err
	}
	if err := materializeCheckpoint(ctx, source, repository, repository, checkpoint, manifest, members, operatorCheckoutSource); err != nil {
		return "", err
	}
	binding, err := verifyStreamedCheckpoint(ctx, session.Session{Workspace: repository, ExternalRef: offer}, checkpoint, manifest)
	if err != nil {
		return "", err
	}
	if err := excludeWorkspaceTaskMetadata(repository, binding.ID); err != nil {
		return "", err
	}
	complete = true
	return repository, nil
}

// The operator's clone is an object donor. Its index, checked out revisions and
// uncommitted files are independent of the pinned job result. The archive has
// already proved the top-level gitlinks match the approved base.
func materializeFleetResultTree(ctx context.Context, source, destination, tree string, objects ...string) error {
	if err := forkspace.HydrateLFS(ctx, destination, tree, append([]string{source}, objects...)...); err != nil {
		return err
	}
	return checkoutFleetSubmodules(ctx, source, destination, tree, 0)
}

func checkoutFleetSubmodules(ctx context.Context, source, destination, tree string, depth int) error {
	links, err := forkspace.Gitlinks(ctx, destination, tree)
	if err != nil {
		return err
	}
	if depth >= 16 && len(links) > 0 {
		return errSessionSubmodulesTooDeep
	}
	for path, commit := range links {
		donor, err := forkspace.SubmoduleDirectory(source, path)
		if err != nil {
			return err
		}
		metadata, err := os.Lstat(filepath.Join(donor, ".git"))
		if err != nil || !(metadata.IsDir() || metadata.Mode().IsRegular()) {
			return fmt.Errorf("initialize local submodule %q with commit %s and retry checkout", path, commit)
		}
		child, err := forkspace.SubmoduleDirectory(destination, path)
		if err != nil {
			return err
		}
		if err := initializeCheckpointRepository(ctx, donor, child, commit); err != nil {
			return fmt.Errorf("copy local submodule %q at %s: %w", path, commit, err)
		}
		if _, _, err := runSessionCompanionGitContext(ctx, child, sessionWorkspaceGitOutputLimit, "reset", "--hard", commit); err != nil {
			return err
		}
		if err := forkspace.HydrateLFS(ctx, child, commit, donor); err != nil {
			return err
		}
		if err := checkoutFleetSubmodules(ctx, donor, child, commit, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func readFleetCheckpoint(wire io.Reader) (workerproto.WorkspaceCheckpoint, io.Reader, error) {
	input := bufio.NewReaderSize(wire, 1<<20)
	header, err := input.ReadSlice('\n')
	if err != nil {
		return workerproto.WorkspaceCheckpoint{}, nil, errors.New("missing fleet checkpoint descriptor")
	}
	checkpoint, err := workerproto.DecodeWorkspaceCheckpoint(header)
	return checkpoint, input, err
}
