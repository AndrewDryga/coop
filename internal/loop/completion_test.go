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
	for _, scenario := range []string{"same session", "explicit no change", "no change after refusal", "edit after no-change completion", "resume after exit", "unconfirmed commit", "failed move", "unfinished checklist", "two correction cap", "unsupported session", "abnormal exit"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			base := gitOut(repo, "rev-parse", "HEAD")
			if scenario == "edit after no-change completion" {
				writeTaskFile(t, filepath.Join(repo, "inherited.txt"), "inherited work\n")
			}
			id := "decision"
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "task.md"), "# Decision\n- [x] required acceptance checks passed\n")
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
				noChange := scenario == "explicit no change" || scenario == "edit after no-change completion"
				complete := func() map[string]any {
					args := map[string]any{"id": id}
					if noChange {
						args["outcome"] = "already_satisfied"
						args["reason"] = "The existing base implementation meets the task."
						args["evidence"] = "The required checklist and focused inspection passed."
					}
					return request("tools/call", map[string]any{"name": "tasks_complete", "arguments": args})
				}
				if attempts == 1 && scenario != "unfinished checklist" && !noChange {
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
				} else if noChange || scenario == "no change after refusal" {
					noChange = true
					if reply := complete(); reply["isError"] == true {
						t.Fatalf("explicit no-change completion refused: %v", reply)
					}
					if scenario == "edit after no-change completion" {
						status := gitOut(repo, "status", "--porcelain", "--untracked-files=all")
						writeTaskFile(t, filepath.Join(repo, "inherited.txt"), "changed after completion\n")
						if got := gitOut(repo, "status", "--porcelain", "--untracked-files=all"); got != status {
							t.Fatal("fixture must leave status labels unchanged")
						}
					}
				} else if scenario != "resume after exit" || attempts == 2 {
					if attempts == 1 || scenario == "resume after exit" {
						writeTaskFile(t, filepath.Join(repo, "answer.txt"), "implemented\n")
						git("add", "answer.txt")
						git("commit", "-m", "Implement the contract\n\nCoop-Task: "+id)
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
			if scenario == "edit after no-change completion" {
				if code == 0 || err == nil || attempts != 1 {
					t.Fatalf("post-completion mutation accepted: %d, %v, %d attempts", code, err, attempts)
				}
				if current, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), id); err != nil || !ok || current.State != tasks.StateInProgress {
					t.Fatalf("changed task was not restored: %v, %v, %v", current, ok, err)
				}
				body, readErr := os.ReadFile(filepath.Join(repo, "inherited.txt"))
				if readErr != nil || string(body) != "changed after completion\n" || gitOut(repo, "rev-parse", "HEAD") != base {
					t.Fatalf("denial lost the user's work/history: %q, %v", body, readErr)
				}
				if strings.Contains(output, "Task completed:") || strings.Contains(output, "All tasks passed") {
					t.Fatalf("post-completion mutation claimed success: %s", output)
				}
				return
			}
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
			if scenario == "same session" || scenario == "explicit no change" || scenario == "no change after refusal" {
				wantAttempts = 1
			}
			if code != 0 || err != nil || attempts != wantAttempts {
				t.Fatalf("MCP loop = %d, %v, %d attempts; want 0, nil, %d", code, err, attempts, wantAttempts)
			}
			if scenario == "explicit no change" || scenario == "no change after refusal" {
				if head := gitOut(repo, "rev-parse", "HEAD"); head != base {
					t.Fatalf("no-change completion advanced HEAD: %s -> %s", base, head)
				}
				log, err := os.ReadFile(filepath.Join(repo, tasksRoot, stateDone, id, "log.md"))
				if err != nil || !strings.Contains(string(log), "The existing base implementation meets the task.") || !strings.Contains(string(log), "The required checklist and focused inspection passed.") {
					t.Fatalf("no-change completion lost its evidence: %s, %v", log, err)
				}
			}
		})
	}
}

// The loop's finalize step warns once under its "Task completed" line when the task it accepted
// keeps a compiled program or more than 25 MB in its folder, in the words `coop tasks done` uses,
// and the agent's tasks_complete result carried the same facts once. A folder of evidence stays
// quiet, names stay escaped, and the task completes with every file in place.
func TestLoopWarnsAboutBulkInACompletedTask(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	const elf = "\x7fELF\x02\x01\x01\x00"
	// What the run prints next starts a paragraph of its own instead of reading as part of the advice.
	const advice = "\n  For a file a build can recreate, record its commit, sha256 and build command in log.md instead.\n" +
		"\nFinal review remains pending for 1 accepted task"
	for _, tc := range []struct {
		name, file string // the one file in the task's artifacts/
		content    string
		mode       os.FileMode
		size       int64    // a sparse apparent size past content, when set
		want       []string // what follows "Task completed" must hold each; none means no warning
		agent      string   // what the agent's result must hold
	}{
		{name: "program", file: "x\x1b[2Jy", content: elf, mode: 0o755,
			want: []string{
				"\n\n⚠ The task folder holds 1 compiled program\n\n",
				"      Programs: artifacts/x\\u001B[2Jy\n      Archived tasks keep their files until someone deletes them.\n" + advice,
			},
			agent: "\n\nThe task folder holds 1 compiled program.\nPrograms: artifacts/x\\u001B[2Jy\n"},
		{name: "oversized folder", file: "trace.jsonl", mode: 0o644, size: 25_000_001,
			want:  []string{"⚠ The task folder holds 25.0 MB\n", "      Largest: artifacts/trace.jsonl (25.0 MB), ", advice},
			agent: "\n\nThe task folder holds 25.0 MB.\nLargest: artifacts/trace.jsonl (25.0 MB), "},
		{name: "evidence", file: "gate.log", content: "ok\n", mode: 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			id := "ship-it"
			dir := filepath.Join(repo, tasksRoot, stateTodo, id)
			writeTaskFile(t, filepath.Join(dir, "task.md"), "# Ship it\n- [x] required checks passed\n")
			writeTaskFile(t, filepath.Join(dir, "state.md"), "# State — Ship it\n\n**Status:** in progress\n**Done so far:** verified\n**Next action:** complete\n**Traps:** none\n")
			artifact := filepath.Join(dir, "artifacts", tc.file)
			writeTaskFile(t, artifact, tc.content)
			if err := os.Chmod(artifact, tc.mode); err != nil {
				t.Fatal(err)
			}
			if tc.size > 0 {
				if err := os.Truncate(artifact, tc.size); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			attempts := 0
			var reply string
			c.boxRun = func(s box.RunSpec) (int, error) {
				if attempts++; attempts > 1 || s.TaskTools == nil {
					t.Fatalf("unexpected attempt %d without usable task tools", attempts)
				}
				writeTaskFile(t, filepath.Join(repo, "answer.txt"), "implemented\n")
				git("add", "answer.txt")
				git("commit", "-m", "Ship it\n\nCoop-Task: "+id)
				client, server := net.Pipe()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- s.TaskTools.Serve(ctx, server) }()
				defer func() { cancel(); client.Close(); server.Close(); <-done }()
				if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				reader, encoder := bufio.NewReader(client), json.NewEncoder(client)
				for _, call := range []map[string]any{
					{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}}},
					{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "tasks_complete", "arguments": map[string]any{"id": id}}},
				} {
					if err := encoder.Encode(call); err != nil {
						t.Fatal(err)
					}
					line, err := reader.ReadBytes('\n')
					if err != nil {
						t.Fatal(err)
					}
					var result struct {
						Result struct {
							Content []struct{ Text string }
							IsError bool
						}
					}
					if err := json.Unmarshal(line, &result); err != nil || result.Result.IsError {
						t.Fatalf("MCP %v: %s, %v", call["method"], line, err)
					}
					if len(result.Result.Content) > 0 {
						reply = result.Result.Content[0].Text
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
			if code != 0 || err != nil {
				t.Fatalf("loop = (%d, %v), want a completed task despite the warning\n%s", code, err, output)
			}
			_, after, found := strings.Cut(output, "✓ Task completed: Ship it")
			if !found {
				t.Fatalf("no completion line:\n%s", output)
			}
			if n, want := strings.Count(output, "The task folder "), min(len(tc.want), 1); n != want {
				t.Errorf("the terminal shows the warning %d times, want %d:\n%s", n, want, output)
			}
			if n, want := strings.Count(reply, "The task folder "), min(len(tc.want), 1); n != want {
				t.Errorf("the agent's result holds the warning %d times, want %d:\n%s", n, want, reply)
			}
			for _, want := range tc.want {
				if !strings.Contains(after, want) {
					t.Errorf("the completion lacks %q:\n%s", want, output)
				}
			}
			if !strings.Contains(reply, tc.agent) {
				t.Errorf("the agent's result lacks %q:\n%s", tc.agent, reply)
			}
			if strings.Contains(output, "\x1b[2J") || strings.Contains(reply, "\x1b[2J") {
				t.Errorf("a raw control sequence reached the terminal or the agent:\n%q\n%q", output, reply)
			}
			info, err := os.Stat(filepath.Join(repo, tasksRoot, stateDone, id, "artifacts", tc.file))
			if err != nil || info.Size() != max(tc.size, int64(len(tc.content))) {
				t.Errorf("the archived task lost its file: %v, %v", info, err)
			}
		})
	}
}

func TestAssignedNoChangeCompletionPreservesUnrelatedIndex(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("another task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "unrelated.txt")
	baseline, err := tasks.CheckoutFingerprint(repo)
	if err != nil {
		t.Fatal(err)
	}
	stagedBefore := gitOut(repo, "diff", "--cached")
	if err := checkAssignedCompletion(repo, base, "decision", nil, nil); err == nil {
		t.Fatal("ordinary implementation completion without a binding was accepted")
	} else {
		for _, want := range []string{"tasks_complete", "already_satisfied", "could_not_reproduce", "wont_fix", "reason", "evidence", "tasks_block"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal omitted no-change recovery %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "--allow-empty") {
			t.Errorf("refusal still prescribes a fake commit: %v", err)
		}
	}
	if err := checkNoChangeCompletion(repo, base, "decision", baseline); err != nil {
		t.Fatalf("explicit no-change completion refused: %v", err)
	}
	if head := gitOut(repo, "rev-parse", "HEAD"); head != base {
		t.Fatalf("no-change completion advanced history: %s", head)
	}
	if staged := gitOut(repo, "diff", "--cached"); staged != stagedBefore {
		t.Fatalf("no-change completion disturbed unrelated index: %q", staged)
	}
}

// The loop's own acceptance path, not just the authority underneath it: a worker that inherits
// staged work, rewrites its content, and leaves every status label identical must be refused here.
func TestAssignedNoChangeCompletionRejectsSameStatusRewrite(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("another task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "unrelated.txt")
	baseline, err := tasks.CheckoutFingerprint(repo)
	if err != nil {
		t.Fatal(err)
	}
	before := gitOut(repo, "status", "--porcelain", "--untracked-files=all")

	// Same path, same staged state, different bytes.
	if err := os.WriteFile(filepath.Join(repo, "unrelated.txt"), []byte("smuggled work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "unrelated.txt")
	if after := gitOut(repo, "status", "--porcelain", "--untracked-files=all"); after != before {
		t.Fatalf("the fixture moved a status label, so it does not test what it claims: %q vs %q", before, after)
	}
	if err := checkNoChangeCompletion(repo, base, "decision", baseline); err == nil {
		t.Fatal("a same-status content rewrite completed as no-change")
	}
}

func TestWorkerTerminalCorrectionPromptReturnsExactBoundedError(t *testing.T) {
	got := workerTerminalCorrectionPrompt("task-a", 2, taskmcp.AssignedTerminalRefusal{
		Action: "tasks_block",
		Detail: "missing required fields: question, why, options, recommendation; resend the complete arguments object.",
	})
	for _, want := range []string{
		"TERMINAL TASK CORRECTION ONLY (2 of 2)",
		"rejected tasks_block action",
		"missing required fields: question, why, options, recommendation; resend the complete arguments object.",
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
