package forkctl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

func TestCostCell(t *testing.T) {
	if got := (forkStatus{}).costCell(); got != "—" {
		t.Errorf("a fork with no cost = %q, want —", got)
	}
	if got := (forkStatus{Cost: 12.3}).costCell(); got != "$12.30" {
		t.Errorf("costCell(12.3) = %q, want $12.30", got)
	}
}

func TestForkStatusDoesNotHideCorruptGeneration(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(forkspace.Workspace(repo, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forkspace.GenerationPath(repo, "broken"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := (&Control{}).gatherForkStatus(repo, "broken")
	if status.stateCell() != "unknown" || len(status.Problems) == 0 {
		t.Fatalf("corrupt generation status = %+v, cell=%q", status, status.stateCell())
	}
}

func TestForkStatusKeepsExactCleanupActionVisibleBesideProblems(t *testing.T) {
	status := forkStatus{Cleanup: true, Problems: []string{"workspace is missing"}}
	if got := status.stateCell(); got != "cleanup" {
		t.Fatalf("cleanup-pending damaged fork state = %q, want cleanup", got)
	}
}

func TestSummarizeForkExecutionsKeepsEveryLivenessClassVisible(t *testing.T) {
	observations := []forkspace.ExecutionObservation{
		{Record: forkspace.ExecutionRecord{ID: "active"}, Running: true, Active: true},
		{Record: forkspace.ExecutionRecord{ID: "parked"}, Running: true},
		{Record: forkspace.ExecutionRecord{ID: "stale"}, Stale: true},
		{Record: forkspace.ExecutionRecord{ID: "unknown"}},
	}
	active, parked, cleanup, unverified, problems := summarizeForkExecutions(observations)
	if active != 1 || parked != 1 || cleanup != 1 || unverified != 1 || len(problems) != 1 {
		t.Fatalf("execution summary = active %d parked %d cleanup %d unverified %d problems %v",
			active, parked, cleanup, unverified, problems)
	}
	if got := (forkStatus{Parked: parked}).stateCell(); got != "parked" {
		t.Fatalf("parked execution state = %q", got)
	}
	if got := (forkStatus{Active: active}).stateCell(); got != "active" {
		t.Fatalf("active execution state = %q", got)
	}
	if got := (forkStatus{UnverifiedSandboxes: unverified}).stateCell(); got != "unknown" {
		t.Fatalf("unverified execution state = %q", got)
	}
}
