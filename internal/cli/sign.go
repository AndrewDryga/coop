package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Box commits are made unsigned (no key ever enters a box), so a remote that requires signed
// commits — a protected main, like many projects — rejects work a loop or an interactive box
// produced. `coop sign` re-signs the UNPUSHED range with your host key on the host, where your
// signing config lives; the loop does the same per cycle. It never pushes and never rewrites pushed
// history — the range is @{upstream}..HEAD (git's own rule for what's safe to rewrite).

// signBase resolves the base of the unpushed range: @{upstream} when the branch tracks one, else the
// explicit from ref (required with no upstream). It REFUSES a range that contains a merge commit (a
// rebase would linearize it). The git reads make it env-dependent, but the decisions are testable.
func signBase(repo, from string) (string, error) {
	base := from
	if base == "" {
		if u := gitOut(repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); u != "" {
			base = u
		} else {
			return "", errors.New("this branch has no upstream, so its unpushed range is unknown — pass --from <ref> (e.g. the commit you last pushed)")
		}
	}
	if gitOut(repo, "rev-parse", "--verify", "--quiet", base+"^{commit}") == "" {
		return "", fmt.Errorf("no such commit: %s", base)
	}
	if merges := gitOut(repo, "rev-list", "--merges", base+"..HEAD"); merges != "" {
		return "", fmt.Errorf("the range %s..HEAD contains a merge commit — re-signing would linearize history; push the merge first, or sign a linear range with --from", base)
	}
	return base, nil
}

// rangeCount is the number of commits in base..head.
func rangeCount(repo, base, head string) int {
	n := 0
	fmt.Sscanf(gitOut(repo, "rev-list", "--count", base+".."+head), "%d", &n)
	return n
}

// rangeTrees returns each commit tree from oldest to newest. Signing may change commit objects,
// parent ids, and committer timestamps, but it must never change the committed snapshots.
func rangeTrees(repo, base, head string) string {
	return gitOut(repo, "log", "--reverse", "--format=%T", base+".."+head)
}

// signRangeBase bounds the rewrite to work descended from base. A loop iteration may amend its
// starting commit, making the old and new commits siblings; in that case their merge base is the
// last commit that must remain untouched. Unrelated histories are refused rather than guessed.
func signRangeBase(repo, base, head string) (string, error) {
	if gitRun(repo, "merge-base", "--is-ancestor", base, head) == nil {
		return base, nil
	}
	common := strings.Fields(gitOut(repo, "merge-base", "--all", base, head))
	if len(common) == 0 {
		return "", fmt.Errorf("cannot safely re-sign work from %s: it has no common base with %s", base, head)
	}
	if len(common) != 1 {
		return "", fmt.Errorf("cannot safely re-sign work from %s: it has multiple common bases with %s", base, head)
	}
	return common[0], nil
}

// signUnpushed re-signs base..HEAD in a clean detached linked worktree. The active checkout's
// index and files are never touched, so unrelated staged, unstaged, untracked, or secret-decoy dirt
// cannot block signing. The candidate must preserve the exact HEAD tree, and the checked-out branch
// advances only through an old-SHA compare-and-swap after the temporary worktree is removed.
func (a *app) signUnpushed(repo, base string) (int, error) {
	branchRef := gitOut(repo, "symbolic-ref", "--quiet", "HEAD")
	if !strings.HasPrefix(branchRef, "refs/heads/") {
		return 0, errors.New("cannot re-sign a detached HEAD; check out the branch first")
	}
	oldHead := gitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	resolvedBase := gitOut(repo, "rev-parse", "--verify", base+"^{commit}")
	if oldHead == "" || resolvedBase == "" {
		return 0, fmt.Errorf("cannot resolve signing range %s..HEAD", base)
	}
	base, err := signRangeBase(repo, resolvedBase, oldHead)
	if err != nil {
		return 0, err
	}
	if merges := gitOut(repo, "rev-list", "--merges", base+".."+oldHead); merges != "" {
		return 0, fmt.Errorf("the range %s..HEAD contains a merge commit — re-signing would linearize history", base)
	}
	n := rangeCount(repo, base, oldHead)
	if n == 0 {
		return 0, nil
	}
	oldTrees := rangeTrees(repo, base, oldHead)

	tempRoot, err := os.MkdirTemp("", "coop-sign-")
	if err != nil {
		return 0, fmt.Errorf("create signing worktree: %w", err)
	}
	worktree := filepath.Join(tempRoot, "worktree")
	added := false
	defer func() {
		if added {
			_ = gitRun(repo, "worktree", "remove", "--force", worktree)
		}
		_ = os.RemoveAll(tempRoot)
	}()

	neutral := forkDriverNeutralizer(repo)
	addArgs := append(append([]string{}, neutral...), "worktree", "add", "--detach", "--quiet", worktree, oldHead)
	if err := gitRun(repo, addArgs...); err != nil {
		return 0, fmt.Errorf("create clean signing worktree: %w", err)
	}
	added = true
	args := append(append(append([]string{}, trustedSignArgs()...), neutral...), "rebase", "-f", "--gpg-sign", base)
	if err := gitSign(worktree, args...); err != nil {
		return 0, fmt.Errorf("re-signing %s..HEAD failed (a signing key/agent issue?): %w", base, err)
	}
	newHead := gitOut(worktree, "rev-parse", "--verify", "HEAD^{commit}")
	newTrees := rangeTrees(worktree, base, newHead)
	if newHead == "" || oldTrees == "" || newTrees == "" || oldTrees != newTrees {
		return 0, errors.New("re-signing changed one or more committed trees; refusing to update the branch")
	}
	if err := gitRun(repo, "worktree", "remove", "--force", worktree); err != nil {
		return 0, fmt.Errorf("remove signing worktree before updating the branch: %w", err)
	}
	added = false
	if err := os.RemoveAll(tempRoot); err != nil {
		return 0, fmt.Errorf("remove signing scratch before updating the branch: %w", err)
	}
	// This is coop's own host-side ref mutation: the same per-worktree ref-authority lock that
	// guards the work loop's validate→consume window guards this compare-and-swap too, so a signing
	// rewrite can never land in the middle of another controller's window (and vice versa) — coop
	// never trips its own compare-and-swap. Acquired only for this short tail, never the rebase above.
	release, lockErr := lockRefAuthority(a.cfg, repo)
	if lockErr != nil {
		return 0, fmt.Errorf("acquire ref authority for %s: %w", repo, lockErr)
	}
	defer release()
	if a.beforeSignRefUpdate != nil {
		a.beforeSignRefUpdate(repo, branchRef, oldHead, newHead)
	}
	if currentRef := gitOut(repo, "symbolic-ref", "--quiet", "HEAD"); currentRef != branchRef {
		return 0, fmt.Errorf("checked-out branch changed during re-signing (%s to %s); refusing to update it", branchRef, currentRef)
	}
	if err := gitRun(repo, "update-ref", "-m", "coop: re-sign commits", branchRef, newHead, oldHead); err != nil {
		return 0, fmt.Errorf("branch moved during re-signing; signed candidate was not applied: %w", err)
	}
	return n, nil
}

// shouldSignOnExit decides whether an interactive/run box's exit should trigger a host-side re-sign
// of the commits it made: only when you sign by default and it's NOT a fork (fork-merge re-signs at
// land). Dirty state is safe because signUnpushed never operates in the active checkout. Pure.
func shouldSignOnExit(isFork, wantsSigning bool) bool {
	return wantsSigning && !isFork
}

// signOnBoxExit re-signs the commits a box made this session (preHead..HEAD) with your host key,
// after the box has exited. Scoped to the SESSION range — not @{upstream}..HEAD — so it signs
// exactly what this box produced and needs no upstream; a session that made no commits is a no-op.
// Best-effort: it never blocks teardown. Forks are skipped because merge-time signing owns them.
func (a *app) signOnBoxExit(repo, preHead string, isFork bool) {
	if repo == "" || preHead == "" || preHead == gitOut(repo, "rev-parse", "HEAD") {
		return // no repo, or the session made no commit → nothing to sign
	}
	if !shouldSignOnExit(isFork, wantsSigning()) {
		return
	}
	if n, err := a.signUnpushed(repo, preHead); err != nil {
		ui.Warn("could not sign this session's commits: %v — run `coop sign`", err)
	} else if n > 0 {
		ui.Info("signed %s with your host key", ui.Count(n, "commit"))
	}
}

// headUnsigned reports whether HEAD carries NO signature — its raw object has no gpgsig header. This
// is the robust check: git's %G?/%GK report N/empty for an SSH commit that IS signed but can't be
// verified (no allowedSignersFile), so they'd flag signed commits as unsigned. One bounded git call.
func headUnsigned(repo string) bool {
	obj := gitOut(repo, "cat-file", "commit", "HEAD")
	return obj != "" && !strings.Contains(obj, "\ngpgsig ")
}

// promptSignWarn reports whether `coop prompt` should show an unsigned nudge: you sign by default
// but HEAD is unsigned (a box commit not yet signed). Pure — the caller supplies the two facts.
func promptSignWarn(signs, headUnsigned bool) bool { return signs && headUnsigned }

// cmdSign re-signs the current branch's unpushed commits with your host signing key — for a remote
// (a protected main) that requires signatures, since box commits are unsigned. Never pushes, never
// rewrites pushed history.
func (a *app) cmdSign(args []string) (int, error) {
	from := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			if i+1 >= len(args) {
				return 2, errors.New("--from needs a <ref>")
			}
			from, i = args[i+1], i+1
		case "-h", "--help":
			return helpForCommand("sign"), nil
		default:
			return 2, fmt.Errorf("coop sign: unexpected argument %q", args[i])
		}
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	base, err := signBase(repo, from)
	if err != nil {
		return 1, err
	}
	n, err := a.signUnpushed(repo, base)
	if err != nil {
		return -1, err
	}
	if n == 0 {
		ui.Info("nothing to sign — no unpushed commits")
		return 0, nil
	}
	ui.OK("signed %s with your host key", ui.Count(n, "unpushed commit"))
	return 0, nil
}
