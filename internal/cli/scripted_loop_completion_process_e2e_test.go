//go:build providere2e

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedLoopUnfinishedChecklist(t *testing.T) {
	suite := newDirectProcessSuite(t)
	for _, checklist := range []string{"", "\n## Subtasks\n- [x] Implement the fixture\n- [ ] Run required verification\n"} {
		name := "empty"
		if checklist != "" {
			name = "required verification pending"
		}
		t.Run(name, func(t *testing.T) {
			resetLoopProcessRepo(t, suite)
			t.Cleanup(func() { logLoopProcessFailure(t, suite) })
			id := "unfinished-checklist"
			seedLoopProcessTask(t, suite.layout.Repo, id)
			body := "# Unfinished fixture\n" + checklist
			dir := filepath.Join(suite.layout.Repo, tasksRoot, stateTodo, id)
			writeTaskFile(t, filepath.Join(dir, "task.md"), body)
			writeTaskFile(t, filepath.Join(dir, "tmp", "evidence.txt"), "retain verification evidence\n")
			target := "claude:loop-model@work"
			suite.reset(t, loopProcessScenario{
				Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
				Loop: loopProcessPlan{TaskID: id, Attempts: []loopProcessAttempt{{Target: target, Stage: "work", Result: "complete"}}},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			result := procharness.Run(ctx, procharness.Command{
				Path: suite.coopBin, Args: []string{"loop", target, "--max-tasks", "1", "--no-preflight", "--no-mcp"},
				Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
			})
			if result.ExitCode != 1 || !strings.Contains(result.Stderr, "task checklist is unfinished") {
				t.Fatalf("unfinished completion = %+v", result)
			}
			current := filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, id)
			if pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, id)) ||
				readProcessFile(t, filepath.Join(current, "task.md")) != body ||
				readProcessFile(t, filepath.Join(current, "tmp", "evidence.txt")) != "retain verification evidence\n" {
				t.Fatal("unfinished task was accepted, rewritten, or lost its resumable evidence")
			}
			if commits := tasks.CommitsForTask(suite.layout.Repo, "", id); len(commits) != 1 {
				t.Fatalf("refusal changed the completed implementation binding: %v", commits)
			}
			assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
		})
	}
}

func TestProviderScriptedLoopCompletionRepair(t *testing.T) {
	suite := newDirectProcessSuite(t)
	for _, scenario := range []string{"repairs decision", "parks and continues", "preserves dirty source", "static terminal", "static quota wait", "human terminal"} {
		t.Run(scenario, func(t *testing.T) {
			resetLoopProcessRepo(t, suite)
			t.Cleanup(func() { logLoopProcessFailure(t, suite) })
			id, nextID := "a-decision", "b-next"
			seedLoopProcessTask(t, suite.layout.Repo, id)
			target := "claude:loop-model@work"
			attempts := []loopProcessAttempt{{Target: target, Stage: "work", Result: "uncommitted-complete"}}
			maxTasks := "1"
			switch scenario {
			case "static quota wait":
				attempts = []loopProcessAttempt{
					{Target: target, Stage: "work", Result: "rate-limit-short"},
					{Target: target, Stage: "work", Result: "decision-complete"},
				}
			case "parks and continues":
				seedLoopProcessTask(t, suite.layout.Repo, nextID)
				writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{target}, nil, 1)
				loopProcessGit(t, suite, "add", ".agent/loop.yaml")
				loopProcessGit(t, suite, "commit", "-m", "fixture review configuration")
				maxTasks = "2"
				attempts = append(attempts,
					loopProcessAttempt{Target: target, Stage: "work", Result: "uncommitted-complete"},
					loopProcessAttempt{TaskID: nextID, Target: target, Stage: "work", Result: "complete"},
					loopProcessAttempt{TaskID: nextID, Target: target, Stage: "signoff", Result: "pass"})
			case "preserves dirty source":
				if err := os.WriteFile(filepath.Join(suite.layout.Repo, "unrelated.txt"), []byte("preserve me\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				attempts = append(attempts, loopProcessAttempt{Target: target, Stage: "work", Result: "decision-complete"})
			}
			suite.reset(t, loopProcessScenario{
				Version: 6, Provider: "claude", ProviderHomes: agents.Names(),
				Loop: loopProcessPlan{TaskID: id, Attempts: attempts},
			})
			command := procharness.Command{
				Path: suite.coopBin, Args: []string{"loop", target, "--max-tasks", maxTasks, "--no-preflight", "--no-mcp"},
				Dir: suite.layout.Repo, Env: suite.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
			}
			if scenario == "parks and continues" {
				command.Args = []string{"loop", target, "--no-preflight", "--no-mcp"}
			}
			if strings.HasPrefix(scenario, "static") || scenario == "human terminal" {
				term := "dumb"
				if scenario == "human terminal" {
					term = "xterm-256color"
				} else {
					command.Env = replaceProcessEnv(command.Env, "NO_COLOR", "1")
				}
				command.Env = replaceProcessEnv(command.Env, "TERM", term)
				command.Env = replaceProcessEnv(command.Env, "COOP_SPINNER", "0")
				command = terminalLoopCommand(t, command)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			result := procharness.Run(ctx, command)
			cancel()
			output := result.Stdout + result.Stderr
			if scenario == "preserves dirty source" {
				if result.ExitCode == 0 || !strings.Contains(output, "completion rejected") || strings.Contains(output, "automatic repair") {
					t.Fatalf("dirty refusal = %+v", result)
				}
				if got := readProcessFile(t, filepath.Join(suite.layout.Repo, "unrelated.txt")); got != "preserve me\n" {
					t.Fatalf("dirty source changed: %q", got)
				}
				if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateInProgress, id)) {
					t.Fatal("dirty task was not preserved in progress")
				}
				return
			}
			wantExit := 0
			if scenario == "parks and continues" {
				wantExit = 3
			}
			if result.ExitCode != wantExit || (wantExit == 0 && result.Err != nil) {
				t.Fatalf("completion repair = %+v", result)
			}
			firstOutcome := "completion_rejected"
			if scenario == "static quota wait" {
				firstOutcome = "rate_limit"
				if !strings.Contains(output, "usage limit reached") || !strings.Contains(output, "Continuing in less than a minute") {
					t.Fatalf("static quota wait was silent: %s", output)
				}
			} else if !strings.Contains(output, "Completion was not accepted") || !strings.Contains(output, "No task-bound commit was found, starting one repair attempt") {
				t.Fatalf("repair was silent: %s", output)
			}
			records := readLoopStageRecords(t, suite)
			if len(records) != len(attempts) || records[0].Outcome != firstOutcome {
				t.Fatalf("repair telemetry = %#v", records)
			}
			if scenario == "parks and continues" {
				if records[1].Outcome != "completion_blocked" || !strings.Contains(output, "still cannot be completed") || !strings.Contains(output, "Continuing the task queue") || !strings.Contains(output, "coop tasks decisions -i") {
					t.Fatalf("park output/telemetry = %s\n%#v", output, records)
				}
				if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateBlocked, id, "decision.md")) || !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, nextID)) || pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, id)) {
					t.Fatal("blocked task was lost, falsely completed, or stopped the next task")
				}
				if !strings.Contains(output, "Stopped for your decision") || strings.Contains(output, "All tasks passed final review") || records[2].Outcome != "success" {
					t.Fatalf("blocked queue claimed success or skipped next task: %s\n%#v", output, records)
				}
			} else {
				if records[1].Outcome != "success" {
					t.Fatalf("repair did not succeed: %#v", records)
				}
				if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateDone, id)) {
					t.Fatal("repaired task was not completed")
				}
				if diff := loopProcessGit(t, suite, "diff", "--stat", suite.repoHead, "HEAD"); diff != "" {
					t.Fatalf("decision fabricated source changes: %s", diff)
				}
			}
			if strings.HasPrefix(scenario, "static") {
				for _, want := range []string{"Task 1 - Attempt 1", "Scripted loop lifecycle", "Agent  " + target, "Task completed:", "Paused after 1 of 1 requested task", "Final review has not run.", "Continue:"} {
					if !strings.Contains(output, want) {
						t.Fatalf("static terminal omitted %q: %s", want, output)
					}
				}
				if strings.Contains(output, "All tasks passed final review") {
					t.Fatalf("bounded static run falsely claimed final review: %s", output)
				}
				for _, repaint := range []string{"\x1b[K", "\x1b[J", "\x1b[1A"} {
					if strings.Contains(output, repaint) {
						t.Fatalf("TERM=dumb repainted the supervised terminal: %q", repaint)
					}
				}
			}
			if scenario == "human terminal" && (!strings.Contains(output, "\x1b[K") || !strings.Contains(output, "\x1b[J") || !strings.Contains(output, target) || !strings.Contains(output, "Task completed:")) {
				t.Fatalf("human terminal lost its live UI or task/provider/result identity: %q", output)
			}
			assertLoopTraceProcessesGone(t, readProcessTrace(t, suite.layout.Trace))
		})
	}
}
