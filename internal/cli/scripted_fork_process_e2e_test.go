//go:build providere2e

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/testutil/procharness"
)

func TestProviderScriptedForkSessionProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)

	t.Run("fresh resume new and remembered provider", func(t *testing.T) {
		resetForkProcessRepo(t, suite)
		name, provider, account := "session-life", "claude", "personal"
		ws := forkWorkspace(suite.layout.Repo, name)
		model, effort := "fork-model-claude", "high"
		target := forkProcessTarget(provider, model, effort, account)

		result, trace := runForkProcess(t, suite, []string{name, target}, provider)
		id := readForkSession(ws, provider, account)
		assertForkProcessSuccess(t, result, provider)
		assertForkProcessContract(t, suite, trace, ws, provider, account, forkStartArgv(provider, model, effort, id), model, effort)
		if id == "" {
			t.Fatal("fresh fork did not persist its explicit session id")
		}
		writeForkProviderSession(t, suite, provider, account, ws, id, "cli", time.Now())
		persistent := filepath.Join(ws, "uncommitted-note.txt")
		if err := os.WriteFile(persistent, []byte("keep across re-entry\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		// With no target, the fork remembers Claude and the marked-default personal account.
		result, trace = runForkProcess(t, suite, []string{name}, provider)
		assertForkProcessSuccess(t, result, provider)
		assertForkProcessContract(t, suite, trace, ws, provider, account, forkResumeArgv(provider, "env-claude", "medium", id), "env-claude", "medium")
		if data, err := os.ReadFile(persistent); err != nil || string(data) != "keep across re-entry\n" {
			t.Fatalf("fork work did not persist across re-entry: %q, %v", data, err)
		}

		result, trace = runForkProcess(t, suite, []string{name, "--new"}, provider)
		newID := readForkSession(ws, provider, account)
		assertForkProcessSuccess(t, result, provider)
		assertForkProcessContract(t, suite, trace, ws, provider, account, forkStartArgv(provider, "env-claude", "medium", newID), "env-claude", "medium")
		if newID == "" || newID == id {
			t.Fatalf("--new id = %q, want a new value distinct from %q", newID, id)
		}

		// --fresh still remembers the provider before destroying the old workspace.
		result, trace = runForkProcess(t, suite, []string{name, "--fresh", "--force", "--yes"}, provider)
		freshID := readForkSession(ws, provider, account)
		assertForkProcessSuccess(t, result, provider)
		assertForkProcessContract(t, suite, trace, ws, provider, account, forkStartArgv(provider, "env-claude", "medium", freshID), "env-claude", "medium")
		if freshID == "" || freshID == newID {
			t.Fatalf("--fresh id = %q, prior %q", freshID, newID)
		}

		result, trace = suite.run(t, []string{"fork", "providerless"}, processScenario("claude", nil, 0, ""))
		if result.ExitCode != 2 || result.Err != nil || len(trace) != 0 || pathExists(forkWorkspace(suite.layout.Repo, "providerless")) {
			t.Fatalf("providerless fork = exit %d err %v trace %d exists %v\n%s", result.ExitCode, result.Err, len(trace), pathExists(forkWorkspace(suite.layout.Repo, "providerless")), result.Stderr)
		}
	})

	t.Run("provider account cwd and id isolation", func(t *testing.T) {
		resetForkProcessRepo(t, suite)
		name, account := "switchboard", "work"
		ws := forkWorkspace(suite.layout.Repo, name)
		ids := map[string]string{}
		for _, provider := range suite.providers {
			model := "fork-model-" + provider
			effort := directTargetEffort(directProviderContracts[provider])
			target := forkProcessTarget(provider, model, effort, account)
			result, trace := runForkProcess(t, suite, []string{name, target}, provider)
			assertForkProcessSuccess(t, result, provider)
			id := readForkSession(ws, provider, account)
			if provider == "codex" {
				id = "11111111-2222-4333-8444-000000000001"
			}
			ids[provider] = id
			assertForkProcessContract(t, suite, trace, ws, provider, account, forkStartArgv(provider, model, effort, id), model, effort)

			// Wrong-cwd/same-id history alone must not resume. Providers with explicit IDs also
			// reject a correct-cwd/different-id history before the exact match is installed.
			wrongCwd := filepath.Join(filepath.Dir(ws), "wrong-cwd")
			writeForkProviderSession(t, suite, provider, account, wrongCwd, id, "cli", time.Now())
			if provider != "codex" {
				writeForkProviderSession(t, suite, provider, account, ws, "99999999-2222-4333-8444-000000000009", "cli", time.Now())
			}
			result, trace = runForkProcess(t, suite, []string{name, target}, provider)
			assertForkProcessSuccess(t, result, provider)
			assertForkProcessContract(t, suite, trace, ws, provider, account, forkStartArgv(provider, model, effort, id), model, effort)

			writeForkProviderSession(t, suite, provider, account, ws, id, "cli", time.Now().Add(-3*time.Minute))
			if provider == "codex" {
				writeForkProviderSession(t, suite, provider, account, ws, "11111111-2222-4333-8444-000000000002", "exec", time.Now().Add(time.Minute))
				writeForkProviderSession(t, suite, provider, "personal", ws, "11111111-2222-4333-8444-000000000003", "cli", time.Now().Add(2*time.Minute))
			}

			result, trace = runForkProcess(t, suite, []string{name, target}, provider)
			assertForkProcessSuccess(t, result, provider)
			assertForkProcessContract(t, suite, trace, ws, provider, account, forkResumeArgv(provider, model, effort, id), model, effort)

			// Every adapter gets a process-level --new proof. Codex writes its provider-minted ID
			// during the child run; the preset-ID providers receive a new Coop-owned UUID in argv.
			newScenario := processScenario(provider, nil, 0, "")
			if provider == "codex" {
				newScenario["native_session"] = map[string]string{
					"account": account, "cwd": ws, "id": "11111111-2222-4333-8444-000000000004",
				}
			}
			result, trace = runForkProcessScenario(t, suite, []string{name, target, "--new"}, newScenario)
			newID := readForkSession(ws, provider, account)
			assertForkProcessSuccess(t, result, provider)
			assertForkProcessContract(t, suite, trace, ws, provider, account, forkStartArgv(provider, model, effort, newID), model, effort)
			if newID == "" || newID == id {
				t.Fatalf("%s --new id = %q, want a new value distinct from %q", provider, newID, id)
			}
			if provider != "codex" {
				writeForkProviderSession(t, suite, provider, account, ws, newID, "cli", time.Now())
			}
			ids[provider] = newID
		}

		var explicitIDs []string
		for _, provider := range []string{"claude", "gemini", "grok"} {
			if ids[provider] == "" || slices.Contains(explicitIDs, ids[provider]) {
				t.Fatalf("provider %s reused or omitted explicit id %q across providers: %v", provider, ids[provider], explicitIDs)
			}
			explicitIDs = append(explicitIDs, ids[provider])
		}

		// The same provider in another account gets another explicit id even if that account's
		// history contains the work account's id; switching back resumes each scoped conversation.
		provider := "claude"
		model, effort := "fork-model-claude", "high"
		writeForkProviderSession(t, suite, provider, "personal", ws, ids[provider], "cli", time.Now())
		personalTarget := forkProcessTarget(provider, model, effort, "personal")
		result, trace := runForkProcess(t, suite, []string{name, personalTarget}, provider)
		personalID := readForkSession(ws, provider, "personal")
		assertForkProcessSuccess(t, result, provider)
		assertForkProcessContract(t, suite, trace, ws, provider, "personal", forkStartArgv(provider, model, effort, personalID), model, effort)
		if personalID == "" || personalID == ids[provider] {
			t.Fatalf("personal id %q reused work id %q", personalID, ids[provider])
		}
		writeForkProviderSession(t, suite, provider, "personal", ws, personalID, "cli", time.Now())

		for _, row := range []struct {
			account, id string
		}{{"work", ids[provider]}, {"personal", personalID}} {
			target := forkProcessTarget(provider, model, effort, row.account)
			result, trace = runForkProcess(t, suite, []string{name, target}, provider)
			assertForkProcessSuccess(t, result, provider)
			assertForkProcessContract(t, suite, trace, ws, provider, row.account, forkResumeArgv(provider, model, effort, row.id), model, effort)
		}
	})

	t.Run("shared container workdir keeps codex forks distinct", func(t *testing.T) {
		resetForkProcessRepo(t, suite)
		override := *suite
		override.env = replaceProcessEnv(suite.env, "COOP_WORKDIR", "/workspace/fork")
		provider, account := "codex", "work"
		model, effort := "fork-model-codex", "high"
		target := forkProcessTarget(provider, model, effort, account)
		oldID := "11111111-2222-4333-8444-000000000000"
		writeForkProviderSession(t, &override, provider, account, "/workspace/fork", oldID, "cli", time.Now().Add(-time.Hour))
		noSessionName := "workdir-no-session"
		result, trace := runForkProcess(t, &override, []string{noSessionName, target}, provider)
		assertForkProcessSuccess(t, result, provider)
		noSessionWS := forkWorkspace(suite.layout.Repo, noSessionName)
		assertForkProcessContractWorkdir(t, &override, trace, noSessionWS, "/workspace/fork", provider, account, forkStartArgv(provider, model, effort, ""), model, effort)
		if got := readForkSession(noSessionWS, provider, account); got != "" {
			t.Fatalf("fresh run with no new native session captured old id %q", got)
		}
		staleID := "11111111-2222-4333-8444-000000000099"
		replacementID := "11111111-2222-4333-8444-000000000003"
		saveForkSession(noSessionWS, provider, account, staleID)
		replacementScenario := processScenario(provider, nil, 0, "")
		replacementScenario["native_session"] = map[string]string{"account": account, "cwd": "/workspace/fork", "id": replacementID}
		result, trace = runForkProcessScenario(t, &override, []string{noSessionName, target}, replacementScenario)
		assertForkProcessSuccess(t, result, provider)
		assertForkProcessContractWorkdir(t, &override, trace, noSessionWS, "/workspace/fork", provider, account, forkStartArgv(provider, model, effort, ""), model, effort)
		if got := readForkSession(noSessionWS, provider, account); got != replacementID {
			t.Fatalf("stale Codex hint was replaced with %q, want %q", got, replacementID)
		}
		rows := []struct {
			name, id string
		}{
			{"workdir-a", "11111111-2222-4333-8444-000000000001"},
			{"workdir-b", "11111111-2222-4333-8444-000000000002"},
		}
		for _, row := range rows {
			ws := forkWorkspace(suite.layout.Repo, row.name)
			scenario := processScenario(provider, nil, 0, "")
			scenario["native_session"] = map[string]string{"account": account, "cwd": "/workspace/fork", "id": row.id}
			result, trace := runForkProcessScenario(t, &override, []string{row.name, target}, scenario)
			assertForkProcessSuccess(t, result, provider)
			assertForkProcessContractWorkdir(t, &override, trace, ws, "/workspace/fork", provider, account, forkStartArgv(provider, model, effort, row.id), model, effort)
			if got := readForkSession(ws, provider, account); got != row.id {
				t.Fatalf("fork %s discovered id %q, want %q", row.name, got, row.id)
			}
		}
		for _, row := range rows {
			ws := forkWorkspace(suite.layout.Repo, row.name)
			result, trace := runForkProcess(t, &override, []string{row.name, target}, provider)
			assertForkProcessSuccess(t, result, provider)
			assertForkProcessContractWorkdir(t, &override, trace, ws, "/workspace/fork", provider, account, forkResumeArgv(provider, model, effort, row.id), model, effort)
			if got := readForkSession(ws, provider, account); got != row.id {
				t.Fatalf("fork %s resume changed exact hint to %q, want %q", row.name, got, row.id)
			}
		}
		// A Codex --new run that exits before creating history must still abandon the old hint;
		// the next entry starts fresh instead of silently returning to the discarded conversation.
		emptyNew := rows[0]
		emptyWS := forkWorkspace(suite.layout.Repo, emptyNew.name)
		result, trace = runForkProcess(t, &override, []string{emptyNew.name, target, "--new"}, provider)
		assertForkProcessSuccess(t, result, provider)
		assertForkProcessContractWorkdir(t, &override, trace, emptyWS, "/workspace/fork", provider, account, forkStartArgv(provider, model, effort, ""), model, effort)
		if got := readForkSession(emptyWS, provider, account); got != "" {
			t.Fatalf("sessionless Codex --new retained old hint %q", got)
		}
	})

	t.Run("overlapping codex discovery is serialized", func(t *testing.T) {
		resetForkProcessRepo(t, suite)
		override := *suite
		override.env = replaceProcessEnv(suite.env, "COOP_WORKDIR", "/workspace/fork")
		provider, account := "codex", "work"
		target := forkProcessTarget(provider, "fork-model-codex", "high", account)

		// The first run deliberately creates no native session and waits inside the provider.
		override.reset(t, processScenario(provider, nil, 0, "wait"))
		first, err := procharness.Start(procharness.Command{
			Path: override.coopBin, Args: []string{"fork", "overlap-a", target},
			Dir: override.layout.Repo, Env: override.env, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer first.Cleanup()
		firstReady := awaitProcessEvent(t, override.layout.Trace, "provider", "ready", 5*time.Second)

		// Put the contender in another parent repository while keeping ConfigDir/account/cwd shared.
		// The lock authority must follow the native history, not either repository's fork state.
		secondRepo := filepath.Join(override.layout.Root, "repo-overlap-b")
		if err := os.MkdirAll(secondRepo, 0o700); err != nil {
			t.Fatal(err)
		}
		gitBin, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		initProcessRepo(t, gitBin, secondRepo, override.env)

		// Give the contender its own immutable scenario. Without config-global serialization it
		// would create this session while A was still running, and A could wrongly claim it.
		secondID := "11111111-2222-4333-8444-000000000010"
		secondScenario := processScenario(provider, nil, 0, "")
		secondScenario["native_session"] = map[string]string{"account": account, "cwd": "/workspace/fork", "id": secondID}
		data, err := json.Marshal(secondScenario)
		if err != nil {
			t.Fatal(err)
		}
		secondScenarioPath := filepath.Join(override.layout.Plans, "fork-overlap-b.json")
		if err := os.WriteFile(secondScenarioPath, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		secondEnv := replaceProcessEnv(override.env, "COOP_PROVIDER_FIXTURE_SCENARIO", secondScenarioPath)
		secondEnv = replaceProcessEnv(secondEnv, "COOP_REPO", secondRepo)
		contenderCtx, contenderCancel := context.WithTimeout(context.Background(), 5*time.Second)
		contended := procharness.Run(contenderCtx, procharness.Command{
			Path: override.coopBin, Args: []string{"fork", "overlap-b", target},
			Dir: secondRepo, Env: secondEnv, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
		})
		contenderCancel()
		if contended.Err != nil || contended.ExitCode != 1 || !strings.Contains(contended.Stderr, "another interactive codex session is active") {
			t.Fatalf("cross-repo contender = exit %d err %v\nstderr:\n%s", contended.ExitCode, contended.Err, contended.Stderr)
		}
		// Ordinary interactive Coop launches participate too; otherwise a direct Codex session can
		// become the fork's sole post-run ID even when every fork command uses the global lock.
		directCtx, directCancel := context.WithTimeout(context.Background(), 5*time.Second)
		direct := procharness.Run(directCtx, procharness.Command{
			Path: override.coopBin, Args: []string{target}, Dir: secondRepo, Env: secondEnv,
			MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
		})
		directCancel()
		if direct.Err != nil || direct.ExitCode != 1 || !strings.Contains(direct.Stderr, "another interactive codex session is active") {
			t.Fatalf("direct contender = exit %d err %v\nstderr:\n%s", direct.ExitCode, direct.Err, direct.Stderr)
		}
		fusionEnv := replaceProcessEnv(secondEnv, "COOP_HOMES", "1")
		fusionCtx, fusionCancel := context.WithTimeout(context.Background(), 5*time.Second)
		fusion := procharness.Run(fusionCtx, procharness.Command{
			Path: override.coopBin, Args: []string{"fusion", target, "--peer", "claude"},
			Dir: secondRepo, Env: fusionEnv, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
		})
		fusionCancel()
		if fusion.Err != nil || fusion.ExitCode != 1 || !strings.Contains(fusion.Stderr, "another interactive codex session is active") {
			t.Fatalf("fusion contender = exit %d err %v\nstderr:\n%s", fusion.ExitCode, fusion.Err, fusion.Stderr)
		}

		// Codex exec rollouts are source:"exec" and excluded from discovery, so a headless run
		// remains concurrent instead of inheriting the interactive producer lock.
		execScenarioPath := filepath.Join(override.layout.Plans, "fork-overlap-exec.json")
		execScenario, err := json.Marshal(processScenario(provider, nil, 0, ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(execScenarioPath, append(execScenario, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		execEnv := replaceProcessEnv(secondEnv, "COOP_PROVIDER_FIXTURE_SCENARIO", execScenarioPath)
		execCtx, execCancel := context.WithTimeout(context.Background(), 5*time.Second)
		execResult := procharness.Run(execCtx, procharness.Command{
			Path: override.coopBin, Args: []string{target, "--", "exec", "fixture prompt"},
			Dir: secondRepo, Env: execEnv, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
		})
		execCancel()
		if execResult.Err != nil || execResult.ExitCode != 0 {
			t.Fatalf("concurrent Codex exec = exit %d err %v\nstderr:\n%s", execResult.ExitCode, execResult.Err, execResult.Stderr)
		}
		starts := 0
		for _, event := range readProcessTrace(t, override.layout.Trace) {
			if event.Source == "provider" && event.Event == "start" {
				starts++
			}
		}
		if starts != 2 {
			t.Fatalf("session contenders reached provider before owner released lock: %d starts", starts)
		}

		// Signal the waiting provider directly so teardown proceeds child-to-parent. Group-wide
		// cancellation is covered elsewhere and can make the harness observe the leader first.
		if err := syscall.Kill(firstReady.PID, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		firstCtx, firstCancel := context.WithTimeout(context.Background(), 5*time.Second)
		firstResult := first.Wait(firstCtx)
		firstCancel()
		if firstResult.Err != nil || firstResult.ExitCode == 0 {
			t.Fatalf("canceled first fork = exit %d err %v", firstResult.ExitCode, firstResult.Err)
		}
		secondCtx, secondCancel := context.WithTimeout(context.Background(), 10*time.Second)
		secondResult := procharness.Run(secondCtx, procharness.Command{
			Path: override.coopBin, Args: []string{"fork", "overlap-b", target},
			Dir: secondRepo, Env: secondEnv, MaxOutput: 1 << 20, KillGrace: 500 * time.Millisecond,
		})
		secondCancel()
		if secondResult.Err != nil || secondResult.ExitCode != 0 {
			t.Fatalf("serialized second fork = exit %d err %v\nstdout:\n%s\nstderr:\n%s", secondResult.ExitCode, secondResult.Err, secondResult.Stdout, secondResult.Stderr)
		}
		firstWS := forkWorkspace(override.layout.Repo, "overlap-a")
		secondWS := forkWorkspace(secondRepo, "overlap-b")
		if got := readForkSession(firstWS, provider, account); got != "" {
			t.Fatalf("sessionless first run claimed contender session %q", got)
		}
		if got := readForkSession(secondWS, provider, account); got != secondID {
			t.Fatalf("contender captured %q, want %q", got, secondID)
		}
		trace := readProcessTrace(t, override.layout.Trace)
		var firstExit, secondStart int
		for _, event := range trace {
			if event.Source != "provider" {
				continue
			}
			if event.PID == firstReady.PID && event.Event == "exit" {
				firstExit = event.Sequence
			} else if event.PID != firstReady.PID && event.Event == "start" {
				secondStart = event.Sequence
			}
		}
		if firstExit == 0 || secondStart <= firstExit {
			t.Fatalf("provider ordering did not prove serialization: first exit %d, second start %d", firstExit, secondStart)
		}
	})
}

func TestProviderScriptedForkLoopMergeProcess(t *testing.T) {
	suite := newDirectProcessSuite(t)
	resetForkProcessRepo(t, suite)
	name, provider, taskID := "merge-flow", "codex", "fork-merge-task"
	seedLoopProcessTask(t, suite.layout.Repo, taskID)
	model, effort, account := "fork-loop-model", "high", "work"
	target := forkProcessTarget(provider, model, effort, account)
	writeLoopReviewConfig(t, suite.layout.Repo, nil, []string{target}, nil, 3)
	loopProcessGit(t, suite, "add", ".agent/loop.yaml")
	loopProcessGit(t, suite, "commit", "-qm", "fixture loop review config")
	attempts := []loopProcessAttempt{
		{Target: target, Stage: "work", Result: "complete"},
		{Target: target, Stage: "signoff", Result: "pass"},
	}
	scenario := loopProcessScenario{
		Version: 6, Provider: provider, ProviderHomes: agents.Names(),
		Loop: loopProcessPlan{TaskID: taskID, Attempts: attempts},
	}
	result, trace := suite.run(t, []string{"fork", name, target, "--loop", "--tasks", filepath.Join(suite.layout.Repo, tasksRoot)}, scenario)
	ws := forkWorkspace(suite.layout.Repo, name)
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "fixture-loop-complete-"+provider) {
		t.Fatalf("fork loop = exit %d err %v\nstdout:\n%s\nstderr:\n%s\ntrace:\n%s", result.ExitCode, result.Err, result.Stdout, result.Stderr, readProcessFile(t, suite.layout.Trace))
	}
	if !pathExists(filepath.Join(ws, tasksRoot, stateDone, taskID)) || !pathExists(filepath.Join(ws, "loop-codex.txt")) {
		t.Fatal("foreground fork loop did not retain its completed queue and committed work")
	}
	if !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateTodo, taskID)) {
		t.Fatal("fork loop changed the parent queue before merge")
	}
	assertForkProcessContract(t, suite, firstForkRunTrace(trace), ws, provider, account,
		loopProcessArgv(provider, model, effort, loopWorkPrompt(ws, []string{tasksRoot}, taskID, provider, nil, nil)), model, effort)

	result, trace = suite.run(t, []string{"fork", "merge", name}, processScenario(provider, nil, 0, ""))
	if result.Err != nil || result.ExitCode != 1 || len(trace) != 0 || !pathExists(ws) || !pathExists(filepath.Join(suite.layout.Repo, tasksRoot, stateTodo, taskID)) {
		t.Fatalf("unconfirmed merge = exit %d err %v trace %d fork %v\n%s", result.ExitCode, result.Err, len(trace), pathExists(ws), result.Stderr)
	}

	result, trace = suite.run(t, []string{"fork", "merge", name, "--yes"}, processScenario(provider, nil, 0, ""))
	if result.Err != nil || result.ExitCode != 0 || len(trace) != 0 || pathExists(ws) {
		t.Fatalf("confirmed merge = exit %d err %v trace %d fork %v\nstdout:\n%s\nstderr:\n%s", result.ExitCode, result.Err, len(trace), pathExists(ws), result.Stdout, result.Stderr)
	}
	landed, err := os.ReadFile(filepath.Join(suite.layout.Repo, "loop-codex.txt"))
	if err != nil || string(landed) != "completed by codex\n" {
		t.Fatalf("landed fork work = %q, %v", landed, err)
	}
	done := filepath.Join(suite.layout.Repo, tasksRoot, stateDone, taskID)
	state, stateErr := os.ReadFile(filepath.Join(done, "state.md"))
	log, logErr := os.ReadFile(filepath.Join(done, "log.md"))
	if stateErr != nil || logErr != nil || !strings.Contains(string(state), "**Status:** complete") || !strings.Contains(string(state), "**Next action:** none") || !strings.Contains(string(log), "reconciled: landed by fork "+name) || pathExists(filepath.Join(done, "tmp")) {
		t.Fatalf("parent task was not reconciled: state %q (%v), log %q (%v)", state, stateErr, log, logErr)
	}
}

func firstForkRunTrace(trace []*processTrace) []*processTrace {
	var first []*processTrace
	seenRun := false
	for _, event := range trace {
		first = append(first, event)
		if event.Source == "runtime" && event.Event == "run" {
			seenRun = true
		}
		if seenRun && event.Source == "runtime" && event.Event == "exit" {
			break
		}
	}
	return first
}

func resetForkProcessRepo(t *testing.T, suite *directProcessSuite) {
	t.Helper()
	resetLoopProcessRepo(t, suite)
	if err := os.RemoveAll(forkHome(suite.layout.Repo)); err != nil {
		t.Fatal(err)
	}
}

func runForkProcess(t *testing.T, suite *directProcessSuite, args []string, provider string) (procharness.Result, []*processTrace) {
	t.Helper()
	return runForkProcessScenario(t, suite, args, processScenario(provider, nil, 0, ""))
}

func runForkProcessScenario(t *testing.T, suite *directProcessSuite, args []string, scenario any) (procharness.Result, []*processTrace) {
	t.Helper()
	return suite.run(t, append([]string{"fork"}, args...), scenario)
}

func assertForkProcessSuccess(t *testing.T, result procharness.Result, provider string) {
	t.Helper()
	if result.Err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "fixture-ok-"+provider) {
		t.Fatalf("fork %s = exit %d err %v\nstdout:\n%s\nstderr:\n%s", provider, result.ExitCode, result.Err, result.Stdout, result.Stderr)
	}
}

func assertForkProcessContract(t *testing.T, suite *directProcessSuite, trace []*processTrace, ws, provider, account string, argv []string, model, effort string) {
	assertForkProcessContractWorkdir(t, suite, trace, ws, ws, provider, account, argv, model, effort)
}

func assertForkProcessContractWorkdir(t *testing.T, suite *directProcessSuite, trace []*processTrace, ws, workdir, provider, account string, argv []string, model, effort string) {
	t.Helper()
	wantArgv := processTraceArgv(argv)
	run := oneProcessEvent(t, trace, "runtime", "run")
	start := oneProcessEvent(t, trace, "provider", "start")
	wantRepo := processTracePath(suite.layout.Root, ws)
	wantWorkdir := "<container>" + filepath.ToSlash(filepath.Clean(workdir))
	if rel, err := filepath.Rel(suite.layout.Root, workdir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		wantWorkdir = processTracePath(suite.layout.Root, workdir)
	}
	if run.Run == nil || run.Run.Provider != provider || run.Run.Workdir != wantWorkdir || run.Run.HostWorkdir != wantRepo || !reflect.DeepEqual(run.Run.ProviderArgv, wantArgv) {
		t.Fatalf("fork runtime contract = %#v, want provider %s workdir %s host %s argv %q", run.Run, provider, wantWorkdir, wantRepo, wantArgv)
	}
	if start.Cwd != wantRepo || !reflect.DeepEqual(start.Argv, wantArgv) {
		t.Fatalf("fork provider contract = cwd %q argv %q, want %q %q", start.Cwd, start.Argv, wantRepo, wantArgv)
	}
	assertProcessMountsAtTarget(t, suite.layout, ws, wantWorkdir, provider, account, run.Run.Mounts)
	assertDirectEnvironment(t, run.Run.Environment, suite.allCredKeys, provider, directProviderContracts[provider], model, effort)
	assertDirectEnvironment(t, start.Environment, suite.allCredKeys, provider, directProviderContracts[provider], model, effort)
	for _, event := range trace {
		awaitProcessGone(t, event.PID)
	}
}

func replaceProcessEnv(env []string, key, value string) []string {
	out := append([]string(nil), env...)
	prefix := key + "="
	for i := range out {
		if strings.HasPrefix(out[i], prefix) {
			out[i] = prefix + value
			return out
		}
	}
	return append(out, prefix+value)
}

func forkProcessTarget(provider, model, effort, account string) string {
	return (agents.Target{Provider: provider, Model: model, Effort: effort, Accounts: []string{account}}).String()
}

func forkStartArgv(provider, model, effort, id string) []string {
	base := directExpectedArgv(directProviderContracts[provider], model, effort, nil)
	if provider == "codex" {
		return base
	}
	return append(base, "--session-id", id)
}

func forkResumeArgv(provider, model, effort, id string) []string {
	base := directExpectedArgv(directProviderContracts[provider], model, effort, nil)
	if provider == "codex" {
		return append([]string{base[0], "resume", id}, base[1:]...)
	}
	return append(base, "--resume", id)
}

func writeForkProviderSession(t *testing.T, suite *directProcessSuite, provider, account, ws, id, source string, modTime time.Time) {
	t.Helper()
	profile := filepath.Join(suite.layout.Config, provider, "profiles", account)
	var path, body string
	switch provider {
	case "claude":
		path = filepath.Join(profile, "projects", agents.ClaudeProjectKey(ws), id+".jsonl")
		body = "{}\n"
	case "codex":
		bucket := fmt.Sprintf("%x", sha256.Sum256([]byte(ws)))[:16]
		path = filepath.Join(profile, "sessions", "2026", "07", "16", bucket+"-"+id+".jsonl")
		body = fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q,\"source\":%q}}\n", id, ws, source)
	case "gemini":
		path = filepath.Join(profile, "tmp", fmt.Sprintf("fixture-%x", sha256.Sum256([]byte(ws)))[:24], "chats", id+".jsonl")
		body = fmt.Sprintf("{\"sessionId\":%q,\"projectHash\":\"%x\"}\n", id, sha256.Sum256([]byte(ws)))
	case "grok":
		path = filepath.Join(profile, "sessions", url.PathEscape(ws), id, "summary.json")
		body = "{}\n"
	default:
		t.Fatalf("unsupported fork session provider %q", provider)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}
