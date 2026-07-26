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
	"github.com/AndrewDryga/coop/internal/loopcfg"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedLoopReviewProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)
	commitUnbound := func(t *testing.T, taskID, position string) string {
		t.Helper()
		name := taskID + "-" + position + "-unbound.txt"
		if err := os.WriteFile(
			filepath.Join(suite.layout.Repo, name),
			[]byte(position+" unbound history\n"),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		loopProcessGit(t, suite, "add", name)
		loopProcessGit(t, suite, "commit", "-q", "-m", "fixture: "+position+" unbound history")
		return loopProcessGit(t, suite, "rev-parse", "HEAD")
	}

	t.Run("Codex footer and echo envelopes preserve PASS and FAIL receipts", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		t.Cleanup(func() { logLoopProcessFailure(t, suite) })
		taskID := "codex-review-wrapper-matrix"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("codex", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "reopen-codex-footer"},
			{Target: work, Stage: "work", Result: "repair-review-binding"},
			{Target: between, Stage: "between", Result: "pass-codex-footer-echo"},
			{Target: signoff, Stage: "signoff", Result: "pass-codex-echo-footer"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReviewPTY(t, suite, work)
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout+result.Stderr, "queue verified done") || strings.Contains(result.Stderr, "review verdict invalid") {
			t.Fatalf("Codex review wrapper matrix = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("Codex wrapper matrix did not leave the reviewed task done")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != len(attempts) || records[1].Reopened != 1 || records[3].Reopened != 0 || records[4].Reopened != 0 {
			t.Fatalf("Codex wrapper matrix telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("background review receipts are discarded and retried with explicit telemetry", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		t.Cleanup(func() { logLoopProcessFailure(t, suite) })
		taskID := "review-background-handoff"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "background-drained-review"},
			{Target: between, Stage: "between", Result: "background-timeout-review"},
			{Target: between, Stage: "between", Result: "pass"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(result.Stderr, "discarding its receipt and starting a fresh observed attempt (1/3)") ||
			!strings.Contains(result.Stderr, "discarding its receipt and starting a fresh observed attempt (2/3)") ||
			!strings.Contains(result.Stdout+result.Stderr, "queue verified done") {
			t.Fatalf("background review handoff = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("background review attempts changed the completed subject")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 5 ||
			records[0].Outcome != "success" ||
			records[1].Outcome != "background_drained" ||
			records[2].Outcome != "background_timeout" ||
			records[3].Outcome != "success" ||
			records[4].Outcome != "success" {
			t.Fatalf("background review telemetry = %#v", records)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopReviewContracts(t, suite, trace, taskID, attempts)
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("Codex split footer echo is accepted without a malformed-verdict retry", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		t.Cleanup(func() { logLoopProcessFailure(t, suite) })
		taskID := "codex-review-split-footer-echo"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "pass-codex-split-footer-echo"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReviewPTY(t, suite, work)
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(result.Stdout+result.Stderr, "queue verified done") ||
			strings.Contains(result.Stderr, "structured verdict was malformed") {
			t.Fatalf("Codex split footer echo = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("Codex split footer echo did not leave the reviewed task done")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != len(attempts) || records[1].Stage != "between" || records[1].Reopened != 0 {
			t.Fatalf("Codex split footer echo telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("malformed verdict gets one corrected full-review attempt at every review stage", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		t.Cleanup(func() { logLoopProcessFailure(t, suite) })
		taskID := "review-verdict-correction-matrix"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		verify := loopRecoveryTarget("grok", "verify-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, []string{verify}, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "malformed-review"},
			{Target: between, Stage: "between", Result: "pass-corrected"},
			{Target: signoff, Stage: "signoff", Result: "malformed-review"},
			{Target: signoff, Stage: "signoff", Result: "pass-corrected"},
			{Target: verify, Stage: "verify", Result: "malformed-review"},
			{Target: verify, Stage: "verify", Result: "reopen-corrected"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReviewPTY(t, suite, work)
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stdout+result.Stderr, "review left 1 task actionable") {
			t.Fatalf("review verdict correction matrix = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("valid corrected FAIL verdict was not applied exactly once")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != len(attempts) {
			t.Fatalf("corrected review telemetry = %#v", records)
		}
		want := []struct {
			stage       string
			in, out     int
			doing, done int
			reopened    int
		}{
			{"work", 101, 11, 0, 1, 0},
			{"between", 202, 22, 0, 1, 0},
			{"between", 202, 22, 0, 1, 0},
			{"signoff", 303, 33, 0, 1, 0},
			{"signoff", 303, 33, 0, 1, 0},
			{"verify", 408, 48, 0, 1, 0},
			{"verify", 408, 48, 1, 0, 1},
		}
		for i, record := range records {
			if record.Stage != want[i].stage || record.Outcome != "success" || record.Exit != 0 ||
				record.InTok != want[i].in || record.OutTok != want[i].out ||
				record.QueueDoing != want[i].doing || record.QueueDone != want[i].done ||
				record.Reopened != want[i].reopened {
				t.Fatalf("corrected review telemetry[%d] = %#v", i, record)
			}
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("two malformed signoff verdicts fail closed", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		t.Cleanup(func() { logLoopProcessFailure(t, suite) })
		taskID := "review-verdict-double-malformed"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "malformed-review"},
			{Target: signoff, Stage: "signoff", Result: "malformed-review-corrected"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, "review verdict invalid") {
			t.Fatalf("double-malformed signoff = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
			t.Fatal("double-malformed verdict changed the review subject")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != len(attempts) || records[1].Stage != "signoff" || records[2].Stage != "signoff" ||
			records[1].Reopened != 0 || records[2].Reopened != 0 {
			t.Fatalf("double-malformed telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

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
		assertLoopReviewContracts(t, suite, trace, taskID, attempts)
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

	t.Run("mid-run loop.yaml edit warns once and keeps the startup snapshot", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		t.Cleanup(func() { logLoopProcessFailure(t, suite) })
		taskID := "loop-config-drift"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		configPath := filepath.Join(suite.layout.Repo, ".agent", "loop.yaml")
		startupConfig, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete-gated"},
			{Target: between, Stage: "between", Result: "pass"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process, err := procharness.Start(loopReviewCommand(suite, work))
		if err != nil {
			t.Fatal(err)
		}
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		// The gated work box holds the run mid-iteration: rewrite the review prompts on disk —
		// the "audit-prompt fix committed mid-run and believed active" incident — then release.
		edited := bytes.ReplaceAll(startupConfig, []byte("FIXTURE"), []byte("EDITED"))
		if err := os.WriteFile(configPath, edited, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		result := process.Wait(ctx)
		cancel()
		combined := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(combined, "queue verified done") {
			t.Fatalf("config drift run = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if want := "loop config: " + loopcfg.File + " (sha256 " + loopcfg.Digest(startupConfig) + ")"; !strings.Contains(combined, want) {
			t.Fatalf("missing startup snapshot announcement %q\nstdout:\n%s\nstderr:\n%s", want, result.Stdout, result.Stderr)
		}
		// One actionable warning for the new digest across BOTH post-edit launches (between and
		// signoff) — not one per launch, and never a silent continue.
		if got := strings.Count(combined, "restart to apply"); got != 1 {
			t.Fatalf("drift warnings = %d, want exactly 1\nstdout:\n%s\nstderr:\n%s", got, result.Stdout, result.Stderr)
		}
		if !strings.Contains(combined, loopcfg.Digest(edited)) {
			t.Fatalf("drift warning does not name the new digest %s\nstderr:\n%s", loopcfg.Digest(edited), result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("config drift run did not leave the reviewed task done")
		}
		// The fixture itself proved the active configuration never changed: both reviews ran and
		// verifyLoopPrompt accepted only the ORIGINAL "FIXTURE BETWEEN"/"FIXTURE SIGNOFF" prompts.
		records := readLoopStageRecords(t, suite)
		if len(records) != 3 || records[0].Stage != "work" || records[1].Stage != "between" || records[2].Stage != "signoff" {
			t.Fatalf("config drift telemetry = %#v", records)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopReviewContracts(t, suite, trace, taskID, attempts)
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

	t.Run("concurrent host completion during passing signoff forces another round", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "signoff-concurrent-completion"
		hostID := taskID + "-host"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		// The parallel human session's task: parked in 50_blocked so the work loop cannot claim
		// it, then finished through the real host CLI while the round-1 signoff box is running.
		seedLoopProcessTaskIn(t, suite.layout.Repo, stateBlocked, hostID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		// Round 1 PASSES its subject and reopens nothing — without the forced round, this is
		// exactly where the loop would exit past the foreign completion. Round 2 must run with
		// the host completion as its ONLY subject (the fixture rejects a prompt that omits it,
		// and the one-evidence-line-per-subject verdict contract rejects any extra subject).
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "pass-host-completion"},
			{Target: signoff, Stage: "signoff", Result: "pass-with-host"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(result.Stderr, "concurrent host completion during review: "+hostID) ||
			!strings.Contains(result.Stderr, "running another signoff round") ||
			!strings.Contains(result.Stderr, "2/2 in 1 iterations") {
			t.Fatalf("concurrent completion loop = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, hostID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, hostID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateBlocked, hostID)) {
			t.Fatal("host-owned concurrent completion did not stay done")
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("reviewed subject did not finish done")
		}
		// records: work, round-1 signoff (passed its subject, tolerated the foreign completion),
		// round-2 signoff reviewing the fed-forward host completion before the loop may exit.
		records := readLoopStageRecords(t, suite)
		if len(records) != 3 || records[1].Stage != "signoff" || records[1].Reopened != 0 ||
			records[1].QueueDoing != 0 || records[1].QueueDone != 2 ||
			records[2].Stage != "signoff" || records[2].Reopened != 0 || records[2].QueueDone != 2 {
			t.Fatalf("concurrent completion telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("concurrent host completion during verify forces later signoff", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "verify-concurrent-completion"
		hostID := taskID + "-host"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		seedLoopProcessTaskIn(t, suite.layout.Repo, stateBlocked, hostID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		verify := loopRecoveryTarget("grok", "verify-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, []string{verify}, 3)
		// The foreign completion lands during the FINAL verify window. The loop must return to
		// signoff, review that exact task, and then verify again before it may exit.
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
			{Target: verify, Stage: "verify", Result: "pass-host-completion"},
			{Target: signoff, Stage: "signoff", Result: "pass-with-host"},
			{Target: verify, Stage: "verify", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(result.Stderr, "verify observed concurrent host completion of "+hostID) ||
			!strings.Contains(result.Stderr, "returning to signoff before exit") {
			t.Fatalf("verify concurrent completion = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, hostID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateBlocked, hostID)) {
			t.Fatal("verify fail-closed disturbed the host-owned completion")
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("verify retry disturbed the verified subject")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 5 || records[3].Stage != "signoff" || records[4].Stage != "verify" {
			t.Fatalf("verify concurrent-completion telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
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
		assertLoopReviewContracts(t, suite, trace, taskID, attempts)
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
			{Target: work, Stage: "work", Result: "repair-review-binding"},
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
		// Every built-in attempt streams now, so each stage records its provider's real
		// closing usage — the fixture's exact cost and tokens, never a synthetic zero.
		if records[0].CostUSD != 0.25 || records[0].InTok != 101 || records[0].OutTok != 11 {
			t.Fatalf("streamed work usage telemetry = %#v", records[0])
		}
		if records[1].InTok != 202 || records[1].OutTok != 22 || records[1].CostUSD != 0 {
			t.Fatalf("streamed between usage telemetry = %#v", records[1])
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("older audit reopen replays an unchanged descendant exactly once", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "older-audit-reopen"
		descendantID := taskID + "-descendant"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		seedLoopProcessTaskIn(t, suite.layout.Repo, stateBlocked, descendantID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, nil, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "reopen-gated"},
			{Target: work, Stage: "work", Result: "repair-older-binding"},
			{Target: between, Stage: "between", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecovery(t, suite, work)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)

		commitUnbound(t, taskID, "intervening")
		descendantFile := filepath.Join(suite.layout.Repo, "descendant.txt")
		if err := os.WriteFile(descendantFile, []byte("descendant\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		loopProcessGit(t, suite, "add", "descendant.txt")
		loopProcessGit(t, suite, "commit", "-q", "-m", "fixture: descendant", "-m", "Coop-Task: "+descendantID)
		commitUnbound(t, taskID, "trailing")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		hostCompletion := procharness.Run(ctx, procharness.Command{
			Path: suite.coopBin, Args: []string{"tasks", "done", descendantID},
			Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20,
		})
		cancel()
		if hostCompletion.Err != nil || hostCompletion.ExitCode != 0 {
			t.Fatalf("host descendant completion = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
				hostCompletion.ExitCode, hostCompletion.Err, hostCompletion.Stdout, hostCompletion.Stderr)
		}
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
		result := process.Wait(ctx)
		cancel()
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("older audit recovery = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
				result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		for _, id := range []string{taskID, descendantID} {
			if got := commitsForTask(suite.layout.Repo, "HEAD", id); len(got) != 1 {
				t.Fatalf("reachable %s bindings = %v, want exactly one", id, got)
			}
			if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, id)) {
				t.Fatalf("task %s did not remain done", id)
			}
		}
		key, err := leaseAuthorityKey(filepath.Join(suite.layout.Repo, tasksRoot), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if pathExists(filepath.Join(suite.layout.XDGCache, "coop", "task-leases", leaseAuthorityVersion, key+".reopen.json")) {
			t.Fatal("accepted audit generation remained reusable")
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("blocked older audit rewrite retains authority for verification-only re-close", func(t *testing.T) {
		cases := []struct {
			name       string
			preUpgrade bool
		}{
			{name: "current blocked record"},
			{name: "pre-upgrade stale blocked record", preUpgrade: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				taskID := "blocked-older-audit-reopen"
				if tc.preUpgrade {
					taskID = "pre-upgrade-blocked-older-audit-reopen"
					t.Setenv(testLeaseAuthorityRootEnv, filepath.Join(
						suite.layout.XDGCache, "coop", "task-leases", leaseAuthorityVersion,
					))
				}
				descendantID := taskID + "-descendant"
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				seedLoopProcessTaskIn(t, suite.layout.Repo, stateBlocked, descendantID)
				work := loopRecoveryTarget("claude", "work-model", "personal")
				between := loopRecoveryTarget("codex", "between-model", "work")
				signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
				writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
				blockResult := "repair-older-binding-blocked"
				if tc.preUpgrade {
					blockResult = "repair-older-binding-blocked-gated"
				}
				blockAttempts := []loopProcessAttempt{
					{Target: work, Stage: "work", Result: "complete"},
					{Target: between, Stage: "between", Result: "reopen-gated"},
					{Target: work, Stage: "work", Result: blockResult},
				}
				suite.reset(t, loopRecoveryScenario(taskID, blockAttempts))
				process := startLoopRecovery(t, suite, work)
				defer process.Cleanup()
				awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
				var staleRecord, legacyRecord auditReopenRecord

				commitUnbound(t, taskID, "intervening")
				descendantFile := filepath.Join(suite.layout.Repo, "descendant.txt")
				if err := os.WriteFile(descendantFile, []byte("descendant\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				loopProcessGit(t, suite, "add", "descendant.txt")
				loopProcessGit(t, suite, "commit", "-q", "-m", "fixture: descendant", "-m", "Coop-Task: "+descendantID)
				commitUnbound(t, taskID, "trailing")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				hostCompletion := procharness.Run(ctx, procharness.Command{
					Path: suite.coopBin, Args: []string{"tasks", "done", descendantID},
					Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20,
				})
				cancel()
				if hostCompletion.Err != nil || hostCompletion.ExitCode != 0 {
					t.Fatalf("host descendant completion = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
						hostCompletion.ExitCode, hostCompletion.Err, hostCompletion.Stdout, hostCompletion.Stderr)
				}
				if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.preUpgrade {
					awaitLoopTraceEventCount(t, suite.layout.Trace, "provider", "ready", 2, 10*time.Second)
					var ok bool
					var err error
					staleRecord, ok, err = readAuditReopenRecord(filepath.Join(suite.layout.Repo, tasksRoot), taskID)
					if err != nil || !ok {
						t.Fatalf("read pre-upgrade audit authority: ok=%v err=%v", ok, err)
					}
					legacyRecord = auditReopenRecord{
						Version: auditReopenLegacyVersion, Generation: staleRecord.Generation,
						TaskID: staleRecord.TaskID, Subject: staleRecord.Subject,
					}
					for _, commit := range staleRecord.History {
						if commit.TaskID != "" {
							legacyRecord.Descendants = append(legacyRecord.Descendants, commit)
						}
					}
					if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-upgrade-release-"+taskID), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
				blockedResult := process.Wait(ctx)
				cancel()
				if blockedResult.Err != nil || blockedResult.ExitCode != 0 {
					t.Fatalf("blocked audit rewrite = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
						blockedResult.ExitCode, blockedResult.Err, blockedResult.Stdout, blockedResult.Stderr)
				}
				if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateBlocked, taskID)) {
					t.Fatal("rewritten audit task did not remain blocked")
				}
				assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, blockAttempts)
				if tc.preUpgrade {
					current, ok, err := readAuditReopenRecord(filepath.Join(suite.layout.Repo, tasksRoot), taskID)
					if err != nil || !ok {
						t.Fatalf("read current blocked audit authority: ok=%v err=%v", ok, err)
					}
					if current.Generation != staleRecord.Generation || current.Subject == staleRecord.Subject {
						t.Fatalf("blocked fixture did not produce one rebased generation: stale=%#v current=%#v", staleRecord, current)
					}
					if err := writeAuditReopenRecord(filepath.Join(suite.layout.Repo, tasksRoot), legacyRecord); err != nil {
						t.Fatalf("restore legacy blocked audit authority: %v", err)
					}
					commitUnbound(t, taskID, "post-block")
					ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
					automatic := procharness.Run(ctx, procharness.Command{
						Path: suite.coopBin, Args: []string{"tasks", "unblock", taskID, "must not auto-upgrade"},
						Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20,
					})
					cancel()
					if automatic.ExitCode == 0 ||
						!strings.Contains(automatic.Stderr, "legacy task-bound-only audit authority") ||
						!strings.Contains(automatic.Stderr, "--adopt-audit-head <full-sha>") {
						t.Fatalf("legacy automatic bridge = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
							automatic.ExitCode, automatic.Err, automatic.Stdout, automatic.Stderr)
					}
					loopProcessGit(t, suite, "reset", "--hard", "-q", staleRecord.BaselineHead)
				}

				unblockArgs := []string{"tasks", "unblock", taskID, "external acceptance passed"}
				if tc.preUpgrade {
					unblockArgs = []string{
						"tasks", "unblock", taskID,
						"--adopt-audit-head", staleRecord.BaselineHead,
						"external acceptance passed",
					}
				}
				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
				unblocked := procharness.Run(ctx, procharness.Command{
					Path: suite.coopBin, Args: unblockArgs,
					Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20,
				})
				cancel()
				if unblocked.Err != nil || unblocked.ExitCode != 0 {
					t.Fatalf("host unblock = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
						unblocked.ExitCode, unblocked.Err, unblocked.Stdout, unblocked.Stderr)
				}

				recloseAttempts := []loopProcessAttempt{
					{Target: work, Stage: "work", Result: "verify-only-after-block"},
					{Target: between, Stage: "between", Result: "pass"},
					{Target: signoff, Stage: "signoff", Result: "pass"},
				}
				suite.reset(t, loopRecoveryScenario(taskID, recloseAttempts))
				recloseHead := loopProcessGit(t, suite, "rev-parse", "HEAD")
				reclosed := runLoopReview(t, suite, work, 20*time.Second)
				if reclosed.Err != nil || reclosed.ExitCode != 0 {
					t.Fatalf("verification-only re-close after unblock = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
						reclosed.ExitCode, reclosed.Err, reclosed.Stdout, reclosed.Stderr)
				}
				if got := loopProcessGit(t, suite, "rev-parse", "HEAD"); got != recloseHead {
					t.Fatalf("verification-only re-close changed HEAD from %s to %s", recloseHead, got)
				}
				for _, id := range []string{taskID, descendantID} {
					if got := commitsForTask(suite.layout.Repo, "HEAD", id); len(got) != 1 {
						t.Fatalf("reachable %s bindings = %v, want exactly one", id, got)
					}
					if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, id)) {
						t.Fatalf("task %s did not remain done", id)
					}
				}
				key, err := leaseAuthorityKey(filepath.Join(suite.layout.Repo, tasksRoot), taskID)
				if err != nil {
					t.Fatal(err)
				}
				if pathExists(filepath.Join(suite.layout.XDGCache, "coop", "task-leases", leaseAuthorityVersion, key+".reopen.json")) {
					t.Fatal("accepted audit generation remained reusable")
				}
				assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, recloseAttempts)
			})
		}
	})

	t.Run("stale active audit authority stops before provider launch", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "stale-active-audit-reopen"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		root := filepath.Join(suite.layout.Repo, tasksRoot)
		task, ok := currentTask(root, taskID)
		if !ok || task.State != stateTodo {
			t.Fatal("stale-authority fixture task is not todo")
		}
		if err := moveTaskDir(root, task, stateInProgress); err != nil {
			t.Fatal(err)
		}
		change := filepath.Join(suite.layout.Repo, "loop-claude.txt")
		if err := os.WriteFile(change, []byte("completed by claude\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		loopProcessGit(t, suite, "add", "loop-claude.txt")
		loopProcessGit(t, suite, "commit", "-q", "-m", "fixture: stale audit subject", "-m", "Coop-Task: "+taskID)
		record, err := captureAuditReopen(suite.layout.Repo, taskID)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(testLeaseAuthorityRootEnv, filepath.Join(
			suite.layout.XDGCache, "coop", "task-leases", leaseAuthorityVersion,
		))
		if err := os.MkdirAll(os.Getenv(testLeaseAuthorityRootEnv), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeAuditReopenRecord(root, record); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(change, []byte("changed outside host authority\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		loopProcessGit(t, suite, "add", "loop-claude.txt")
		loopProcessGit(t, suite, "commit", "--amend", "-q", "--no-edit")
		head := loopProcessGit(t, suite, "rev-parse", "HEAD")
		if auditReopenCurrentValid(suite.layout.Repo, head, taskID, record) {
			t.Fatal("stale-authority fixture unexpectedly remained current-valid")
		}

		work := loopRecoveryTarget("claude", "work-model", "personal")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, nil, nil, 3)
		attempts := []loopProcessAttempt{{Target: work, Stage: "work", Result: "verify-only-after-block"}}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 5*time.Second)
		for _, want := range []string{
			"host audit-reopen authority no longer matches current HEAD",
			"no provider started",
			"task parked in blocked",
			"restore the exact audited pre-attempt baseline " + record.BaselineHead,
			"git rev-parse HEAD",
			"coop tasks unblock " + taskID,
			"blocking and unblocking alone cannot repair Git history",
		} {
			if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, want) {
				t.Fatalf("stale audit authority = exit %d err %v, missing %q\nstdout:\n%s\nstderr:\n%s",
					result.ExitCode, result.Err, want, result.Stdout, result.Stderr)
			}
		}
		for _, event := range readProcessTrace(t, suite.layout.Trace) {
			if event.Source == "provider" || event.Source == "runtime" && event.Event == "run" {
				t.Fatalf("stale audit authority launched external work: %#v", event)
			}
		}
		current, ok := currentTask(root, taskID)
		if !ok || current.State != stateBlocked {
			t.Fatal("stale audit authority did not park the task")
		}
		decision, err := os.ReadFile(filepath.Join(current.Dir, "decision.md"))
		if err != nil || !strings.Contains(string(decision), record.BaselineHead) ||
			!strings.Contains(string(decision), "git rev-parse HEAD") ||
			!strings.Contains(string(decision), "Blocking and unblocking alone cannot repair Git history") {
			t.Fatalf("stale audit decision = %q, %v", decision, err)
		}
		if got, ok, err := readAuditReopenRecord(root, taskID); err != nil || !ok || !sameAuditReopenRecord(got, record) {
			t.Fatalf("stale audit authority changed the host record: got=%#v ok=%v err=%v", got, ok, err)
		}
	})

	t.Run("older audit reopen rejects a changed descendant", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "changed-descendant-reopen"
		descendantID := taskID + "-descendant"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		seedLoopProcessTaskIn(t, suite.layout.Repo, stateBlocked, descendantID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, nil, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "reopen-gated"},
			{Target: work, Stage: "work", Result: "repair-older-binding-changed-descendant"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecovery(t, suite, work)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		if err := os.WriteFile(filepath.Join(suite.layout.Repo, "descendant.txt"), []byte("descendant\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		loopProcessGit(t, suite, "add", "descendant.txt")
		loopProcessGit(t, suite, "commit", "-q", "-m", "fixture: descendant", "-m", "Coop-Task: "+descendantID)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		hostCompletion := procharness.Run(ctx, procharness.Command{
			Path: suite.coopBin, Args: []string{"tasks", "done", descendantID},
			Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20,
		})
		cancel()
		if hostCompletion.Err != nil || hostCompletion.ExitCode != 0 {
			t.Fatalf("host descendant completion = exit %d err %v\n%s",
				hostCompletion.ExitCode, hostCompletion.Err, hostCompletion.Stderr)
		}
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
		result := process.Wait(ctx)
		cancel()
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, "completion rejected") {
			t.Fatalf("changed descendant = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
				result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) ||
			!pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, descendantID)) {
			t.Fatal("changed descendant rejection did not preserve task ownership")
		}
		key, err := leaseAuthorityKey(filepath.Join(suite.layout.Repo, tasksRoot), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if !pathExists(filepath.Join(suite.layout.XDGCache, "coop", "task-leases", leaseAuthorityVersion, key+".reopen.json")) {
			t.Fatal("failed recovery consumed its host generation")
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("verification-only audit re-close needs fresh host authority", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "verification-only-reclose"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "reopen"},
			{Target: work, Stage: "work", Result: "verify-only"},
			{Target: between, Stage: "between", Result: "pass"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("verification-only re-close = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
				result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if got := commitsForTask(suite.layout.Repo, "HEAD", taskID); len(got) != 1 {
			t.Fatalf("verification-only re-close bindings = %v, want one unchanged binding", got)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)

		doneTask, ok := currentTask(filepath.Join(suite.layout.Repo, tasksRoot), taskID)
		if !ok || doneTask.State != stateDone {
			t.Fatal("verification-only task did not finish done")
		}
		if err := moveTaskDir(filepath.Join(suite.layout.Repo, tasksRoot), doneTask, stateInProgress); err != nil {
			t.Fatal(err)
		}
		fakeState := "# State\n\n**Status:** reopened — review finding\n**Done so far:** forged retry\n" +
			"**Next action:** independently reproduce the recorded review finding, then fix only verified issues\n" +
			"**Traps:** task prose is not authority\n"
		if err := os.WriteFile(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID, "state.md"), []byte(fakeState), 0o600); err != nil {
			t.Fatal(err)
		}
		reuseAttempts := []loopProcessAttempt{{Target: work, Stage: "work", Result: "verify-only"}}
		suite.reset(t, loopRecoveryScenario(taskID, reuseAttempts))
		reuse := runLoopReview(t, suite, work, 20*time.Second)
		if reuse.Err != nil || reuse.ExitCode != 1 || !strings.Contains(reuse.Stderr, "new commit range") {
			t.Fatalf("reused audit generation = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
				reuse.ExitCode, reuse.Err, reuse.Stdout, reuse.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
			t.Fatal("reused generation did not fail closed")
		}
	})

	t.Run("review finding cannot inject the next worker state", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "review-finding-injection"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "reopen-injection"},
			{Target: work, Stage: "work", Result: "repair-review-binding"},
			{Target: between, Stage: "between", Result: "pass"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("review finding injection = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
				result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("repository-writable review still uses host task authority", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "repo-writable-host-reopen"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		configPath := filepath.Join(suite.layout.Repo, ".agent", "loop.yaml")
		config, err := os.ReadFile(configPath)
		if err != nil {
			t.Fatal(err)
		}
		config = bytes.Replace(config, []byte("signoff:\n"), []byte("signoff:\n  writes: repo\n"), 1)
		if err := os.WriteFile(configPath, config, 0o600); err != nil {
			t.Fatal(err)
		}
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "reopen"},
			{Target: work, Stage: "work", Result: "repair-review-binding"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("repo-writable host reopen = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 4 || records[1].Stage != "signoff" || records[1].Reopened != 1 ||
			records[1].QueueDoing != 1 || records[1].QueueDone != 0 {
			t.Fatalf("repo-writable host reopen telemetry = %#v", records)
		}
		var runs []*processTrace
		for _, event := range readProcessTrace(t, suite.layout.Trace) {
			if event.Source == "runtime" && event.Event == "run" {
				runs = append(runs, event)
			}
		}
		if len(runs) != len(attempts) {
			t.Fatalf("repo-writable review runtime runs = %d, want %d", len(runs), len(attempts))
		}
		if runs[1].Run == nil || runs[3].Run == nil {
			t.Fatalf("repo-writable review run details missing: %#v", runs)
		}
		assertLoopReviewRepoMount(t, suite.layout, runs[1].Run.Mounts, false)
		assertLoopReviewRepoMount(t, suite.layout, runs[3].Run.Mounts, false)
	})

	t.Run("reopened work cannot add a second task binding", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "reopen-second-binding"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		between := loopRecoveryTarget("codex", "between-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, []string{between}, nil, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: between, Stage: "between", Result: "reopen"},
			{Target: work, Stage: "work", Result: "second-binding"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 1 ||
			!strings.Contains(result.Stderr, "host audit authority accepts only") {
			t.Fatalf("second binding = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("second binding rejection did not restore the task")
		}
		if commits := commitsForTask(suite.layout.Repo, "", taskID); len(commits) != 2 {
			t.Fatalf("fixture produced %d reachable bindings, want 2: %v", len(commits), commits)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)

		traceBeforeRetry := readProcessTrace(t, suite.layout.Trace)
		retry := runLoopReview(t, suite, work, 5*time.Second)
		for _, want := range []string{
			"host audit-reopen authority no longer matches current HEAD",
			"no provider started",
			"task parked in blocked",
			"restore the exact audited pre-attempt baseline",
			"git rev-parse HEAD",
			"blocking and unblocking alone cannot repair Git history",
		} {
			if retry.Err != nil || retry.ExitCode != 1 || !strings.Contains(retry.Stderr, want) {
				t.Fatalf("second binding retry = exit %d err %v, missing %q\nstdout:\n%s\nstderr:\n%s",
					retry.ExitCode, retry.Err, want, retry.Stdout, retry.Stderr)
			}
		}
		traceAfterRetry := readProcessTrace(t, suite.layout.Trace)
		if len(traceAfterRetry) < len(traceBeforeRetry) {
			t.Fatalf("second binding retry truncated process trace: before=%d after=%d", len(traceBeforeRetry), len(traceAfterRetry))
		}
		for _, event := range traceAfterRetry[len(traceBeforeRetry):] {
			if event.Source == "provider" || event.Source == "runtime" && event.Event == "run" {
				t.Fatalf("second binding retry launched external work: %#v", event)
			}
		}
		current, ok := currentTask(filepath.Join(suite.layout.Repo, tasksRoot), taskID)
		if !ok || current.State != stateBlocked {
			t.Fatalf("second binding retry task = %+v/%v, want blocked", current, ok)
		}
		decision, err := os.ReadFile(filepath.Join(current.Dir, "decision.md"))
		if err != nil || !strings.Contains(string(decision), "restore the exact pre-attempt baseline") ||
			!strings.Contains(string(decision), "git rev-parse HEAD") {
			t.Fatalf("second binding retry decision = %q, %v", decision, err)
		}
	})

	t.Run("worker cannot forge review authority for an archived task", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "forged-review-authority"
		archiveID := taskID + "-archive"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		writeTaskFile(t, filepath.Join(suite.layout.Repo, tasksRoot, stateDone, archiveID, "task.md"), "# Archive\n")
		work := loopRecoveryTarget("claude", "work-model", "personal")
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete-forged-archive-binding"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 1 ||
			!strings.Contains(result.Stderr, "reachable HEAD each need exactly one commit") {
			t.Fatalf("forged review authority = exit %d err %v\nstdout:\n%s\nstderr:\n%s",
				result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
			t.Fatal("forged binding rejection did not restore the assigned task")
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, archiveID)) {
			t.Fatal("forged binding rejection disturbed the archived task")
		}
		if commits := commitsForTask(suite.layout.Repo, "", archiveID); len(commits) != 1 {
			t.Fatalf("fixture produced %d forged archived bindings, want 1: %v", len(commits), commits)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
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
			{Target: work, Stage: "work", Result: "repair-review-binding"},
			{Target: signoff, Stage: "signoff", Result: "reopen"},
			{Target: work, Stage: "work", Result: "repair-review-binding"},
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
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
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
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("failed review receipt cannot reopen", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "failed-review-reopen"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("claude", "work-model", "personal")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "reopen-authentication"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		if result.Err != nil || result.ExitCode != 1 || !strings.Contains(result.Stderr, "authentication failed") {
			t.Fatalf("failed review reopen = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
			t.Fatal("failed review receipt changed task state")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 || records[1].Stage != "signoff" || records[1].Outcome != "authentication" ||
			records[1].Exit != 23 || records[1].Retries != 0 || records[1].Reopened != 0 {
			t.Fatalf("failed review reopen telemetry = %#v", records)
		}
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
	})

	t.Run("hard stop after review receipt leaves task done", func(t *testing.T) {
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
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) ||
			pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)) {
			t.Fatal("interrupted review receipt changed task state")
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 || records[1].Stage != "between" || records[1].Outcome != "interrupted" || records[1].Exit != loopInterruptedExitCode || records[1].Reopened != 0 {
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
		assertLoopReviewContracts(t, suite, readProcessTrace(t, suite.layout.Trace), taskID, attempts)
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
		assertLoopReviewContracts(t, suite, trace, taskID, attempts)
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
		assertLoopReviewContracts(t, suite, trace, taskID, attempts)
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

func assertLoopReviewContracts(t *testing.T, suite *directProcessSuite, trace []*processTrace, taskID string, attempts []loopProcessAttempt) {
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
		// Built-in attempts always request the adapter stream, PTY or not — it feeds the
		// provider watchdog.
		argv, ok := iterationCommand(target.Provider, loopProcessArgv(target.Provider, target.Model, target.Effort, "fixture-prompt-sentinel"), nil)
		if !ok {
			t.Fatalf("review attempt %d provider %q has no streaming command", i, target.Provider)
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
		agent, ok := agents.Get(target.Provider)
		if !ok {
			t.Fatalf("review attempt %d provider %q is unregistered", i, target.Provider)
		}
		for _, flag := range agent.Stream().Flags {
			if processSafeFlag(flag) && !slices.Contains(run.ProviderArgv, flag) {
				t.Fatalf("review attempt %d %s missing streaming flag %q in %q", i, attempt.Stage, flag, run.ProviderArgv)
			}
		}
		wantExit := 0
		if attempt.Result != "complete" && attempt.Result != "complete-delay" && attempt.Result != "complete-gated" &&
			attempt.Result != "complete-forged-archive-binding" && attempt.Result != "repair-binding" &&
			attempt.Result != "repair-review-binding" && attempt.Result != "repair-older-binding" &&
			attempt.Result != "repair-older-binding-blocked" &&
			attempt.Result != "repair-older-binding-blocked-gated" &&
			attempt.Result != "repair-older-binding-changed-descendant" && attempt.Result != "verify-only" &&
			attempt.Result != "verify-only-after-block" &&
			attempt.Result != "second-binding" && attempt.Result != "pass" && attempt.Result != "pass-host-completion" &&
			attempt.Result != "pass-with-host" && attempt.Result != "pass-with-descendant" &&
			attempt.Result != "pass-corrected" && attempt.Result != "reopen" && attempt.Result != "reopen-gated" &&
			attempt.Result != "reopen-injection" && attempt.Result != "reopen-corrected" &&
			attempt.Result != "malformed-review" && attempt.Result != "malformed-review-corrected" &&
			!slices.Contains([]string{"pass-codex-footer", "reopen-codex-footer", "pass-codex-footer-echo", "reopen-codex-footer-echo", "pass-codex-echo-footer", "reopen-codex-echo-footer", "pass-codex-split-footer-echo"}, attempt.Result) {
			wantExit = 23
		}
		if attempt.Result == "wait" {
			wantExit = 130
		} else if attempt.Result == "background-drained-review" {
			wantExit = 190
		} else if attempt.Result == "background-timeout-review" {
			wantExit = 191
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
	profile := processTracePath(layout.Root, filepath.Join(layout.Config, provider, "profiles", account))
	profileTarget := "<container>/home/node/." + provider
	foundRepo, foundProfile := false, false
	for _, mount := range mounts {
		switch {
		case mount.Source == repo && mount.Target == repo && mount.ReadOnly:
			foundRepo = true
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
	if !foundRepo || !foundProfile {
		t.Fatalf("review mounts repo/profile = %v/%v in %#v", foundRepo, foundProfile, mounts)
	}
}

func assertLoopReviewRepoMount(t *testing.T, layout procharness.Layout, mounts []processMount, readOnly bool) {
	t.Helper()
	repo := processTracePath(layout.Root, layout.Repo)
	queue := processTracePath(layout.Root, filepath.Join(layout.Repo, tasksRoot))
	var matches, protected []processMount
	for _, mount := range mounts {
		if mount.Source == repo && mount.Target == repo {
			matches = append(matches, mount)
		}
		if mount.Source == queue && mount.Target == queue {
			protected = append(protected, mount)
		}
	}
	if len(matches) != 1 || matches[0].ReadOnly != readOnly {
		t.Fatalf("review repo mount = %#v, want exactly one read-only=%v mount", matches, readOnly)
	}
	if !readOnly && (len(protected) != 1 || !protected[0].ReadOnly) {
		t.Fatalf("repository-writable review task mounts = %#v, want one read-only queue mount", protected)
	}
}
