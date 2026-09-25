package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/eval"
)

// A loop scenario is the eval that actually answers the question people ask: does this preset, or
// this loop recipe, or this build of Coop, get more of a real queue finished? One case is a whole
// `coop loop` run — a fixture repository, an ordinary task queue, and the loop working it until the
// queue drains or the budget runs out — graded at the end by the case's independent verifier.
//
// It runs as a SUBPROCESS, not in this process, and that is a correctness requirement rather than a
// style choice. The loop engine takes no context: the only thing that stops it is a signal, and Go
// delivers a signal to every handler registered in the process. In-process, one trial hitting its
// deadline would tear down every other trial running beside it, and the two runs' narration would
// interleave on one stderr. A child process per trial gives each one its own signal domain, its own
// output, and an exit code we can read — and `exec.CommandContext` with a process-group kill turns
// the trial's deadline into something the loop will actually honor.
//
// The loop's exit code is CONTEXT, never the verdict. Whether the work is good is the verifier's
// call, made afterwards on the snapshot, exactly as for an agent case. The exit code only separates
// "the run happened" from "the run could not start" and "the run was cut off".

// Loop exit codes, from the engine's own banners: what each means for a trial.
const (
	loopExitDrained     = 0   // queue drained and the final review accepted it
	loopExitWorkRemains = 1   // work is left, or the final verification failed
	loopExitBlocked     = 3   // stopped on a human decision
	loopExitInterrupted = 130 // signalled — for us, the deadline
)

// materializeLoopScenario puts everything the loop needs into the trial workspace. It runs BEFORE
// the trial's size and "did anything happen" baselines are taken, so the harness's own files — the
// queue, the preset, the recipe — are never mistaken for the candidate's work.
func (r *trialRunner) materializeLoopScenario(t eval.Trial, workspace string) error {
	// The queue the loop will work: an ordinary Coop queue, materialized into the trial's own
	// repository. It is the case's template, never the developer's live queue.
	if err := eval.MaterializeQueue(filepath.Join(r.suite.Dir, t.Case.Tasks), workspace); err != nil {
		return err
	}
	// A preset is resolved from the repository the loop works (or a global directory), and a trial
	// workspace is a fresh fixture with neither — so the preset this scenario compares has to travel
	// into the workspace, or `coop loop frontier` would simply not find it. Staged once per run, so
	// every trial reads identical bytes.
	if t.Config.Kind == eval.ConfigPreset {
		staged, ok := r.presets[t.Config.Label]
		if !ok {
			return fmt.Errorf("preset %q was not staged for this run", t.Config.Label)
		}
		if err := eval.MaterializePreset(staged, workspace, t.Config.Label); err != nil {
			return err
		}
	}
	// The loop recipe under test. The engine reads .agent/loop.yaml from the repo it works and
	// nowhere else, so the FROZEN bytes — what this run is actually comparing — are written there.
	if len(t.Config.LoopConfig) > 0 {
		dest := filepath.Join(workspace, ".agent", "loop.yaml")
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, t.Config.LoopConfig, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// loopExecutable is which coop a loop trial runs — this build, the one whose identity the run
// recorded. A variable so a test can point it somewhere that is not a real loop.
var loopExecutable = os.Executable

// runLoopTrial runs one bounded `coop loop` against an already-materialized scenario and reports
// whether grading should proceed. It returns the loop's own output for the record.
func (r *trialRunner) runLoopTrial(ctx context.Context, t eval.Trial, workspace string) (attemptOutcome, error) {
	self, err := loopExecutable()
	if err != nil {
		return attemptOutcome{}, fmt.Errorf("locate this coop binary: %w", err)
	}
	// The configuration is the loop's positional: a preset name or a target. This is where a loop
	// suite differs from an agent suite — a preset is exactly what it evaluates.
	args := []string{"loop", t.Config.Label, "--no-preflight", "--no-mcp"}

	out, errOut := &tailBuffer{max: 256 << 10}, &tailBuffer{max: 64 << 10}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir = workspace
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.Stdin = nil
	// Its own process group, so a deadline signals the loop and its own children together. (The
	// boxes are NOT in this group — each runtime client gets its own — so container teardown is the
	// loop's own signal handler, which tears down the running box and reaps by run label.)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	// And a bounded wait after that signal: if the loop's teardown hangs, or a grandchild holds the
	// output pipe open, the trial must still end rather than pin a worker for the rest of the run.
	cmd.WaitDelay = 2 * time.Minute
	// The child's environment is PINNED, not merely inherited. An inherited COOP_REPO is the one
	// that matters most: `coop loop` resolves its repository from config, not from the working
	// directory, so an operator who has COOP_REPO set would have the trial run the loop against
	// THEIR OWN CHECKOUT with their credentials — real commits in a real repo — and then grade the
	// untouched trial workspace. Every knob that could redirect the loop is set explicitly here.
	cmd.Env = append(os.Environ(),
		"COOP_REPO="+workspace, // the trial's repository, never the operator's
		"COOP_TASKS=",          // the queue we materialized, not a configured one
		"COOP_IMAGE="+r.image,  // the same image the grader uses, never one built from the fixture
		"COOP_CACHE=0",         // no shared cache volume between trials
		"COOP_MCP_FILE=",       // no operator MCP servers inside a trial
		"COOP_RUN_ARGS=",       // no ambient host mounts or unrecorded runtime flags
		"COOP_NO_UPDATE_CHECK=1",
	)

	runErr := cmd.Run()
	detail := gradeDetail(out.String(), errOut.String())
	if cmd.ProcessState == nil {
		// It never started — a missing binary, a bad working directory. There is no exit code to
		// read, and nothing about the configuration to learn.
		return attemptOutcome{detail: detail}, fmt.Errorf("the loop could not be started: %w", runErr)
	}
	code := cmd.ProcessState.ExitCode()
	return attemptOutcome{detail: detail, code: code}, loopOutcome(code, ctx.Err())
}

// loopOutcome decides whether a finished loop run should be graded. Separated from running it so the
// decision — which is the whole judgement of this file — is testable without launching anything.
//
// nil means "grade it": the scenario ran, and how well it went is the verifier's call. An error
// means there is nothing to grade, and the trial is recorded as a timeout or a harness error rather
// than as anything about the configuration.
func loopOutcome(code int, ctxErr error) error {
	switch {
	case ctxErr != nil:
		return ctxErr // the deadline; the caller reports it as a timeout
	case code == loopExitInterrupted:
		return errors.New("the loop was interrupted before it finished")
	case code == loopExitDrained || code == loopExitWorkRemains || code == loopExitBlocked:
		// It ran. Work left over, or a task blocked on a human decision, is a real result about the
		// configuration — a loop that finishes less of the queue is exactly what we are measuring —
		// so it goes to the verifier like any other.
		return nil
	default:
		// 2 and -1 are refusals to start (no queue, missing image, unusable runtime) — the scenario
		// never ran, so there is nothing to grade and nothing to say about the configuration.
		return fmt.Errorf("the loop could not start (exit %d)", code)
	}
}
