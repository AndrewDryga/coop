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

// fleetEntry is one fork in the declarative fleet: a name and the model to run it.
type fleetEntry struct {
	name  string
	agent string
}

// fleetFile is the declarative fleet: .agent/fleet, one fork per line as
// "<name> [agent]" (agent defaults to claude). Blank and `#` lines are ignored.
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
		if len(f) > 1 {
			switch f[1] {
			case "claude", "codex", "gemini":
				e.agent = f[1]
			default:
				return nil, fmt.Errorf("fleet: unknown agent %q for %q", f[1], f[0])
			}
		}
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
		if code, err := a.cmdFork([]string{e.name, e.agent, "--loop", "-d"}); err != nil {
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
// per-fork slices (.agent/TASKS.<name>.md). It is a DUMB split — for semantic
// slicing, have an agent partition the queue. Names come from .agent/fleet, or from
// `coop fleet split <n>` (slice1..N).
func (a *app) fleetSplit(args []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	var names []string
	if len(args) >= 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 {
			return 2, errors.New("usage: coop fleet split <n>")
		}
		for i := 1; i <= n; i++ {
			names = append(names, "slice"+strconv.Itoa(i))
		}
	} else if fleet, err := a.loadFleet(repo); err == nil {
		for _, e := range fleet {
			names = append(names, e.name)
		}
	}
	if len(names) == 0 {
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
	buckets := make([][]string, len(names))
	for i, t := range todos {
		buckets[i%len(names)] = append(buckets[i%len(names)], t)
	}
	for i, name := range names {
		if len(buckets[i]) == 0 {
			continue
		}
		slice := filepath.Join(repo, ".agent", "TASKS."+name+".md")
		body := fmt.Sprintf("# .agent/TASKS.%s.md — slice for fork %s\n\n%s\n", name, name, strings.Join(buckets[i], "\n"))
		if err := os.WriteFile(slice, []byte(body), 0o644); err != nil {
			return -1, err
		}
		ui.Info("wrote %s (%d items)", slice, len(buckets[i]))
	}
	ui.Info("mechanical round-robin split — review the slices, then 'coop fleet up'")
	return 0, nil
}
