package forkspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionRegistryTracksAllBoundIdentityAndExactCleanup(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	workspace := Workspace(repo, "perf")
	record, err := BeginExecution(repo, ExecutionSpec{
		Kind: ExecutionForkLoop, Role: ExecutionRoleDetachedWorker, Workspace: workspace, Fork: &identity,
		Task:     &ExecutionTaskRef{QueueID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), ID: "task", Assignment: strings.Repeat("c", 32)},
		SourceID: "run-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertForkStatePerm(t, StateDir(repo), 0o700)
	assertForkStatePerm(t, executionDir(repo), 0o700)
	assertForkStatePerm(t, filepath.Join(executionDir(repo), record.ID+".json"), 0o600)
	got, problems := Executions(repo)
	if len(problems) != 0 || len(got) != 1 || !got[0].Running || !got[0].Active || got[0].Record.Fork == nil || *got[0].Record.Fork != identity {
		t.Fatalf("executions = %#v, problems=%v", got, problems)
	}
	wrong := record
	wrong.Token = "linux-proc-v1:wrong:1"
	if err := EndExecution(repo, wrong); err == nil {
		t.Fatal("wrong process identity removed an execution record")
	}
	if err := EndExecution(repo, record); err != nil {
		t.Fatal(err)
	}
	if got, problems := Executions(repo); len(got) != 0 || len(problems) != 0 {
		t.Fatalf("after cleanup = %#v, problems=%v", got, problems)
	}
}

func TestForkExecutionReservationSerializesMutationAndWarmDoesNotCountActive(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	makeGenerationWorkspace(t, repo, "perf")
	identity := ensureTestGeneration(t, repo, "perf")
	unlock, err := LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	var record ExecutionRecord
	go func() {
		var beginErr error
		record, beginErr = BeginExecution(repo, ExecutionSpec{
			Kind: ExecutionForkACP, Role: ExecutionRoleWarm,
			Workspace: Workspace(repo, identity.Name), Fork: &identity, SourceID: "warm-1",
		})
		result <- beginErr
	}()
	select {
	case err := <-result:
		t.Fatalf("activity publication ignored lifecycle lock: %v", err)
	default:
	}
	unlock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = EndExecution(repo, record) })
	observations, problems := Executions(repo)
	if len(problems) != 0 || len(observations) != 1 || observations[0].Active || !observations[0].Running {
		t.Fatalf("warm execution = %+v, problems=%v", observations, problems)
	}
	unlock, err = LockState(repo, identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	guardErr := RequireNoForkExecutionsLocked(repo, identity)
	unlock()
	if guardErr == nil || !strings.Contains(guardErr.Error(), "warm activity") {
		t.Fatalf("mutation guard = %v", guardErr)
	}
	if err := UpdateExecutionRoleBySource(repo, "warm-1", ExecutionRoleActive); err != nil {
		t.Fatal(err)
	}
	observations, _ = Executions(repo)
	if len(observations) != 1 || !observations[0].Active || observations[0].Record.Role != ExecutionRoleActive {
		t.Fatalf("activated execution = %+v", observations)
	}
	if err := EndExecution(repo, record); err != nil {
		t.Fatalf("original process authority could not clean a role-updated record: %v", err)
	}
	record = ExecutionRecord{}
	if observations, problems = Executions(repo); len(observations) != 0 || len(problems) != 0 {
		t.Fatalf("role-updated execution remained after exact cleanup: %+v, problems=%v", observations, problems)
	}
}

func TestExecutionRegistryReportsCorruptSiblingWithoutHidingHealthy(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := BeginExecution(repo, ExecutionSpec{Kind: ExecutionInteractive, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = EndExecution(repo, record) })
	if err := os.WriteFile(filepath.Join(executionDir(repo), strings.Repeat("d", 32)+".json"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, problems := Executions(repo)
	if len(got) != 1 || len(problems) != 1 {
		t.Fatalf("executions = %#v, problems=%v", got, problems)
	}
}

func TestExecutionRegistryFallsBackToUserStateWhenProjectParentIsReadOnly(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	workspace := filepath.Join(repo, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv(TestExecutionRegistryRootEnv, root)
	previous := ensureProjectExecutionDir
	ensureProjectExecutionDir = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { ensureProjectExecutionDir = previous })

	record, err := BeginExecution(repo, ExecutionSpec{
		Kind: ExecutionInteractive, Role: ExecutionRoleActive, Workspace: workspace, SourceID: "readonly-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.registry == "" || !strings.HasPrefix(record.registry, root+string(filepath.Separator)) {
		t.Fatalf("fallback registry = %q, want beneath %s", record.registry, root)
	}
	if _, err := os.Lstat(filepath.Join(record.registry, record.ID+".json")); err != nil {
		t.Fatalf("fallback record: %v", err)
	}
	observations, problems := Executions(repo)
	if len(problems) != 0 || len(observations) != 1 || observations[0].Record.registry != record.registry {
		t.Fatalf("fallback observations = %#v, problems=%v", observations, problems)
	}
	if err := UpdateExecutionRoleBySource(repo, record.SourceID, ExecutionRoleWarm); err != nil {
		t.Fatal(err)
	}
	if err := EndExecution(repo, record); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(record.registry, record.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback record after cleanup: %v", err)
	}
}
