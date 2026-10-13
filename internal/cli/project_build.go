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
	"github.com/AndrewDryga/coop/internal/ui"
)

func projectBuildPromptEligible(spec box.RunSpec, session, terminal bool) bool {
	ag, ok := agents.Get(spec.Agent)
	return ok && len(spec.Cmd) > 0 && ag.InteractiveInvocation(spec.Cmd) && session && terminal && spec.AgentCommand && !spec.Login && !spec.ControllerJob &&
		!spec.Batch && !spec.Quiet && !spec.ForceNoTTY && !spec.Mode.Restricted() && spec.SupervisorID == "" &&
		os.Getenv("COOP_BOX") == "" && os.Getenv(box.ControllerJobEnv) == ""
}

func (a *app) admitInteractiveNetwork(spec box.RunSpec, session bool) (capture *box.CapturedEgress, result error) {
	capture, err := box.AdmitNetwork(a.cfg, a.rt, spec, a.network.admission())
	var required *box.ProjectBuildRequired
	if !errors.As(err, &required) || !projectBuildPromptEligible(spec, session, ui.IsTerminal(os.Stdin) && ui.IsTerminal(os.Stderr)) {
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
	return box.AdmitNetwork(a.cfg, rt, spec, a.network.admission())
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
