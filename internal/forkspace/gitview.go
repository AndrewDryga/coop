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

	"github.com/AndrewDryga/coop/internal/shadowpath"
	"golang.org/x/sys/unix"
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
// Production views persist per git directory, so an interrupted rebase can be recovered by the
// next coop process. Refresh uses invocation-private scratch files and leaves an in-flight
// operation's HEAD alone. A test binary must name its own root (internal/testutil/teststate does).

// gitViewHardening is appended after GitHardening on every view-side command: no auto-gc or
// maintenance (they pack refs), no pruning and no commit-graph writes. These defaults
// alone do not prevent submodule descent: .gitmodules can override ignore settings.
// TrustedGitCommand enforces the command-level policy for every view and snapshot.
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
	// mu serializes this view's refresh and guards workTree. A refresh reads files a box can
	// replace, so it never runs under gitViewsMu: one repository's slow or refused refresh holds
	// up only its own commands.
	mu        sync.Mutex
	workTree  string
	gitDir    string // the worktree's own git dir (HEAD, index, logs/HEAD)
	commonDir string // objects, refs, packed-refs, config — same as gitDir for a plain clone
	dir       string
}

var (
	gitViewsMu   sync.Mutex              // guards the map and the root, never a refresh
	gitViews     = map[string]*gitView{} // by git dir
	gitViewsRoot string
)

// GitViewRootEnv overrides where views live, and a test binary must set it. Without it, coop uses
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
	_, viewEnv, err := openGitView(ctx, dir, env)
	if errors.Is(err, errNoRepository) {
		// No repository above dir, so git reports that in its own words. It must not look for one
		// itself when it starts: a box that made a .git, with a filter driver in its config, after
		// this check would have host git run that driver. A git dir that cannot exist stops the
		// search, and the ceiling keeps it below dir's parent.
		noRepository := append(withoutGitEnv(env), "GIT_DIR="+noGitDir, "GIT_CEILING_DIRECTORIES="+filepath.Dir(filepath.Clean(dir)))
		cmd := TrustedGitCommand(ctx, dir, noRepository, args...)
		return cmd, nil
	}
	if err != nil {
		return nil, err
	}
	cmd := TrustedGitCommand(ctx, dir, viewEnv, args...)
	return cmd, nil
}

var errNoRepository = errors.New("not inside a git repository")

// noGitDir is a git dir no one can create: nothing can be made below /dev/null, which is a file.
const noGitDir = "/dev/null/coop-no-repository"

// GitRefCommand builds a hardened git command on the REAL git dir, for the few operations that
// replace a top-level git-dir entry by rename and so cannot run under the view: pack-refs and the
// deletion of a packed ref, update-ref, a writing symbolic-ref, worktree add/remove. None of them
// passes worktree content through a filter or diff driver; hooks are blanked by the hardening. Git
// finds the repository itself here, so checkRefRepository first makes sure it finds a view's.
func GitRefCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := TrustedGitCommand(ctx, dir, withoutGitEnv(os.Environ()), args...)
	if err := refOnlyGitArgs(args); err != nil {
		cmd.Err = err
		return cmd
	}
	err := validateIsolatedGitAccess(dir)
	// git init makes the folder's own repository and never searches above it.
	if err == nil && (len(args) == 0 || args[0] != "init") {
		err = checkRefRepository(dir)
	}
	if err != nil {
		cmd.Err = err
	}
	return cmd
}

// checkRefRepository makes sure git, which searches for the repository itself under
// GitRefCommand, stops at the one a view of dir uses: gitDirsOf refuses a search that climbs to
// the home folder or above, and git passes over a git dir it cannot use, as a .git a box emptied
// or half deleted is, and climbs on, to a home folder kept in Git among others. With no
// repository anywhere above dir, git reports that itself.
func checkRefRepository(dir string) error {
	gitDir, commonDir, err := gitDirsOf(dir)
	if errors.Is(err, errNoRepository) {
		return nil
	}
	if err != nil {
		return err
	}
	if !gitCanUse(gitDir, commonDir) {
		return fmt.Errorf("%s is not a Git repository git can use: it has no valid HEAD, objects or refs; "+
			"restore the folder's .git, or run git init in it", gitDir)
	}
	return nil
}

// gitCanUse is the test git's search puts to a git dir it meets (is_git_directory in git's
// setup.c): objects and refs folders in its common dir, and a HEAD that names a ref or a commit.
func gitCanUse(gitDir, commonDir string) bool {
	for _, name := range []string{"objects", "refs"} {
		if unix.Access(filepath.Join(commonDir, name), unix.X_OK) != nil {
			return false
		}
	}
	head := filepath.Join(gitDir, "HEAD")
	if target, err := os.Readlink(head); err == nil {
		return strings.HasPrefix(target, "refs/") // the linked HEAD old git wrote
	}
	data, err := readGitFile(head)
	if err != nil {
		return false
	}
	if ref, symbolic := strings.CutPrefix(string(data), "ref:"); symbolic {
		return strings.HasPrefix(strings.TrimLeft(ref, " \t\r\n"), "refs/")
	}
	return validPinnedCommit(strings.TrimRight(string(data), "\r\n"))
}

// CloseGitViews forgets this process's views, so the next command resolves the root again.
func CloseGitViews() {
	gitViewsMu.Lock()
	defer gitViewsMu.Unlock()
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
		// A per-process temporary root used to stand in here, and every test binary that never
		// removed it left one behind in the temp directory, about 130 a day.
		return "", fmt.Errorf("a test binary must set %s (internal/testutil/teststate does)", GitViewRootEnv)
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

// openGitView brings workTree's view up to date and returns it with the environment that runs git
// under it, built from base. The environment is built under the view's lock: once the lock is
// free, a command reaching the same git dir by another path moves the view to that path.
func openGitView(ctx context.Context, workTree string, base []string) (*gitView, []string, error) {
	if err := validateIsolatedGitAccess(workTree); err != nil {
		return nil, nil, err
	}
	gitDir, commonDir, err := gitDirsOf(workTree)
	if err != nil {
		return nil, nil, err
	}
	view, err := cachedGitView(gitDir, commonDir)
	if err != nil {
		return nil, nil, err
	}
	view.mu.Lock()
	defer view.mu.Unlock()
	// One git dir can serve successive worktree paths (git reuses a removed worktree's slot under
	// .git/worktrees/<name>), so the view follows the path it is asked for — and the state an
	// operation left behind in the previous worktree's life goes with that life. A view new to this
	// process keeps what it finds: that is how an interrupted rebase reaches the next coop.
	fresh := view.workTree == ""
	if view.workTree != workTree {
		if !fresh {
			if err := view.clearOperationState(); err != nil {
				return nil, nil, err
			}
		}
		view.workTree = workTree
	}
	// Another coop's daily sweep may have removed the folder, or be removing it, while its
	// repository was away (an unplugged disk, a re-clone); laying it out again is safe to repeat,
	// so a process never fails until a restart.
	err = os.MkdirAll(view.dir, 0o700)
	if err == nil {
		err = view.populate(ctx)
	}
	if err != nil {
		if fresh {
			// A view this process never laid out is not kept, so the next command still finds it
			// new and keeps the state an earlier coop left in it.
			view.workTree = ""
			forgetGitView(view)
		}
		return nil, nil, fmt.Errorf("trusted git view of %s: %w", workTree, err)
	}
	return view, view.envFrom(base), nil
}

// forgetGitView drops view from this process's cache, unless another view already took its place.
func forgetGitView(view *gitView) {
	gitViewsMu.Lock()
	defer gitViewsMu.Unlock()
	if gitViews[view.gitDir] == view {
		delete(gitViews, view.gitDir)
	}
}

// cachedGitView returns this process's view of gitDir, adding it on first use; the caller lays it
// out under the view's own lock.
func cachedGitView(gitDir, commonDir string) (*gitView, error) {
	gitViewsMu.Lock()
	defer gitViewsMu.Unlock()
	if view := gitViews[gitDir]; view != nil {
		return view, nil
	}
	root, err := gitViewRoot()
	if err != nil {
		return nil, fmt.Errorf("git view root: %w", err)
	}
	sum := sha256.Sum256([]byte(gitDir))
	view := &gitView{gitDir: gitDir, commonDir: commonDir, dir: filepath.Join(root, hex.EncodeToString(sum[:8]))}
	gitViews[gitDir] = view
	return view, nil
}

// RepositoryRoot is the folder holding the nearest `.git` at or above dir, by the rules gitDirsOf
// follows, so the folder Coop takes for a project and the git dir its views read never disagree.
// ok is false when no folder up to the filesystem root holds one.
func RepositoryRoot(dir string) (root string, ok bool, err error) {
	root, _, err = nearestGitDir(dir)
	if errors.Is(err, errNoRepository) {
		return "", false, nil
	}
	return root, err == nil, err
}

// nearestGitDir finds the nearest `.git` at or above dir the way git does: a directory, or a
// `gitdir: <path>` file for a linked worktree or a submodule. A `.git` that cannot be stat'd, a
// dangling link say, or that is neither a folder nor a regular file is passed over as git passes
// it. It returns the folder holding that `.git` and the git dir it names, or errNoRepository.
func nearestGitDir(dir string) (root, gitDir string, err error) {
	for ; ; dir = filepath.Dir(dir) {
		dotGit := filepath.Join(dir, ".git")
		info, statErr := os.Stat(dotGit)
		switch {
		case statErr == nil && info.IsDir():
			return dir, dotGit, nil
		case statErr == nil && info.Mode().IsRegular():
			data, err := readGitFile(dotGit)
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
			return dir, filepath.Clean(target), nil
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "", "", errNoRepository
		}
	}
}

// gitDirsOf resolves the worktree's git dir and common dir the way git does, without running
// git: the nearest `.git` above workTree — a directory, or a `gitdir: <path>` file for a linked
// worktree — and that dir's `commondir` pointer when it has one. Symlinks are resolved first so
// one repository has one view. A linked worktree's git dir must name this worktree back, and a
// repository the walk climbs to may not be the home folder or a folder above it.
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
	root, gitDir, err := nearestGitDir(abs)
	if errors.Is(err, errNoRepository) {
		return "", "", fmt.Errorf("%s: %w", workTree, errNoRepository)
	}
	if err != nil {
		return "", "", err
	}
	// A box that deletes the folder's own .git moves the walk up to whatever repository holds the
	// folder, a home folder kept in Git among them, and host git would then read and write that
	// repository; box.FindRepo never climbs to one either. A folder that holds the .git itself
	// still names its repository, as a COOP_REPO at the home folder does. The refusal is not
	// errNoRepository, on which GitCommand and GitRefCommand run plain git, and plain git climbs
	// there by itself.
	if root != abs {
		if home, above := shadowpath.HomeOrAbove(root); above {
			return "", "", fmt.Errorf("%s has no .git of its own, and the nearest Git repository above it is %s, "+
				"at your home folder %s or above it, where Coop does not run git for a folder inside it; "+
				"restore the folder's .git, or run git init in it", workTree, root, home)
		}
	}
	if real, err := filepath.EvalSymlinks(gitDir); err == nil {
		gitDir = real
	}
	commonDir = gitDir
	data, err := readGitFile(filepath.Join(gitDir, "commondir"))
	if errors.Is(err, os.ErrNotExist) {
		return gitDir, commonDir, nil
	}
	if err != nil {
		return "", "", err
	}
	target := strings.TrimSpace(string(data))
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	if real, err := filepath.EvalSymlinks(target); err == nil {
		target = real
	}
	commonDir = filepath.Clean(target)
	// A git dir with its own common dir is a linked worktree's. Unless it is this folder's own
	// .git, the worktree reached it through a `gitdir:` file or a link, which a box can aim at
	// any repository's worktree.
	if commonDir != gitDir && gitDir != filepath.Join(root, ".git") {
		if err := checkLinkedWorktree(root, gitDir, commonDir, readGitFile); err != nil {
			return "", "", err
		}
	}
	return gitDir, commonDir, nil
}

// checkLinkedWorktree refuses a linked worktree's git dir that is not root's; root is the
// worktree folder with its links resolved. Git keeps that dir at <common dir>/worktrees/<name>
// with a `gitdir` file naming the worktree's .git. A box can write a pointer to any repository's
// worktree, but not that repository's backlink, so the backlink is what proves the git dir is
// this worktree's. Views and `coop approve` both check with it; read is how each opens the
// backlink, reporting a missing one as os.ErrNotExist.
//
// `git worktree repair`, run in this worktree, makes the backlink name it. That restores a lost
// link, but against a git dir another worktree still uses it would hand that git dir to this
// folder, so the advice appears only when the backlink names nothing that exists.
func checkLinkedWorktree(root, gitDir, commonDir string, read func(string) ([]byte, error)) error {
	dotGit := filepath.Join(root, ".git")
	if filepath.Base(filepath.Dir(gitDir)) != "worktrees" || !sameFile(filepath.Dir(filepath.Dir(gitDir)), commonDir) {
		return fmt.Errorf("%s points at %s, which is not a worktree folder of the repository at %s", dotGit, gitDir, commonDir)
	}
	data, err := read(filepath.Join(gitDir, "gitdir"))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s points at %s, whose gitdir file is missing; run `git worktree repair` in this worktree to restore it", dotGit, gitDir)
	}
	if err != nil {
		return fmt.Errorf("%s points at %s, whose worktree cannot be checked: %w", dotGit, gitDir, err)
	}
	backlink := strings.TrimSpace(string(data))
	if backlink == "" {
		return fmt.Errorf("%s points at %s, whose gitdir file is empty; run `git worktree repair` in this worktree to restore it", dotGit, gitDir)
	}
	if !filepath.IsAbs(backlink) {
		backlink = filepath.Join(gitDir, backlink)
	}
	// Folders are compared, never the .git files: a box can replace its .git with a symbolic or
	// hard link to another worktree's, which is that very file. root holds no link, and a folder
	// cannot be hard-linked.
	if filepath.Base(backlink) == ".git" && sameFile(filepath.Dir(backlink), root) {
		return nil
	}
	if _, err := os.Lstat(backlink); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s points at %s, which git keeps for the worktree at %s, and that worktree is gone; "+
			"if this worktree was moved from there, run `git worktree repair` in it", dotGit, gitDir, filepath.Dir(backlink))
	}
	return fmt.Errorf("%s points at %s, which belongs to the worktree at %s", dotGit, gitDir, filepath.Dir(backlink))
}

// sameFile reports whether a and b name one existing file or folder. It compares identities, not
// spellings: a case-insensitive disk or a linked folder (/var is /private/var on macOS) spells one
// path several ways.
func sameFile(a, b string) bool {
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
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
	realHead, realInfo, err := readGitMetadata(realPath, gitPointerLimit)
	if err != nil {
		return err
	}
	viewHead, viewInfo, err := readGitMetadata(viewPath, gitPointerLimit)
	if errors.Is(err, os.ErrNotExist) {
		return writeViewFile(viewPath, realHead)
	}
	if err != nil {
		return err
	}
	if string(viewHead) == string(realHead) {
		return nil
	}
	if viewInfo.ModTime().After(realInfo.ModTime()) {
		return writeViewFile(realPath, viewHead)
	}
	return writeViewFile(viewPath, realHead)
}

// operationInFlight reports whether git left the state of an unfinished rebase, merge, cherry-pick
// or revert in the view; that operation owns HEAD until it finishes or is aborted.
func (v *gitView) operationInFlight() bool { return viewHoldsOperation(v.dir) }

// viewOperationState is what git leaves in a view while a rebase, merge, cherry-pick or revert is
// unfinished. Only the view holds it, so a view holding any of it is never swept.
var viewOperationState = []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"}

func viewHoldsOperation(dir string) bool {
	for _, name := range viewOperationState {
		if fileExists(filepath.Join(dir, name)) {
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
		target, readErr := os.Readlink(path)
		if readErr != nil {
			// Another refresher may already have removed this now-unused optional link.
			if !required && !fileExists(source) {
				if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
					return nil
				}
			}
			return readErr
		}
		if target != source {
			return fmt.Errorf("view entry %s points to %s instead of %s", path, target, source)
		}
		if !fileExists(source) {
			if required {
				return fmt.Errorf("%s is missing", source)
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
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
	if err := os.Symlink(source, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process may have populated this persistent view after our lstat.
			if target, readErr := os.Readlink(path); readErr == nil && target == source {
				return nil
			}
		}
		return err
	}
	return nil
}

func writeViewFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
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
