package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// secret-looking filenames, large blobs, and — by scanning each changed blob's
// content — real tokens sitting in ordinary files (which a filename check can't see).
// Empty means nothing flagged.
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
func wantsSigning() bool {
	// Read from your GLOBAL config, never the agent-writable repo: a poisoned repo could otherwise
	// force signing on so its planted gpg.program runs — and your signing preference is global anyway.
	return gitGlobalOut("--bool", "--get", "commit.gpgsign") == "true"
}

// trustedSignArgs returns the -c flags to sign the rebased commits with the host's key, every
// value read from your GLOBAL git config so neither the fork NOR the agent-writable parent repo can
// point gpg.program at a planted binary. They are appended after gitHardening — which turns signing
// off by default — so these re-enable it with vetted values. The program key tracks gpg.format
// (openpgp/ssh/x509).
func trustedSignArgs() []string {
	args := []string{"-c", "commit.gpgsign=true"}
	format := gitGlobalOut("--get", "gpg.format")
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
	prog := gitGlobalOut("--get", progKey)
	if prog == "" {
		prog = def // git's built-in default — set explicitly so the hardening's "=false" loses
	}
	args = append(args, "-c", progKey+"="+prog)
	if key := gitGlobalOut("--get", "user.signingkey"); key != "" {
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
	// Every git command here runs on an agent-controlled tree (the fork ws AND the parent repo,
	// whose .git the agent could have poisoned), so all go through the hardened helpers — a
	// planted .git/hooks/* or malicious .git/config must not execute on the host (see gitHardening).
	if err := gitRun(ws, "fetch", "--quiet", repo); err != nil {
		return fmt.Errorf("%s: fetching parent into the fork: %w", name, err)
	}
	// Box commits are unsigned (the box holds no key). If you sign your commits, sign
	// them here with your host key as the rebase rewrites them — -f forces the rewrite
	// so even a fast-forward land gets signed. The signing config comes from the parent
	// via trustedSignArgs, not the fork. Run with real stdio so a passphrase pinentry
	// can prompt.
	// Blank any filter/merge/diff driver the fork's .git/config defines before the rebase checks
	// the tree out — an in-tree .gitattributes + a fork-local driver would otherwise run host code
	// on checkout/merge/diff (the residual gitHardening can't close, since the names are arbitrary).
	neut := forkDriverNeutralizer(ws)
	withNeut := func(args ...string) []string { return append(append([]string{}, neut...), args...) }
	var rebaseErr error
	if wantsSigning() {
		rebaseErr = gitInteractive(ws, withNeut(append(trustedSignArgs(), "rebase", "-f", "--gpg-sign", head)...)...)
	} else {
		rebaseErr = gitRun(ws, withNeut("rebase", head)...)
	}
	if rebaseErr != nil {
		_ = gitRun(ws, withNeut("rebase", "--abort")...)
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
