package cli

import (
	"errors"
	"fmt"

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
