// Package cli is the command-line surface: it parses argv, resolves the config
// and runtime, and dispatches to the box engine and scaffolder. The routing:
// bare `coop` prints help (running an agent is explicit), `coop <agent>` runs a
// named agent, known subcommands run their command, and an unrecognized command is
// an error — raw commands run in the box explicitly, via `coop run -- <cmd>`.
package cli

import (
	"fmt"
	"runtime/debug"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
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
	cfg *config.Config
	rt  runtime.Runtime
}

// Main is the process entry point. It returns the exit code to pass to os.Exit.
func Main(argv []string) int {
	// Bare `coop`, help, and version all work without a container runtime. Bare
	// `coop` prints help rather than launching an agent — running one is explicit
	// (`coop claude`), so a stray `coop` never turns an agent loose on the cwd.
	if len(argv) == 0 {
		printHelp(config.Load())
		return 0
	}
	switch argv[0] {
	case "help", "-h", "--help":
		// `coop help <cmd>` shows that command's help — same as `coop <cmd> --help`. Bare `coop
		// help` (or -h/--help) is the top-level reference.
		if argv[0] == "help" && len(argv) > 1 {
			return helpForCommand(argv[1])
		}
		printHelp(config.Load())
		return 0
	case "version", "-v", "--version":
		if helpRequested(argv[1:]) { // `coop version --help` prints its help, not a self-referential error
			return helpForCommand("version")
		}
		if err := rejectArgs("version", argv[1:]); err != nil { // reject extras like every no-arg command
			ui.Error("%v", err)
			return 2
		}
		fmt.Println("coop " + resolveVersion())
		return 0
	}

	// `-h`/`--help` (or a bare `help` arg) on coop's own subcommands prints that command's
	// help without needing a runtime — fork gets its own family help. Agent and raw
	// commands (claude/codex/gemini/run/…) aren't in the map, so they fall through and
	// forward `--help`/`help` to the underlying CLI / box.
	if helpRequested(argv[1:]) || (len(argv) > 1 && argv[1] == "help") {
		if argv[0] == "fork" || argv[0] == "clone" {
			code, _ := forkHelp()
			return code
		}
		if h, ok := commandHelp[argv[0]]; ok {
			printCommandHelp(h)
			return 0
		}
	}

	cfg := config.Load()
	for _, w := range cfg.Warnings { // non-fatal config problems (e.g. an unrecognized COOP_EGRESS)
		ui.Warn("%s", w)
	}
	rt, err := runtime.Detect(cfg.RuntimeName)
	if err != nil {
		ui.Error("%v", err)
		return 1
	}

	a := &app{cfg: cfg, rt: rt}
	code, err := a.dispatch(argv)
	if err != nil {
		ui.Error("%v", err)
		if code == 0 {
			code = 1
		}
	}
	if code < 0 {
		code = 1
	}
	return code
}

func (a *app) dispatch(argv []string) (int, error) {
	if len(argv) == 0 { // unreachable (Main intercepts bare coop); defensive
		printHelp(a.cfg)
		return 0, nil
	}
	sub, rest := argv[0], argv[1:]
	switch sub {
	case "run":
		return a.cmdRun(rest)
	case "shell":
		if err := rejectArgs("shell", rest); err != nil {
			return 2, err
		}
		return a.runInBox([]string{a.cfg.Shell}, "", false)
	case "login":
		return a.cmdLogin(rest)
	case "profiles":
		return a.cmdProfiles(rest)
	case "pool":
		return a.cmdPool(rest)
	case "acp":
		return a.cmdACP(rest)
	case "fusion":
		return a.cmdFusion(rest)
	case "fork":
		return a.cmdFork(rest)
	case "clone": // back-compat alias for `coop fork`
		return a.cmdFork(rest)
	case "fleet":
		return a.cmdFleet(rest)
	case "tasks":
		return a.cmdTasks(rest)
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
	case "check-secrets":
		return a.cmdCheckSecrets(rest)
	case "build":
		return a.cmdBuild(rest)
	case "update":
		return a.cmdUpdate(rest)
	default:
		if agents.Valid(sub) { // coop claude|codex|gemini|… — run the agent
			return a.launchAgent(sub, rest)
		}
		// Don't ship an unrecognized command to the box to exec and fail with a cryptic
		// "not found" after a slow toolchain spin-up — a typo'd subcommand should fail
		// fast here. Raw box commands are explicit (`coop run -- <cmd>`).
		return 2, unknownCommandErr(argv)
	}
}

// topLevelCommands is coop's own subcommands, used only to suggest a correction on a
// mistyped one. Keep in sync with the dispatch switch above.
var topLevelCommands = []string{
	"run", "shell", "login", "profiles", "pool", "acp", "fusion", "fork", "fleet", "tasks",
	"loop", "up", "down", "init", "doctor", "check-secrets", "build", "update", "help", "version",
}

// helpForCommand prints one command's help for `coop help <cmd>`, matching `coop <cmd> --help`:
// fork's family help, a static commandHelp entry, a pointer for the agent/raw commands whose
// --help forwards to the underlying CLI, or an unknown-command error (exit 2) for anything else.
func helpForCommand(cmd string) int {
	switch {
	case cmd == "fork" || cmd == "clone":
		code, _ := forkHelp()
		return code
	case cmd == "run":
		printCommandHelp(runHelp)
		return 0
	case cmd == "help":
		// `coop help help` — help IS the top-level reference, so print it (not a broken pointer
		// to `coop help --help`, which these have no underlying CLI for).
		printHelp(config.Load())
		return 0
	case cmd == "version":
		fmt.Println("coop version — print coop's version.")
		return 0
	case commandHelp[cmd] != "":
		printCommandHelp(commandHelp[cmd])
		return 0
	case isKnownCommand(cmd):
		// The agents (claude/codex/gemini) forward --help to their own CLI, so coop keeps no
		// static page — point there instead of inventing one.
		fmt.Printf("coop %s forwards --help to the underlying CLI — run 'coop %s --help'.\n", cmd, cmd)
		return 0
	default:
		candidates := append(append([]string{}, topLevelCommands...), agents.Names()...)
		msg := fmt.Sprintf("unknown command %q", cmd)
		if guess, ok := nearestCommand(cmd, candidates); ok {
			msg += fmt.Sprintf("; did you mean %q?", guess)
		}
		ui.Error("%s — run 'coop help' for the list", msg)
		return 2
	}
}

// isKnownCommand reports whether cmd is one of coop's own subcommands or a coding agent.
func isKnownCommand(cmd string) bool {
	for _, c := range topLevelCommands {
		if c == cmd {
			return true
		}
	}
	return agents.Valid(cmd)
}

// unknownCommandErr explains an unrecognized command: a "did you mean" for a likely typo,
// and how to run an actual command in the box (which is no longer implicit).
func unknownCommandErr(argv []string) error {
	sub := argv[0]
	if sub == "status" { // removed — replaced by the task board (the default) + the per-fork fleet board
		return fmt.Errorf("coop status was removed — watch the work with `coop tasks watch` " +
			"(the queue + any active forks, deduped); for the per-fork fleet board use `coop fleet watch` " +
			"(snapshot: `coop fork ls`)")
	}
	msg := fmt.Sprintf("unknown command %q", sub)
	candidates := append(append([]string{}, topLevelCommands...), agents.Names()...)
	if guess, ok := nearestCommand(sub, candidates); ok {
		msg += fmt.Sprintf("; did you mean %q?", guess)
	}
	return fmt.Errorf("%s\n  run it in the box:  coop run -- %s\n  see all commands:   coop help",
		msg, strings.Join(argv, " "))
}
