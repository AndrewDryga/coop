package cli

import (
	"errors"
	"fmt"
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

// rangeCount is the number of commits in base..HEAD.
func rangeCount(repo, base string) int {
	n := 0
	fmt.Sscanf(gitOut(repo, "rev-list", "--count", base+"..HEAD"), "%d", &n)
	return n
}

// signUnpushed re-signs base..HEAD with the host's signing config (trustedSignArgs, read from the
// GLOBAL git config so a poisoned repo can't plant a gpg.program). -f forces the rewrite so an
// already-signed or already-based range is still re-signed. Returns how many commits were re-signed.
func (a *app) signUnpushed(repo, base string) (int, error) {
	n := rangeCount(repo, base)
	if n == 0 {
		return 0, nil
	}
	args := append(trustedSignArgs(), "rebase", "-f", "--gpg-sign", base)
	if err := gitInteractive(repo, args...); err != nil {
		_ = gitRun(repo, "rebase", "--abort")
		return 0, fmt.Errorf("re-signing %s..HEAD failed (a signing key/agent issue?): %w", base, err)
	}
	return n, nil
}

// shouldSignOnExit decides whether an interactive/run box's exit should trigger a host-side re-sign
// of the commits it made: only when you sign by default, it's NOT a fork (fork-merge re-signs at
// land), and the tree is clean — an interactive session may exit mid-edit, and a re-sign must never
// touch an in-progress tree (the caller hints `coop sign` instead). Pure.
func shouldSignOnExit(isFork, wantsSigning, dirty bool) bool {
	return wantsSigning && !isFork && !dirty
}

// signOnBoxExit re-signs the commits a box made this session (preHead..HEAD) with your host key,
// after the box has exited. Scoped to the SESSION range — not @{upstream}..HEAD — so it signs
// exactly what this box produced and needs no upstream; a session that made no commits is a no-op.
// Best-effort: it never blocks teardown. Forks/dirty-tree are skipped (shouldSignOnExit), with a
// `coop sign` hint when the session committed but the tree is now dirty.
func (a *app) signOnBoxExit(repo, preHead string, isFork bool) {
	if repo == "" || preHead == "" || preHead == gitOut(repo, "rev-parse", "HEAD") {
		return // no repo, or the session made no commit → nothing to sign
	}
	if !shouldSignOnExit(isFork, wantsSigning(), gitDirty(repo)) {
		if wantsSigning() && !isFork {
			ui.Info("this session's commits are unsigned and your tree is dirty — run `coop sign` after you commit or stash")
		}
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
	if gitDirty(repo) {
		return 1, errors.New("your working tree has uncommitted changes — commit or stash before signing (the re-sign rebases, which a dirty tree blocks)")
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
