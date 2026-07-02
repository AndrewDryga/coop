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

// fleetEntry is one fork in the declarative fleet: a name, the agent to run it, the tasks tree
// that seeds its loop, and optionally the credential profile(s) its loop rotates (so a fleet can
// put each fork on its own account instead of all contending for the repo pool's first profile),
// the model it runs (so e.g. a risky fork gets the big model and a chore fork a cheap one), and
// whether its loop iterations may consult the authed peers (the orchestrator pattern, headless).
type fleetEntry struct {
	name     string
	agent    string
	tasks    string
	profiles []string
	model    string
	consult  bool
}

// fleetLineShape is the one-line grammar shown in fleet parse errors.
const fleetLineShape = "<name> [agent] <tasks-path> [profile=a,b] [model=m] [consult=1]"

// fleetFile is the declarative fleet: .agent/fleet, one fork per line as
// "<name> [agent] <tasks-path>" (agent defaults to claude; the tasks path is required
// and is relative to the repo root). Blank and `#` lines are ignored.
func fleetFile(repo string) string { return filepath.Join(repo, ".agent", "fleet") }

func parseFleet(data string) ([]fleetEntry, error) {
	var out []fleetEntry
	seen := map[string]bool{}
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
			return nil, fmt.Errorf("fleet: %q needs a tasks path — %q", e.name, fleetLineShape)
		}
		e.tasks = rest[0]
		// Any further tokens are key=value options (currently only profile=). A bare extra token is a
		// misspelled middle agent (so the real path got mis-read) or a space in the path — rejecting
		// it turns a baffling later "no such file" into a clear error at parse time.
		for _, tok := range rest[1:] {
			key, val, ok := strings.Cut(tok, "=")
			if !ok {
				return nil, fmt.Errorf("fleet: %q — unexpected token %q; expected %q (a middle token must be a known agent %s; the path can't contain spaces; extra options are key=value)",
					e.name, tok, fleetLineShape, strings.Join(agents.Names(), ", "))
			}
			switch key {
			case "profile":
				if e.profiles = parseProfileList(val); len(e.profiles) == 0 {
					return nil, fmt.Errorf("fleet: %q — profile= needs a name or comma-separated list", e.name)
				}
			case "model":
				if e.model = strings.TrimSpace(val); e.model == "" {
					return nil, fmt.Errorf("fleet: %q — model= needs a model name", e.name)
				}
			case "consult":
				// Explicit both ways — a typo'd value must error, not silently mean off.
				switch strings.ToLower(strings.TrimSpace(val)) {
				case "1", "true", "yes", "on":
					e.consult = true
				case "0", "false", "no", "off":
					e.consult = false
				default:
					return nil, fmt.Errorf("fleet: %q — consult= takes 1/0 (or true/false, on/off), got %q", e.name, val)
				}
			default:
				return nil, fmt.Errorf("fleet: %q — unknown option %q (known: profile=, model=, consult=)", e.name, key)
			}
		}
		if !validForkName(e.name) {
			return nil, fmt.Errorf("fleet: invalid fork name %q", e.name)
		}
		if seen[e.name] {
			return nil, fmt.Errorf("fleet: duplicate fork name %q — each fork shares one workspace/branch, so a name can appear once", e.name)
		}
		seen[e.name] = true
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
	case "":
		return groupHelp("fleet") // bare `coop fleet` shows help, not an error (see rule)
	case "init":
		return a.fleetInit()
	case "up":
		return a.fleetUp(args[1:])
	case "down":
		return a.fleetDown(args[1:])
	case "split":
		return a.fleetSplit(args[1:])
	case "watch":
		if err := rejectArgs("fleet watch", args[1:]); err != nil {
			return 2, err
		}
		return a.fleetWatch()
	case "prune":
		return a.fleetPrune(args[1:])
	default:
		// `ls` lives on `coop fork ls`, not here.
		return 2, unknownErr("fleet command", sub, []string{"init", "up", "down", "split", "watch", "prune"})
	}
}

// fleetTemplate seeds .agent/fleet with a documented, ready-to-edit format.
const fleetTemplate = `# coop fleet — one fork per line:  <name> [agent] <tasks-path> [profile=a,b] [model=m] [consult=1]
#   <name>        the fork's name (also its git branch)
#   [agent]       claude (default), codex, or gemini
#   <tasks-path>  the task tree that seeds the fork's loop (a dir, relative to the repo)
#   profile=a,b   optional: the credential profile(s) this fork's loop uses (rotated on a
#                 rate limit). Give each fork a DIFFERENT account so they run in parallel
#                 instead of all contending for the same one. Omit to share the repo pool.
#   model=m       optional: the model this fork runs (see 'coop models'). Omit for the
#                 profile's marked default / COOP_LOOP_MODEL / the agent's own default.
#   consult=1     optional: iterations may ask the other signed-in agents for a read-only
#                 second opinion (mounts their credentials into this fork's boxes).
# Blank lines and #-comments are ignored.  Start the fleet with: coop fleet up
#
# Example:
# api    codex   .agent/tasks.api   profile=work       model=gpt-5-codex
# deps   gemini  .agent/tasks.deps  profile=personal,backup
# core   claude  .agent/tasks.core  model=claude-fable-5  consult=1
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
	ui.OK("wrote .agent/fleet — add a fork per line, then 'coop fleet up'")
	return 0, nil
}

// fleetAbortErr formats the error when `fleet up` fails fast partway through. Failing fast (over a
// silent partial fleet) is the intended behavior — but when forks already started, the error must
// be loud about it and name the cleanup, so a half-started fleet isn't discovered hours later.
func fleetAbortErr(name string, err error, started int) error {
	if started > 0 {
		return fmt.Errorf("fleet up: %q failed to start (%w) — aborted with %d fork(s) already running; stop them with 'coop fleet down' (or inspect via 'coop fork ls')", name, err, started)
	}
	return fmt.Errorf("fleet up: %q failed to start: %w", name, err)
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
	// Validate per-fork profiles up front, so a typo fails loud here instead of silently in a
	// detached worker's log. (A fork with no profile= falls back to the repo pool / all signed-in.)
	var unsigned []string
	for _, e := range fleet {
		for _, p := range e.profiles {
			if !box.ProfileAuthed(a.cfg, e.agent, p) {
				unsigned = append(unsigned, fmt.Sprintf("%s/%s %q", e.name, e.agent, p))
			}
		}
	}
	if len(unsigned) > 0 {
		return 2, fmt.Errorf("fleet up: these profiles aren't signed in: %s — run: coop login <agent> --profile <name>", strings.Join(unsigned, ", "))
	}
	started := 0
	for _, e := range fleet {
		if pid := forkRunningPid(repo, e.name); pid != 0 {
			ui.Note("fork %s already running (pid %d) — skipping", e.name, pid)
			continue // idempotent: re-running `fleet up` leaves live loops alone
		}
		tasks := e.tasks // fleet paths are repo-relative; make them absolute for the fork
		if !filepath.IsAbs(tasks) {
			tasks = filepath.Join(repo, tasks)
		}
		forkArgs := []string{e.name, e.agent, "--loop", "-d", "--tasks", tasks}
		if len(e.profiles) > 0 {
			forkArgs = append(forkArgs, "--profile", strings.Join(e.profiles, ","))
		}
		if e.model != "" {
			forkArgs = append(forkArgs, "--model", e.model)
		}
		if e.consult {
			forkArgs = append(forkArgs, "--consult")
		}
		if code, err := a.cmdFork(forkArgs); err != nil {
			return code, fleetAbortErr(e.name, err, started)
		}
		started++
	}
	ui.OK("%s detached — coop fork ls · coop fork logs -f", ui.Count(started, "fork"))
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
	names := make([]string, len(fleet))
	stopped := 0
	for i, e := range fleet {
		names[i] = e.name
		if forkRunningPid(repo, e.name) != 0 {
			if _, err := a.forkStop([]string{e.name}); err == nil {
				stopped++
			}
		}
	}
	ui.OK("stopped %s", ui.Count(stopped, "fork"))
	// `down` only stops forks listed in .agent/fleet — surface a running fork that isn't (removed
	// from the file, or started by hand) rather than leave it silently running.
	for _, n := range fleetOrphans(names, forkNames(repo)) {
		if forkRunningPid(repo, n) != 0 {
			ui.Info("note: fork %s is running but not in .agent/fleet — stop it with: coop fork stop %s", n, n)
		}
	}
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
		ui.Note("nothing to prune — every fork is in .agent/fleet")
		return nil
	}
	removed, kept := 0, 0
	for _, n := range orphans {
		if forkRunningPid(repo, n) != 0 {
			ui.Warn("kept %s — still running (coop fork stop %s first)", n, n)
			kept++
			continue
		}
		ws := forkWorkspace(repo, n)
		if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
			ui.Warn("kept %s — %s", n, err)
			kept++
			continue
		}
		if err := destroyFork(repo, n); err != nil {
			ui.Error("prune %s: %v", n, err)
			kept++
			continue
		}
		ui.OK("removed %s", n)
		removed++
	}
	if kept > 0 {
		ui.OK("pruned %s, kept %d", ui.Count(removed, "fork"), kept)
	} else {
		ui.OK("pruned %s", ui.Count(removed, "fork"))
	}
	return nil
}

// fleetSplit mechanically round-robins the todo task folders in .agent/tasks into per-fork
// task trees (.agent/tasks.<name>) and writes a matching .agent/fleet naming each slice's
// path. It is a DUMB split — for semantic slicing, have an agent partition the queue. Forks
// come from .agent/fleet (preserving its agents), or from `coop fleet split <n>` (slice1..N).
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
	// Round-robin the todo task folders into per-fork task trees (.agent/tasks.<name>).
	root := filepath.Join(repo, tasksRoot)
	names, agts := make([]string, len(targets)), make([]string, len(targets))
	for i, t := range targets {
		names[i], agts[i] = t.name, t.agent
	}
	return a.fleetSplitFolders(repo, root, names, agts)
}

// fleetSplitFolders round-robins the todo task folders into per-fork task trees
// (.agent/tasks.<name>, copies) and
// writes .agent/fleet pointing each fork at its slice directory (a dir → folder mode).
func (a *app) fleetSplitFolders(repo, root string, names, agts []string) (int, error) {
	written, counts, total, err := splitTodoFolders(repo, root, names)
	if err != nil {
		return -1, err
	}
	if total == 0 {
		ui.Note("no todo tasks to split")
		return 0, nil
	}
	var fleetLines []string
	for i := range names {
		if written[i] == "" {
			continue
		}
		ui.Note("wrote %s (%s)", written[i], ui.Count(counts[i], "task"))
		fleetLines = append(fleetLines, fmt.Sprintf("%s %s %s", names[i], agts[i], written[i]))
	}
	// Don't clobber a hand-authored .agent/fleet — print the lines to reconcile instead.
	if !fileExists(fleetFile(repo)) {
		header := "# coop fleet — one fork per line: <name> [agent] <tasks-path>\n"
		out := header + strings.Join(fleetLines, "\n") + "\n"
		if err := os.WriteFile(fleetFile(repo), []byte(out), 0o644); err != nil {
			return -1, err
		}
		ui.OK("wrote .agent/fleet — review the slices, then 'coop fleet up'")
		return 0, nil
	}
	ui.Note(".agent/fleet already exists — add these lines to it yourself:")
	for _, l := range fleetLines {
		fmt.Printf("  %s\n", l)
	}
	return 0, nil
}
