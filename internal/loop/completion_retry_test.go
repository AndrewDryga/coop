package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

func TestNoChangeCompletionKeepsFirstAttemptEvidence(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, scenario := range []string{"unchanged retry", "inherited dirty retry", "dirty retry", "committed retry", "post-completion hidden edit", "restored retry", "implemented retry", "next task baseline", "output limit retry", "account rotation retry", "background timeout retry"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			writeTaskFile(t, filepath.Join(repo, "source"), "original\n")
			git("add", ".gitignore", "source")
			git("commit", "-m", "base")
			if scenario == "inherited dirty retry" {
				writeTaskFile(t, filepath.Join(repo, "source"), "inherited\n")
			}
			id := "decision"
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "task.md"), "# Decision\n- [x] Required checks passed\n")
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, id, "state.md"), "# State — Decision\n\n**Status:** in progress\n**Done so far:** verified\n**Next action:** complete\n**Traps:** none\n")
			if scenario == "next task baseline" {
				writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "next", "task.md"), "# Next\n- [x] Required checks passed\n")
				writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "next", "state.md"), "# State — Next\n\n**Status:** in progress\n**Done so far:** verified\n**Next action:** complete\n**Traps:** none\n")
			}
			cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			attempts := 0
			c.boxRun = func(spec box.RunSpec) (int, error) {
				attempts++
				if attempts > 5 || spec.TaskTools == nil {
					t.Fatalf("unexpected attempt %d", attempts)
				}
				if attempts == 1 && scenario != "post-completion hidden edit" && scenario != "next task baseline" {
					if scenario != "unchanged retry" && scenario != "inherited dirty retry" {
						writeTaskFile(t, filepath.Join(repo, "source"), "attempt work\n")
					}
					if scenario == "committed retry" {
						git("add", "source")
						git("commit", "-m", "unfinished work without a task binding")
					}
					if scenario == "dirty retry" {
						_, err := io.WriteString(spec.Stdout, "{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"result\":\"ordinary work failure\"}\n")
						return 1, err
					}
					if scenario == "output limit retry" || scenario == "account rotation retry" {
						diagnostic := "finish_reason: length\n"
						if scenario == "account rotation retry" {
							diagnostic = "not logged in\n"
						}
						_, err := io.WriteString(spec.Stderr, diagnostic)
						return 1, err
					}
					if scenario == "background timeout retry" {
						return box.DescendantsTimedOutExit, nil
					}
					return box.DescendantsDrainedExit, nil
				}
				if scenario == "restored retry" {
					writeTaskFile(t, filepath.Join(repo, "source"), "original\n")
				}
				implemented := scenario == "implemented retry" || scenario == "next task baseline" && attempts == 1
				if implemented {
					writeTaskFile(t, filepath.Join(repo, "source"), "verified implementation\n")
					git("add", "source")
					git("commit", "-m", "Implement verified work\n\nCoop-Task: "+id)
				}
				completionID := id
				if scenario == "next task baseline" && attempts == 2 {
					completionID = "next"
				}
				client, server := net.Pipe()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- spec.TaskTools.Serve(ctx, server) }()
				defer func() { cancel(); client.Close(); server.Close(); <-done }()
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
						t.Fatalf("MCP %s: %s", method, line)
					}
					return result
				}
				request("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}})
				args := map[string]any{"id": completionID}
				if !implemented {
					args["outcome"] = "already_satisfied"
					args["reason"] = "Existing implementation satisfies the task."
					args["evidence"] = "Required checks and inspection passed."
				}
				reply := request("tools/call", map[string]any{"name": "tasks_complete", "arguments": args})
				t.Logf("attempt %d completion response: %v", attempts, reply)
				wantDenial := scenario == "dirty retry" || scenario == "committed retry" || scenario == "output limit retry" || scenario == "account rotation retry" || scenario == "background timeout retry"
				if (reply["isError"] == true) != wantDenial {
					t.Fatalf("completion refusal = %v, want denial %v", reply, wantDenial)
				}
				if wantDenial {
					body, err := json.Marshal(reply)
					if err != nil || !strings.Contains(string(body), "no-change completion changed") {
						t.Fatalf("wrong refusal: %s, %v", body, err)
					}
				}
				if attempts == 1 && scenario == "post-completion hidden edit" {
					if reply["isError"] == true {
						t.Fatalf("initial completion denied: %v", reply)
					}
					git("update-index", "--assume-unchanged", "source")
					writeTaskFile(t, filepath.Join(repo, "source"), "attempt work\n")
					if status := forkspace.GitOut(repo, "status", "--porcelain", "--untracked-files=all"); status != "" {
						t.Fatalf("hidden edit is visible: %s", status)
					}
				}
				// An absent native session makes a denied terminal action stop without additional
				// exact-session corrections. Completion itself still crosses the real MCP server.
				_, err := io.WriteString(spec.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"result\":\"done\"}\n")
				return 0, err
			}
			var code int
			var runErr error
			maxTasks := 1
			if scenario == "next task baseline" {
				maxTasks = 2
			}
			targets := []agents.Target{target("claude", "test")}
			if scenario == "account rotation retry" {
				targets = append(targets, target("claude", "backup"))
			}
			output := captureStderr(t, func() {
				code, runErr = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: maxTasks, Rotation: ladder.NewRotation(targets)})
			})
			allowed := scenario == "unchanged retry" || scenario == "inherited dirty retry" || scenario == "restored retry" || scenario == "implemented retry" || scenario == "next task baseline"
			current, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), id)
			if err != nil || !ok {
				t.Fatalf("task disappeared: %v %v", ok, err)
			}
			if allowed {
				if code != 0 || runErr != nil || current.State != tasks.StateDone || attempts != 2 {
					t.Fatalf("unchanged retry failed: %d %v state=%s attempts=%d", code, runErr, current.State, attempts)
				}
				wantBody := "original\n"
				if scenario == "inherited dirty retry" {
					wantBody = "inherited\n"
				} else if scenario == "implemented retry" || scenario == "next task baseline" {
					wantBody = "verified implementation\n"
				}
				body, err := os.ReadFile(filepath.Join(repo, "source"))
				if err != nil || string(body) != wantBody {
					t.Fatalf("accepted retry lost work: %q, %v", body, err)
				}
				if scenario == "next task baseline" {
					next, ok, err := tasks.CurrentTask(filepath.Join(repo, tasksRoot), "next")
					if err != nil || !ok || next.State != tasks.StateDone {
						t.Fatalf("next task did not establish its own baseline: %v, %v, %v", next, ok, err)
					}
				}
			} else {
				wantAttempts := 2
				if scenario == "post-completion hidden edit" {
					wantAttempts = 1
				}
				if attempts != wantAttempts {
					t.Fatalf("denied task retried %d times, want %d", attempts, wantAttempts)
				}
				if code == 0 || runErr == nil || current.State != tasks.StateInProgress || strings.Contains(output, "All tasks passed") {
					t.Fatalf("prior-attempt work certified as no-change: %d %v state=%s attempts=%d\n%s", code, runErr, current.State, attempts, output)
				}
				body, err := os.ReadFile(filepath.Join(repo, "source"))
				if err != nil || string(body) != "attempt work\n" {
					t.Fatalf("denial lost work: %q %v", body, err)
				}
			}
		})
	}
}

// A .coopignore the secret decider cannot read refuses every launch the same way, and a box can plant
// one mid-run. The loop stops at the first refusal and returns it, so coop loop names the file,
// instead of retrying it as a failed task attempt and ending on "Stopped after 5 failed attempts".
// Nothing ran in the box, so the refusal leaves no completion window for the next run to recover.
func TestAnUnreadableCoopignoreStopsTheRunAtItsFirstLaunch(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	for _, stage := range []string{"pre-flight", "task attempt"} {
		t.Run(stage, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "work", "task.md"), "# Work\n")
			if stage == "pre-flight" {
				writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), "preflight:\n  enabled: true\n  prompt: tidy the queue\n")
			}
			cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			refusal := box.UnreadableCoopignore{File: "src/.coopignore", Reason: "is a folder"}
			attempts := 0
			c.boxRun = func(box.RunSpec) (int, error) {
				attempts++
				return -1, fmt.Errorf("prepare the box: %w", refusal) // as box.Run refuses before any launch
			}
			var code int
			var runErr error
			output := captureStderr(t, func() {
				code, runErr = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard,
					Preflight: stage == "pre-flight", Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
			})
			var got box.UnreadableCoopignore
			if code != 1 || !errors.As(runErr, &got) || got != refusal {
				t.Fatalf("run = (%d, %v), want exit 1 with the unreadable .coopignore\n%s", code, runErr, output)
			}
			if attempts != 1 || strings.Contains(output, "Task attempt failed") {
				t.Errorf("launches = %d, want the run to stop at the first refusal\n%s", attempts, output)
			}
			// Left open, the window would outlive this process, and the next loop would try to recover
			// it, failing if a task was completed on the host meanwhile.
			if index, err := tasks.ReadCompletionWindowIndex(filepath.Join(repo, tasksRoot)); err != nil || len(index.Windows) != 0 {
				t.Errorf("the refused launch left completion windows %v (%v)", index.Windows, err)
			}
		})
	}
}
