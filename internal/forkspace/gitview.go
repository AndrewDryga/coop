package forkspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Every repository coop runs git against is agent-writable, and git executes whatever the
// repository's configuration names: clean/smudge/process filters and textconv drivers on status,
// diff, checkout, rebase and apply; merge drivers on rebase. GitHardening blanks the fixed-name
// knobs, but a driver's name is arbitrary, and enumerating the names first (the retired
// DriverNeutralizer) missed included config and lost the race against an agent writing the config
// between the enumeration and git's own read of it.
//
// The trusted view closes the whole class: host git never reads the repository's configuration.
// GIT_DIR points at a coop-owned directory whose config coop generated from an allowlist
// (gitview_config.go), while the real object store, refs, reflogs and index sit behind it —
// objects/refs/logs/packed-refs/shallow as symlinks into the real (common) git dir, HEAD as a
// regenerated copy (git rejects a symlinked HEAD whose target is not refs/…), the real index named
// by GIT_INDEX_FILE. Nothing hooks/info/attributes/config.worktree/modules may name can be found,
// so an in-tree .gitattributes that assigns a driver refers to nothing and is inert. It is
// race-free by construction: git reads exactly the bytes coop wrote.
//
// Writes are safe when they touch loose refs, the index and files: those land inside the
// symlinked directories or on the real index. A write that REPLACES a top-level entry by rename —
// packed-refs (pack-refs, deleting a packed branch), the worktree list, HEAD itself (symbolic-ref)
// — would land in the view, so those run through GitRefCommand on the real git dir instead; they
// pass no content through a driver. Auto-gc is disabled under the view for the same reason.
//
// Views are per process (a shared view would let one process refresh HEAD under another's rebase)
// and removed by CloseGitViews at exit; a crash leaves a small coop-gitview-* directory in the
// temp dir and any in-flight rebase state with it — the worktree is then just dirty, and `git
// reset --hard` recovers it.

// gitViewHardening is appended after GitHardening on every view-side command: no auto-gc or
// maintenance (they pack refs), no pruning, no commit-graph writes, and no submodule descent (a
// submodule's own git dir is agent-writable too).
var gitViewHardening = []string{
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "fetch.writeCommitGraph=false",
	"-c", "fetch.prune=false",
	"-c", "diff.ignoreSubmodules=all",
	"-c", "status.submoduleSummary=false",
	"-c", "submodule.recurse=false",
}

type gitView struct {
	workTree  string
	gitDir    string // the worktree's own git dir (HEAD, index, logs/HEAD)
	commonDir string // objects, refs, packed-refs, config — same as gitDir for a plain clone
	dir       string
}

var (
	gitViewsMu   sync.Mutex
	gitViews     = map[string]*gitView{} // by git dir
	gitViewsRoot string
)

// GitViewRootEnv overrides where views live — tests point it at a scratch directory. Without it,
// a test binary uses a per-process temp root (removed by CloseGitViews) and coop uses
// ~/.local/state/coop/gitviews, where a view outlives the process: an interrupted rebase keeps
// its state there for the next `coop fork merge` to recover.
const GitViewRootEnv = "COOP_GITVIEW_ROOT"

// GitCommand builds `git -C dir <hardening> <args>` under the trusted view of dir's repository.
// It is the one way host code runs git against a working tree; see the package comment.
func GitCommand(ctx context.Context, dir string, args ...string) (*exec.Cmd, error) {
	return GitCommandWithEnv(ctx, dir, os.Environ(), args...)
}

// GitCommandWithEnv is GitCommand with a caller-chosen base environment (extra GIT_CONFIG_*
// entries, a private HOME); the repository-locating variables are still the view's own.
func GitCommandWithEnv(ctx context.Context, dir string, env []string, args ...string) (*exec.Cmd, error) {
	full := append(append(append([]string{"-C", dir}, GitHardening...), gitViewHardening...), args...)
	view, err := openGitView(ctx, dir)
	if errors.Is(err, errNoRepository) {
		// Nothing to shield: with no repository above dir, git itself finds no config to read
		// (the repository-locating variables are stripped) and reports that in its own words.
		cmd := exec.CommandContext(ctx, "git", full...)
		cmd.Env = withoutGitEnv(env)
		return cmd, nil
	}
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = view.envFrom(env)
	return cmd, nil
}

var errNoRepository = errors.New("not inside a git repository")

// GitRefCommand builds a hardened git command on the REAL git dir, for the few operations that
// replace a top-level git-dir entry by rename and so cannot run under the view: pack-refs and the
// deletion of a packed ref, update-ref, a writing symbolic-ref, worktree add/remove. None of them
// passes worktree content through a filter or diff driver; hooks are blanked by the hardening.
func GitRefCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	full := append(append(append([]string{"-C", dir}, GitHardening...), gitViewHardening...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = withoutGitEnv(os.Environ())
	return cmd
}

// CloseGitViews forgets this process's views and, for a test binary's temp root, removes them.
func CloseGitViews() {
	gitViewsMu.Lock()
	defer gitViewsMu.Unlock()
	if gitViewsRoot != "" && strings.HasPrefix(filepath.Base(gitViewsRoot), "coop-gitview-") {
		_ = os.RemoveAll(gitViewsRoot)
	}
	gitViewsRoot = ""
	gitViews = map[string]*gitView{}
}

func gitViewRoot() (string, error) {
	if gitViewsRoot != "" {
		return gitViewsRoot, nil
	}
	root := os.Getenv(GitViewRootEnv)
	switch {
	case root != "":
	case strings.HasSuffix(os.Args[0], ".test"):
		dir, err := os.MkdirTemp("", "coop-gitview-")
		if err != nil {
			return "", err
		}
		root = dir
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state", "coop", "gitviews")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	gitViewsRoot = root
	return root, nil
}

func openGitView(ctx context.Context, workTree string) (*gitView, error) {
	gitDir, commonDir, err := gitDirsOf(workTree)
	if err != nil {
		return nil, err
	}
	gitViewsMu.Lock()
	defer gitViewsMu.Unlock()
	if view := gitViews[gitDir]; view != nil {
		// One git dir can serve successive worktree paths (git reuses a removed worktree's slot
		// under .git/worktrees/<name>), so the view follows the path it is asked for — and the
		// state an operation left behind in the previous worktree's life goes with that life.
		if view.workTree != workTree {
			view.workTree = workTree
			if err := view.clearOperationState(); err != nil {
				return nil, err
			}
		}
		if err := view.refresh(ctx); err != nil {
			return nil, err
		}
		return view, nil
	}
	root, err := gitViewRoot()
	if err != nil {
		return nil, fmt.Errorf("git view root: %w", err)
	}
	sum := sha256.Sum256([]byte(gitDir))
	dir := filepath.Join(root, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	view := &gitView{workTree: workTree, gitDir: gitDir, commonDir: commonDir, dir: dir}
	if err := view.populate(ctx); err != nil {
		return nil, fmt.Errorf("trusted git view of %s: %w", workTree, err)
	}
	gitViews[gitDir] = view
	return view, nil
}

// gitDirsOf resolves the worktree's git dir and common dir the way git does, without running
// git: the nearest `.git` above workTree — a directory, or a `gitdir: <path>` file for a linked
// worktree — and that dir's `commondir` pointer when it has one. Symlinks are resolved first so
// one repository has one view.
func gitDirsOf(workTree string) (gitDir, commonDir string, err error) {
	abs, err := filepath.Abs(workTree)
	if err != nil {
		return "", "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	// A bare repository (coop's own audit snapshots) is its own git dir: no .git, but HEAD and
	// an object store right here.
	if fileExists(filepath.Join(abs, "HEAD")) && fileExists(filepath.Join(abs, "objects")) && !fileExists(filepath.Join(abs, ".git")) {
		return abs, abs, nil
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		dotGit := filepath.Join(dir, ".git")
		info, statErr := os.Stat(dotGit)
		switch {
		case statErr == nil && info.IsDir():
			gitDir = dotGit
		case statErr == nil:
			data, err := os.ReadFile(dotGit)
			if err != nil {
				return "", "", err
			}
			target := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
			if target == "" || !strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir:") {
				return "", "", fmt.Errorf("%s is not a git directory pointer", dotGit)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			gitDir = filepath.Clean(target)
		default:
			if parent := filepath.Dir(dir); parent != dir {
				continue
			}
			return "", "", fmt.Errorf("%s: %w", workTree, errNoRepository)
		}
		break
	}
	if real, err := filepath.EvalSymlinks(gitDir); err == nil {
		gitDir = real
	}
	commonDir = gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		target := strings.TrimSpace(string(data))
		if !filepath.IsAbs(target) {
			target = filepath.Join(gitDir, target)
		}
		if real, err := filepath.EvalSymlinks(target); err == nil {
			target = real
		}
		commonDir = filepath.Clean(target)
	}
	return gitDir, commonDir, nil
}

// GitSwitchBranch puts dir on branch: HEAD is rewritten on the real git dir (a view would keep the
// rewrite to itself), then the index and files reset under the view, where the repository's config
// can name no filter for the checkout to run. The tree must be clean; nothing is discarded.
func GitSwitchBranch(ctx context.Context, dir, branch string) error {
	if err := GitRefCommand(ctx, dir, "symbolic-ref", "HEAD", "refs/heads/"+branch).Run(); err != nil {
		return err
	}
	cmd, err := GitCommand(ctx, dir, "reset", "--hard", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	return cmd.Run()
}

// GitDetach is GitSwitchBranch for a detached HEAD at commit.
func GitDetach(ctx context.Context, dir, commit string) error {
	if err := GitRefCommand(ctx, dir, "update-ref", "--no-deref", "HEAD", commit).Run(); err != nil {
		return err
	}
	cmd, err := GitCommand(ctx, dir, "reset", "--hard", "--quiet", commit)
	if err != nil {
		return err
	}
	return cmd.Run()
}

// populate lays out the view once: the two stores that always exist as symlinks, `info/` as a
// real directory (never the agent-writable attributes), `logs/` as a real directory. Everything
// optional, plus HEAD and the config, is maintained by refresh.
func (v *gitView) populate(ctx context.Context) error {
	for _, name := range []string{"objects", "refs"} {
		if err := ensureSymlink(filepath.Join(v.dir, name), filepath.Join(v.commonDir, name), true); err != nil {
			return err
		}
	}
	for _, name := range []string{"info", "logs"} {
		if err := os.MkdirAll(filepath.Join(v.dir, name), 0o700); err != nil {
			return err
		}
	}
	return v.refresh(ctx)
}

// refresh brings the view up to date before a command: optional store entries the real repo may
// have gained or lost since (packed-refs after a pack, a shallow file, reflogs), the config
// projection (a legitimate new remote shows up; a driver never does), and HEAD — unless an
// operation in flight in this view owns HEAD, as a rebase does between its picks.
func (v *gitView) refresh(ctx context.Context) error {
	for _, link := range []struct{ name, source string }{
		{"packed-refs", filepath.Join(v.commonDir, "packed-refs")},
		{"shallow", filepath.Join(v.commonDir, "shallow")},
		{filepath.Join("info", "exclude"), filepath.Join(v.commonDir, "info", "exclude")},
		{filepath.Join("logs", "refs"), filepath.Join(v.commonDir, "logs", "refs")},
		{filepath.Join("logs", "HEAD"), filepath.Join(v.gitDir, "logs", "HEAD")},
	} {
		if err := ensureSymlink(filepath.Join(v.dir, link.name), link.source, false); err != nil {
			return err
		}
	}
	config, err := trustedGitConfig(ctx, filepath.Join(v.commonDir, "config"), v.dir)
	if err != nil {
		return err
	}
	if err := writeViewFile(filepath.Join(v.dir, "config"), config); err != nil {
		return err
	}
	if v.operationInFlight() {
		return nil
	}
	return v.reconcileHead()
}

// reconcileHead keeps the view's HEAD and the real one the same file's worth of truth. They
// diverge in two legitimate ways: a ref-store command on the real git dir moved HEAD (a
// symbolic-ref switch), or a view command did (a finished or aborted rebase re-attaching HEAD to
// its branch, a `checkout -b`). The later write wins, by modification time — both are written on
// this host, moments apart at most — and is copied over the other, so the real repository always
// records the branch its files and index are on.
func (v *gitView) reconcileHead() error {
	realPath, viewPath := filepath.Join(v.gitDir, "HEAD"), filepath.Join(v.dir, "HEAD")
	realHead, err := os.ReadFile(realPath)
	if err != nil {
		return err
	}
	viewHead, err := os.ReadFile(viewPath)
	if errors.Is(err, os.ErrNotExist) {
		return writeViewFile(viewPath, realHead)
	}
	if err != nil {
		return err
	}
	if string(viewHead) == string(realHead) {
		return nil
	}
	realInfo, err := os.Stat(realPath)
	if err != nil {
		return err
	}
	viewInfo, err := os.Stat(viewPath)
	if err != nil {
		return err
	}
	if viewInfo.ModTime().After(realInfo.ModTime()) {
		return writeViewFile(realPath, viewHead)
	}
	return writeViewFile(viewPath, realHead)
}

// operationInFlight reports whether git left the state of an unfinished rebase, merge, cherry-pick
// or revert in the view; that operation owns HEAD until it finishes or is aborted.
func (v *gitView) operationInFlight() bool {
	for _, name := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		if fileExists(filepath.Join(v.dir, name)) {
			return true
		}
	}
	return false
}

func (v *gitView) clearOperationState() error {
	for _, name := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "ORIG_HEAD"} {
		if err := os.RemoveAll(filepath.Join(v.dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// ensureSymlink keeps path a symlink to source while source exists, and absent otherwise. A
// regular file where the link should be means a rename landed in the view — a packed-refs rewrite
// the ref-store runner was meant to keep on the real git dir — and is refused rather than hidden.
func ensureSymlink(path, source string, required bool) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink == 0:
		return fmt.Errorf("view entry %s is not a symlink: a rewrite landed in the view; remove it and retry", path)
	case err == nil:
		if !fileExists(source) {
			return os.Remove(path)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if !fileExists(source) {
		if required {
			return fmt.Errorf("%s is missing", source)
		}
		return nil
	}
	return os.Symlink(source, path)
}

func writeViewFile(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (v *gitView) envFrom(base []string) []string {
	return append(withoutGitEnv(base),
		"GIT_DIR="+v.dir,
		"GIT_WORK_TREE="+v.workTree,
		"GIT_INDEX_FILE="+filepath.Join(v.gitDir, "index"),
	)
}

// withoutGitEnv drops the repository-locating variables a caller's shell may carry, so they cannot
// redirect a coop command at another repository or index.
func withoutGitEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES":
			continue
		}
		out = append(out, entry)
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
