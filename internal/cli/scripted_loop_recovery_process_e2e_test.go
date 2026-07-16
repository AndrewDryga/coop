//go:build providere2e

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedLoopRecoveryProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)

	t.Run("all directed provider rotations", func(t *testing.T) {
		pairs := 0
		for _, source := range suite.providers {
			for _, destination := range suite.providers {
				if source == destination {
					continue
				}
				pairs++
				t.Run(source+"-to-"+destination, func(t *testing.T) {
					resetLoopProcessRepo(t, suite)
					t.Cleanup(func() { logLoopProcessFailure(t, suite) })
					taskID := "rotation-" + source + "-to-" + destination
					seedLoopProcessTask(t, suite.layout.Repo, taskID)
					sourceTarget := loopRecoveryTarget(source, "rotation-"+source, "work")
					destinationTarget := loopRecoveryTarget(destination, "rotation-"+destination, "work")
					writeLoopRecoveryPreset(t, suite.layout.Repo, "rotation", []string{sourceTarget, destinationTarget})
					attempts := []loopProcessAttempt{
						{Target: sourceTarget, Stage: "work", Result: "rate-limit"},
						{Target: destinationTarget, Stage: "work", Result: "complete"},
					}
					suite.reset(t, loopRecoveryScenario(taskID, attempts))
					result := runLoopRecovery(t, suite, "rotation")
					if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, fmt.Sprintf("target %q rate limited — switching to %q", sourceTarget, destinationTarget)) {
						t.Fatalf("directed rotation = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
					}
					trace := readProcessTrace(t, suite.layout.Trace)
					assertLoopAttemptContracts(t, suite, trace, taskID, attempts)
					destinationParsed, _ := agents.ParseTarget(destinationTarget)
					assertLoopProcessResult(t, suite, destination, taskID, destinationParsed.Model, destinationParsed.Effort, destinationParsed.Account(), suite.repoHead, 2, false)
					records := readLoopStageRecords(t, suite)
					if records[0].Provider != source || records[0].Outcome != "rate_limit" || records[0].Exit != 23 || len(records[0].Finished) != 0 || records[1].Provider != destination || records[1].Outcome != "success" || records[1].Exit != 0 {
						t.Fatalf("directed rotation telemetry = %#v", records)
					}
					assertLoopTraceProcessesGone(t, trace)
				})
			}
		}
		if pairs != len(suite.providers)*(len(suite.providers)-1) || pairs != 12 {
			t.Fatalf("directed provider pairs = %d, want registry-derived 12", pairs)
		}
	})

	t.Run("authentication fails fast for every provider", func(t *testing.T) {
		for _, provider := range suite.providers {
			t.Run(provider, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				taskID := "authentication-" + provider
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				target := loopRecoveryTarget(provider, "auth-model", "work")
				attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: "authentication"}}
				suite.reset(t, loopRecoveryScenario(taskID, attempts))
				result := runLoopRecovery(t, suite, target)
				if result.ExitCode == 0 || !strings.Contains(result.Stderr, "coop login "+provider+"@work") || strings.Contains(result.Stderr, "switching to") || strings.Contains(result.Stderr, "retrying in") {
					t.Fatalf("authentication failure = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
				}
				trace := readProcessTrace(t, suite.layout.Trace)
				assertLoopAttemptContracts(t, suite, trace, taskID, attempts)
				records := readLoopStageRecords(t, suite)
				if len(records) != 1 || records[0].Outcome != "authentication" {
					t.Fatalf("authentication telemetry = %#v", records)
				}
				assertLoopTaskRecoverable(t, suite, taskID)
				assertLoopTraceProcessesGone(t, trace)
			})
		}
	})

	t.Run("output exhaustion resumes the same target", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "output-resume-codex"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "output-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "output-limit"},
			{Target: target, Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, target)
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stderr, "model output limit — resuming immediately") || strings.Contains(result.Stderr, "switching to") {
			t.Fatalf("output resume = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopAttemptContracts(t, suite, trace, taskID, attempts)
		parsed, _ := agents.ParseTarget(target)
		assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 2, false)
		records := readLoopStageRecords(t, suite)
		if records[0].Outcome != "output_limit" || records[0].Exit != 23 || records[0].Retries != 1 || records[1].Outcome != "success" || records[1].Exit != 0 {
			t.Fatalf("output resume telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("ordinary failures and diagnostic prose do not rotate", func(t *testing.T) {
		for _, resultKind := range []string{"ordinary", "ambiguous-limit-prose", "ambiguous-auth-prose"} {
			t.Run(resultKind, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				taskID := "ordinary-" + resultKind
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				target := loopRecoveryTarget("codex", "ordinary-model", "work")
				writeLoopRecoveryPreset(t, suite.layout.Repo, "ordinary", []string{target, loopRecoveryTarget("claude", "must-not-run", "work")})
				attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: resultKind}}
				suite.reset(t, loopRecoveryScenario(taskID, attempts))
				process := startLoopRecovery(t, suite, "ordinary")
				defer process.Cleanup()
				awaitLoopProcessOutput(t, process, "iteration failed (1/5) — retrying in 10s", 5*time.Second)
				if err := process.SignalGroup(syscall.SIGINT); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				got := process.Wait(ctx)
				cancel()
				if got.ExitCode == 0 || strings.Contains(got.Stderr, "switching to") {
					t.Fatalf("ordinary classification = exit %d err %v\nstdout:\n%s\nstderr:\n%s", got.ExitCode, got.Err, got.Stdout, got.Stderr)
				}
				trace := readProcessTrace(t, suite.layout.Trace)
				assertLoopAttemptContracts(t, suite, trace, taskID, attempts)
				records := readLoopStageRecords(t, suite)
				if len(records) != 1 || records[0].Outcome != "process_failure" {
					t.Fatalf("ordinary telemetry = %#v, want process_failure", records)
				}
				assertLoopTaskRecoverable(t, suite, taskID)
				assertLoopTraceProcessesGone(t, trace)
			})
		}
	})

	t.Run("streamed malformed and truncated events fail closed", func(t *testing.T) {
		for _, resultKind := range []string{"malformed", "truncated"} {
			t.Run(resultKind, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				taskID := "stream-" + resultKind
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				target := loopRecoveryTarget("codex", "stream-model", "work")
				attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: resultKind}}
				suite.reset(t, loopRecoveryScenario(taskID, attempts))
				process := startLoopRecoveryPTY(t, suite, target)
				defer process.Cleanup()
				awaitLoopProcessOutput(t, process, "iteration failed (1/5) — retrying in 10s", 10*time.Second)
				if err := process.SignalGroup(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				result := process.Wait(ctx)
				cancel()
				if result.ExitCode == 0 || strings.Contains(result.Stderr, "switching to") {
					t.Fatalf("stream failure = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
				}
				trace := readProcessTrace(t, suite.layout.Trace)
				assertLoopAttemptContractsWithStreaming(t, suite, trace, taskID, attempts, true)
				records := readLoopStageRecords(t, suite)
				if len(records) != 1 || records[0].Outcome != "malformed_stream" {
					t.Fatalf("stream failure telemetry = %#v, want malformed_stream", records)
				}
				assertLoopTaskRecoverable(t, suite, taskID)
				assertLoopTraceProcessesGone(t, trace)
			})
		}
	})

	t.Run("cooling is scoped by account and model", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "target-scoped-cooling"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		targets := []string{
			loopRecoveryTarget("claude", "same-model", "personal"),
			loopRecoveryTarget("claude", "same-model", "work"),
			loopRecoveryTarget("claude", "other-model", "personal"),
		}
		writeLoopRecoveryPreset(t, suite.layout.Repo, "cooling", targets)
		attempts := []loopProcessAttempt{
			{Target: targets[0], Stage: "work", Result: "rate-limit"},
			{Target: targets[1], Stage: "work", Result: "rate-limit"},
			{Target: targets[2], Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, "cooling")
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("target cooling = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopAttemptContracts(t, suite, trace, taskID, attempts)
		parsed, _ := agents.ParseTarget(targets[2])
		assertLoopProcessResult(t, suite, "claude", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 3, false)
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("all targets limited wait is cancelable", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "all-targets-limited"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		var targets []string
		var attempts []loopProcessAttempt
		for _, provider := range suite.providers {
			target := loopRecoveryTarget(provider, "limited-"+provider, "work")
			targets = append(targets, target)
			attempts = append(attempts, loopProcessAttempt{Target: target, Stage: "work", Result: "rate-limit"})
		}
		writeLoopRecoveryPreset(t, suite.layout.Repo, "all-limited", targets)
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecovery(t, suite, "all-limited")
		defer process.Cleanup()
		awaitLoopProcessOutput(t, process, "all 4 targets are rate limited — waiting for the soonest reset", 10*time.Second)
		if err := process.SignalGroup(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result := process.Wait(ctx)
		cancel()
		if result.ExitCode == 0 {
			t.Fatalf("all-limited cancellation = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopAttemptContracts(t, suite, trace, taskID, attempts)
		for _, record := range readLoopStageRecords(t, suite) {
			if record.Outcome != "rate_limit" {
				t.Fatalf("all-limited telemetry = %#v", readLoopStageRecords(t, suite))
			}
		}
		assertLoopTaskRecoverable(t, suite, taskID)
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("interrupted provider resumes the same task", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "interrupted-resume"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "resume-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "wait"},
			{Target: target, Stage: "work", Result: "complete"},
		}
		scenario := loopRecoveryScenario(taskID, attempts)
		suite.reset(t, scenario)
		process := startLoopRecovery(t, suite, target)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 5*time.Second)
		if err := process.SignalGroup(syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		first := process.Wait(ctx)
		cancel()
		if first.ExitCode == 0 {
			t.Fatalf("interrupted loop unexpectedly succeeded: %#v", first)
		}
		firstTrace := readProcessTrace(t, suite.layout.Trace)
		assertLoopTaskRecoverable(t, suite, taskID)
		assertLoopTraceProcessesGone(t, firstTrace)

		suite.reset(t, scenario)
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-cursor.json"), []byte("{\"index\":1}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		result := runLoopRecovery(t, suite, target)
		if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "fixture-loop-complete-codex") {
			t.Fatalf("resumed loop = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		assertLoopAttemptContracts(t, suite, trace, taskID, attempts[1:])
		parsed, _ := agents.ParseTarget(target)
		assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 1, false)
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("hard interrupt records interrupted work", func(t *testing.T) {
		resetLoopProcessRepo(t, suite)
		taskID := "hard-interrupt-telemetry"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "interrupt-model", "work")
		attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: "wait"}}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecoveryPTY(t, suite, target)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), 5*time.Second)
		if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		awaitLoopProcessOutput(t, process, "finishing this iteration, then stopping", 5*time.Second)
		if err := syscall.Kill(coopPID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result := process.Wait(ctx)
		cancel()
		if result.ExitCode == 0 {
			t.Fatalf("hard-interrupted loop unexpectedly succeeded: %#v", result)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		adapter, _ := agents.Get("codex")
		var starts, runs int
		for _, event := range trace {
			if event.Source == "provider" && event.Event == "start" {
				starts++
				for _, flag := range adapter.Stream().Flags {
					if !slices.Contains(event.Argv, flag) {
						t.Fatalf("hard-interrupted provider argv lacks streaming flag %q: %q", flag, event.Argv)
					}
				}
			}
			if event.Source == "runtime" && event.Event == "run" {
				runs++
			}
		}
		if runs != 1 || starts != 1 {
			t.Fatalf("hard interrupt attempts runs/starts = %d/%d, want 1/1\n%#v", runs, starts, trace)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 1 || records[0].Stage != "work" || records[0].Outcome != "interrupted" {
			t.Fatalf("hard-interrupt telemetry = %#v", records)
		}
		assertLoopTaskRecoverable(t, suite, taskID)
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("interrupted completion is reconciled before cleanup", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			result string
		}{
			{name: "bound", result: "complete-wait"},
			{name: "unbound", result: "unbound-wait"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				taskID := "interrupted-completion-" + tc.name
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				target := loopRecoveryTarget("codex", "completion-model", "work")
				attempt := loopProcessAttempt{Target: target, Stage: "work", Result: tc.result}
				suite.reset(t, loopRecoveryScenario(taskID, []loopProcessAttempt{attempt}))
				process := startLoopRecovery(t, suite, target)
				defer process.Cleanup()
				awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 5*time.Second)
				if err := process.SignalGroup(syscall.SIGINT); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				first := process.Wait(ctx)
				cancel()
				if first.ExitCode == 0 {
					t.Fatalf("interrupted completion unexpectedly succeeded: %#v", first)
				}
				firstTrace := readProcessTrace(t, suite.layout.Trace)
				assertLoopTraceProcessesGone(t, firstTrace)
				done := filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)
				if !fileExists(filepath.Join(done, "tmp", "lease.json")) {
					t.Fatal("interrupted done task did not retain crash-identifying lease metadata")
				}
				crashHead := loopProcessGit(t, suite, "rev-parse", "HEAD")

				suite.reset(t, loopRecoveryScenario(taskID, []loopProcessAttempt{{Target: target, Stage: "work", Result: "repair-binding"}}))
				restart := runLoopRecovery(t, suite, target)
				trace := readProcessTrace(t, suite.layout.Trace)
				if restart.Err != nil || restart.ExitCode != 0 || !loopTraceHasAttempt(trace) {
					t.Fatalf("completion restart = exit %d err %v trace=%#v\nstdout:\n%s\nstderr:\n%s", restart.ExitCode, restart.Err, trace, restart.Stdout, restart.Stderr)
				}
				if pathExists(filepath.Join(done, "tmp")) {
					t.Fatal("resumed interrupted completion retained tmp")
				}
				parsed, _ := agents.ParseTarget(target)
				assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), crashHead, 1, tc.name == "bound")
				assertLoopTraceProcessesGone(t, trace)
			})
		}
	})
}

func loopRecoveryScenario(taskID string, attempts []loopProcessAttempt) loopProcessScenario {
	target, err := agents.ParseTarget(attempts[0].Target)
	if err != nil {
		panic(err)
	}
	return loopProcessScenario{Version: 6, Provider: target.Provider, ProviderHomes: agents.Names(), Loop: loopProcessPlan{TaskID: taskID, Attempts: attempts}}
}

func loopRecoveryTarget(provider, model, account string) string {
	target := provider + ":" + model
	if effort := directTargetEffort(directProviderContracts[provider]); effort != "" {
		target += "/" + effort
	}
	return target + "@" + account
}

func writeLoopRecoveryPreset(t *testing.T, repo, name string, targets []string) {
	t.Helper()
	dir := filepath.Join(repo, ".agent", "presets", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "lead: {agent: [" + strings.Join(targets, ", ") + "]}\n"
	if err := os.WriteFile(filepath.Join(dir, "preset.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runLoopRecovery(t *testing.T, suite *directProcessSuite, target string) procharness.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return procharness.Run(ctx, loopRecoveryCommand(suite, target))
}

func startLoopRecovery(t *testing.T, suite *directProcessSuite, target string) *procharness.Process {
	t.Helper()
	process, err := procharness.Start(loopRecoveryCommand(suite, target))
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func startLoopRecoveryPTY(t *testing.T, suite *directProcessSuite, target string) *procharness.Process {
	t.Helper()
	command := terminalLoopCommand(t, loopRecoveryCommand(suite, target))
	process, err := procharness.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	return process
}

func terminalLoopCommand(t *testing.T, base procharness.Command) procharness.Command {
	t.Helper()
	path, err := exec.LookPath("script")
	if err != nil {
		t.Fatal("script(1) is required for terminal-bound loop coverage")
	}
	command := procharness.Command{
		Path: path, Dir: base.Dir, Env: base.Env, MaxOutput: base.MaxOutput,
		KillGrace: base.KillGrace,
	}
	switch runtime.GOOS {
	case "darwin", "freebsd":
		command.Args = append([]string{"-q", "/dev/null", base.Path}, base.Args...)
	case "linux":
		parts := append([]string{base.Path}, base.Args...)
		for i := range parts {
			parts[i] = shellQuote(parts[i])
		}
		command.Args = []string{"-qefc", strings.Join(parts, " "), "/dev/null"}
	default:
		t.Skipf("terminal-bound loop coverage is unsupported on %s", runtime.GOOS)
	}
	return command
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func awaitDescendantPID(t *testing.T, root int, executable string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
		if err == nil {
			type process struct {
				pid, parent int
				command     string
			}
			var processes []process
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 3 {
					continue
				}
				pid, pidErr := strconv.Atoi(fields[0])
				parent, parentErr := strconv.Atoi(fields[1])
				if pidErr == nil && parentErr == nil {
					processes = append(processes, process{pid: pid, parent: parent, command: filepath.Base(fields[2])})
				}
			}
			descendants := map[int]bool{root: true}
			for range len(processes) {
				for _, process := range processes {
					if descendants[process.parent] {
						descendants[process.pid] = true
					}
				}
			}
			for _, process := range processes {
				if descendants[process.pid] && process.command == executable {
					return process.pid
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s descendant of pid %d appeared", executable, root)
	return 0
}

func loopRecoveryCommand(suite *directProcessSuite, target string) procharness.Command {
	return procharness.Command{
		Path: suite.coopBin, Args: []string{"loop", target, "--max-tasks", "1", "--no-preflight", "--no-mcp"},
		Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	}
}

func assertLoopAttemptContracts(t *testing.T, suite *directProcessSuite, trace []*processTrace, taskID string, attempts []loopProcessAttempt) {
	assertLoopAttemptContractsWithStreaming(t, suite, trace, taskID, attempts, false)
}

func assertLoopAttemptContractsWithStreaming(t *testing.T, suite *directProcessSuite, trace []*processTrace, taskID string, attempts []loopProcessAttempt, streaming bool) {
	t.Helper()
	var runs, starts, exits []*processTrace
	for i, event := range trace {
		if event.Version != 1 || event.Sequence != i+1 || event.Event == "error" {
			t.Fatalf("invalid recovery trace event %d: %#v", i, event)
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
		t.Fatalf("attempt trace counts runs/starts/exits = %d/%d/%d, want %d\n%#v", len(runs), len(starts), len(exits), len(attempts), trace)
	}
	for i, attempt := range attempts {
		target, err := agents.ParseTarget(attempt.Target)
		if err != nil {
			t.Fatal(err)
		}
		provider := target.Provider
		prompt := loopWorkPrompt(suite.layout.Repo, []string{tasksRoot}, taskID, provider, nil, nil)
		argv := loopProcessArgv(provider, target.Model, target.Effort, prompt)
		if streaming {
			var ok bool
			argv, ok = iterationCommand(provider, argv, nil, true)
			if !ok {
				t.Fatalf("provider %s has no streaming loop command", provider)
			}
		}
		wantArgv := processTraceArgv(argv)
		run := runs[i].Run
		if run == nil || run.Provider != provider || !reflect.DeepEqual(run.ProviderArgv, wantArgv) {
			t.Fatalf("attempt %d runtime = %#v, want %s %q", i, run, provider, wantArgv)
		}
		assertProcessMounts(t, suite.layout, provider, target.Account(), run.Mounts)
		assertDirectEnvironment(t, run.Environment, suite.allCredKeys, provider, directProviderContracts[provider], target.Model, target.Effort)
		if !reflect.DeepEqual(starts[i].Argv, wantArgv) {
			t.Fatalf("attempt %d provider argv = %q, want %q", i, starts[i].Argv, wantArgv)
		}
		assertDirectEnvironment(t, starts[i].Environment, suite.allCredKeys, provider, directProviderContracts[provider], target.Model, target.Effort)
		wantExit := 23
		if (strings.HasPrefix(attempt.Result, "complete") && attempt.Result != "complete-wait") ||
			(strings.HasPrefix(attempt.Result, "unbound") && attempt.Result != "unbound-wait") || attempt.Result == "repair-binding" {
			wantExit = 0
		} else if attempt.Result == "wait" {
			wantExit = 130
		}
		if exits[i].ExitCode == nil || *exits[i].ExitCode != wantExit {
			t.Fatalf("attempt %d provider exit = %#v, want %d", i, exits[i], wantExit)
		}
	}
}

func readLoopStageRecords(t *testing.T, suite *directProcessSuite) []stageRecord {
	t.Helper()
	paths, err := loopStageTelemetryPaths(suite.layout.Repo)
	if err != nil || len(paths) != 1 {
		t.Fatalf("loop telemetry files = %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var records []stageRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record stageRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func awaitLoopProcessOutput(t *testing.T, process *procharness.Process, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stdout, stderr := process.Output()
		if strings.Contains(stdout, needle) || strings.Contains(stderr, needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	stdout, stderr := process.Output()
	t.Fatalf("timed out waiting for process output %q\nstdout:\n%s\nstderr:\n%s", needle, stdout, stderr)
}

func assertLoopTaskRecoverable(t *testing.T, suite *directProcessSuite, taskID string, expectedHead ...string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID, "task.md")); err != nil {
		t.Fatalf("recoverable task missing from in_progress: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(suite.layout.Repo, tasksRoot, stateDone))
	if err != nil || len(entries) != 0 {
		t.Fatalf("recoverable task appeared done: %v, %v", entries, err)
	}
	wantHead := suite.repoHead
	if len(expectedHead) > 0 {
		wantHead = expectedHead[0]
	}
	if head := loopProcessGit(t, suite, "rev-parse", "HEAD"); head != wantHead {
		t.Fatalf("recoverable task changed HEAD to %s, want %s", head, wantHead)
	}
}

func assertLoopTraceProcessesGone(t *testing.T, trace []*processTrace) {
	t.Helper()
	for _, event := range trace {
		awaitProcessGone(t, event.PID)
	}
}

func loopTraceHasAttempt(trace []*processTrace) bool {
	for _, event := range trace {
		if event.Event == "run" || event.Source == "provider" {
			return true
		}
	}
	return false
}
