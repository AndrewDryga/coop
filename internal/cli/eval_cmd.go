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
var evalCommands = []string{"run", "runs", "inspect", "compare", "ls", "init"}

// evalRunOptions are the flags `coop eval run` accepts after its positionals.
var evalRunOptions = []string{"--jobs", "--repeat", "--timeout", "--loop-config", "--dry-run"}

// cmdEval is the `coop eval` family: run a suite, compare two runs, list starters, scaffold a
// custom suite. Every request is fully resolved and validated BEFORE anything launches — the suite,
// each target/preset, every flag — so no invalid request ever reaches a provider, and the plan is
// always shown first (`--dry-run` stops there).
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
	case "inspect":
		return a.evalInspect(rest)
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

// evalOverview is bare `coop eval`: the preview, run and inspection workflow.
func (a *app) evalOverview() (int, error) {
	return groupHelp("eval")
}

// evalList shows the shipped starter catalog, with custom authoring as the fallback when empty.
func (a *app) evalList(args []string) (int, error) {
	if err := rejectArgs("eval ls", args); err != nil {
		return 2, err
	}
	starters := eval.Starters()
	if len(starters) == 0 {
		fmt.Println("No public starter suites are qualified yet.")
		fmt.Println("Create a custom suite: coop eval init ./evals/my-suite")
		return 0, nil
	}
	fmt.Println("Public starter suites:")
	for _, s := range starters {
		fmt.Printf("  %-24s %s\n", s.ID, s.Summary)
	}
	fmt.Println("\nPreview core:  coop eval run core codex --timeout 35m --dry-run")
	fmt.Println("Compare presets with the queue suite; see 'coop help eval run'.")
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
		fmt.Println("Preview your first: coop eval run core codex --timeout 35m --dry-run")
		return 0, nil
	}
	fmt.Println("Recorded runs (newest first):")
	for _, id := range ids {
		run, rerr := eval.LoadRun(root, id)
		if rerr != nil {
			fmt.Printf("\n%s — unreadable record\n", evalDisplayText(id))
			fmt.Printf("  Inspect: coop eval inspect %s\n", evalDisplayText(id))
			continue
		}
		fmt.Printf("\n%s  %s\n", evalDisplayText(id), evalDisplayText(run.Suite))
		printEvalText("  ", strings.Join(evalConfigLabels(run), ", "))
		if sum, ok, serr := eval.LoadSummary(root, id); serr != nil {
			fmt.Println("  Unreadable summary; inspect the saved records.")
		} else if ok {
			printEvalText("  ", evalResultLine(*sum))
		} else {
			fmt.Println("  No final summary — running or interrupted.")
		}
	}
	fmt.Println("\nInspect latest: coop eval inspect")
	fmt.Println("Inspect older:  coop eval inspect <run-id>")
	fmt.Println("Compare:        coop eval compare <before-id> <after-id>")
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
		return 2, ui.MissingArgument("<suite>", "coop eval run", "coop eval run <suite> <target|preset>... --timeout 60m")
	}
	if len(positionals) == 0 {
		return 2, ui.MissingArgument("<target|preset>", "coop eval run", "coop eval run "+suitePath+" <target|preset>... --timeout 60m")
	}
	suite, err := a.resolveEvalSuite(suitePath)
	if err != nil {
		return 1, err
	}
	configs, err := a.resolveEvalConfigurations(positionals)
	if err != nil {
		return 1, err
	}
	if !suite.IsLoop() {
		// Preview must reject the same unsupported configurations as execution, before
		// recording a run or preparing any workspaces. Keep one selection contract.
		for _, c := range configs {
			if _, err := applyEvalConfiguration(a.cfg.Clone(), eval.FrozenConfig{Kind: c.Kind, Label: c.Label}); err != nil {
				return 1, err
			}
		}
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
	var loopConfig []byte
	if loopConfigPath != "" {
		loopConfig, err = os.ReadFile(loopConfigPath)
		if err != nil {
			return 1, fmt.Errorf("freeze loop config %s: %w", loopConfigPath, err)
		}
	}
	build := a.evalBuildIdentity() // one digest of the binary, shared by every configuration
	frozen := make([]eval.FrozenConfig, 0, len(configs))
	for _, c := range configs {
		f, ferr := a.freezeConfiguration(c, loopConfig, build)
		if ferr != nil {
			return 1, ferr
		}
		frozen = append(frozen, f)
	}
	root, err := evalStateRoot()
	if err != nil {
		return 1, err
	}
	staged, err := eval.StageSuite(root, suite)
	if err != nil {
		return 1, fmt.Errorf("freeze suite inputs: %w", err)
	}
	stagedDir := staged.Dir
	defer eval.RemoveStagedSuite(stagedDir) // a real run moves these exact bytes under its own record
	plan.Suite = staged
	if staged.IsLoop() {
		// Every configuration hashes the same captured recipe, which is also what trials run.
		if err := os.WriteFile(filepath.Join(staged.Dir, "loop.yaml"), loopConfig, 0o600); err != nil {
			return 1, fmt.Errorf("freeze loop config: %w", err)
		}
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
	inputs := filepath.Join(store.Dir(), "inputs")
	if err := os.Rename(plan.Suite.Dir, inputs); err != nil {
		return 1, fmt.Errorf("retain frozen suite under run %s: %w", store.ID(), err)
	}
	plan.Suite.Dir = inputs
	plan.Suite.Path = filepath.Join(inputs, "suite.yaml")
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
		return 1, fmt.Errorf("the box image %s is not built yet — run 'coop build --egress open' in a directory without a project Dockerfile", image)
	}
	// Stage every preset configuration once, so each trial materializes identical bytes into its own
	// workspace (a fixture repository has no .agent/presets of its own).
	presets := map[string]string{}
	for _, c := range plan.Configs {
		if c.Kind != eval.ConfigPreset {
			continue
		}
		loaded, perr := a.loadRunPreset(c.Label)
		if perr != nil {
			return 1, perr
		}
		staged, serr := eval.StagePreset(loaded.Dir, filepath.Join(workRoot, "presets"), c.Label)
		if serr != nil {
			return 1, serr
		}
		presets[c.Label] = staged
	}
	runner := &trialRunner{app: a, suite: plan.Suite, workRoot: workRoot, image: image, presets: presets}
	fmt.Printf("Running %s (%d trials)…\n", store.ID(), len(plan.Suite.Cases)*len(plan.Configs)*plan.Repeat)
	summary, err := eval.Execute(context.Background(), plan, frozen, store, runner.run, time.Now)
	if err != nil {
		return 1, err
	}
	renderEvalSummary(store.ID(), summary)
	return evalRunExitCode(summary), nil
}

// A graded failure is a valid evaluation result; an execution or grading gap is not.
func evalRunExitCode(s eval.RunSummary) int {
	if s.Counts[eval.TrialPassed]+s.Counts[eval.TrialFailed] < s.Requested {
		return 1
	}
	return 0
}

// renderEvalSummary prints a finished run the same honest way a comparison does: passes over the
// FULL requested matrix, with coverage as its own figure, so an incomplete run can never read as a
// clean sweep.
func renderEvalSummary(id string, s eval.RunSummary) {
	covered := s.Counts[eval.TrialPassed] + s.Counts[eval.TrialFailed]
	fmt.Printf("\nRun %s\n", evalDisplayText(id))
	printEvalText("  ", evalResultLine(s))
	if covered < s.Requested {
		fmt.Println("⚠ coverage is incomplete — trials without a graded verdict are not passes or failures")
	}
	fmt.Printf("\nInspect results: coop eval inspect %s\n", id)
	fmt.Printf("Compare:         coop eval compare <before-id> %s\n", id)
}

// resolveEvalSuite loads a suite from a filesystem path or a shipped starter ID.
func (a *app) resolveEvalSuite(ref string) (*eval.Suite, error) {
	root, err := evalStateRoot()
	if err != nil {
		return nil, err
	}
	starter, ok, err := eval.StarterPath(ref, root)
	if err != nil {
		return nil, err
	}
	if ok {
		ref = starter
	} else if !strings.ContainsAny(ref, "/.") {
		return nil, fmt.Errorf("no starter suite %q exists; run `coop eval ls` to see them, or give a path to your own suite.yaml", ref)
	}
	return eval.Load(ref)
}

// resolveEvalConfigurations turns each positional into an evaluated configuration, using the same
// target parsing and preset resolution as every other launch. Each positional is its OWN
// configuration — there is no fallback to the preceding one. A malformed target or a missing preset
// is refused here, by name, before any plan is shown.
func (a *app) resolveEvalConfigurations(positionals []string) ([]eval.Configuration, error) {
	configs := make([]eval.Configuration, 0, len(positionals))
	seen := make(map[eval.Configuration]bool, len(positionals))
	for _, who := range positionals {
		var c eval.Configuration
		if isTargetHead(who) {
			t, err := agents.ParseTarget(who)
			if err != nil {
				return nil, err
			}
			// A pinned @account must exist, refused by name before any work — the same promise every
			// other launch makes. A bare target (no account) is left to the provider's own defaults.
			profiles := box.EffectiveProfiles(a.cfg, t.Provider)
			for _, acct := range t.Accounts {
				if !slices.Contains(profiles, acct) {
					return nil, fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", t.Provider, acct, t.Provider, acct)
				}
			}
			c = eval.Configuration{Kind: eval.ConfigTarget, Label: t.String()}
		} else {
			if !preset.ValidName(who) {
				return nil, fmt.Errorf("%q is neither a target (provider[:model][/effort][@account]) nor a preset name", who)
			}
			if _, err := a.loadRunPreset(who); err != nil {
				return nil, err
			}
			c = eval.Configuration{Kind: eval.ConfigPreset, Label: who}
		}
		if seen[c] {
			return nil, fmt.Errorf("configuration %q is repeated; use --repeat for deliberate repeated trials", c.Label)
		}
		seen[c] = true
		configs = append(configs, c)
	}
	return configs, nil
}

// evalCompare pairs two sealed runs of the same suite and shows the paired before/after report:
// coverage, pass counts, per-case wins and regressions. It refuses to merge two runs of different
// workloads into one score, and refuses an interrupted (unsealed) run.
func (a *app) evalCompare(args []string) (int, error) {
	const usage = "coop eval compare <before-id> <after-id>"
	if len(args) < 2 {
		name := "before run ID"
		if len(args) == 1 {
			name = "after run ID"
		}
		return 2, ui.MissingArgument(name, "coop eval compare", usage)
	}
	if len(args) > 2 {
		return 2, ui.UnexpectedArgument(args[2], "coop eval compare", usage)
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
	fmt.Printf("Comparison: %s\n", evalDisplayText(c.Suite))
	if c.Mismatch != "" {
		printEvalText("⚠ ", c.Mismatch)
	}
	line := func(label, id string, o eval.ConfigOutcome) {
		// Lead with passed / REQUESTED (never / covered): a pending or errored trial stays in the
		// denominator, so a run can't look better by not finishing. Coverage is a separate figure.
		printEvalText(label+": ", strings.Join(o.Configs, ", "))
		fmt.Printf("  Run: %s\n", evalDisplayText(id))
		printEvalText("  ", evalResultLine(eval.RunSummary{Requested: o.Requested, Counts: map[eval.TrialStatus]int{
			eval.TrialPassed: o.Passed, eval.TrialFailed: o.Failed, eval.TrialError: o.Errored,
			eval.TrialTimedOut: o.TimedOut, eval.TrialPending: o.Pending,
		}}))
		// Beside the counts, never inside them: this is a review signal, not a score. Its explicit
		// denominator keeps a missing optional cloc from reading like a zero-sized solution.
		printEvalText("  Change size: ", evalChangeSizeLine(o.Size, o.Covered()))
		fmt.Println()
	}
	fmt.Println()
	line("Before", c.BaseID, c.Base)
	line("After", c.NewID, c.New)
	if c.Base.Covered() < c.Base.Requested || c.New.Covered() < c.New.Requested {
		fmt.Println("⚠ Incomplete grading — no definitive winner.")
		fmt.Println("  Errors and timeouts are not model-quality failures.")
	}
	fmt.Printf("\nInspect before: coop eval inspect %s\n", c.BaseID)
	fmt.Printf("Inspect after:  coop eval inspect %s\n", c.NewID)
	if c.Mismatch != "" || len(c.Cases) == 0 {
		return
	}
	fmt.Println("\nPer case (before → after; summed over configurations):")
	for _, cc := range c.Cases {
		fmt.Printf("  %s\n", evalDisplayText(cc.Case))
		printEvalText("    ", evalCaseResult(cc.Base)+" → "+evalCaseResult(cc.New))
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
	fmt.Printf("Preview it: coop eval run %s codex --timeout 12m --dry-run\n", shellWord(manifest))
	fmt.Println("Edit suite.yaml and its fixture/verifier, then remove --dry-run to execute.")
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
		if *i+1 >= len(args) || strings.HasPrefix(args[*i+1], "--") {
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
	fmt.Println()
	fmt.Println("Configurations:")
	for i, c := range p.Configs {
		fmt.Printf("  - %-28s (%s) [config %s, build %s]\n", c.Label, c.Kind,
			frozen[i].Fingerprint().Short(), frozen[i].Build)
	}
	fmt.Println()
	fmt.Printf("Matrix: %d case(s) x %d configuration(s) x %d repeat(s) = %d trial(s)\n",
		len(p.Suite.Cases), len(p.Configs), p.Repeat, p.Trials())
	fmt.Printf("Workers: %d\n", p.Jobs)
	fmt.Printf("Deadline: %s (covers preparation, work, grading and cleanup)\n", p.Timeout)
	fmt.Println()
	fmt.Println("Isolation: operator MCP servers and COOP_RUN_ARGS are omitted from trials")
	fmt.Println()
	fmt.Println("Cases:")
	for _, c := range p.Suite.Cases {
		fmt.Printf("  - %-24s budget %s\n", c.ID, c.Timeout)
	}
}
