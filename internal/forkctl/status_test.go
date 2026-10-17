package forkctl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/testutil/starttoken"

	"github.com/AndrewDryga/coop/internal/config"
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
		"running":   "background loop running",
		"elsewhere": "background loop started on Linux",
		"active":    "agent running",
		"session":   "reserved for a session",
		"parked":    "waiting",
		"landing":   "merging",
		"ready":     "ready to merge",
		"legacy":    "older fork format",
		"cleanup":   "cleanup needed",
		"unknown":   "status unavailable",
		"idle":      "idle",
	} {
		if got := stateWords(state, "Linux"); got != want {
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

// A background loop that a coop on another operating system started, as one in a Linux container
// sharing this folder with macOS, is recorded with a pid this kernel cannot check, whether that pid
// is missing here or belongs to a process here. The listing called such a record a cleanup, pointing
// at a stop that refuses it, or "status unavailable" once the loop's own sandbox record, unverifiable
// for the same reason, was there too. It now says which system started the loop, in words and in the
// JSON, and calls it neither cleanup nor running.
func TestForkLsNamesTheSystemThatStartedALoop(t *testing.T) {
	system := forkspace.ProcTokenSystem(starttoken.Foreign())
	wantText := "Forks\n\n  perf · background loop started on " + system + " · updated 7 days ago\n\nReview a fork: coop fork review <name>\n"
	const wantStatus = `{"state":"elsewhere","tasks":{"todo":0,"in_progress":0,"done":0,"blocked":0},` +
		`"active_sandboxes":0,"parked_sandboxes":0,"cleanup_sandboxes":0,"unverified_sandboxes":%d,` +
		`"detached_running":false,"detached_system":%q,"workspace_reserved":false,"candidate":false,` +
		`"landing":false,"cleanup_pending":false,"legacy_generation":false%s}`
	for _, tc := range []struct {
		name string
		pid  int
	}{{"pid missing here", 2147483645}, {"pid alive here", os.Getpid()}} {
		pid := tc.pid
		t.Run(tc.name, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			workspace := forkspace.Workspace(repo, "perf")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatal(err)
			}
			unlock, err := forkspace.LockState(repo, "perf")
			if err != nil {
				t.Fatal(err)
			}
			identity, err := forkspace.EnsureGenerationLocked(repo, "perf")
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			worker := forkspace.WorkerState{Pid: pid, Token: starttoken.Foreign(), Generation: identity.Generation}
			if err := forkspace.WriteWorkerState(repo, "perf", worker); err != nil {
				t.Fatal(err)
			}
			// A fixed age keeps the heading's "updated" part out of the test's timing.
			lastWeek := time.Now().Add(-7*24*time.Hour - time.Hour)
			if err := os.Chtimes(workspace, lastWeek, lastWeek); err != nil {
				t.Fatal(err)
			}
			c := &Control{cfg: &config.Config{RepoOverride: repo}}

			text, stderr, status := listForkStatus(t, c, "perf")
			if text != wantText {
				t.Errorf("fork ls printed:\n%s\nwant:\n%s", text, wantText)
			}
			if stderr != "" {
				t.Errorf("fork ls reported a problem:\n%s", stderr)
			}
			if want := fmt.Sprintf(wantStatus, 0, system, ""); status != want {
				t.Errorf("fork ls --json status =\n%s\nwant\n%s", status, want)
			}

			sandbox := forkspace.ExecutionRecord{
				Version: 1, ID: strings.Repeat("c", 32), Kind: forkspace.ExecutionForkLoop,
				Role: forkspace.ExecutionRoleDetachedWorker, Workspace: workspace, Fork: &identity,
				SourceID: "fork-loop-perf-" + string(identity.Generation), PID: pid, Token: starttoken.Foreign(),
				StartedAt: time.Now().UTC(),
			}
			body, err := json.Marshal(sandbox)
			if err != nil {
				t.Fatal(err)
			}
			executionDir := filepath.Join(forkspace.StateDir(repo), "executions")
			if err := os.MkdirAll(executionDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(executionDir, sandbox.ID+".json"), append(body, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			cause := "sandbox " + sandbox.ID + " liveness is unverifiable"
			text, stderr, status = listForkStatus(t, c, "perf")
			if text != wantText {
				t.Errorf("fork ls beside the loop's sandbox printed:\n%s\nwant:\n%s", text, wantText)
			}
			if !strings.Contains(stderr, cause) {
				t.Errorf("fork ls did not report %q:\n%s", cause, stderr)
			}
			if want := fmt.Sprintf(wantStatus, 1, system, `,"problems":[`+strconv.Quote(cause)+`]`); status != want {
				t.Errorf("fork ls --json status beside the loop's sandbox =\n%s\nwant\n%s", status, want)
			}
		})
	}
}

// listForkStatus runs coop fork ls and coop fork ls --json, and returns the listing, what the listing
// printed to stderr, and the named fork's JSON status, compacted.
func listForkStatus(t *testing.T, c *Control, name string) (text, stderr, status string) {
	t.Helper()
	var code int
	var err error
	text = captureStdout(t, func() {
		stderr = captureStderr(t, func() { code, err = c.ForkLs(nil) })
	})
	if code != 0 || err != nil {
		t.Fatalf("fork ls = (%d, %v)", code, err)
	}
	out := captureStdout(t, func() { code, err = c.ForkLs([]string{"--json"}) })
	if code != 0 || err != nil {
		t.Fatalf("fork ls --json = (%d, %v)", code, err)
	}
	var got struct {
		Workspaces []struct {
			Name   string          `json:"name"`
			Status json.RawMessage `json:"status"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	for _, workspace := range got.Workspaces {
		if workspace.Name == name {
			var compact bytes.Buffer
			if err := json.Compact(&compact, workspace.Status); err != nil {
				t.Fatal(err)
			}
			return text, stderr, compact.String()
		}
	}
	t.Fatalf("fork ls --json did not list fork %s:\n%s", name, out)
	return "", "", ""
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
