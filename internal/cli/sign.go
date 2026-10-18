package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Box commits are made unsigned (no key ever enters a box), so a remote that requires signed
// commits — a protected main, like many projects — rejects work a loop or an interactive box
// produced. `coop sign` signs the UNPUSHED range with your host key on the host, where your
// signing config lives; the loop does the same per cycle. It never pushes. Every signing pass starts
// at the first unsigned commit and never rewrites a commit the branch's upstream already has (git's
// own line for what is safe to rewrite), so a signed or pushed commit keeps the ID others recorded.

// signBase resolves the base of the unpushed range: @{upstream} when the branch tracks one, else the
// explicit from ref (required with no upstream). A merge in that range is signUnpushed's to judge:
// only the part a pass rewrites must be linear. The git reads make it env-dependent, but the
// decisions are testable.
func signBase(repo, from string) (string, error) {
	base := from
	if base == "" {
		if u := forkspace.GitOut(repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); u != "" {
			base = u
		} else {
			return "", ui.CommandFailed("Could not determine which commits to sign", "This branch has no upstream.",
				[2]string{"", "Use --from with the last commit you pushed."},
				[2]string{"Help:", "coop help sign"})
		}
	}
	if forkspace.GitOut(repo, "rev-parse", "--verify", "--quiet", base+"^{commit}") == "" {
		return "", fmt.Errorf("no such commit: %s", base)
	}
	return base, nil
}

// rangeCount is the number of commits in base..head.
func rangeCount(repo, base, head string) int {
	n := 0
	fmt.Sscanf(forkspace.GitOut(repo, "rev-list", "--count", base+".."+head), "%d", &n)
	return n
}

// rangeTrees returns each commit tree from oldest to newest. Signing may change commit objects,
// parent ids, and committer timestamps, but it must never change the committed snapshots.
func rangeTrees(repo, base, head string) string {
	return forkspace.GitOut(repo, "log", "--reverse", "--format=%T", base+".."+head)
}

// signRangeBase bounds the rewrite to work descended from base. A loop iteration may amend its
// starting commit, making the old and new commits siblings; in that case their merge base is the
// last commit that must remain untouched. Unrelated histories are refused rather than guessed.
func signRangeBase(repo, base, head string) (string, error) {
	if forkspace.GitRun(repo, "merge-base", "--is-ancestor", base, head) == nil {
		return base, nil
	}
	common := strings.Fields(forkspace.GitOut(repo, "merge-base", "--all", base, head))
	if len(common) == 0 {
		return "", fmt.Errorf("cannot safely re-sign work from %s: it has no common base with %s", base, head)
	}
	if len(common) != 1 {
		return "", fmt.Errorf("cannot safely re-sign work from %s: it has multiple common bases with %s", base, head)
	}
	return common[0], nil
}

// signingStart returns where signing base..head begins: the last of the signed commits the range
// starts with, in topological order — base when its first commit is unsigned, head when every
// commit is signed. A signed merge among those leading commits stays below the start, so only the
// rewritten part has to be linear. Those signed commits keep their IDs: another agent's commit in
// the same checkout, an earlier cycle's, one already pushed. Signing never reaches history the
// branch's upstream has either: when the first unsigned commit is already pushed, it stops instead
// of rewriting it.
func signingStart(repo, branchRef, base, head string) (string, error) {
	out, err := gitOutputBytes(repo, "rev-list", "--reverse", "--topo-order", "--header", base+".."+head)
	if err != nil {
		return "", fmt.Errorf("read the commits in %s..%s: %w", base, head, err)
	}
	for _, record := range strings.Split(string(out), "\x00") {
		commit, raw, _ := strings.Cut(record, "\n")
		if commit == "" {
			continue // the empty field after the last record's NUL
		}
		if !commitSigned(raw) {
			if err := refusePushedRewrite(repo, branchRef, commit); err != nil {
				return "", err
			}
			return base, nil
		}
		base = commit
	}
	return base, nil
}

// refusePushedRewrite refuses to rewrite commit when the branch's upstream already has it. With no
// upstream, or one whose remote-tracking branch is gone, nothing local records what was pushed.
func refusePushedRewrite(repo, branchRef, commit string) error {
	branch := strings.TrimPrefix(branchRef, "refs/heads/")
	upstream := forkspace.GitOut(repo, "rev-parse", "--verify", "--quiet", branch+"@{upstream}^{commit}")
	if upstream == "" {
		return nil
	}
	var exitErr *exec.ExitError
	switch err := forkspace.GitRun(repo, "merge-base", "--is-ancestor", commit, upstream); {
	case err == nil:
		return pushedCommitRefusal(forkspace.GitOut(repo, "rev-parse", "--short", commit),
			forkspace.GitOut(repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}"))
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
		return nil // git's own "not an ancestor": the commit is not pushed
	default:
		return fmt.Errorf("check whether %s is already pushed: %w", commit, err)
	}
}

// pushedCommitRefusal names the unsigned commit the upstream already has. `coop sign` with no
// --from starts at the upstream, so it signs what is left without touching pushed history.
func pushedCommitRefusal(commit, upstream string) error {
	return ui.CommandFailed("Could not sign without rewriting pushed history",
		fmt.Sprintf("Commit %s is not signed and is already pushed to %s.", commit, upstream),
		[2]string{"", "To sign only the commits you have not pushed, run coop sign."},
		[2]string{"Help:", "coop help sign"})
}

// commitSigned reports whether a raw commit object carries a signature: a gpgsig header, or
// gpgsig-sha256 in a SHA-256 repository. Only the header counts, since a message line can say
// anything. The signature is not verified: git's %G? reads a signed SSH commit as unsigned when no
// allowedSignersFile is set, and a commit anyone signed is not Coop's to re-sign.
func commitSigned(raw string) bool {
	header, _, _ := strings.Cut(raw, "\n\n")
	for _, line := range strings.Split(header, "\n") {
		if strings.HasPrefix(line, "gpgsig ") || strings.HasPrefix(line, "gpgsig-sha256 ") {
			return true
		}
	}
	return false
}

// signUnpushed signs the unsigned commits of base..HEAD in a clean detached linked worktree,
// starting at the first unsigned one (signingStart), and returns how many commits it rewrote. The
// active checkout's index and files are never touched, so unrelated staged, unstaged, untracked, or
// secret-decoy dirt cannot block signing. The candidate must preserve the exact HEAD tree, and the
// checked-out branch advances only through an old-SHA compare-and-swap after the temporary
// worktree is removed.
func (a *app) signUnpushed(repo, base string) (int, error) {
	branchRef := forkspace.GitOut(repo, "symbolic-ref", "--quiet", "HEAD")
	if !strings.HasPrefix(branchRef, "refs/heads/") {
		return 0, ui.CommandFailed("Could not sign these commits", "No branch is checked out.",
			[2]string{"", "Check out the branch you want to sign."})
	}
	oldHead := forkspace.GitOut(repo, "rev-parse", "--verify", "HEAD^{commit}")
	resolvedBase := forkspace.GitOut(repo, "rev-parse", "--verify", base+"^{commit}")
	if oldHead == "" || resolvedBase == "" {
		return 0, fmt.Errorf("cannot resolve signing range %s..HEAD", base)
	}
	base, err := signRangeBase(repo, resolvedBase, oldHead)
	if err != nil {
		return 0, err
	}
	if base, err = signingStart(repo, branchRef, base, oldHead); err != nil {
		return 0, err
	}
	// Re-signing rebases what it rewrites, which would linearize a merge. A signed merge below the
	// first unsigned commit is not rewritten, so only this part has to be linear.
	if merges := forkspace.GitOut(repo, "rev-list", "--merges", base+".."+oldHead); merges != "" {
		return 0, ui.CommandFailed("Could not sign these commits", "The selected range contains a merge commit.",
			[2]string{"", "Choose a linear range with --from."},
			[2]string{"Help:", "coop help sign"})
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
			_ = forkspace.GitRefCommand(context.Background(), repo, "worktree", "remove", "--force", worktree).Run()
		}
		_ = os.RemoveAll(tempRoot)
	}()

	// The worktree list lives in the real git dir, so it is added there — without a checkout: the
	// files are then populated under the trusted view of the new worktree, so the repository's
	// config can name no filter or driver for that checkout to run.
	if err := forkspace.GitRefCommand(context.Background(), repo, "worktree", "add", "--no-checkout", "--detach", "--quiet", worktree, oldHead).Run(); err != nil {
		return 0, fmt.Errorf("create clean signing worktree: %w", err)
	}
	added = true
	if _, err := forkspace.GitOutErr(worktree, "reset", "--hard", "--quiet", oldHead); err != nil {
		return 0, fmt.Errorf("populate clean signing worktree: %w", err)
	}
	// -f stays: base is the exact parent of the first commit to sign, and without it git reports the
	// branch up to date and signs nothing.
	args := append(append([]string{}, forkspace.TrustedSignArgs()...), "rebase", "-f", "--gpg-sign", base)
	if err := gitSign(worktree, args...); err != nil {
		_ = forkspace.GitRun(worktree, "rebase", "--abort") // leave no rebase state behind for the next signing worktree in this slot
		return 0, signingFailure{fmt.Errorf("could not re-sign %s..HEAD: %w", base, err)}
	}
	newHead := forkspace.GitOut(worktree, "rev-parse", "--verify", "HEAD^{commit}")
	newTrees := rangeTrees(worktree, base, newHead)
	if newHead == "" || oldTrees == "" || newTrees == "" || oldTrees != newTrees {
		return 0, errors.New("re-signing changed one or more committed trees; refusing to update the branch")
	}
	oldCommits := strings.Fields(forkspace.GitOut(repo, "log", "--reverse", "--format=%H", base+".."+oldHead))
	if len(oldCommits) != n {
		return 0, errors.New("could not list the commits to sign; refusing to update the branch")
	}
	if err := forkspace.GitRefCommand(context.Background(), repo, "worktree", "remove", "--force", worktree).Run(); err != nil {
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
	release, lockErr := tasks.LockRefAuthority(a.cfg, repo)
	if lockErr != nil {
		return 0, fmt.Errorf("lock the branch before applying signed commits in %s: %w", repo, lockErr)
	}
	defer release()
	if a.beforeSignRefUpdate != nil {
		a.beforeSignRefUpdate(repo, branchRef, oldHead, newHead)
	}
	if currentRef := forkspace.GitOut(repo, "symbolic-ref", "--quiet", "HEAD"); currentRef != branchRef {
		return 0, ui.CommandFailed("Could not apply the signed commits", "The branch changed while Coop was signing.",
			[2]string{"", "Review the current branch before running coop sign again."})
	}
	// The rebase took seconds, or minutes at a pinentry prompt, and a push in that window must not
	// be rewritten either: read the upstream again under the lock, just before the swap.
	if err := refusePushedRewrite(repo, branchRef, oldCommits[0]); err != nil {
		return 0, err
	}
	if err := forkspace.GitRefCommand(context.Background(), repo, "update-ref", "-m", "coop: re-sign commits", branchRef, newHead, oldHead).Run(); err != nil {
		return 0, ui.CommandFailed("Could not apply the signed commits", "The branch changed while Coop was signing.\n"+err.Error(),
			[2]string{"", "Review the current branch before running coop sign again."})
	}
	// A task the final review reopened records by ID the commits its rework must keep, and this pass
	// may have replaced them with signed copies.
	if err := tasks.CarryReopenedReviewsAfterSigning(repo, oldHead); err != nil {
		ui.Warn("signed the commits, but could not move a reopened task's review authority to them: %v", err)
	}
	return n, nil
}

// signingFailure is the signing rebase itself failing: a missing or locked key, an unreachable
// agent, a declined passphrase prompt. It is the one failure the Git signing setup explains, so it
// is the one that gets advice about that setup. Its message is the wrapped error's, unchanged.
type signingFailure struct{ err error }

func (f signingFailure) Error() string { return f.err.Error() }
func (f signingFailure) Unwrap() error { return f.err }

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
	if repo == "" || preHead == "" || preHead == forkspace.GitOut(repo, "rev-parse", "HEAD") {
		return // no repo, or the session made no commit → nothing to sign
	}
	if !shouldSignOnExit(isFork, forkspace.WantsSigning()) {
		return
	}
	if n, err := a.signUnpushed(repo, preHead); err != nil {
		// A warning, not a failure: the session's work is committed either way. The cause is the
		// host signing operation's own — a branch that moved, pushed history, a key problem — and
		// it is carried verbatim. Only the signing rebase failing is about the Git signing setup;
		// every other cause names its own next step or has none to name.
		action := ""
		if errors.As(err, new(signingFailure)) {
			action = "Run coop sign after fixing your Git signing setup."
		}
		ui.Warning("Could not sign this session's commits", err.Error(), action)
	} else if n > 0 {
		ui.Note("signed %s with your host key", ui.Count(n, "commit"))
	}
}

// headUnsigned reports whether HEAD carries NO signature header (see commitSigned). One bounded
// git call.
func headUnsigned(repo string) bool {
	obj := forkspace.GitOut(repo, "cat-file", "commit", "HEAD")
	return obj != "" && !commitSigned(obj)
}

// cmdSign re-signs the current branch's unpushed commits with your host signing key — for a remote
// (a protected main) that requires signatures, since box commits are unsigned. Never pushes, never
// rewrites pushed history.
func (a *app) cmdSign(args []string) (int, error) {
	from := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--from":
			if i+1 >= len(args) {
				return 2, ui.MissingOptionValue("--from", "coop sign", "coop sign --from origin/main")
			}
			from, i = args[i+1], i+1
		case "-h", "--help":
			return helpForPath([]string{"sign"}, a.cfg, false)
		default:
			return 2, ui.UnexpectedArgument(args[i], "coop sign", "coop sign [--from <ref>]")
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
	// An explicit --from is TRUSTED on a branch with no upstream: those commits may well be pushed
	// already, so the result counts commits and says which reference they follow. Only the default
	// range — git's own @{upstream}..HEAD — has the evidence to call them unpushed.
	if n == 0 {
		// Nothing was rewritten: the range is empty, or every commit in it is already signed.
		signed := rangeCount(repo, base, "HEAD") > 0
		switch {
		case from != "" && signed:
			ui.Note("Every commit after %s is already signed.", from)
		case from != "":
			ui.Note("No commits after %s to sign.", from)
		case signed:
			ui.Note("Every unpushed commit is already signed.")
		default:
			ui.Note("No unpushed commits to sign.")
		}
		return 0, nil
	}
	if from != "" {
		ui.OK("Signed %s with your host key", ui.Count(n, "commit"))
		return 0, nil
	}
	ui.OK("Signed %s with your host key", ui.Count(n, "unpushed commit"))
	return 0, nil
}
