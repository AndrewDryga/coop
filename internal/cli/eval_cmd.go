package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/eval"
	"github.com/AndrewDryga/coop/internal/preset"
	"github.com/AndrewDryga/coop/internal/ui"
)

// evalCommands are the `coop eval` verbs, in help order.
var evalCommands = []string{"ls", "run", "compare", "init"}

// evalRunOptions are the flags `coop eval run` accepts after its positionals.
var evalRunOptions = []string{"--jobs", "--repeat", "--timeout", "--loop-config"}

// cmdEval is the `coop eval` family: run a suite, compare two runs, list starters, scaffold a
// custom suite. v1 milestone 1 wires parsing, strict validation and the run PLAN — it resolves every
// target/preset and loads the suite before showing what would run, and refuses bad input here, so no
// invalid request ever reaches a provider. Trial execution and comparison land in later milestones.
func (a *app) cmdEval(args []string) (int, error) {
	if len(args) == 0 {
		return a.evalOverview()
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "ls":
		return a.evalList(rest)
	case "run":
		return a.evalRun(rest)
	case "compare":
		return a.evalCompare(rest)
	case "init":
		return a.evalInit(rest)
	default:
		return 2, unknownSubcommandErr("eval", verb, evalCommands)
	}
}

// evalOverview is bare `coop eval`: the shipped starters (none yet) and how to author your own.
func (a *app) evalOverview() (int, error) {
	fmt.Println("coop eval — compare a preset, loop config or Coop build before and after a change")
	fmt.Println()
	if _, err := a.evalList(nil); err != nil {
		return 1, err
	}
	fmt.Println()
	fmt.Println("Author your own:  coop eval init ./evals/my-suite")
	fmt.Println("Run one:          coop eval run <suite> <target|preset>...")
	return 0, nil
}

// evalList shows the shipped starter catalog. It is empty until the public starters are qualified
// (a later milestone), so today it names that and points at custom authoring rather than pretending.
func (a *app) evalList(args []string) (int, error) {
	if err := rejectArgs("eval ls", args); err != nil {
		return 2, err
	}
	starters := eval.Starters()
	if len(starters) == 0 {
		fmt.Println("No public starter suites are qualified yet.")
		fmt.Println("Run a custom suite:  coop eval run ./evals/my-suite/suite.yaml <target|preset>...")
		return 0, nil
	}
	fmt.Println("Public starter suites:")
	for _, s := range starters {
		fmt.Printf("  %-24s %s\n", s.ID, s.Summary)
	}
	return 0, nil
}

// evalRun parses `coop eval run <suite> <target|preset>... [flags]`, resolves every positional and
// the suite once, and shows the plan. It launches nothing in milestone 1 — but it does ALL the
// validation, so a bad suite, target, preset or flag is refused before any run is attempted.
func (a *app) evalRun(args []string) (int, error) {
	suitePath, positionals, opts, err := parseEvalRunArgs(args)
	if err != nil {
		return 2, err
	}
	if suitePath == "" {
		return 2, ui.MissingArgument("<suite>", "coop eval run", "coop eval run <suite> <target|preset>...")
	}
	if len(positionals) == 0 {
		return 2, ui.MissingArgument("<target|preset>", "coop eval run", "coop eval run "+suitePath+" <target|preset>...")
	}
	suite, err := a.resolveEvalSuite(suitePath)
	if err != nil {
		return 1, err
	}
	configs, err := a.resolveEvalConfigurations(positionals)
	if err != nil {
		return 1, err
	}
	plan, err := eval.BuildPlan(suite, configs, opts)
	if err != nil {
		return 1, err
	}
	// Resolve the loop config to freeze: the suite's own loop_config is relative to the suite (the
	// loader validated it does not escape or pass a symlink), but a --loop-config OVERRIDE is the
	// operator's explicit path — CWD-relative like any CLI path argument, and trusted as their own
	// file — so the two resolve against different roots.
	loopConfigPath := ""
	if suite.IsLoop() {
		if opts.LoopConfigOverride != "" {
			if loopConfigPath, err = filepath.Abs(opts.LoopConfigOverride); err != nil {
				return 1, err
			}
		} else {
			loopConfigPath = filepath.Join(suite.Dir, filepath.Clean(suite.LoopConfig))
		}
	}
	build := a.evalBuildIdentity() // one digest of the binary, shared by every configuration
	frozen := make([]eval.FrozenConfig, 0, len(configs))
	for _, c := range configs {
		f, ferr := a.freezeConfiguration(c, suite, loopConfigPath, build)
		if ferr != nil {
			return 1, ferr
		}
		frozen = append(frozen, f)
	}
	renderEvalPlan(plan, frozen)
	fmt.Println()
	fmt.Println("Planning only: trial execution and comparison ship in a later Coop release.")
	return 0, nil
}

// resolveEvalSuite loads a suite from a filesystem path or, later, a shipped starter id. Milestone 1
// resolves a path; a bare starter id that is not a path is refused by name until the catalog ships.
func (a *app) resolveEvalSuite(ref string) (*eval.Suite, error) {
	if starter, ok := eval.StarterPath(ref); ok {
		ref = starter
	} else if !strings.ContainsAny(ref, "/.") {
		return nil, fmt.Errorf("no starter suite %q is qualified yet; give a path to your own suite.yaml", ref)
	}
	return eval.Load(ref)
}

// resolveEvalConfigurations turns each positional into an evaluated configuration, using the same
// target parsing and preset resolution as every other launch. Each positional is its OWN
// configuration — there is no fallback to the preceding one. A malformed target or a missing preset
// is refused here, by name, before any plan is shown.
func (a *app) resolveEvalConfigurations(positionals []string) ([]eval.Configuration, error) {
	configs := make([]eval.Configuration, 0, len(positionals))
	for _, who := range positionals {
		if isTargetHead(who) {
			t, err := agents.ParseTarget(who)
			if err != nil {
				return nil, err
			}
			// A pinned @account must exist, refused by name before any work — the same promise every
			// other launch makes. A bare target (no account) is left to the provider's own defaults.
			if acct := t.Account(); acct != "" && !slices.Contains(box.EffectiveProfiles(a.cfg, t.Provider), acct) {
				return nil, fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", t.Provider, acct, t.Provider, acct)
			}
			configs = append(configs, eval.Configuration{Kind: eval.ConfigTarget, Label: t.String()})
			continue
		}
		if !preset.ValidName(who) {
			return nil, fmt.Errorf("%q is neither a target (provider[:model][/effort][@account]) nor a preset name", who)
		}
		if _, err := a.loadRunPreset(who); err != nil {
			return nil, err
		}
		configs = append(configs, eval.Configuration{Kind: eval.ConfigPreset, Label: who})
	}
	return configs, nil
}

// evalCompare pairs two run ids. Runs are produced by trial execution, which does not exist yet, so
// milestone 1 refuses cleanly rather than pretending to have results.
func (a *app) evalCompare(args []string) (int, error) {
	if len(args) != 2 {
		return 2, ui.MissingArgument("<run-id> <run-id>", "coop eval compare", "coop eval compare <run-id> <run-id>")
	}
	return 1, fmt.Errorf("no eval runs exist yet: run execution and comparison ship in a later Coop release")
}

// evalInit scaffolds a custom suite: a working agent example and a documented loop example, so a
// user can copy the shape rather than learn the schema from an error. Create-only — it refuses to
// write into a directory that already has a suite.
func (a *app) evalInit(args []string) (int, error) {
	if len(args) != 1 {
		return 2, ui.MissingArgument("<dir>", "coop eval init", "coop eval init ./evals/my-suite")
	}
	dir := args[0]
	manifest := filepath.Join(dir, "suite.yaml")
	if _, err := os.Stat(manifest); err == nil {
		return 1, fmt.Errorf("%s already exists; eval init never overwrites a suite", manifest)
	}
	if err := eval.Scaffold(dir); err != nil {
		return 1, err
	}
	fmt.Printf("Wrote a starter suite to %s\n", dir)
	fmt.Printf("Edit it, then:  coop eval run %s <target|preset>...\n", manifest)
	return 0, nil
}

// parseEvalRunArgs splits `run` into the suite, the target/preset positionals and the flags. The
// suite is the first positional; every positional after it is a configuration; flags may appear
// anywhere. Defaults: one repetition, one worker, no cost limit; a run needs an explicit --timeout.
func parseEvalRunArgs(args []string) (suite string, positionals []string, opts eval.Options, err error) {
	const cmd = "coop eval run"
	opts = eval.Options{Jobs: 1, Repeat: 1}
	seen := map[string]bool{}
	takeValue := func(flag string, i *int) (string, error) {
		if seen[flag] {
			return "", ui.RepeatedOption(flag, cmd)
		}
		seen[flag] = true
		if *i+1 >= len(args) {
			return "", ui.MissingOptionValue(flag, cmd, "coop eval run <suite> <target|preset>... --timeout 60m")
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--jobs":
			v, e := takeValue(arg, &i)
			if e != nil {
				return "", nil, opts, e
			}
			if opts.Jobs, err = positiveInt(arg, v); err != nil {
				return "", nil, opts, err
			}
		case arg == "--repeat":
			v, e := takeValue(arg, &i)
			if e != nil {
				return "", nil, opts, e
			}
			if opts.Repeat, err = positiveInt(arg, v); err != nil {
				return "", nil, opts, err
			}
		case arg == "--timeout":
			v, e := takeValue(arg, &i)
			if e != nil {
				return "", nil, opts, e
			}
			d, de := time.ParseDuration(v)
			if de != nil || d <= 0 {
				return "", nil, opts, ui.InvalidOptionValue(v, arg, cmd, "not a positive duration", "60m")
			}
			opts.Timeout = d
		case arg == "--loop-config":
			if opts.LoopConfigOverride, err = takeValue(arg, &i); err != nil {
				return "", nil, opts, err
			}
		case strings.HasPrefix(arg, "-"):
			return "", nil, opts, unknownOptionErr(arg, "eval run", evalRunOptions)
		case suite == "":
			suite = arg
		default:
			positionals = append(positionals, arg)
		}
	}
	if opts.Timeout <= 0 {
		return "", nil, opts, ui.MissingOptionValue("--timeout", cmd, "coop eval run <suite> <target|preset>... --timeout 60m")
	}
	return suite, positionals, opts, nil
}

func positiveInt(flag, value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, ui.InvalidOptionValue(value, flag, "coop eval run", "not a positive whole number", "3")
	}
	return n, nil
}

// renderEvalPlan shows the matrix before any work: the suite, its cases, every configuration, the
// repetitions, the effective workers and the total deadline. Resource, reviewer, image, network and
// cache detail resolve at preparation (a later milestone) and are named as such rather than faked.
func renderEvalPlan(p *eval.Plan, frozen []eval.FrozenConfig) {
	kind := "agent"
	if p.Suite.IsLoop() {
		kind = "loop"
	}
	fmt.Printf("Suite: %s (%s runner, %d case(s)) [workload %s]\n", p.Suite.Name, kind, len(p.Suite.Cases),
		eval.WorkloadFingerprint(p.Suite).Short())
	if p.Suite.IsLoop() {
		fmt.Printf("Loop config: %s\n", p.LoopConfig)
	}
	fmt.Println("Configurations:")
	for i, c := range p.Configs {
		fmt.Printf("  - %-28s (%s) [config %s, build %s]\n", c.Label, c.Kind,
			frozen[i].Fingerprint().Short(), frozen[i].Build)
	}
	fmt.Printf("Matrix: %d case(s) x %d configuration(s) x %d repeat(s) = %d trial(s)\n",
		len(p.Suite.Cases), len(p.Configs), p.Repeat, p.Trials())
	fmt.Printf("Workers: %d\n", p.Jobs)
	fmt.Printf("Deadline: %s (covers preparation, work, grading and cleanup)\n", p.Timeout)
	fmt.Println("Cases:")
	for _, c := range p.Suite.Cases {
		fmt.Printf("  - %-24s budget %s\n", c.ID, c.Timeout)
	}
}
