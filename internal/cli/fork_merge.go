package cli

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// secretRe flags filenames that look like credentials or keys — agents are good at
// passing the gate while quietly widening the blast radius, so a merge surfaces them.
var secretRe = regexp.MustCompile(`(?i)(^|/)(\.env(\.|$)|[^/]*\.(pem|key|p12|pfx)$|id_rsa|id_ed25519|credentials(\.|$)|[^/]*secret[^/]*)`)

// policyScan returns human-readable concerns about a fork's added/changed files:
// secret-looking files and large blobs. Empty means nothing flagged.
func policyScan(repo, ref string) []string {
	out := gitOut(repo, "diff", "--name-status", "HEAD..."+ref)
	if out == "" {
		return nil
	}
	var warns []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "D" { // deletions are never a concern
			continue
		}
		path := f[len(f)-1]
		if secretRe.MatchString(path) {
			warns = append(warns, "secret-like file: "+path)
		}
		if size := gitBlobSize(repo, ref, path); size > 5<<20 {
			warns = append(warns, fmt.Sprintf("large file (%dMB): %s", size>>20, path))
		}
	}
	return warns
}

func gitBlobSize(repo, ref, path string) int64 {
	n, _ := strconv.ParseInt(gitOut(repo, "cat-file", "-s", ref+":"+path), 10, 64)
	return n
}

// mergeGate resolves the box image when COOP_GATE is set (so a merge can be
// revalidated in the box), or returns "" when no gate is configured.
func (a *app) mergeGate(repo string) (string, error) {
	if len(a.cfg.Gate) == 0 {
		return "", nil
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if !box.ImageExists(a.rt, img) {
		return "", fmt.Errorf("COOP_GATE is set but image %q isn't built — run 'coop build'", img)
	}
	return img, nil
}

// runGate runs COOP_GATE in the box against repo, reporting whether it passed.
func (a *app) runGate(repo, img string) bool {
	ui.Info("revalidating: %s", strings.Join(a.cfg.Gate, " "))
	code, _ := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: a.cfg.Gate, Batch: true,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
	return code == 0
}

// mergeOne fetches a fork's branch, merges it into the parent's HEAD, and — when a
// gate is configured — revalidates the merged result, rolling back on failure.
// "green" thus means green against the tree as it stands now, not the stale base the
// fork was cut from. Reports whether the merge landed.
func (a *app) mergeOne(repo, img, name string, force bool) (bool, error) {
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return false, fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return false, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	ref := "review/" + name
	if warns := policyScan(repo, ref); len(warns) > 0 && !force {
		return false, fmt.Errorf("%s: policy flagged risky changes:\n%s\n(use --force to merge anyway)", name, indent(strings.Join(warns, "\n")))
	}
	// Say which branch we're landing onto — merge rebases onto your *current* branch,
	// so this is your chance to notice you're on the wrong one.
	target := gitOut(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if target == "" || target == "HEAD" {
		target = "the current commit (detached HEAD)"
	}
	ui.Info("landing %s onto %s", name, target)
	pre := gitOut(repo, "rev-parse", "HEAD")
	if err := a.landFork(repo, ws, name); err != nil {
		return false, err
	}
	if img != "" { // COOP_GATE configured
		if !a.runGate(repo, img) {
			_ = gitRun(repo, "reset", "--hard", pre)
			return false, fmt.Errorf("%s: gate failed after rebase — rolled back", name)
		}
	}
	return true, nil
}

// wantsSigning reports whether you sign commits (commit.gpgsign=true in your git
// config), so a fork's unsigned box commits can be signed with your key on land.
func wantsSigning(repo string) bool {
	return gitOut(repo, "config", "--bool", "--get", "commit.gpgsign") == "true"
}

// trustedSignArgs returns the -c flags to sign the rebased commits with the host's
// key, every value read from the *parent* repo (trusted) so a fork's local signing
// config can't point gpg.program at a planted binary. They are appended after
// forkGitHardening — which turns signing off by default — so these re-enable it with
// vetted values. The program key tracks gpg.format (openpgp/ssh/x509).
func trustedSignArgs(repo string) []string {
	args := []string{"-c", "commit.gpgsign=true"}
	format := gitOut(repo, "config", "--get", "gpg.format")
	progKey, def := "gpg.program", "gpg"
	switch format {
	case "ssh":
		progKey, def = "gpg.ssh.program", "ssh-keygen"
	case "x509":
		progKey, def = "gpg.x509.program", "gpgsm"
	}
	if format != "" {
		args = append(args, "-c", "gpg.format="+format)
	}
	prog := gitOut(repo, "config", "--get", progKey)
	if prog == "" {
		prog = def // git's built-in default — set explicitly so the hardening's "=false" loses
	}
	args = append(args, "-c", progKey+"="+prog)
	if key := gitOut(repo, "config", "--get", "user.signingkey"); key != "" {
		args = append(args, "-c", "user.signingkey="+key)
	}
	return args
}

// landFork rebases the fork's branch onto the parent's current HEAD — in the fork,
// where that branch is checked out — then fast-forwards the parent onto the result.
// Forks therefore land as a linear replay, never a merge commit. A rebase conflict
// leaves the fork untouched and points at where to resolve.
func (a *app) landFork(repo, ws, name string) error {
	head := gitOut(repo, "rev-parse", "HEAD")
	// Every git command here runs with -C ws, an agent-controlled tree, so it goes
	// through the hardened helpers — a planted .git/hooks/* or malicious .git/config
	// must not execute on the host (see forkGitHardening).
	if err := gitRunFork(ws, "fetch", "--quiet", repo); err != nil {
		return fmt.Errorf("%s: fetching parent into the fork: %w", name, err)
	}
	// Box commits are unsigned (the box holds no key). If you sign your commits, sign
	// them here with your host key as the rebase rewrites them — -f forces the rewrite
	// so even a fast-forward land gets signed. The signing config comes from the parent
	// via trustedSignArgs, not the fork. Run with real stdio so a passphrase pinentry
	// can prompt.
	var rebaseErr error
	if wantsSigning(repo) {
		rebaseErr = gitInteractiveFork(ws, append(trustedSignArgs(repo), "rebase", "-f", "--gpg-sign", head)...)
	} else {
		rebaseErr = gitRunFork(ws, "rebase", head)
	}
	if rebaseErr != nil {
		_ = gitRunFork(ws, "rebase", "--abort")
		return fmt.Errorf("%s: rebase onto %s failed (conflicts or signing) — fix it in the fork (cd %q && git rebase %s), then re-run", name, gitBranch(repo), ws, head)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return fmt.Errorf("%s: git fetch: %w", name, err)
	}
	if err := gitRun(repo, "merge", "--ff-only", "review/"+name); err != nil {
		return fmt.Errorf("%s: fast-forward after rebase failed unexpectedly", name)
	}
	return nil
}

func (a *app) forkMerge(args []string) (int, error) {
	all, force, yes, name := false, false, false, ""
	for _, x := range args {
		switch x {
		case "--all":
			all = true
		case "--force", "-f":
			force = true
		case "--yes", "-y":
			yes = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork merge: unknown flag %q", x)
			}
			name = x
		}
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if gitDirty(repo) {
		return 1, errors.New("your working tree has uncommitted changes — commit or stash before merging")
	}
	// Merging lands work and (by default) deletes the fork. A non-interactive run has
	// no one to answer the prompts, so refuse rather than proceed on the default —
	// pass --yes to opt in explicitly.
	if !yes && !ui.IsTerminal(os.Stdin) {
		return 1, errors.New("coop fork merge: refusing to land in a non-interactive shell — pass --yes to confirm")
	}
	img, err := a.mergeGate(repo)
	if err != nil {
		return -1, err
	}
	if all {
		return a.forkMergeAll(repo, img, force)
	}
	if name == "" {
		return 2, errors.New("usage: coop fork merge <name> [--all] [--yes]")
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return -1, fmt.Errorf("git fetch: %w", err)
	}
	ref := "review/" + name
	ahead := gitOut(repo, "rev-list", "--count", "HEAD.."+ref)
	ins, del := parseShortstat(gitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	ui.Info("rebase %s onto %s — %s commit(s), +%d -%d", ref, gitBranch(repo), ahead, ins, del)
	if !approve("rebase and land?", yes) {
		return 0, nil
	}
	landed, err := a.mergeOne(repo, img, name, force)
	if err != nil {
		ui.Error("%v", err)
		return 1, nil
	}
	if !landed {
		return 1, nil
	}
	ui.Info("%s", ui.Green("✓ landed "+name))
	if approve("remove the fork?", yes) {
		if err := destroyFork(repo, name); err != nil {
			return -1, err
		}
		ui.Info("removed fork %s", name)
	}
	return 0, nil
}

// forkMergeAll lands every fork as a revalidating rebase queue: each is rebased onto
// the result of the previous one and re-gated, so a later fork can't ride in green
// against a base that an earlier landing already changed. It stops at the first
// conflict or gate failure, leaving the remaining forks untouched.
func (a *app) forkMergeAll(repo, img string, force bool) (int, error) {
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Info("no forks to merge")
		return 0, nil
	}
	var landed []string
	for _, n := range names {
		ws := forkWorkspace(repo, n)
		if err := gitFetchInto(repo, ws, n); err != nil {
			continue
		}
		if gitOut(repo, "rev-list", "--count", "HEAD..review/"+n) == "0" {
			continue // nothing to land
		}
		ok, err := a.mergeOne(repo, img, n, force)
		if err != nil {
			ui.Error("%v", err)
			ui.Info("rebase queue stopped at %s — %d landed, the rest left untouched", n, len(landed))
			return 1, nil
		}
		if ok {
			ui.Info("%s", ui.Green("✓ landed "+n))
			_ = destroyFork(repo, n)
			landed = append(landed, n)
		}
	}
	ui.Info("rebase queue: %d landed", len(landed))
	return 0, nil
}
