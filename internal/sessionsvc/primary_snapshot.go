package sessionsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

const primarySnapshotVersion = forkspace.ImmutablePrimaryVersion
const primarySnapshotMarker = forkspace.ImmutablePrimaryMarker

// Constructed only from the frozen controller job, never a repository hint.
type primarySnapshotSource struct {
	stateRoot string
	source    workerproto.JobSource
}

type primarySnapshotProjection = forkspace.ImmutablePrimarySnapshot

func (p *primarySnapshotSource) binding(repo, workspace string) session.CompanionRepository {
	return session.CompanionRepository{Name: "primary", Repository: repo, Workspace: workspace, BaseCommit: p.source.Binding.SelectedCommit}
}

func (p *primarySnapshotSource) key(repo string) (string, []string, error) {
	expected, err := stagedJobRepository(p.stateRoot, p.source)
	if err != nil || !sameRealPath(expected, repo) {
		return "", nil, errors.New("read-only primary is outside its frozen source authority")
	}
	base, err := companionSnapshotKey(p.stateRoot, p.binding(repo, ""))
	if err != nil {
		return "", nil, err
	}
	b := p.source.Binding
	roots := []string{b.SelectedCommit, b.DefaultCommit, b.BaseCommit}
	slices.Sort(roots)
	roots = slices.Compact(roots)
	// Companion materialization and its receipt stay unchanged. Primary history
	// additionally owns every frozen comparison root, even on diverged branches.
	data, err := json.Marshal(struct {
		Version    int
		Kind, Base string
		Roots      []string
	}{primarySnapshotVersion, "readonly-primary", base, roots})
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), roots, nil
}

func verifySnapshotHistory(ctx context.Context, workspace string, roots []string) error {
	if len(roots) == 0 {
		return nil
	}
	args := append([]string{"rev-list", "--quiet", "--objects"}, roots...)
	command, err := forkspace.GitCommand(ctx, workspace, args...)
	if err != nil {
		return err
	}
	return command.Run()
}

func createReadOnlyPrimarySnapshot(ctx context.Context, p *primarySnapshotSource, repo, name, workspace string) error {
	key, roots, err := p.key(repo)
	if err != nil {
		return err
	}
	return createCachedSessionSnapshot(ctx, p.stateRoot, p.binding(repo, workspace), key, roots, func(stage string) error {
		if err := forkspace.PrepareProjectedForkContext(ctx, repo, stage, name, p.source.Binding.SelectedCommit); err != nil {
			return err
		}
		b := p.source.Binding
		defaultRef := "refs/remotes/origin/" + strings.TrimPrefix(b.DefaultRef, "refs/heads/")
		for _, args := range [][]string{
			{"update-ref", defaultRef, b.DefaultCommit},
			{"symbolic-ref", "refs/remotes/origin/HEAD", defaultRef},
		} {
			if err := forkspace.GitRefCommand(ctx, stage, args...).Run(); err != nil {
				return err
			}
		}
		if selected := b.SelectedRefValue(); strings.HasPrefix(selected, "refs/heads/") && selected != b.DefaultRef {
			if err := forkspace.GitRefCommand(ctx, stage, "update-ref", "refs/remotes/origin/"+strings.TrimPrefix(selected, "refs/heads/"), b.SelectedCommit).Run(); err != nil {
				return err
			}
		}
		// These private files described a detached companion, not a primary fork.
		for _, marker := range []string{sessionCompanionMarkerFile, sessionCompanionHistoryFile} {
			if err := os.Remove(filepath.Join(stage, ".git", marker)); err != nil {
				return err
			}
		}
		if err := forkspace.RecordImmutablePrimarySnapshot(stage, key); err != nil {
			return err
		}
		_, err := verifySessionWorkspaceContext(ctx, repo, name, stage, b.SelectedCommit)
		return err
	})
}

// Old independent forks have no marker and retain their existing hydration
// path. A marked projection must prove exact ownership before a retry can use
// it; it is verification-only even if a cache entry has since been evicted.
func verifyPrimarySnapshotProjection(ctx context.Context, p *primarySnapshotSource, repo, workspace string) (bool, error) {
	receipt, present, err := forkspace.ReadImmutablePrimarySnapshot(workspace)
	if err != nil {
		return present, err
	}
	if !present {
		if p != nil {
			// Missing provenance must not turn a linked projection into a mutable
			// historical clone. Old independent readonly forks own every inode.
			err = filepath.WalkDir(workspace, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				if info.Mode().IsRegular() && path != filepath.Join(workspace, forkspace.GenerationMarkerName) {
					stat, ok := info.Sys().(*syscall.Stat_t)
					if !ok || stat.Nlink > 1 {
						return errors.New("unproven shared primary payload requires its ownership receipt")
					}
				}
				return nil
			})
		}
		return false, err
	}
	if p == nil {
		return true, errors.New("immutable primary cannot become writable")
	}
	key, roots, err := p.key(repo)
	if err != nil {
		return true, err
	}
	if receipt.Key != key {
		return true, errors.New("immutable primary receipt changed")
	}
	if err := verifySnapshotHistory(ctx, workspace, roots); err != nil {
		return true, err
	}
	return true, verifySessionSubmodules(ctx, workspace, p.source.Binding.SelectedCommit)
}
