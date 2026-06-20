package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

// fleetEntry is one fork in the declarative fleet: a name, the model to run it, and
// the tasks file that seeds its loop.
type fleetEntry struct {
	name  string
	agent string
	tasks string
}

// fleetFile is the declarative fleet: .agent/fleet, one fork per line as
// "<name> [agent] <tasks-path>" (agent defaults to claude; the tasks path is required
// and is relative to the repo root). Blank and `#` lines are ignored.
func fleetFile(repo string) string { return filepath.Join(repo, ".agent", "fleet") }

func parseFleet(data string) ([]fleetEntry, error) {
	var out []fleetEntry
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		e := fleetEntry{name: f[0], agent: agents.Default()}
		rest := f[1:]
		// An optional agent token may precede the required tasks path.
		if len(rest) > 0 && agents.Valid(rest[0]) {
			e.agent = rest[0]
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return nil, fmt.Errorf("fleet: %q needs a tasks path — %q", e.name, "<name> [agent] <tasks-path>")
		}
		e.tasks = rest[0]
		if !validForkName(e.name) {
			return nil, fmt.Errorf("fleet: invalid fork name %q", e.name)
		}
		out = append(out, e)
	}
	return out, nil
}

func (a *app) loadFleet(repo string) ([]fleetEntry, error) {
	data, err := os.ReadFile(fleetFile(repo))
	if err != nil {
		return nil, errors.New("no .agent/fleet — declare one fork per line: <name> [agent]")
	}
	return parseFleet(string(data))
}

// cmdFleet manages a declarative fleet of forks from .agent/fleet.
func (a *app) cmdFleet(args []string) (int, error) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "init":
		return a.fleetInit()
	case "up":
		return a.fleetUp(args[1:])
	case "down":
		return a.fleetDown(args[1:])
	case "split":
		return a.fleetSplit(args[1:])
	case "watch":
		return a.fleetWatch()
	case "prune":
		return a.fleetPrune(args[1:])
	default:
		// `ls` lives on `coop fork ls`, not here.
		return 2, unknownErr("fleet command", sub, []string{"init", "up", "down", "split", "watch", "prune"})
	}
}

// fleetTemplate seeds .agent/fleet with a documented, ready-to-edit format.
const fleetTemplate = `# coop fleet — one fork per line:  <name> [agent] <tasks-path>
#   <name>        the fork's name (also its git branch)
#   [agent]       claude (default), codex, or gemini
#   <tasks-path>  the tasks file that seeds the fork's loop (relative to the repo)
# Blank lines and #-comments are ignored.  Start the fleet with: coop fleet up
#
# Example:
# api    codex   .agent/TASKS.api.md
# deps   gemini  .agent/TASKS.deps.md
`

// fleetInit writes a documented .agent/fleet template so you can declare a fleet without
// remembering the format. It never clobbers an existing file.
func (a *app) fleetInit() (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	path := fleetFile(repo)
	if fileExists(path) {
		return 1, errors.New(".agent/fleet already exists — edit it, or remove it to start over")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(path, []byte(fleetTemplate), 0o644); err != nil {
		return -1, err
	}
	ui.Info("wrote .agent/fleet — add a fork per line, then 'coop fleet up'")
	return 0, nil
}

func (a *app) fleetUp(args []string) (int, error) {
	prune, force, err := parseFleetActionFlags("up", args)
	if err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	fleet, err := a.loadFleet(repo)
	if err != nil {
		return -1, err
	}
	for _, e := range fleet {
		tasks := e.tasks // fleet paths are repo-relative; make them absolute for the fork
		if !filepath.IsAbs(tasks) {
			tasks = filepath.Join(repo, tasks)
		}
		if code, err := a.cmdFork([]string{e.name, e.agent, "--loop", "-d", "--tasks", tasks}); err != nil {
			ui.Error("fleet: %s failed: %v", e.name, err)
			return code, err
		}
	}
	ui.Info("fleet up: %d fork(s) detached — coop fork ls · coop fork logs -f", len(fleet))
	if prune {
		if err := a.pruneFleet(repo, force); err != nil {
			return -1, err
		}
	}
	return 0, nil
}

func (a *app) fleetDown(args []string) (int, error) {
	prune, force, err := parseFleetActionFlags("down", args)
	if err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	fleet, err := a.loadFleet(repo)
	if err != nil {
		return -1, err
	}
	stopped := 0
	for _, e := range fleet {
		if forkRunningPid(repo, e.name) != 0 {
			if _, err := a.forkStop([]string{e.name}); err == nil {
				stopped++
			}
		}
	}
	ui.Info("fleet down: stopped %d", stopped)
	if prune {
		if err := a.pruneFleet(repo, force); err != nil {
			return -1, err
		}
	}
	return 0, nil
}

// parseFleetActionFlags parses the optional --prune (and --force, which applies to that prune)
// on `coop fleet up`/`down`. cmd is "up"/"down", for the usage message.
func parseFleetActionFlags(cmd string, args []string) (prune, force bool, err error) {
	for _, x := range args {
		switch x {
		case "--prune":
			prune = true
		case "--force", "-f":
			force = true
		default:
			return false, false, fmt.Errorf("coop fleet %s: unknown flag %q (usage: coop fleet %s [--prune [--force]])", cmd, x, cmd)
		}
	}
	if force && !prune {
		return false, false, fmt.Errorf("coop fleet %s: --force only applies with --prune", cmd)
	}
	return prune, force, nil
}

// fleetOrphans returns the forks not named in the fleet — the cleanup candidates for prune.
func fleetOrphans(fleetNames, forkNames []string) []string {
	inFleet := make(map[string]bool, len(fleetNames))
	for _, n := range fleetNames {
		inFleet[n] = true
	}
	var orphans []string
	for _, n := range forkNames {
		if !inFleet[n] {
			orphans = append(orphans, n)
		}
	}
	return orphans
}

// fleetPrune is `coop fleet prune [--force]` — the cleanup for after you edit .agent/fleet.
func (a *app) fleetPrune(args []string) (int, error) {
	force := false
	for _, x := range args {
		switch x {
		case "--force", "-f":
			force = true
		default:
			return 2, fmt.Errorf("coop fleet prune: unknown flag %q (usage: coop fleet prune [--force])", x)
		}
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := a.pruneFleet(repo, force); err != nil {
		return -1, err
	}
	return 0, nil
}

// pruneFleet removes forks no longer listed in .agent/fleet. It honors the same guard as `coop
// fork rm`: a fork with uncommitted or unmerged work is kept unless force, and a running fork is
// always kept (stop it first), so the safe path can never silently drop an agent's work. Shared
// by `coop fleet prune` and the --prune flag on `coop fleet up`/`down`.
func (a *app) pruneFleet(repo string, force bool) error {
	fleet, err := a.loadFleet(repo) // need the fleet file to know which forks to keep
	if err != nil {
		return err
	}
	names := make([]string, len(fleet))
	for i, e := range fleet {
		names[i] = e.name
	}
	orphans := fleetOrphans(names, forkNames(repo))
	if len(orphans) == 0 {
		ui.Info("nothing to prune — every fork is in .agent/fleet")
		return nil
	}
	removed, kept := 0, 0
	for _, n := range orphans {
		if forkRunningPid(repo, n) != 0 {
			ui.Info("kept %s — still running (coop fork stop %s first)", n, n)
			kept++
			continue
		}
		ws := forkWorkspace(repo, n)
		if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
			ui.Info("kept %s — %s", n, err)
			kept++
			continue
		}
		if err := destroyFork(repo, n); err != nil {
			ui.Error("prune %s: %v", n, err)
			kept++
			continue
		}
		ui.Info("removed %s", n)
		removed++
	}
	if kept > 0 {
		ui.Info("pruned %d fork(s), kept %d", removed, kept)
	} else {
		ui.Info("pruned %d fork(s)", removed)
	}
	return nil
}

// fleetSplit mechanically round-robins the unchecked items in .agent/TASKS.md into
// per-fork slices (.agent/TASKS.<name>.md) and writes a matching .agent/fleet that
// names each slice's path explicitly. It is a DUMB split — for semantic slicing, have
// an agent partition the queue. Forks come from .agent/fleet (preserving its agents),
// or from `coop fleet split <n>` (slice1..N, all claude).
func (a *app) fleetSplit(args []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	type target struct{ name, agent string }
	var targets []target
	if len(args) >= 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 {
			return 2, errors.New("usage: coop fleet split <n>")
		}
		for i := 1; i <= n; i++ {
			targets = append(targets, target{"slice" + strconv.Itoa(i), agents.Default()})
		}
	} else if fleet, err := a.loadFleet(repo); err == nil {
		for _, e := range fleet {
			targets = append(targets, target{e.name, e.agent})
		}
	}
	if len(targets) == 0 {
		return 2, errors.New("usage: coop fleet split <n>   (or define .agent/fleet first)")
	}
	data, err := os.ReadFile(filepath.Join(repo, ".agent", "TASKS.md"))
	if err != nil {
		return -1, errors.New("no .agent/TASKS.md — run 'coop init'")
	}
	// Slice whole task blocks (title + five-part body), not bare title lines, so each
	// fork's queue stays self-contained. Shares the anchored splitter with `coop tasks`.
	buckets := splitOpenTaskBlocks(string(data), len(targets))
	empty := true
	for _, b := range buckets {
		if len(b) > 0 {
			empty = false
		}
	}
	if empty {
		ui.Info("no unchecked [ ] items to split")
		return 0, nil
	}
	var fleetLines []string
	for i, t := range targets {
		if len(buckets[i]) == 0 {
			continue
		}
		rel := filepath.Join(".agent", "TASKS."+t.name+".md")
		body := fmt.Sprintf("# %s — slice for fork %s\n\n%s\n", rel, t.name, strings.Join(buckets[i], "\n\n"))
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
			return -1, err
		}
		ui.Info("wrote %s (%d items)", rel, len(buckets[i]))
		fleetLines = append(fleetLines, fmt.Sprintf("%s %s %s", t.name, t.agent, rel))
	}
	// Write .agent/fleet so its config shows each fork's explicit tasks path. Don't
	// clobber a hand-authored fleet (it already carries the agents/paths you chose) —
	// print the lines to reconcile instead.
	if !fileExists(fleetFile(repo)) {
		header := "# coop fleet — one fork per line: <name> [agent] <tasks-path>\n"
		out := header + strings.Join(fleetLines, "\n") + "\n"
		if err := os.WriteFile(fleetFile(repo), []byte(out), 0o644); err != nil {
			return -1, err
		}
		ui.Info("wrote .agent/fleet — review the slices, then 'coop fleet up'")
		return 0, nil
	}
	ui.Info("mechanical round-robin split — .agent/fleet exists, so reconcile these lines:")
	for _, l := range fleetLines {
		fmt.Printf("  %s\n", l)
	}
	return 0, nil
}
