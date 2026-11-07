package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/ui"
)

func projectBuildPromptEligible(spec box.RunSpec, session, terminal bool) bool {
	ag, ok := agents.Get(spec.Agent)
	return ok && len(spec.Cmd) > 0 && ag.InteractiveInvocation(spec.Cmd) && session && terminal && spec.AgentCommand && !spec.Login && !spec.ControllerJob &&
		!spec.Batch && !spec.Quiet && !spec.ForceNoTTY && spec.SupervisorID == "" &&
		os.Getenv("COOP_BOX") == "" && os.Getenv(box.ControllerJobEnv) == ""
}

// stoppedLaunch is a launch a signal stopped while it was still being admitted. The stop line is
// already printed, so it is reported, and the status is the one the shell gives that signal. It is a
// cancellation to anything that asks, as a stop once the box runs is.
type stoppedLaunch struct{ status int }

func (e *stoppedLaunch) Error() string { return "stopped before the box started" }

func (e *stoppedLaunch) Unwrap() error { return context.Canceled }

// admissionStatus is the exit status of a launch refused or failed before its box: the shell's
// status for a signal that stopped it, or 1.
func admissionStatus(err error) int {
	var stopped *stoppedLaunch
	if errors.As(err, &stopped) {
		return stopped.status
	}
	return 1
}

// admitUntilInterrupted is box.AdmitNetwork for a launch a person is watching. The first filtered
// launch on a host qualifies it inside admission, which takes seconds and runs a box of its own,
// before the launch has any stop handling: a Ctrl-C or a closed terminal killed Coop there with that
// box and its gateway still running, and said nothing. Now the signal cancels admission, whose
// cleanup removes what it made by name, and the launch ends as a stop before the box started
// always does. A signal that lands as admission finishes still stops the launch: nothing else saw it.
func (a *app) admitUntilInterrupted(cfg *config.Config, rt runtime.Runtime, spec box.RunSpec) (*box.CapturedEgress, error) {
	admit := box.AdmitNetwork
	if a.admitNetwork != nil {
		admit = a.admitNetwork
	}
	var capture *box.CapturedEgress
	status, err := box.UntilInterrupted(func(ctx context.Context) (err error) {
		spec.Ctx = ctx
		capture, err = admit(cfg, rt, spec, a.network.admission())
		return err
	})
	if status == 0 {
		return capture, err
	}
	_ = capture.Close()
	box.NoteStoppedBeforeStart()
	return nil, ui.Reported(&stoppedLaunch{status: status})
}

func (a *app) admitInteractiveNetwork(spec box.RunSpec, session bool) (capture *box.CapturedEgress, result error) {
	capture, err := a.admitUntilInterrupted(a.cfg, a.rt, spec)
	var required *box.ProjectBuildRequired
	if !errors.As(err, &required) || required.Refused || !projectBuildPromptEligible(spec, session, ui.IsTerminal(os.Stdin) && ui.IsTerminal(os.Stderr)) {
		return capture, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rt, err := a.rt.FreezeCompose(ctx)
	if err != nil {
		return nil, err
	}
	prepared, err := box.PrepareFilteredProjectBuild(ctx, a.cfg, spec)
	if err != nil {
		return nil, err
	}
	// Closing happens on decline and cancellation too. The snapshot is never mounted into a box.
	defer func() { result = errors.Join(result, prepared.Close()) }()
	if err := confirmProjectBuild(ctx, filepath.Join(prepared.Directory(), prepared.Dockerfile()), required.Reason, func() bool {
		return confirmTerminalPrompt(ctx, "Build and continue?")
	}); err != nil {
		return nil, err
	}
	if err := prepared.Build(ctx, rt, a.cfg, os.Stdout, os.Stderr); err != nil {
		return nil, buildFailure("Could not build the project box", err, a.rt.Name, "coop build --egress filtered")
	}
	if err := prepared.Close(); err != nil {
		return nil, err
	}
	ui.Pass("Project box built")
	// Re-admit once, not a rebuild loop. Changes after consent still require a new explicit build,
	// and the ordinary final launch proofs remain the last authority before execution.
	return a.admitUntilInterrupted(a.cfg, rt, spec)
}

func confirmProjectBuild(ctx context.Context, dockerfile, reason string, confirm func() bool) error {
	ui.Section("Preparing the project box")
	ui.Note("  %s", upperFirstSentence(reason))
	ui.Note("  Dockerfile: %s", dockerfile)
	ui.Note("  Review this saved snapshot and its copied build files before continuing.")
	ui.Note("  Only these saved files will be built; later checkout edits are not approved.")
	ui.Note("  This build uses ordinary networking; the agent will still use filtered access.")
	ui.Note("")
	if err := ctx.Err(); err != nil {
		return err
	}
	confirmed := confirm()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !confirmed {
		return errors.New("cancelled. No project image was built")
	}
	return nil
}
