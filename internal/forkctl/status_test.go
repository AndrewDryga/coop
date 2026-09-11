package forkctl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

// An unreported cost is omitted, never rendered as a free run.
func TestCostLine(t *testing.T) {
	if got := (forkStatus{}).costLine(); got != "" {
		t.Errorf("a fork with no reported cost = %q, want it omitted", got)
	}
	if got := (forkStatus{Cost: 12.3}).costLine(); got != "$12.30" {
		t.Errorf("costLine(12.3) = %q, want $12.30", got)
	}
}

// The listing renders the assignment PHASES, so a fork with reviewed-but-unmerged work is never
// described as done. gatherForkStatus folds reviewing and ready into Counts.Done for the machine
// projection; the human line must not reuse that number.
func TestTasksLineRendersEachPhase(t *testing.T) {
	s := forkStatus{Doing: 1, Reviewing: 1, Blocked: 1}
	if got, want := s.tasksLine(), "1 in progress · 1 being reviewed · 1 blocked"; got != want {
		t.Errorf("tasksLine = %q, want %q", got, want)
	}
	if got, want := (forkStatus{ReadyTasks: 1}).tasksLine(), "1 ready to merge"; got != want {
		t.Errorf("ready tasksLine = %q, want %q", got, want)
	}
	if got := (forkStatus{}).tasksLine(); got != "" {
		t.Errorf("a fork with no tracked task = %q, want it omitted", got)
	}
}

// An uncommitted tree is stated in words; the glyph it replaced meant nothing on its own.
func TestChangesLine(t *testing.T) {
	if got, want := (forkStatus{Ins: 18, Del: 4}).changesLine(), "+18 −4"; got != want {
		t.Errorf("changesLine = %q, want %q", got, want)
	}
	if got, want := (forkStatus{Ins: 18, Del: 4, Dirty: true}).changesLine(), "+18 −4 · uncommitted changes"; got != want {
		t.Errorf("dirty changesLine = %q, want %q", got, want)
	}
}

// Every machine state has a human translation; the machine value itself is the JSON contract and
// is never rewritten (see forkLsJSON).
func TestStateWordsCoversEveryState(t *testing.T) {
	for state, want := range map[string]string{
		"running": "background loop running",
		"active":  "agent running",
		"session": "reserved for a session",
		"parked":  "waiting",
		"landing": "merging",
		"ready":   "ready to merge",
		"legacy":  "older fork format",
		"cleanup": "cleanup needed",
		"unknown": "status unavailable",
		"idle":    "idle",
	} {
		if got := stateWords(state); got != want {
			t.Errorf("stateWords(%q) = %q, want %q", state, got, want)
		}
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
