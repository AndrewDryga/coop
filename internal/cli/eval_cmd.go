package cli

import (
	"context"
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
var evalCommands = []string{"ls", "runs", "run", "compare", "init"}

// evalRunOptions are the flags `coop eval run` accepts after its positionals.
var evalRunOptions = []string{"--jobs", "--repeat", "--timeout", "--loop-config", "--dry-run"}

// cmdEval is the `coop eval` family: run a suite, compare two runs, list starters, scaffold a
// custom suite. Every request is fully resolved and validated BEFORE anything launches — the suite,
// each target/preset, every flag — so no invalid request ever reaches a provider, and the plan is
// always shown first (`--dry-run` stops there). Agent suites execute; loop scenarios and the public
// starter catalog land in later milestones.
func (a *app) cmdEval(args []string) (int, error) {
	if len(args) == 0 {
		return a.evalOverview()
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "ls":
		return a.evalList(rest)
	case "runs":
		return a.evalRuns(rest)
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

// evalRuns lists recorded runs, newest first, so a user can find the two ids to compare.
func (a *app) evalRuns(args []string) (int, error) {
	if err := rejectArgs("eval runs", args); err != nil {
		return 2, err
	}
	root, err := evalStateRoot()
	if err != nil {
		return 1, err
	}
	ids, err := eval.ListRuns(root)
	if err != nil {
		return 1, err
	}
	if len(ids) == 0 {
		fmt.Println("No eval runs recorded yet.")
		return 0, nil
	}
	fmt.Println("Recorded runs (newest first):")
	for _, id := range ids {
		run, rerr := eval.LoadRun(root, id)
		if rerr != nil {
			fmt.Printf("  %s\n", id)
			continue
		}
		sealed := ""
		if _, ok, serr := eval.LoadSummary(root, id); serr != nil {
			sealed = " (unreadable summary)"
		} else if !ok {
			sealed = " (interrupted)"
		}
		fmt.Printf("  %-32s %s%s\n", id, run.Suite, sealed)
	}
	return 0, nil
}

// evalRun parses `coop eval run <suite> <target|preset>... [flags]`, resolves every positional and
// the suite once, shows the plan, and then works the matrix. ALL validation happens before the plan
// is printed, so a bad suite, target, preset or flag is refused before any provider work; `--dry-run`
// stops after the plan, which is the last point before money is spent.
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
	if opts.DryRun {
		fmt.Println("Dry run: nothing was launched.")
		return 0, nil
	}
	return a.executeEvalRun(plan, frozen)
}

// executeEvalRun creates the run record, works the whole matrix and prints the sealed summary. Every
// trial's workspace lives under one scratch root that is removed when the run ends — the durable
// record is the store's, not a pile of temp directories.
func (a *app) executeEvalRun(plan *eval.Plan, frozen []eval.FrozenConfig) (int, error) {
	// Every trial runs in a box, so the runtime has to be detected before the first launch — and
	// refused HERE, by name, rather than surfacing later as an unhelpful per-trial error.
	if err := a.ensureRuntime(); err != nil {
		return 1, err
	}
	root, err := evalStateRoot()
	if err != nil {
		return 1, err
	}
	store, err := eval.CreateRun(root, eval.NewRunRecord(plan, frozen, time.Now()))
	if err != nil {
		return 1, err
	}
	// Trial working directories live UNDER the run, not in a temp dir that vanishes: a trial that
	// did not pass leaves its graded workspace behind for the user to look at, and it is removed
	// with the run's own record rather than on the way out of this function.
	workRoot := filepath.Join(store.Dir(), "work")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return 1, err
	}
	defer os.Remove(workRoot) // succeeds only when every trial passed and removed its own directory

	// Resolve the managed base image to its definition-pinned tag, the same way every other box
	// command does — an unresolved "coop-box" is not a tag that exists.
	box.ResolveBaseImage(a.cfg)
	image := box.ImageForRepo("", a.cfg.BaseImage, a.cfg.ImageOverride)
	if !box.ImageExists(a.rt, image) {
		return 1, fmt.Errorf("the box image %s is not built yet — run: coop build", image)
	}
	runner := &trialRunner{app: a, suite: plan.Suite, workRoot: workRoot, image: image}
	fmt.Printf("Running %s (%d trials)…\n", store.ID(), len(plan.Suite.Cases)*len(plan.Configs)*plan.Repeat)
	summary, err := eval.Execute(context.Background(), plan, frozen, store, runner.run, time.Now)
	if err != nil {
		return 1, err
	}
	renderEvalSummary(store.ID(), summary)
	return 0, nil
}

// renderEvalSummary prints a finished run the same honest way a comparison does: passes over the
// FULL requested matrix, with coverage as its own figure, so an incomplete run can never read as a
// clean sweep.
func renderEvalSummary(id string, s eval.RunSummary) {
	covered := s.Counts[eval.TrialPassed] + s.Counts[eval.TrialFailed]
	fmt.Printf("\nRun %s: %d/%d passed; coverage %d/%d graded\n",
		id, s.Counts[eval.TrialPassed], s.Requested, covered, s.Requested)
	for _, st := range []eval.TrialStatus{eval.TrialFailed, eval.TrialError, eval.TrialTimedOut, eval.TrialPending} {
		if n := s.Counts[st]; n > 0 {
			fmt.Printf("  %-9s %d\n", st, n)
		}
	}
	if covered < s.Requested {
		fmt.Println("⚠ coverage is incomplete — trials without a graded verdict are not passes or failures")
	}
	fmt.Printf("\nCompare it with another run: coop eval compare <other-run> %s\n", id)
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

// evalCompare pairs two sealed runs of the same suite and shows the paired before/after report:
// coverage, pass counts, per-case wins and regressions. It refuses to merge two runs of different
// workloads into one score, and refuses an interrupted (unsealed) run.
func (a *app) evalCompare(args []string) (int, error) {
	if len(args) != 2 {
		return 2, ui.MissingArgument("<run-id> <run-id>", "coop eval compare", "coop eval compare <run-id> <run-id>")
	}
	root, err := evalStateRoot()
	if err != nil {
		return 1, err
	}
	cmp, err := eval.Compare(root, args[0], args[1])
	if err != nil {
		return 1, err
	}
	renderEvalComparison(cmp)
	return 0, nil
}

// renderEvalComparison prints a comparison: each run's coverage and counts, then per-case pairing.
func renderEvalComparison(c *eval.Comparison) {
	fmt.Printf("Comparing %s (before) vs %s (after)\n", c.BaseID, c.NewID)
	if c.Mismatch != "" {
		fmt.Printf("\n⚠ %s\n", c.Mismatch)
	}
	line := func(label string, o eval.ConfigOutcome) {
		// Lead with passed / REQUESTED (never / covered): a pending or errored trial stays in the
		// denominator, so a run can't look better by not finishing. Coverage is a separate figure.
		fmt.Printf("  %-7s %v: %d/%d passed; coverage %d/%d graded; failed %d, error %d, timed out %d, pending %d\n",
			label, o.Configs, o.Passed, o.Requested, o.Covered(), o.Requested, o.Failed, o.Errored, o.TimedOut, o.Pending)
	}
	fmt.Println()
	line("before", c.Base)
	line("after", c.New)
	if c.Base.Covered() < c.Base.Requested || c.New.Covered() < c.New.Requested {
		fmt.Println("⚠ coverage is incomplete; with unfinished trials there is no definitive winner")
	}
	if c.Mismatch != "" || len(c.Cases) == 0 {
		return
	}
	fmt.Println("\nPer case (before → after; summed over configurations):")
	for _, cc := range c.Cases {
		fmt.Printf("  %-24s passed %d→%d  failed %d→%d  error %d→%d\n",
			cc.Case, cc.Base.Passed, cc.New.Passed, cc.Base.Failed, cc.New.Failed, cc.Base.Errored, cc.New.Errored)
	}
}

// evalStateRoot is the owner-private eval results directory, outside every candidate mount.
func evalStateRoot() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "coop", "eval"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "coop", "eval"), nil
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
		case arg == "--dry-run":
			if seen[arg] {
				return "", nil, opts, ui.RepeatedOption(arg, cmd)
			}
			seen[arg] = true
			opts.DryRun = true
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
	// A whole-invocation deadline is required, not defaulted: an eval launches real models, and the
	// one number that bounds what it can spend should be a decision the operator made, not one Coop
	// guessed. It is required for --dry-run too, because the dry run's job is to show exactly what
	// the real run would do — deadline included.
	if opts.Timeout <= 0 {
		return "", nil, opts, ui.MissingArgument("--timeout", cmd, "coop eval run <suite> <target|preset>... --timeout 60m")
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
