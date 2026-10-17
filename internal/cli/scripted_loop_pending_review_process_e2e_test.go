//go:build providere2e

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/loop"
	taskpkg "github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
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
	firstCtx, firstCancel := context.WithTimeout(context.Background(), wait.Deadline)
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
	second := runLoopReview(t, suite, work, wait.Deadline)
	if second.Err != nil || second.ExitCode != 0 ||
		!strings.Contains(second.Stderr, "Resuming final review for 1 task completed in an earlier loop") ||
		!strings.Contains(second.Stdout+second.Stderr, "All tasks passed final review") {
		t.Fatalf("review resume = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}

	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID},
	})
	thirdCtx, thirdCancel := context.WithTimeout(context.Background(), wait.Deadline)
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

// The 2026-09-12 incident: a graceful Ctrl-C after the loop accepted a task, before its final
// review, and the next run treated the task as reviewed history. The review must resume, after a
// `coop sign` that replaced the task's commit in between, and under the review settings it started
// with rather than today's loop.yaml.
func TestProviderScriptedLoopPendingReviewSurvivesCtrlCAndSigning(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetLoopProcessRepo(t, suite)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })

	taskID := "pending-review-ctrl-c"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	work := loopRecoveryTarget("claude", "work-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
	base := loopProcessGit(t, suite, "rev-parse", "HEAD")
	suite.reset(t, loopRecoveryScenario(taskID, []loopProcessAttempt{{Target: work, Stage: "work", Result: "complete-gated"}}))
	process, err := procharness.Start(terminalLoopCommand(t, loopReviewCommand(suite, work)))
	if err != nil {
		t.Fatal(err)
	}
	defer process.Cleanup()
	awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", wait.Deadline)
	coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), wait.Deadline)
	if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), loopReviewBudget(1))
	first := process.Wait(ctx)
	cancel()
	if first.ExitCode != loop.LoopInterruptedExitCode || !strings.Contains(first.Stdout+first.Stderr, "Final review remains pending") ||
		!pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
		t.Fatalf("Ctrl-C after the task = exit %d err %v\nstdout:\n%s\nstderr:\n%s", first.ExitCode, first.Err, first.Stdout, first.Stderr)
	}
	accepted := taskpkg.CommitsForTask(suite.layout.Repo, base+"..HEAD", taskID)
	if len(accepted) != 1 {
		t.Fatalf("accepted task commits = %v, want one", accepted)
	}

	key := filepath.Join(t.TempDir(), "signing-key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-f", key, "-N", "", "-C", "coop-e2e").CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen is required to sign between the runs: %v\n%s", err, out)
	}
	signingConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(signingConfig, []byte("[gpg]\n\tformat = ssh\n[user]\n\tsigningkey = "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signEnv := slices.Clone(suite.env)
	for i, entry := range signEnv {
		if strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=") {
			signEnv[i] = "GIT_CONFIG_GLOBAL=" + signingConfig
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), wait.Deadline)
	signed := procharness.Run(ctx, procharness.Command{
		Path: suite.coopBin, Args: []string{"sign", "--from", base},
		Dir: suite.layout.Repo, Env: signEnv, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	})
	cancel()
	resigned := taskpkg.CommitsForTask(suite.layout.Repo, base+"..HEAD", taskID)
	if signed.Err != nil || signed.ExitCode != 0 || len(resigned) != 1 || resigned[0] == accepted[0] ||
		!strings.Contains(loopProcessGit(t, suite, "cat-file", "commit", resigned[0]), "\ngpgsig ") {
		t.Fatalf("coop sign = exit %d err %v, commits %v -> %v\nstderr:\n%s", signed.ExitCode, signed.Err, accepted, resigned, signed.Stderr)
	}

	// Today's loop.yaml adds a final check; the resumed review keeps the settings it started with.
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, []string{loopRecoveryTarget("claude", "verify-model", "work")}, 3)
	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: signoff, Stage: "signoff", Result: "pass"}}},
	})
	second := runLoopReview(t, suite, work, loopReviewBudget(1))
	if second.Err != nil || second.ExitCode != 0 ||
		!strings.Contains(second.Stderr, "Resuming final review for 1 task completed in an earlier loop") ||
		!strings.Contains(second.Stdout+second.Stderr, "All tasks passed final review") {
		t.Fatalf("review resume after signing = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}
	starts := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start")
	if len(starts) != 1 || processEnvironmentValue(starts[0].Environment, "ANTHROPIC_MODEL") != "signoff-model" {
		t.Fatalf("resumed process attempts = %#v, want the stored signoff only", starts)
	}
}

// A worker writes its own commit messages, so it can name a task that waits for final review. The
// completion check rejects that commit, because the task was archived before the worker started,
// and the next loop counts two commits for the task in its review range and starts no reviewer.
func TestProviderScriptedLoopPendingReviewRejectsAForgedSubjectCommit(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetLoopProcessRepo(t, suite)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })

	taskID := "pending-review-forger"
	subjectID := taskID + "-archive" // the fixture's forged trailer names this task
	// The loop works in-progress tasks before todo ones, so the subject completes first.
	seedLoopProcessTaskIn(t, suite.layout.Repo, stateInProgress, subjectID)
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	work := loopRecoveryTarget("claude", "work-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
	// Both attempts use one provider; the subject's empty decision commit leaves the fixture's change
	// file to the forger.
	attempts := []loopProcessAttempt{
		{TaskID: subjectID, Target: work, Stage: "work", Result: "decision-complete"},
		{TaskID: taskID, Target: work, Stage: "work", Result: "complete-forged-archive-binding"},
	}
	suite.reset(t, loopRecoveryScenario(taskID, attempts))
	first := runLoopReview(t, suite, work, loopReviewBudget(len(attempts)))
	if first.Err != nil || first.ExitCode != 1 || !strings.Contains(first.Stderr, "Task completion rejected") {
		t.Fatalf("forged subject commit = exit %d err %v\nstdout:\n%s\nstderr:\n%s", first.ExitCode, first.Err, first.Stdout, first.Stderr)
	}
	if starts := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start"); len(starts) != len(attempts) {
		t.Fatalf("worker attempts = %#v, want the subject's and the forger's", starts)
	}
	if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, subjectID)) ||
		!pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
		t.Fatal("the rejection did not keep the subject archived and the forger in progress")
	}
	if commits := taskpkg.CommitsForTask(suite.layout.Repo, "", subjectID); len(commits) != 2 {
		t.Fatalf("subject commits = %v, want its own and the forged one", commits)
	}

	suite.reset(t, loopProcessScenario{Version: 6, Provider: "claude", ProviderHomes: agents.Names(), Loop: loopProcessPlan{TaskID: taskID}})
	retry := runLoopReview(t, suite, work, wait.Deadline)
	if retry.Err != nil || retry.ExitCode != 1 ||
		!strings.Contains(retry.Stderr, "needs exactly one commit with \"Coop-Task: "+subjectID+"\"") ||
		!strings.Contains(retry.Stderr, "found 2") {
		t.Fatalf("review after the forged commit = exit %d err %v\nstdout:\n%s\nstderr:\n%s", retry.ExitCode, retry.Err, retry.Stdout, retry.Stderr)
	}
	if trace := readProcessTrace(t, suite.layout.Trace); len(trace) != 0 {
		t.Fatalf("the refused review launched a provider: %#v", trace)
	}
}

// Another session committed its task before this loop started and runs `coop tasks done` while a
// review box runs. The completion is host-receipted, so it succeeds and joins the final review at
// the task's one commit in the branch. A controller that dies during that review leaves a review
// window the next start replays; that start resumes the review instead of failing every time.
func TestProviderScriptedLoopPendingReviewTakesAHostCompletionFromBeforeTheRun(t *testing.T) {
	suite := newDirectProcessSuite(t)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })
	work := loopRecoveryTarget("claude", "work-model", "work")
	between := loopRecoveryTarget("codex", "between-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	setup := func(t *testing.T, taskID string) (hostID string) {
		t.Helper()
		resetLoopProcessRepo(t, suite)
		hostID = taskID + "-host" // the fixture's signoff checks that the prompt names this task
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		seedLoopProcessTaskIn(t, suite.layout.Repo, stateBlocked, hostID)
		if err := os.WriteFile(filepath.Join(suite.layout.Repo, "host.txt"), []byte("committed before the loop\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		loopProcessGit(t, suite, "add", "host.txt")
		loopProcessGit(t, suite, "commit", "-q", "-m", "outside agent work", "-m", "Coop-Task: "+hostID)
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		return hostID
	}
	hostDone := func(t *testing.T, hostID string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
		done := procharness.Run(ctx, procharness.Command{
			Path: suite.coopBin, Args: []string{"tasks", "done", hostID},
			Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20,
		})
		cancel()
		if done.Err != nil || done.ExitCode != 0 {
			t.Fatalf("host completion during the review = exit %d err %v\nstdout:\n%s\nstderr:\n%s", done.ExitCode, done.Err, done.Stdout, done.Stderr)
		}
	}

	t.Run("the run reviews it", func(t *testing.T) {
		taskID := "early-host-review"
		hostID := setup(t, taskID)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "pass-gated"},
			{Target: signoff, Stage: "signoff", Result: "pass-with-subject-and-host"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process, err := procharness.Start(loopReviewCommand(suite, work))
		if err != nil {
			t.Fatal(err)
		}
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", wait.Deadline) // the between review waits
		hostDone(t, hostID)
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), loopReviewBudget(len(attempts)))
		result := process.Wait(ctx)
		cancel()
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, "All tasks passed final review · 2/2 done") {
			t.Fatalf("review with the early host completion = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("a restart after the controller died replays its review", func(t *testing.T) {
		taskID := "early-host-restart"
		hostID := setup(t, taskID)
		suite.reset(t, loopRecoveryScenario(taskID, []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "wait"},
		}))
		process, err := procharness.Start(terminalLoopCommand(t, loopReviewCommand(suite, work)))
		if err != nil {
			t.Fatal(err)
		}
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", wait.Deadline)
		hostDone(t, hostID)
		coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), wait.Deadline)
		if err := syscall.Kill(coopPID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
		first := process.Wait(ctx)
		cancel()
		terminateOrphanedProviders(t, readProcessTrace(t, suite.layout.Trace))
		if first.ExitCode == 0 {
			t.Fatalf("killed review = exit %d err %v\nstdout:\n%s\nstderr:\n%s", first.ExitCode, first.Err, first.Stdout, first.Stderr)
		}

		suite.reset(t, loopRecoveryScenario(taskID, []loopProcessAttempt{{Target: signoff, Stage: "signoff", Result: "pass-with-subject-and-host"}}))
		second := runLoopReview(t, suite, work, loopReviewBudget(1))
		if second.Err != nil || second.ExitCode != 0 ||
			!strings.Contains(second.Stderr, "Resuming final review for 2 tasks completed in an earlier loop") ||
			!strings.Contains(second.Stderr, "All tasks passed final review · 2/2 done") {
			t.Fatalf("restart after the killed review = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
		}

		suite.reset(t, loopProcessScenario{Version: 6, Provider: "claude", ProviderHomes: agents.Names(), Loop: loopProcessPlan{TaskID: taskID}})
		third := runLoopReview(t, suite, work, loopReviewBudget(1))
		if third.Err != nil || third.ExitCode != 0 || strings.Contains(third.Stderr, "Resuming final review") {
			t.Fatalf("run after the settled review = exit %d err %v\nstdout:\n%s\nstderr:\n%s", third.ExitCode, third.Err, third.Stdout, third.Stderr)
		}
		if starts := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start"); len(starts) != 0 {
			t.Fatalf("the settled review launched a provider: %#v", starts)
		}
	})
}

// A signoff verdict covers the content it reviewed. Here the accepted task's commit is amended while
// the final check runs, keeping its message and trailer, and another session completes a task in the
// same window, which lets that check accept the moved HEAD. The check must not clear content no
// reviewer saw: the task goes back to signoff, is reviewed again beside the host completion, and only
// then is verified.
func TestProviderScriptedLoopPendingReviewReviewsAChangedAcceptedCommitAgain(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetLoopProcessRepo(t, suite)
	t.Cleanup(func() { logLoopProcessFailure(t, suite) })

	taskID := "pending-review-changed"
	hostID := taskID + "-host"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	seedLoopProcessTaskIn(t, suite.layout.Repo, stateBlocked, hostID)
	work := loopRecoveryTarget("claude", "work-model", "work")
	signoff := loopRecoveryTarget("claude", "signoff-model", "work")
	verify := loopRecoveryTarget("claude", "verify-model", "work")
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, []string{verify}, 3)
	attempts := []loopProcessAttempt{
		{Target: work, Stage: "work", Result: "complete"},
		{Target: signoff, Stage: "signoff", Result: "pass"},
		{Target: verify, Stage: "verify", Result: "pass-gated"},
		{Target: signoff, Stage: "signoff", Result: "pass-with-subject-and-host"},
		{Target: verify, Stage: "verify", Result: "pass-with-subject-and-host"},
	}
	suite.reset(t, loopRecoveryScenario(taskID, attempts))
	process, err := procharness.Start(loopReviewCommand(suite, work))
	if err != nil {
		t.Fatal(err)
	}
	defer process.Cleanup()
	awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", wait.Deadline) // the final check waits
	if err := os.WriteFile(filepath.Join(suite.layout.Repo, "loop-claude.txt"), []byte("changed after signoff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loopProcessGit(t, suite, "add", "loop-claude.txt")
	loopProcessGit(t, suite, "commit", "-q", "--amend", "--no-edit")
	if err := os.WriteFile(filepath.Join(suite.layout.Repo, "host.txt"), []byte("another session's work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loopProcessGit(t, suite, "add", "host.txt")
	loopProcessGit(t, suite, "commit", "-q", "-m", "host work", "-m", "Coop-Task: "+hostID)
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
	done := procharness.Run(ctx, procharness.Command{
		Path: suite.coopBin, Args: []string{"tasks", "done", hostID},
		Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20,
	})
	cancel()
	if done.Err != nil || done.ExitCode != 0 {
		t.Fatalf("host completion during the final check = exit %d err %v\nstderr:\n%s", done.ExitCode, done.Err, done.Stderr)
	}
	if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), loopReviewBudget(len(attempts)))
	result := process.Wait(ctx)
	cancel()
	if result.Err != nil || result.ExitCode != 0 ||
		!strings.Contains(result.Stderr, "1 accepted task changed after review. Reviewing again before finishing.") ||
		!strings.Contains(result.Stderr, "Another session completed 1 task during this review.") ||
		!strings.Contains(result.Stderr, "All tasks passed final review · 2/2 done") {
		t.Fatalf("changed accepted commit = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
	}
	assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
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
	awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", wait.Deadline)
	coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), wait.Deadline)
	if err := syscall.Kill(coopPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
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
	second := runLoopReview(t, suite, work, wait.Deadline)
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
	awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", wait.Deadline)
	coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), wait.Deadline)
	if err := syscall.Kill(coopPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait.Deadline)
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
	second := runLoopReview(t, suite, work, wait.Deadline)
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
	if err := taskpkg.CompleteTrustedTask(suite.layout.Repo, root, item); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), loopReviewBudget(2))
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

	// Importing the reviewed task again is an explicit request for another review.
	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{
			{Target: signoff, Stage: "signoff", Result: "pass"},
			{Target: verify, Stage: "verify", Result: "pass"},
		}},
	})
	ctx, cancel = context.WithTimeout(context.Background(), loopReviewBudget(2))
	second := procharness.Run(ctx, command)
	cancel()
	if second.Err != nil || second.ExitCode != 0 || !strings.Contains(second.Stderr, "Imported 1 archived task into pending final review") {
		t.Fatalf("reviewed re-import = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}
	if starts := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start"); len(starts) != 2 {
		t.Fatalf("reviewed re-import attempts = %#v, want signoff then verification again", starts)
	}
}
