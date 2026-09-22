//go:build providerlivee2e

package loop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/liveprovider"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func accountLiveTaskBody(marker string) string {
	return fmt.Sprintf("# Verify account recovery\n\n**Context:** Complete one disposable recovery test.\n\n**Acceptance criteria:** Create `%s` containing exactly `%s` followed by one newline, with no other tracked change. Run `git diff --check`, then commit that file with subject `test: account recovery` and the exact trailer `Coop-Task: %s`. Check off the subtask without changing its text, append log.md, update state.md through tasks_update_state, then call tasks_complete as your final action. Do not create proposals, other tasks, skills, hooks, or Git configuration changes.\n\n**Approach:** Follow those mechanical steps only.\n\n## Subtasks\n- [ ] Create the marker, verify it and commit the task-bound change\n", accountLiveFile, marker, accountLiveTaskID)
}

// This writer runs only before handing a fresh InitRepository fixture to a client (or in an
// unpaid synthetic control). Post-client decisions use the production hardened gitOutErr instead.
func accountLiveFixtureGit(layout procharness.Layout, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", layout.Repo}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + layout.Home,
		"GIT_CONFIG_GLOBAL=" + layout.GitConfig, "GIT_CONFIG_SYSTEM=" + layout.GitConfig, "GIT_CONFIG_NOSYSTEM=1"}
	if err := cmd.Run(); err != nil {
		return errors.New("live account fixture Git operation failed")
	}
	return nil
}

func prepareAccountLiveRepository(layout procharness.Layout, marker string) (string, error) {
	if err := liveprovider.InitRepository(layout); err != nil {
		return "", err
	}
	contract := "# Disposable qualification repository\n\nWork only the assigned task. Its stated git diff --check gate is the complete gate here. Do not change this file, repository configuration or the loop settings. No peers or proposals are needed.\n"
	if err := os.WriteFile(filepath.Join(layout.Repo, "AGENTS.md"), []byte(contract), 0o600); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"add", "AGENTS.md"}, {"commit", "-qm", "test: add account recovery contract"}} {
		if err := accountLiveFixtureGit(layout, args...); err != nil {
			return "", err
		}
	}
	root := filepath.Join(layout.Repo, tasksRoot)
	for _, state := range []string{stateTodo, stateInProgress, stateBlocked, stateDone} {
		if err := os.MkdirAll(filepath.Join(root, state), 0o700); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(root, stateTodo, accountLiveTaskID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	for name, body := range map[string]string{
		"task.md": accountLiveTaskBody(marker), "log.md": "# Log — Account recovery\n",
		"state.md": "# State\n\n**Status:** not started\n**Done so far:** none\n**Next action:** complete the marker task\n**Traps:** none\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return "", err
		}
	}
	return gitOutErr(layout.Repo, "rev-parse", "HEAD")
}

func accountLiveRead(layout procharness.Layout, path string) ([]byte, error) {
	f, err := procharness.OpenRegularFile(layout.Root, path, os.O_RDONLY)
	if err != nil {
		return nil, errors.New("unsafe account evidence file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, errors.New("oversized or unreadable account evidence")
	}
	return data, nil
}

func verifyAccountLiveRepository(layout procharness.Layout, baseline, marker string) error {
	data, err := accountLiveRead(layout, filepath.Join(layout.Repo, accountLiveFile))
	if err != nil || string(data) != marker+"\n" {
		return errors.New("account recovery marker mismatch")
	}
	for _, state := range []string{stateTodo, stateInProgress, stateBlocked, stateDone} {
		entries, err := os.ReadDir(filepath.Join(layout.Repo, tasksRoot, state))
		if err != nil || (state != stateDone && len(entries) != 0) ||
			(state == stateDone && (len(entries) != 1 || entries[0].Name() != accountLiveTaskID || !entries[0].IsDir())) {
			return errors.New("account recovery queue did not finish exactly its assigned task")
		}
	}
	dir := filepath.Join(layout.Repo, tasksRoot, stateDone, accountLiveTaskID)
	data, err = accountLiveRead(layout, filepath.Join(dir, "task.md"))
	if err != nil || string(data) != strings.ReplaceAll(accountLiveTaskBody(marker), "- [ ]", "- [x]") {
		return errors.New("account recovery task contract changed or checklist unfinished")
	}
	data, err = accountLiveRead(layout, filepath.Join(dir, "state.md"))
	if err != nil || !strings.Contains(string(data), "**Status:** complete") || !strings.Contains(string(data), "**Next action:** none") {
		return errors.New("account recovery resume snapshot incomplete")
	}
	data, err = accountLiveRead(layout, filepath.Join(dir, "log.md"))
	if err != nil || len(strings.TrimSpace(string(data))) <= len("# Log — Account recovery") {
		return errors.New("account recovery log was not updated")
	}
	// These are semantic checks through a trusted Git view; no repository-controlled hooks,
	// filters or config are executed on the host. Hidden controller telemetry is not agent output.
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "--porcelain", "--untracked-files=all"}, ""},
		{[]string{"rev-list", "--count", baseline + "..HEAD"}, "1"},
		{[]string{"rev-parse", "HEAD^"}, baseline},
		{[]string{"diff", "--name-status", baseline, "HEAD"}, "A\t" + accountLiveFile},
		{[]string{"show", "HEAD:" + accountLiveFile}, marker},
		{[]string{"log", "-1", "--format=%B"}, "test: account recovery\n\nCoop-Task: " + accountLiveTaskID},
	} {
		got, err := gitOutErr(layout.Repo, check.args...)
		if err != nil || got != check.want {
			return errors.New("account recovery committed change mismatch")
		}
	}
	return nil
}

func verifyAccountLiveTelemetry(data []byte, run string, target agents.Target) error {
	if len(target.Accounts) != 2 || len(data) > 64<<10 {
		return errors.New("invalid account recovery telemetry scope")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var records []StageRecord
	for {
		var record StageRecord
		if err := d.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil || len(records) == 2 {
			return errors.New("malformed or extra account recovery telemetry")
		}
		records = append(records, record)
	}
	if len(records) != 2 {
		return errors.New("account recovery needs exactly two work outcomes")
	}
	for i, record := range records {
		if record.Run != run || record.Stage != "work" || record.Provider != target.Provider ||
			record.Account != target.Accounts[i] || record.Retries != 0 || len(record.GateFiles) != 0 {
			return errors.New("account recovery telemetry target mismatch")
		}
	}
	first, second := records[0], records[1]
	if first.Outcome != "authentication" || len(first.Finished) != 0 || first.HeadBefore == "" || first.HeadBefore != first.HeadAfter ||
		second.Outcome != "success" || second.Exit != 0 || second.HeadBefore != first.HeadAfter || second.HeadAfter == second.HeadBefore ||
		!slices.Equal(second.Finished, []string{accountLiveTaskID}) {
		return errors.New("account recovery did not prove rejection then successful completion")
	}
	return nil
}

func TestProviderAccountsLiveContractTelemetry(t *testing.T) {
	target := agents.Target{Provider: "codex", Accounts: []string{"first", "second"}}
	valid := []StageRecord{
		{Run: "run", Stage: "work", Provider: "codex", Account: "first", Outcome: "authentication", HeadBefore: "base", HeadAfter: "base"},
		{Run: "run", Stage: "work", Provider: "codex", Account: "second", Outcome: "success", HeadBefore: "base", HeadAfter: "changed", Finished: []string{accountLiveTaskID}},
	}
	for _, scenario := range []string{"valid", "missing", "extra", "same account", "rate limit", "wrong task", "gate change", "no commit", "malformed", "unknown field"} {
		t.Run(scenario, func(t *testing.T) {
			records := slices.Clone(valid)
			switch scenario {
			case "missing":
				records = records[:1]
			case "extra":
				records = append(records, valid[1])
			case "same account":
				records[1].Account = "first"
			case "rate limit":
				records[0].Outcome = "rate_limit"
			case "wrong task":
				records[1].Finished = []string{"other"}
			case "gate change":
				records[1].GateFiles = []string{"AGENTS.md"}
			case "no commit":
				records[1].HeadAfter = "base"
			}
			var b bytes.Buffer
			for _, record := range records {
				_ = json.NewEncoder(&b).Encode(record)
			}
			if scenario == "malformed" {
				b.WriteString("invalid\n")
			}
			data := b.Bytes()
			if scenario == "unknown field" {
				data = bytes.Replace(data, []byte(`"run":"run"`), []byte(`"run":"run","unexpected":true`), 1)
			}
			if err := verifyAccountLiveTelemetry(data, "run", target); (err == nil) != (scenario == "valid") {
				t.Fatalf("telemetry accepted=%v: %v", err == nil, err)
			}
		})
	}
}

func TestProviderAccountsLiveContractLaunchCap(t *testing.T) {
	// A nonnil task tool server is part of the paid-work scope; a tiny no-op fixture suffices here.
	spec := box.RunSpec{Agent: "codex", AgentCommand: true, AssignedTask: accountLiveTaskID,
		TaskTools: accountLiveNoopTaskTools{}, Ctx: context.Background()}
	guard := accountLiveLaunchGuard{provider: "codex", accounts: []string{"first", "second"}}
	if guard.admit(spec, "second") == nil || guard.calls != 0 {
		t.Fatal("out-of-order launch admitted")
	}
	if err := guard.admit(spec, "first"); err != nil {
		t.Fatal(err)
	}
	if guard.admit(spec, "first") == nil || guard.calls != 1 {
		t.Fatal("retrying first account admitted")
	}
	if err := guard.admit(spec, "second"); err != nil {
		t.Fatal(err)
	}
	if guard.admit(spec, "second") == nil || guard.calls != 2 {
		t.Fatal("third paid launch admitted")
	}
	for _, mutate := range []func(*box.RunSpec){
		func(s *box.RunSpec) { s.AssignedTask = "other" }, func(s *box.RunSpec) { s.AgentCommand = false },
		func(s *box.RunSpec) { s.TaskTools = nil }, func(s *box.RunSpec) { s.Ctx = nil },
	} {
		bad := spec
		mutate(&bad)
		g := accountLiveLaunchGuard{provider: "codex", accounts: []string{"first", "second"}}
		if g.admit(bad, "first") == nil {
			t.Fatal("wrong work scope admitted")
		}
	}
}

type accountLiveNoopTaskTools struct{}

func (accountLiveNoopTaskTools) Serve(context.Context, io.ReadWriter) error { return nil }

func TestProviderAccountsLiveContractController(t *testing.T) {
	layout, err := procharness.NewLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", layout.GitConfig)
	t.Setenv("GIT_CONFIG_SYSTEM", layout.GitConfig)
	t.Setenv("XDG_STATE_HOME", layout.XDGState)
	t.Setenv(tasks.TestLeaseAuthorityRootEnv, filepath.Join(layout.State, "leases"))
	t.Setenv("TERM", "dumb")
	const marker = "SYNTHETIC_ACCOUNT_PROOF"
	baseline, err := prepareAccountLiveRepository(layout, marker)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ConfigDir: layout.Config, RepoOverride: layout.Repo, Homes: true}
	c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
	guard := accountLiveLaunchGuard{provider: "claude", accounts: []string{"first", "second"}}
	c.boxRun = func(spec box.RunSpec) (int, error) {
		if err := guard.admit(spec, cfg.ActiveProfile("claude")); err != nil {
			t.Fatal(err)
		}
		if guard.calls == 1 {
			_, _ = io.WriteString(spec.Stderr, "Not logged in · Please run /login\n")
			_, _ = io.WriteString(spec.Stdout, "{\"type\":\"result\",\"subtype\":\"error\",\"is_error\":true,\"result\":\"not logged in\"}\n")
			return 1, nil
		}
		writeTaskFile(t, filepath.Join(layout.Repo, accountLiveFile), marker+"\n")
		for _, args := range [][]string{{"add", accountLiveFile}, {"diff", "--check"}, {"commit", "-qm", "test: account recovery\n\nCoop-Task: " + accountLiveTaskID}} {
			if err := accountLiveFixtureGit(layout, args...); err != nil {
				t.Fatal(err)
			}
		}
		dir := filepath.Join(layout.Repo, tasksRoot, stateInProgress, accountLiveTaskID)
		writeTaskFile(t, filepath.Join(dir, "task.md"), strings.ReplaceAll(accountLiveTaskBody(marker), "- [ ]", "- [x]"))
		writeTaskFile(t, filepath.Join(dir, "state.md"), "# State\n\n**Status:** in progress\n**Done so far:** verified\n**Next action:** complete\n**Traps:** none\n")
		writeTaskFile(t, filepath.Join(dir, "log.md"), "# Log — Account recovery\n\nVerified the marker and committed it.\n")
		client, server := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- spec.TaskTools.Serve(ctx, server) }()
		defer func() { cancel(); client.Close(); server.Close(); <-done }()
		if err := client.SetDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(client)
		for _, request := range []map[string]any{
			{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "0"}}},
			{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "tasks_complete", "arguments": map[string]any{"id": accountLiveTaskID}}},
		} {
			if err := json.NewEncoder(client).Encode(request); err != nil {
				t.Fatal(err)
			}
			line, err := reader.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Result struct {
					IsError bool `json:"isError"`
				} `json:"result"`
				Error json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &reply) != nil || reply.Result.IsError || len(reply.Error) != 0 {
				t.Fatalf("task MCP refused synthetic completion: %s", line)
			}
		}
		_, err := io.WriteString(spec.Stdout, "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"result\":\"done\"}\n")
		return 0, err
	}
	target := agents.Target{Provider: "claude", Accounts: guard.accounts}
	code, runErr := c.Run(RunSpec{Repo: layout.Repo, Image: "fixture", Agent: target.Provider, Queues: []string{tasksRoot}, MaxTasks: 1, Sink: io.Discard,
		Rotation: ladder.NewRotation([]agents.Target{{Provider: target.Provider, Accounts: target.Accounts[:1]}, {Provider: target.Provider, Accounts: target.Accounts[1:]}})})
	if code != 0 || runErr != nil || guard.calls != 2 {
		t.Fatalf("controller: code=%d calls=%d error=%v", code, guard.calls, runErr)
	}
	data, err := readRunFile(layout.Repo, c.runID+".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAccountLiveTelemetry(data, c.runID, target); err != nil {
		t.Fatalf("telemetry: %v\n%s", err, data)
	}
	if err := verifyAccountLiveRepository(layout, baseline, marker); err != nil {
		t.Fatal(err)
	}
	// Controls reject a plausible but uncommitted or differently graded result after success.
	writeTaskFile(t, filepath.Join(layout.Repo, accountLiveFile), "wrong\n")
	if verifyAccountLiveRepository(layout, baseline, marker) == nil {
		t.Fatal("wrong marker accepted")
	}
	writeTaskFile(t, filepath.Join(layout.Repo, accountLiveFile), marker+"\n")
	writeTaskFile(t, filepath.Join(layout.Repo, "extra.txt"), "unexpected\n")
	if verifyAccountLiveRepository(layout, baseline, marker) == nil {
		t.Fatal("uncommitted extra output accepted")
	}
}
