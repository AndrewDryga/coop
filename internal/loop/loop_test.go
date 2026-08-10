package loop

import (
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/gitrepo"
)

// The prune nudge fires only once done/ has piled up past the threshold, names the exact command,
// and stays quiet below it — pruning destroys state, so the loop only ever SUGGESTS it.
func TestPruneNudge(t *testing.T) {
	if n := pruneNudge(doneNudgeThreshold - 1); n != "" {
		t.Errorf("below the threshold there should be no nudge, got %q", n)
	}
	n := pruneNudge(23)
	if !strings.Contains(n, "23 done task folders") || !strings.Contains(n, "coop tasks rm --all-done") {
		t.Errorf("nudge should name the count and the exact command, got %q", n)
	}
}

// TestAdvanceStallHeadRead: the stall bookkeeping reads HEAD to tell a committing iteration from a
// stalled one, so a HEAD that cannot be read stops the loop — it must never be counted as "no new
// commit", which would spend the stall budget on a broken repo and hide the real failure.
func TestAdvanceStallHeadRead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, run := gitrepo.New(t)
	run("commit", "-q", "--allow-empty", "-m", "base")
	c := &Control{cfg: &config.Config{}}
	hosts := []string{filepath.Join(t.TempDir(), "queue")} // empty queue: nothing settles on its own
	base := gitOut(repo, "rev-parse", "HEAD")

	// No new commit and nothing settled → one stall, prevHead held.
	head, baseline, stalls, err := c.advanceStall(repo, hosts, base, 0, 0, "task-x")
	if err != nil || head != base || baseline != 0 || stalls != 1 {
		t.Fatalf("stalled iteration = (%s, %d, %d, %v), want (%s, 0, 1, nil)", head, baseline, stalls, err, base)
	}
	// A new commit is progress: it rebaselines and clears the stall count.
	run("commit", "-q", "--allow-empty", "-m", "work")
	committed := gitOut(repo, "rev-parse", "HEAD")
	head, baseline, stalls, err = c.advanceStall(repo, hosts, base, 0, 2, "task-x")
	if err != nil || head != committed || baseline != 0 || stalls != 0 {
		t.Fatalf("committing iteration = (%s, %d, %d, %v), want (%s, 0, 0, nil)", head, baseline, stalls, err, committed)
	}
	// An unreadable HEAD fails the iteration and leaves the bookkeeping untouched.
	head, baseline, stalls, err = c.advanceStall(filepath.Join(t.TempDir(), "gone"), hosts, committed, 4, 2, "task-x")
	if err == nil {
		t.Fatal("advanceStall with an unreadable HEAD = nil error, want a loud stop")
	}
	if head != committed || baseline != 4 || stalls != 2 {
		t.Errorf("failed head read perturbed stall state: got (%s, %d, %d), want (%s, 4, 2)", head, baseline, stalls, committed)
	}
	if !strings.Contains(err.Error(), "HEAD") || !strings.Contains(err.Error(), "coop loop") {
		t.Errorf("head-read failure %q should name what failed and how to recover", err)
	}
}

func TestLoopTaskLimitCountsSettledTasks(t *testing.T) {
	unlimited := loopTaskLimit{}
	unlimited.assign("first")
	if got := unlimited.scope(); got != "" {
		t.Fatalf("unlimited loop scope = %q, want empty", got)
	}

	limit := loopTaskLimit{max: 2}
	limit.assign("first")
	if reached, err := limit.observe(map[string]string{"first": stateInProgress}); reached || err != nil || limit.settled != 0 {
		t.Fatalf("active first task = (reached=%v, err=%v, settled=%d), want not counted", reached, err, limit.settled)
	}
	if reached, err := limit.observe(map[string]string{"first": stateDone}); reached || err != nil || limit.settled != 1 || limit.scope() != "" {
		t.Fatalf("done first task = (reached=%v, err=%v, settled=%d, scope=%q), want 1 and unpinned", reached, err, limit.settled, limit.scope())
	}
	limit.assign("second")
	if reached, err := limit.observe(map[string]string{"second": stateInProgress}); reached || err != nil {
		t.Fatalf("review-reopened second task should remain selected: reached=%v err=%v", reached, err)
	}
	if reached, err := limit.observe(map[string]string{"second": stateBlocked}); !reached || err != nil || limit.settled != 2 || limit.scope() != "second" {
		t.Fatalf("blocked second task = (reached=%v, err=%v, settled=%d, scope=%q), want limit reached and retained", reached, err, limit.settled, limit.scope())
	}
}

func TestLoopTaskLimitRejectsLostSelection(t *testing.T) {
	limit := loopTaskLimit{max: 1}
	limit.assign("missing")
	if _, err := limit.observe(map[string]string{}); err == nil || !strings.Contains(err.Error(), "lost task missing") {
		t.Fatalf("lost selected task error = %v", err)
	}
}

func TestLoopRejectsActionableDuplicateIDsAcrossQueues(t *testing.T) {
	// "lease.lock"/"lease.json" mirror internal/tasks's own unexported leaseLockName/
	// leaseMetadataName (lease.go) — inlined rather than exported, since this crash-recovery
	// simulation is their only consumer outside that package.
	const leaseLockName, leaseMetadataName = "lease.lock", "lease.json"
	for _, tc := range []struct {
		name      string
		crashDone bool
	}{
		{name: "already actionable"},
		{name: "made actionable by crash recovery", crashDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			queues := []string{"queue-a", "queue-b"}
			for _, queue := range queues {
				state := tasks.StateTodo
				if tc.crashDone {
					state = tasks.StateDone
				}
				dir := filepath.Join(repo, queue, state, "same-id")
				writeTaskFile(t, filepath.Join(dir, "task.md"), "# same id\n")
				if tc.crashDone {
					writeTaskFile(t, filepath.Join(dir, "log.md"), "# log\n")
					writeTaskFile(t, filepath.Join(dir, "state.md"), "# state\n")
					writeTaskFile(t, filepath.Join(dir, "tmp", leaseLockName), "")
					writeTaskFile(t, filepath.Join(dir, "tmp", leaseMetadataName), "{}\n")
				}
			}
			c := &Control{cfg: &config.Config{}}
			code, err := c.Run(RunSpec{Repo: repo, Image: "missing-image", Agent: "codex", Queues: queues})
			if code != 1 || err == nil || !strings.Contains(err.Error(), "same-id") || !strings.Contains(err.Error(), "multiple queues") {
				t.Fatalf("duplicate loop = code %d err %v", code, err)
			}
			if tc.crashDone {
				for _, queue := range queues {
					if !pathExists(filepath.Join(repo, queue, tasks.StateInProgress, "same-id")) {
						t.Fatalf("%s crash candidate was not restored before duplicate validation", queue)
					}
				}
			}
		})
	}
}

// TestLoopAcceptsFolderQueue is the regression guard for the loop's queue-existence check:
// it used fileExists, which is false for a directory, so it rejected every folder queue with
// "no task file found" before running a single iteration. The guard must accept a real
// .agent/tasks directory and proceed (here it then fails at the image check — runtime "false"
// makes ImageExists report no image — which proves the guard passed).
func TestLoopAcceptsFolderQueue(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{Name: "false"}, "test", Host{})

	code, err := c.Run(RunSpec{Repo: repo, Image: "no-such-image", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard})
	if err == nil {
		t.Fatalf("expected loop to fail at the image check, got (%d, nil)", code)
	}
	if strings.Contains(err.Error(), "no task queue") || strings.Contains(err.Error(), "no task file") {
		t.Fatalf("loop rejected a valid folder queue at the existence guard: %v", err)
	}
	if !strings.Contains(err.Error(), "not built") {
		t.Fatalf("guard should pass and fail at the image check, got: %v", err)
	}
}

func TestLoopTaskLimitWithNoActionableTaskNeedsNoImage(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateDone, "2026-01-01-done", "task.md"), "# Done\n")
	writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), "preflight:\n  enabled: true\n  prompt: should not launch a box\n")
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{Name: "false"}, "test", Host{})

	code, err := c.Run(RunSpec{Repo: repo, Image: "no-such-image", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, Preflight: true, MaxTasks: 3})
	if err != nil || code != 0 {
		t.Fatalf("idle task-limited loop = (%d, %v), want success without an image", code, err)
	}
}
