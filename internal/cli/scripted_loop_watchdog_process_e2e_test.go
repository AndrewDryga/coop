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
	"github.com/AndrewDryga/coop/internal/tasks"
)

// setLoopWatchdogDeadlines appends the internal COOP_PROVIDER_TIMEOUTS override to the suite's
// conf file so ONE subtest runs the real binary under short watchdog deadlines, restoring the
// original conf afterwards — the process-harness environment is allowlist-only, so the conf
// file is the deadline's channel into the host process.
func setLoopWatchdogDeadlines(t *testing.T, suite *directProcessSuite, value string) {
	t.Helper()
	conf := filepath.Join(suite.layout.Config, "missing.conf")
	original, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(conf, original, 0o600); err != nil {
			t.Errorf("restore suite conf: %v", err)
		}
	})
	updated := string(original) + "COOP_PROVIDER_TIMEOUTS=" + value + "\n"
	if err := os.WriteFile(conf, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

// holdLoopHostSetup blocks the host-side setup of every box run until the returned release is
// called — the fixture runtime holds its daemon probe while the file exists. Idempotent, and
// released on cleanup so a failed assertion cannot leave the next subtest wedged.
func holdLoopHostSetup(t *testing.T, suite *directProcessSuite) func() {
	t.Helper()
	path := filepath.Join(suite.layout.State, "runtime-setup-hold")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := os.Remove(path); err != nil {
			t.Errorf("release host setup hold: %v", err)
		}
	}
	t.Cleanup(release)
	return release
}

func TestProviderScriptedLoopWatchdogProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)

	// Default-on, end to end: nobody armed this run. The override names ONE phase — the only
	// sanctioned way to watch a 10-minute deadline in a test, since it may shorten and nothing may
	// lengthen — and the phases it does not name keep coop's own values, which the announcement
	// prints. So a wedged provider CLI that would once have held the loop, the task lease, and the
	// credential until a human noticed is killed, retried, and the drain finishes on its own.
	t.Run("armed by default a silent provider cannot hold the loop", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=2s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-default-armed"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "wedged-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "wait"},
			{Target: target, Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		// runLoopRecovery's own 20s ceiling is what "terminates within its budgets" means here:
		// nothing else stops an attempt that waits for a signal nobody sends, so a watchdog that
		// failed to fire would end this run as a harness kill instead of a finished drain.
		result := runLoopRecovery(t, suite, target)
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_start_timeout)") ||
			!strings.Contains(output, "starting a fresh attempt") {
			t.Fatalf("default-armed silence = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		// The kill names its clock AND the silence it observed, so the warning is actionable
		// without reading coop's source for what the outcome means.
		if !strings.Contains(output, "after no first model action for 2s (start deadline 2s)") {
			t.Fatalf("timeout warning does not report the observed silence\nstdout:\n%s\nstderr:\n%s", result.Stdout, result.Stderr)
		}
		// The phases the override never named ran at the SHIPPED values — this is the default-on
		// proof: with the old disabled defaults they would be announced as "disabled".
		if !strings.Contains(output, "start=2s idle=30m0s tool=2h0m0s attempt ceiling=48h0m0s") {
			t.Fatalf("unnamed phases did not keep the shipped armed defaults\nstdout:\n%s\nstderr:\n%s", result.Stdout, result.Stderr)
		}
		parsed, _ := agents.ParseTarget(target)
		assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 2, false)
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Outcome != "provider_start_timeout" || records[1].Outcome != "success" {
			t.Fatalf("default-armed telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	// The dangerous shape of a kill: the attempt had already moved the task to done and committed,
	// then went silent before the host could observe a trustworthy finish. A completion from an
	// attempt that had to be killed is not evidence, so it is restored — task actionable again, with
	// the informed-resume contract for the commit it left behind — and the retry closes it properly.
	t.Run("a killed attempt's completion is restored and reworked", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "idle=2s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-restored-completion"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "restore-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "complete-wait"},
			{Target: target, Stage: "work", Result: "repair-binding"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, target)
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_idle_timeout)") ||
			!strings.Contains(output, "starting a fresh attempt") {
			t.Fatalf("restored completion = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		log, err := os.ReadFile(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID, "log.md"))
		if err != nil || !strings.Contains(string(log), "the host watchdog killed this provider attempt after it stopped producing observable progress") {
			t.Fatalf("timed-out completion left no restore record in the task log: %q, %v", log, err)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Outcome != "provider_idle_timeout" || records[1].Outcome != "success" ||
			records[1].HeadBefore != records[0].HeadAfter {
			t.Fatalf("restored completion telemetry = %#v", records)
		}
		// The killed attempt's COMMIT survives its restored completion — only the completion was
		// untrustworthy — so the retry starts from it and amends it under the recovery contract:
		// the task ends done with exactly one binding whose parent is still the run's baseline.
		parsed, _ := agents.ParseTarget(target)
		assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), records[0].HeadAfter, 2, true)
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("no first output rotates without cooling then completes", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=2s,idle=20s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-silent-start"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		targets := []string{
			loopRecoveryTarget("claude", "silent-model", "work"),
			loopRecoveryTarget("codex", "rescue-model", "work"),
		}
		writeLoopRecoveryPreset(t, suite.layout.Repo, "watchdog-start", targets)
		attempts := []loopProcessAttempt{
			{Target: targets[0], Stage: "work", Result: "wait"},
			{Target: targets[1], Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, "watchdog-start")
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_start_timeout)") ||
			!strings.Contains(output, "switching to") ||
			strings.Contains(output, "iteration failed (") ||
			strings.Contains(output, "rate limited") {
			t.Fatalf("silent start = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		parsed, _ := agents.ParseTarget(targets[1])
		assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 2, false)
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Stage != "work" || records[0].Outcome != "provider_start_timeout" || records[0].Provider != "claude" ||
			records[1].Stage != "work" || records[1].Outcome != "success" || records[1].Provider != "codex" {
			t.Fatalf("silent start telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("held host setup is not provider silence", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=2s,idle=20s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-held-setup"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		targets := []string{
			loopRecoveryTarget("claude", "held-setup-model", "work"),
			loopRecoveryTarget("codex", "rescue-model", "work"),
		}
		writeLoopRecoveryPreset(t, suite.layout.Repo, "watchdog-setup", targets)
		attempts := []loopProcessAttempt{
			{Target: targets[0], Stage: "work", Result: "wait"},
			{Target: targets[1], Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		release := holdLoopHostSetup(t, suite)
		process := startLoopRecovery(t, suite, "watchdog-setup")
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "runtime", "hold", 10*time.Second)
		// Hold the box's host setup well past the 2s start deadline. A clock armed before
		// box.Run would fire right here — against a box that has launched nothing, and that
		// cancellation cannot reach while its setup runs synchronously.
		time.Sleep(3 * time.Second)
		if started := processEvents(readProcessTrace(t, suite.layout.Trace), "provider", "start"); len(started) != 0 {
			t.Fatalf("held host setup launched %d provider(s), want none", len(started))
		}
		release()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result := process.Wait(ctx)
		cancel()
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_start_timeout)") ||
			!strings.Contains(output, "switching to") {
			t.Fatalf("held setup = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		// BOTH attempts launched a provider: the delayed first attempt was clocked from its own
		// launch and then killed for real silence, not written off while setup was still running.
		if started := processEvents(trace, "provider", "start"); len(started) != 2 {
			t.Fatalf("provider launches = %d, want two\ntrace:\n%s", len(started), readProcessFile(t, suite.layout.Trace))
		}
		parsed, _ := agents.ParseTarget(targets[1])
		assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 2, false)
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Stage != "work" || records[0].Outcome != "provider_start_timeout" || records[0].Provider != "claude" ||
			records[1].Stage != "work" || records[1].Outcome != "success" || records[1].Provider != "codex" {
			t.Fatalf("held setup telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("post-progress silence retries the sole rung", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=20s,idle=2s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-idle-silence"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "idle-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "progress-wait"},
			{Target: target, Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, target)
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_idle_timeout)") ||
			!strings.Contains(output, "starting a fresh attempt") ||
			strings.Contains(output, "switching to") {
			t.Fatalf("idle silence = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		parsed, _ := agents.ParseTarget(target)
		assertLoopProcessResult(t, suite, "codex", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 2, false)
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Outcome != "provider_idle_timeout" || records[1].Outcome != "success" {
			t.Fatalf("idle silence telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("archived departure fails closed before timeout retry", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=1s,idle=20s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-archive-departure"
		archiveID := taskID + "-archive"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		seedLoopProcessTask(t, suite.layout.Repo, archiveID)
		root := filepath.Join(suite.layout.Repo, tasksRoot)
		if err := os.Rename(filepath.Join(root, stateTodo, archiveID), filepath.Join(root, stateDone, archiveID)); err != nil {
			t.Fatal(err)
		}
		target := loopRecoveryTarget("codex", "departure-model", "work")
		attempt := loopProcessAttempt{Target: target, Stage: "work", Result: "reopen-archive-wait"}
		suite.reset(t, loopRecoveryScenario(taskID, []loopProcessAttempt{attempt}))
		result := runLoopRecovery(t, suite, target)
		if result.ExitCode != 1 ||
			!strings.Contains(result.Stderr, "work stage reopened unowned archived task(s) "+archiveID) {
			t.Fatalf("timed-out archive departure = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		if attempts := processEvents(trace, "provider", "start"); len(attempts) != 1 {
			t.Fatalf("archive departure provider attempts = %d, want one\ntrace:\n%s", len(attempts), readProcessFile(t, suite.layout.Trace))
		}
		if !pathExists(filepath.Join(root, stateInProgress, taskID)) ||
			!pathExists(filepath.Join(root, stateInProgress, archiveID)) {
			t.Fatal("timeout ownership failure did not leave both tasks actionable")
		}
		t.Setenv(tasks.TestLeaseAuthorityRootEnv, processLeaseAuthorityRoot(suite.layout))
		index, err := tasks.ReadCompletionWindowIndex(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(index.Windows) != 1 {
			t.Fatalf("timeout ownership failure retained %d completion windows, want one", len(index.Windows))
		}
		for _, window := range index.Windows {
			if window.WorkSubject != taskID {
				t.Fatalf("retained completion window subject = %q, want %q", window.WorkSubject, taskID)
			}
			if _, ok := window.Baseline[archiveID]; !ok {
				t.Fatalf("retained completion window lost archived baseline %s", archiveID)
			}
		}
		if err := tasks.ReconcileCompletionWindows([]string{root}); err == nil ||
			!strings.Contains(err.Error(), "archived task(s) "+archiveID+" left done") {
			t.Fatalf("startup recovery archive departure = %v", err)
		}
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("open foreground tool suspends the idle deadline", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=20s,idle=1s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-tool-gate"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("claude", "gate-model", "work")
		attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: "tool-gated-complete"}}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecovery(t, suite, target)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		// Hold the tool open well past the idle deadline; a false idle fire would kill the
		// provider and fail the run below.
		time.Sleep(2500 * time.Millisecond)
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result := process.Wait(ctx)
		cancel()
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 || strings.Contains(output, "timed out (") {
			t.Fatalf("gated tool = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		parsed, _ := agents.ParseTarget(target)
		assertLoopProcessResult(t, suite, "claude", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 1, false)
		records := readLoopStageRecords(t, suite)
		if len(records) != 1 || records[0].Outcome != "success" {
			t.Fatalf("gated tool telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	// Grok declares no tool lifecycle, so its long foreground work reaches the host as nothing but
	// post-progress silence. Under the ordinary idle deadline it would be killed mid-gate; under
	// the conservative fallback (4 × idle) it finishes. Same 2s idle budget as the codex attempt in
	// "post-progress silence retries the sole rung" above, opposite outcome — the difference is the
	// adapter's declaration, nothing measured at runtime.
	t.Run("silent foreground work survives past the ordinary idle deadline", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=20s,idle=2s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-no-lifecycle-gate"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("grok", "gate-model", "work")
		attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: "progress-gated-complete"}}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecovery(t, suite, target)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		// Well past the 2s idle deadline and well inside the 8s fallback: a policy that ignored the
		// declaration would kill this gate here and fail the assertions below.
		time.Sleep(3500 * time.Millisecond)
		if err := os.WriteFile(filepath.Join(suite.layout.State, "loop-release-"+taskID), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		result := process.Wait(ctx)
		cancel()
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 || strings.Contains(output, "timed out (") {
			t.Fatalf("no-lifecycle gate = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		parsed, _ := agents.ParseTarget(target)
		assertLoopProcessResult(t, suite, "grok", taskID, parsed.Model, parsed.Effort, parsed.Account(), suite.repoHead, 1, false)
		records := readLoopStageRecords(t, suite)
		if len(records) != 1 || records[0].Outcome != "success" {
			t.Fatalf("no-lifecycle gate telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	// The other half of the same policy: conservative is not unbounded. The identical stream shape
	// that never finishes still dies — as post-progress silence, at the fallback rather than at the
	// ordinary idle deadline it outlives.
	t.Run("silence on a stream with no tool lifecycle is still bounded", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=20s,idle=2s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-no-lifecycle-silence"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("grok", "silence-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "progress-wait"},
			{Target: target, Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		process := startLoopRecovery(t, suite, target)
		defer process.Cleanup()
		awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
		silent := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result := process.Wait(ctx)
		cancel()
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_idle_timeout)") ||
			!strings.Contains(output, "starting a fresh attempt") {
			t.Fatalf("no-lifecycle silence = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		// The kill is the FALLBACK's, not the 2s idle deadline's: the whole run — kill, rotation,
		// and a second completed attempt — cannot fit inside the ordinary deadline it replaced.
		if elapsed := time.Since(silent); elapsed < 4*time.Second {
			t.Fatalf("silent grok attempt died %s after going silent, before the 8s fallback", elapsed)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Outcome != "provider_idle_timeout" || records[1].Outcome != "success" {
			t.Fatalf("no-lifecycle silence telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("tool cap kills a wedged foreground tool", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=20s,idle=2s,tool=4s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-tool-cap"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "cap-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "tool-wait"},
			{Target: target, Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, target)
		output := result.Stdout + result.Stderr
		// The tool-cap outcome doubles as the suspension proof: with idle at 2s, an
		// unsuspended idle deadline would have fired first and named the wrong timeout.
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_tool_timeout)") {
			t.Fatalf("tool cap = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Outcome != "provider_tool_timeout" || records[1].Outcome != "success" {
			t.Fatalf("tool cap telemetry = %#v", records)
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("forged events injected into the provider's stdout cannot outlast the cap", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=20s,idle=2s,tool=4s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-forged-flood"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("codex", "flood-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "forged-flood-wait"},
			{Target: target, Stage: "work", Result: "complete"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, target)
		output := result.Stdout + result.Stderr
		// The provider opens one real tool, then a CHILD process — not the provider CLI — floods
		// the same stdout with hundreds of forged, unique-ID tool starts. Every one of them is
		// schema-valid and content-bearing, so the host does accept them as activity; what it must
		// not do is let them move the absolute cap off the first tool or grow its own state. The
		// attempt therefore still dies on the tool deadline, and never on start or idle.
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "timed out (provider_tool_timeout)") {
			t.Fatalf("forged flood = exit %d err %v\nstderr:\n%s", result.ExitCode, result.Err, result.Stderr)
		}
		// Without this the subtest could pass on a flood that never left the box.
		if injected := strings.Count(result.Stdout, "forged"); injected < 100 {
			t.Fatalf("only %d forged events reached the host decoder — the injection channel did not open", injected)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 2 ||
			records[0].Outcome != "provider_tool_timeout" || records[1].Outcome != "success" {
			t.Fatalf("forged flood telemetry = %#v", records)
		}
		trace := readProcessTrace(t, suite.layout.Trace)
		if started := processEvents(trace, "provider", "start"); len(started) != 2 {
			t.Fatalf("forged flood provider launches = %d, want two\ntrace:\n%s", len(started), readProcessFile(t, suite.layout.Trace))
		}
		assertLoopTraceProcessesGone(t, trace)
	})

	t.Run("three consecutive timeouts stop with the task actionable", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=1s,idle=20s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-timeout-cap"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		target := loopRecoveryTarget("claude", "cap-model", "work")
		attempts := []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "wait"},
			{Target: target, Stage: "work", Result: "wait"},
			{Target: target, Stage: "work", Result: "wait"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopRecovery(t, suite, target)
		if result.ExitCode == 0 || !strings.Contains(result.Stderr, "timed out 3 times in a row") {
			t.Fatalf("timeout cap = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 3 {
			t.Fatalf("timeout cap telemetry = %#v", records)
		}
		for i, record := range records {
			if record.Outcome != "provider_start_timeout" {
				t.Fatalf("timeout cap record %d outcome = %q, want provider_start_timeout", i, record.Outcome)
			}
		}
		assertLoopTaskRecoverable(t, suite, taskID)
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	t.Run("user interrupt wins over an armed watchdog", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=8s,idle=20s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-interrupt-precedence"
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
		records := readLoopStageRecords(t, suite)
		if len(records) != 1 || records[0].Outcome != "interrupted" {
			t.Fatalf("interrupt precedence telemetry = %#v, want interrupted (never a provider timeout)", records)
		}
		assertLoopTaskRecoverable(t, suite, taskID)
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})

	for _, interactive := range []bool{true, false} {
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
			mode := "redirected"
			if interactive {
				mode = "interactive"
			}
			t.Run(mode+" "+sig.String()+" tears down active provider", func(t *testing.T) {
				setLoopWatchdogDeadlines(t, suite, "start=8s,idle=20s,tool=30s")
				resetLoopProcessRepo(t, suite)
				taskID := "watchdog-" + mode + "-" + strings.ToLower(sig.String())
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				target := loopRecoveryTarget("codex", "termination-model", "work")
				attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: "wait"}}
				suite.reset(t, loopRecoveryScenario(taskID, attempts))
				start := startLoopRecovery
				if interactive {
					start = startLoopRecoveryPTY
				}
				process := start(t, suite, target)
				defer process.Cleanup()
				awaitProcessEvent(t, suite.layout.Trace, "provider", "ready", 10*time.Second)
				coopPID := awaitDescendantPID(t, process.PID(), filepath.Base(suite.coopBin), 5*time.Second)
				if err := syscall.Kill(coopPID, sig); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				result := process.Wait(ctx)
				cancel()
				if result.ExitCode == 0 {
					t.Fatalf("%s-interrupted loop unexpectedly succeeded: %#v", sig, result)
				}
				records := readLoopStageRecords(t, suite)
				if len(records) != 1 || records[0].Outcome != "interrupted" {
					t.Fatalf("%s telemetry = %#v, want interrupted", sig, records)
				}
				assertLoopTaskRecoverable(t, suite, taskID)
				assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
			})
		}
	}

	t.Run("signoff timeout discards the receipt and retries", func(t *testing.T) {
		setLoopWatchdogDeadlines(t, suite, "start=2s,idle=20s,tool=30s")
		resetLoopProcessRepo(t, suite)
		taskID := "watchdog-signoff-timeout"
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		work := loopRecoveryTarget("codex", "work-model", "work")
		signoff := loopRecoveryTarget("gemini", "signoff-model", "work")
		writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{signoff}, nil, 3)
		attempts := []loopProcessAttempt{
			{Target: work, Stage: "work", Result: "complete"},
			{Target: signoff, Stage: "signoff", Result: "wait"},
			{Target: signoff, Stage: "signoff", Result: "pass"},
		}
		suite.reset(t, loopRecoveryScenario(taskID, attempts))
		result := runLoopReview(t, suite, work, 20*time.Second)
		output := result.Stdout + result.Stderr
		if result.Err != nil || result.ExitCode != 0 ||
			!strings.Contains(output, "review provider attempt timed out (provider_start_timeout)") {
			t.Fatalf("signoff timeout = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
		}
		records := readLoopStageRecords(t, suite)
		if len(records) != 3 ||
			records[0].Stage != "work" || records[0].Outcome != "success" ||
			records[1].Stage != "signoff" || records[1].Outcome != "provider_start_timeout" ||
			records[2].Stage != "signoff" || records[2].Outcome != "success" {
			t.Fatalf("signoff timeout telemetry = %#v", records)
		}
		if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)) {
			t.Fatal("signoff-timed-out task did not stay completed after the retried pass")
		}
		assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
	})
}
