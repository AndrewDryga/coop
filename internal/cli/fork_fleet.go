package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
	"gopkg.in/yaml.v3"
)

// fleetEntry is one fork in the declarative fleet: a name, the agent to run it, the tasks tree
// that seeds its loop, and optionally the single account it pins (so a fleet can put each fork
// on its own credential instead of all contending for the same first one), the model it runs
// (so e.g. a risky fork gets the big model and a chore fork a cheap one), the orchestration
// preset it runs under, and whether its loop iterations may consult the authed peers (the
// orchestrator pattern, headless). agent may be empty when a preset supplies the lead. A fork
// takes one model/credential; a full rotation ladder lives in a preset. Per-fork
// model/credential/consult override the preset for that fork only.
type fleetEntry struct {
	name       string
	agent      string
	tasks      string
	model      string // --model one-off (may be model@account); a full ladder → use preset
	credential string // --credential one-off (account)
	preset     string
	consult    bool
}

// fleetFile is the PRE-V3 one-line-per-fork fleet (.agent/fleet). It is never read —
// its presence only produces a migrate-and-delete error, so a stale file can't silently
// diverge from .agent/fleet.yaml (the one fleet format).
func fleetFile(repo string) string { return filepath.Join(repo, ".agent", "fleet") }

// fleetYAMLFile is the declarative fleet: .agent/fleet.yaml, a `forks:` map of fork
// name → {tasks, agent, preset, credential, model, consult}.
func fleetYAMLFile(repo string) string { return filepath.Join(repo, ".agent", "fleet.yaml") }

// fleetForkYAML is one fork's YAML shape. Tasks is required; agent defaults to the
// preset's lead when preset is set, else the default agent. Model/credential/consult
// override the preset for this fork only.
type fleetForkYAML struct {
	Agent      string `yaml:"agent"`
	Tasks      string `yaml:"tasks"`
	Preset     string `yaml:"preset"`
	Model      string `yaml:"model"`      // one-off model (may be model@account); a ladder → preset
	Credential string `yaml:"credential"` // one-off account
	Consult    bool   `yaml:"consult"`
}

// parseFleetYAML parses .agent/fleet.yaml preserving the author's fork order (a plain
// map decode would randomize it, and `fleet up` starts forks in file order). Unknown
// fields, duplicate names, and every invalid value fail with the fork named.
func parseFleetYAML(data string) ([]fleetEntry, error) {
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
	var out []fleetEntry
	seen := map[string]bool{}
	for i := 0; i+1 < len(doc.Forks.Content); i += 2 {
		name := doc.Forks.Content[i].Value
		// Node.Decode doesn't honor KnownFields, so reject unknown per-fork keys explicitly —
		// a typo'd key (or the pre-v3 profile= spelling) must error, not silently drop.
		if node := doc.Forks.Content[i+1]; node.Kind == yaml.MappingNode {
			for k := 0; k+1 < len(node.Content); k += 2 {
				switch key := node.Content[k].Value; key {
				case "agent", "tasks", "preset", "model", "credential", "consult":
				default:
					return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: unknown key %q (known: agent, tasks, preset, model, credential, consult)", name, key)
				}
			}
		}
		var f fleetForkYAML
		if err := doc.Forks.Content[i+1].Decode(&f); err != nil {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: %v", name, err)
		}
		e := fleetEntry{name: name, agent: f.Agent, tasks: f.Tasks, model: f.Model, credential: f.Credential, preset: f.Preset, consult: f.Consult}
		if !validForkName(e.name) {
			return nil, fmt.Errorf(".agent/fleet.yaml: invalid fork name %q", e.name)
		}
		if seen[e.name] {
			return nil, fmt.Errorf(".agent/fleet.yaml: duplicate fork name %q — each fork shares one workspace/branch, so a name can appear once", e.name)
		}
		seen[e.name] = true
		if e.tasks == "" {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q needs tasks: <path> (the task tree that seeds its loop)", e.name)
		}
		if e.agent != "" && !agents.Valid(e.agent) {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: unknown agent %q (use %s)", e.name, e.agent, strings.Join(agents.Names(), ", "))
		}
		if e.agent == "" && e.preset == "" {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q needs an agent: or a preset: (no implicit default)", e.name)
		}
		out = append(out, e)
	}
	return out, nil
}

// composeTarget builds a fork's positional target (provider[:model][@account]) from a fleet
// entry's separate agent/model/credential fields — the bridge from the fleet's still-separate
// YAML keys (Task B folds them into agent:) to the one target grammar the fork CLI now takes.
// model may itself carry an @account (the one-off spelling); giving BOTH that and a credential,
// with different accounts, is a contradiction and errors.
func composeTarget(agent, model, credential string) (string, error) {
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
	if acct != "" {
		t += "@" + acct
	}
	return t, nil
}

func (a *app) loadFleet(repo string) ([]fleetEntry, error) {
	// The pre-v3 one-line .agent/fleet is never read — even alongside a fleet.yaml, its
	// presence is an error, so a stale copy can't sit there quietly diverging.
	if fileExists(fleetFile(repo)) {
		return nil, errors.New(".agent/fleet is the pre-v3 fleet format and is no longer read — translate each line into a forks: entry in .agent/fleet.yaml (see MIGRATING.md), then delete .agent/fleet")
	}
	data, err := os.ReadFile(fleetYAMLFile(repo))
	if err != nil {
		return nil, errors.New("no .agent/fleet.yaml — run 'coop fleet init' to scaffold one")
	}
	return parseFleetYAML(string(data))
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
	case "watch":
		if err := rejectArgs("fleet watch", args[1:]); err != nil {
			return 2, err
		}
		return a.fleetWatch()
	case "prune":
		return a.fleetPrune(args[1:])
	case "ls":
		// A fleet is its forks — there's no fleet-level listing. Point at the two real views instead of
		// a bare "unknown command" (rule: `ls` is the list verb, so it must lead somewhere useful).
		return 2, fmt.Errorf("coop fleet has no %q — list the forks with `coop fork ls`, or watch the live board with `coop fleet watch`", sub)
	default:
		return 2, unknownErr("fleet command", sub, []string{"init", "up", "down", "watch", "prune"})
	}
}

// fleetTemplate seeds .agent/fleet.yaml with a documented, ready-to-edit format.
const fleetTemplate = `# coop fleet — a declarative set of fork loops. Start it with: coop fleet up
#
# Each fork under forks: needs tasks: (the task tree that seeds its loop, relative to
# the repo). Everything else is optional:
#   agent:        claude, codex, or gemini (defaults to the preset's lead, else claude)
#   preset:       an orchestration preset from .agent/presets/<name>/ (its lead + models
#                 ladder drive the fork — see 'coop help presets'). A full model/account
#                 fallback ladder lives in the preset, not here.
#   model:        a one-off model for this fork (see 'coop models'); may pin an account as
#                 model@account (e.g. gemini-3.5-flash@work). Overrides the preset.
#   credential:   pin this fork's account. Give each fork a DIFFERENT one so they run in
#                 parallel instead of contending for the same rate limit.
#   consult:      true — iterations may ask the other signed-in agents for a read-only
#                 second opinion (mounts their credentials into this fork's boxes).
#
# Example:
# forks:
#   core:
#     tasks: .agent/tasks.core
#     preset: frontier
#   chores:
#     agent: gemini
#     tasks: .agent/tasks.chores
#     model: gemini-3.5-flash@work

forks: {}
`

// fleetInit writes a documented .agent/fleet.yaml template so you can declare a fleet
// without remembering the format. It never clobbers an existing fleet, in either format.
func (a *app) fleetInit() (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if fileExists(fleetFile(repo)) {
		return 1, errors.New(".agent/fleet is the pre-v3 format and is no longer read — translate it into .agent/fleet.yaml (see MIGRATING.md), then delete it")
	}
	path := fleetYAMLFile(repo)
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
		if e.agent == "" {
			// The preset supplies the lead; a per-fork model/credential has nowhere to attach (a
			// preset's lead takes no override) — refuse here, not silently in a worker's log.
			if e.model != "" || e.credential != "" {
				return 2, fmt.Errorf("fleet up: fork %q sets model/credential but no agent — a preset's lead takes no per-fork override; name the agent (agent: <provider>:<model>@<account>) or make a variant preset", e.name)
			}
			continue // preset-only fork; forkCreate validates the lead after resolving it
		}
		if e.credential != "" && !box.ProfileAuthed(a.cfg, e.agent, e.credential) {
			unsigned = append(unsigned, fmt.Sprintf("%s/%s %q", e.name, e.agent, e.credential))
		}
	}
	if len(unsigned) > 0 {
		return 2, fmt.Errorf("fleet up: these accounts aren't signed in: %s — run: coop login <provider>@<account>", strings.Join(unsigned, ", "))
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
		forkArgs := []string{e.name}
		if e.agent != "" { // empty = the fork's preset supplies the lead agent
			tgt, terr := composeTarget(e.agent, e.model, e.credential)
			if terr != nil {
				return 2, fleetAbortErr(e.name, terr, started)
			}
			forkArgs = append(forkArgs, tgt) // the fork positional is one target: provider[:model][@account]
		}
		forkArgs = append(forkArgs, "--loop", "-d", "--tasks", tasks)
		if e.preset != "" {
			forkArgs = append(forkArgs, "--preset", e.preset)
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
			ui.Info("note: fork %s is running but not in .agent/fleet.yaml — stop it with: coop fork stop %s", n, n)
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

// pruneFleet removes forks no longer listed in .agent/fleet.yaml. It honors the same guard as `coop
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
		ui.Note("nothing to prune — every fork is in .agent/fleet.yaml")
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
