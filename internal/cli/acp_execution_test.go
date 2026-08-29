package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	containerruntime "github.com/AndrewDryga/coop/internal/runtime"
)

func TestReapACPChildBoxesUsesExactExecutionAuthority(t *testing.T) {
	repo := t.TempDir()
	t.Setenv(forkspace.TestExecutionRegistryRootEnv, t.TempDir())
	trace := filepath.Join(t.TempDir(), "runtime.log")
	runtimeCLI := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(runtimeCLI, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COOP_TEST_RUNTIME_TRACE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_RUNTIME_TRACE", trace)

	owned, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionACP, Role: forkspace.ExecutionRoleActive,
		Workspace: repo, SourceID: "owned-supervisor",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forkspace.EndExecution(repo, owned)
	unrelated, err := forkspace.BeginExecution(repo, forkspace.ExecutionSpec{
		Kind: forkspace.ExecutionACP, Role: forkspace.ExecutionRoleActive,
		Workspace: repo, SourceID: "replacement-supervisor",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forkspace.EndExecution(repo, unrelated)

	a := &app{rt: containerruntime.Runtime{Name: runtimeCLI}, rtSet: true}
	if !a.reapACPChildBoxes(repo, owned.SourceID, owned.PID) {
		t.Fatal("exact ACP child cleanup was not proven")
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "label=coop.execution="+owned.ID) {
		t.Fatalf("runtime cleanup did not use owned execution label:\n%s", got)
	}
	if strings.Contains(got, unrelated.ID) {
		t.Fatalf("runtime cleanup selected replacement execution %s:\n%s", unrelated.ID, got)
	}
}
