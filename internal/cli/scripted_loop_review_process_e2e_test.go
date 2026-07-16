//go:build providere2e

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedLoopReviewProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)

	t.Run("four provider stage and usage matrix", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		t.Cleanup(func() { logLoopProcessFailure(t, suite) })
		taskID := "review-stage-matrix"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		verify := loopRecoveryTarget("grok", "verify-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, []string{verify}, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "pass"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
			{Target: verify, Stage: "verify", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReviewPTY(t, suite, work)
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout+result.Stderr, "queue verified done") {
			t.Fatalf("review stage matrix = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopReviewContracts(t, suite, trace, taskID, attempts, true)
		records := readLoopStageRecords(t, suite)
		if len(records) != 4 {
			t.Fatalf("review stage telemetry = %#v", records)
		}
		want := []struct {
			stage, provider, model, account string
			cost                            float64
			in, out                         int
		}{
			{"work", "claude", "work-model", "personal", 0.25, 101, 11},
			{"between", "codex", "between-model", "work", 0, 202, 22},
			{"signoff", "gemini", "signoff-model", "work", 0, 303, 33},
			{"verify", "grok", "verify-model", "work", 0, 408, 48},
		}
		for i, expected := range want {
			record := records[i]
			target, err := agents.ParseTarget(attempts[i].Target)
			if err != nil {
				t.Fatal(err)
			}
			if record.Stage != expected.stage || record.Provider != expected.provider || record.Model != expected.model ||
				record.Effort != target.Effort || record.Account != expected.account || record.Outcome != "success" || record.Exit != 0 || record.Retries != 0 ||
				record.Reopened != 0 || record.CostUSD != expected.cost || record.InTok != expected.in || record.OutTok != expected.out ||
				record.QueueTodo != 0 || record.QueueDoing != 0 || record.QueueDone != 1 {
				t.Fatalf("review stage telemetry[%d] = %#v, want %+v", i, record, expected)
			}
		}
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("signoff cannot complete an unowned task", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "signoff-unowned-completion"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "complete-extra"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		extraID := taskID + "-extra"
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, "completion rejected for unleased task(s) "+extraID) {
			t.Fatalf("signoff unowned completion = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, extraID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, extraID)) {
			t.Fatal("signoff's unowned completion was not restored")
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("signoff ownership rejection disturbed the reviewed task")
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("between rotation records terminal target", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "between-rotation"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		limited := loopRecoveryTarget("codex", "limited-review", "work")
		fallback := loopRecoveryTarget("gemini", "fallback-review", "work")
		signoff := loopRecoveryTarget("grok", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{limited, fallback}, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: limited, Stage: "between", Result: "rate-limit"},
			{Target: fallback, Stage: "between", Result: "pass"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, "switching to") {
			t.Fatalf("between rotation = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopReviewContracts(t, suite, trace, taskID, attempts, false)
		records := readLoopStageRecords(t, suite)
		if len(records) != 3 || records[1].Stage != "between" || records[1].Provider != "gemini" || records[1].Model != "fallback-review" ||
			records[1].Account != "work" || records[1].Outcome != "success" || records[1].Retries != 1 {
			t.Fatalf("between rotation telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("between reopen is reworked before signoff", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "between-reopen"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "reopen"},
			{Target: work, Stage: "work", Result: "repair-binding"},
			{Target: between, Stage: "between", Result: "pass"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("between reopen = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 5 || records[1].Stage != "between" || records[1].Reopened != 1 || records[1].QueueDoing != 1 || records[1].QueueDone != 0 ||
			records[3].Stage != "between" || records[3].Reopened != 0 || records[4].Stage != "signoff" {
			t.Fatalf("between reopen telemetry = %#v", records)
		}
		paths, err := loopStageTelemetryPaths(suite.layout.Repo)
		if err != nil || len(paths) != 1 {
			t.Fatalf("loop telemetry paths = %v, %v", paths, err)
		}
		raw, err := os.ReadFile(paths[0])
		if err != nil || bytes.Contains(raw, []byte(`"cost_usd"`)) || bytes.Contains(raw, []byte(`"in_tok"`)) || bytes.Contains(raw, []byte(`"out_tok"`)) {
			t.Fatalf("unavailable usage was serialized as synthetic zero: %s, %v", raw, err)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts, false)
	})

	t.Run("signoff round cap blocks repeated reopen", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "signoff-round-cap"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("codex", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "reopen"},
			{Target: work, Stage: "work", Result: "repair-binding"},
			{Target: signoff, Stage: "signoff", Result: "reopen"},
			{Target: work, Stage: "work", Result: "repair-binding"},
			{Target: signoff, Stage: "signoff", Result: "reopen"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 3 || !strings.Contains(result.Stderr, "signoff still reopening after 3 rounds") {
			t.Fatalf("signoff cap = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		blocked := filepath.Join(suite.layout.Repo, tasksRoot, stateBlocked, taskID)
		decision, err := os.ReadFile(filepath.Join(blocked, "decision.md"))
		if err != nil || !strings.Contains(string(decision), "after 3 rounds") {
			t.Fatalf("signoff cap decision = %q, %v", decision, err)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 6 {
			t.Fatalf("signoff cap telemetry = %#v", records)
		}
		for _, index := range []int{1, 3, 5} {
			if records[index].Stage != "signoff" || records[index].Reopened != 1 {
				t.Fatalf("signoff cap telemetry[%d] = %#v", index, records[index])
			}
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts, false)
	})

	t.Run("verify reopen exits unverified", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "verify-reopen"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		verify := loopRecoveryTarget("grok", "verify-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, []string{verify}, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
			{Target: verify, Stage: "verify", Result: "reopen"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, "review left 1 task actionable") {
			t.Fatalf("verify reopen = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if _, err := os.Stat(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID, "task.md")); err != nil {
			t.Fatalf("verify reopen did not leave task actionable: %v", err)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 3 || records[2].Stage != "verify" || records[2].Reopened != 1 || records[2].QueueDoing != 1 || records[2].QueueDone != 0 {
			t.Fatalf("verify reopen telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts, false)
	})

	t.Run("failed review that reopens stops without retry", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "failed-review-reopen"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "reopen-ordinary"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, "failed review stage also reopened "+taskID) {
			t.Fatalf("failed review reopen = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
			t.Fatal("failed review did not preserve its reopened task")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 || records[1].Stage != "signoff" || records[1].Outcome != "process_failure" ||
			records[1].Exit != 23 || records[1].Retries != 0 || records[1].Reopened != 1 {
			t.Fatalf("failed review reopen telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts, false)
	})

	t.Run("hard stop after review reopen remains interrupted", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "hard-stop-after-review-reopen"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, nil, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete-gated"},
			{Target: between, Stage: "between", Result: "reopen-wait"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecoveryPTY(t, suite, work)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), 5*time.Second)
		if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		awaitLoopTraceEventCount(t, suite.layout.Trace, "provider", "ready", 2, 10*time.Second)
		if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result := process.Wait(ctx)
		cancel()
		if result.ExitCode != loopInterruptedExitCode || !strings.Contains(result.Stdout+result.Stderr, "interrupted") || strings.Contains(result.Stderr, "completion ownership") {
			t.Fatalf("hard stop after reopen = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
			t.Fatal("interrupted review did not preserve its reopened task")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 || records[1].Stage != "between" || records[1].Outcome != "interrupted" || records[1].Exit != loopInterruptedExitCode || records[1].Reopened != 1 {
			t.Fatalf("hard stop after reopen telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("signoff output exhaustion fails closed", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "signoff-output-cap"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("codex", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{{Target: work, Stage: "work", Result: "complete"}}
		for range 6 {
			attempts = append(attempts, loopProcessAttempt{Target: signoff, Stage: "signoff", Result: "output-limit"})
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 30*time.Second)
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, "review stage reached the model output limit 6 times") || strings.Contains(result.Stderr, "queue verified done") {
			t.Fatalf("signoff output cap = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if _, err := os.Stat(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID, "task.md")); err != nil {
			t.Fatalf("failed signoff changed completed task state: %v", err)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 || records[1].Stage != "signoff" || records[1].Outcome != "output_limit" || records[1].Exit != 23 || records[1].Retries != 5 || records[1].Reopened != 0 {
			t.Fatalf("signoff output cap telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts, false)
	})

	t.Run("soft stop still runs completed task audit", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "soft-stop-audit"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{loopRecoveryTarget("gemini", "must-not-run", "work")}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete-gated"},
			{Target: between, Stage: "between", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecoveryPTY(t, suite, work)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), 5*time.Second)
		if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		awaitLoopProcessOutput(t, process, "finishing this iteration, then stopping", 5*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result := process.Wait(ctx)
		cancel()
		if result.ExitCode != loopInterruptedExitCode || !strings.Contains(result.Stdout+result.Stderr, "between-tasks audit") || strings.Contains(result.Stdout+result.Stderr, "running signoff") {
			t.Fatalf("soft-stop audit = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopReviewContracts(t, suite, trace, taskID, attempts, true)
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 || records[0].Stage != "work" || records[1].Stage != "between" || records[1].Outcome != "success" {
			t.Fatalf("soft-stop audit telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("hard stop interrupts between audit backoff", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "hard-stop-audit-backoff"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{loopRecoveryTarget("gemini", "must-not-run", "work")}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete-gated"},
			{Target: between, Stage: "between", Result: "rate-limit"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecoveryPTY(t, suite, work)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), 5*time.Second)
		if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		awaitLoopProcessOutput(t, process, "between-tasks audit", 5*time.Second)
		awaitLoopTraceEventCount(t, suite.layout.Trace, "provider", "exit", 2, 5*time.Second)
		awaitLoopProcessOutput(t, process, "model rate limited — waiting", 5*time.Second)
		if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result := process.Wait(ctx)
		cancel()
		if result.ExitCode != loopInterruptedExitCode || !strings.Contains(result.Stdout+result.Stderr, "interrupted") {
			t.Fatalf("hard-stop audit = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopReviewContracts(t, suite, trace, taskID, attempts, true)
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 || records[1].Stage != "between" || records[1].Outcome != "interrupted" || records[1].Exit != loopInterruptedExitCode || records[1].Provider != "codex" || records[1].Retries != 1 || records[1].QueueDone != 1 || records[1].QueueDoing != 0 {
			t.Fatalf("hard-stop audit telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, trace)
	})
}

func awaitLoopTraceEventCount(t *testing.T, path, source, event string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count := 0
		for _, record := range readProcessTrace(t, path) {
			if record.Source == source && record.Event == event {
				count++
			}
		}
		if count >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s/%s trace events", want, source, event)
}

func writeLoopReviewConfig(t *testing.T, repo string, between, signoff, verify []string, rounds int) {
	t.Helper()
	var body strings.Builder
	body.WriteString("preflight:\n  enabled: false\n")
	if len(between) > 0 {
		body.WriteString("between:\n  enabled: true\n  agent:\n")
		for _, target := range between {
			fmt.Fprintf(&body, "    - %s\n", target)
		}
		body.WriteString("  prompt: FIXTURE BETWEEN\n")
	}
	body.WriteString("signoff:\n  agent:\n")
	for _, target := range signoff {
		fmt.Fprintf(&body, "    - %s\n", target)
	}
	fmt.Fprintf(&body, "  rounds: %d\n  prompt: FIXTURE SIGNOFF\n", rounds)
	if len(verify) > 0 {
		body.WriteString("verify:\n  enabled: true\n  agent:\n")
		for _, target := range verify {
			fmt.Fprintf(&body, "    - %s\n", target)
		}
		body.WriteString("  prompt: FIXTURE VERIFY\n")
	}
	body.WriteString("mcp: false\n")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "loop.yaml"), []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runLoopReview(t *testing.T, suite *directProcessSuite, target string, timeout time.Duration) procharness.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return procharness.Run(ctx, loopReviewCommand(suite, target))
}

func runLoopReviewPTY(t *testing.T, suite *directProcessSuite, target string) procharness.Result {
	t.Helper()
	command := terminalLoopCommand(t, loopReviewCommand(suite, target))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return procharness.Run(ctx, command)
}

func loopReviewCommand(suite *directProcessSuite, target string) procharness.Command {
	return procharness.Command{
		Path: suite.coopBin, Args: []string{"loop", target, "--no-preflight", "--no-mcp"},
		Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	}
}

func assertLoopReviewContracts(t *testing.T, suite *directProcessSuite, trace []*processTrace, taskID string, attempts []loopProcessAttempt, streaming bool) {
	t.Helper()
	var runs, starts, exits []*processTrace
	for i, event := range trace {
		if event.Version != 1 || event.Sequence != i+1 || event.Event == "error" {
			t.Fatalf("invalid review trace event %d: %#v", i, event)
		}
		switch {
		case event.Source == "runtime" && event.Event == "run":
			runs = append(runs, event)
		case event.Source == "provider" && event.Event == "start":
			starts = append(starts, event)
		case event.Source == "provider" && event.Event == "exit":
			exits = append(exits, event)
		}
	}
	if len(runs) != len(attempts) || len(starts) != len(attempts) || len(exits) != len(attempts) {
		t.Fatalf("review trace counts runs/starts/exits = %d/%d/%d, want %d\n%#v", len(runs), len(starts), len(exits), len(attempts), trace)
	}
	for i, attempt := range attempts {
		target, err := agents.ParseTarget(attempt.Target)
		if err != nil {
			t.Fatal(err)
		}
		argv := loopProcessArgv(target.Provider, target.Model, target.Effort, "fixture-prompt-sentinel")
		var ok bool
		if streaming {
			argv, ok = iterationCommand(target.Provider, argv, nil, true)
			if !ok {
				t.Fatalf("review attempt %d provider %q has no streaming command", i, target.Provider)
			}
		}
		wantArgv := processTraceArgv(argv)
		promptIndex, ok := loopPromptIndex(target.Provider, argv)
		if !ok || promptIndex >= len(starts[i].Argv) {
			t.Fatalf("review attempt %d has no provider prompt position in %q", i, starts[i].Argv)
		}
		// Prompt contents are independently stage-checked inside the provider fixture. The trace
		// stores only their digest, so retain that one value while independently deriving every
		// native executable/model/effort/streaming argument around it.
		wantArgv[promptIndex] = starts[i].Argv[promptIndex]
		run := runs[i].Run
		if run == nil || run.Provider != target.Provider || !reflect.DeepEqual(run.ProviderArgv, wantArgv) || !reflect.DeepEqual(starts[i].Argv, wantArgv) {
			t.Fatalf("review attempt %d runtime/start argv = %#v / %q, want %q", i, run, starts[i].Argv, wantArgv)
		}
		if attempt.Stage == "work" {
			assertProcessMounts(t, suite.layout, target.Provider, target.Account(), run.Mounts)
		} else {
			assertLoopReviewMounts(t, suite.layout, target.Provider, target.Account(), run.Mounts)
		}
		assertDirectEnvironment(t, run.Environment, suite.allCredKeys, target.Provider, directProviderContracts[target.Provider], target.Model, target.Effort)
		assertDirectEnvironment(t, starts[i].Environment, suite.allCredKeys, target.Provider, directProviderContracts[target.Provider], target.Model, target.Effort)
		if streaming {
			agent, ok := agents.Get(target.Provider)
			if !ok {
				t.Fatalf("review attempt %d provider %q is unregistered", i, target.Provider)
			}
			for _, flag := range agent.Stream().Flags {
				if processSafeFlag(flag) && !slices.Contains(run.ProviderArgv, flag) {
					t.Fatalf("review attempt %d %s missing streaming flag %q in %q", i, attempt.Stage, flag, run.ProviderArgv)
				}
			}
		}
		wantExit := 0
		if attempt.Result != "complete" && attempt.Result != "complete-delay" && attempt.Result != "complete-gated" && attempt.Result != "repair-binding" && attempt.Result != "pass" && attempt.Result != "reopen" {
			wantExit = 23
		}
		if attempt.Result == "wait" {
			wantExit = 130
		}
		if exits[i].ExitCode == nil || *exits[i].ExitCode != wantExit {
			t.Fatalf("review attempt %d provider exit = %#v, want %d", i, exits[i], wantExit)
		}
	}
}

func loopPromptIndex(provider string, argv []string) (int, bool) {
	if provider == "codex" && len(argv) > 0 {
		return len(argv) - 1, true
	}
	for i := len(argv) - 2; i >= 0; i-- {
		if argv[i] == "-p" {
			return i + 1, true
		}
	}
	return 0, false
}

func assertLoopReviewMounts(t *testing.T, layout procharness.Layout, provider, account string, mounts []processMount) {
	t.Helper()
	repo := processTracePath(layout.Root, layout.Repo)
	queue := repo + "/.agent/tasks"
	profile := processTracePath(layout.Root, filepath.Join(layout.Config, provider, "profiles", account))
	profileTarget := "<container>/home/node/." + provider
	foundRepo, foundQueue, foundProfile := false, false, false
	for _, mount := range mounts {
		switch {
		case mount.Source == repo && mount.Target == repo && mount.ReadOnly:
			foundRepo = true
		case mount.Source == queue && mount.Target == queue && !mount.ReadOnly:
			foundQueue = true
		case mount.Source == profile && mount.Target == profileTarget && !mount.ReadOnly:
			foundProfile = true
		case mount.Named:
			if mount.ReadOnly || (mount.Source != "coop-cache" && mount.Source != "coop-asdf") {
				t.Errorf("invalid review named mount %#v", mount)
			}
		case !mount.ReadOnly:
			t.Errorf("unexpected writable review mount %#v", mount)
		case !strings.HasPrefix(mount.Source, "<root>/tmp/coop-"):
			t.Errorf("review read-only mount did not come from generated temp state: %#v", mount)
		}
	}
	if !foundRepo || !foundQueue || !foundProfile {
		t.Fatalf("review mounts repo/queue/profile = %v/%v/%v in %#v", foundRepo, foundQueue, foundProfile, mounts)
	}
}
