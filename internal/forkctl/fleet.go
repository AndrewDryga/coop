package forkctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
	"gopkg.in/yaml.v3"
)

// FleetEntry is one fork in the declarative fleet: a name, the who-runs it loops under, and the
// tasks tree that seeds its loop. The who is a TARGET (provider[:model][/effort][@account] — so a
// fleet can put each fork on its own model/account instead of all contending for the same first
// one) OR a PRESET NAME (its lead + ladder drive the fork). ParseFleetYAML classifies the agent:
// key into exactly one of these two fields. A fork takes ONE account (no @a,b ladder — a full
// rotation lives in a preset).
type FleetEntry struct {
	Name   string
	Agent  string // a target: provider[:model][/effort][@account] (empty ⇒ preset drives the fork)
	Tasks  string
	Preset string // a preset name (set when agent: named a preset instead of a target)
}

// FleetYAMLFile is the declarative fleet: .agent/fleet.yaml, a `forks:` map of fork
// name → {tasks, agent}.
func FleetYAMLFile(repo string) string { return filepath.Join(repo, ".agent", "fleet.yaml") }

// fleetForkYAML is one fork's YAML shape. Tasks is required; agent is a target
// (provider[:model][/effort][@account]) OR a preset name (ParseFleetYAML classifies it).
type fleetForkYAML struct {
	Agent string `yaml:"agent"` // a target OR a preset name
	Tasks string `yaml:"tasks"`
}

// ParseFleetYAML parses .agent/fleet.yaml preserving the author's fork order (a plain
// map decode would randomize it, and `fleet up` starts forks in file order). Unknown
// fields, duplicate names, and every invalid value fail with the fork named.
func ParseFleetYAML(data string) ([]FleetEntry, error) {
	var doc struct {
		Forks yaml.Node `yaml:"forks"`
	}
	dec := yaml.NewDecoder(strings.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf(".agent/fleet.yaml: malformed YAML: %v", err)
	}
	if doc.Forks.Kind == 0 || doc.Forks.IsZero() {
		return nil, errors.New(".agent/fleet.yaml: a top-level `forks:` map is required")
	}
	if doc.Forks.Kind != yaml.MappingNode {
		return nil, errors.New(".agent/fleet.yaml: `forks:` must be a map of fork name → settings")
	}
	var out []FleetEntry
	seen := map[string]bool{}
	for i := 0; i+1 < len(doc.Forks.Content); i += 2 {
		name := doc.Forks.Content[i].Value
		// Node.Decode doesn't honor KnownFields, so reject unknown per-fork keys explicitly —
		// a typo'd key must error, not silently drop.
		if node := doc.Forks.Content[i+1]; node.Kind == yaml.MappingNode {
			for k := 0; k+1 < len(node.Content); k += 2 {
				switch key := node.Content[k].Value; key {
				case "agent", "tasks":
				default:
					return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: unknown key %q (known: agent, tasks)", name, key)
				}
			}
		}
		var f fleetForkYAML
		if err := doc.Forks.Content[i+1].Decode(&f); err != nil {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: %v", name, err)
		}
		e := FleetEntry{Name: name, Agent: f.Agent, Tasks: f.Tasks}
		if !forkspace.ValidName(e.Name) {
			return nil, fmt.Errorf(".agent/fleet.yaml: invalid fork name %q", e.Name)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf(".agent/fleet.yaml: duplicate fork name %q — each fork shares one workspace/branch, so a name can appear once", e.Name)
		}
		seen[e.Name] = true
		if e.Tasks == "" {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q needs tasks: <path> (the task tree that seeds its loop)", e.Name)
		}
		if e.Agent != "" {
			if isTargetHead(e.Agent) {
				// agent: is a target; a fork takes ONE account (a >1 @a,b ladder is a preset's job).
				t, terr := agents.ParseTarget(e.Agent)
				if terr != nil {
					return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: agent: %v", e.Name, terr)
				}
				if len(t.Accounts) > 1 {
					return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: agent %q pins an account ladder — a fork takes one account (put a rotation in a preset)", e.Name, e.Agent)
				}
			} else {
				// agent: is a preset name (not a target) — its lead + ladder drive the fork.
				e.Preset, e.Agent = e.Agent, ""
			}
		}
		if e.Agent == "" && e.Preset == "" {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q needs agent: (a target or a preset name)", e.Name)
		}
		out = append(out, e)
	}
	return out, nil
}

// composeTarget rebuilds a positional target (provider[:model][/effort][@account]) from the pieces a
// fork parsed out of one — used by DetachForkLoop to forward the fork's agent+model+account to
// its re-exec'd worker as a single token. model may itself carry an @account (a contradiction
// with a separate account is rejected).
func composeTarget(agent, model, effort, credential string) (string, error) {
	modelPart, acctInModel, hasAt := strings.Cut(model, "@")
	acct := credential
	if hasAt && acctInModel != "" {
		if credential != "" && credential != acctInModel {
			return "", fmt.Errorf("account set twice: model %q pins @%s but credential is %q", model, acctInModel, credential)
		}
		acct = acctInModel
	}
	t := agent
	if modelPart != "" {
		t += ":" + modelPart
	}
	if effort != "" {
		t += "/" + effort
	}
	if acct != "" {
		t += "@" + acct
	}
	return t, nil
}

func (c *Control) LoadFleet(repo string) ([]FleetEntry, error) {
	data, err := os.ReadFile(FleetYAMLFile(repo))
	if err != nil {
		return nil, errors.New("no .agent/fleet.yaml — run 'coop fleet init' to scaffold one")
	}
	return ParseFleetYAML(string(data))
}

// fleetTemplate seeds .agent/fleet.yaml with a documented, ready-to-edit format.
const fleetTemplate = `# coop fleet — a declarative set of fork loops.
#
# Start it with:  coop fleet up
#
# Each fork listed under 'forks:' gets its own clone, branch, and loop. Two
# fields, both required:
#
#   tasks:    the task tree that seeds this fork's loop, relative to the repo —
#             for example .agent/tasks.core
#
#   agent:    who runs — a TARGET (provider[:model][/effort][@account]) OR a
#             PRESET NAME:
#               claude, codex:gpt-5.5, gemini:gemini-3.5-flash@work   (targets)
#               frontier                                              (a preset)
#             A target puts the fork on that model/account; a preset's lead +
#             ladder drive it. (See 'coop models', 'coop credentials', and
#             'coop help presets'.)
#
#             A fork takes ONE account, so give each fork a DIFFERENT one and
#             they won't contend for the same rate limit. A full rotation ladder
#             belongs in a preset, not here.
#
# Example — two forks, one on a preset, one on a pinned model:
#
#         forks:
#           core:
#             tasks: .agent/tasks.core
#             agent: frontier
#
#           chores:
#             agent: gemini:gemini-3.5-flash@work
#             tasks: .agent/tasks.chores

forks: {}
`

// FleetInit writes a documented .agent/fleet.yaml template so you can declare a fleet
// without remembering the format. It never clobbers an existing fleet.
func (c *Control) FleetInit() (int, error) {
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	path := FleetYAMLFile(repo)
	if fileExists(path) {
		return 1, errors.New(".agent/fleet.yaml already exists — edit it, or remove it to start over")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return -1, err
	}
	if err := os.WriteFile(path, []byte(fleetTemplate), 0o644); err != nil {
		return -1, err
	}
	ui.OK("wrote .agent/fleet.yaml — add your forks under forks:, then 'coop fleet up'")
	return 0, nil
}

func UnsignedFleetAccounts(cfg *config.Config, fleet []FleetEntry) []string {
	var unsigned []string
	for _, e := range fleet {
		if e.Agent == "" {
			continue // preset-only fork; the launch path validates the lead after resolving it
		}
		// agent: parsed clean in ParseFleetYAML; check its pinned account is signed in (fail loud
		// here, not silently in a worker's log). No account → the loop rotates all signed-in ones.
		t, _ := agents.ParseTarget(e.Agent)
		if len(t.Accounts) == 1 && !box.ProfileAuthed(cfg, t.Provider, t.Accounts[0]) {
			unsigned = append(unsigned, fmt.Sprintf("%s/%s %q", e.Name, t.Provider, t.Accounts[0]))
		}
	}
	return unsigned
}

func (c *Control) FleetDown(args []string) (int, error) {
	prune, force, yes, err := ParseFleetActionFlags("down", args)
	if err != nil {
		return 2, err
	}
	if prune && !yes && !ui.IsTerminal(os.Stdin) {
		return 2, errors.New("refusing to prune forks without confirmation — re-run with --yes (no terminal to prompt)")
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	fleet, err := c.LoadFleet(repo)
	if err != nil {
		return -1, err
	}
	names := make([]string, len(fleet))
	stopped := 0
	var stopErrs []error
	for i, e := range fleet {
		names[i] = e.Name
		// A pidfile may be a live worker OR a cleanup marker left by a failed exact-label reap.
		// Retry either state; silently skipping the marker would strand the fork's box forever.
		if pathExists(forkspace.PidPath(repo, e.Name)) {
			if _, err := c.ForkStop([]string{e.Name}); err == nil {
				stopped++
			} else if !pathExists(forkspace.PidPath(repo, e.Name)) {
				// The worker exited and cleared its pidfile between our check and ForkStop's lock.
				continue
			} else {
				stopErrs = append(stopErrs, fmt.Errorf("%s: %w", e.Name, err))
			}
		}
	}
	if len(stopErrs) > 0 {
		return 1, fmt.Errorf("fleet down stopped %s but failed to stop %s — fix each reported cause, then retry: coop fleet down: %w", ui.Count(stopped, "fork"), ui.Count(len(stopErrs), "fork"), errors.Join(stopErrs...))
	}
	ui.OK("stopped %s", ui.Count(stopped, "fork"))
	// `down` only stops forks listed in .agent/fleet — surface a running fork that isn't (removed
	// from the file, or started by hand) rather than leave it silently running.
	for _, n := range fleetOrphans(names, forkspace.LifecycleNames(repo)) {
		if forkspace.NeedsStop(repo, n) {
			ui.Info("note: fork %s is running or awaiting cleanup but not in .agent/fleet.yaml — stop it with: coop fork stop %s", n, n)
		}
	}
	if prune {
		if code, err := c.PruneFleet(repo, force, yes); err != nil {
			return code, err
		}
	}
	return 0, nil
}

// ParseFleetActionFlags parses optional prune flags on `coop fleet up`/`down`. --force overrides
// the work guard; --yes confirms deletion; both apply only with --prune. cmd identifies the caller
// for the usage message.
func ParseFleetActionFlags(cmd string, args []string) (prune, force, yes bool, err error) {
	for _, x := range args {
		switch x {
		case "--prune":
			prune = true
		case "--force", "-f":
			force = true
		case "--yes", "-y":
			yes = true
		default:
			return false, false, false, fmt.Errorf("coop fleet %s: unknown flag %q (usage: coop fleet %s [--prune [--force] [--yes]])", cmd, x, cmd)
		}
	}
	if (force || yes) && !prune {
		return false, false, false, fmt.Errorf("coop fleet %s: --force/--yes only apply with --prune", cmd)
	}
	return prune, force, yes, nil
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

// FleetPrune is `coop fleet prune [--force] [--yes]` — cleanup after editing .agent/fleet.
func (c *Control) FleetPrune(args []string) (int, error) {
	force, yes := false, false
	for _, x := range args {
		switch x {
		case "--force", "-f":
			force = true
		case "--yes", "-y":
			yes = true
		default:
			return 2, fmt.Errorf("coop fleet prune: unknown flag %q (usage: coop fleet prune [--force] [--yes])", x)
		}
	}
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	return c.PruneFleet(repo, force, yes)
}

// PruneFleet removes forks no longer listed in .agent/fleet.yaml. It honors the same guard as `coop
// fork rm`: a fork with uncommitted or unmerged work is kept unless force, and a running fork is
// always kept (stop it first), so the safe path can never silently drop an agent's work. Shared
// by `coop fleet prune` and the --prune flag on `coop fleet up`/`down`.
func (c *Control) PruneFleet(repo string, force, yes bool) (int, error) {
	fleet, err := c.LoadFleet(repo) // need the fleet file to know which forks to keep
	if err != nil {
		return -1, err
	}
	names := make([]string, len(fleet))
	for i, e := range fleet {
		names[i] = e.Name
	}
	orphans := fleetOrphans(names, forkspace.LifecycleNames(repo))
	if len(orphans) == 0 {
		ui.Note("nothing to prune — every fork is in .agent/fleet.yaml")
		return 0, nil
	}
	kept := 0
	type pruneCandidate struct {
		name string
		file *os.File
		info os.FileInfo
	}
	var candidates []pruneCandidate
	defer func() {
		for _, candidate := range candidates {
			_ = candidate.file.Close()
		}
	}()
	for _, n := range orphans {
		if forkspace.NeedsStop(repo, n) {
			ui.Warn("kept %s — still running or awaiting cleanup (coop fork stop %s first)", n, n)
			kept++
			continue
		}
		ws := forkspace.Workspace(repo, n)
		handle, info, err := forkspace.Pin(ws)
		if err != nil {
			ui.Warn("kept %s — workspace changed while selecting it for prune", n)
			kept++
			continue
		}
		if err := ForkRmSafe(ForkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
			_ = handle.Close()
			ui.Warn("kept %s — %s", n, err)
			kept++
			continue
		}
		candidates = append(candidates, pruneCandidate{name: n, file: handle, info: info})
	}
	if len(candidates) == 0 {
		ui.OK("pruned 0 forks, kept %d", kept)
		return 0, nil
	}
	candidateNames := make([]string, len(candidates))
	for i, candidate := range candidates {
		candidateNames[i] = candidate.name
	}
	if err := ui.DestroyGate("delete pruned forks: "+strings.Join(candidateNames, ", "), yes); err != nil {
		return 2, err
	}
	removed := 0
	for _, candidate := range candidates {
		n := candidate.name
		unlock, err := forkspace.LockState(repo, n)
		if err != nil {
			ui.Error("prune %s: lock state: %v", n, err)
			kept++
			continue
		}
		ws := forkspace.Workspace(repo, n)
		if !forkspace.SamePinned(ws, candidate.info) || forkspace.NeedsStop(repo, n) {
			ui.Warn("kept %s — changed while awaiting confirmation", n)
			kept++
			unlock()
			continue
		}
		if err := ForkRmSafe(ForkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
			ui.Warn("kept %s — changed while awaiting confirmation: %s", n, err)
			kept++
			unlock()
			continue
		}
		if err := DestroyFork(c.rt, repo, n); err != nil {
			ui.Error("prune %s: %v", n, err)
			kept++
			unlock()
			continue
		}
		unlock()
		ui.OK("removed %s", n)
		removed++
	}
	if kept > 0 {
		ui.OK("pruned %s, kept %d", ui.Count(removed, "fork"), kept)
	} else {
		ui.OK("pruned %s", ui.Count(removed, "fork"))
	}
	return 0, nil
}
