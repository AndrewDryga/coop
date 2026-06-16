package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
		e := fleetEntry{name: f[0], agent: "claude"}
		rest := f[1:]
		// An optional agent token may precede the required tasks path.
		if len(rest) > 0 {
			switch rest[0] {
			case "claude", "codex", "gemini":
				e.agent = rest[0]
				rest = rest[1:]
			}
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
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "up":
		return a.fleetUp()
	case "down":
		return a.fleetDown()
	case "ls":
		return a.forkLs(nil)
	case "split":
		return a.fleetSplit(args[1:])
	default:
		return 2, errors.New("usage: coop fleet up|ls|down|split")
	}
}

func (a *app) fleetUp() (int, error) {
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
	return 0, nil
}

func (a *app) fleetDown() (int, error) {
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
	return 0, nil
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
			targets = append(targets, target{"slice" + strconv.Itoa(i), "claude"})
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
	var todos []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "[ ]") {
			todos = append(todos, line)
		}
	}
	if len(todos) == 0 {
		ui.Info("no unchecked [ ] items to split")
		return 0, nil
	}
	buckets := make([][]string, len(targets))
	for i, t := range todos {
		buckets[i%len(targets)] = append(buckets[i%len(targets)], t)
	}
	var fleetLines []string
	for i, t := range targets {
		if len(buckets[i]) == 0 {
			continue
		}
		rel := filepath.Join(".agent", "TASKS."+t.name+".md")
		body := fmt.Sprintf("# %s — slice for fork %s\n\n%s\n", rel, t.name, strings.Join(buckets[i], "\n"))
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
