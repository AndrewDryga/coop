package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

func setupForkBoundary(cfg *config.Config, repo, name string, isolated bool) error {
	if err := cloneForkBoundary(repo, name, isolated); err != nil {
		return err
	}
	// Creating one is allowed, as it always was; its loop and merge are what refuse it, so say it
	// now rather than after an agent has worked in it.
	if tasks.ShallowCheckout(repo) {
		ui.Warn("this checkout is a shallow clone, so fork %s cannot be looped or merged until it has the full history: run git fetch --unshallow here, then start a new fork", name)
	}
	ws := forkspace.Workspace(repo, name)
	if err := box.SnapshotForkApprovals(context.Background(), cfg, repo, ws); err != nil {
		return errors.Join(fmt.Errorf("copy approvals into new fork: %w", err), os.RemoveAll(ws))
	}
	return nil
}

func cloneForkBoundary(repo, name string, isolated bool) error {
	if !isolated {
		_, err := forkspace.Setup(repo, name)
		return err
	}
	output, err := forkspace.ObserveGit(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("capture isolated fork base: %w", err)
	}
	// Git emits exactly one object ID followed by a newline.
	commit := string(output)
	if len(commit) > 0 && commit[len(commit)-1] == '\n' {
		commit = commit[:len(commit)-1]
	}
	_, err = forkspace.SetupIsolatedContext(context.Background(), repo, name, commit)
	return err
}

func bindForkBoundary(repo, name string, isolated bool) (forkspace.Identity, error) {
	if err := forkspace.RefuseImmutablePrimaryMutation(forkspace.Workspace(repo, name)); err != nil {
		return forkspace.Identity{}, err
	}
	if isolated {
		return forkspace.EnsureIsolatedGenerationLocked(repo, name)
	}
	return forkspace.EnsureGenerationLocked(repo, name)
}

func forkBoundaryMode(repo, name string, requested, fresh bool) (bool, error) {
	if err := forkspace.RefuseImmutablePrimaryMutation(forkspace.Workspace(repo, name)); err != nil {
		return false, err
	}
	identity, present, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return false, err
	}
	exists := pathExists(forkspace.Workspace(repo, name))
	if !present && exists {
		identity, present, err = recoverForkBoundary(repo, name)
		if err != nil {
			return false, err
		}
	}
	var isolated bool
	if present {
		isolated, err = forkspace.IsolatedGeneration(repo, identity)
		if err != nil {
			return false, err
		}
	}
	if exists && requested && !isolated && !fresh {
		return false, fmt.Errorf("fork %s is not isolated — create a new fork with --isolated; its existing work is unchanged", name)
	}
	// Read surviving authority before orphan cleanup, even when the workspace is absent.
	return requested || isolated, nil
}

func recoverForkBoundary(repo, name string) (forkspace.Identity, bool, error) {
	if _, err := os.Lstat(filepath.Join(forkspace.Workspace(repo, name), forkspace.GenerationMarkerName)); errors.Is(err, os.ErrNotExist) {
		return forkspace.Identity{}, false, nil
	} else if err != nil {
		return forkspace.Identity{}, false, err
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return forkspace.Identity{}, false, err
	}
	defer unlock()
	identity, err := forkspace.EnsureGenerationLocked(repo, name)
	return identity, err == nil, err
}
