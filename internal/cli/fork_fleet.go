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
	"gopkg.in/yaml.v3"
)

// fleetEntry is one fork in the declarative fleet: a name, the agent to run it, the tasks tree
// that seeds its loop, and optionally the credential(s) its loop rotates (so a fleet can put
// each fork on its own account instead of all contending for the repo pool's first one), the
// model it runs (so e.g. a risky fork gets the big model and a chore fork a cheap one), the
// orchestration preset it runs under, and whether its loop iterations may consult the authed
// peers (the orchestrator pattern, headless). agent may be empty when a preset supplies the
// lead. Per-fork credentials/model/consult override the preset for that fork only.
type fleetEntry struct {
	name     string
	agent    string
	tasks    string
	profiles []string
	model    string
	preset   string
	consult  bool
}

// fleetLineShape is the one-line grammar shown in legacy fleet parse errors.
const fleetLineShape = "<name> [agent] <tasks-path> [profile=a,b] [model=m] [consult=1]"

// fleetFile is the LEGACY one-line-per-fork fleet (.agent/fleet), read only as a
// compatibility path; fleetYAMLFile is the primary format.
func fleetFile(repo string) string { return filepath.Join(repo, ".agent", "fleet") }

// fleetYAMLFile is the declarative fleet: .agent/fleet.yaml, a `forks:` map of fork
// name → {tasks, agent, preset, credentials, model, consult}.
func fleetYAMLFile(repo string) string { return filepath.Join(repo, ".agent", "fleet.yaml") }

// fleetForkYAML is one fork's YAML shape. Tasks is required; agent defaults to the
// preset's lead when preset is set, else the default agent. Credentials/model/consult
// override the preset for this fork only.
type fleetForkYAML struct {
	Agent       string           `yaml:"agent"`
	Tasks       string           `yaml:"tasks"`
	Preset      string           `yaml:"preset"`
	Credentials []credentialYAML `yaml:"credentials"`
	Model       string           `yaml:"model"`
	Consult     bool             `yaml:"consult"`
}

// credentialYAML is one credential target in a fleet fork's credentials: list — a plain
// name ("work", or the compact "work@opus"), or the structured form
// {name: work, model: opus} for a model fallback member. Both normalize to the pool's
// credential[@model] wire form.
type credentialYAML struct {
	Name  string
	Model string
}

func (c *credentialYAML) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		t := parsePoolTarget(n.Value)
		c.Name, c.Model = t.credential, t.model
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("a credential is a name (\"work\", \"work@opus\") or {name: work, model: opus}")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch key := n.Content[i].Value; key {
		case "name", "model":
		default:
			return fmt.Errorf("credential: unknown key %q (known: name, model)", key)
		}
	}
	var m struct {
		Name  string `yaml:"name"`
		Model string `yaml:"model"`
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	c.Name, c.Model = m.Name, m.Model
	return nil
}

// wire renders the target in the pool's credential[@model] member form.
func (c credentialYAML) wire() string {
	return poolTarget{credential: c.Name, model: c.Model}.String()
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
		// a typo'd key (or the legacy profile= spelling) must error, not silently drop.
		if node := doc.Forks.Content[i+1]; node.Kind == yaml.MappingNode {
			for k := 0; k+1 < len(node.Content); k += 2 {
				switch key := node.Content[k].Value; key {
				case "agent", "tasks", "preset", "credentials", "model", "consult":
				default:
					return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: unknown key %q (known: agent, tasks, preset, credentials, model, consult)", name, key)
				}
			}
		}
		var f fleetForkYAML
		if err := doc.Forks.Content[i+1].Decode(&f); err != nil {
			return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: %v", name, err)
		}
		e := fleetEntry{name: name, agent: f.Agent, tasks: f.Tasks, model: f.Model, preset: f.Preset, consult: f.Consult}
		for _, c := range f.Credentials {
			e.profiles = append(e.profiles, c.wire())
		}
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
			e.agent = agents.Default() // no preset to supply a lead — same default as the legacy format
		}
		for _, c := range e.profiles {
			if parsePoolTarget(c).credential == "" {
				return nil, fmt.Errorf(".agent/fleet.yaml: fork %q: credentials has an empty name", e.name)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

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
	yamlData, yamlErr := os.ReadFile(fleetYAMLFile(repo))
	legacyData, legacyErr := os.ReadFile(fleetFile(repo))
	switch {
	case yamlErr == nil && legacyErr == nil:
		// Two sources of truth would silently diverge — refuse until one is gone.
		return nil, errors.New("both .agent/fleet.yaml and the legacy .agent/fleet exist — keep fleet.yaml and delete .agent/fleet (its lines translate to forks: entries)")
	case yamlErr == nil:
		return parseFleetYAML(string(yamlData))
	case legacyErr == nil:
		ui.Info("note: reading the legacy .agent/fleet — migrate to .agent/fleet.yaml (same data, YAML shape; see 'coop fleet init')")
		return parseFleet(string(legacyData))
	default:
		return nil, errors.New("no .agent/fleet.yaml — run 'coop fleet init' to scaffold one")
	}
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
	case "ls":
		// A fleet is its forks — there's no fleet-level listing. Point at the two real views instead of
		// a bare "unknown command" (rule: `ls` is the list verb, so it must lead somewhere useful).
		return 2, fmt.Errorf("coop fleet has no %q — list the forks with `coop fork ls`, or watch the live board with `coop fleet watch`", sub)
	default:
		return 2, unknownErr("fleet command", sub, []string{"init", "up", "down", "split", "watch", "prune"})
	}
}

// fleetTemplate seeds .agent/fleet.yaml with a documented, ready-to-edit format.
const fleetTemplate = `# coop fleet — a declarative set of fork loops. Start it with: coop fleet up
#
# Each fork under forks: needs tasks: (the task tree that seeds its loop, relative to
# the repo). Everything else is optional:
#   agent:        claude, codex, or gemini (defaults to the preset's lead, else claude)
#   preset:       an orchestration preset from .agent/presets/<name>/ (lead, roles,
#                 models, credentials — see 'coop help presets')
#   credentials:  the credential(s) this fork's loop rotates on a rate limit. Give each
#                 fork a DIFFERENT account so they run in parallel instead of contending.
#                 A member may carry a model for same-account fallback — "work@opus" or
#                 {name: work, model: opus} — tried in order before the next account.
#                 Overrides the preset's lead credentials for this fork.
#   model:        the model this fork runs (see 'coop models'); overrides the preset.
#   consult:      true — iterations may ask the other signed-in agents for a read-only
#                 second opinion (mounts their credentials into this fork's boxes).
#
# Example:
# forks:
#   core:
#     tasks: .agent/tasks.core
#     preset: frontier
#     credentials: [work]
#   chores:
#     agent: gemini
#     tasks: .agent/tasks.chores
#     model: gemini-3.5-flash
#     credentials: [work, backup]

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
		return 1, errors.New("a legacy .agent/fleet already exists — migrate it to .agent/fleet.yaml (same data, YAML shape), then delete it")
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
			continue // the fork's preset supplies the lead; forkCreate validates after resolving it
		}
		for _, p := range e.profiles {
			if cred := parsePoolTarget(p).credential; !box.ProfileAuthed(a.cfg, e.agent, cred) {
				unsigned = append(unsigned, fmt.Sprintf("%s/%s %q", e.name, e.agent, cred))
			}
		}
	}
	if len(unsigned) > 0 {
		return 2, fmt.Errorf("fleet up: these credentials aren't signed in: %s — run: coop login <agent> --credential <name>", strings.Join(unsigned, ", "))
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
			forkArgs = append(forkArgs, e.agent)
		}
		forkArgs = append(forkArgs, "--loop", "-d", "--tasks", tasks)
		if e.preset != "" {
			forkArgs = append(forkArgs, "--preset", e.preset)
		}
		if len(e.profiles) > 0 {
			forkArgs = append(forkArgs, "--credential", strings.Join(e.profiles, ","))
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
	var forkBlocks []string
	for i := range names {
		if written[i] == "" {
			continue
		}
		ui.Note("wrote %s (%s)", written[i], ui.Count(counts[i], "task"))
		forkBlocks = append(forkBlocks, fmt.Sprintf("  %s:\n    agent: %s\n    tasks: %s", names[i], agts[i], written[i]))
	}
	// Don't clobber a hand-authored fleet (either format) — print the entries to reconcile instead.
	if !fileExists(fleetYAMLFile(repo)) && !fileExists(fleetFile(repo)) {
		out := "# coop fleet — generated by 'coop fleet split'; see 'coop fleet init' for the format\nforks:\n" + strings.Join(forkBlocks, "\n") + "\n"
		if err := os.WriteFile(fleetYAMLFile(repo), []byte(out), 0o644); err != nil {
			return -1, err
		}
		ui.OK("wrote .agent/fleet.yaml — review the slices, then 'coop fleet up'")
		return 0, nil
	}
	ui.Note("a fleet file already exists — add these forks to it yourself:")
	for _, b := range forkBlocks {
		fmt.Println(b)
	}
	return 0, nil
}
