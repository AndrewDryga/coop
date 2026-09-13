// Package cli is the command-line surface: it parses argv, resolves the config
// and runtime, and dispatches to the box engine and scaffolder. The routing:
// bare `coop` prints help (running an agent is explicit), `coop <agent>` runs a
// named agent, known subcommands run their command, and an unrecognized command is
// an error — raw commands run in the box explicitly, via `coop run -- <cmd>`.
package cli

import (
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Version is the tool version, reported by `coop version`. Defaults to "dev" and
// is overridden at build time via -ldflags (GoReleaser and the Makefile).
var Version = "dev"

// resolveVersion returns the -ldflags version if set, otherwise the module
// version embedded by `go install pkg@version`, otherwise "dev".
func resolveVersion() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return Version
}

type app struct {
	cfg                  *config.Config
	rt                   runtime.Runtime
	rtSet                bool                                         // whether rt has been detected yet (ensureRuntime is lazy — see below)
	argv                 []string                                     // the invocation, so a remedy can name the command to repeat
	loginProvider        string                                       // presentation context for the shared sign-in flow, including provider-first aliases
	sweptRepos           map[string]bool                              // repos already swept for orphaned boxes this process (see sweepOrphanBoxes)
	sweptNetworks        bool                                         // orphaned coop networks already swept this process (they are not per repo)
	preset               *preset.Preset                               // the run's loaded preset (from the who-runs slot), carried into each RunSpec (see applyPreset)
	network              networkFlags                                 // this launch's --egress/--allow-domain/--egress-rules, resolved by box.AdmitNetwork
	mode                 agents.ExecutionMode                         // this launch's --readonly/--bare; "" (normal) is every launch that takes neither
	acpCapture           *box.CapturedEgress                          // an ACP supervisor's frozen policy; every child it spawns gets a reference to this one
	acpPeers             []agents.Target                              // exact ad-hoc peers whose accounts a filtered ACP supervisor freezes for its children
	acpNetworkTargets    []agents.Target                              // exact provider/accounts admitted with acpCapture, including optional warm/probe choices
	acpAccountBindings   map[string]acpAccountBinding                 // filtered child-only account/default handoff, checked against its reloaded preset before mounts
	acpResume            *acpctl.ResumeState                          // consumed once before admission; restored by the supervisor afterward
	beforeSignRefUpdate  func(repo, ref, oldHead, newHead string)     // test seam for a concurrent signing ref move
	afterDetachedPublish func()                                       // test seam for state replacement before repeated child validation
	acpModels            func(agent string) ([]agents.Model, error)   // test seam for Claude/Gemini model refresh; nil → a real ACP box
	acpSupervise         func([]string, *acpctl.Control) (int, error) // test seam; nil → the real stdio supervisor
}

// ensureRuntime lazily detects and caches the container runtime the first time a box-running command
// needs it. Pure-local families (tasks, profiles, models, init, check-secrets, fork ls/path, group
// help) never call it, so they work with no runtime installed — Main no longer detects eagerly. The
// error is the same actionable "runtime not found" Main used to surface.
func (a *app) ensureRuntime() error {
	if a.rtSet {
		return nil
	}
	rt, err := runtime.Detect(a.cfg.RuntimeName)
	if err != nil {
		return err
	}
	a.rt, a.rtSet = rt, true
	return nil
}

// Main is the process entry point. It returns the exit code to pass to os.Exit.
func Main(argv []string) int {
	cfg, err := config.Load()
	if err != nil {
		ui.Error("%v", err)
		return 1
	}
	if !detachedWorkerReexec(argv) {
		// Once a day, check for a newer coop in the background and mention it as the command's
		// parting line (deferred, so it runs on every return path). See startUpdateCheck.
		defer startUpdateCheck(cfg, argv)()
		defer forkspace.CloseGitViews() // the trusted git views of every repository this process touched
		// Sweep temp entries no box is using. Per-run cleanup is a deferred call
		// and a killed process skips it, so what supervision and restarts leave
		// behind accumulates until it fills the volume.
		startTempReap(cfg)
	}

	// Bare `coop`, help, and version all work without a container runtime. Bare
	// `coop` prints help rather than launching an agent — running one is explicit
	// (`coop claude`), so a stray `coop` never turns an agent loose on the cwd.
	if len(argv) == 0 {
		printHelp(cfg)
		return 0
	}
	switch argv[0] {
	case "help", "-h", "--help":
		// `coop help <cmd> [<sub>]` shows that command's help — the same page as
		// `coop <cmd> [<sub>] --help`. Bare `coop help` (or -h/--help) is the top-level menu.
		if argv[0] == "help" && len(argv) > 1 {
			if argv[1] == "--all" { // the whole manual: docs/cli.md's bytes (see RenderManual), then this project's presets
				fmt.Print(RenderManual(cfg) + presetManualPages(cfg))
				return 0
			}
			return reportExit(helpForPath(helpPath(argv[1:]), cfg, true))
		}
		printHelp(cfg)
		return 0
	case "version", "-v", "--version":
		if helpRequested(argv[1:]) { // `coop version --help` prints its help, not a self-referential error
			return reportExit(helpForPath([]string{"version"}, cfg, false))
		}
		if err := rejectArgs("version", argv[1:]); err != nil { // reject extras like every no-arg command
			return reportExit(2, err)
		}
		fmt.Println("coop " + resolveVersion())
		return 0
	}

	// `-h`/`--help` (or a bare `help` arg) before any `--` asks for COOP's page for the command
	// path that was typed — including a registered agent, whose own CLI is reached explicitly with
	// `coop <agent> -- --help`. It resolves without a runtime, a login, or the command's own
	// required arguments, because nothing is being run.
	if helpRequested(argv[1:]) || (len(argv) > 1 && argv[1] == "help") {
		return reportExit(helpForPath(helpPath(argv), cfg, false))
	}

	// The runtime is detected lazily (a.ensureRuntime), only by box-running commands — so pure-local
	// families work with no container runtime installed. See dispatch and resolveImage.
	a := &app{cfg: cfg, argv: argv}
	return reportExit(a.dispatch(argv))
}

// reportExit renders a command's error the way its kind deserves and returns the process exit code:
// rejected input gets the shared usage block (see ui.UsageError) and exit 2, a runtime failure drawn
// in that same block (ui.CommandFailed) exits 1 because the input was fine, a launch section that
// already rendered its own failure (ui.Fail) is not repeated, and everything else gets one ✗ line.
func reportExit(code int, err error) int {
	if err != nil {
		usage := asUsage(err)
		switch {
		case usage != nil:
			ui.PrintUsageError(usage)
			if code <= 0 {
				code = usage.ExitCode
				if code == 0 {
					code = 2
				}
			}
		case errors.Is(err, ui.ErrReported):
		default:
			ui.Error("%v", err)
		}
		if code == 0 {
			code = 1
		}
	}
	if code < 0 {
		code = 1
	}
	return code
}

// asUsage is where a failure REPORTED AS DATA becomes the block a person reads. The engines below
// the CLI keep rendering at the edges (internal/importdag_test.go), so they hand up the parts —
// where the problem is, what is wrong, what to change — and the terminal's owner draws them in the
// one shape every refusal uses. Anything that already is a ui.UsageError passes through unchanged.
func asUsage(err error) *ui.UsageError {
	var usage *ui.UsageError
	if errors.As(err, &usage) {
		return usage
	}
	var settings *config.Failure
	if errors.As(err, &settings) {
		cause := settings.Problem
		if settings.Where != "" {
			cause = settings.Where + "\n" + settings.Problem
		}
		var rows [][2]string
		if settings.Action != "" {
			rows = append(rows, [2]string{"", settings.Action})
		}
		return ui.CommandFailed(settings.Headline, cause, rows...)
	}
	var projectFile *project.FileError
	if errors.As(err, &projectFile) {
		return ui.CommandFailed("Could not read "+project.File, projectFile.Cause)
	}
	return nil
}

// helpPath is the command path a help request is ABOUT: the leading plain words of argv, stopping
// at the first option, at `--` (everything after it belongs to the agent), and at two words deep —
// past a family and its subcommand, the rest is arguments, not a deeper page. fork is the one
// family that takes a third word: its commands sit AFTER the fork name (`coop fork login acp`).
func helpPath(argv []string) []string {
	depth := 2
	switch {
	case len(argv) > 0 && argv[0] == "fork":
		depth = 3
	case len(argv) > 0 && argv[0] == "credentials":
		// credentials names an agent AND an account before its verb: `coop help credentials codex
		// personal rm` is four words, and the last one is what picks the page.
		depth = 4
	}
	var path []string
	for _, arg := range argv {
		if len(path) == depth || arg == "--" || arg == "help" || strings.HasPrefix(arg, "-") {
			break
		}
		path = append(path, arg)
	}
	return path
}

// detachedWorkerReexec identifies the private child form before ordinary process housekeeping.
// Its parent already ran that housekeeping, while the child must not mutate anything before it
// proves that its exact launch reservation still owns the fork lifecycle.
func detachedWorkerReexec(argv []string) bool {
	if len(argv) < 2 || argv[0] != "fork" {
		return false
	}
	return slices.ContainsFunc(argv[1:], func(arg string) bool {
		return arg == "--_detached" || strings.HasPrefix(arg, "--_detached=")
	})
}

func (a *app) dispatch(argv []string) (int, error) {
	if len(argv) == 0 { // unreachable (Main intercepts bare coop); defensive
		printHelp(a.cfg)
		return 0, nil
	}
	sub, rest := argv[0], argv[1:]
	// These commands always run a container, so detect the runtime up front (fail fast with the
	// actionable "runtime not found"). The mixed command fork (ls/path are local) and update
	// (--self-only is local) — and every pure-local family detect lazily in their box-running paths
	// (resolveImage, forkStop, mergeGate, cmdUpdate), so they work with no runtime. loop detects
	// after its usage and config validation (cmdLoop), so a bad flag, target, or loop.yaml is
	// reported as such rather than as a missing runtime.
	switch sub {
	case "run", "shell", "login", "acp", "up", "down", "build":
		// A bare launch has no project: it must work outside any repository, so the eager
		// project load is skipped for it (the command still refuses everything else by name).
		if mode, _, err := extractExposureFlags("coop "+sub, rest); err != nil || mode != agents.ModeBare {
			if _, _, err := loadProject(a.cfg.RepoOverride); err != nil {
				return -1, err
			}
		}
		if err := a.ensureRuntime(); err != nil {
			return -1, err
		}
	case "doctor":
		if err := a.ensureRuntime(); err != nil {
			return -1, err
		}
	}
	switch sub {
	case "run":
		return a.cmdRun(rest)
	case "shell":
		rest, err := a.takeNetworkFlags(rest)
		if err != nil {
			return 2, err
		}
		if rest, err = a.takeExposureFlags("coop shell", rest); err != nil {
			return 2, err
		}
		if err := rejectArgs("shell", rest); err != nil {
			return 2, err
		}
		return a.runInBox([]string{a.cfg.Shell}, "", nil)
	case "login":
		return a.cmdLogin(rest)
	case "credentials":
		return a.cmdCredentials(rest)
	case "presets":
		return a.cmdPresets(rest)
	case "models":
		return a.cmdModels(rest)
	case "acp":
		return a.cmdACP(rest)
	case "fork":
		return a.cmdFork(rest)
	case "tasks":
		return a.cmdTasks(rest)
	case "context":
		return a.cmdContext(rest)
	case "backlog":
		return a.cmdBacklog(rest)
	case "loop":
		return a.cmdLoop(rest)
	case "up":
		return a.cmdUp(rest)
	case "down":
		return a.cmdDown(rest)
	case "init":
		return a.cmdInit(rest)
	case "doctor":
		return a.cmdDoctor(rest)
	case "net": // host-wide: restricted networking setup (no project needed)
		return a.cmdNet(rest)
	case "check-secrets":
		return a.cmdCheckSecrets(rest)
	case "build":
		return a.cmdBuild(rest)
	case "update":
		return a.cmdUpdate(rest)
	case "sign": // host-local: re-sign the unpushed range with your host key (no box)
		return a.cmdSign(rest)
	case "prompt": // pure-local: a one-line status for a shell prompt / tmux (no git per fork, no docker)
		return a.cmdPrompt(rest)
	case "sessions": // host-local: the owner-private remote session controller
		return a.cmdSessions(rest)
	case "completion": // pure-local: print a shell completion script
		return cmdCompletion(rest)
	case "__complete": // hidden: dynamic completion candidates for the shell scripts
		return a.cmdComplete(rest)
	default:
		if isTargetHead(sub) { // coop claude|claude:opus|… — run the agent target
			return a.launchAgent(sub, rest)
		}
		// coop <preset> — a bare word that names a preset runs it interactively (its lead is
		// the agent). The command switch above runs FIRST, so a command name is never shadowed
		// by a same-named preset.
		if p, ok, perr := a.presetNamed(sub); ok {
			if perr != nil {
				return 2, perr // the preset exists but is broken — surface the load error
			}
			return a.launchPreset(p, rest)
		}
		// Don't ship an unrecognized command to the box to exec and fail with a cryptic
		// "not found" after a slow toolchain spin-up — a typo'd subcommand should fail
		// fast here. Raw box commands are explicit (`coop run -- <cmd>`).
		return 2, unknownCommandErr(argv[:1], false)
	}
}

// tasksHost builds the real tasks.Host: the command-help catalog and the live-board driver the
// `coop tasks`/`coop backlog` verb family need but cannot own itself (see internal/tasks's Host
// doc) — both cli-wide, not task-specific.
func tasksHost() tasks.Host {
	return tasks.Host{
		GroupHelp:    groupHelp,
		RunWatchLoop: runWatchLoop,
	}
}

// cmdTasks and cmdBacklog are thin `*app` shims: internal/tasks owns the folder-mode verb family
// as plain functions (it has no `*app` of its own to be a method on), so cli's dispatch table calls
// through these rather than the package directly.
func (a *app) cmdTasks(args []string) (int, error) {
	return tasks.CmdTasks(tasksHost(), a.cfg, args)
}

func (a *app) cmdBacklog(args []string) (int, error) {
	return tasks.CmdBacklog(a.cfg, args)
}

// topLevelCommands is coop's own subcommands: the correction candidates for a mistyped one, the
// completion menu, and the manual's coverage list. Keep in sync with the dispatch switch above.
var topLevelCommands = []string{
	"run", "shell", "login", "credentials", "presets", "models", "acp", "fork", "tasks", "context", "backlog",
	"loop", "up", "down", "init", "doctor", "net", "check-secrets", "sign", "build", "update", "completion", "prompt", "sessions", "help", "version",
}

// helpForPath prints the page for one command PATH, so `coop help tasks add` and
// `coop tasks add --help` reach the SAME page: fork's family help, run's page, a static
// commandHelp entry, a registered agent's page (its own CLI stays behind `coop <agent> -- --help`),
// a preset's recipe, or the approved unknown-command refusal. Resolution order is built-in command,
// then registered agent, then preset — so a preset can never shadow a command someone typed.
// asHelp marks a `coop help …` spelling, whose correction must stay a help command.
func helpForPath(path []string, cfg *config.Config, asHelp bool) (int, error) {
	if len(path) == 0 {
		printHelp(cfg)
		return 0, nil
	}
	cmd := path[0]
	// A subcommand is checked against its family's OWN verb list first: a typo'd leaf is an error,
	// never a silent fallback to the family page it isn't part of. A verb with its own page — the
	// `net` family's ten — is keyed by the full path, so `coop help net blocked` and
	// `coop net blocked --help` reach the same one page.
	if len(path) > 1 {
		if verbs, closed := familyVerbs(cmd); closed && !slices.Contains(verbs, path[1]) {
			guess, _ := nearestCommand(path[1], verbs)
			return 2, ui.UnknownCommandPath(path[:2], guess, asHelp)
		}
		// A leaf with its own page answers for itself. Leaf pages register under the FULL path
		// ("sessions serve"), in the same commandHelp map as their family, so the manual and
		// `coop help <family> <leaf>` can never disagree about which page a leaf has.
		if leaf := strings.Join(path, " "); commandHelp[leaf] != "" {
			printTopicHelp(leaf, commandHelp[leaf])
			return 0, nil
		}
	}
	// A family whose commands carry their own pages registers them under the "<family> <command>"
	// key, so `coop help tasks release` and `coop tasks release --help` reach the leaf page.
	if len(path) > 1 {
		if page := commandHelp[cmd+" "+path[1]]; page != "" {
			printTopicHelp(cmd+" "+path[1], page)
			return 0, nil
		}
	}
	// `coop help credentials codex personal rm` names an agent and an account BETWEEN the family and
	// its command — the spelling a person actually types. The pages are about the SHAPE of that
	// command, so all three resolve to the same page whichever account is named.
	if cmd == "credentials" && len(path) > 2 {
		leaf := "credentials account"
		switch path[len(path)-1] {
		case "default":
			leaf = "credentials default"
		case "rm":
			leaf = "credentials rm"
		}
		printTopicHelp(leaf, commandHelp[leaf])
		return 0, nil
	}
	// `coop help fork <name> acp` names a fork BETWEEN the family and its command, so fork's leaf
	// pages also resolve on the last word — `coop help fork login acp` is that page, not a fork
	// called "acp".
	if cmd == "fork" && len(path) > 1 {
		if leaf := cmd + " " + path[len(path)-1]; commandHelp[leaf] != "" {
			printTopicHelp(leaf, commandHelp[leaf])
			return 0, nil
		}
	}
	switch {
	case cmd == "fork":
		// `coop fork login --help` is the launch contract for THAT fork; `coop help fork` the family.
		name := ""
		if len(path) > 1 {
			name = path[1]
		}
		code, _ := forkHelp(name)
		return code, nil
	case cmd == "run":
		printHelpPage(runHelp) // self-contained: the page closes with its own `coop help net` pointer
		return 0, nil
	case cmd == "help":
		// `coop help help` — help IS the top-level reference, so print it (not a broken pointer
		// to `coop help --help`, which these have no underlying CLI for).
		printHelp(cfg)
		return 0, nil
	case commandHelp[cmd] != "":
		printTopicHelp(cmd, commandHelp[cmd])
		return 0, nil
	}
	// `coop help claude`, `coop claude:opus/high@work --help` — coop's OWN page for that provider:
	// the flags it reads before `--`, and where its models and accounts live.
	if isTargetHead(cmd) {
		if t, err := agents.ParseTarget(cmd); err == nil {
			printHelpPage(agentHelp(t.Provider))
			return 0, nil
		}
	}
	// Anything runnable as `coop <preset>` is explainable as `coop help <preset>`: same roots and
	// repo-over-global precedence as execution, files only — no runtime, no box.
	if code, ok := helpForPreset(cmd, cfg); ok {
		return code, nil
	}
	return 2, unknownCommandErr(path[:1], asHelp)
}

// familyVerbs returns a command family's real subcommand list — from the family's OWN registry,
// never a second copy — and whether that family is closed. An OPEN family (fork takes a fork name,
// credentials an account, presets a preset name) reports false: its second word is a value, and
// rejecting it as a mistyped verb would refuse a legitimate name.
func familyVerbs(cmd string) ([]string, bool) {
	switch cmd {
	case "tasks":
		return tasks.TasksVerbs, true
	case "backlog":
		return tasks.BacklogVerbs, true
	case "net":
		return netCommands, true
	case "sessions":
		return sessionCommands, true
	}
	return nil, false
}

// unknownCommandErr refuses a top-level command coop doesn't have, naming the FULL rejected
// command (a bare token would read like a rejected shell command) and correcting a likely typo
// against coop's commands and registered agents.
func unknownCommandErr(path []string, asHelp bool) error {
	candidates := append(append([]string{}, topLevelCommands...), agents.Names()...)
	guess, _ := nearestCommand(path[len(path)-1], candidates)
	return ui.UnknownCommandPath(path, guess, asHelp)
}
