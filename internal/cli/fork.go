package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A fork is a throwaway local clone of your repo handed to an agent: its origin
// is a local path (so the agent has nowhere to push) and gitignored secrets never
// come along. The lifecycle mirrors a contractor's PR — open, review, merge, close.
//
//	coop fork perf codex   open (or resume) a fork; codex works in it
//	coop fork ls           the forks of this repo
//	coop fork review perf  fetch the fork's branch + show the diff
//	coop fork merge perf   merge it back into your working tree
//	coop fork rm perf      discard the fork
//
// Forks live in a sibling directory <repo>-forks/, one subdirectory per fork.

const forkSuffix = "-forks"

// forkVerbs are the reserved subcommands of `coop fork`; a fork can't be named one.
var forkVerbs = map[string]bool{
	"ls": true, "review": true, "merge": true, "rm": true, "open": true,
	"logs": true, "stop": true,
}

// forkHome is the sibling directory that holds every fork of repo.
func forkHome(repo string) string {
	return filepath.Join(filepath.Dir(repo), filepath.Base(repo)+forkSuffix)
}

// forkWorkspace is the clone directory for one named fork.
func forkWorkspace(repo, name string) string {
	return filepath.Join(forkHome(repo), name)
}

// validForkName keeps a name to a single safe path/branch segment.
func validForkName(name string) bool {
	if name == "" || forkVerbs[name] {
		return false
	}
	if name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
}

// cmdFork is the `coop fork` family. Bare `coop fork` lists; a reserved verb runs
// that subcommand; anything else opens (or resumes) a fork by that name.
func (a *app) cmdFork(args []string) (int, error) {
	if len(args) == 0 {
		return a.forkLs(nil)
	}
	switch args[0] {
	case "ls":
		return a.forkLs(args[1:])
	case "review":
		return a.forkReview(args[1:])
	case "merge":
		return a.forkMerge(args[1:])
	case "rm":
		return a.forkRm(args[1:])
	case "open":
		return a.forkOpen(args[1:])
	case "logs":
		return a.forkLogs(args[1:])
	case "stop":
		return a.forkStop(args[1:])
	default:
		return a.forkCreate(args)
	}
}

// forkArgs is the parsed form of `coop fork <name> [agent] [flags]`.
type forkArgs struct {
	name   string
	agent  string
	fresh  bool
	loop   bool
	detach bool
	worker bool // internal: this process IS the detached loop worker (--_detached)
}

func parseForkCreate(args []string) (forkArgs, error) {
	fa := forkArgs{agent: "claude"}
	if len(args) == 0 || args[0] == "" {
		return fa, errors.New("usage: coop fork <name> [claude|codex|gemini] [--loop [-d]]")
	}
	fa.name = args[0]
	for _, x := range args[1:] {
		switch x {
		case "claude", "codex", "gemini":
			fa.agent = x
		case "--fresh":
			fa.fresh = true
		case "--loop":
			fa.loop = true
		case "-d", "--detach":
			fa.detach = true
			fa.loop = true
		case "--_detached": // hidden: re-exec target for a detached loop
			fa.worker = true
			fa.loop = true
		default:
			return fa, fmt.Errorf("coop fork: unexpected argument %q", x)
		}
	}
	if !validForkName(fa.name) {
		return fa, fmt.Errorf("invalid fork name %q (no slashes, not a reserved verb)", fa.name)
	}
	return fa, nil
}

// forkCreate opens a new fork (clone + branch) or resumes an existing one, then
// runs the chosen agent in it. The agent's exit status doesn't fail the handoff.
func (a *app) forkCreate(args []string) (int, error) {
	fa, err := parseForkCreate(args)
	if err != nil {
		return 2, err
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, fa.name)
	if fa.fresh && pathExists(ws) {
		if err := destroyFork(repo, fa.name); err != nil {
			return -1, err
		}
	}
	if !pathExists(ws) {
		ui.Info("forking %s → %s (secrets are gitignored, so they don't come along)", filepath.Base(repo), ws)
		if _, err := setupFork(repo, fa.name); err != nil {
			return -1, err
		}
	} else if !fa.worker {
		ui.Info("resuming fork %s (%s)", fa.name, ws)
	}
	if fa.loop {
		switch {
		case fa.worker:
			return a.runForkLoop(repo, ws, fa.name, fa.agent, true)
		case fa.detach:
			return a.detachForkLoop(repo, fa.name, fa.agent)
		default:
			return a.runForkLoop(repo, ws, fa.name, fa.agent, false)
		}
	}
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: ws, Cmd: a.defaultCmd(fa.agent), ConsultLead: fa.agent,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
	forkNextSteps(fa.name)
	return 0, nil
}

func forkNextSteps(name string) {
	ui.Info("review · merge · discard:")
	ui.Info("  coop fork review %s   coop fork merge %s   coop fork rm %s", name, name, name)
}

// setupFork creates the clone and its branch (the git half of forkCreate, with no
// agent run — so the lifecycle is testable without a container).
func setupFork(repo, name string) (string, error) {
	ws := forkWorkspace(repo, name)
	if err := os.MkdirAll(forkHome(repo), 0o755); err != nil {
		return ws, err
	}
	if err := gitClone(repo, ws); err != nil {
		return ws, fmt.Errorf("git clone: %w", err)
	}
	_ = gitCheckoutNewBranch(ws, name) // branch may already exist in origin; fine
	return ws, nil
}

// destroyFork removes a fork's workspace and its review/<name> ref, then prunes an
// empty forks home. Best-effort on the ref so it works for partially-built forks.
func destroyFork(repo, name string) error {
	_ = gitRun(repo, "branch", "-q", "-D", "review/"+name)
	if err := os.RemoveAll(forkWorkspace(repo, name)); err != nil {
		return err
	}
	if entries, _ := os.ReadDir(forkHome(repo)); len(entries) == 0 {
		_ = os.Remove(forkHome(repo))
	}
	return nil
}

// forkNames lists the forks of repo (subdirectories of the forks home, skipping
// the hidden state dir).
func forkNames(repo string) []string {
	entries, _ := os.ReadDir(forkHome(repo))
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func (a *app) forkLs(_ []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Info("no forks yet — open one with 'coop fork <name>'")
		return 0, nil
	}
	fmt.Printf("  %-16s %-12s %-9s %-15s %s\n",
		ui.Bold("NAME"), ui.Bold("BRANCH"), ui.Bold("STATE"), ui.Bold("CHANGES"), ui.Bold("UPDATED"))
	for _, n := range names {
		ws := forkWorkspace(repo, n)
		state := "idle"
		if forkRunningPid(repo, n) != 0 {
			state = "running"
		}
		fmt.Printf("  %-16s %-12s %-9s %-15s %s\n", n, gitBranch(ws), state, forkChanges(ws), forkUpdated(ws))
	}
	return 0, nil
}

// forkChanges summarizes a fork's diff against the point it forked from, plus a
// flag when it has uncommitted work.
func forkChanges(ws string) string {
	ins, del := parseShortstat(gitOut(ws, "diff", "--shortstat", "origin/HEAD"))
	out := fmt.Sprintf("+%d -%d", ins, del)
	if gitDirty(ws) {
		out += " ⚑"
	}
	return out
}

func forkUpdated(ws string) string {
	if rel := gitOut(ws, "log", "-1", "--format=%cr"); rel != "" {
		return rel
	}
	return "—"
}

// gitFetchInto fetches a fork's branch into review/<name> in the parent repo.
func gitFetchInto(repo, ws, name string) error {
	return gitRun(repo, "fetch", "--quiet", ws, name+":review/"+name)
}

func (a *app) forkReview(args []string) (int, error) {
	name, stat := "", false
	for _, x := range args {
		switch x {
		case "--stat":
			stat = true
		default:
			name = x
		}
	}
	if name == "" {
		return 2, errors.New("usage: coop fork review <name> [--stat]")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if !pathExists(forkWorkspace(repo, name)) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, forkWorkspace(repo, name), name); err != nil {
		return -1, fmt.Errorf("git fetch: %w", err)
	}
	ref := "review/" + name
	ui.Info("%s ← %s", ref, name)
	diffArgs := []string{"diff"}
	if stat {
		diffArgs = append(diffArgs, "--stat")
	}
	diffArgs = append(diffArgs, "HEAD..."+ref)
	_ = gitInteractive(repo, diffArgs...)
	return 0, nil
}

// forkRmSafe is the guard for `rm`: never silently drop an agent's work.
func forkRmSafe(unmerged, dirty, force bool) error {
	if force {
		return nil
	}
	if dirty {
		return errors.New("fork has uncommitted changes — use --force to discard")
	}
	if unmerged {
		return errors.New("fork has unmerged commits — merge it first, or use --force")
	}
	return nil
}

// forkUnmerged reports whether the fork's branch tip is NOT yet an ancestor of the
// parent repo's HEAD (unknown-to-parent counts as unmerged, which is the safe side).
func forkUnmerged(repo, ws string) bool {
	sha := gitOut(ws, "rev-parse", "HEAD")
	if sha == "" {
		return false
	}
	return gitRun(repo, "merge-base", "--is-ancestor", sha, "HEAD") != nil
}

func (a *app) forkRm(args []string) (int, error) {
	name, force := "", false
	for _, x := range args {
		switch x {
		case "--force", "-f":
			force = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork rm: unknown flag %q", x)
			}
			name = x
		}
	}
	if name == "" {
		return 2, errors.New("usage: coop fork rm <name> [--force]")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
		return 1, err
	}
	if err := destroyFork(repo, name); err != nil {
		return -1, err
	}
	ui.Info("removed fork %s", name)
	return 0, nil
}

// forkOpen prints a fork's path (for `cd "$(coop fork open <name>)"`).
func (a *app) forkOpen(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork open <name>")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, args[0])
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", args[0])
	}
	fmt.Println(ws)
	return 0, nil
}
