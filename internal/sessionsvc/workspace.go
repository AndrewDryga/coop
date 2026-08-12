package sessionsvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const (
	sessionWorkspaceGitOutputLimit = 1 << 20
	sessionWorkspacePatchLimit     = 1 << 20
	sessionWorkspaceErrorLimit     = 8 << 10
)

var errSessionWorkspaceDetachedHead = errors.New("workspace HEAD is detached")

// sessionWorkspace is the host-owned identity captured when a remote session fork is created.
// The fields are exported so the unexported boundary remains directly JSON-marshalable.
type sessionWorkspace struct {
	Repo       string `json:"repo"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"base_commit"`
	ForkHead   string `json:"fork_head"`
}

type sessionWorkspaceChange struct {
	Path         string `json:"path"`
	PathBytes    []byte `json:"path_bytes"`
	OldPath      string `json:"old_path,omitempty"`
	OldPathBytes []byte `json:"old_path_bytes,omitempty"`
	Status       string `json:"status"`
}

type sessionWorkspaceParentDivergence struct {
	Ahead        int  `json:"ahead"`
	Behind       int  `json:"behind"`
	BaseToFork   int  `json:"base_to_fork"`
	BaseToParent int  `json:"base_to_parent"`
	Diverged     bool `json:"diverged"`
}

type WorkspaceChanges struct {
	BaseCommit       string                           `json:"base_commit"`
	ForkHead         string                           `json:"fork_head"`
	ForkTree         string                           `json:"fork_tree"`
	PullRequestTree  string                           `json:"pull_request_tree,omitempty"`
	ParentHead       string                           `json:"parent_head"`
	Committed        []sessionWorkspaceChange         `json:"committed,omitempty"`
	Staged           []sessionWorkspaceChange         `json:"staged,omitempty"`
	Unstaged         []sessionWorkspaceChange         `json:"unstaged,omitempty"`
	Untracked        []sessionWorkspaceChange         `json:"untracked,omitempty"`
	Conflicts        []sessionWorkspaceChange         `json:"conflicts,omitempty"`
	ParentDivergence sessionWorkspaceParentDivergence `json:"parent_divergence"`
	Patch            string                           `json:"patch,omitempty"`
	Truncated        bool                             `json:"truncated"`
	PatchDigest      string                           `json:"patch_digest,omitempty"`
	PatchBytes       int64                            `json:"patch_bytes"`
	PatchOffset      int64                            `json:"patch_offset"`
	PatchNextOffset  int64                            `json:"patch_next_offset"`
	PatchHasMore     bool                             `json:"patch_has_more"`
}

type sessionWorkspaceIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type WorkspaceDiscardPlan struct {
	Repo              string                   `json:"repo"`
	Name              string                   `json:"name"`
	Workspace         string                   `json:"workspace"`
	WorkspaceIdentity sessionWorkspaceIdentity `json:"workspace_identity"`
	Branch            string                   `json:"branch"`
	Head              string                   `json:"head"`
	ParentHead        string                   `json:"parent_head"`
	StatusDigest      string                   `json:"status_digest"`
	Dirty             bool                     `json:"dirty"`
	Unmerged          bool                     `json:"unmerged"`
	Running           bool                     `json:"running"`
	AcceptedDirty     bool                     `json:"accepted_dirty,omitempty"`
	AcceptedUnmerged  bool                     `json:"accepted_unmerged,omitempty"`
}

type sessionWorkspaceLimitedWriter struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

type sessionWorkspaceWindowWriter struct {
	buf    bytes.Buffer
	digest hash.Hash
	offset int64
	limit  int64
	total  int64
}

func (w *sessionWorkspaceLimitedWriter) Write(p []byte) (int, error) {
	if w.limit < 0 {
		return 0, errors.New("negative output limit")
	}
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = w.buf.Write(p)
		} else {
			_, _ = w.buf.Write(p[:remaining])
		}
	}
	if len(p) > remaining {
		w.truncated = true
	}
	return len(p), nil
}

func runSessionWorkspaceGit(dir string, limit int, args ...string) ([]byte, bool, error) {
	return runSessionWorkspaceGitWithEnv(dir, limit, nil, args...)
}

func runSessionWorkspaceGitWithEnv(
	dir string, limit int, env []string, args ...string,
) ([]byte, bool, error) {
	stdout := &sessionWorkspaceLimitedWriter{limit: limit}
	stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	cmd := exec.Command("git", gitArgs(dir, args)...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.buf.String())
		if detail != "" {
			return nil, false, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, detail)
		}
		return nil, false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.buf.Bytes(), stdout.truncated, nil
}

func (w *sessionWorkspaceWindowWriter) Write(p []byte) (int, error) {
	if w.offset < 0 || w.limit < 0 {
		return 0, errors.New("negative output window")
	}
	if w.digest == nil {
		w.digest = sha256.New()
	}
	_, _ = w.digest.Write(p)
	start := w.total
	w.total += int64(len(p))
	windowStart := w.offset
	windowEnd := w.offset + w.limit
	chunkEnd := start + int64(len(p))
	copyStart := max(start, windowStart)
	copyEnd := min(chunkEnd, windowEnd)
	if copyStart < copyEnd {
		from := copyStart - start
		to := copyEnd - start
		_, _ = w.buf.Write(p[from:to])
	}
	return len(p), nil
}

func runSessionWorkspaceGitWindow(
	dir string,
	offset int64,
	limit int,
	args ...string,
) ([]byte, int64, string, bool, error) {
	if offset < 0 || limit < 1 || limit > sessionWorkspacePatchLimit {
		return nil, 0, "", false, errors.New("invalid output window")
	}
	stdout := &sessionWorkspaceWindowWriter{
		digest: sha256.New(),
		offset: offset,
		limit:  int64(limit),
	}
	stderr := &sessionWorkspaceLimitedWriter{limit: sessionWorkspaceErrorLimit}
	cmd := exec.Command("git", gitArgs(dir, args)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.buf.String())
		if detail != "" {
			return nil, 0, "", false, fmt.Errorf(
				"git %s: %w: %s",
				strings.Join(args, " "),
				err,
				detail,
			)
		}
		return nil, 0, "", false, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if offset > stdout.total {
		return nil, stdout.total, hex.EncodeToString(stdout.digest.Sum(nil)), false,
			errors.New("output window starts beyond the patch")
	}
	hasMore := offset+int64(stdout.buf.Len()) < stdout.total
	return stdout.buf.Bytes(), stdout.total,
		hex.EncodeToString(stdout.digest.Sum(nil)), hasMore, nil
}

func sessionWorkspaceGitText(dir string, limit int, args ...string) ([]byte, error) {
	out, truncated, err := runSessionWorkspaceGit(dir, limit, args...)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("git %s output exceeds %d bytes", strings.Join(args, " "), limit)
	}
	return out, nil
}

func sessionWorkspaceCommit(dir, revision string) (string, error) {
	if revision == "" || strings.ContainsAny(revision, "\x00\r\n") {
		return "", errors.New("invalid commit revision")
	}
	out, err := sessionWorkspaceGitText(dir, sessionWorkspaceGitOutputLimit,
		"rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve commit %q: %w", revision, err)
	}
	commit := strings.TrimSpace(string(out))
	if !validSessionWorkspaceCommit(commit) {
		return "", fmt.Errorf("resolve commit %q: malformed identity %q", revision, commit)
	}
	return commit, nil
}

func validSessionWorkspaceCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, r := range commit {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func sessionWorkspaceBranch(dir string) (string, error) {
	out, err := sessionWorkspaceGitText(dir, 4<<10, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", errSessionWorkspaceDetachedHead
		}
		return "", fmt.Errorf("resolve workspace branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || strings.ContainsAny(branch, "\x00\r\n") {
		return "", errors.New("workspace has no valid branch")
	}
	return branch, nil
}

func createSessionWorkspace(repo, generatedName string) (sessionWorkspace, error) {
	if repo == "" {
		return sessionWorkspace{}, errors.New("repository is required")
	}
	if !forkspace.ValidName(generatedName) {
		return sessionWorkspace{}, fmt.Errorf("invalid session workspace name %q", generatedName)
	}
	base, err := sessionWorkspaceCommit(repo, "HEAD")
	if err != nil {
		return sessionWorkspace{}, fmt.Errorf("capture parent HEAD: %w", err)
	}
	return ensureSessionWorkspace(repo, generatedName, base)
}

// ensureSessionWorkspace creates or adopts only the deterministic workspace bound to an
// already-persisted base. An existing path is read-only inspected: a crash-recovery retry must
// never reset, clean, or replace an ambiguous workspace it did not create.
func ensureSessionWorkspace(repo, generatedName, base string) (sessionWorkspace, error) {
	if repo == "" || !filepath.IsAbs(repo) || !forkspace.ValidName(generatedName) || !validSessionWorkspaceCommit(base) {
		return sessionWorkspace{}, errors.New("invalid session workspace binding")
	}
	ws := forkspace.Workspace(repo, generatedName)
	unlock, err := forkspace.LockState(repo, generatedName)
	if err != nil {
		return sessionWorkspace{}, fmt.Errorf("lock session workspace %s: %w", generatedName, err)
	}
	defer unlock()

	_, statErr := os.Lstat(ws)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return sessionWorkspace{}, fmt.Errorf("inspect session workspace %q: %w", generatedName, statErr)
	}
	if created {
		createdPath, createErr := forkspace.Setup(repo, generatedName)
		if createErr != nil {
			return sessionWorkspace{}, removeCreatedSessionWorkspace(ws, fmt.Errorf("create session workspace: %w", createErr))
		}
		if createdPath != ws {
			return sessionWorkspace{}, fmt.Errorf("setup fork returned unexpected workspace %q", createdPath)
		}
		cleanupPartial := func(cause error) error {
			handle, info, pinErr := forkspace.Pin(ws)
			if errors.Is(pinErr, os.ErrNotExist) {
				return cause
			}
			if pinErr != nil {
				return errors.Join(cause, fmt.Errorf("cannot prove ownership of partial workspace %q: %w", ws, pinErr))
			}
			defer handle.Close()
			if !forkspace.SamePinned(ws, info) {
				return errors.Join(cause, fmt.Errorf("partial workspace %q changed before cleanup", ws))
			}
			if removeErr := os.RemoveAll(ws); removeErr != nil {
				return errors.Join(cause, fmt.Errorf("remove partial workspace %q: %w", ws, removeErr))
			}
			return cause
		}
		if head, headErr := sessionWorkspaceCommit(ws, "HEAD"); headErr != nil {
			return sessionWorkspace{}, cleanupPartial(fmt.Errorf("verify new session workspace HEAD: %w", headErr))
		} else if head != base {
			if _, _, resetErr := runSessionWorkspaceGit(ws, sessionWorkspaceGitOutputLimit, "reset", "--hard", base); resetErr != nil {
				return sessionWorkspace{}, cleanupPartial(fmt.Errorf("reset new session workspace to persisted base: %w", resetErr))
			}
		}
	}

	workspace, err := verifySessionWorkspace(repo, generatedName, ws, base)
	if err != nil {
		if created {
			return sessionWorkspace{}, removeCreatedSessionWorkspace(ws, err)
		}
		return sessionWorkspace{}, fmt.Errorf("refusing to adopt existing session workspace: %w", err)
	}
	return workspace, nil
}

func removeCreatedSessionWorkspace(path string, cause error) error {
	handle, info, pinErr := forkspace.Pin(path)
	if errors.Is(pinErr, os.ErrNotExist) {
		return cause
	}
	if pinErr != nil {
		return errors.Join(cause, fmt.Errorf("cannot prove ownership of new workspace %q: %w", path, pinErr))
	}
	defer handle.Close()
	if !forkspace.SamePinned(path, info) {
		return errors.Join(cause, fmt.Errorf("new workspace %q changed before cleanup", path))
	}
	if err := os.RemoveAll(path); err != nil {
		return errors.Join(cause, fmt.Errorf("remove new workspace %q: %w", path, err))
	}
	return cause
}

func verifySessionWorkspace(repo, name, workspace, base string) (sessionWorkspace, error) {
	info, err := os.Lstat(workspace)
	if err != nil {
		return sessionWorkspace{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return sessionWorkspace{}, errors.New("workspace is not a real directory")
	}
	branch, err := sessionWorkspaceBranch(workspace)
	if err != nil {
		return sessionWorkspace{}, err
	}
	head, err := sessionWorkspaceCommit(workspace, "HEAD")
	if err != nil {
		return sessionWorkspace{}, err
	}
	if branch != name {
		return sessionWorkspace{}, fmt.Errorf("workspace branch is %q, want %q", branch, name)
	}
	if head != base {
		return sessionWorkspace{}, fmt.Errorf("workspace HEAD is %s, want persisted base %s", head, base)
	}
	status, truncated, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "-z")
	if err != nil {
		return sessionWorkspace{}, fmt.Errorf("verify workspace cleanliness: %w", err)
	}
	if truncated || len(status) != 0 {
		return sessionWorkspace{}, errors.New("workspace is not clean")
	}
	origin, err := sessionWorkspaceGitText(workspace, 4<<10, "remote", "get-url", "origin")
	if err != nil {
		return sessionWorkspace{}, fmt.Errorf("verify workspace origin: %w", err)
	}
	if !sameRealPath(strings.TrimSpace(string(origin)), repo) {
		return sessionWorkspace{}, fmt.Errorf("workspace origin is not repository %q", repo)
	}
	return sessionWorkspace{Repo: repo, Name: name, Path: workspace, Branch: branch, BaseCommit: base, ForkHead: head}, nil
}

func sameRealPath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftReal, leftErr := filepath.EvalSymlinks(leftAbs)
	rightReal, rightErr := filepath.EvalSymlinks(rightAbs)
	if leftErr != nil || rightErr != nil {
		return filepath.Clean(leftAbs) == filepath.Clean(rightAbs)
	}
	return filepath.Clean(leftReal) == filepath.Clean(rightReal)
}

func sessionWorkspaceChangeFor(status string, path, oldPath []byte) sessionWorkspaceChange {
	change := sessionWorkspaceChange{
		Path:      string(path),
		PathBytes: append([]byte(nil), path...),
		Status:    status,
	}
	if len(oldPath) > 0 {
		change.OldPath = string(oldPath)
		change.OldPathBytes = append([]byte(nil), oldPath...)
	}
	return change
}

func appendSessionWorkspaceXY(staged, unstaged, conflicts *[]sessionWorkspaceChange, xy string, path, oldPath []byte) error {
	if len(xy) != 2 {
		return fmt.Errorf("malformed Git status code %q", xy)
	}
	if xy[0] != '.' {
		*staged = append(*staged, sessionWorkspaceChangeFor(string(xy[0]), path, oldPath))
	}
	if xy[1] != '.' {
		*unstaged = append(*unstaged, sessionWorkspaceChangeFor(string(xy[1]), path, oldPath))
	}
	if xy[0] == 'U' || xy[1] == 'U' {
		*conflicts = append(*conflicts, sessionWorkspaceChangeFor(xy, path, oldPath))
	}
	return nil
}

func parseSessionWorkspaceStatus(raw []byte) (WorkspaceChanges, error) {
	var changes WorkspaceChanges
	records := bytes.Split(raw, []byte{0})
	for i := 0; i < len(records); i++ {
		record := records[i]
		if len(record) == 0 {
			continue
		}
		fields := strings.SplitN(string(record), " ", 9)
		switch record[0] {
		case '1':
			if len(fields) != 9 {
				return WorkspaceChanges{}, fmt.Errorf("malformed ordinary Git status record")
			}
			if err := appendSessionWorkspaceXY(&changes.Staged, &changes.Unstaged, &changes.Conflicts, fields[1], []byte(fields[8]), nil); err != nil {
				return WorkspaceChanges{}, err
			}
		case '2':
			if len(strings.SplitN(string(record), " ", 10)) != 10 || i+1 >= len(records) || len(records[i+1]) == 0 {
				return WorkspaceChanges{}, fmt.Errorf("malformed rename Git status record")
			}
			fields = strings.SplitN(string(record), " ", 10)
			i++
			if err := appendSessionWorkspaceXY(&changes.Staged, &changes.Unstaged, &changes.Conflicts, fields[1], []byte(fields[9]), records[i]); err != nil {
				return WorkspaceChanges{}, err
			}
		case 'u':
			fields = strings.SplitN(string(record), " ", 11)
			if len(fields) != 11 {
				return WorkspaceChanges{}, fmt.Errorf("malformed unmerged Git status record")
			}
			changes.Conflicts = append(changes.Conflicts, sessionWorkspaceChangeFor(fields[1], []byte(fields[10]), nil))
		case '?':
			if len(record) < 3 || record[1] != ' ' {
				return WorkspaceChanges{}, fmt.Errorf("malformed untracked Git status record")
			}
			changes.Untracked = append(changes.Untracked, sessionWorkspaceChangeFor("??", record[2:], nil))
		case '!':
			// Ignored entries are not requested by this inspection and are not emitted by the
			// command below. Keep a fail-closed parser if that command is changed later.
			return WorkspaceChanges{}, errors.New("unexpected ignored Git status record")
		default:
			return WorkspaceChanges{}, fmt.Errorf("unknown Git status record %q", record[0])
		}
	}
	return changes, nil
}

func parseSessionWorkspaceNameStatus(raw []byte) ([]sessionWorkspaceChange, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	records := bytes.Split(raw, []byte{0})
	changes := make([]sessionWorkspaceChange, 0, len(records)/2)
	for i := 0; i < len(records); i++ {
		if len(records[i]) == 0 {
			continue
		}
		if i+1 >= len(records) || len(records[i+1]) == 0 {
			return nil, errors.New("malformed Git name-status output")
		}
		changes = append(changes, sessionWorkspaceChangeFor(string(records[i]), records[i+1], nil))
		i++
	}
	return changes, nil
}

func sessionWorkspaceCounts(dir string, args ...string) (int, int, error) {
	out, err := sessionWorkspaceGitText(dir, 128, args...)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("malformed Git count output %q", string(out))
	}
	left, leftErr := strconv.Atoi(fields[0])
	right, rightErr := strconv.Atoi(fields[1])
	if leftErr != nil || rightErr != nil || left < 0 || right < 0 {
		return 0, 0, fmt.Errorf("malformed Git counts %q", string(out))
	}
	return left, right, nil
}

func sessionWorkspaceCount(dir string, args ...string) (int, error) {
	out, err := sessionWorkspaceGitText(dir, 128, args...)
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || count < 0 {
		return 0, fmt.Errorf("malformed Git count %q", string(out))
	}
	return count, nil
}

func inspectSessionChanges(repo, workspace, base string, maxPatchBytes int) (WorkspaceChanges, error) {
	parentHead, err := sessionWorkspaceCommit(repo, "HEAD")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve current parent HEAD: %w", err)
	}
	return inspectSessionChangesPageAtParent(repo, workspace, base, parentHead, 0, maxPatchBytes)
}

func inspectSessionChangesPage(
	repo string,
	workspace string,
	base string,
	patchOffset int64,
	patchLimit int,
) (WorkspaceChanges, error) {
	parentHead, err := sessionWorkspaceCommit(repo, "HEAD")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve current parent HEAD: %w", err)
	}
	return inspectSessionChangesPageAtParent(repo, workspace, base, parentHead, patchOffset, patchLimit)
}

func inspectSessionChangesPageAtParent(
	repo string,
	workspace string,
	base string,
	parent string,
	patchOffset int64,
	patchLimit int,
) (WorkspaceChanges, error) {
	if repo == "" || workspace == "" {
		return WorkspaceChanges{}, errors.New("repository and workspace are required")
	}
	if patchOffset < 0 {
		return WorkspaceChanges{}, errors.New("patch offset must not be negative")
	}
	if patchLimit < 1 || patchLimit > sessionWorkspacePatchLimit {
		return WorkspaceChanges{}, fmt.Errorf("patch limit must be between 1 and %d bytes", sessionWorkspacePatchLimit)
	}
	baseCommit, err := sessionWorkspaceCommit(repo, base)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve inspection base: %w", err)
	}
	forkHead, err := sessionWorkspaceCommit(workspace, "HEAD")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve workspace HEAD: %w", err)
	}
	forkTree, err := sessionWorkspaceTree(workspace, forkHead)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve workspace tree: %w", err)
	}
	parentHead, err := sessionWorkspaceCommit(repo, parent)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("resolve current parent: %w", err)
	}

	statusRaw, statusTruncated, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "-z")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("inspect workspace status: %w", err)
	}
	if statusTruncated {
		return WorkspaceChanges{}, fmt.Errorf("workspace status exceeds %d bytes", sessionWorkspaceGitOutputLimit)
	}
	changes, err := parseSessionWorkspaceStatus(statusRaw)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("parse workspace status: %w", err)
	}
	committedRaw, committedTruncated, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"diff", "--name-status", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", baseCommit, forkHead, "--")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("inspect committed workspace changes: %w", err)
	}
	if committedTruncated {
		return WorkspaceChanges{}, fmt.Errorf("committed workspace changes exceed %d bytes", sessionWorkspaceGitOutputLimit)
	}
	changes.Committed, err = parseSessionWorkspaceNameStatus(committedRaw)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("parse committed workspace changes: %w", err)
	}

	patch, patchBytes, patchDigest, patchHasMore, err := runSessionWorkspaceGitWindow(
		workspace,
		patchOffset,
		patchLimit,
		"diff", "--no-ext-diff", "--no-textconv", "--binary", baseCommit, "--")
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("inspect tracked workspace patch: %w", err)
	}
	// The parent may have advanced since this fork was cloned. Import only the exact trusted
	// parent commit into the fork object database, without updating FETCH_HEAD or any ref, so the
	// cross-repository divergence calculation does not turn a normal parent advance into "bad object".
	if _, _, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", repo, parentHead); err != nil {
		return WorkspaceChanges{}, fmt.Errorf("read current parent for comparison: %w", err)
	}
	ahead, behind, err := sessionWorkspaceCounts(workspace, "rev-list", "--left-right", "--count", parentHead+"..."+forkHead)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("compare workspace with current parent: %w", err)
	}
	baseToFork, err := sessionWorkspaceCount(workspace, "rev-list", "--count", baseCommit+".."+forkHead)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("count workspace commits from base: %w", err)
	}
	baseToParent, err := sessionWorkspaceCount(repo, "rev-list", "--count", baseCommit+".."+parentHead)
	if err != nil {
		return WorkspaceChanges{}, fmt.Errorf("count parent commits from base: %w", err)
	}
	changes.BaseCommit = baseCommit
	changes.ForkHead = forkHead
	changes.ForkTree = forkTree
	changes.ParentHead = parentHead
	changes.ParentDivergence = sessionWorkspaceParentDivergence{
		Ahead:        behind,
		Behind:       ahead,
		BaseToFork:   baseToFork,
		BaseToParent: baseToParent,
		Diverged:     ahead > 0 && behind > 0,
	}
	changes.Patch = string(patch)
	changes.PatchDigest = patchDigest
	changes.PatchBytes = patchBytes
	changes.PatchOffset = patchOffset
	changes.PatchNextOffset = patchOffset + int64(len(patch))
	changes.PatchHasMore = patchHasMore
	changes.Truncated = patchOffset > 0 || patchHasMore
	return changes, nil
}

func sessionWorkspaceTree(dir, revision string) (string, error) {
	out, err := sessionWorkspaceGitText(
		dir, sessionWorkspaceGitOutputLimit,
		"rev-parse", "--verify", "--end-of-options", revision+"^{tree}",
	)
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(out))
	if !validSessionWorkspaceCommit(tree) {
		return "", fmt.Errorf("malformed tree identity %q", tree)
	}
	return tree, nil
}

func sessionWorkspaceName(repo, workspace string) (string, error) {
	if repo == "" || workspace == "" {
		return "", errors.New("repository and workspace are required")
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace path: %w", err)
	}
	name := filepath.Base(absWorkspace)
	if !forkspace.ValidExistingName(name) {
		return "", fmt.Errorf("invalid session workspace path %q", workspace)
	}
	expected, err := filepath.Abs(forkspace.Workspace(repo, name))
	if err != nil || filepath.Clean(expected) != filepath.Clean(absWorkspace) {
		return "", fmt.Errorf("workspace %q is not a fork of repository %q", workspace, repo)
	}
	return name, nil
}

func sessionWorkspaceIdentityFor(info os.FileInfo) (sessionWorkspaceIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return sessionWorkspaceIdentity{}, errors.New("workspace has unsupported inode metadata")
	}
	return sessionWorkspaceIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func sessionWorkspaceStatusDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func planSessionWorkspaceDiscard(repo, workspace string, acceptDirty, acceptUnmerged bool) (WorkspaceDiscardPlan, error) {
	parentHead, err := sessionWorkspaceCommit(repo, "HEAD")
	if err != nil {
		return WorkspaceDiscardPlan{}, fmt.Errorf("resolve discard parent: %w", err)
	}
	return planSessionWorkspaceDiscardAtParent(repo, workspace, parentHead, acceptDirty, acceptUnmerged)
}

func planSessionWorkspaceDiscardAtParent(
	repo, workspace, parent string,
	acceptDirty, acceptUnmerged bool,
) (WorkspaceDiscardPlan, error) {
	name, err := sessionWorkspaceName(repo, workspace)
	if err != nil {
		return WorkspaceDiscardPlan{}, err
	}
	handle, info, err := forkspace.Pin(workspace)
	if errors.Is(err, os.ErrNotExist) {
		// The workspace is already gone — crashed teardown, manual removal, a
		// reinstalled machine. There is nothing to inspect and nothing the dirty
		// or unmerged guards could protect, and discardSessionWorkspace already
		// treats a missing workspace as removed. Refusing to PLAN here was the
		// gap that made such sessions unremovable: the record could never reach
		// discarded, and cleanup retried into a permanent internal_error. Only
		// absence takes this path; a workspace that exists but fails inspection
		// still fails loudly, because that one may hold work.
		return WorkspaceDiscardPlan{
			Repo: repo, Name: name, Workspace: workspace,
			StatusDigest: sessionWorkspaceStatusDigest(nil),
		}, nil
	}
	if err != nil {
		return WorkspaceDiscardPlan{}, fmt.Errorf("pin session workspace for discard: %w", err)
	}
	defer handle.Close()
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil {
		return WorkspaceDiscardPlan{}, err
	}
	branch, err := sessionWorkspaceBranch(workspace)
	if err != nil {
		return WorkspaceDiscardPlan{}, err
	}
	head, err := sessionWorkspaceCommit(workspace, "HEAD")
	if err != nil {
		return WorkspaceDiscardPlan{}, err
	}
	statusRaw, truncated, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "-z")
	if err != nil {
		return WorkspaceDiscardPlan{}, fmt.Errorf("inspect discard status: %w", err)
	}
	if truncated {
		return WorkspaceDiscardPlan{}, fmt.Errorf("discard status exceeds %d bytes", sessionWorkspaceGitOutputLimit)
	}
	if _, err := parseSessionWorkspaceStatus(statusRaw); err != nil {
		return WorkspaceDiscardPlan{}, fmt.Errorf("parse discard status: %w", err)
	}
	if !forkspace.SamePinned(workspace, info) {
		return WorkspaceDiscardPlan{}, errors.New("workspace changed while planning discard")
	}
	dirty := len(statusRaw) > 0
	// "Unmerged" means committed work that is not already contained by the parent, matching
	// `coop fork rm`; a clean fork with valuable commits still requires explicit loss consent.
	parentHead, err := sessionWorkspaceCommit(repo, parent)
	if err != nil {
		return WorkspaceDiscardPlan{}, fmt.Errorf("resolve discard parent: %w", err)
	}
	unmerged, err := sessionWorkspaceUnmergedFromParent(repo, workspace, parentHead)
	if err != nil {
		return WorkspaceDiscardPlan{}, fmt.Errorf("compare workspace with discard parent: %w", err)
	}
	return WorkspaceDiscardPlan{
		Repo:              repo,
		Name:              name,
		Workspace:         workspace,
		WorkspaceIdentity: identity,
		Branch:            branch,
		Head:              head,
		ParentHead:        parentHead,
		StatusDigest:      sessionWorkspaceStatusDigest(statusRaw),
		Dirty:             dirty,
		Unmerged:          unmerged,
		Running:           forkspace.NeedsStop(repo, name),
		AcceptedDirty:     dirty && acceptDirty,
		AcceptedUnmerged:  unmerged && acceptUnmerged,
	}, nil
}

func discardSessionWorkspace(plan WorkspaceDiscardPlan) error {
	name, err := sessionWorkspaceName(plan.Repo, plan.Workspace)
	if err != nil || name != plan.Name {
		if err != nil {
			return fmt.Errorf("invalid discard plan: %w", err)
		}
		return errors.New("invalid discard plan: workspace name changed")
	}
	unlock, err := forkspace.LockState(plan.Repo, plan.Name)
	if err != nil {
		return fmt.Errorf("lock session workspace discard: %w", err)
	}
	defer unlock()
	if plan.Running || forkspace.NeedsStop(plan.Repo, plan.Name) {
		return errors.New("refusing to discard a running or cleanup-pending workspace")
	}
	handle, info, err := forkspace.Pin(plan.Workspace)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if plan.Running || forkspace.NeedsStop(plan.Repo, plan.Name) {
				return errors.New("refusing to discard a missing workspace with active or pending work")
			}
			return nil
		}
		return fmt.Errorf("discard plan is stale: pin workspace: %w", err)
	}
	defer handle.Close()
	identity, err := sessionWorkspaceIdentityFor(info)
	if err != nil {
		return fmt.Errorf("discard plan is stale: %w", err)
	}
	if identity != plan.WorkspaceIdentity || !forkspace.SamePinned(plan.Workspace, info) {
		return errors.New("discard plan is stale: workspace was replaced")
	}
	branch, err := sessionWorkspaceBranch(plan.Workspace)
	if err != nil {
		return fmt.Errorf("discard plan is stale: branch: %w", err)
	}
	head, err := sessionWorkspaceCommit(plan.Workspace, "HEAD")
	if err != nil {
		return fmt.Errorf("discard plan is stale: HEAD: %w", err)
	}
	statusRaw, truncated, err := runSessionWorkspaceGit(plan.Workspace, sessionWorkspaceGitOutputLimit,
		"status", "--porcelain=v2", "--untracked-files=all", "--no-renames", "-z")
	if err != nil {
		return fmt.Errorf("discard plan is stale: status: %w", err)
	}
	if truncated {
		return fmt.Errorf("discard plan is stale: status exceeds %d bytes", sessionWorkspaceGitOutputLimit)
	}
	if _, err := parseSessionWorkspaceStatus(statusRaw); err != nil {
		return fmt.Errorf("discard plan is stale: parse status: %w", err)
	}
	dirty := len(statusRaw) > 0
	unmerged, err := sessionWorkspaceUnmergedFromParent(plan.Repo, plan.Workspace, plan.ParentHead)
	if err != nil {
		return fmt.Errorf("discard plan is stale: compare parent: %w", err)
	}
	if branch != plan.Branch || head != plan.Head || sessionWorkspaceStatusDigest(statusRaw) != plan.StatusDigest || dirty != plan.Dirty || unmerged != plan.Unmerged {
		return errors.New("discard plan is stale: workspace HEAD, branch, or status changed")
	}
	if dirty && !plan.AcceptedDirty {
		return errors.New("refusing to discard dirty workspace without exact dirty-loss acknowledgement")
	}
	if unmerged && !plan.AcceptedUnmerged {
		return errors.New("refusing to discard unmerged workspace without exact unmerged-loss acknowledgement")
	}
	if plan.Running || forkspace.NeedsStop(plan.Repo, plan.Name) {
		return errors.New("refusing to discard a workspace that started running")
	}
	if !forkspace.SamePinned(plan.Workspace, info) {
		return errors.New("discard plan is stale: workspace was replaced before removal")
	}
	// The on-disk removal only: the session service already brought this workspace's sibling
	// services down (with their volumes) before planning the discard, so nothing here may do it
	// a second time.
	if err := forkspace.Destroy(plan.Repo, plan.Name); err != nil {
		return fmt.Errorf("discard session workspace: %w", err)
	}
	return nil
}

func sessionWorkspaceUnmergedFromParent(repo, workspace, parentHead string) (bool, error) {
	if !validSessionWorkspaceCommit(parentHead) {
		return false, errors.New("parent commit is invalid")
	}
	if _, _, err := runSessionWorkspaceGit(workspace, sessionWorkspaceGitOutputLimit,
		"fetch", "--quiet", "--no-write-fetch-head", "--no-tags", "--", repo, parentHead); err != nil {
		return false, fmt.Errorf("fetch parent commit: %w", err)
	}
	head, err := sessionWorkspaceCommit(workspace, "HEAD")
	if err != nil {
		return false, err
	}
	cmd := exec.Command("git", gitArgs(workspace, []string{"merge-base", "--is-ancestor", head, parentHead})...)
	err = cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}
