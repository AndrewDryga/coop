package acpproxy_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// An editor that connects while another box runs in a project with services hears why it waits, in a
// thread it can open at once: an unanswered initialize shows only a spinner, for as long as that box
// runs. Nothing starts the project's services until the lock is free, and then the session moves onto
// its own box by itself, and says so. This is the whole path on the real binary: an open session, since
// a filtered one needs a real Docker daemon, whose wait is the same code from the supervisor up.
func TestScriptedACPConnectsBesideABoxThatHoldsTheServiceLaunchLock(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tmp := scriptedTempDir(t)
	coopBin, fixtureBin := filepath.Join(tmp, "coop"), filepath.Join(tmp, "acpfixture")
	buildTestBinary(t, root, coopBin, ".")
	buildTestBinary(t, root, fixtureBin, "./internal/acpproxy/testdata/acpfixture")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "compose.yml"), []byte("services:\n  db:\n    image: postgres:18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The supervisor and this test read the same lock and the same records.
	t.Setenv(forkspace.PrivateStateRootEnv, filepath.Join(tmp, "home", ".local", "state", "coop"))
	t.Setenv(forkspace.TestExecutionRegistryRootEnv, filepath.Join(tmp, "home", ".local", "state", "coop", "executions", "v1"))
	running, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{Kind: forkspace.ExecutionLocalLoop, Workspace: repo, SourceID: "a loop run"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forkspace.EndExecution(repo, running) })
	release, err := forkspace.LockServiceLaunch(context.Background(), repo, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	forkspace.NoteServiceLaunchHeld(repo, running.ID)

	moved := matrixGeneration("codex", "real", repo, "unused")[:2]
	moved[1].Events = []any{map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": "real", "update": map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": []any{}},
	}}}
	plan := writeMatrixPlan(t, tmp, matrixPlan{Providers: map[string][][]matrixStep{
		"codex": {matrixGeneration("codex", "held", repo, "unused")[:2], moved},
	}})
	proc := startScriptedACPEnv(t, coopBin, fixtureBin, repo, tmp, plan, "codex", map[string]string{
		"COOP_EGRESS": "open", "COOP_NETWORK": "1", "COOP_AUTO_UP": "1", "COOP_HOMES": "0",
	}, "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mark := proc.client.mark()
	if _, err := proc.client.req(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		t.Fatalf("initialize, with a box holding the lock: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	if _, err := proc.client.req(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}}); err != nil {
		t.Fatalf("session/new, with a box holding the lock: %v\nstderr:\n%s", err, proc.stderr.String())
	}
	awaitScriptedEventContains(t, ctx, proc, mark, "Waiting for another box in this project")
	awaitScriptedEventContains(t, ctx, proc, mark, "Running now: a loop.")

	state := filepath.Join(tmp, "fixture-state")
	calls := fixtureRuntimeCalls(t, state)
	// Reading the file for the editor's links is fine; bringing anything up is not.
	if slices.ContainsFunc(calls, func(call []string) bool { return call[0] == "compose" && slices.Contains(call, "up") }) {
		t.Fatalf("services started with the lock held:\n%s", formatCalls(calls))
	}
	first := slices.IndexFunc(calls, func(call []string) bool { return call[0] == "run" })
	if first < 0 || !slices.Contains(calls[first], repo+":"+repo+":ro") {
		t.Fatalf("the box that holds the session sees the project writable:\n%s", formatCalls(calls))
	}

	mark = proc.client.mark()
	if err := forkspace.EndExecution(repo, running); err != nil {
		t.Fatal(err)
	}
	release()
	awaitScriptedEventContains(t, ctx, proc, mark, "moved onto it")
	calls = fixtureRuntimeCalls(t, state)
	last := -1
	for i, call := range calls {
		if call[0] == "run" {
			last = i
		}
	}
	if last == first || slices.Contains(calls[last], repo+":"+repo+":ro") || strings.Contains(strings.Join(calls[last], " "), "HANDSHAKE") {
		t.Fatalf("the session did not move onto its own box:\n%s", formatCalls(calls))
	}
	proc.stop(t)
}
