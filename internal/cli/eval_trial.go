package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/eval"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// One trial, end to end. The order of these steps IS the isolation contract, so it is written once,
// here, rather than assembled per call site:
//
//  1. materialize a private workspace from the case's files — no source history, no host paths, and
//     never the verifier (which lives outside every candidate mount);
//  2. measure its size BEFORE the candidate touches it;
//  3. run the candidate's attempt in a box with its own credentials, bounded by the trial deadline;
//  4. snapshot the finished workspace — the candidate's box has exited, so nothing is still writing;
//  5. grade the SNAPSHOT in a fresh, credential-free, network-free sandbox;
//  6. measure the snapshot and attach net growth beside the verdict, never folded into it.
//
// Step 3 is the only step that spends money, and every step after it is deliberately independent of
// what the candidate did to its own container: a candidate that breaks its shell, its interpreter or
// its network cannot break or steer its own grading.

// trialRunner is the eval.TrialFunc's receiver: everything one run needs to execute a trial. It is
// built once per run by the CLI, so eval stays a leaf and this keeps the box/agent knowledge.
type trialRunner struct {
	app      *app
	suite    *eval.Suite
	image    string // the trusted image: the candidate's attempt AND the grader run from it
	workRoot string // per-run scratch root; each trial gets its own subdirectory
	// presets maps a preset configuration's label to its staged copy, captured once per run so every
	// trial materializes the same bytes.
	presets map[string]string
	runBox  boxRunner
}

// run executes one trial and always returns a status — never an error — because a run records why a
// trial has no verdict rather than aborting the sweep.
func (r *trialRunner) run(ctx context.Context, t eval.Trial) eval.TrialResult {
	dir := filepath.Join(r.workRoot, trialSlug(t))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(ctx, "could not create the trial directory: "+err.Error())
	}

	// 1. The private workspace. PrepareWorkspace returns the synthetic initial COMMIT, not the path —
	// the workspace is the destination we named.
	// An agent case starts from its `files`; a loop scenario starts from its `fixture` repository.
	// Either way PrepareWorkspace gives the trial a private tree with a synthetic initial commit and
	// none of the author's history.
	source := t.Case.Files
	if r.suite.IsLoop() {
		source = t.Case.Fixture
	}
	workspace := filepath.Join(dir, "workspace")
	if _, err := eval.PrepareWorkspace(ctx, filepath.Join(r.suite.Dir, source), workspace); err != nil {
		return fail(ctx, "could not prepare the workspace: "+err.Error())
	}

	// 2. A loop scenario needs its queue, preset and recipe in place BEFORE the baseline is taken,
	// or the harness's own files would read as candidate work — inflating the change size and
	// defeating the untouched-workspace check below.
	if r.suite.IsLoop() {
		if err := r.materializeLoopScenario(t, workspace); err != nil {
			return fail(ctx, "could not materialize the scenario: "+err.Error())
		}
	}

	// 3. Size before the candidate touches anything, and a signature of the initial tree so we can
	// tell afterwards whether it did anything at all. Both ignore the harness's own bookkeeping:
	// the queue's state moves and the loop's telemetry are not the candidate's code.
	ignore := harnessPaths(r.suite.IsLoop())
	before, beforeErr := eval.MeasureSize(ctx, workspace, ignore...)
	beforeSig, _ := eval.TreeSignature(workspace, ignore...)

	// 4. The attempt. This is the only paid step: one headless agent call, or — for a loop scenario
	// — a whole bounded `coop loop` working the case's queue.
	attempt := r.attempt
	if r.suite.IsLoop() {
		attempt = r.runLoopTrial
	}
	att, err := attempt(ctx, t, workspace)
	if err != nil {
		return fail(ctx, joinDetail("the attempt could not run: "+err.Error(), att.detail))
	}
	if att.limited {
		// The provider refused — an expired login, a rate limit, a quota. The model never got to
		// try, so grading its untouched workspace would record a FAIL that is really our problem.
		return fail(ctx, joinDetail("the provider refused the attempt (rate limit, quota or sign-in), so there is nothing to grade", att.detail))
	}
	// The same judgement, reached without having to recognize any particular provider's wording: the
	// agent exited non-zero AND left the workspace exactly as it found it. A model that genuinely
	// tried and failed leaves something behind — a file, an edit, a broken attempt. Nothing at all,
	// plus a non-zero exit, is what an unsupported model, an expired login or a missing binary looks
	// like from out here, and calling that a FAIL would put a confident zero on a trial where the
	// model never got to work. It is recorded as a harness error instead, with the agent's own words.
	// A blocked loop is a result even without source edits: its human decision lives in the queue,
	// which the signature deliberately ignores. The verifier still decides whether that result passes.
	blockedLoop := r.suite.IsLoop() && att.code == loopExitBlocked && loopQueueBlocked(workspace)
	if att.code != 0 && !blockedLoop {
		if sig, sigErr := eval.TreeSignature(workspace, ignore...); sigErr == nil && beforeSig != "" && sig == beforeSig {
			return fail(ctx, joinDetail(
				fmt.Sprintf("the agent exited %d having changed nothing in the workspace, so there is no work to grade — recorded as a harness error, not a model failure", att.code),
				att.detail))
		}
	}

	// 5. The candidate's box has exited, so the workspace is quiet: snapshot it. A non-zero agent
	// exit is NOT decided here — the verifier decides whether the work is good, because an agent
	// that exits non-zero may still have done the job (and one that exits 0 may not have).
	snap, err := eval.SnapshotWorkspace(workspace, filepath.Join(dir, "snapshot"))
	if err != nil {
		return fail(ctx, "could not snapshot the workspace for grading: "+err.Error())
	}

	// 6. Grade the snapshot in the sandbox.
	res := r.app.gradeSnapshot(ctx, gradeRequest{
		Image: r.image, Workspace: snap.Dir, Verifier: filepath.Join(r.suite.Dir, t.Case.Verifier), CaseID: t.Case.ID,
	}, r.runBox)

	// 7. Size after, reported BESIDE the verdict. Size never changes a verdict: a smaller wrong
	// answer is not better than a larger right one.
	after, afterErr := eval.MeasureSize(ctx, snap.Dir, ignore...)
	if beforeErr == nil && afterErr == nil {
		// Recorded structurally as well as in prose, so a comparison can add it up.
		res.Size = &eval.TrialSize{CodeBefore: before.TotalCode(), CodeAfter: after.TotalCode()}
	}
	res.Detail = joinDetail(res.Detail, sizeNote(before, beforeErr, after, afterErr, snap.Skipped))

	// A trial that PASSED needs no evidence kept — its workspace is megabytes of "it worked". One
	// that did not is exactly what a user needs to look at, so its snapshot stays under the run and
	// the detail says where. (The live workspace goes either way: the snapshot is what was graded.)
	if res.Status == eval.TrialPassed {
		os.RemoveAll(dir)
	} else {
		// Keep what a reader needs to understand a non-pass: the graded tree, and what the MODEL
		// said. Without the attempt's own output, a trial where the model did nothing at all is
		// indistinguishable from one where it tried and was refused.
		os.RemoveAll(workspace) // the pre-grading copy is redundant once the snapshot exists
		if att.detail != "" {
			res.Detail = joinDetail(res.Detail, "the model's last words: "+att.detail)
		}
		res.Detail = joinDetail(res.Detail, "graded workspace kept at "+snap.Dir)
	}
	return res
}

// A subprocess status alone does not prove a blocked-only queue. The candidate has stopped;
// verify that the queue agrees. QueueCounts rejects links within the queue; check its
// candidate-controlled ancestor too, before reading anything through it.
func loopQueueBlocked(workspace string) bool {
	info, err := os.Lstat(filepath.Join(workspace, ".agent"))
	if err != nil || !info.IsDir() {
		return false
	}
	counts, _, err := tasks.QueueCounts(filepath.Join(workspace, eval.TasksRoot))
	return err == nil && counts.Blocked > 0 && counts.Todo+counts.Doing == 0
}

// attemptOutcome is what one candidate run tells us. `limited` means the PROVIDER refused (expired
// sign-in, rate limit, quota) — the model never worked, so this is a harness fact, not a result.
type attemptOutcome struct {
	detail  string
	code    int
	limited bool
}

// attempt runs the candidate once. It returns an error only when the box could not be launched or
// was killed — a non-zero exit is normal and left to the verifier, because an agent that exits
// non-zero may still have done the job.
func (r *trialRunner) attempt(ctx context.Context, t eval.Trial, workspace string) (attemptOutcome, error) {
	// A CLONE of the config, not `*r.app.cfg`: Config carries per-run maps, and a shallow copy shares
	// them — so one trial's model/effort/profile would leak into the next, which may be evaluating
	// the OTHER configuration, and concurrent workers would race on one map.
	cfg := evalTrialConfig(r.app.cfg)
	agentName, err := applyEvalConfiguration(cfg, t.Config)
	if err != nil {
		return attemptOutcome{}, err
	}
	adapter, ok := agent.Get(agentName)
	if !ok {
		return attemptOutcome{}, fmt.Errorf("unknown agent %q in configuration %q", agentName, t.Config.Label)
	}

	// Bounded capture: a candidate can print forever, and a trial must not be able to exhaust the
	// host's memory. Only the tail is kept — that is where a refusal or a final message is.
	out, errOut := &tailBuffer{max: 64 << 10}, &tailBuffer{max: 64 << 10}
	var stdout io.Writer = out
	probe := adapter.PlainOutputProbe()
	if probe != nil {
		stdout = io.MultiWriter(out, probe) // the adapter's own recognizer of a provider refusal
	}
	spec := box.RunSpec{
		Ctx:   ctx,
		Image: r.image,
		Repo:  workspace,
		// The same mount point the grader uses, so anything the candidate wrote with an absolute
		// path still resolves when its work is graded.
		Workdir: gradeWorkspaceDir,
		Cmd:     adapter.Headless(cfg, t.Case.Instruction),
		Agent:   agentName,
		Batch:   true,
		Quiet:   true,
		// The candidate DOES get its credentials — it has to call its model. This is the one place
		// in an eval where they are mounted; the grader never sees them.
		Homes: cfg.Homes,
		// No shared cache volume: it persists between trials, so one candidate could leave something
		// there for the next — which may be the other configuration.
		Cache:  false,
		Stdout: stdout,
		Stderr: errOut,
	}
	run := r.runBox
	if run == nil {
		run = func(s box.RunSpec) (int, error) { return box.Run(cfg, r.app.rt, s) }
	}
	code, err := run(spec)
	res := attemptOutcome{detail: gradeDetail(out.String(), errOut.String()), code: code}
	if probe != nil {
		res.limited = probe.Limited(code)
	}
	return res, err
}

// evalTrialConfig is the config a candidate runs under: a CLONE of the operator's (Config's per-run
// maps are shared by a shallow copy, so one trial's model/effort/account would otherwise leak into
// the next), with MCP servers removed.
//
// No MCP for a candidate, for two reasons that each disqualify on their own. They are a route OUT of
// the trial — to the operator's infrastructure, their tickets, a web search that might surface the
// answer — and they differ from machine to machine, so a run that used them would not be
// reproducible by anyone else. An eval measures the model on the case.
func evalTrialConfig(base *config.Config) *config.Config {
	cfg := base.Clone()
	cfg.MCPFile = ""
	return cfg
}

// applyEvalConfiguration selects on cfg exactly what the configuration names, and returns the agent
// to run. It is separate from launching so that "which credential, which model, which effort does
// this label actually mean" is decided — and tested — in one place: a run that is LABELLED with one
// credential but executed on another is not a wrong number, it is a comparison that means nothing.
func applyEvalConfiguration(cfg *config.Config, c eval.FrozenConfig) (string, error) {
	if c.Kind != eval.ConfigTarget {
		// A preset shapes a loop — roles, delegation, fallbacks — which a single headless call does
		// not exercise. Rather than silently evaluate only its lead, say so.
		return "", fmt.Errorf("configuration %q is a preset; presets are compared through loop suites, so this agent suite needs bare targets", c.Label)
	}
	target, err := agent.ParseTarget(c.Label)
	if err != nil {
		return "", fmt.Errorf("configuration %q: %w", c.Label, err)
	}
	name := target.Provider
	if target.Model != "" {
		cfg.SetActiveModel(name, target.Model)
	}
	if target.Effort != "" {
		cfg.SetActiveEffort(name, target.Effort)
	}
	// More than one account is a LADDER — fine for a loop that may rotate mid-run, but an eval
	// measures ONE configuration, so it is refused rather than silently narrowed to the first.
	if len(target.Accounts) > 1 {
		return "", fmt.Errorf("configuration %q names %d accounts; an eval configuration must pin exactly one", c.Label, len(target.Accounts))
	}
	if acct := target.Account(); acct != "" {
		cfg.SetActiveProfile(name, acct)
	}
	return name, nil
}

// tailBuffer keeps only the last max bytes written to it. os/exec copies stdout and stderr from
// separate goroutines, so it is mutex-guarded.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *tailBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

func (w *tailBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// sizeNote renders the change-size figures beside the verdict, and says plainly when a measurement
// is missing instead of printing a zero that would read as "no change".
func sizeNote(before eval.SizeMetrics, beforeErr error, after eval.SizeMetrics, afterErr error, skipped []string) string {
	var parts []string
	if beforeErr != nil || afterErr != nil {
		parts = append(parts, "change size not measured (cloc unavailable or failed)")
	} else {
		parts = append(parts, fmt.Sprintf("net code %+d (%d→%d lines)", eval.NetCodeGrowth(before, after), before.TotalCode(), after.TotalCode()))
	}
	if n := len(skipped); n > 0 {
		named := skipped
		if len(named) > 3 {
			named = named[:3]
		}
		parts = append(parts, fmt.Sprintf("%d workspace entries could not be snapshotted (%s)", n, strings.Join(named, ", ")))
	}
	return strings.Join(parts, "; ")
}

// harnessPaths are the workspace paths that belong to the harness rather than the candidate, and so
// are excluded from both the change-size figures and the did-anything-happen check. A loop scenario
// adds the queue (whose folders move as tasks are worked) and the loop's telemetry; the preset and
// the recipe are materialized before the baseline, so they cancel out on their own.
func harnessPaths(isLoop bool) []string {
	if !isLoop {
		return nil
	}
	return []string{eval.TasksRoot, ".agent/runs"}
}

// fail turns a step failure into the right status. An expired budget is a TIMEOUT wherever it is
// noticed — during preparation, the attempt or the snapshot — because "we ran out of time" is a
// different fact about the run than "the harness broke", and conflating them would hide a suite
// whose case budgets are simply too small.
func fail(ctx context.Context, detail string) eval.TrialResult {
	if ctx != nil && ctx.Err() != nil {
		return eval.TrialResult{Status: eval.TrialTimedOut, Detail: joinDetail("the trial ran out of its budget", detail)}
	}
	return eval.TrialResult{Status: eval.TrialError, Detail: detail}
}

// trialSlug is a filesystem-safe, collision-free directory name for one trial. Case ids are already
// validated to a safe alphabet; the configuration label is not (it carries ':' and '/'), so it is
// reduced to its index — the run record maps index back to label.
func trialSlug(t eval.Trial) string {
	return fmt.Sprintf("%s-c%d-r%d", t.Case.ID, t.ConfigIndex, t.Repetition)
}
