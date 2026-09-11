package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// `coop init` — the first thing a person runs, and the one command whose whole job is to leave a
// project in a state its owner recognises. Everything it writes is no-clobber, every question has
// a safe default, and nothing it says claims a step it did not take.

// scaffoldableAgents are the agents with a per-agent dir `coop init` can scaffold (grok reads the
// root AGENTS.md, no dir of its own).
var scaffoldableAgents = []string{"claude", "codex", "gemini"}

// initInput is the narrow test seam ui.confirmInput already establishes for the same problem: a
// first-run question can only be answered from the terminal coop detected, and no test has one.
// nil — which it always is outside this package's own tests — means the real path, os.Stdin gated
// by IsTerminal, so an interactive person's behavior is unchanged. It is the only way the complete
// first-run transcripts are reachable as byte-exact fixtures.
var initInput io.Reader

// askTerminal returns the reader for init's questions, and false when there is nobody to ask.
func askTerminal() (*bufio.Scanner, bool) {
	if initInput != nil {
		return bufio.NewScanner(initInput), true
	}
	if !ui.IsTerminal(os.Stdin) {
		return nil, false
	}
	// ONE reader for every first-run question: a second bufio.Scanner over the same stdin buffers
	// past its own line and eats the next prompt's answer.
	return bufio.NewScanner(os.Stdin), true
}

// listOption describes a closed-list option completely enough both to VALIDATE a value and to
// EXPLAIN a rejection — the accepted tokens, the sentinel that must stand alone, the owning command
// and one representative example. The refusals below are that data handed to the shared renderer,
// not per-option sentences.
type listOption struct {
	flag     string   // "--agents"
	command  string   // the full owning command path, "coop init"
	example  string   // a valid invocation to show when the value was missing or wrong
	valid    []string // the tokens the parser really accepts
	sentinel string   // a value that must be used alone ("all", "none")
	expand   []string // what the sentinel selects
}

// parse normalizes and de-duplicates a comma/space-separated value against the option's closed
// list. The sentinel must stand alone and maps to expand ("none" → nil; "all" → every agent).
func (o listOption) parse(value string) ([]string, error) {
	tokens := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return r == ',' || r == ' ' })
	if slices.Contains(tokens, o.sentinel) {
		if len(tokens) != 1 {
			return nil, ui.StandaloneValue(o.sentinel, o.flag, o.command)
		}
		return slices.Clone(o.expand), nil
	}
	var out []string
	for _, token := range tokens {
		if !slices.Contains(o.valid, token) {
			return nil, ui.InvalidOptionValue(token, o.flag, o.command, o.choices(), o.example)
		}
		if !slices.Contains(out, token) {
			out = append(out, token)
		}
	}
	return out, nil
}

// choices is the cause line for a rejected value: the tokens the parser really accepts, read as a
// sentence, so the reader does not have to guess the spelling of the one they wanted.
func (o listOption) choices() string {
	return "Choose " + ui.List(append(slices.Clone(o.valid), o.sentinel), "or") + "."
}

// initServices and initAgents are `coop init`'s closed-list options, and initOptions the full set a
// correction may suggest — one description each, read by both the parser and its refusals.
var (
	initServices = listOption{
		flag: "--services", command: "coop init", example: "coop init --services postgres,redis",
		valid: scaffold.ComposeServices, sentinel: "none",
	}
	initAgents = listOption{
		flag: "--agents", command: "coop init", example: "coop init --agents claude,codex",
		valid: scaffoldableAgents, sentinel: "all", expand: scaffoldableAgents,
	}
	initOptions = []string{"--stack", "--services", "--agents"}
)

// scaffoldAgentSet resolves the implicit per-agent dirs from signed-in agents. Empty means
// .agent/ only; a box synthesizes a missing agent's skills from the shared source on demand.
func scaffoldAgentSet(cfg *config.Config) []string {
	var out []string
	for _, name := range box.AuthedAgents(cfg) {
		if slices.Contains(scaffoldableAgents, name) && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func (a *app) cmdInit(args []string) (int, error) {
	stack := ""
	var services []string
	servicesSet, servicesChooser := false, false
	var agentDirs []string
	agentsSet := false
	for i := 0; i < len(args); i++ {
		// A bare --services opens the chooser; --services= is a missing value, not a chooser.
		// The two have to stay distinguishable, so this flag is read before the shared parser.
		if args[i] == "--services" && (i+1 >= len(args) || strings.HasPrefix(args[i+1], "-")) {
			servicesSet, servicesChooser = true, true
			continue
		}
		if v, n, ok, e := flagValue(args, i, "--stack"); ok {
			if e != nil {
				return 2, ui.MissingOptionValue("--stack", "coop init", "coop init --stack node")
			}
			stack = v
			i += n - 1
			continue
		}
		if v, n, ok, e := flagValue(args, i, "--services"); ok {
			if e != nil {
				return 2, ui.MissingOptionValue("--services", "coop init", "coop init --services postgres")
			}
			services, e = initServices.parse(v)
			if e != nil {
				return 2, e
			}
			servicesSet = true
			i += n - 1
			continue
		}
		if v, n, ok, e := flagValue(args, i, "--agents"); ok {
			if e != nil { // the list is required: say so with a valid example, not a bare "needs a value"
				return 2, ui.MissingOptionValue(initAgents.flag, initAgents.command, initAgents.example)
			}
			agentDirs, e = initAgents.parse(v)
			if e != nil {
				return 2, e
			}
			agentsSet = true
			i += n - 1
			continue
		}
		// An unknown token is a typo — error before doing any scaffold work, rather than
		// silently ignoring it and acting as if a flag were never passed.
		return 2, unknownOptionErr(args[i], "coop init", initOptions)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// `coop init --services` on its own is a request to ADD services to a project that already
	// exists. It writes nothing else, asks nothing else, and starts nothing.
	if servicesSet && stack == "" && !agentsSet && scaffold.Initialized(repo) {
		return a.addServices(repo, services, servicesChooser)
	}
	// A re-init asks nothing. Every scaffold write is no-clobber, so on an already-initialized repo
	// an answer to either prompt below could not take effect — and `coop init` is run from inside a
	// repo often enough (after a coop upgrade, or just from a subdirectory) that interrogating the
	// user each time is pure friction. --services / --stack still work explicitly.
	already := scaffold.Initialized(repo)
	// A --stack coop can't honor is refused before it says a word or asks a question: nothing is
	// worse than answering three prompts and then being told the flag was wrong.
	if err := scaffold.CheckStack(repo, stack); err != nil {
		return 1, reported("Could not use --stack "+stack, sentence(firstLine(err)),
			"Add the tool versions this project needs, then run coop init --stack "+stack+".")
	}
	// Detect the repo's stack(s) for the commit gate; if nothing's detected and we're at a
	// terminal, ask rather than guess — coop never imposes a check the repo doesn't use.
	langs := scaffold.DetectStacks(repo)
	if !already {
		ui.Note("Setting up Coop")
	}
	if ask, ok := askTerminal(); !already && ok {
		// Git first, and only with an explicit yes: the tracked hooks below need a repo to point
		// core.hooksPath at, and a piped/declining run must never have one created behind its back.
		if !pathExists(filepath.Join(repo, ".git")) {
			if err := askGitInit(ask, repo); err != nil {
				return 1, err
			}
		}
		if len(langs) == 0 {
			langs = promptGateLangs(ask)
		}
		// Sibling services (db/redis) are opt-in — coop doesn't add a compose file a project may
		// not want. Ask at a terminal unless --services already said.
		if !servicesSet {
			services = promptServices(ask)
		}
	}
	// Without an explicit --agents list, scaffold dirs for the signed-in agents. Others aren't
	// clutter you delete later — a box synthesizes a missing agent's skills from the repo's shared
	// source on demand.
	if !agentsSet {
		agentDirs = scaffoldAgentSet(a.cfg)
	}
	notices, err := scaffold.Init(repo, stack, langs, agentDirs)
	if err != nil {
		return 1, setupFailure(repo, err)
	}
	if err := scaffold.WriteCompose(repo, services); err != nil {
		return 1, setupFailure(repo, err)
	}
	if err := a.writeMCPStub(); err != nil {
		return 1, setupFailure(repo, err)
	}
	// Monorepo: detect member dirs (each with a .agent/), record them in the root .agent/project.yaml
	// so coop aggregates their task queues, and give each member a project.yaml if it lacks one. A
	// single repo still gets a project.yaml template. Never clobbers an existing file.
	subs := scaffold.DetectSubprojects(repo)
	if _, err := scaffold.WriteProject(repo, subs); err != nil {
		return 1, setupFailure(repo, err)
	}
	var registered, unregistered []string
	for _, s := range subs {
		// Members get only the minimal set — their own task queue (plus a backlog drawer on demand)
		// — since they share the root's AGENTS.md, skills, rules, hooks, box, and its single
		// top-level project.yaml. A member never gets a project.yaml of its own.
		if err := scaffold.InitSubproject(repo, filepath.Join(repo, filepath.FromSlash(s))); err != nil {
			return 1, setupFailure(repo, err)
		}
	}
	if len(subs) > 0 {
		// A re-init keeps an existing project.yaml, so newly-added members were previously only
		// REPORTED and you had to list them by hand. Register them instead — an unlisted member is
		// a queue coop silently ignores, which is never what you wanted when you created it.
		registered, err = scaffold.RegisterSubprojects(repo, subs)
		if err != nil {
			return 1, registrationFailure(err)
		}
		// Only if the edit couldn't be placed (a hand-restructured project.yaml) does the advisory
		// remain — coop never silently drops a member on the floor.
		if pj, err := project.Load(repo); err == nil {
			for _, s := range subs {
				if !slices.Contains(pj.Subprojects, s) {
					unregistered = append(unregistered, s)
				}
			}
		}
	}
	// One result, not a ledger: what this project now is, what a box may reach, and only the
	// actions its ACTUAL state still needs.
	if already {
		ui.OK("Coop project updated")
		ui.Note("  %s — existing files kept, anything missing added", repo)
		if len(registered) > 0 {
			ui.Note("  Task queues registered: %s", strings.Join(registered, ", "))
		}
	} else {
		// One blank line between the questions (or the header, when nobody was asked) and the
		// result, so a first run reads as two paragraphs rather than one wall.
		ui.Note("")
		ui.OK("Coop project created")
		ui.Note("  %s", initSharedLine(agentDirs))
		if len(langs) > 0 {
			ui.Note("  Formatting checked before every commit: %s", strings.Join(langs, ", "))
		}
		if len(services) > 0 {
			ui.Note("  Services for agents to use: %s", strings.Join(services, ", "))
		}
		// Say once what a filtered box can reach, in the terms a newcomer arrives with: their
		// agents keep working, everything else waits for them. New project files select filtered.
		ui.Note("")
		ui.Note("Network access is filtered.")
		if reach := initProviderLine(agentDirs); reach != "" {
			ui.Note("%s", reach)
		}
		ui.Note("Other network traffic is blocked until you approve rules allowing it.")
	}
	if len(unregistered) > 0 {
		ui.Note("")
		warnBlock(fmt.Sprintf("%s still need to be registered", countWord(len(unregistered), "task queue")),
			project.File+" uses a subprojects list Coop could not update safely.",
			fmt.Sprintf("Add %s to subprojects in %s.", joinAnd(unregistered), project.File))
	}
	for _, notice := range notices {
		ui.Note("")
		warnBlock(notice.Headline, notice.Reason, notice.Action)
	}
	reportDockerSetup(repo)
	for _, g := range initActions(a.cfg, repo, services, agentDirs, !already) {
		g.print()
	}
	a.netPendingNotice(repo) // only when this project asks for network access nobody approved
	return 0, nil
}

// registrationFailure reports a member registration that could not finish. A project.yaml that
// kept moving under the edit has its own remedy — wait for the other editor — while anything else
// (an unreadable file, a failed write) is the ordinary partial-setup failure, and saying "kept
// changing" about a permissions error would send the reader nowhere.
func registrationFailure(err error) error {
	if !errors.Is(err, scaffold.ErrProjectChanged) {
		return setupFailure("", err)
	}
	return reported("Could not finish Coop setup",
		project.File+" kept changing while Coop registered task queues.",
		"Run coop init again after the other edit finishes.",
		"Files already created have been kept.")
}

// setupFailure reports a scaffold that stopped partway. The writes it already made are real files
// on disk, so it says so: claiming a rollback that did not happen is worse than the partial state.
func setupFailure(repo string, err error) error {
	// An entry that is already something else is not a permissions problem, and telling someone
	// to chmod their way out of a symlink sends them to the wrong fix.
	fix := "Fix the path or permissions, then run coop init again."
	if errors.Is(err, scaffold.ErrNotRegular) {
		fix = "Fix the path, then run coop init again."
	}
	return reported("Could not finish Coop setup", sentence(pathReason(repo, "create", err)),
		fix, "Files already created have been kept.")
}

// addServices is `coop init --services` on an initialized project: an additive edit to the
// Compose file it already has. It never overwrites a configuration, never renames a service
// somebody else declared, and never starts anything — `coop up` is a separate decision.
func (a *app) addServices(repo string, services []string, chooser bool) (int, error) {
	if chooser {
		ask, ok := askTerminal()
		if !ok {
			return 2, reported("Choosing services requires a terminal", "",
				"Example: coop init --services postgres,redis",
				"Help:    coop help init")
		}
		ui.Note("Add services")
		services = promptExactTokens(ask, []string{
			"Choose services to run alongside your agents.",
			"Existing services and their data will be kept.",
		}, scaffold.ComposeServices, "Services", unknownServiceBlock)
	}
	p, err := project.Load(repo)
	if err != nil {
		return -1, err
	}
	rel := p.ComposeRel()
	if len(services) == 0 {
		ui.Note("")
		ui.Note("No services added.")
		return 0, nil
	}
	added, err := scaffold.AddComposeServices(repo, rel, services)
	if err != nil {
		var collision *scaffold.ComposeCollision
		if errors.As(err, &collision) {
			return 1, reported(fmt.Sprintf("Could not add %s to %s", collision.Catalog, rel),
				sentence(collision.Error()),
				fmt.Sprintf("Review %s before adding %s.", rel, titleName(collision.Catalog)),
				"No services were changed.")
		}
		return 1, reported("Could not add services to "+rel, sentence(firstLine(err)),
			fmt.Sprintf("Fix %s, then run coop init --services again.", rel),
			"No services were changed.")
	}
	if len(added.Added) == 0 {
		ui.Note("")
		ui.Note("Services are already configured in %s.", rel)
		for _, name := range added.Existing {
			ui.Note("  %s → %s", name, scaffold.ServiceName(name))
		}
		return 0, nil
	}
	ui.Note("")
	// The names follow on their own lines, so a count in the headline would only restate what the
	// reader is about to read. Plural, not "2 Services added".
	ui.OK("%s added to %s", plural(len(added.Added), "Service"), rel)
	for _, name := range added.Added {
		ui.Note("  %s → %s", name, scaffold.ServiceName(name))
	}
	for _, name := range added.Existing {
		ui.Note("  %s is already configured as %s.", name, scaffold.ServiceName(name))
	}
	ui.Note("")
	ui.Note("Start services:")
	ui.Note("  %s coop up", ui.Cyan("→"))
	return 0, nil
}

// reportDockerSetup points at the project's own Docker configuration when coop's box is not set
// up yet. It suggests; it never adopts a file an application owns.
func reportDockerSetup(repo string) {
	setup := scaffold.DetectDockerSetup(repo)
	if setup == nil {
		return
	}
	ui.Note("")
	ui.Note("%s", ui.Bold("Use your project's Docker setup"))
	switch {
	case setup.Dockerfile != "" && setup.Compose != "":
		ui.Note("  Dockerfile: %s", setup.Dockerfile)
		ui.Note("  Services:   %s", setup.Compose)
		ui.Note("")
		ui.Note("  Set box.dockerfile or box.compose in %s to use these files.", project.File)
		ui.Note("  Review the files before running coop build or coop up.")
	case setup.Dockerfile != "":
		ui.Note("  Dockerfile: %s", setup.Dockerfile)
		ui.Note("")
		ui.Note("  Set box.dockerfile in %s to use this file.", project.File)
		ui.Note("  Review the file before running coop build.")
	default:
		ui.Note("  Services: %s", setup.Compose)
		ui.Note("")
		ui.Note("  Set box.compose in %s to use this file.", project.File)
		ui.Note("  Review the file before running coop up.")
	}
}

// initAction is one job the user still has to do, with the commands that do it and — when the
// commands alone would not be enough — the one sentence that frames them.
type initAction struct {
	title   string
	note    string
	actions []string
}

// print renders one job: a bold heading, an optional framing line, then each command on a
// cyan-arrow line. It goes through ui rather than straight to the terminal so a live view can
// position it like every other line coop writes.
func (g initAction) print() {
	if len(g.actions) == 0 {
		return
	}
	ui.Note("")
	ui.Note("%s", ui.Bold(g.title))
	if g.note != "" {
		ui.Note("  %s", g.note)
	}
	for _, s := range g.actions {
		ui.Note("  %s %s", ui.Cyan("→"), s)
	}
}

// initActions is what this project actually needs next, derived from real state — no Git repo, no
// signed-in agent, a box image to build, services to start — followed on a first run by the two
// jobs every new project has: prove the sandbox, then start working. A repo that needs none of the
// first four prints only those two.
func initActions(cfg *config.Config, repo string, services, agentDirs []string, fresh bool) []initAction {
	var out []initAction
	// coop runs forks and the loop on top of git (worktrees, rebase-merge), and the commit gate
	// needs core.hooksPath — which only the re-init after `git init` can set. Lead with both.
	if !pathExists(filepath.Join(repo, ".git")) {
		out = append(out, initAction{title: "Finish setup — Coop needs a Git repository", actions: []string{"git init", "coop init"}})
	}
	// The agent to sign in to is also the one the loop line below names, so a first run reads as
	// one story rather than two unrelated suggestions.
	loopTarget := "claude"
	if len(agentDirs) > 0 {
		loopTarget = agentDirs[0]
	}
	if len(box.AuthedAgents(cfg)) == 0 {
		out = append(out, initAction{title: "Sign in to an agent", actions: []string{"coop login " + loopTarget}})
	}
	if dfRel := project.DockerfilePath(repo); fileExists(filepath.Join(repo, dfRel)) {
		out = append(out, initAction{title: "Build the box", note: "Review " + dfRel + ", then run:", actions: []string{"coop build"}})
	}
	if len(services) > 0 {
		out = append(out, initAction{title: "Start " + joinAnd(services), actions: []string{"coop up"}})
	}
	// First-run advice only. On a repo that has been building for weeks it's noise at best.
	if fresh {
		out = append(out,
			initAction{title: "Verify the sandbox", actions: []string{"coop doctor"}},
			// A bare `coop loop` has no target in a fresh project's configuration, so naming one
			// is the difference between a command that works and one that prints a usage error.
			initAction{title: "Start working", actions: []string{`coop tasks add "Describe your first task"`, "coop loop " + loopTarget}},
		)
	}
	return out
}

// plural agrees a noun with a count that is not itself printed — the list under the headline is
// the count, so "2 Services added" would say it twice.
func plural(n int, singular string) string {
	if n == 1 {
		return singular
	}
	return singular + "s"
}

// countWord spells a small count for the start of a sentence — "Two task queues still need…" —
// and falls back to digits past nine, where words stop being easier to read than numerals.
func countWord(n int, noun string) string {
	words := [...]string{"Zero", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine"}
	count := ui.Count(n, noun)
	if n >= 0 && n < len(words) {
		_, rest, _ := strings.Cut(count, " ")
		return words[n] + " " + rest
	}
	return count
}

// initSharedLine says what the selected agents now share. A repo that scaffolded no per-agent dir
// still gets the shared set — a box synthesizes a missing agent's artifacts from it on demand.
func initSharedLine(agentDirs []string) string {
	names := make([]string, 0, len(agentDirs))
	for _, name := range agentDirs {
		names = append(names, titleName(name))
	}
	switch len(names) {
	case 0:
		return "Instructions, skills, and one task queue are ready for any agent."
	case 1:
		return names[0] + " has instructions, skills, and one task queue."
	default:
		return joinAnd(names) + " share instructions, skills, and one task queue."
	}
}

// initProviderLine names, per selected agent, the one vendor a filtered box lets it reach — so the
// sentence fits the project ("Claude can reach Anthropic") instead of listing agents it never uses.
func initProviderLine(agentDirs []string) string {
	var clauses []string
	for _, name := range agentDirs {
		if ag, ok := agents.Get(name); ok {
			clauses = append(clauses, fmt.Sprintf("%s can reach %s", titleName(name), ag.Vendor()))
		}
	}
	if len(clauses) == 0 {
		return ""
	}
	return joinAnd(clauses) + "."
}

// joinAnd renders a list as English prose: "a", "a and b", "a, b, and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0, 1:
		return strings.Join(items, "")
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

// writeMCPStub seeds an empty shared mcp.json — coop's one MCP source of truth, translated to
// each agent — at the global config path if absent, so there's an obvious, correctly-shaped file
// to drop servers into. The validated snapshot boundary treats an empty (no-server) file as inert,
// so the stub changes no run until you add a server. Never clobbers an existing config.
func (a *app) writeMCPStub() error {
	path := a.cfg.MCPFile
	if path == "" {
		return nil
	}
	if fileExists(path) {
		// mcp.json is the GLOBAL shared MCP config, not part of this repo's scaffold — when it already
		// exists `coop init` changed nothing, so say nothing. (A "kept existing mcp.json" line during a
		// fresh repo's init reads as if it were a repo file; the e2e review flagged it as misleading.)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("{\n  \"mcpServers\": {}\n}\n"), 0o600)
}

// askGitInit says in two plain sentences why Coop needs Git, then creates the repository only on
// an explicit yes — before the tracked hooks are installed, so core.hooksPath lands in this same
// run and no hook-activation command is left stranded in the result. A decline, or a closed stdin,
// creates nothing: the result then names `git init` as the action that finishes setup.
func askGitInit(ask *bufio.Scanner, repo string) error {
	ui.Note("")
	ui.Note("This folder is not a Git repository.")
	ui.Note("Coop uses Git for tasks, forks, and commit checks.")
	ui.Note("")
	answer, ok := askOneOK(ask, "Initialize Git here? [Y/n]: ")
	if !ok || !ui.ConfirmationResponse(answer, true) {
		return nil
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		ui.Note("")
		return reported("Could not initialize Git in "+repo, sentence(gitInitCause(string(out), err)+" while creating .git"),
			"Fix the folder permissions, then run coop init again.")
	}
	return nil
}

// gitInitCause is why git could not create the repository, in the words the OS used. git writes
// the offending path first and the OS's reason last ("/…/atlas/.git: Permission denied", "fatal:
// cannot mkdir /…/.git: Permission denied"), so the reason is the tail — and dropping the head
// also keeps a machine-specific absolute path out of a sentence that already names .git.
func gitInitCause(out string, err error) string {
	line := lastLine(out, err)
	if i := strings.LastIndex(line, ": "); i > 0 && strings.ContainsAny(line[:i], "/\\") {
		return line[i+2:]
	}
	return line
}

// lastLine is git's own final complaint, or the exec error when it said nothing — the bounded
// cause, never the whole transcript.
func lastLine(out string, err error) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return err.Error()
	}
	return strings.TrimSpace(out[strings.LastIndex(out, "\n")+1:])
}

// promptServices asks (on a tty) which services the project's agents should have alongside them.
// Blank → none (coop adds no db/redis you didn't ask for).
func promptServices(ask *bufio.Scanner) []string {
	return promptExactTokens(ask, []string{
		"Coop can run services alongside your agents.",
		"Choose any this project needs, or press Enter for none.",
	}, scaffold.ComposeServices, "Services", unknownServiceBlock)
}

// promptGateLangs asks (on a tty) which languages the project will use, when coop couldn't detect
// a stack — the commit format checks follow from the answer. Blank → no formatting check.
func promptGateLangs(ask *bufio.Scanner) []string {
	return promptExactTokens(ask, []string{
		"Coop couldn't detect which languages this project will use.",
		"Choose one or more. Coop will check their formatting before every commit.",
	}, scaffold.GateLangs, "Languages", unknownLanguageLine)
}

// unknownLanguageLine is the approved wording for the first-run language prompt, kept verbatim:
// a reviewed line stays reviewed, and restyling it to match a newer error shape would be a
// silent change to something a person already signed off.
func unknownLanguageLine(unknown string, valid []string) {
	ui.Error("Unknown language “%s”.", unknown)
	ui.Note("  Choose from: %s", strings.Join(valid, ", "))
}

// unknownServiceBlock is the service chooser's rejection, in the ordinary outcome-block shape.
func unknownServiceBlock(unknown string, valid []string) {
	failBlock(fmt.Sprintf("Unknown service %q", unknown), "Choose from: "+strings.Join(valid, ", "))
}

// promptExactTokens asks for a space-separated subset of valid and keeps asking until every token
// is one of them. The menu lists ONLY accepted tokens: a second word in a choice row (the formatter
// a language implies, say) reads as selectable even though the parser would reject it. An unknown
// answer is NAMED and the question repeats — silently dropping half an answer leaves the user
// believing they chose something they didn't. Blank means none; input order is kept, duplicates
// dropped. EOF means none too, so a closed stdin can never hang the prompt.
func promptExactTokens(ask *bufio.Scanner, intro, valid []string, label string, reject func(unknown string, valid []string)) []string {
	ui.Note("")
	for _, line := range intro {
		ui.Note("%s", line)
	}
	ui.Note("")
	for _, v := range valid {
		ui.Note("  %s", v)
	}
	question := fmt.Sprintf("\n%s (space-separated, or press Enter for none): ", label)
	for {
		line, ok := askOneOK(ask, question)
		if !ok {
			return nil
		}
		var chosen []string
		unknown := ""
		for _, tok := range strings.Fields(strings.ToLower(line)) {
			switch {
			case !slices.Contains(valid, tok):
				unknown = tok
			case !slices.Contains(chosen, tok):
				chosen = append(chosen, tok)
			}
			if unknown != "" {
				break
			}
		}
		if unknown == "" {
			return chosen
		}
		ui.Note("")
		reject(unknown, valid)
		question = "\n" + label + ": "
	}
}

// askOneOK writes a question to the terminal and reads one answer back. The false it returns on a
// closed stdin is what a repeating prompt needs to stop asking, and what keeps a Ctrl-D from
// reading as the default yes.
func askOneOK(ask *bufio.Scanner, question string) (string, bool) {
	fmt.Fprint(os.Stderr, question)
	if !ask.Scan() {
		return "", false
	}
	return ask.Text(), true
}
