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
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

type loopProcessPlan struct {
	TaskID   string               `json:"task_id"`
	Attempts []loopProcessAttempt `json:"attempts"`
}

type loopProcessAttempt struct {
	Target string `json:"target"`
	Result string `json:"result"`
}

type loopProcessScenario struct {
	Version       int             `json:"version"`
	Provider      string          `json:"provider"`
	ProviderHomes []string        `json:"provider_homes"`
	Loop          loopProcessPlan `json:"loop"`
}

func TestProviderScriptedLoopProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)
	t.Run("lifecycle matrix", func(t *testing.T) {
		for _, provider := range suite.providers {
			t.Run(provider, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				t.Cleanup(func() { logLoopProcessFailure(t, suite) })
				taskID := "loop-task-" + provider
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				model := "loop-model-" + provider
				effort := directTargetEffort(directProviderContracts[provider])
				target := provider + ":" + model
				if effort != "" {
					target += "/" + effort
				}
				target += "@work"
				scenario := loopProcessScenario{
					Version: 5, Provider: provider, ProviderHomes: agents.Names(),
					Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: target, Result: "complete"}}},
				}

				suite.reset(t, scenario)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				result := procharness.Run(ctx, procharness.Command{
					Path: suite.coopBin,
					Args: []string{"loop", target, "--max-tasks", "1", "--no-preflight", "--no-mcp"},
					Dir:  suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
				})
				cancel()
				trace := readProcessTrace(t, suite.layout.Trace)
				if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "fixture-loop-complete-"+provider) || !strings.Contains(result.Stderr, "task limit reached") {
					t.Fatalf("coop loop %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", target, result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
				}

				prompt := loopWorkPrompt(suite.layout.Repo, []string{tasksRoot}, taskID, provider, nil, nil)
				argv := loopProcessArgv(provider, model, effort, prompt)
				assertDirectRunContract(t, suite, trace, provider, "work", argv, model, effort)
				assertLoopProcessResult(t, suite, provider, taskID, model, effort, "work", suite.repoHead, 1, false)
				for _, event := range trace {
					awaitProcessGone(t, event.PID)
				}
			})
		}
	})
	t.Run("rejects unbound completion", func(t *testing.T) {
		for _, outcome := range []string{"unbound", "unbound-log-symlink", "unbound-state-symlink"} {
			t.Run(outcome, func(t *testing.T) {
				resetLoopProcessRepo(t, suite)
				t.Cleanup(func() { logLoopProcessFailure(t, suite) })
				provider := "codex"
				taskID := "loop-task-" + outcome
				seedLoopProcessTask(t, suite.layout.Repo, taskID)
				target := "codex:loop-model/high@work"
				sentinel := filepath.Join(suite.layout.State, "recovery-sentinel")
				wantSentinel := "outside recovery sentinel\n"
				if err := os.WriteFile(sentinel, []byte(wantSentinel), 0o600); err != nil {
					t.Fatal(err)
				}
				suite.reset(t, loopProcessScenario{
					Version: 5, Provider: provider, ProviderHomes: agents.Names(),
					Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: target, Result: outcome}}},
				})
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				result := procharness.Run(ctx, procharness.Command{
					Path: suite.coopBin, Args: []string{"loop", target, "--max-tasks", "1", "--no-preflight", "--no-mcp"},
					Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
				})
				cancel()
				wantError := "completion rejected"
				if result.ExitCode == 0 || !strings.Contains(result.Stderr, wantError) {
					t.Fatalf("unbound loop = exit %d err %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr)
				}
				inProgress := filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, taskID)
				if outcome != "unbound-state-symlink" {
					state, err := os.ReadFile(filepath.Join(inProgress, "state.md"))
					if err != nil || !strings.Contains(string(state), "**Status:** in progress — completion rejected") ||
						!strings.Contains(string(state), "**Next action:** repair the commit binding") {
						t.Fatalf("restored task state = %q, %v", state, err)
					}
				}
				if outcome == "unbound-log-symlink" && !strings.Contains(result.Stderr, "recovery bookkeeping also failed") {
					t.Fatalf("log recovery failure omitted bookkeeping diagnostic:\n%s", result.Stderr)
				}
				if outcome != "unbound" {
					data, err := os.ReadFile(sentinel)
					if err != nil || string(data) != wantSentinel {
						t.Fatalf("recovery touched outside sentinel = %q, %v", data, err)
					}
				}
				doneEntries, err := os.ReadDir(filepath.Join(suite.layout.Repo, tasksRoot, stateDone))
				if err != nil || len(doneEntries) != 0 {
					t.Fatalf("unbound task remained done: %v, %v", doneEntries, err)
				}
				if _, err := os.Stat(inProgress); err != nil {
					t.Fatalf("unbound task was not restored to in_progress: %v", err)
				}
				if paths, err := filepath.Glob(filepath.Join(suite.layout.Repo, ".agent", "runs", "*.jsonl")); err != nil || len(paths) != 0 {
					t.Fatalf("unbound completion wrote success telemetry: %v, %v", paths, err)
				}
				if trailer := loopProcessGit(t, suite, "log", "-1", "--format=%(trailers:key=Coop-Task,valueonly)"); trailer != "" {
					t.Fatalf("unbound fixture wrote trailer %q", trailer)
				}
				firstTrace := readProcessTrace(t, suite.layout.Trace)
				for _, event := range firstTrace {
					awaitProcessGone(t, event.PID)
				}
				if outcome == "unbound-state-symlink" {
					unboundHead := loopProcessGit(t, suite, "rev-parse", "HEAD")
					statePath := filepath.Join(inProgress, "state.md")
					if err := os.Remove(statePath); err != nil {
						t.Fatal(err)
					}
					repairedState := "# State - repaired\n\n**Status:** in progress\n**Done so far:** commit exists without binding\n**Next action:** repair binding\n**Traps:** none\n"
					if err := os.WriteFile(statePath, []byte(repairedState), 0o600); err != nil {
						t.Fatal(err)
					}
					suite.reset(t, loopProcessScenario{
						Version: 5, Provider: provider, ProviderHomes: agents.Names(),
						Loop: loopProcessPlan{TaskID: taskID, Attempts: []loopProcessAttempt{{Target: target, Result: "repair-binding"}}},
					})
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					retry := procharness.Run(ctx, procharness.Command{
						Path: suite.coopBin, Args: []string{"loop", target, "--max-tasks", "1", "--no-preflight", "--no-mcp"},
						Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
					})
					cancel()
					if retry.Err != nil || retry.ExitCode != 0 {
						t.Fatalf("repaired loop = exit %d err %v\nstdout:\n%s\nstderr:\n%s", retry.ExitCode, retry.Err, retry.Stdout, retry.Stderr)
					}
					assertLoopProcessResult(t, suite, provider, taskID, "loop-model", "high", "work", unboundHead, 1, false)
					if data, err := os.ReadFile(sentinel); err != nil || string(data) != wantSentinel {
						t.Fatalf("repaired loop touched outside sentinel = %q, %v", data, err)
					}
					for _, event := range readProcessTrace(t, suite.layout.Trace) {
						awaitProcessGone(t, event.PID)
					}
				}
			})
		}
	})
}

func resetLoopProcessRepo(t *testing.T, suite *directProcessSuite) {
	t.Helper()
	loopProcessGit(t, suite, "reset", "--hard", suite.repoHead)
	loopProcessGit(t, suite, "clean", "-fdx")
}

func seedLoopProcessTask(t *testing.T, repo, id string) {
	t.Helper()
	root := filepath.Join(repo, tasksRoot)
	for _, state := range []string{stateTodo, stateInProgress, stateBlocked, stateDone} {
		if err := os.MkdirAll(filepath.Join(root, state), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	task := filepath.Join(root, stateTodo, id)
	if err := os.Mkdir(task, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"task.md":  "# Scripted loop lifecycle\n\n**Context:** prove the external loop path.\n**Acceptance criteria:** commit and complete this task.\n**Approach:** use the closed fixture action.\n",
		"state.md": "# State - Scripted loop lifecycle\n\n**Status:** not started\n**Done so far:** none\n**Next action:** complete the fixture task\n**Traps:** none\n",
		"log.md":   "# Log - Scripted loop lifecycle\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(task, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func loopProcessArgv(provider, model, effort, prompt string) []string {
	base := directExpectedArgv(directProviderContracts[provider], model, effort, nil)
	if provider == "codex" {
		base = append([]string{base[0], "exec"}, base[1:]...)
		return append(base, prompt)
	}
	return append(base, "-p", prompt)
}

func assertLoopProcessResult(t *testing.T, suite *directProcessSuite, provider, taskID, model, effort, account, headBefore string, wantRecords int, wantRecovery bool) {
	t.Helper()
	done := filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)
	if _, err := os.Stat(filepath.Join(done, "task.md")); err != nil {
		t.Fatalf("done task missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(done, "tmp")); !os.IsNotExist(err) {
		t.Fatalf("done task retained tmp: %v", err)
	}
	state, err := os.ReadFile(filepath.Join(done, "state.md"))
	if err != nil || !strings.Contains(string(state), "**Status:** complete") || !strings.Contains(string(state), "**Next action:** none") || !strings.Contains(string(state), provider+" loop lifecycle") {
		t.Fatalf("final task state = %q, %v", state, err)
	}
	log, err := os.ReadFile(filepath.Join(done, "log.md"))
	if err != nil || !strings.Contains(string(log), provider+" completed the closed loop lifecycle") {
		t.Fatalf("final task log = %q, %v", log, err)
	}
	change, err := os.ReadFile(filepath.Join(suite.layout.Repo, "loop-"+provider+".txt"))
	if err != nil || string(change) != "completed by "+provider+"\n" {
		t.Fatalf("loop change = %q, %v", change, err)
	}
	message := loopProcessGit(t, suite, "log", "-1", "--format=%B")
	wantMessage := "fixture: complete " + provider + " loop task\n\nCoop-Task: " + taskID
	gotMessage := strings.TrimSpace(message)
	if wantRecovery {
		if !strings.HasPrefix(gotMessage, wantMessage+"\nCoop-Recovery: fixture-") || strings.Count(gotMessage, "Coop-Recovery:") != 1 || strings.Count(gotMessage, "Coop-Task:") != 1 {
			t.Fatalf("recovered task commit message = %q", message)
		}
	} else if gotMessage != wantMessage {
		t.Fatalf("task commit message = %q", message)
	}
	parent := loopProcessGit(t, suite, "rev-parse", "HEAD^")
	if parent != suite.repoHead {
		t.Fatalf("loop commit parent = %s, want %s", parent, suite.repoHead)
	}
	paths := loopProcessGit(t, suite, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if paths != "loop-"+provider+".txt" {
		t.Fatalf("loop commit paths = %q", paths)
	}
	if status := loopProcessGit(t, suite, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("loop worktree is dirty:\n%s", status)
	}
	queueRoot := filepath.Join(suite.layout.Repo, tasksRoot)
	for _, state := range []string{stateTodo, stateInProgress, stateBlocked} {
		entries, err := os.ReadDir(filepath.Join(queueRoot, state))
		if err != nil || len(entries) != 0 {
			t.Fatalf("loop queue %s = %v, %v", state, entries, err)
		}
	}
	doneEntries, err := os.ReadDir(filepath.Join(queueRoot, stateDone))
	if err != nil || len(doneEntries) != 1 || doneEntries[0].Name() != taskID {
		t.Fatalf("loop done queue = %v, %v", doneEntries, err)
	}

	telemetryPaths, err := loopStageTelemetryPaths(suite.layout.Repo)
	if err != nil || len(telemetryPaths) != 1 {
		t.Fatalf("loop telemetry files = %v, %v", telemetryPaths, err)
	}
	data, err := os.ReadFile(telemetryPaths[0])
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
	if len(records) != wantRecords {
		t.Fatalf("loop telemetry records = %#v", records)
	}
	record := records[len(records)-1]
	head := loopProcessGit(t, suite, "rev-parse", "HEAD")
	if record.Stage != "work" || record.Outcome != "success" || record.Provider != provider || record.Model != model || record.Effort != effort || record.Account != account || record.Exit != 0 || record.Retries != 0 || record.Reopened != 0 || record.HeadBefore != headBefore || record.HeadAfter != head || !slices.Equal(record.Finished, []string{taskID}) || len(record.Untrailered) != 0 || len(record.GateFiles) != 0 || record.QueueTodo != 0 || record.QueueDoing != 0 || record.QueueDone != 1 {
		t.Fatalf("loop telemetry = %#v", record)
	}
}

func loopStageTelemetryPaths(repo string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(repo, ".agent", "runs", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(paths, func(path string) bool { return strings.HasSuffix(path, ".peers.jsonl") }), nil
}

func loopProcessGit(t *testing.T, suite *directProcessSuite, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = suite.layout.Repo, suite.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func logLoopProcessFailure(t *testing.T, suite *directProcessSuite) {
	t.Helper()
	if !t.Failed() {
		return
	}
	trace, _ := os.ReadFile(suite.layout.Trace)
	t.Logf("provider trace:\n%s", trace)
	for _, args := range [][]string{{"status", "--porcelain", "--untracked-files=all"}, {"log", "--oneline", "--decorate", "-5"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = suite.layout.Repo, suite.env
		output, _ := cmd.CombinedOutput()
		t.Logf("git %s:\n%s", strings.Join(args, " "), output)
	}
	var queue strings.Builder
	_ = filepath.WalkDir(filepath.Join(suite.layout.Repo, tasksRoot), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&queue, "ERROR %s: %v\n", path, err)
			return nil
		}
		rel, _ := filepath.Rel(suite.layout.Repo, path)
		fmt.Fprintln(&queue, rel)
		if !entry.IsDir() && entry.Type().IsRegular() {
			if data, readErr := os.ReadFile(path); readErr == nil && len(data) <= 64<<10 {
				queue.Write(data)
				if len(data) > 0 && data[len(data)-1] != '\n' {
					queue.WriteByte('\n')
				}
			}
		}
		return nil
	})
	t.Logf("task queue evidence:\n%s", queue.String())
}
