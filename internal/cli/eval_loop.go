package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/eval"
	"github.com/AndrewDryga/coop/internal/loop"
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
// output, and an exit code we can read. A guarded process group gives the loop time to tear down
// its box on cancellation, then stops descendants that survive its exit.
//
// The loop's exit code is CONTEXT, never the verdict. Whether the work is good is the verifier's
// call, made afterwards on the snapshot, exactly as for an agent case. The exit code only separates
// "the run happened" from "the run could not start" and "the run was cut off".

// Loop exit codes, from the engine's own banners: what each means for a trial.
const (
	loopExitDrained     = 0 // queue drained and the final review accepted it
	loopExitWorkRemains = 1 // work is left, or the final verification failed
	loopExitBlocked     = 3 // stopped on a human decision
)

// Loop cancellation may spend 30 seconds on private services and five more on exact-run container
// removal. Keep that cleanup chance before forcing the still-owned group down.
const loopTrialShutdownGrace = 45 * time.Second

// materializeLoopScenario puts everything the loop needs into the trial workspace. It runs BEFORE
// the trial's size and "did anything happen" baselines are taken, so the harness's own files — the
// queue, the preset, the recipe — are never mistaken for the candidate's work.
func (r *trialRunner) materializeLoopScenario(ctx context.Context, t eval.Trial, workspace string) error {
	// The queue the loop will work: an ordinary Coop queue, materialized into the trial's own
	// repository. It is the case's template, never the developer's live queue.
	if err := eval.MaterializeQueue(ctx, filepath.Join(r.suite.Dir, t.Case.Tasks), workspace); err != nil {
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
		if err := eval.MaterializePreset(ctx, staged, workspace, t.Config.Label); err != nil {
			return err
		}
	}
	// The loop recipe under test. The engine reads .agent/loop.yaml from the repo it works and
	// nowhere else, so the FROZEN bytes — what this run is actually comparing — are written there.
	if len(t.Config.LoopConfig) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		dest := filepath.Join(workspace, ".agent", "loop.yaml")
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, t.Config.LoopConfig, 0o644); err != nil {
			return err
		}
	}
	return ctx.Err()
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
	group, err := startLoopTrialGroup(ctx)
	if err != nil {
		return attemptOutcome{}, err
	}
	defer group.close()
	cmd := exec.Command(self, args...)
	cmd.Dir = workspace
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.Stdin = nil
	// Join the guardian's process group so a deadline signals the loop and its children together.
	// Boxes are not in this group; the loop's signal handler tears those down by run label.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group.pid()}
	// With no CommandContext, WaitDelay starts when the loop exits, not when its deadline fires.
	// A stray descendant cannot hold the output pipes indefinitely after the leader is gone.
	cmd.WaitDelay = time.Second
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
		"COOP_EVAL_DISABLE_WEB_TOOLS=1",
	)

	if err := ctx.Err(); err != nil {
		return attemptOutcome{}, err
	}
	if err := cmd.Start(); err != nil {
		return attemptOutcome{}, fmt.Errorf("the loop could not be started: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-ctx.Done():
		if err := group.signal(syscall.SIGTERM); err != nil {
			_ = group.signal(syscall.SIGKILL)
			_ = cmd.Process.Kill()
			return attemptOutcome{}, fmt.Errorf("stop the loop at its deadline: %w", err)
		}
		select {
		case runErr = <-done:
		case <-time.After(loopTrialShutdownGrace):
			if err := group.signal(syscall.SIGKILL); err != nil {
				_ = cmd.Process.Kill()
				return attemptOutcome{}, fmt.Errorf("force-stop the loop after cleanup grace: %w", err)
			}
			runErr = <-done
		}
	}
	// The guardian pins this group ID until here, so the final signal cannot hit a reused group.
	if err := group.signal(syscall.SIGKILL); err != nil {
		return attemptOutcome{}, fmt.Errorf("finish stopping the loop process group: %w", err)
	}
	detail := gradeDetail(out.String(), errOut.String())
	code := cmd.ProcessState.ExitCode()
	if ctx.Err() == nil && errors.Is(runErr, exec.ErrWaitDelay) {
		return attemptOutcome{detail: detail, code: code}, fmt.Errorf("the loop left a process holding its output open: %w", runErr)
	}
	return attemptOutcome{detail: detail, code: code}, loopOutcome(code, ctx.Err())
}

// Keep an owned member in the loop's process group until the final signal. Cmd.Wait reaps the
// loop leader before its pipe readers finish, so its PID alone is not safe to signal later.
type loopTrialGroup struct {
	guard *exec.Cmd
	input *os.File
}

func (g *loopTrialGroup) pid() int { return g.guard.Process.Pid }

func (g *loopTrialGroup) signal(sig syscall.Signal) error {
	return syscall.Kill(-g.pid(), sig)
}

func (g *loopTrialGroup) close() {
	_ = g.input.Close()
	_ = g.guard.Wait()
}

func startLoopTrialGroup(ctx context.Context) (*loopTrialGroup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		inputRead.Close()
		inputWrite.Close()
		return nil, err
	}
	// The shell uses only builtins: it ignores TERM, acknowledges readiness, then holds its stdin
	// open. Closing that pipe also releases it if the parent exits before the final signal.
	guard := exec.Command("/bin/sh", "-c", "trap '' TERM; printf x >&3; read _")
	guard.Stdin = inputRead
	guard.Env = []string{"PATH=/usr/bin:/bin"}
	guard.ExtraFiles = []*os.File{readyWrite}
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = guard.Start()
	inputRead.Close()
	readyWrite.Close()
	if err != nil {
		readyRead.Close()
		inputWrite.Close()
		return nil, fmt.Errorf("start loop process-group guard: %w", err)
	}
	group := &loopTrialGroup{guard: guard, input: inputWrite}
	ready := make(chan error, 1)
	go func() {
		var mark [1]byte
		_, err := io.ReadFull(readyRead, mark[:])
		readyRead.Close()
		ready <- err
	}()
	select {
	case err := <-ready:
		if err == nil {
			return group, nil
		}
		_ = group.signal(syscall.SIGKILL)
		group.close()
		return nil, fmt.Errorf("ready loop process-group guard: %w", err)
	case <-ctx.Done():
		_ = group.signal(syscall.SIGKILL)
		group.close()
		<-ready
		return nil, ctx.Err()
	}
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
	case code == loop.LoopInterruptedExitCode:
		return errors.New("the loop was interrupted before it finished")
	case code < 0:
		return fmt.Errorf("the loop was killed before it finished (exit %d)", code)
	case code == loopExitDrained || code == loopExitWorkRemains || code == loopExitBlocked:
		// It ran. Work left over, or a task blocked on a human decision, is a real result about the
		// configuration — a loop that finishes less of the queue is exactly what we are measuring —
		// so it goes to the verifier like any other.
		return nil
	default:
		// 2 is a refusal to start (no queue, missing image, unusable runtime) — the scenario
		// never ran, so there is nothing to grade and nothing to say about the configuration.
		return fmt.Errorf("the loop could not start (exit %d)", code)
	}
}
