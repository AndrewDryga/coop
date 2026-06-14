// Package cli is the command-line surface: it parses argv, resolves the config
// and runtime, and dispatches to the box engine and scaffolder. The routing
// mirrors the original tool: bare `coop` runs Claude, `agent <agent>` runs a
// named agent, known subcommands run their command, and anything else is run as
// a command inside the box (so `agent npm test` just works).
package cli

import (
	"fmt"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

// Version is the tool version, reported by `coop version`.
const Version = "2.0.0"

type app struct {
	cfg *config.Config
	rt  runtime.Runtime
}

// Main is the process entry point. It returns the exit code to pass to os.Exit.
func Main(argv []string) int {
	// help and version work without a container runtime.
	if len(argv) > 0 {
		switch argv[0] {
		case "help", "-h", "--help":
			printHelp(config.Load())
			return 0
		case "version", "-v", "--version":
			fmt.Println("coop " + Version)
			return 0
		}
	}

	cfg := config.Load()
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
	if len(argv) == 0 {
		return a.cmdRun(nil) // bare `coop` → Claude
	}
	sub, rest := argv[0], argv[1:]
	switch sub {
	case "claude", "codex", "gemini":
		return a.launchAgent(sub, rest)
	case "run":
		return a.cmdRun(rest)
	case "shell":
		return a.runInBox([]string{a.cfg.Shell})
	case "login":
		return a.cmdLogin(rest)
	case "acp":
		return a.cmdACP(rest)
	case "clone":
		return a.cmdClone(rest)
	case "dispatch":
		return a.cmdDispatch(rest)
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
	case "build":
		return a.cmdBuild(rest)
	default:
		return a.cmdRun(argv) // e.g. `agent npm test`
	}
}
