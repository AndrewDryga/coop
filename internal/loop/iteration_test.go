package loop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
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

// TestRunIterationStopsBeforeLaunchOnCompletionWindowSetupFailure and (in loop_test.go)
// TestLoopRejectsActionableDuplicateIDsAcrossQueues live HERE, not in internal/tasks, despite testing
// a completion-window/task-queue setup failure: their subject under test — runIteration/Run — is this
// package's own orchestration, which internal/tasks does not own and must not import back (see
// internal/importdag_test.go's invariant 1). They reach into internal/tasks only for exported setup
// primitives, the same shape internal/tasks's own refauthority_test.go staying tests use.
func TestRunIterationStopsBeforeLaunchOnCompletionWindowSetupFailure(t *testing.T) {
	root := t.TempDir()
	indexName, err := tasks.CompletionWindowIndexName(root)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tasks.OpenLeaseAuthorityRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.AtomicWriteTaskFile(registry, indexName, []byte("{not-json\n")); err != nil {
		_ = registry.Close()
		t.Fatal(err)
	}
	_ = registry.Close()

	c := &Control{}
	code, output, usage, classification, windows, err := c.runIteration(
		context.Background(), t.TempDir(), "must-not-launch", "codex", "", []string{"must-not-launch"},
		false, false, []string{root}, completionWindowWork, []string{"assigned"}, nil, true, io.Discard, nil, "setup failure", "", nil,
	)
	if code != 1 || !errors.Is(err, tasks.ErrCompletionWindowSetup) || windows != nil || output != "" || usage != nil {
		t.Fatalf("setup-failed iteration = code %d output %q usage %#v windows %#v err %v", code, output, usage, windows, err)
	}
	if !strings.Contains(err.Error(), "read completion window index") {
		t.Fatalf("setup failed for another reason: %v", err)
	}
	if classification.outcome != "process_failure" {
		t.Fatalf("setup-failed iteration outcome = %q, want process_failure", classification.outcome)
	}
}

// Every production caller names a mode. An iteration with none stops at setup, before a window opens
// or a box launches, instead of running under another mode's rules.
func TestRunIterationRefusesAnIterationWithNoCompletionWindowMode(t *testing.T) {
	launched := false
	c := &Control{
		cfg: &config.Config{}, net: newNetworkLog(),
		boxRun: func(box.RunSpec) (int, error) { launched = true; return 0, nil },
	}
	code, _, _, _, windows, err := c.runIteration(
		context.Background(), t.TempDir(), "must-not-launch", "codex", "", []string{"must-not-launch"},
		false, false, []string{t.TempDir()}, 0, []string{"assigned"}, nil, true, io.Discard, nil, "no mode", "", nil,
	)
	if code != 1 || !errors.Is(err, tasks.ErrCompletionWindowSetup) || windows != nil || launched {
		t.Fatalf("iteration with no mode = code %d windows %#v launched %t err %v", code, windows, launched, err)
	}
}

func TestLoopIterationOptsIntoNarrationButNotProjectPortPublication(t *testing.T) {
	root := t.TempDir()
	var launched box.RunSpec
	c := &Control{
		cfg: &config.Config{}, net: newNetworkLog(),
		boxRun: func(spec box.RunSpec) (int, error) { launched = spec; return 0, nil },
	}
	_, _, _, _, windows, err := c.runIteration(
		context.Background(), t.TempDir(), "image", "codex", "", []string{"true"},
		false, false, []string{root}, completionWindowWork, []string{"assigned"}, nil, true, io.Discard, nil, "test", "", nil,
	)
	if windows != nil {
		_ = windows.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if !launched.Batch || !launched.LoopPresentation || launched.Serve {
		t.Fatalf("loop launch flags = Batch %t LoopPresentation %t Serve %t", launched.Batch, launched.LoopPresentation, launched.Serve)
	}
}

// A box that can consult names the run's peer ledger in COOP_RUN_ID, so each consult's usage reaches
// the closing cost digest. Before, only a preset box got it, and a --peer loop recorded nothing. A box
// with no peer to ask, or a run whose ledger could not be created, gets none.
func TestConsultBoxesNameTheRunsPeerLedger(t *testing.T) {
	for _, tc := range []struct {
		name   string
		peers  []agents.Target
		ledger bool
		want   bool
	}{
		{"peer with a ledger", []agents.Target{{Provider: "codex"}}, true, true},
		{"no peer", nil, true, false},
		{"ledger not created", []agents.Target{{Provider: "codex"}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var launched box.RunSpec
			c := &Control{
				cfg: &config.Config{}, net: newNetworkLog(), runID: "0123456789abcdef", peerLedger: tc.ledger,
				boxRun: func(spec box.RunSpec) (int, error) { launched = spec; return 0, nil },
			}
			_, _, _, _, windows, err := c.runIteration(
				context.Background(), t.TempDir(), "image", "claude", "", []string{"true"},
				false, false, []string{t.TempDir()}, completionWindowWork, []string{"assigned"}, nil, true, io.Discard, tc.peers, "test", "", nil,
			)
			if windows != nil {
				_ = windows.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(launched.ExtraArgs, "COOP_RUN_ID=0123456789abcdef"); got != tc.want {
				t.Fatalf("box environment %q names the ledger = %t, want %t", launched.ExtraArgs, got, tc.want)
			}
		})
	}
}

// The --debug-on-fail shell keeps the agent's account, but it is a shell: its box announces the
// shell, and the hint above the prompt says how to stop the loop, since Ctrl-C there reaches only
// the shell.
func TestDebugShellIsAnnouncedAsAShell(t *testing.T) {
	var launched box.RunSpec
	c := &Control{cfg: &config.Config{Shell: "/bin/bash"}, net: newNetworkLog(),
		boxRun: func(spec box.RunSpec) (int, error) { launched = spec; return 0, nil }}
	c.debugShell(t.TempDir(), "img", "claude", "")
	if launched.Agent != "claude" || !slices.Equal(launched.Cmd, []string{"/bin/bash"}) || launched.Subject != "bash" {
		t.Errorf("debug shell = agent %q cmd %q subject %q, want the claude account running bash, named bash", launched.Agent, launched.Cmd, launched.Subject)
	}
	if launched.StartingNotice != "Exit the shell to retry. To stop the loop, press Ctrl-C after you exit." {
		t.Errorf("debug shell hint = %q", launched.StartingNotice)
	}
}

func TestLoopIdentityIsCapturedButRawTraceStaysRaw(t *testing.T) {
	repo, root := t.TempDir(), t.TempDir()
	events := "{\"type\":\"system\",\"subtype\":\"init\",\"model\":\"claude-opus-5\"}\n" +
		"{\"type\":\"result\",\"subtype\":\"success\",\"num_turns\":1}\n"
	var forkLog bytes.Buffer
	c := &Control{
		cfg: &config.Config{StreamTrace: true}, net: newNetworkLog(), runID: "run",
		boxRun: func(spec box.RunSpec) (int, error) {
			_, err := io.WriteString(spec.Stdout, events)
			return 0, err
		},
	}
	_, _, _, _, windows, err := c.runIteration(
		context.Background(), repo, "image", "claude", "", []string{"claude"},
		true, true, []string{root}, completionWindowWork, []string{"assigned"}, nil, true, &forkLog, nil, "test", "", nil,
	)
	if windows != nil {
		_ = windows.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	traceDir := filepath.Join(repo, ".agent", "runs", "run.streams")
	raw, err := os.ReadFile(filepath.Join(traceDir, "01-claude.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != events {
		t.Fatalf("raw trace = %q, want untouched provider events", raw)
	}
	rendered, err := os.ReadFile(filepath.Join(traceDir, "01-claude.out"))
	if err != nil {
		t.Fatal(err)
	}
	for label, output := range map[string]string{"fork log": forkLog.String(), "rendered trace": string(rendered)} {
		if strings.Count(output, "Starting claude:claude-opus-5") != 1 || strings.Contains(output, "· using") {
			t.Fatalf("%s did not capture exactly one replacement identity: %q", label, output)
		}
	}
}

// Review rework adds one repair commit without a Coop-Task trailer: the task's one binding is
// already in history. The box's prepare-commit-msg hook stamps the trailer of RunSpec.AssignedTask
// on every commit, so a rework attempt hands the box none. The fake box commits the way that hook
// does — the trailer exactly when AssignedTask is set — and closes through tasks_complete: the
// repair is accepted, and the same repair stamped is refused.
func TestReviewReworkCommitsStayUnboundThroughTheHook(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, stamp := range []bool{false, true} {
		t.Run(map[bool]string{false: "the hook stamps nothing", true: "a stamped repair is refused"}[stamp], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			const id = "rework"
			git("commit", "--allow-empty", "-m", "Implement the task\n\nCoop-Task: "+id)
			root := filepath.Join(repo, tasksRoot)
			writeTaskFile(t, filepath.Join(root, stateInProgress, id, "task.md"), "# Rework\n- [x] verified\n")
			writeTaskFile(t, filepath.Join(root, stateInProgress, id, "log.md"), "# Log — Rework\n\n- Repaired the review finding.\n")
			record, err := tasks.CaptureAuditReopen(repo, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := tasks.WriteAuditReopenRecord(root, record); err != nil {
				t.Fatal(err)
			}
			cfg := openConfig(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true})
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			refused, refusal, attempts := false, "", 0
			c.boxRun = func(s box.RunSpec) (int, error) {
				if attempts++; attempts > 1 {
					t.Fatalf("attempt %d after tasks_complete refused = %v: %s", attempts, refused, refusal)
				}
				if s.AssignedTask != "" {
					t.Errorf("review rework handed the box task %q for its commit hook to stamp", s.AssignedTask)
				}
				message := "Repair the finding"
				if s.AssignedTask != "" || stamp {
					message += "\n\nCoop-Task: " + id
				}
				writeTaskFile(t, filepath.Join(repo, "repair.txt"), "repaired\n")
				git("add", "repair.txt")
				git("commit", "-m", message)
				refused, refusal = completeThroughTools(t, s.TaskTools, id)
				_, err := io.WriteString(s.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"result\":\"done\"}\n")
				return 0, err
			}
			var code int
			output := captureStderr(t, func() {
				code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: 1, Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
			})
			if refused != stamp {
				t.Fatalf("tasks_complete refused = %v (%s), want %v\n%s", refused, refusal, stamp, output)
			}
			if stamp {
				// Refused for the trailer, not for a fixture gap another completion rule caught.
				if !strings.Contains(refusal, "without a Coop-Task trailer") {
					t.Fatalf("stamped repair refused for another reason: %s", refusal)
				}
				return
			}
			if code != 0 || err != nil {
				t.Fatalf("loop = %d, %v\n%s", code, err, output)
			}
			if current, ok, err := tasks.CurrentTask(root, id); err != nil || !ok || current.State != tasks.StateDone {
				t.Fatalf("reworked task = %v, %v, %v; want done", current, ok, err)
			}
			if bound := tasks.CommitsForTask(repo, "", id); len(bound) != 1 {
				t.Fatalf("task bindings after rework = %v, want the original one", bound)
			}
		})
	}
}

// Before a task has a commit it starts at the head it was leased at. Once its commit is in history it
// started from that commit's parent — the oldest one's, when the task has several. When history
// cannot name that parent (a root commit, a history Git cannot read) there is no base: the lease
// head would hide the task's commit from a changed-files check.
func TestTaskReviewBase(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	repo, git := gitrepo.New(t)
	git("commit", "--allow-empty", "-m", "Start the repository\n\nCoop-Task: root-task")
	git("commit", "--allow-empty", "-m", "base")
	start := forkspace.GitOut(repo, "rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "Implement the task\n\nCoop-Task: task")
	git("commit", "--allow-empty", "-m", "Another task\n\nCoop-Task: other")
	git("commit", "--allow-empty", "-m", "Finish the task\n\nCoop-Task: task")
	const leaseHead = "lease-head"
	for id, want := range map[string]string{"new-task": leaseHead, "task": start, "root-task": ""} {
		if got := taskReviewBase(repo, id, leaseHead); got != want {
			t.Errorf("taskReviewBase(%s) = %q, want %q", id, got, want)
		}
	}
	if got := taskReviewBase(t.TempDir(), "task", leaseHead); got != "" {
		t.Errorf("taskReviewBase outside a repository = %q, want none", got)
	}
}

// A worker whose task is already committed — interrupted before its folder move, or reopened by a
// review — gets the commit before the task's commit as COOP_REVIEW_BASE, so a changed-files check
// sees exactly the task's change. The review reopen's own baseline already holds that commit: a
// verification-only re-close checked against it would see nothing.
func TestCommittedTaskWorkerGetsTheCommitItStartedFrom(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "noglobal"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "nosystem"))
	for _, reopened := range []bool{false, true} {
		t.Run(map[bool]string{false: "resumed after its commit", true: "reopened by a review"}[reopened], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			repo, git := gitrepo.New(t)
			writeTaskFile(t, filepath.Join(repo, ".gitignore"), ".agent/\n")
			git("add", ".gitignore")
			git("commit", "-m", "base")
			start := forkspace.GitOut(repo, "rev-parse", "HEAD")
			const id = "committed"
			writeTaskFile(t, filepath.Join(repo, "feature.txt"), "implemented\n")
			git("add", "feature.txt")
			git("commit", "-m", "Implement the task\n\nCoop-Task: "+id)
			root := filepath.Join(repo, tasksRoot)
			writeTaskFile(t, filepath.Join(root, stateInProgress, id, "task.md"), "# Committed\n- [x] verified\n")
			writeTaskFile(t, filepath.Join(root, stateInProgress, id, "log.md"), "# Log — Committed\n\n- Verified the committed work.\n")
			if reopened {
				record, err := tasks.CaptureAuditReopen(repo, id)
				if err != nil {
					t.Fatal(err)
				}
				if record.BaselineHead == start {
					t.Fatal("fixture reopen baseline is the task's start; the test could not tell them apart")
				}
				if err := tasks.WriteAuditReopenRecord(root, record); err != nil {
					t.Fatal(err)
				}
			}
			cfg := openConfig(&config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true})
			c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
			refused, refusal, attempts := false, "", 0
			c.boxRun = func(s box.RunSpec) (int, error) {
				if attempts++; attempts > 1 {
					t.Fatalf("attempt %d after tasks_complete refused = %v: %s", attempts, refused, refusal)
				}
				if !slices.Contains(s.ExtraArgs, "COOP_REVIEW_BASE="+start) {
					t.Fatalf("worker environment %v lacks the task's starting commit %s", s.ExtraArgs, start)
				}
				if changed := forkspace.GitOut(repo, "diff", "--name-only", start+"...HEAD"); changed != "feature.txt" {
					t.Fatalf("changes since the task's base = %q, want its own feature.txt", changed)
				}
				if !reopened {
					// The resume recipe for an interrupted commit: verify it, mark it recovered, finish.
					git("commit", "--amend", "-m", "Implement the task\n\nCoop-Task: "+id+"\nCoop-Recovery: 2026-11-06T00:00:00Z")
				}
				refused, refusal = completeThroughTools(t, s.TaskTools, id)
				_, err := io.WriteString(s.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"result\":\"done\"}\n")
				return 0, err
			}
			var code int
			var err error
			output := captureStderr(t, func() {
				code, err = c.Run(RunSpec{Repo: repo, Image: "fixture", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, MaxTasks: 1, Rotation: ladder.NewRotation([]agents.Target{target("claude", "test")})})
			})
			if attempts != 1 || refused || code != 0 || err != nil {
				t.Fatalf("loop = %d, %v after %d attempts, refused %v (%s)\n%s", code, err, attempts, refused, refusal, output)
			}
			if current, ok, err := tasks.CurrentTask(root, id); err != nil || !ok || current.State != tasks.StateDone {
				t.Fatalf("committed task = %v, %v, %v; want done", current, ok, err)
			}
		})
	}
}

// completeThroughTools calls tasks_complete for id over the box's task tools and reports whether the
// tool refused it, with its reply.
func completeThroughTools(t *testing.T, tools box.TaskToolServer, id string) (bool, string) {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tools.Serve(ctx, server) }()
	defer func() { cancel(); client.Close(); server.Close(); <-done }()
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader, encoder := bufio.NewReader(client), json.NewEncoder(client)
	var result struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	for _, call := range []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}}},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "tasks_complete", "arguments": map[string]any{"id": id, "unrun_checks": []string{}}}},
	} {
		if err := encoder.Encode(call); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(line, &result); err != nil {
			t.Fatalf("MCP %v: %s, %v", call["method"], line, err)
		}
	}
	text := ""
	if len(result.Result.Content) > 0 {
		text = result.Result.Content[0].Text
	}
	return result.Result.IsError, text
}
