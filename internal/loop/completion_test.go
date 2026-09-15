package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/taskmcp"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestLoopCompletionMCPRepair(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, scenario := range []string{"same session", "explicit no change", "resume after exit", "unconfirmed commit", "failed move", "unfinished checklist", "two correction cap", "unsupported session", "abnormal exit"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			id := "decision"
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "task.md"), "# Decision\n- [x] verified no source change is required\n")
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "state.md"), "# State — Decision\n\n**Status:** in progress\n**Done so far:** verified\n**Next action:** complete\n**Traps:** none\n")
			if scenario == "unfinished checklist" {
				writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "task.md"), "# Decision\n- [ ] required verification is still pending\n")
			}
			cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			attempts := 0
			const sessionID = "018f6352-6281-7ae1-a1d5-07c3399de43d"
			c.boxRun = func(s box.RunSpec) (int, error) {
				attempts++
				if attempts > 5 || s.TaskTools == nil {
					t.Fatalf("unexpected attempt %d without usable task tools", attempts)
				}
				if scenario == "abnormal exit" {
					if strings.Contains(strings.Join(s.Cmd, "\n"), "TERMINAL TASK CORRECTION ONLY") {
						t.Fatal("abnormal provider exit entered terminal correction")
					}
					_, _ = io.WriteString(s.Stderr, "not logged in\n")
					_, _ = io.WriteString(s.Stdout, "{\"type\":\"result\",\"subtype\":\"error\",\"is_error\":true,\"num_turns\":1,\"session_id\":\""+sessionID+"\",\"result\":\"not logged in\"}\n")
					return 1, nil
				}
				if attempts > 1 {
					joined := strings.Join(s.Cmd, "\n")
					if !strings.Contains(joined, "--resume\n"+sessionID) || !strings.Contains(joined, "TERMINAL TASK CORRECTION ONLY") {
						t.Fatalf("correction did not resume exact session with bounded prompt: %q", s.Cmd)
					}
				}
				client, server := net.Pipe()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- s.TaskTools.Serve(ctx, server) }()
				defer func() {
					cancel()
					client.Close()
					server.Close()
					<-done
				}()
				reader := bufio.NewReader(client)
				request := func(method string, params any) map[string]any {
					t.Helper()
					if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
						t.Fatal(err)
					}
					if err := json.NewEncoder(client).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}); err != nil {
						t.Fatal(err)
					}
					line, err := reader.ReadBytes('\n')
					if err != nil {
						t.Fatal(err)
					}
					var reply map[string]any
					if err := json.Unmarshal(line, &reply); err != nil {
						t.Fatal(err)
					}
					result, ok := reply["result"].(map[string]any)
					if !ok {
						t.Fatalf("request %s failed: %s", method, line)
					}
					return result
				}
				request("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
				complete := func() map[string]any {
					args := map[string]any{"id": id}
					if scenario == "explicit no change" {
						args["outcome"] = "already_satisfied"
						args["reason"] = "The existing base implementation meets the task."
						args["evidence"] = "The required checklist and focused inspection passed."
					}
					return request("tools/call", map[string]any{"name": "tasks_complete", "arguments": args})
				}
				if attempts == 1 && scenario != "unfinished checklist" && scenario != "explicit no change" {
					if reply := complete(); reply["isError"] != true {
						t.Fatalf("no-commit completion accepted: %v", reply)
					}
					if current, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), id); err != nil || !ok || current.State != tasks.StateInProgress {
						t.Fatalf("refusal changed task: %v, %v, %v", current, ok, err)
					}
				}
				if scenario == "two correction cap" || scenario == "unsupported session" {
					// Keep exiting normally without a terminal action. Coop must stop after two
					// exact-session corrections instead of starting a fresh worker forever.
				} else if scenario == "explicit no change" {
					if reply := complete(); reply["isError"] == true {
						t.Fatalf("explicit no-change completion refused: %v", reply)
					}
				} else if scenario != "resume after exit" || attempts == 2 {
					if attempts == 1 || scenario == "resume after exit" {
						git("commit", "--allow-empty", "--only", "-m", "Keep the existing contract\n\nVerified acceptance; no source change required.\n\nCoop-Task: "+id)
					}
					if scenario == "failed move" {
						obstruction := filepath.Join(repo, tasksRoot, tasks.StateDone, id)
						if attempts == 1 {
							writeTaskFile(t, obstruction, "obstruct the folder move\n")
						} else if err := os.Remove(obstruction); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "unfinished checklist" && attempts == 2 {
						writeTaskFile(t, filepath.Join(repo, tasksRoot, stateInProgress, id, "task.md"), "# Decision\n- [x] required verification is complete\n")
					}
					if scenario != "unconfirmed commit" || attempts == 2 {
						wantRefusal := attempts == 1 && (scenario == "failed move" || scenario == "unfinished checklist")
						if reply := complete(); (reply["isError"] == true) != wantRefusal {
							t.Fatalf("repaired completion = %v in %s attempt %d", reply, scenario, attempts)
						}
					}
				}
				streamSession := sessionID
				if scenario == "unsupported session" {
					streamSession = ""
				}
				_, err := io.WriteString(s.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"session_id\":\""+streamSession+"\",\"result\":\"done\"}\n")
				return 0, err
			}
			var code int
			var err error
			output := captureStderr(t, func() {
				code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: 1, Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
			})
			if scenario == "two correction cap" {
				if code != 1 || err == nil || attempts != 3 || !strings.Contains(err.Error(), "after 2 terminal corrections") {
					t.Fatalf("terminal correction cap = %d, %v, %d attempts", code, err, attempts)
				}
				if current, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), id); err != nil || !ok || current.State != tasks.StateInProgress {
					t.Fatalf("capped task was not preserved: %v, %v, %v", current, ok, err)
				}
				for _, want := range []string{"Task terminal correction stopped", "preserved in progress", "Continue:", "coop loop"} {
					if !strings.Contains(output, want) {
						t.Fatalf("capped stop omitted %q: %s", want, output)
					}
				}
				if strings.Contains(output, "Task completed:") || strings.Contains(output, "All tasks passed") {
					t.Fatalf("capped task claimed success: %s", output)
				}
				return
			}
			if scenario == "unsupported session" {
				if code != 1 || err == nil || attempts != 1 || !strings.Contains(err.Error(), "cannot resume the exact worker session") {
					t.Fatalf("unsupported correction = %d, %v, %d attempts", code, err, attempts)
				}
				return
			}
			if scenario == "abnormal exit" {
				if code == 0 || err == nil || attempts < 1 || strings.Contains(output, "Returning its terminal validation error") {
					t.Fatalf("abnormal exit entered terminal correction: %d, %v, %d attempts\n%s", code, err, attempts, output)
				}
				return
			}
			wantAttempts := 2
			if scenario == "same session" || scenario == "explicit no change" {
				wantAttempts = 1
			}
			if code != 0 || err != nil || attempts != wantAttempts {
				t.Fatalf("MCP loop = %d, %v, %d attempts; want 0, nil, %d", code, err, attempts, wantAttempts)
			}
		})
	}
}

func TestAssignedCompletionDecisionRecordPreservesUnrelatedIndex(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	if err := checkAssignedCompletion(repo, base, "decision", nil, nil); err == nil {
		t.Fatal("a log-only claim without a decision commit was accepted")
	}
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("another task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "unrelated.txt")
	git("commit", "--allow-empty", "--only", "-m", "Keep the existing coverage pointer\n\nThe spec already links its tested contract. No source change is needed.\nVerified the existing pointer and the documentation gate.\n\nCoop-Task: decision")
	if err := checkAssignedCompletion(repo, base, "decision", nil, nil); err != nil {
		t.Fatalf("meaningful decision commit refused: %v", err)
	}
	if diff := gitOut(repo, "diff", "--name-only", base, "HEAD"); diff != "" {
		t.Fatalf("decision changed source: %s", diff)
	}
	if staged := gitOut(repo, "diff", "--cached", "--name-only"); staged != "unrelated.txt" {
		t.Fatalf("decision disturbed unrelated index: %q", staged)
	}
}

func TestWorkerTerminalCorrectionPromptReturnsExactBoundedError(t *testing.T) {
	got := workerTerminalCorrectionPrompt("task-a", 2, taskmcp.AssignedTerminalRefusal{
		Action: "tasks_block",
		Detail: "missing required fields: decision, options, recommendation; resend the complete arguments object.",
	})
	for _, want := range []string{
		"TERMINAL TASK CORRECTION ONLY (2 of 2)",
		"rejected tasks_block action",
		"missing required fields: decision, options, recommendation; resend the complete arguments object.",
		"Do not inspect or re-analyze source",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("correction prompt omitted %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "missing required fields:") != 1 {
		t.Fatalf("validation error was rewritten or duplicated:\n%s", got)
	}
}

func TestAssignedCompletionFeedbackRejectsInvalidBindings(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, tc := range []struct {
		name     string
		messages []string
	}{
		{"missing", []string{"work without trailer"}},
		{"malformed", []string{"work\n\nCoop-Task: task extra"}},
		{"duplicate", []string{"first\n\nCoop-Task: task", "second\n\nCoop-Task: task"}},
		{"archived foreign", []string{"forged\n\nCoop-Task: archived", "work\n\nCoop-Task: task"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, git := gitrepo.New(t)
			git("commit", "--allow-empty", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			for _, message := range tc.messages {
				git("commit", "--allow-empty", "-m", message)
			}
			err := checkAssignedCompletion(repo, base, "task", nil, map[string]string{"archived": tasks.StateDone})
			if err == nil || !strings.Contains(err.Error(), "exactly one new and reachable") {
				t.Fatalf("invalid completion feedback = %v", err)
			}
		})
	}
}
