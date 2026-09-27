package sessionsvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/session"
)

const (
	sessionCompanionBoxRoot        = "/coop/repositories"
	sessionCompanionMarker         = "coop-session-companion-v1\n"
	sessionCompanionMarkerFile     = "coop-session-companion"
	sessionCompanionHistoryFile    = "coop-session-companion-history"
	sessionCompanionHistoryFull    = "full\n"
	sessionCompanionHistoryBounded = "bounded\n"
	sessionCompanionHistoryShallow = "shallow\n"
)

type sessionCompanionGitConfig struct {
	Key   string
	Value string
}

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
	return ensureSessionCompanionContext(context.Background(), stateRoot, sessionID, binding)
}

func ensureSessionCompanionContext(
	ctx context.Context, stateRoot, sessionID string,
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
	unlock, err := forkspace.LockStateContext(ctx, binding.Repository, lockName)
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
		if err := createSessionCompanionContext(ctx, binding); err != nil {
			return session.CompanionRepository{}, err
		}
		return binding, nil
	}
	if err := verifySessionCompanionContext(ctx, binding); err != nil {
		return session.CompanionRepository{}, err
	}
	if err := verifySessionSubmodules(ctx, binding.Workspace, binding.BaseCommit); err != nil {
		return session.CompanionRepository{}, err
	}
	if err := materializeSessionSubmodules(ctx, binding.Repository, binding.Workspace, binding.BaseCommit); err != nil {
		return session.CompanionRepository{}, err
	}
	return binding, nil
}

func createSessionCompanionContext(ctx context.Context, binding session.CompanionRepository) (returnErr error) {
	stage, err := os.MkdirTemp(
		filepath.Dir(binding.Workspace), "."+binding.Name+"-",
	)
	if err != nil {
		return fmt.Errorf("create companion staging directory: %w", err)
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil {
		return errors.Join(
			fmt.Errorf("inspect companion staging directory: %w", err), os.Remove(stage),
		)
	}
	stageIdentity, err := sessionWorkspaceIdentityFor(stageInfo)
	if err != nil {
		return errors.Join(
			fmt.Errorf("identify companion staging directory: %w", err), os.Remove(stage),
		)
	}
	defer func() {
		if stage != "" {
			returnErr = errors.Join(
				returnErr, removeSessionCompanionStage(stage, stageIdentity),
			)
		}
	}()

	// The worker has already fetched a self-contained object closure. Use the
	// same credential-free pinned clone as primary workspaces, without a second
	// pack builder or a history budget that rejects large working trees.
	if err := forkspace.GitClonePinnedContext(ctx, binding.Repository, stage, binding.BaseCommit); err != nil {
		return fmt.Errorf("clone companion source: %w", err)
	}
	if err := forkspace.GitRefCommand(ctx, stage, "remote", "remove", "origin").Run(); err != nil {
		return fmt.Errorf("remove companion transport: %w", err)
	}
	if err := forkspace.GitRefCommand(ctx, stage, "update-ref", "--no-deref", "HEAD", binding.BaseCommit).Run(); err != nil {
		return fmt.Errorf("pin companion commit: %w", err)
	}
	refs, err := forkspace.GitRefCommand(ctx, stage, "for-each-ref", "--format=delete %(refname)").Output()
	if err != nil {
		return fmt.Errorf("enumerate companion clone refs: %w", err)
	}
	prune := forkspace.GitRefCommand(ctx, stage, "update-ref", "--stdin")
	prune.Stdin = bytes.NewReader(refs)
	if err := prune.Run(); err != nil {
		return fmt.Errorf("remove companion clone refs: %w", err)
	}
	if _, _, err := runSessionWorkspaceGitWithEnvContext(ctx, stage, sessionWorkspaceGitOutputLimit,
		sessionCompanionCheckoutGitEnv(), "reset", "--hard", "--quiet", binding.BaseCommit); err != nil {
		return fmt.Errorf("checkout companion commit: %w", err)
	}
	if err := materializeSessionSubmodules(ctx, binding.Repository, stage, binding.BaseCommit); err != nil {
		return fmt.Errorf("materialize companion submodules: %w", err)
	}

	metadataPath := filepath.Join(stage, ".git")
	if err := os.RemoveAll(filepath.Join(metadataPath, "logs")); err != nil {
		return fmt.Errorf("remove companion reflogs: %w", err)
	}
	marker := filepath.Join(metadataPath, sessionCompanionMarkerFile)
	if err := os.WriteFile(
		marker, []byte(sessionCompanionMarker+binding.BaseCommit+"\n"), 0o600,
	); err != nil {
		return fmt.Errorf("mark companion workspace: %w", err)
	}
	if err := os.WriteFile(
		filepath.Join(metadataPath, sessionCompanionHistoryFile),
		[]byte(sessionCompanionHistoryFull), 0o600,
	); err != nil {
		return fmt.Errorf("record companion history mode: %w", err)
	}
	stagedBinding := binding
	stagedBinding.Workspace = stage
	if err := verifySessionCompanionContext(ctx, stagedBinding); err != nil {
		return fmt.Errorf("verify staged companion workspace: %w", err)
	}
	if _, err := os.Lstat(binding.Workspace); !errors.Is(err, os.ErrNotExist) {
		return errors.New("publish companion workspace: destination appeared during creation")
	}
	if err := os.Rename(stage, binding.Workspace); err != nil {
		return fmt.Errorf("publish companion workspace: %w", err)
	}
	stage = ""
	return nil
}

func sessionCompanionGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, sessionCompanionGitEnvExtras()...)
}

// sessionCompanionGitEnvExtras is what companion transfers add on top of any base environment —
// including the trusted git view's, which already carries GIT_DIR and friends.
func sessionCompanionGitEnvExtras() []string {
	return []string{
		"GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	}
}

func runSessionCompanionGitContext(
	ctx context.Context, dir string, limit int, args ...string,
) ([]byte, bool, error) {
	return runSessionWorkspaceGitWithEnvContext(
		ctx, dir, limit, sessionCompanionGitEnv(), args...,
	)
}

func sessionCompanionGitEnvWithConfig(config []sessionCompanionGitConfig) []string {
	env := append(sessionCompanionGitEnv(), "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for index, entry := range config {
		suffix := strconv.Itoa(index)
		env = append(
			env,
			"GIT_CONFIG_KEY_"+suffix+"="+entry.Key,
			"GIT_CONFIG_VALUE_"+suffix+"="+entry.Value,
		)
	}
	return env
}

func sessionCompanionCheckoutGitEnv() []string {
	return append(
		sessionCompanionGitEnvWithConfig([]sessionCompanionGitConfig{{
			Key: "core.attributesFile", Value: os.DevNull,
		}}),
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

func sessionCompanionGitTextContext(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	out, truncated, err := runSessionCompanionGitContext(ctx, dir, limit, args...)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", strings.Join(args, " "), limit)
	}
	return out, nil
}

func removeSessionCompanionStage(
	stage string, expected sessionWorkspaceIdentity,
) error {
	info, err := os.Lstat(stage)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("companion staging directory was replaced")
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil || !identity.sameDirectory(expected) {
		return errors.New("companion staging directory identity changed")
	}
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("remove companion staging directory: %w", err)
	}
	return nil
}

func verifySessionCompanionContext(ctx context.Context, binding session.CompanionRepository) error {
	info, err := os.Lstat(binding.Workspace)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("companion workspace is not a real directory")
	}
	metadataPath := filepath.Join(binding.Workspace, ".git")
	metadata, err := os.Lstat(metadataPath)
	if err != nil || metadata.Mode()&os.ModeSymlink != 0 ||
		(!metadata.IsDir() && !metadata.Mode().IsRegular()) {
		return errors.New("companion workspace has invalid Git metadata")
	}
	root, err := realSessionCompanionRepositoryContext(ctx, binding.Workspace)
	if err != nil {
		return fmt.Errorf("companion workspace is not an exact Git checkout: %w", err)
	}
	if root != binding.Workspace {
		return fmt.Errorf("companion workspace is not an exact Git checkout: top level %s is not %s", root, binding.Workspace)
	}
	workspaceCommon, err := sessionCompanionGitCommonDirContext(ctx, binding.Workspace)
	if err != nil {
		return err
	}
	if metadata.IsDir() {
		metadataPath, err = filepath.EvalSymlinks(metadataPath)
		if err != nil || workspaceCommon != metadataPath {
			return errors.New("companion workspace Git metadata is not self-contained")
		}
		marker, err := os.ReadFile(filepath.Join(metadataPath, sessionCompanionMarkerFile))
		if err != nil || string(marker) != sessionCompanionMarker+binding.BaseCommit+"\n" {
			return errors.New("companion workspace ownership marker does not match")
		}
	} else {
		// Sessions created before self-contained companions used linked worktrees. Verify source
		// ownership before any command (such as status) that can consult its local driver config.
		sourceCommon, err := sessionCompanionGitCommonDirContext(ctx, binding.Repository)
		if err != nil || workspaceCommon != sourceCommon {
			return errors.New("companion workspace belongs to another repository")
		}
	}
	head, err := sessionCompanionCommitContext(ctx, binding.Workspace, "HEAD")
	if err != nil || head != binding.BaseCommit {
		return errors.New("companion workspace HEAD does not match its persisted commit")
	}
	branch, err := sessionCompanionGitTextContext(ctx,
		binding.Workspace, 64, "rev-parse", "--abbrev-ref", "HEAD",
	)
	if err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		return errors.New("companion workspace is not detached")
	}
	status, truncated, err := sessionCompanionStatusContext(ctx, binding)
	if err != nil || truncated || len(status) != 0 {
		return errors.New("companion workspace is not clean")
	}
	if metadata.IsDir() {
		formatBytes, err := sessionCompanionGitTextContext(ctx,
			binding.Workspace, 16, "rev-parse", "--show-object-format",
		)
		objectFormat := strings.TrimSpace(string(formatBytes))
		if err != nil || (objectFormat != "sha1" || len(binding.BaseCommit) != 40) &&
			(objectFormat != "sha256" || len(binding.BaseCommit) != 64) {
			return errors.New("companion workspace object format does not match")
		}
		remotes, err := sessionCompanionGitTextContext(ctx,
			binding.Workspace, sessionWorkspaceGitOutputLimit, "remote",
		)
		if err != nil || len(strings.TrimSpace(string(remotes))) != 0 {
			return errors.New("companion workspace exposes a source remote")
		}
		refs, err := sessionCompanionGitTextContext(ctx,
			binding.Workspace, sessionWorkspaceGitOutputLimit,
			"for-each-ref", "--format=%(refname)",
		)
		if err != nil || len(strings.TrimSpace(string(refs))) != 0 {
			return errors.New("companion workspace exposes non-pinned refs")
		}
		alternatesPath := filepath.Join(metadataPath, "objects", "info", "alternates")
		if _, err := os.Lstat(alternatesPath); !errors.Is(err, os.ErrNotExist) {
			return errors.New("companion workspace depends on an object alternate")
		}
		if _, err := os.Lstat(filepath.Join(metadataPath, "logs")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("companion workspace exposes reflogs")
		}
		shallow, err := sessionCompanionGitTextContext(ctx,
			binding.Workspace, 16, "rev-parse", "--is-shallow-repository",
		)
		if err != nil {
			return errors.New("companion workspace history mode is unavailable")
		}
		shallowState := strings.TrimSpace(string(shallow))
		if shallowState != "true" && shallowState != "false" {
			return errors.New("companion workspace history mode is invalid")
		}
		isShallow := shallowState == "true"
		historyMode, historyErr := os.ReadFile(
			filepath.Join(metadataPath, sessionCompanionHistoryFile),
		)
		if errors.Is(historyErr, os.ErrNotExist) {
			historyMode = []byte(sessionCompanionHistoryFull)
		} else if historyErr != nil {
			return errors.New("companion workspace history marker is unavailable")
		}
		if _, _, err := runSessionCompanionGitContext(ctx,
			binding.Workspace, sessionWorkspaceGitOutputLimit,
			"fsck", "--connectivity-only", "--no-dangling", "--no-reflogs",
		); err != nil {
			return errors.New("companion workspace history is incomplete")
		}
		switch string(historyMode) {
		case sessionCompanionHistoryShallow:
			commits, err := sessionCompanionGitTextContext(ctx,
				binding.Workspace, 16, "rev-list", "--count", "HEAD",
			)
			if err != nil || !isShallow || strings.TrimSpace(string(commits)) != "1" {
				return errors.New("companion workspace shallow history does not match")
			}
		case sessionCompanionHistoryBounded:
			// The exact commit count depends on when the companion was built,
			// so what is checked is the shape the marker promises: history
			// that is present and truncated.
			commits, err := sessionCompanionGitTextContext(ctx,
				binding.Workspace, 16, "rev-list", "--count", "HEAD",
			)
			if err != nil || !isShallow {
				return errors.New("companion workspace bounded history does not match")
			}
			if count, convErr := strconv.Atoi(strings.TrimSpace(string(commits))); convErr != nil || count < 1 {
				return errors.New("companion workspace bounded history is empty")
			}
		case sessionCompanionHistoryFull:
			if isShallow {
				return errors.New("companion workspace full history is marked shallow")
			}
		default:
			return errors.New("companion workspace history marker is invalid")
		}
		return nil
	}
	return nil
}

func realSessionCompanionRepositoryContext(ctx context.Context, path string) (string, error) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("repository is not a real path: %w", err)
	}
	if filepath.Clean(realPath) != path {
		return "", errors.New("repository must name its real directory, not a symlink or alias")
	}
	info, err := os.Stat(realPath)
	if err != nil || !info.IsDir() {
		return "", errors.New("repository is not a directory")
	}
	out, err := sessionCompanionGitTextContext(ctx,
		realPath, sessionWorkspaceGitOutputLimit, "rev-parse", "--show-toplevel",
	)
	if err != nil {
		return "", fmt.Errorf("repository is not a Git worktree: %w", err)
	}
	root := strings.TrimSpace(string(out))
	root, err = filepath.EvalSymlinks(root)
	if err != nil || filepath.Clean(root) != realPath {
		return "", errors.New("repository path is not the exact Git worktree root")
	}
	return realPath, nil
}

func sessionCompanionStatus(
	binding session.CompanionRepository,
) (status []byte, truncated bool, returnErr error) {
	return sessionCompanionStatusContext(context.Background(), binding)
}

func sessionCompanionStatusContext(
	ctx context.Context, binding session.CompanionRepository,
) ([]byte, bool, error) {
	status, truncated, err := sessionPinnedStatusContext(ctx, binding)
	if err != nil {
		return nil, false, err
	}
	clean, err := sessionCompanionGitlinksCleanContext(ctx, binding.Workspace, binding.BaseCommit)
	if err != nil {
		return nil, false, err
	}
	if !clean {
		return nil, false, errors.New("companion gitlink worktree is modified")
	}
	return status, truncated, nil
}

// Compare content with a fresh pinned index: an agent's assume-unchanged and
// skip-worktree bits must not conceal valuable work from review or discard.
func sessionPinnedStatusContext(
	ctx context.Context, binding session.CompanionRepository,
) (status []byte, truncated bool, returnErr error) {
	returnErr = withSessionPrivateIndex(ctx, binding, func(env []string) error {
		command := exec.CommandContext(ctx, "git", gitArgs(binding.Workspace, []string{
			"status", "--porcelain=v2", "--untracked-files=all", "--no-renames",
			"--ignore-submodules=all", "-z",
		})...)
		command.Env = env
		output := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceGitOutputLimit}
		err := forkspace.RunLFSStatus(ctx, binding.Workspace, command, output, func(limit int, args ...string) ([]byte, error) {
			output, truncated, err := runSessionPrivateGitContext(ctx, binding.Workspace, limit, env, args...)
			if truncated {
				return nil, errors.New("LFS status read exceeds its bound")
			}
			return output, err
		})
		status, truncated = output.buf.Bytes(), output.truncated
		return err
	})
	return status, truncated, returnErr
}

func withSessionPrivateIndex(
	ctx context.Context, binding session.CompanionRepository, action func([]string) error,
) (returnErr error) {
	statusRoot, err := os.MkdirTemp(
		filepath.Dir(binding.Workspace), ".coop-companion-status-",
	)
	if err != nil {
		return fmt.Errorf("create companion status repository: %w", err)
	}
	statusInfo, err := os.Lstat(statusRoot)
	if err != nil {
		return errors.Join(
			fmt.Errorf("inspect companion status repository: %w", err), os.Remove(statusRoot),
		)
	}
	statusIdentity, err := sessionWorkspaceIdentityFor(statusInfo)
	if err != nil {
		return errors.Join(
			fmt.Errorf("identify companion status repository: %w", err), os.Remove(statusRoot),
		)
	}
	defer func() {
		returnErr = errors.Join(
			returnErr, removeSessionCompanionStage(statusRoot, statusIdentity),
		)
	}()
	formatBytes, err := sessionCompanionGitTextContext(ctx,
		binding.Workspace, 16, "rev-parse", "--show-object-format",
	)
	if err != nil {
		return fmt.Errorf("resolve companion status object format: %w", err)
	}
	objectFormat := strings.TrimSpace(string(formatBytes))
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return errors.New("companion status object format is invalid")
	}
	objectsBytes, _, err := runSessionWorkspaceGitRealContext(ctx,
		binding.Workspace, sessionWorkspaceGitOutputLimit, sessionCompanionGitEnv(),
		"rev-parse", "--path-format=absolute", "--git-path", "objects",
	)
	if err != nil {
		return fmt.Errorf("resolve companion status objects: %w", err)
	}
	objectsPath, err := filepath.EvalSymlinks(strings.TrimSpace(string(objectsBytes)))
	if err != nil || !filepath.IsAbs(objectsPath) || strings.ContainsAny(objectsPath, "\x00\r\n") {
		return errors.New("companion status objects are unavailable")
	}
	emptyTemplate := filepath.Join(statusRoot, "template")
	if err := os.Mkdir(emptyTemplate, 0o700); err != nil {
		return fmt.Errorf("create companion status template: %w", err)
	}
	gitDir := filepath.Join(statusRoot, "git")
	initCmd := exec.CommandContext(ctx,
		"git", "init", "--bare", "--quiet", "--object-format="+objectFormat,
		"--template="+emptyTemplate, gitDir,
	)
	initCmd.Env = sessionCompanionGitEnv()
	if out, err := initCmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		return fmt.Errorf(
			"initialize companion status repository: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	if err := os.WriteFile(
		filepath.Join(gitDir, "HEAD"), []byte(binding.BaseCommit+"\n"), 0o600,
	); err != nil {
		return fmt.Errorf("pin companion status HEAD: %w", err)
	}
	alternatesPath := filepath.Join(gitDir, "objects", "info", "alternates")
	if err := os.WriteFile(alternatesPath, []byte(objectsPath+"\n"), 0o600); err != nil {
		return fmt.Errorf("link companion status objects: %w", err)
	}
	env := append(
		sessionCompanionCheckoutGitEnv(),
		"GIT_DIR="+gitDir,
		"GIT_INDEX_FILE="+filepath.Join(gitDir, "index"),
		"GIT_WORK_TREE="+binding.Workspace,
	)
	if _, _, err := runSessionPrivateGitContext(
		ctx, binding.Workspace, sessionWorkspaceGitOutputLimit, env,
		"read-tree", binding.BaseCommit,
	); err != nil {
		return fmt.Errorf("prepare companion status index: %w", err)
	}
	return action(env)
}

func sessionCompanionGitlinksCleanContext(ctx context.Context, workspace, commit string) (bool, error) {
	links, err := forkspace.Gitlinks(ctx, workspace, commit)
	if err != nil {
		return false, err
	}
	for path, head := range links {
		child, err := forkspace.SubmoduleDirectory(workspace, path)
		if err != nil {
			return false, nil
		}
		entries, err := os.ReadDir(child)
		if err != nil {
			return false, err
		}
		if len(entries) == 0 {
			continue // Only historical cleanup accepts empty placeholders.
		}
		if err := verifyPinnedSubmodule(ctx, child, head); err != nil {
			return false, nil
		}
		clean, err := sessionCompanionGitlinksCleanContext(ctx, child, head)
		if err != nil || !clean {
			return false, err
		}
	}
	return true, nil
}

func sessionCompanionGitCommonDirContext(ctx context.Context, workspace string) (string, error) {
	out, _, err := runSessionWorkspaceGitRealContext(ctx,
		workspace, sessionWorkspaceGitOutputLimit, sessionCompanionGitEnv(),
		"rev-parse", "--path-format=absolute", "--git-common-dir",
	)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

func sessionCompanionCommitContext(ctx context.Context, dir, revision string) (string, error) {
	if revision == "" || strings.ContainsAny(revision, "\x00\r\n") {
		return "", errors.New("invalid companion commit revision")
	}
	out, err := sessionCompanionGitTextContext(ctx,
		dir, sessionWorkspaceGitOutputLimit,
		"rev-parse", "--verify", "--end-of-options", revision+"^{commit}",
	)
	if err != nil {
		return "", fmt.Errorf("resolve companion commit %q: %w", revision, err)
	}
	commit := strings.TrimSpace(string(out))
	if !validSessionWorkspaceCommit(commit) {
		return "", fmt.Errorf("resolve companion commit %q: malformed identity %q", revision, commit)
	}
	return commit, nil
}

func planSessionCompanionDiscard(
	binding session.CompanionRepository,
) (sessionCompanionDiscardPlan, error) {
	return planSessionCompanionDiscardContext(context.Background(), binding)
}

func planSessionCompanionDiscardContext(
	ctx context.Context, binding session.CompanionRepository,
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
	if err := verifySessionCompanionContext(ctx, binding); err != nil {
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
	return discardSessionCompanionContext(context.Background(), plan)
}

func discardSessionCompanionContext(ctx context.Context, plan sessionCompanionDiscardPlan) error {
	info, err := os.Lstat(plan.Workspace)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("companion discard plan is stale: workspace is not a real directory")
	}
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil || !identity.sameDirectory(plan.WorkspaceIdentity) {
		return errors.New("companion discard plan is stale: workspace was replaced")
	}
	binding := session.CompanionRepository{
		Name: plan.Name, Repository: plan.Repo,
		Workspace: plan.Workspace, BaseCommit: plan.Head,
	}
	if err := verifySessionCompanionContext(ctx, binding); err != nil {
		return fmt.Errorf("companion discard plan is stale: %w", err)
	}
	return removeSessionCompanion(binding)
}

func removeSessionCompanion(binding session.CompanionRepository) error {
	metadata, err := os.Lstat(filepath.Join(binding.Workspace, ".git"))
	if err != nil {
		return fmt.Errorf("inspect companion Git metadata: %w", err)
	}
	if metadata.IsDir() && metadata.Mode()&os.ModeSymlink == 0 {
		if err := os.RemoveAll(binding.Workspace); err != nil {
			return fmt.Errorf("remove companion workspace: %w", err)
		}
		_ = os.Remove(filepath.Dir(binding.Workspace))
		return nil
	}

	// Legacy linked companions must be unregistered from the source repository.
	cmd := forkspace.GitRefCommand(context.Background(), binding.Repository, "worktree", "remove", "--force", "--", binding.Workspace)
	cmd.Env = sessionCompanionGitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf(
			"remove companion workspace: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	_ = os.Remove(filepath.Dir(binding.Workspace))
	return nil
}
