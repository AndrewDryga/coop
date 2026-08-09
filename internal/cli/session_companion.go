package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

const sessionCompanionBoxRoot = "/coop/repositories"

type sessionCompanionDiscardPlan struct {
	Name              string                   `json:"name"`
	Repo              string                   `json:"repo"`
	Workspace         string                   `json:"workspace"`
	WorkspaceIdentity sessionWorkspaceIdentity `json:"workspace_identity"`
	Head              string                   `json:"head"`
	StatusDigest      string                   `json:"status_digest"`
}

func sessionCompanionBoxPath(name string) string {
	return filepath.Join(sessionCompanionBoxRoot, name)
}

func sessionCompanionWorkspace(stateRoot, sessionID, name string) (string, error) {
	if !filepath.IsAbs(stateRoot) || !validSessionPathComponent(sessionID) ||
		!validCompanionRepositoryName(name) {
		return "", errors.New("invalid companion workspace binding")
	}
	root, err := filepath.EvalSymlinks(stateRoot)
	if err != nil || !filepath.IsAbs(root) {
		return "", errors.New("session state root is unavailable")
	}
	return filepath.Join(root, "repositories", sessionID, name), nil
}

func ensureSessionCompanion(
	stateRoot, sessionID string,
	binding session.CompanionRepository,
) (session.CompanionRepository, error) {
	if !filepath.IsAbs(binding.Repository) ||
		!validSessionWorkspaceCommit(binding.BaseCommit) ||
		!validCompanionRepositoryName(binding.Name) {
		return session.CompanionRepository{}, errors.New("invalid companion repository binding")
	}
	expected, err := sessionCompanionWorkspace(stateRoot, sessionID, binding.Name)
	if err != nil {
		return session.CompanionRepository{}, err
	}
	if binding.Workspace != expected {
		return session.CompanionRepository{}, errors.New("companion workspace is not deterministic")
	}
	if err := ensurePrivateDirectory(filepath.Dir(expected)); err != nil {
		return session.CompanionRepository{}, fmt.Errorf("prepare companion workspace: %w", err)
	}
	lockName := deterministicForkName("companion\x00" + sessionID + "\x00" + binding.Name)
	unlock, err := forkspace.LockState(binding.Repository, lockName)
	if err != nil {
		return session.CompanionRepository{}, fmt.Errorf("lock companion workspace: %w", err)
	}
	defer unlock()

	_, statErr := os.Lstat(expected)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return session.CompanionRepository{}, fmt.Errorf("inspect companion workspace: %w", statErr)
	}
	if created {
		cmd := exec.Command(
			"git", "-C", binding.Repository, "-c", "core.fsmonitor=false",
			"worktree", "add", "--detach", "--", expected, binding.BaseCommit,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = os.RemoveAll(expected)
			return session.CompanionRepository{}, fmt.Errorf(
				"create companion workspace: %w: %s",
				err, strings.TrimSpace(string(out)),
			)
		}
	}
	if err := verifySessionCompanion(binding); err != nil {
		if created {
			_ = removeSessionCompanion(binding)
		}
		return session.CompanionRepository{}, err
	}
	return binding, nil
}

func verifySessionCompanion(binding session.CompanionRepository) error {
	info, err := os.Lstat(binding.Workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("companion workspace is not a real directory")
	}
	root, err := realGitRepository(binding.Workspace)
	if err != nil || root != binding.Workspace {
		return errors.New("companion workspace is not an exact Git worktree")
	}
	head, err := sessionWorkspaceCommit(binding.Workspace, "HEAD")
	if err != nil || head != binding.BaseCommit {
		return errors.New("companion workspace HEAD does not match its persisted commit")
	}
	branch, err := sessionWorkspaceGitText(
		binding.Workspace, 64, "rev-parse", "--abbrev-ref", "HEAD",
	)
	if err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		return errors.New("companion workspace is not detached")
	}
	status, truncated, err := runSessionWorkspaceGit(
		binding.Workspace, sessionWorkspaceGitOutputLimit,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "-z",
	)
	if err != nil || truncated || len(status) != 0 {
		return errors.New("companion workspace is not clean")
	}
	sourceCommon, err := sessionGitCommonDir(binding.Repository)
	if err != nil {
		return err
	}
	workspaceCommon, err := sessionGitCommonDir(binding.Workspace)
	if err != nil || workspaceCommon != sourceCommon {
		return errors.New("companion workspace belongs to another repository")
	}
	return nil
}

func sessionGitCommonDir(workspace string) (string, error) {
	out, err := sessionWorkspaceGitText(
		workspace, sessionWorkspaceGitOutputLimit,
		"rev-parse", "--path-format=absolute", "--git-common-dir",
	)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

func planSessionCompanionDiscard(
	binding session.CompanionRepository,
) (sessionCompanionDiscardPlan, error) {
	// A companion snapshot that is already gone plans as absent, mirroring the
	// primary workspace: discardSessionCompanion treats a missing workspace as
	// removed, and a snapshot holds no unpublished work by construction.
	if _, err := os.Lstat(binding.Workspace); errors.Is(err, os.ErrNotExist) {
		return sessionCompanionDiscardPlan{
			Name: binding.Name, Repo: binding.Repository, Workspace: binding.Workspace,
			Head: binding.BaseCommit, StatusDigest: sessionWorkspaceStatusDigest(nil),
		}, nil
	}
	if err := verifySessionCompanion(binding); err != nil {
		return sessionCompanionDiscardPlan{}, err
	}
	info, err := os.Lstat(binding.Workspace)
	if err != nil {
		return sessionCompanionDiscardPlan{}, err
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil {
		return sessionCompanionDiscardPlan{}, err
	}
	return sessionCompanionDiscardPlan{
		Name: binding.Name, Repo: binding.Repository, Workspace: binding.Workspace,
		WorkspaceIdentity: identity, Head: binding.BaseCommit,
		StatusDigest: sessionWorkspaceStatusDigest(nil),
	}, nil
}

func discardSessionCompanion(plan sessionCompanionDiscardPlan) error {
	info, err := os.Lstat(plan.Workspace)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("companion discard plan is stale: workspace is not a real directory")
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil || identity != plan.WorkspaceIdentity {
		return errors.New("companion discard plan is stale: workspace was replaced")
	}
	binding := session.CompanionRepository{
		Name: plan.Name, Repository: plan.Repo,
		Workspace: plan.Workspace, BaseCommit: plan.Head,
	}
	if err := verifySessionCompanion(binding); err != nil {
		return fmt.Errorf("companion discard plan is stale: %w", err)
	}
	return removeSessionCompanion(binding)
}

func removeSessionCompanion(binding session.CompanionRepository) error {
	cmd := exec.Command(
		"git", "-C", binding.Repository, "-c", "core.fsmonitor=false",
		"worktree", "remove", "--force", "--", binding.Workspace,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf(
			"remove companion workspace: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	_ = os.Remove(filepath.Dir(binding.Workspace))
	return nil
}
