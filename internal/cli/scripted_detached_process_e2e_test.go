//go:build providere2e

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedDetachedForkLifecycle(t *testing.T) {
	suite := newDirectProcessSuite(t)
	provider := suite.providers[0]
	t.Run(provider, func(t *testing.T) {
		resetForkProcessRepo(t, suite)
		name := "detached-" + provider
		taskID := "detached-task-" + provider
		target := loopRecoveryTarget(provider, "detached-model-"+provider, "work")
		seedLoopProcessTask(t, suite.layout.Repo, taskID)
		suite.reset(t, loopProcessScenario{
			Version: 6, Provider: provider, ProviderHomes: agents.Names(),
			Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: target, Stage: "work", Result: "wait"}}},
		})

		started := runDetachedCLI(t, suite, suite.layout.Repo, suite.env,
			"fork", name, target, "--loop", "--detach", "--tasks", filepath.Join(suite.layout.Repo, tasksRoot))
		if started.Err != nil || started.ExitCode != 0 || !strings.Contains(started.Stderr, "started fork "+name) {
			t.Fatalf("start detached fork = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", started.ExitCode, started.Err, started.Stdout, started.Stderr, readProcessFile(t, suite.layout.Trace))
		}
		t.Cleanup(func() {
			if pathExists(forkPid(suite.layout.Repo, name)) {
				_ = runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fork", "stop", name)
			}
		})
		ready := awaitProcessEventCount(t, suite.layout.Trace, "provider", "ready", 1, 10*time.Second)
		awaitPathState(t, forkPid(suite.layout.Repo, name), true)

		duplicate := runDetachedCLI(t, suite, suite.layout.Repo, suite.env,
			"fork", name, target, "--loop", "--detach", "--tasks", filepath.Join(suite.layout.Repo, tasksRoot))
		if duplicate.ExitCode == 0 || !strings.Contains(duplicate.Stderr, "already has a loop running") {
			t.Fatalf("duplicate detached start = exit %d err %v\nstderr:\n%s", duplicate.ExitCode, duplicate.Err, duplicate.Stderr)
		}
		if got := countProcessEvents(readProcessTrace(t, suite.layout.Trace), "provider", "ready"); got != 1 {
			t.Fatalf("duplicate start reached provider: %d ready events", got)
		}

		trace := readProcessTrace(t, suite.layout.Trace)
		run := oneProcessEvent(t, trace, "runtime", "run")
		wantLabels := []string{
			"coop.fork=" + processTraceValue(name),
			"coop.fork-owner=" + processTraceValue(forkContainerOwner(suite.layout.Repo, name)),
		}
		for _, label := range wantLabels {
			if run.Run == nil || !slices.Contains(run.Run.Labels, label) {
				t.Fatalf("detached runtime labels = %#v, missing %q", run.Run, label)
			}
		}
		for _, args := range [][]string{{"fork", "ls"}, {"fleet", "watch"}} {
			status := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, args...)
			if status.Err != nil || status.ExitCode != 0 || !strings.Contains(status.Stdout, name) || !strings.Contains(status.Stdout, "running") {
				t.Fatalf("%v status = exit %d err %v\nstdout:\n%s\nstderr:\n%s", args, status.ExitCode, status.Err, status.Stdout, status.Stderr)
			}
		}

		stopped := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fork", "stop", name)
		if stopped.Err != nil || stopped.ExitCode != 0 {
			t.Fatalf("stop detached fork = exit %d err %v\nstdout:\n%s\nstderr:\n%s", stopped.ExitCode, stopped.Err, stopped.Stdout, stopped.Stderr)
		}
		awaitPathState(t, forkPid(suite.layout.Repo, name), false)
		awaitProcessGone(t, ready.PID)
		if entries, err := os.ReadDir(filepath.Join(suite.layout.State, "runtime-containers")); err == nil && len(entries) != 0 {
			t.Fatalf("runtime records survived stop: %v", entries)
		}
		again := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fork", "stop", name)
		if again.Err != nil || again.ExitCode != 0 {
			t.Fatalf("second stop = exit %d err %v\nstderr:\n%s", again.ExitCode, again.Err, again.Stderr)
		}

		unconfirmed := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fork", "rm", name, "--force")
		if unconfirmed.ExitCode != 2 || !pathExists(forkWorkspace(suite.layout.Repo, name)) || !strings.Contains(unconfirmed.Stderr, "--yes") {
			t.Fatalf("unconfirmed rm = exit %d err %v exists %v\nstderr:\n%s", unconfirmed.ExitCode, unconfirmed.Err, pathExists(forkWorkspace(suite.layout.Repo, name)), unconfirmed.Stderr)
		}
		confirmed := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fork", "rm", name, "--force", "--yes")
		if confirmed.Err != nil || confirmed.ExitCode != 0 || pathExists(forkWorkspace(suite.layout.Repo, name)) {
			t.Fatalf("confirmed rm = exit %d err %v exists %v\nstdout:\n%s\nstderr:\n%s", confirmed.ExitCode, confirmed.Err, pathExists(forkWorkspace(suite.layout.Repo, name)), confirmed.Stdout, confirmed.Stderr)
		}
	})
}

func TestProviderScriptedFleetPresetLifecycle(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetForkProcessRepo(t, suite)
	name, taskID := "fleet-rotation", "fleet-rotation-task"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	var targets []string
	var attempts []loopProcessAttempt
	for i, provider := range suite.providers {
		target := loopRecoveryTarget(provider, "fleet-model-"+provider, "work")
		targets = append(targets, target)
		result := "rate-limit"
		if i == len(suite.providers)-1 {
			result = "wait"
		}
		attempts = append(attempts, loopProcessAttempt{Target: target, Stage: "work", Result: result})
	}
	writeLoopRecoveryPreset(t, suite.layout.Repo, "fleet-all", targets)
	fleet := fmt.Sprintf("forks:\n  %s:\n    tasks: .agent/tasks\n    agent: fleet-all\n", name)
	if err := os.WriteFile(fleetYAMLFile(suite.layout.Repo), []byte(fleet), 0o600); err != nil {
		t.Fatal(err)
	}
	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: suite.providers[0], ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: attempts},
	})

	up := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "up")
	if up.Err != nil || up.ExitCode != 0 {
		t.Fatalf("fleet up = exit %d err %v\nstdout:\n%s\nstderr:\n%s", up.ExitCode, up.Err, up.Stdout, up.Stderr)
	}
	t.Cleanup(func() {
		if pathExists(forkPid(suite.layout.Repo, name)) {
			_ = runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "down")
		}
	})
	ready := awaitProcessEventCount(t, suite.layout.Trace, "provider", "ready", 1, 10*time.Second)
	awaitPathState(t, forkPid(suite.layout.Repo, name), true)

	trace := readProcessTrace(t, suite.layout.Trace)
	var gotProviders []string
	for _, record := range trace {
		if record.Source == "runtime" && record.Event == "run" && record.Run != nil {
			gotProviders = append(gotProviders, record.Run.Provider)
		}
	}
	if !slices.Equal(gotProviders, suite.providers) {
		t.Fatalf("fleet preset provider order = %v, want %v\ntrace:\n%s", gotProviders, suite.providers, readProcessFile(t, suite.layout.Trace))
	}

	second := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "up")
	if second.Err != nil || second.ExitCode != 0 || !strings.Contains(second.Stderr, "already running") {
		t.Fatalf("second fleet up = exit %d err %v\nstdout:\n%s\nstderr:\n%s", second.ExitCode, second.Err, second.Stdout, second.Stderr)
	}
	if got := countProcessEvents(readProcessTrace(t, suite.layout.Trace), "provider", "ready"); got != 1 {
		t.Fatalf("second fleet up launched another terminal provider: %d ready events", got)
	}
	watch := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "watch")
	if watch.Err != nil || watch.ExitCode != 0 || !strings.Contains(watch.Stdout, name) || !strings.Contains(watch.Stdout, "running") {
		t.Fatalf("fleet watch = exit %d err %v\nstdout:\n%s\nstderr:\n%s", watch.ExitCode, watch.Err, watch.Stdout, watch.Stderr)
	}
	down := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "down")
	if down.Err != nil || down.ExitCode != 0 || !strings.Contains(down.Stderr, "stopped 1 fork") {
		t.Fatalf("fleet down = exit %d err %v\nstdout:\n%s\nstderr:\n%s", down.ExitCode, down.Err, down.Stdout, down.Stderr)
	}
	awaitPathState(t, forkPid(suite.layout.Repo, name), false)
	awaitProcessGone(t, ready.PID)
	assertRuntimeRegistryEmpty(t, suite)
	again := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "down")
	if again.Err != nil || again.ExitCode != 0 || !strings.Contains(again.Stderr, "stopped 0 forks") {
		t.Fatalf("second fleet down = exit %d err %v\nstdout:\n%s\nstderr:\n%s", again.ExitCode, again.Err, again.Stdout, again.Stderr)
	}
	assertRuntimeRegistryEmpty(t, suite)

	orphan, err := setupFork(suite.layout.Repo, "prune-orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "dirty.txt"), []byte("discard\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unconfirmed := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "prune", "--force")
	if unconfirmed.ExitCode != 2 || !strings.Contains(unconfirmed.Stderr, "--yes") || !pathExists(orphan) {
		t.Fatalf("unconfirmed fleet prune = exit %d err %v exists %v\nstderr:\n%s", unconfirmed.ExitCode, unconfirmed.Err, pathExists(orphan), unconfirmed.Stderr)
	}
	confirmed := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fleet", "prune", "--force", "--yes")
	if confirmed.Err != nil || confirmed.ExitCode != 0 || pathExists(orphan) {
		t.Fatalf("confirmed fleet prune = exit %d err %v exists %v\nstdout:\n%s\nstderr:\n%s", confirmed.ExitCode, confirmed.Err, pathExists(orphan), confirmed.Stdout, confirmed.Stderr)
	}
}

func TestProviderScriptedDetachedCrashCleanupIsRepoScoped(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetForkProcessRepo(t, suite)
	provider, name, taskID := "codex", "same-name", "repo-scoped-cleanup"
	target := loopRecoveryTarget(provider, "crash-model", "work")
	seedLoopProcessTask(t, suite.layout.Repo, taskID)

	secondRepo := filepath.Join(suite.layout.Root, "repo-second")
	if err := os.MkdirAll(secondRepo, 0o700); err != nil {
		t.Fatal(err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	initProcessRepo(t, gitBin, secondRepo, suite.env)
	seedLoopProcessTask(t, secondRepo, taskID)
	secondEnv := replaceProcessEnv(suite.env, "COOP_REPO", secondRepo)

	suite.reset(t, loopProcessScenario{
		Version: 6, Provider: provider, ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{
			{Target: target, Stage: "work", Result: "wait"},
			{Target: target, Stage: "work", Result: "wait"},
		}},
	})
	started := runDetachedCLI(t, suite, suite.layout.Repo, suite.env,
		"fork", name, target, "--loop", "--detach", "--tasks", filepath.Join(suite.layout.Repo, tasksRoot))
	if started.Err != nil || started.ExitCode != 0 {
		t.Fatalf("start %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s", suite.layout.Repo, started.ExitCode, started.Err, started.Stdout, started.Stderr)
	}
	firstReady := awaitProcessEventCount(t, suite.layout.Trace, "provider", "ready", 1, 10*time.Second)
	started = runDetachedCLI(t, suite, secondRepo, secondEnv,
		"fork", name, target, "--loop", "--detach", "--tasks", filepath.Join(secondRepo, tasksRoot))
	if started.Err != nil || started.ExitCode != 0 {
		t.Fatalf("start %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s", secondRepo, started.ExitCode, started.Err, started.Stdout, started.Stderr)
	}
	secondReady := awaitProcessEventCount(t, suite.layout.Trace, "provider", "ready", 2, 10*time.Second)
	t.Cleanup(func() {
		for _, row := range []struct {
			repo string
			env  []string
		}{{suite.layout.Repo, suite.env}, {secondRepo, secondEnv}} {
			if pathExists(forkPid(row.repo, name)) {
				_ = runDetachedCLI(t, suite, row.repo, row.env, "fork", "stop", name)
			}
		}
	})
	if firstReady.PID == secondReady.PID {
		t.Fatal("two repos shared one provider process")
	}

	trace := readProcessTrace(t, suite.layout.Trace)
	var runs []*processRun
	for _, record := range trace {
		if record.Source == "runtime" && record.Event == "run" && record.Run != nil {
			runs = append(runs, record.Run)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("runtime runs = %d, want 2", len(runs))
	}
	readable := "coop.fork=" + processTraceValue(name)
	firstOwner := "coop.fork-owner=" + processTraceValue(forkContainerOwner(suite.layout.Repo, name))
	secondOwner := "coop.fork-owner=" + processTraceValue(forkContainerOwner(secondRepo, name))
	if firstOwner == secondOwner || !slices.Contains(runs[0].Labels, readable) || !slices.Contains(runs[1].Labels, readable) ||
		!slices.Contains(runs[0].Labels, firstOwner) || !slices.Contains(runs[1].Labels, secondOwner) {
		t.Fatalf("repo-scoped labels = %#v / %#v, want readable %q and owners %q / %q", runs[0].Labels, runs[1].Labels, readable, firstOwner, secondOwner)
	}

	stateData, err := os.ReadFile(forkPid(suite.layout.Repo, name))
	if err != nil {
		t.Fatal(err)
	}
	workerState, err := parseForkWorkerState(string(stateData))
	if err != nil || workerState.pid <= 1 || workerState.token == "" {
		t.Fatalf("first worker state = %+v, %v", workerState, err)
	}
	if err := syscall.Kill(workerState.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	awaitProcessGone(t, workerState.pid)

	unrelated := exec.Command("sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	})
	reused, err := (forkWorkerState{pid: unrelated.Process.Pid, token: workerState.token}).marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkPid(suite.layout.Repo, name), reused, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(forkWorkspace(suite.layout.Repo, name)); err != nil {
		t.Fatal(err)
	}
	status := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fork", "ls")
	if status.Err != nil || status.ExitCode != 0 || !strings.Contains(status.Stdout, name) || !strings.Contains(status.Stdout, "cleanup") {
		t.Fatalf("pidfile-only cleanup status = exit %d err %v\nstdout:\n%s\nstderr:\n%s", status.ExitCode, status.Err, status.Stdout, status.Stderr)
	}

	cleaned := runDetachedCLI(t, suite, suite.layout.Repo, suite.env, "fork", "stop", name)
	if cleaned.Err != nil || cleaned.ExitCode != 0 || !strings.Contains(cleaned.Stderr, "removed 1 orphaned box container") {
		t.Fatalf("repo-scoped crash cleanup = exit %d err %v\nstdout:\n%s\nstderr:\n%s", cleaned.ExitCode, cleaned.Err, cleaned.Stdout, cleaned.Stderr)
	}
	if !procharness.ProcessAlive(unrelated.Process.Pid) || !procharness.ProcessAlive(secondReady.PID) {
		t.Fatalf("cleanup signaled unrelated pid %d or namesake provider %d", unrelated.Process.Pid, secondReady.PID)
	}
	if pathExists(forkPid(suite.layout.Repo, name)) {
		t.Fatal("repo-scoped crash cleanup retained pidfile")
	}
	awaitProcessGone(t, firstReady.PID)
	secondStop := runDetachedCLI(t, suite, secondRepo, secondEnv, "fork", "stop", name)
	if secondStop.Err != nil || secondStop.ExitCode != 0 {
		t.Fatalf("stop second repo namesake = exit %d err %v\nstdout:\n%s\nstderr:\n%s", secondStop.ExitCode, secondStop.Err, secondStop.Stdout, secondStop.Stderr)
	}
	awaitProcessGone(t, secondReady.PID)
	assertRuntimeRegistryEmpty(t, suite)
}

func runDetachedCLI(t *testing.T, suite *directProcessSuite, repo string, env []string, args ...string) procharness.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return procharness.Run(ctx, procharness.Command{
		Path: suite.coopBin, Args: args, Dir: repo, Env: env,
		MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
	})
}

func awaitProcessEventCount(t *testing.T, path, source, event string, count int, timeout time.Duration) *processTrace {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var matches []*processTrace
		data, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var record processTrace
				if line != "" && json.Unmarshal([]byte(line), &record) == nil && record.Source == source && record.Event == event {
					copy := record
					matches = append(matches, &copy)
				}
			}
		}
		if len(matches) >= count {
			return matches[count-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s/%s #%d\ntrace:\n%s", source, event, count, readProcessFile(t, path))
	return nil
}

func awaitPathState(t *testing.T, path string, exists bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pathExists(path) == exists {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("path %s exists=%v, want %v", path, pathExists(path), exists)
}

func assertRuntimeRegistryEmpty(t *testing.T, suite *directProcessSuite) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(suite.layout.State, "runtime-containers"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("runtime records survived cleanup: %v", entries)
	}
}

func countProcessEvents(trace []*processTrace, source, event string) int {
	count := 0
	for _, record := range trace {
		if record.Source == source && record.Event == event {
			count++
		}
	}
	return count
}
