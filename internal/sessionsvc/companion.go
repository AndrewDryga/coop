package sessionsvc

import (
	"bufio"
	"bytes"
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
	sessionCompanionBoxRoot                          = "/coop/repositories"
	sessionCompanionMarker                           = "coop-session-companion-v1\n"
	sessionCompanionMarkerFile                       = "coop-session-companion"
	sessionCompanionHistoryFile                      = "coop-session-companion-history"
	sessionCompanionHistoryFull                      = "full\n"
	sessionCompanionHistoryShallow                   = "shallow\n"
	sessionCompanionFullHistoryMaxLogicalSize uint64 = 1 << 30
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
		if err := createSessionCompanion(binding); err != nil {
			return session.CompanionRepository{}, err
		}
		return binding, nil
	}
	if err := verifySessionCompanion(binding); err != nil {
		return session.CompanionRepository{}, err
	}
	return binding, nil
}

func createSessionCompanion(binding session.CompanionRepository) error {
	return createSessionCompanionWithHistoryLimit(
		binding, sessionCompanionFullHistoryMaxLogicalSize,
	)
}

func createSessionCompanionWithHistoryLimit(
	binding session.CompanionRepository, fullHistoryMaxLogicalSize uint64,
) (returnErr error) {
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

	formatBytes, err := sessionCompanionGitText(
		binding.Repository, 16, "rev-parse", "--show-object-format",
	)
	if err != nil {
		return fmt.Errorf("resolve companion object format: %w", err)
	}
	objectFormat := strings.TrimSpace(string(formatBytes))
	if (objectFormat != "sha1" || len(binding.BaseCommit) != 40) &&
		(objectFormat != "sha256" || len(binding.BaseCommit) != 64) {
		return errors.New("companion commit does not match its repository object format")
	}
	fullHistory, err := sessionCompanionHistoryWithinLimit(
		binding, fullHistoryMaxLogicalSize,
	)
	if err != nil {
		return err
	}
	shallow := !fullHistory
	emptyTemplate := filepath.Join(stage, ".coop-empty-git-template")
	if err := os.Mkdir(emptyTemplate, 0o700); err != nil {
		return fmt.Errorf("create empty companion Git template: %w", err)
	}
	initCmd := exec.Command(
		"git", gitArgs(stage, []string{
			"init", "--quiet", "--object-format=" + objectFormat,
			"--template=" + emptyTemplate,
		})...,
	)
	for _, entry := range sessionCompanionGitEnv() {
		if !strings.HasPrefix(entry, "GIT_TEMPLATE_DIR=") {
			initCmd.Env = append(initCmd.Env, entry)
		}
	}
	if out, err := initCmd.CombinedOutput(); err != nil {
		return fmt.Errorf(
			"initialize companion workspace: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	if err := os.Remove(emptyTemplate); err != nil {
		return fmt.Errorf("remove empty companion Git template: %w", err)
	}
	if err := materializeSessionCompanionObjects(binding, stage, shallow); err != nil {
		return err
	}
	historyMode := sessionCompanionHistoryFull
	if shallow {
		historyMode = sessionCompanionHistoryShallow
		if err := os.WriteFile(
			filepath.Join(stage, ".git", "shallow"),
			[]byte(binding.BaseCommit+"\n"), 0o600,
		); err != nil {
			return fmt.Errorf("write companion shallow boundary: %w", err)
		}
	}
	if _, _, err := runSessionWorkspaceGitWithEnv(
		stage, sessionWorkspaceGitOutputLimit, sessionCompanionCheckoutGitEnv(),
		"checkout", "--quiet", "--detach", binding.BaseCommit,
	); err != nil {
		return fmt.Errorf("checkout companion commit: %w", err)
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
		[]byte(historyMode), 0o600,
	); err != nil {
		return fmt.Errorf("record companion history mode: %w", err)
	}
	stagedBinding := binding
	stagedBinding.Workspace = stage
	if err := verifySessionCompanion(stagedBinding); err != nil {
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

func materializeSessionCompanionObjects(
	binding session.CompanionRepository, stage string, shallow bool,
) (returnErr error) {
	packPrefix := filepath.Join(stage, ".git", "objects", "pack", "pack")
	if !shallow {
		packCmd := exec.Command(
			"git", gitArgs(
				binding.Repository,
				[]string{"pack-objects", "--quiet", "--revs", packPrefix},
			)...,
		)
		packCmd.Env = sessionCompanionGitEnv()
		packCmd.Stdin = strings.NewReader(binding.BaseCommit + "\n")
		if out, err := packCmd.CombinedOutput(); err != nil {
			return fmt.Errorf(
				"materialize companion history: %w: %s",
				err, strings.TrimSpace(string(out)),
			)
		}
		return nil
	}

	objectList, err := os.CreateTemp(stage, ".coop-companion-objects-")
	if err != nil {
		return fmt.Errorf("create companion object list: %w", err)
	}
	objectListPath := objectList.Name()
	defer func() {
		if objectList != nil {
			returnErr = errors.Join(returnErr, objectList.Close())
		}
		if objectListPath != "" {
			if err := os.Remove(objectListPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove companion object list: %w", err))
			}
		}
	}()
	if _, err := objectList.WriteString(binding.BaseCommit + "\n"); err != nil {
		return fmt.Errorf("write companion commit object: %w", err)
	}
	stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	listCmd := exec.Command(
		"git", gitArgs(
			binding.Repository,
			[]string{
				"rev-list", "--objects", "--no-object-names",
				binding.BaseCommit + "^{tree}",
			},
		)...,
	)
	listCmd.Env = sessionCompanionGitEnv()
	listCmd.Stdout = objectList
	listCmd.Stderr = stderr
	if err := listCmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.buf.String())
		if detail != "" {
			return fmt.Errorf("enumerate companion tree objects: %w: %s", err, detail)
		}
		return fmt.Errorf("enumerate companion tree objects: %w", err)
	}
	if _, err := objectList.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind companion object list: %w", err)
	}
	packCmd := exec.Command(
		"git", gitArgs(
			binding.Repository,
			[]string{
				"pack-objects", "--quiet", "--window=0", "--depth=0", packPrefix,
			},
		)...,
	)
	packCmd.Env = sessionCompanionGitEnv()
	packCmd.Stdin = objectList
	if out, err := packCmd.CombinedOutput(); err != nil {
		return fmt.Errorf(
			"materialize companion objects: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	if err := objectList.Close(); err != nil {
		return fmt.Errorf("close companion object list: %w", err)
	}
	objectList = nil
	if err := os.Remove(objectListPath); err != nil {
		return fmt.Errorf("remove companion object list: %w", err)
	}
	objectListPath = ""
	return nil
}

func sessionCompanionHistoryWithinLimit(
	binding session.CompanionRepository, limit uint64,
) (bool, error) {
	listStderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	listCmd := exec.Command(
		"git", gitArgs(
			binding.Repository,
			[]string{
				"rev-list", "--objects", "--no-object-names", binding.BaseCommit,
			},
		)...,
	)
	listCmd.Env = sessionCompanionGitEnv()
	objectIDs, err := listCmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("read companion history objects: %w", err)
	}
	listCmd.Stderr = listStderr
	sizeStderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	sizeCmd := exec.Command(
		"git", gitArgs(
			binding.Repository,
			[]string{"cat-file", "--buffer", "--batch-check=%(objectsize)"},
		)...,
	)
	sizeCmd.Env = sessionCompanionGitEnv()
	sizeCmd.Stdin = objectIDs
	sizeCmd.Stderr = sizeStderr
	sizes, err := sizeCmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("read companion history sizes: %w", err)
	}
	if err := sizeCmd.Start(); err != nil {
		return false, fmt.Errorf("measure companion history: %w", err)
	}
	if err := listCmd.Start(); err != nil {
		_ = sizeCmd.Process.Kill()
		_ = sizeCmd.Wait()
		return false, fmt.Errorf("enumerate companion history: %w", err)
	}
	complete := true
	total := uint64(0)
	scanner := bufio.NewScanner(sizes)
	for scanner.Scan() {
		size, err := strconv.ParseUint(strings.TrimSpace(scanner.Text()), 10, 64)
		if err != nil || size > limit-total {
			complete = false
			_ = listCmd.Process.Kill()
			_ = sizeCmd.Process.Kill()
			break
		}
		total += size
	}
	if err := scanner.Err(); err != nil && complete {
		_ = listCmd.Process.Kill()
		_ = sizeCmd.Process.Kill()
		_ = listCmd.Wait()
		_ = sizeCmd.Wait()
		return false, fmt.Errorf("scan companion history sizes: %w", err)
	}
	sizeWaitErr := sizeCmd.Wait()
	if sizeWaitErr != nil {
		_ = listCmd.Process.Kill()
	}
	listWaitErr := listCmd.Wait()
	if !complete {
		return false, nil
	}
	if sizeWaitErr != nil || listWaitErr != nil {
		return false, nil
	}
	return true, nil
}

func sessionCompanionGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+5)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return append(
		env,
		"GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	)
}

func runSessionCompanionGit(
	dir string, limit int, args ...string,
) ([]byte, bool, error) {
	return runSessionWorkspaceGitWithEnv(
		dir, limit, sessionCompanionGitEnv(), args...,
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
	)
}

func sessionCompanionGitText(dir string, limit int, args ...string) ([]byte, error) {
	out, truncated, err := runSessionCompanionGit(dir, limit, args...)
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
	if err != nil || identity != expected {
		return errors.New("companion staging directory identity changed")
	}
	if err := os.RemoveAll(stage); err != nil {
		return fmt.Errorf("remove companion staging directory: %w", err)
	}
	return nil
}

func verifySessionCompanion(binding session.CompanionRepository) error {
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
	root, err := realSessionCompanionRepository(binding.Workspace)
	if err != nil || root != binding.Workspace {
		return errors.New("companion workspace is not an exact Git checkout")
	}
	workspaceCommon, err := sessionCompanionGitCommonDir(binding.Workspace)
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
		sourceCommon, err := sessionCompanionGitCommonDir(binding.Repository)
		if err != nil || workspaceCommon != sourceCommon {
			return errors.New("companion workspace belongs to another repository")
		}
	}
	head, err := sessionCompanionCommit(binding.Workspace, "HEAD")
	if err != nil || head != binding.BaseCommit {
		return errors.New("companion workspace HEAD does not match its persisted commit")
	}
	branch, err := sessionCompanionGitText(
		binding.Workspace, 64, "rev-parse", "--abbrev-ref", "HEAD",
	)
	if err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		return errors.New("companion workspace is not detached")
	}
	status, truncated, err := sessionCompanionStatus(binding)
	if err != nil || truncated || len(status) != 0 {
		return errors.New("companion workspace is not clean")
	}
	if metadata.IsDir() {
		formatBytes, err := sessionCompanionGitText(
			binding.Workspace, 16, "rev-parse", "--show-object-format",
		)
		objectFormat := strings.TrimSpace(string(formatBytes))
		if err != nil || (objectFormat != "sha1" || len(binding.BaseCommit) != 40) &&
			(objectFormat != "sha256" || len(binding.BaseCommit) != 64) {
			return errors.New("companion workspace object format does not match")
		}
		remotes, err := sessionCompanionGitText(
			binding.Workspace, sessionWorkspaceGitOutputLimit, "remote",
		)
		if err != nil || len(strings.TrimSpace(string(remotes))) != 0 {
			return errors.New("companion workspace exposes a source remote")
		}
		refs, err := sessionCompanionGitText(
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
		shallow, err := sessionCompanionGitText(
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
		if _, _, err := runSessionCompanionGit(
			binding.Workspace, sessionWorkspaceGitOutputLimit,
			"fsck", "--connectivity-only", "--no-dangling", "--no-reflogs",
		); err != nil {
			return errors.New("companion workspace history is incomplete")
		}
		switch string(historyMode) {
		case sessionCompanionHistoryShallow:
			commits, err := sessionCompanionGitText(
				binding.Workspace, 16, "rev-list", "--count", "HEAD",
			)
			if err != nil || !isShallow || strings.TrimSpace(string(commits)) != "1" {
				return errors.New("companion workspace shallow history does not match")
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

func realSessionCompanionRepository(path string) (string, error) {
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
	out, err := sessionCompanionGitText(
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
	statusRoot, err := os.MkdirTemp(
		filepath.Dir(binding.Workspace), ".coop-companion-status-",
	)
	if err != nil {
		return nil, false, fmt.Errorf("create companion status repository: %w", err)
	}
	statusInfo, err := os.Lstat(statusRoot)
	if err != nil {
		return nil, false, errors.Join(
			fmt.Errorf("inspect companion status repository: %w", err), os.Remove(statusRoot),
		)
	}
	statusIdentity, err := sessionWorkspaceIdentityFor(statusInfo)
	if err != nil {
		return nil, false, errors.Join(
			fmt.Errorf("identify companion status repository: %w", err), os.Remove(statusRoot),
		)
	}
	defer func() {
		returnErr = errors.Join(
			returnErr, removeSessionCompanionStage(statusRoot, statusIdentity),
		)
	}()
	formatBytes, err := sessionCompanionGitText(
		binding.Workspace, 16, "rev-parse", "--show-object-format",
	)
	if err != nil {
		return nil, false, fmt.Errorf("resolve companion status object format: %w", err)
	}
	objectFormat := strings.TrimSpace(string(formatBytes))
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return nil, false, errors.New("companion status object format is invalid")
	}
	objectsBytes, err := sessionCompanionGitText(
		binding.Workspace, sessionWorkspaceGitOutputLimit,
		"rev-parse", "--path-format=absolute", "--git-path", "objects",
	)
	if err != nil {
		return nil, false, fmt.Errorf("resolve companion status objects: %w", err)
	}
	objectsPath, err := filepath.EvalSymlinks(strings.TrimSpace(string(objectsBytes)))
	if err != nil || !filepath.IsAbs(objectsPath) || strings.ContainsAny(objectsPath, "\x00\r\n") {
		return nil, false, errors.New("companion status objects are unavailable")
	}
	emptyTemplate := filepath.Join(statusRoot, "template")
	if err := os.Mkdir(emptyTemplate, 0o700); err != nil {
		return nil, false, fmt.Errorf("create companion status template: %w", err)
	}
	gitDir := filepath.Join(statusRoot, "git")
	initCmd := exec.Command(
		"git", "init", "--bare", "--quiet", "--object-format="+objectFormat,
		"--template="+emptyTemplate, gitDir,
	)
	initCmd.Env = sessionCompanionGitEnv()
	if out, err := initCmd.CombinedOutput(); err != nil {
		return nil, false, fmt.Errorf(
			"initialize companion status repository: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	if err := os.WriteFile(
		filepath.Join(gitDir, "HEAD"), []byte(binding.BaseCommit+"\n"), 0o600,
	); err != nil {
		return nil, false, fmt.Errorf("pin companion status HEAD: %w", err)
	}
	alternatesPath := filepath.Join(gitDir, "objects", "info", "alternates")
	if err := os.WriteFile(alternatesPath, []byte(objectsPath+"\n"), 0o600); err != nil {
		return nil, false, fmt.Errorf("link companion status objects: %w", err)
	}
	env := append(
		sessionCompanionCheckoutGitEnv(),
		"GIT_DIR="+gitDir,
		"GIT_INDEX_FILE="+filepath.Join(statusRoot, "index"),
		"GIT_WORK_TREE="+binding.Workspace,
	)
	if _, _, err := runSessionWorkspaceGitWithEnv(
		binding.Workspace, sessionWorkspaceGitOutputLimit, env,
		"read-tree", binding.BaseCommit,
	); err != nil {
		return nil, false, fmt.Errorf("prepare companion status index: %w", err)
	}
	status, truncated, err = runSessionWorkspaceGitWithEnv(
		binding.Workspace, sessionWorkspaceGitOutputLimit, env,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames",
		"--ignore-submodules=all", "-z",
	)
	if err != nil {
		return nil, false, fmt.Errorf("inspect isolated companion status: %w", err)
	}
	gitlinksClean, err := sessionCompanionGitlinksClean(binding.Workspace, env)
	if err != nil {
		return nil, false, err
	}
	if !gitlinksClean {
		return nil, false, errors.New("companion gitlink worktree is modified")
	}
	return status, truncated, nil
}

func sessionCompanionGitlinksClean(workspace string, env []string) (bool, error) {
	stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	cmd := exec.Command(
		"git", gitArgs(workspace, []string{"ls-files", "--stage", "-z"})...,
	)
	cmd.Env = env
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("read companion gitlinks: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("enumerate companion gitlinks: %w", err)
	}

	clean := true
	scanner := bufio.NewScanner(stdout)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if index := bytes.IndexByte(data, 0); index >= 0 {
			return index + 1, data[:index], nil
		}
		if atEOF && len(data) != 0 {
			return 0, nil, errors.New("unterminated companion index entry")
		}
		return 0, nil, nil
	})
	scanner.Buffer(make([]byte, 4096), sessionWorkspaceGitOutputLimit)
	for scanner.Scan() {
		headerAndPath := scanner.Text()
		tab := strings.IndexByte(headerAndPath, '\t')
		if tab < 0 {
			clean = false
			break
		}
		header := strings.Fields(headerAndPath[:tab])
		if len(header) != 3 || header[0] != "160000" {
			continue
		}
		relative := filepath.FromSlash(headerAndPath[tab+1:])
		if !filepath.IsLocal(relative) || relative == "." {
			clean = false
			break
		}
		path := filepath.Join(workspace, relative)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			clean = false
			break
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			clean = false
			break
		}
	}
	scanErr := scanner.Err()
	if !clean || scanErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if !clean {
		return false, nil
	}
	if scanErr != nil {
		return false, fmt.Errorf("scan companion gitlinks: %w", scanErr)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(stderr.buf.String())
		if detail != "" {
			return false, fmt.Errorf("enumerate companion gitlinks: %w: %s", waitErr, detail)
		}
		return false, fmt.Errorf("enumerate companion gitlinks: %w", waitErr)
	}
	return true, nil
}

func sessionCompanionGitCommonDir(workspace string) (string, error) {
	out, err := sessionCompanionGitText(
		workspace, sessionWorkspaceGitOutputLimit,
		"rev-parse", "--path-format=absolute", "--git-common-dir",
	)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory: %w", err)
	}
	return filepath.EvalSymlinks(strings.TrimSpace(string(out)))
}

func sessionCompanionCommit(dir, revision string) (string, error) {
	if revision == "" || strings.ContainsAny(revision, "\x00\r\n") {
		return "", errors.New("invalid companion commit revision")
	}
	out, err := sessionCompanionGitText(
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
	cmd := exec.Command(
		"git", gitArgs(
			binding.Repository,
			[]string{"worktree", "remove", "--force", "--", binding.Workspace},
		)...,
	)
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
