package forkctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// PolicyScan returns human-readable concerns about a fork's added/changed files:
// secret-looking filenames, large blobs, and — by scanning each changed blob's
// content — real tokens sitting in ordinary files (which a filename check can't see).
// Empty means nothing flagged.
func PolicyScan(repo, ref string) []string {
	out := gitOut(repo, "diff", "--name-status", "HEAD..."+ref)
	if out == "" {
		return nil
	}
	// Flag a credential-looking filename with the SAME decider that shadows secrets from the box,
	// so the merge gate can't drift from the shadow denylist (it used to be a separate hand-rolled
	// regex that missed kubeconfig/.npmrc/.netrc/service_account.json/*.kdbx/… that SecretGlobs covers).
	shadowed := box.NewShadowDecider(repo)
	var warns []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "D" { // deletions are never a concern
			continue
		}
		path := f[len(f)-1]
		if shadowed(filepath.ToSlash(path)) {
			warns = append(warns, "secret-like file: "+path)
		}
		// Files that run host code the moment a human touches the merged tree (cd, open the
		// folder, `make`) — path-based, so a huge/binary blob can't dodge it below.
		if w := interactionRiskPath(f[0], path); w != "" {
			warns = append(warns, w)
		}
		if size := gitBlobSize(repo, ref, path); size > 5<<20 {
			warns = append(warns, fmt.Sprintf("large file (%dMB): %s", size>>20, path))
			continue // don't read a huge blob's content
		}
		content := gitOut(repo, "show", ref+":"+path)
		if strings.IndexByte(content, 0) >= 0 { // skip binaries
			continue
		}
		if filepath.Base(path) == "package.json" {
			if k := addedLifecycleScript(repo, ref, path, content); k != "" {
				warns = append(warns, path+" adds a "+k+" script — npm runs it automatically on `npm install`")
			}
		}
		for _, s := range box.ScanSecrets(content) {
			warns = append(warns, fmt.Sprintf("possible secret in %s:%d (%s) — remove it or add the file to .coopignore", path, s.Line, s.Kind))
		}
	}
	return warns
}

func gitBlobSize(repo, ref, path string) int64 {
	n, _ := strconv.ParseInt(gitOut(repo, "cat-file", "-s", ref+":"+path), 10, 64)
	return n
}

// interactionRiskPath flags an added/changed file that runs host code the moment a human
// interacts with the merged tree — direnv's .envrc on `cd`, a VS Code tasks.json on folder-open,
// a Makefile on `make`. It's a review aid (these block a merge like a secret hit unless you pass
// --force), not a sandbox: it names high-signal files, it doesn't try to prove them safe. status
// is the `git diff --name-status` code (A/M/R…); "" means not flagged.
func interactionRiskPath(status, path string) string {
	base := filepath.Base(path)
	added := status != "" && (status[0] == 'A' || status[0] == 'R') // R = rename → new path here
	switch {
	case base == ".envrc":
		return path + " runs on `cd` into the dir (direnv) — review it before entering"
	case base == "tasks.json" && strings.Contains(path, ".vscode/"):
		return path + " can auto-run a task when the folder opens (VS Code)"
	case (base == "Makefile" || base == "GNUmakefile") && added:
		return path + " runs host commands on `make` — review the new Makefile"
	}
	return ""
}

// addedLifecycleScript returns the name of an npm install/prepare lifecycle script the fork ADDS
// or changes in package.json (preinstall/install/postinstall/prepare) — npm runs these
// automatically on `npm install`, so a fork can plant one to execute host code post-merge — or ""
// when the change touches no such script (an ordinary dependency bump isn't flagged).
func addedLifecycleScript(repo, ref, path, newContent string) string {
	newS := pkgScripts(newContent)
	oldS := pkgScripts(gitOut(repo, "show", "HEAD:"+path)) // "" → nil when the file is new
	for _, k := range []string{"preinstall", "install", "postinstall", "prepare"} {
		if v := newS[k]; v != "" && v != oldS[k] {
			return k
		}
	}
	return ""
}

// pkgScripts parses a package.json's "scripts" map, or nil if it doesn't parse.
func pkgScripts(content string) map[string]string {
	var p struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal([]byte(content), &p) != nil {
		return nil
	}
	return p.Scripts
}

// gateFor resolves the fork-merge revalidation gate for repo: an explicit COOP_GATE (env/conf)
// wins; otherwise the repo's committed .agent/project.yaml gate:. The gate runs IN THE BOX (see
// runGate), so a repo-authored command executes sandboxed — same trust class as the code it merges.
func (c *Control) gateFor(repo string) []string {
	if c.cfg.Explicit("COOP_GATE") {
		return c.cfg.Gate
	}
	if p, err := project.Load(repo); err == nil {
		if g := strings.TrimSpace(p.Gate); g != "" {
			return config.ShellSplit(g)
		}
	}
	return c.cfg.Gate
}

// MergeGate resolves the box image when a merge gate is configured (so a merge can be revalidated
// in the box), or returns "" when none is.
func (c *Control) MergeGate(repo string) (string, error) {
	if len(c.gateFor(repo)) == 0 {
		return "", nil // no gate configured → the merge is pure-local, no runtime needed
	}
	if err := c.ensureRuntime(); err != nil {
		return "", err
	}
	img := box.ImageForRepo(repo, c.cfg.BaseImage, c.cfg.ImageOverride)
	if !box.ImageExists(c.rt, img) {
		// Same rule as resolveImage: `image inspect` cannot tell a missing image from a dead daemon,
		// and a merge blocked on the wrong one sends the fix at a build that would not have helped.
		if err := c.rt.EnsureDaemon(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("a merge gate is set but image %q isn't built — run 'coop build'", img)
	}
	return img, nil
}

// runGateMode runs the merge gate in the box against treeDir. The gate POLICY is resolved from
// gateRepo (the trusted parent), never treeDir — a fork can't weaken its own checker — but it RUNS
// against treeDir (the rebased candidate), so a red gate never touches the parent. A non-zero gate
// is a normal red result; an error means the box never started.
func (c *Control) runGateMode(gateRepo, treeDir, img string, review bool) (bool, error) {
	gate := c.gateFor(gateRepo)
	ui.Info("revalidating: %s", strings.Join(gate, " "))
	reviewBase := strings.TrimSpace(gitOut(treeDir, "rev-parse", "--verify", "refs/coop/session-parent^{commit}"))
	if reviewBase == "" {
		reviewBase = strings.TrimSpace(gitOut(gateRepo, "rev-parse", "--verify", "HEAD^{commit}"))
	}
	if reviewBase == "" {
		return false, errors.New("resolve trusted review base commit")
	}
	code, err := box.Run(c.cfg, c.rt, box.RunSpec{
		Image: img, Repo: treeDir, Cmd: gate, Batch: true,
		PolicyRepo: gateRepo,
		Review:     review,
		Serve:      review,
		ExtraArgs:  []string{"-e", "COOP_REVIEW_BASE=" + reviewBase},
		Homes:      c.cfg.Homes, Network: c.cfg.Network, Cache: c.cfg.Cache,
	})
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// runGate preserves merge's existing bool-only contract: a box startup failure is a failed gate.
func (c *Control) runGate(gateRepo, treeDir, img string) bool {
	ok, _ := c.runGateMode(gateRepo, treeDir, img, false)
	return ok
}

// gatePasses runs the merge gate (or the test seam, when set).
func (c *Control) gatePasses(gateRepo, treeDir, img string) bool {
	if c.gateOK != nil {
		return c.gateOK(gateRepo, treeDir, img)
	}
	return c.runGate(gateRepo, treeDir, img)
}

// ReviewGatePasses is the review-only gate path. The disposable candidate may write ignored build
// output; its caller verifies the pinned source identity and cleanliness after the gate returns.
// Startup errors remain distinguishable from an ordinary red gate.
func (c *Control) ReviewGatePasses(gateRepo, treeDir, img string) (bool, error) {
	if c.gateOK != nil {
		return c.gateOK(gateRepo, treeDir, img), nil
	}
	return c.runGateMode(gateRepo, treeDir, img, true)
}

type forkMergeLifecycleError struct{ cause error }

func (e *forkMergeLifecycleError) Error() string { return e.cause.Error() }
func (e *forkMergeLifecycleError) Unwrap() error { return e.cause }

func isForkMergeLifecycleError(err error) bool {
	var target *forkMergeLifecycleError
	return errors.As(err, &target)
}

// lockForkForMerge is the destructive merge authority. The early command preflight gives a useful
// refusal before runtime work, but only this flock closes the check-to-rebase/delete race with a
// concurrent detached start. Callers hold it for every host mutation of the fork or its workspace.
func lockForkForMerge(repo, name string) (func(), error) {
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return nil, fmt.Errorf("lock fork %s state: %w", name, err)
	}
	if err := CheckWorkerStateFormat(repo, name); err != nil {
		unlock()
		return nil, &forkMergeLifecycleError{cause: err}
	}
	if forkspace.NeedsStop(repo, name) {
		unlock()
		return nil, &forkMergeLifecycleError{cause: fmt.Errorf("fork %q is running or awaiting cleanup — stop it first: coop fork stop %s", name, name)}
	}
	return unlock, nil
}

func fetchForkForMerge(repo, ws, name string) error {
	unlock, err := lockForkForMerge(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	if !pathExists(ws) {
		return fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return fmt.Errorf("%s: git fetch: %w", name, err)
	}
	return nil
}

func destroyLandedFork(rt runtime.Runtime, repo, name string) error {
	unlock, err := lockForkForMerge(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	if !pathExists(forkspace.Workspace(repo, name)) {
		return fmt.Errorf("no such fork: %s", name)
	}
	return DestroyFork(rt, repo, name)
}

// mergeOne fetches a fork's branch, merges it into the parent's HEAD, and — when a
// gate is configured — revalidates the merged result, rolling back on failure.
// "green" thus means green against the tree as it stands now, not the stale base the
// fork was cut from. Reports whether the merge landed: landed=false with an error is a merge that
// did NOT happen, while landed=true WITH an error means the commits are in the parent but the queue
// reconciliation below couldn't be done — the caller reports it and stops, never rolls the land back.
func (c *Control) mergeOne(repo, img, name string, force bool) (bool, error) {
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return false, fmt.Errorf("no such fork: %s", name)
	}
	unlock, err := lockForkForMerge(repo, name)
	if err != nil {
		return false, err
	}
	defer unlock()
	if !pathExists(ws) {
		return false, fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return false, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	ref := "review/" + name
	if warns := PolicyScan(repo, ref); len(warns) > 0 && !force {
		return false, fmt.Errorf("%s: policy flagged risky changes:\n%s\n(use --force to merge anyway)", name, indent(strings.Join(warns, "\n")))
	}
	// Say which branch we're landing onto — merge rebases onto your *current* branch,
	// so this is your chance to notice you're on the wrong one.
	target := gitOut(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if target == "" || target == "HEAD" {
		target = "the current commit (detached HEAD)"
	}
	ui.Info("landing %s onto %s", name, target)
	// Rebase the fork onto the parent's HEAD inside the fork's OWN clone — an isolated candidate.
	// The parent tree is NOT touched here, so a red gate below has nothing to roll back.
	if err := c.rebaseForkOntoParent(repo, ws, name); err != nil {
		return false, err
	}
	parentBeforeLand := gitOut(repo, "rev-parse", "HEAD")
	// Gate the CANDIDATE (the rebased fork), never the live parent — with the parent's own gate
	// policy. A red gate leaves the parent exactly as it was: no reset --hard of a shared tree.
	if img != "" && !c.gatePasses(repo, ws, img) {
		return false, fmt.Errorf("%s: gate failed on the rebased fork — parent untouched; fix it in the fork (%s), then re-run", name, ws)
	}
	// Advance the parent by a fast-forward-ONLY merge — an atomic compare-and-swap: it refuses if a
	// concurrent commit moved the parent since the rebase, so a divergence lands nothing (re-run to
	// rebase onto the new HEAD) instead of being erased by a rollback.
	if err := c.FastForwardParent(repo, ws, name); err != nil {
		return false, err
	}
	// Reconcile the parent queue: a task whose Coop-Task trailer just landed moves to done/, so the
	// parent loop doesn't redo work this fork already completed. The land already stuck, so a
	// reconcile that couldn't run comes back as landed-with-an-error, never as a rollback.
	if err := tasks.ReconcileQueueAfterMerge(c.cfg, repo, name, parentBeforeLand+"..HEAD"); err != nil {
		return true, err
	}
	return true, nil
}

// landFork rebases the fork's branch onto the parent's current HEAD — in the fork,
// where that branch is checked out — then fast-forwards the parent onto the result.
// Forks therefore land as a linear replay, never a merge commit. A rebase conflict
// leaves the fork untouched and points at where to resolve.
// rebaseForkOntoParent rebases the fork's branch onto the parent's current HEAD inside the fork's
// OWN clone (ws), producing the landing candidate WITHOUT touching the parent. Box commits are
// unsigned (the box holds no key); when you sign, the rebase re-signs the rewritten commits with
// your host key (-f forces the rewrite so even a fast-forward land gets signed), the signing config
// coming from the parent via forkspace.TrustedSignArgs, never the fork.
func (c *Control) rebaseForkOntoParent(repo, ws, name string) error {
	// A rebase that FAILS is aborted below, but a coop killed mid-rebase (a host crash, a SIGKILL)
	// leaves git's state dir behind, and every later merge then dies on the leftover state instead of
	// recovering it. Clear it first — under the same ownership rule as every other destructive path.
	if err := recoverInterruptedRebase(repo, ws, name); err != nil {
		return err
	}
	head := gitOut(repo, "rev-parse", "HEAD")
	if head == "" {
		// A parent with no commits yet → `git rebase ""` fails with a cryptic "invalid upstream";
		// give a clear cause instead.
		return fmt.Errorf("%s: the parent repo has no commits yet — make an initial commit before landing a fork", name)
	}
	// Every git command here runs on an agent-controlled tree (the fork ws AND the parent repo,
	// whose .git the agent could have poisoned), so all go through the hardened helpers — a planted
	// .git/hooks/* or malicious .git/config must not execute on the host (forkspace.GitHardening).
	if err := gitRun(ws, "fetch", "--quiet", repo); err != nil {
		return fmt.Errorf("%s: fetching parent into the fork: %w", name, err)
	}
	// Blank any filter/merge/diff driver the fork's .git/config defines before the rebase checks
	// the tree out — an in-tree .gitattributes + a fork-local driver would otherwise run host code
	// on checkout/merge/diff (the residual forkspace.GitHardening can't close, since the names are
	// arbitrary).
	neut := forkspace.DriverNeutralizer(ws)
	withNeut := func(args ...string) []string { return append(append([]string{}, neut...), args...) }
	// Rebase the fork's branch by NAME, not whatever the agent left checked out — `git rebase
	// <upstream> <branch>` checks out and rebases exactly `name`, so the branch we sign and rebase
	// is provably the same one the parent fast-forwards to (an agent that `git checkout`ed a
	// different branch in the ws can't make us land un-rebased, unsigned commits).
	var rebaseErr error
	if forkspace.WantsSigning() {
		rebaseErr = gitSign(ws, withNeut(append(forkspace.TrustedSignArgs(), "rebase", "-f", "--gpg-sign", head, name)...)...)
	} else {
		rebaseErr = gitRun(ws, withNeut("rebase", head, name)...)
	}
	if rebaseErr != nil {
		_ = gitRun(ws, withNeut("rebase", "--abort")...)
		return fmt.Errorf("%s: rebase onto %s failed (conflicts or signing) — fix it in the fork (cd %q && git rebase %s %s), then re-run", name, gitBranch(repo), ws, head, name)
	}
	return nil
}

// leftoverRebaseState returns the state directory of an unfinished rebase in ws, or "". Git keeps it
// in the worktree's own git dir — rebase-merge for the default backend, rebase-apply for the am one —
// and --git-path resolves either, in a plain clone as in a linked worktree.
func leftoverRebaseState(ws string) string {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		path := gitOut(ws, "rev-parse", "--git-path", dir)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(ws, path)
		}
		if pathExists(path) {
			return path
		}
	}
	return ""
}

// recoverInterruptedRebase clears rebase state a CRASHED land left in the fork's clone, so the next
// merge recovers instead of failing on it forever. Recovery is destructive — `rebase --abort` resets
// that worktree — so it runs only when the fork's lifecycle state names nobody who could still be
// running (same pid + start-token test the stop path signals by; see forkspace.StateOwner). Both
// outcomes are loud: what was found and what was done, or who owns it and how to stop them.
func recoverInterruptedRebase(repo, ws, name string) error {
	dir := leftoverRebaseState(ws)
	if dir == "" {
		return nil
	}
	if pid, held := forkspace.StateOwner(repo, name); held {
		owner := "an owner coop cannot verify"
		if pid > 0 {
			owner = fmt.Sprintf("pid %d", pid)
		}
		return fmt.Errorf("%s: the fork worktree has an unfinished rebase (%s) from an interrupted land, but its lifecycle state is still held by %s — stop it first: coop fork stop %s", name, filepath.Base(dir), owner, name)
	}
	ui.Warn("fork %s has an unfinished rebase (%s) from an interrupted land — aborting it to recover the worktree", name, filepath.Base(dir))
	// gitOutErr, not gitRun: git's own stderr is the only explanation a human gets for a failed abort.
	if _, err := gitOutErr(ws, append(forkspace.DriverNeutralizer(ws), "rebase", "--abort")...); err != nil {
		return fmt.Errorf("%s: could not abort the unfinished rebase in %s: %w — finish it by hand (cd %q && git status; git rebase --abort), then re-run the merge", name, filepath.Base(dir), err, ws)
	}
	ui.Detail("aborted the unfinished rebase; %s is back on its branch", name)
	return nil
}

// FastForwardParent fetches the rebased candidate back into the parent and advances the parent
// branch by a fast-forward-ONLY merge. --ff-only IS the compare-and-swap: it succeeds only while the
// parent is still the commit the fork was rebased onto, so a concurrent commit during the gate makes
// it refuse — nothing lands, the divergence is preserved — instead of a reset --hard erasing it.
func (c *Control) FastForwardParent(repo, ws, name string) error {
	if err := gitFetchInto(repo, ws, name); err != nil {
		return fmt.Errorf("%s: git fetch: %w", name, err)
	}
	// This is coop's own host-side ref mutation on the PARENT checkout — the same worktree a `coop
	// loop` there may be validating a completion against. The ref-authority lock keeps this
	// fast-forward from landing inside that loop's validate→consume window (and vice versa), so
	// landing a fork can never trip the loop's own compare-and-swap.
	release, lockErr := tasks.LockRefAuthority(c.cfg, repo)
	if lockErr != nil {
		return fmt.Errorf("%s: acquire ref authority for %s: %w", name, repo, lockErr)
	}
	defer release()
	if err := gitRun(repo, "merge", "--ff-only", "review/"+name); err != nil {
		return fmt.Errorf("%s: the parent advanced during the merge — nothing landed; re-run to rebase onto the new HEAD", name)
	}
	return nil
}

func (c *Control) ForkMerge(args []string) (int, error) {
	all, force, yes := false, false, false
	var pos []string
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
			pos = append(pos, x)
		}
	}
	name, err := oneForkName("merge", pos)
	if err != nil {
		return 2, err
	}
	// Validate the static args before the environment: a missing <name> (without --all) is a usage
	// error (exit 2), not the dirty-tree / non-interactive error (exit 1) the env gates below report.
	if !all && name == "" {
		return 2, errors.New("usage: coop fork merge <name> [--all] [--yes]")
	}
	if !all && !forkspace.ValidName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	var names []string
	if all {
		names = forkspace.Names(repo)
		for _, n := range names {
			if err := CheckWorkerStateFormat(repo, n); err != nil {
				return 1, err
			}
		}
	} else if err := CheckWorkerStateFormat(repo, name); err != nil {
		return 1, err
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
	img, err := c.MergeGate(repo)
	if err != nil {
		return -1, err
	}
	if all {
		return c.forkMergeAll(repo, names, img, force, yes)
	}
	ws := forkspace.Workspace(repo, name) // name is non-empty here (the !all && name=="" check above returned)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	// Rebasing/deleting a fork whose loop is still mid-iteration corrupts the in-flight work and
	// orphans the worker. Stop the loop first.
	if err := fetchForkForMerge(repo, ws, name); err != nil {
		if isForkMergeLifecycleError(err) {
			return 1, err
		}
		return -1, err
	}
	ref := "review/" + name
	ahead := gitOut(repo, "rev-list", "--count", "HEAD.."+ref)
	ins, del := parseShortstat(gitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	ui.Info("rebase %s onto %s — %s commit(s), +%d -%d", ref, gitBranch(repo), ahead, ins, del)
	if _, s := c.host.forkCost(ws); s != "" {
		ui.Info("fork cost: %s", s)
	}
	if !approve("rebase and land?", yes) {
		return 0, nil
	}
	landed, err := c.mergeOne(repo, img, name, force)
	if landed {
		ui.OK("landed %s", name) // say it BEFORE any error: the commits are in the parent either way
	}
	if err != nil {
		// A landed fork whose queue reconciliation failed keeps its workspace and its exit code: the
		// human is being asked to check the queue, so this is no moment to delete anything.
		ui.Error("%v", err)
		return 1, nil
	}
	if !landed {
		return 1, nil
	}
	// The merge landed the committed work (via review/<name>); an interrupted iteration can still leave
	// uncommitted changes in the fork's worktree, so keep a dirty fork with a note rather than discard it.
	if gitDirty(ws) {
		ui.Warn("keeping fork %s — its worktree has uncommitted changes; inspect, then 'coop fork rm %s --force'", name, name)
		return 0, nil
	}
	// Default-No delete confirm (the land above was the default-Yes step); --yes is already required
	// for a non-interactive run, so this only prompts at a TTY. Declining just keeps the landed fork.
	if ui.DestroyGate("remove the landed fork "+name, yes) == nil {
		if err := destroyLandedFork(c.rt, repo, name); err != nil {
			if isForkMergeLifecycleError(err) {
				return 1, err
			}
			return -1, err
		}
		ui.OK("removed fork %s", name)
	}
	return 0, nil
}

// forkMergeAll lands every fork as a revalidating rebase queue: each is rebased onto
// the result of the previous one and re-gated, so a later fork can't ride in green
// against a base that an earlier landing already changed. It stops at the first
// conflict or gate failure, leaving the remaining forks untouched.
func (c *Control) forkMergeAll(repo string, names []string, img string, force, yes bool) (int, error) {
	if len(names) == 0 {
		ui.Info("no forks to merge")
		return 0, nil
	}
	// Never touch a fork whose loop is still running — rebasing/deleting its live worktree corrupts
	// in-flight work and orphans the worker. Skip those with a notice and land the rest.
	skip := map[string]bool{}
	if live := runningForkNames(repo, names); len(live) > 0 {
		ui.Info("skipping %s: %s — stop each with 'coop fork stop <name>' to land", ui.Count(len(live), "running/cleanup-pending fork"), strings.Join(live, ", "))
		for _, n := range live {
			skip[n] = true
		}
	}
	if len(skip) == len(names) {
		ui.Info("no forks to merge — every fork is running or awaiting cleanup")
		return 0, nil
	}
	// Landing every fork also DELETES each one — and unlike the single-fork path (which prompts per
	// fork), this runs unattended. Ask once before destroying anything; --yes (already required for a
	// non-interactive run) skips the prompt.
	if !approve(fmt.Sprintf("rebase and land up to %s? each that lands is then deleted", ui.Count(len(names)-len(skip), "fork")), yes) {
		return 0, nil
	}
	var landed []string
	for _, n := range names {
		if skip[n] {
			continue
		}
		ws := forkspace.Workspace(repo, n)
		if err := fetchForkForMerge(repo, ws, n); err != nil {
			return 1, err
		}
		if gitOut(repo, "rev-list", "--count", "HEAD..review/"+n) == "0" {
			continue // nothing to land
		}
		ok, err := c.mergeOne(repo, img, n, force)
		if ok {
			ui.OK("landed %s", n)
			// Keep the fork when its worktree still holds uncommitted work (an interrupted iteration),
			// and when its queue reconciliation failed — deleting a workspace right after an
			// unexplained bookkeeping failure removes the one thing left to inspect.
			if gitDirty(ws) {
				ui.Warn("keeping fork %s — uncommitted changes; 'coop fork rm %s --force' after review", n, n)
			} else if err == nil {
				if destroyErr := destroyLandedFork(c.rt, repo, n); destroyErr != nil {
					ui.Warn("keeping landed fork %s — it changed before removal: %v", n, destroyErr)
				}
			}
			landed = append(landed, n)
		}
		if err != nil {
			ui.Error("%v", err)
			ui.Info("rebase queue stopped at %s — %d landed, the rest left untouched", n, len(landed))
			return 1, nil
		}
	}
	ui.OK("%s landed", ui.Count(len(landed), "fork"))
	return 0, nil
}
