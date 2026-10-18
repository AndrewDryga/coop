package loop

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/processidentity"
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
	if !strings.Contains(n, "23 completed task folders are archived") || !strings.Contains(n, "coop tasks rm --all-done") {
		t.Errorf("nudge should name the count and the exact command, got %q", n)
	}
}

func TestLoopServiceFingerprintChangesWithDefinition(t *testing.T) {
	t.Setenv(box.ServiceStateRootEnv, t.TempDir())
	repo := t.TempDir()
	compose := filepath.Join(repo, ".agent", "compose.yml")
	if err := os.MkdirAll(filepath.Dir(compose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: postgres:18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := loopServiceFingerprint(repo, repo)
	if first == "" || first != loopServiceFingerprint(repo, repo) {
		t.Fatalf("service fingerprint is empty or unstable: %q", first)
	}
	if err := os.WriteFile(compose, []byte("services:\n  db:\n    image: postgres:19\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if second := loopServiceFingerprint(repo, repo); second == "" || second == first {
		t.Fatalf("changed service definition kept fingerprint %q", second)
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
	base := forkspace.GitOut(repo, "rev-parse", "HEAD")

	// No new commit and nothing settled → one stall, prevHead held.
	head, baseline, stalls, err := c.advanceStall(repo, hosts, base, 0, 0, "task-x")
	if err != nil || head != base || baseline != 0 || stalls != 1 {
		t.Fatalf("stalled iteration = (%s, %d, %d, %v), want (%s, 0, 1, nil)", head, baseline, stalls, err, base)
	}
	// A new commit is progress: it rebaselines and clears the stall count.
	run("commit", "-q", "--allow-empty", "-m", "work")
	committed := forkspace.GitOut(repo, "rev-parse", "HEAD")
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
	if unread := (*progressReadError)(nil); !errors.As(err, &unread) {
		t.Errorf("head-read failure %v is not a progress read error, so the loop would call it a stall", err)
	}
}

func TestLoopTaskLimitCountsSettledTasks(t *testing.T) {
	unlimited := loopTaskLimit{}
	unlimited.assign("first", "First task")
	if got := unlimited.scope(); got != "" {
		t.Fatalf("unlimited loop scope = %q, want empty", got)
	}

	limit := loopTaskLimit{max: 2}
	limit.assign("first", "First task")
	if reached, err := limit.observe(map[string]string{"first": stateInProgress}); reached || err != nil || limit.settled != 0 {
		t.Fatalf("active first task = (reached=%v, err=%v, settled=%d), want not counted", reached, err, limit.settled)
	}
	if reached, err := limit.observe(map[string]string{"first": stateDone}); reached || err != nil || limit.settled != 1 || limit.scope() != "" {
		t.Fatalf("done first task = (reached=%v, err=%v, settled=%d, scope=%q), want 1 and unpinned", reached, err, limit.settled, limit.scope())
	}
	limit.assign("second", "Second task")
	if reached, err := limit.observe(map[string]string{"second": stateInProgress}); reached || err != nil {
		t.Fatalf("review-reopened second task should remain selected: reached=%v err=%v", reached, err)
	}
	if reached, err := limit.observe(map[string]string{"second": stateBlocked}); !reached || err != nil || limit.settled != 2 || limit.scope() != "second" {
		t.Fatalf("blocked second task = (reached=%v, err=%v, settled=%d, scope=%q), want limit reached and retained", reached, err, limit.settled, limit.scope())
	}
}

func TestLoopTaskLimitRejectsLostSelection(t *testing.T) {
	limit := loopTaskLimit{max: 1}
	limit.assign("missing", "Missing task")
	if _, err := limit.observe(map[string]string{}); err == nil || !strings.Contains(err.Error(), "lost task missing") {
		t.Fatalf("lost selected task error = %v", err)
	}
}

func TestLoopRejectsActionableDuplicateIDsAcrossQueues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		crashDone bool
	}{
		{name: "already actionable"},
		{name: "made actionable by crash recovery", crashDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			authorityRoot := t.TempDir()
			t.Setenv(tasks.TestLeaseAuthorityRootEnv, authorityRoot)
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
					key, err := tasks.LeaseAuthorityKey(filepath.Join(repo, queue), "same-id")
					if err != nil {
						t.Fatal(err)
					}
					writeTaskFile(t, filepath.Join(authorityRoot, key+".json"),
						"{\"version\":1,\"heartbeat_at\":\"2026-08-25T00:00:00Z\"}\n")
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

func TestCandidateReviewDoesNotReconcileTaskLifecycleBeforeLaunch(t *testing.T) {
	repo := t.TempDir()
	authorityRoot := t.TempDir()
	t.Setenv(tasks.TestLeaseAuthorityRootEnv, authorityRoot)
	id := "candidate-subject"
	done := filepath.Join(repo, tasksRoot, tasks.StateDone, id)
	writeTaskFile(t, filepath.Join(done, "task.md"), "# candidate subject\n")
	writeTaskFile(t, filepath.Join(done, "log.md"), "# log\n")
	writeTaskFile(t, filepath.Join(done, "state.md"), "# state\n")
	key, err := tasks.LeaseAuthorityKey(filepath.Join(repo, tasksRoot), id)
	if err != nil {
		t.Fatal(err)
	}
	writeTaskFile(t, filepath.Join(authorityRoot, key+".json"),
		"{\"version\":1,\"heartbeat_at\":\"2026-08-25T00:00:00Z\"}\n")
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{Name: "false"}, "test", Host{})
	code, err := c.Run(RunSpec{
		Repo: repo, Image: "no-such-image", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard,
		CandidateReview: &CandidateReviewSpec{
			CandidateID: "frozen-candidate", Head: "head", Tree: "tree", Round: 1, TaskIDs: []string{id},
		},
	})
	if code != -1 || err == nil || !strings.Contains(err.Error(), "not built") {
		t.Fatalf("candidate review prelaunch = code %d err %v", code, err)
	}
	if !pathExists(done) || pathExists(filepath.Join(repo, tasksRoot, tasks.StateInProgress, id)) {
		t.Fatal("candidate-only review reconciled task lifecycle before its read-only launch")
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

// A queue whose only work another agent holds is not a finished queue: the loop stops before any
// box, says who has the work, and exits 1 — the help's "work remains" — never 0.
func TestLoopExitsOneWhenAnotherAgentHoldsTheRemainingWork(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo, _ := gitrepo.NewCommitted(t)
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
	if code, err := tasks.CmdTasksFolder(repo, filepath.Join(repo, tasksRoot), []string{"claim", "2026-01-01-x"}); code != 0 || err != nil {
		t.Fatalf("claim = (%d, %v)", code, err)
	}
	cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
	writeTaskFile(t, filepath.Join(cfg.AgentProfileDir("claude", "default"), ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"access","expiresAt":4102444800000,"scopes":["user:inference"]}}`)
	c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
	c.boxRun = func(box.RunSpec) (int, error) {
		t.Error("a box launched for work another agent holds")
		return 0, nil
	}

	var code int
	var err error
	out := captureStderr(t, func() {
		code, err = c.Run(RunSpec{Repo: repo, Image: "img", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard,
			Rotation: ladder.NewRotation([]agents.Target{target("claude", "default")})})
	})
	if code != 1 || err != nil {
		t.Fatalf("held queue = (%d, %v), want exit 1 without an error", code, err)
	}
	if !strings.Contains(out, "The remaining work is reserved by another agent.") {
		t.Errorf("held queue did not say who has the work:\n%s", out)
	}
}

// A subproject folder that is a link, or a link an earlier box left on the way to a queue, passes a
// check by name, and then the box's task tools, which refuse such a queue, fail every call. The loop
// refuses the queue before it starts, naming it and the link.
func TestLoopRefusesAQueueReachedThroughALinkAtStartup(t *testing.T) {
	repo := t.TempDir()
	other := t.TempDir()
	writeTaskFile(t, filepath.Join(other, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
	if err := os.Symlink(other, filepath.Join(repo, "api")); err != nil {
		t.Fatal(err)
	}
	queue := filepath.Join("api", tasksRoot)
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{Name: "false"}, "test", Host{})

	code, err := c.Run(RunSpec{Repo: repo, Image: "no-such-image", Agent: "claude", Queues: []string{queue}, Sink: io.Discard})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "refusing task queue "+filepath.Join(repo, queue)) ||
		!strings.Contains(err.Error(), `"api" is a link`) {
		t.Fatalf("loop over a linked subproject queue = (%d, %v), want a refusal naming the queue and the link", code, err)
	}
}

func TestLoopRejectsInvalidVerifyAgentBeforeImageCheck(t *testing.T) {
	repo := t.TempDir()
	taskPath := filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md")
	writeTaskFile(t, taskPath, "# x\n")
	writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), "verify:\n  agent: [\"claude:opus:extra\"]\n")
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{Name: "false"}, "test", Host{})

	code, err := c.Run(RunSpec{Repo: repo, Image: "no-such-image", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "verify.agent") {
		t.Fatalf("invalid verify agent = (%d, %v), want load-time field error", code, err)
	}
	if strings.Contains(err.Error(), "not built") {
		t.Fatalf("invalid verify agent reached the image check: %v", err)
	}
	if _, statErr := os.Stat(taskPath); statErr != nil {
		t.Fatalf("invalid loop config changed the queued task: %v", statErr)
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

// Pre-flight releases a claim whose owning process is gone, so the loop can resume that task
// instead of skipping it forever as "claimed".
func TestLoopPreflightReleasesGoneOwnerClaims(t *testing.T) {
	t.Setenv(tasks.TestLeaseAuthorityRootEnv, t.TempDir())
	repo := t.TempDir()
	root := filepath.Join(repo, tasksRoot)
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-orphan", "task.md"), "# Orphan\n")
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Skipf("cannot start a sleeper: %v", err)
	}
	if !processidentity.Stable(processidentity.StartToken(sleeper.Process.Pid)) {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
		t.Skip("no stable process identity on this platform")
	}
	if code, err := tasks.CmdTasksFolder(repo, root, []string{"claim", "2026-01-01-orphan", "--as", "codex", "--pid", strconv.Itoa(sleeper.Process.Pid)}); code != 0 || err != nil {
		t.Fatalf("claim bound to the sleeper = %d, %v", code, err)
	}
	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()

	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{Name: "false"}, "test", Host{})
	// The task becomes actionable once released, so the run then fails on the missing image; the
	// release itself is what this proves.
	code, runErr := c.Run(RunSpec{Repo: repo, Image: "no-such-image", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard, Preflight: true, MaxTasks: 1})
	if _, owned, err := tasks.ReadTaskOwnerRecord(root, "2026-01-01-orphan"); err != nil || owned {
		t.Fatalf("owner record after pre-flight = owned %t, err %v; want the dead owner's claim released (run = %d, %v)", owned, err, code, runErr)
	}
}

// A drain that dies on "run 'coop build'" when the daemon is what actually went away sends the
// morning's debugging at a rebuild that changes nothing. The loop must name the daemon instead.
func TestLoopBlamesTheDaemonNotTheImage(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
	// Named "docker": EnsureDaemon probes only the Docker kind, which it reads off the binary.
	shim := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(&config.Config{RepoOverride: repo}, runtime.Runtime{Name: shim}, "test", Host{})

	_, err := c.Run(RunSpec{Repo: repo, Image: "no-such-image", Agent: "claude", Queues: []string{tasksRoot}, Sink: io.Discard})
	if err == nil {
		t.Fatal("loop succeeded with an unreachable daemon; want an error")
	}
	if !strings.Contains(err.Error(), "Docker is unavailable") {
		t.Errorf("loop = %q, want it to name the unreachable daemon", err)
	}
}

// The custom pre-flight box must run the rung the FIRST ITERATION will take, not the provider left
// over from the launch line. applyTarget is what points cfg at the active rung's account, model and
// effort AND returns the provider its argv is built from — so a pre-flight built before that call
// mounts one provider's credential while carrying another's command.
func TestLoopCustomPreflightRunsTheActiveRung(t *testing.T) {
	repo := t.TempDir() // deliberately not a git repo: the run stops at the HEAD read, before iteration 1
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# x\n")
	writeTaskFile(t, filepath.Join(repo, ".agent", "loop.yaml"), "preflight:\n  enabled: true\n  prompt: tidy the queue\n")
	cfg := &config.Config{RepoOverride: repo, ConfigDir: t.TempDir(), Homes: true}
	c := New(cfg, runtime.Runtime{Name: "true"}, "test", Host{})
	var launched []box.RunSpec
	c.boxRun = func(s box.RunSpec) (int, error) { launched = append(launched, s); return 0, nil }
	rot := ladder.NewRotation([]agents.Target{target("codex", "work"), target("claude", "personal")})

	code, err := c.Run(RunSpec{
		Repo: repo, Image: "img", Agent: "claude", Queues: []string{tasksRoot},
		Sink: io.Discard, Preflight: true, Rotation: rot,
	})
	if err == nil || !strings.Contains(err.Error(), "read HEAD") {
		t.Fatalf("run = (%d, %v), want the stop at the HEAD read so only the pre-flight box launched", code, err)
	}
	if len(launched) != 1 {
		t.Fatalf("boxes launched = %d, want exactly the pre-flight one", len(launched))
	}
	if launched[0].Agent != "codex" {
		t.Errorf("pre-flight box ran agent %q, want the active rung's provider codex", launched[0].Agent)
	}
	if got := cfg.ActiveProfile("codex"); got != "work" {
		t.Errorf("pre-flight launched before applyTarget: active codex account = %q, want work", got)
	}
}

// coop eval tells a loop that never started from one that ran and left work (both exit 1) by the
// start record the loop creates when its first box reaches the runtime launch. A box whose host
// setup failed launched nothing, the box's own launch hook still runs, and a record that cannot be
// written warns without stopping the box.
func TestLoopRecordsItsStartAtTheFirstRuntimeLaunch(t *testing.T) {
	launches := func(c *Control, reach bool, hook func()) {
		t.Helper()
		c.boxRun = func(s box.RunSpec) (int, error) {
			if reach {
				s.OnRuntimeLaunch()
			}
			return 1, nil
		}
		if _, err := c.runBox(box.RunSpec{Image: "img", OnRuntimeLaunch: hook}); err != nil {
			t.Fatal(err)
		}
	}
	started := filepath.Join(t.TempDir(), "loop-started")
	c := &Control{cfg: &config.Config{EvalLoopStartedFile: started}}
	launches(c, false, nil)
	if _, err := os.Lstat(started); !os.IsNotExist(err) {
		t.Fatalf("a box that never reached its runtime launch recorded a start: %v", err)
	}
	armed := 0
	launches(c, true, func() { armed++ })
	if info, err := os.Lstat(started); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the first runtime launch left no start record: %v", err)
	}
	if armed != 1 {
		t.Errorf("the box's own launch hook ran %d times, want 1", armed)
	}

	unwritable := &Control{cfg: &config.Config{EvalLoopStartedFile: filepath.Join(t.TempDir(), "gone", "loop-started")}}
	armed = 0
	out := captureStderr(t, func() { launches(unwritable, true, func() { armed++ }) })
	if armed != 1 || !strings.Contains(out, "Could not save the start record coop eval asked for") {
		t.Errorf("an unwritable start record: launch hook ran %d times, stderr %q", armed, out)
	}

	var plain box.RunSpec
	outside := &Control{cfg: &config.Config{}, boxRun: func(s box.RunSpec) (int, error) { plain = s; return 0, nil }}
	if _, err := outside.runBox(box.RunSpec{Image: "img"}); err != nil {
		t.Fatal(err)
	}
	if plain.OnRuntimeLaunch != nil {
		t.Error("a loop outside coop eval got a launch hook nobody asked for")
	}
}

// A progress check that cannot read the queue or HEAD is not a stall: it stops the loop with its own
// headline naming what it could not read, and only five quiet attempts say no task moved.
func TestStallNoticeTellsAReadFailureFromAStall(t *testing.T) {
	c := &Control{cfg: &config.Config{}}
	queue := filepath.Join(t.TempDir(), "queue")
	if err := os.WriteFile(queue, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := c.advanceStall(t.TempDir(), []string{queue}, "base", 0, 0, "task-x")
	var unread *progressReadError
	if !errors.As(err, &unread) || unread.what != "the task queue" {
		t.Fatalf("advanceStall over an unreadable queue = %v, want a progress read error for the queue", err)
	}
	if headline, cause := stallNotice(err, "task-x"); headline != "The loop could not check its progress" ||
		!strings.Contains(cause, "could not read the task queue") || strings.Contains(cause, "No task finished") {
		t.Errorf("read failure notice = %q / %q", headline, cause)
	}
	stall := errors.New("no task finished, blocked, or committed in 5 iterations — stopping")
	if headline, cause := stallNotice(stall, "task-x"); headline != "The loop stopped making progress" ||
		!strings.Contains(cause, "No task finished, became blocked or produced a commit in 5 attempts") {
		t.Errorf("stall notice = %q / %q", headline, cause)
	}
}
