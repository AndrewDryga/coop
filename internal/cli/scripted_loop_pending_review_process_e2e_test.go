//go:build providere2e

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	taskpkg "github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedLoopPendingReviewResumeProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetLoopProcessRepo(t, suite)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })

	taskID := "pending-review-restart"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	work := loopRecoveryTarget("claude", "work-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)

	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: work, Stage: "work", Result: "complete"}}},
	})
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 20*time.Second)
	first := procharness.Run(firstCtx, procharness.Command{
		Path: suite.coopBin, Args: []string{"loop", work, "--max-tasks", "1", "--no-preflight", "--no-mcp"},
		Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	})
	firstCancel()
	if first.Err != nil || first.ExitCode != 0 || !strings.Contains(first.Stderr, "Final review remains pending") {
		t.Fatalf("bounded first process = exit %d err %v\nstdout:\n%s\nstderr:\n%s", first.ExitCode, first.Err, first.Stdout, first.Stderr)
	}

	if err := os.Remove(filepath.Join(suite.layout.Repo, "loop-claude.txt")); err != nil {
		t.Fatal(err)
	}
	loopProcessGit(t, suite, "add", "-u")
	loopProcessGit(t, suite, "commit", "-qm", "reset fixture work file between cohorts")
	freshID := "fresh-after-pending-review"
	seedLoopProcessTask(t, suite.layout.Repo, freshID)
	currentSignoff := loopRecoveryTarget("claude", "current-signoff-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{currentSignoff}, nil, 3)
	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{
			{TaskID: taskID, Target: signoff, Stage: "signoff", Result: "pass"},
			{TaskID: freshID, Target: work, Stage: "work", Result: "complete"},
			{TaskID: freshID, Target: currentSignoff, Stage: "signoff", Result: "pass"},
		}},
	})
	second := runLoopReview(t, suite, work, 20*time.Second)
	if second.Err != nil || second.ExitCode != 0 ||
		!strings.Contains(second.Stderr, "Resuming final review for 1 task completed in an earlier loop") ||
		!strings.Contains(second.Stdout+second.Stderr, "All tasks passed final review") {
		t.Fatalf("review resume = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}

	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID},
	})
	thirdCtx, thirdCancel := context.WithTimeout(context.Background(), 20*time.Second)
	third := procharness.Run(thirdCtx, procharness.Command{
		Path: suite.coopBin, Args: []string{"loop", work, "--max-tasks", "1", "--no-preflight", "--no-mcp"},
		Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	})
	thirdCancel()
	if third.Err != nil || third.ExitCode != 0 || strings.Contains(third.Stderr, "Resuming final review") {
		t.Fatalf("third no-repeat process = exit %d err %v\nstdout:\n%s\nstderr:\n%s", third.ExitCode, third.Err, third.Stdout, third.Stderr)
	}
	if trace := readProcessTrace(t, suite.layout.Trace); len(trace) != 0 {
		t.Fatalf("third process launched a provider: %#v", trace)
	}
	if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
		t.Fatal("reviewed task did not remain archived")
	}
	if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, freshID)) {
		t.Fatal("second process did not continue with fresh queue work after settling prior review")
	}
}

func TestProviderScriptedLoopPendingVerificationSurvivesProcessDeath(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetLoopProcessRepo(t, suite)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })

	taskID := "pending-verification-restart"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	work := loopRecoveryTarget("claude", "work-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	verify := loopRecoveryTarget("claude", "verify-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, []string{verify}, 3)
	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
			{Target: verify, Stage: "verify", Result: "wait"},
		}},
	})
	process, err := procharness.Start(terminalLoopCommand(t, loopReviewCommand(suite, work)))
	if err != nil {
		t.Fatal(err)
	}
	defer process.Cleanup()
	awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 15*time.Second)
	coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), 5*time.Second)
	if err := syscall.Kill(coopPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	first := process.Wait(ctx)
	cancel()
	trace := readProcessTrace(t, suite.layout.Trace)
	terminateOrphanedProviders(t, trace)
	if first.ExitCode == 0 || !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
		t.Fatalf("killed verification = exit %d err %v\nstdout:\n%s\nstderr:\n%s", first.ExitCode, first.Err, first.Stdout, first.Stderr)
	}

	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: verify, Stage: "verify", Result: "pass"}}},
	})
	second := runLoopReview(t, suite, work, 20*time.Second)
	if second.Err != nil || second.ExitCode != 0 ||
		!strings.Contains(second.Stderr, "Resuming final review for 1 task completed in an earlier loop") ||
		!strings.Contains(second.Stdout+second.Stderr, "All tasks passed final review") {
		t.Fatalf("verification resume = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}
	secondTrace := readProcessTrace(t, suite.layout.Trace)
	starts := processEvents(secondTrace, "provider", "start")
	if len(starts) != 1 || processEnvironmentValue(starts[0].Environment, "ANTHROPIC_MODEL") != "verify-model" {
		t.Fatalf("resumed process attempts = %#v, want verify only", starts)
	}
}

func TestProviderScriptedLoopPendingSignoffResumesTheStartedRound(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetLoopProcessRepo(t, suite)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })

	taskID := "pending-signoff-restart"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	work := loopRecoveryTarget("claude", "work-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "wait"},
		}},
	})
	process, err := procharness.Start(terminalLoopCommand(t, loopReviewCommand(suite, work)))
	if err != nil {
		t.Fatal(err)
	}
	defer process.Cleanup()
	awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 15*time.Second)
	coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), 5*time.Second)
	if err := syscall.Kill(coopPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	first := process.Wait(ctx)
	cancel()
	terminateOrphanedProviders(t, readProcessTrace(t, suite.layout.Trace))
	if first.ExitCode == 0 || !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
		t.Fatalf("killed signoff = exit %d err %v\nstdout:\n%s\nstderr:\n%s", first.ExitCode, first.Err, first.Stdout, first.Stderr)
	}

	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: signoff, Stage: "signoff", Result: "pass"}}},
	})
	second := runLoopReview(t, suite, work, 20*time.Second)
	output := second.Stdout + second.Stderr
	if second.Err != nil || second.ExitCode != 0 || !strings.Contains(output, "Final review · Round 1 of 3") ||
		!strings.Contains(output, "All tasks passed final review") {
		t.Fatalf("signoff resume = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}
	starts := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start")
	if len(starts) != 1 || processEnvironmentValue(starts[0].Environment, "ANTHROPIC_MODEL") != "signoff-model" {
		t.Fatalf("resumed process attempts = %#v, want signoff only", starts)
	}
}

func TestProviderScriptedLoopExplicitPendingReviewImport(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetLoopProcessRepo(t, suite)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })

	taskID := "explicit-review-import"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	if err := os.WriteFile(filepath.Join(suite.layout.Repo, "imported.txt"), []byte("legacy accepted work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loopProcessGit(t, suite, "add", "imported.txt")
	loopProcessGit(t, suite, "commit", "-qm", "legacy accepted work\n\nCoop-Task: "+taskID)
	t.Setenv(taskpkg.TestLeaseAuthorityRootEnv,
		filepath.Join(suite.layout.Home, ".local", "state", "coop", "task-leases", taskpkg.LeaseAuthorityVersion))
	root := filepath.Join(suite.layout.Repo, tasksRoot)
	item, ok, err := taskpkg.CurrentTask(root, taskID)
	if err != nil || !ok {
		t.Fatalf("current import task = %+v, %v, %v", item, ok, err)
	}
	if err := taskpkg.CompleteTrustedTask(root, item); err != nil {
		t.Fatal(err)
	}

	work := loopRecoveryTarget("claude", "work-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	verify := loopRecoveryTarget("claude", "verify-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, []string{verify}, 3)
	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{
			{Target: signoff, Stage: "signoff", Result: "pass"},
			{Target: verify, Stage: "verify", Result: "pass"},
		}},
	})
	command := loopReviewCommand(suite, work)
	command.Args = append(command.Args, "--review-task", taskID)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	first := procharness.Run(ctx, command)
	cancel()
	if first.Err != nil || first.ExitCode != 0 || !strings.Contains(first.Stderr, "Imported 1 archived task into pending final review") {
		t.Fatalf("explicit import = exit %d err %v\nstdout:\n%s\nstderr:\n%s", first.ExitCode, first.Err, first.Stdout, first.Stderr)
	}
	starts := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start")
	if len(starts) != 2 || processEnvironmentValue(starts[0].Environment, "ANTHROPIC_MODEL") != "signoff-model" ||
		processEnvironmentValue(starts[1].Environment, "ANTHROPIC_MODEL") != "verify-model" {
		t.Fatalf("explicit import attempts = %#v, want signoff then verification", starts)
	}

	suite.reset(t, loopProcessScenario{Version: 6, Provider: "claude", ProviderHomes: agents.Names()})
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	second := procharness.Run(ctx, command)
	cancel()
	if second.ExitCode == 0 || !strings.Contains(second.Stderr, "already passed final review") {
		t.Fatalf("reviewed re-import = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}
	if starts := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start"); len(starts) != 0 {
		t.Fatalf("reviewed re-import launched provider: %#v", starts)
	}
}
