package sessionsvc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

// materializeSessionSubmodules is shared by primary, companion and review
// checkouts. Only independently owned, credential-free Git directories cross
// into a sandbox; Git never follows repository-supplied submodule URLs.
func materializeSessionSubmodules(ctx context.Context, source, workspace, commit string, lfsSources ...string) error {
	sourceHead, err := sessionWorkspaceCommitContext(ctx, source, "HEAD")
	if err != nil {
		return err
	}
	pinned, err := forkspace.Gitlinks(ctx, source, sourceHead)
	if err != nil {
		return err
	}
	links, err := forkspace.Gitlinks(ctx, workspace, commit)
	if err != nil || !maps.Equal(pinned, links) {
		return errors.New("changed submodules require separately authorized source and publication")
	}
	if err := forkspace.HydrateLFS(ctx, workspace, commit, append([]string{source}, lfsSources...)...); err != nil {
		return err
	}
	for _, path := range slices.Sorted(maps.Keys(links)) {
		childSource, err := forkspace.SubmoduleDirectory(source, path)
		if err != nil {
			return err
		}
		child, err := forkspace.SubmoduleDirectory(workspace, path)
		if err != nil {
			return err
		}
		if err := verifyPinnedSubmodule(ctx, childSource, links[path]); err != nil {
			return err
		}
		entries, err := os.ReadDir(child)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := cloneSessionSubmodule(ctx, childSource, child, links[path]); err != nil {
				return err
			}
			continue // The unpublished clone was populated recursively before rename.
		}
		if err := verifyPinnedSubmodule(ctx, child, links[path]); err != nil {
			return fmt.Errorf("refusing to replace an existing submodule: %w", err)
		}
		if err := materializeSessionSubmodules(ctx, childSource, child, links[path]); err != nil {
			return err
		}
	}
	return nil
}

func cloneSessionSubmodule(ctx context.Context, source, destination, commit string) error {
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".coop-submodule-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := forkspace.GitCopyPinnedHistoryContext(ctx, source, stage, commit); err != nil {
		return err
	}
	if err := forkspace.PropagateGitIdentityContext(ctx, source, stage); err != nil {
		return err
	}
	pin := forkspace.GitRefCommand(ctx, stage, "update-ref", "--no-deref", "HEAD", commit)
	pin.Env = sessionCompanionGitEnv()
	if err := pin.Run(); err != nil {
		return err
	}
	if _, _, err := runSessionWorkspaceGitWithEnvContext(ctx, stage, sessionWorkspaceGitOutputLimit,
		sessionCompanionCheckoutGitEnv(), "reset", "--hard", "--quiet", commit); err != nil {
		return err
	}
	if err := materializeSessionSubmodules(ctx, source, stage, commit); err != nil {
		return err
	}
	// Darwin does not replace even an empty directory with rename. Remove only
	// the empty checkout placeholder; Remove refuses newly arrived content.
	if err := os.Remove(destination); err != nil {
		return err
	}
	return os.Rename(stage, destination)
}

func verifySessionSubmodules(ctx context.Context, repository, commit string) error {
	return verifySessionSubmodulesDepth(ctx, "", repository, commit, 0)
}

func verifyUnchangedSessionSubmodules(ctx context.Context, source, repository, commit string) error {
	return verifySessionSubmodulesDepth(ctx, source, repository, commit, 0)
}

func verifySessionSubmodulesDepth(ctx context.Context, source, repository, commit string, depth int) error {
	links, err := forkspace.Gitlinks(ctx, repository, commit)
	if err != nil {
		return err
	}
	if source != "" {
		head, err := sessionWorkspaceCommitContext(ctx, source, "HEAD")
		if err != nil {
			return err
		}
		pinned, err := forkspace.Gitlinks(ctx, source, head)
		if err != nil || !maps.Equal(pinned, links) {
			return errors.New("changed submodules require separately authorized source and publication")
		}
	}
	if depth >= 16 && len(links) != 0 {
		return errors.New("submodules exceed the authorized nesting depth")
	}
	for path, head := range links {
		child, err := forkspace.SubmoduleDirectory(repository, path)
		if err != nil {
			return err
		}
		if err := verifyPinnedSubmodule(ctx, child, head); err != nil {
			return fmt.Errorf("submodule %q: %w", path, err)
		}
		childSource := ""
		if source != "" {
			childSource, err = forkspace.SubmoduleDirectory(source, path)
			if err != nil {
				return err
			}
		}
		if err := verifySessionSubmodulesDepth(ctx, childSource, child, head, depth+1); err != nil {
			return fmt.Errorf("submodule %q: %w", path, err)
		}
	}
	return nil
}

func verifyPinnedSubmodule(ctx context.Context, repository, commit string) error {
	metadata := filepath.Join(repository, ".git")
	info, err := os.Lstat(metadata)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("submodule must own its Git directory")
	}
	if _, err := os.Lstat(filepath.Join(metadata, "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("submodule must own its Git objects")
	}
	if _, err := os.Lstat(filepath.Join(metadata, "commondir")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("submodule must not redirect its Git common directory")
	}
	objects, err := os.Lstat(filepath.Join(metadata, "objects"))
	if err != nil || !objects.IsDir() || objects.Mode()&os.ModeSymlink != 0 {
		return errors.New("submodule must own its Git objects")
	}
	head, err := sessionWorkspaceCommitContext(ctx, repository, "HEAD")
	if err != nil || head != commit {
		return errors.New("submodule HEAD no longer matches its pinned gitlink")
	}
	status, truncated, err := sessionPinnedStatusContext(ctx, session.CompanionRepository{Workspace: repository, BaseCommit: commit})
	if err != nil || truncated || len(status) != 0 {
		return errors.New("submodule contains uncommitted work")
	}
	return nil
}
