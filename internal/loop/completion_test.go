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
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestLoopCompletionMCPRepair(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, scenario := range []string{"same session", "next attempt", "unconfirmed commit", "failed move", "unfinished checklist"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			id := "decision"
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "task.md"), "# Decision\n- [x] verified no source change is required\n")
			if scenario == "unfinished checklist" {
				writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "task.md"), "# Decision\n- [ ] required verification is still pending\n")
			}
			cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			attempts := 0
			c.boxRun = func(s box.RunSpec) (int, error) {
				attempts++
				if scenario == "unfinished checklist" && attempts > 1 {
					t.Fatal("unfinished committed task was retried with an advanced completion base")
				}
				if attempts > 2 || s.TaskTools == nil {
					t.Fatalf("unexpected attempt %d without usable task tools", attempts)
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
					return request("tools/call", map[string]any{"name": "tasks_complete", "arguments": map[string]any{"id": id}})
				}
				if attempts == 1 && scenario != "unfinished checklist" {
					if reply := complete(); reply["isError"] != true {
						t.Fatalf("no-commit completion accepted: %v", reply)
					}
					if current, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), id); err != nil || !ok || current.State != tasks.StateInProgress {
						t.Fatalf("refusal changed task: %v, %v, %v", current, ok, err)
					}
				}
				if scenario != "next attempt" || attempts == 2 {
					git("commit", "--allow-empty", "--only", "-m", "Keep the existing contract\n\nVerified acceptance; no source change required.\n\nCoop-Task: "+id)
					if scenario == "failed move" {
						writeTaskFile(t, filepath.Join(repo, tasksRoot, tasks.StateDone, id), "obstruct the folder move\n")
					}
					if scenario != "unconfirmed commit" {
						if reply := complete(); (reply["isError"] == true) != (scenario == "failed move" || scenario == "unfinished checklist") {
							t.Fatalf("repaired completion = %v in %s", reply, scenario)
						}
					}
				}
				_, err := io.WriteString(s.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"result\":\"done\"}\n")
				return 0, err
			}
			var code int
			var err error
			output := captureStderr(t, func() {
				code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: 1, Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
			})
			if scenario == "unconfirmed commit" || scenario == "failed move" || scenario == "unfinished checklist" {
				if code != 1 || err == nil || attempts != 1 || !strings.Contains(err.Error(), "before confirming completion") {
					t.Fatalf("unconfirmed commit was retried or accepted: %d, %v, %d attempts", code, err, attempts)
				}
				if current, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), id); err != nil || !ok || current.State != tasks.StateInProgress {
					t.Fatalf("unconfirmed task was not preserved: %v, %v, %v", current, ok, err)
				}
				for _, want := range []string{"Task completion was not confirmed", "preserved in progress", "coop tasks path decision", "Then continue:", "coop loop"} {
					if !strings.Contains(output, want) {
						t.Fatalf("unconfirmed stop omitted %q: %s", want, output)
					}
				}
				if scenario == "unfinished checklist" {
					for _, want := range []string{"0/1 subtasks done", "finish the remaining work and required checks", "pending verification is not complete"} {
						if !strings.Contains(output, want) {
							t.Fatalf("unfinished checklist stop omitted %q: %s", want, output)
						}
					}
				} else if strings.Contains(output, "task checklist is unfinished") {
					t.Fatalf("completed checklist received the wrong repair: %s", output)
				}
				if strings.Contains(output, "Task completed:") || strings.Contains(output, "All tasks passed") {
					t.Fatalf("unconfirmed task claimed success: %s", output)
				}
				return
			}
			wantAttempts := 2
			if scenario == "same session" {
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
