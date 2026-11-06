package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/testutil/wait"
)

// runForkLoopAs runs a fork's loop on the project's own queues, in the foreground or as the detached
// worker, and returns what it printed.
func runForkLoopAs(t *testing.T, a *app, repo, ws string, identity forkspace.Identity, detached bool) (int, string, error) {
	t.Helper()
	var code int
	var runErr error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, runErr = a.runForkLoop(repo, ws, identity, "claude", nil, "", "", "", nil, networkFlags{}, false, detached)
		})
	})
	return code, stdout + stderr, runErr
}

// ignoreTheQueue keeps the project's task folders out of Git, as `coop init` does, so a merge sees a
// clean checkout while the queue moves tasks about.
func ignoreTheQueue(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".agent/tasks/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "-qm", "ignore the task queue")
}

// A fork kept after its work merged takes no new task: its candidate history is closed, and the
// scheduler leaves a kept workspace alone. A loop on it, in the foreground or in the background, used
// to end at once with only next steps, or after a "Started" and an empty log. It refuses up front now,
// names why, and gives two ways on that work: starting the fork over, and a new fork.
func TestForkLoopOnAMergedForkRefusesUpFront(t *testing.T) {
	repo := initRepo(t)
	ignoreTheQueue(t, repo)
	seedForkDecisionTask(t, repo, "2026-11-06-fork-merged-a")
	a := forkNetworkPreflightApp(t, repo)
	var ran []string
	reviews := 0
	a.forkLoopRun = completingForkPass(t, &ran, &reviews)
	ws, identity := setUpForkForDecision(t, repo, "kept")
	if code, out, err := runForkLoopAs(t, a, repo, ws, identity, false); code != 0 || err != nil {
		t.Fatalf("the fork's loop = (%d, %v)\n%s", code, err, out)
	}
	// Merge it and keep the fork: a file it has not committed makes merge keep it.
	if err := os.WriteFile(filepath.Join(ws, "scratch.txt"), []byte("not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var code int
	var err error
	if out := captureStderr(t, func() { code, err = a.forkctl().ForkMerge([]string{"kept", "--yes"}) }); code != 0 || err != nil || !pathExists(ws) {
		t.Fatalf("merge = (%d, %v), fork kept: %v\n%s", code, err, pathExists(ws), out)
	}
	if status, ok, err := tasks.ReadForkCandidateRoundStatus(repo, identity); err != nil || !ok || !status.Landed() {
		t.Fatalf("candidate after the merge = %+v, %v, %v; want landed", status, ok, err)
	}
	// A new task arrives. The kept fork must not take it, and must say so.
	second := "2026-11-06-fork-merged-b"
	seedForkDecisionTask(t, repo, second)
	ran, reviews = nil, 0
	for _, tc := range []struct {
		args    []string
		options string
	}{
		{[]string{"kept", "claude", "--loop"}, "--loop"},
		{[]string{"kept", "claude", "--loop", "-d"}, "--loop -d"},
	} {
		var out string
		stdout := captureStdout(t, func() {
			out = captureStderr(t, func() { code, err = a.forkCreate(tc.args) })
		})
		block := asUsage(err)
		if code != 1 || block == nil || block.Headline != "Fork kept already merged its work" ||
			block.Cause != "A merged fork keeps its files for you to look at, and takes no new tasks." {
			t.Fatalf("coop fork %s = (%d, %v)\n%s%s", strings.Join(tc.args, " "), code, err, stdout, out)
		}
		wantRows := [][2]string{
			{"Start it over:", "coop fork kept claude " + tc.options + " --fresh"},
			{"Use a new fork:", "coop fork <new-name> claude " + tc.options},
		}
		if !slices.Equal(block.Rows, wantRows) {
			t.Errorf("coop fork %s rows = %v, want %v", strings.Join(tc.args, " "), block.Rows, wantRows)
		}
		if strings.Contains(stdout+out, "Started fork") || pathExists(forkspace.PidPath(repo, "kept")) {
			t.Errorf("a refused loop started a worker:\n%s%s", stdout, out)
		}
	}
	if len(ran) != 0 || reviews != 0 {
		t.Errorf("a refused loop ran passes: %v, %d reviews", ran, reviews)
	}
	assertForkNetworkTaskUnassigned(t, repo, second)
}

// A loop that takes no task says so, the way coop loop does for an empty queue: in the terminal for a
// foreground loop, in the log for a background one, and beside the idle fork in coop fork ls. It used
// to end in silence, after a "✓ Started" for a worker that had done nothing.
func TestForkLoopWithNothingToTakeSaysWhy(t *testing.T) {
	type outcome struct {
		code int
		out  string
	}
	run := func(t *testing.T, a *app, repo, name string, detached bool) (string, outcome) {
		t.Helper()
		ws, identity := setUpForkForDecision(t, repo, name)
		code, out, err := runForkLoopAs(t, a, repo, ws, identity, detached)
		if err != nil {
			t.Fatalf("the fork's loop failed: %v\n%s", err, out)
		}
		listing := captureStdout(t, func() {
			if code, err := a.forkctl().ForkLs(nil); code != 0 || err != nil {
				t.Fatalf("fork ls = (%d, %v)", code, err)
			}
		})
		return listing, outcome{code, out}
	}
	for _, detached := range []bool{false, true} {
		name := map[bool]string{false: "foreground", true: "detached"}[detached]
		t.Run("an empty queue, "+name, func(t *testing.T) {
			repo := initRepo(t)
			if err := tasks.ScaffoldStateDirs(filepath.Join(repo, ".agent", "tasks")); err != nil {
				t.Fatal(err)
			}
			a := forkNetworkPreflightApp(t, repo)
			a.forkLoopRun = func(loop.RunSpec) (int, error) { t.Error("a loop pass ran with no task"); return 1, nil }
			listing, got := run(t, a, repo, "idle", detached)
			if got.code != 0 || !strings.Contains(got.out, "No tasks ready to work on.") {
				t.Errorf("empty queue = (%d)\n%s", got.code, got.out)
			}
			if steps := strings.Contains(got.out, "coop fork review idle"); steps == detached {
				t.Errorf("next steps printed = %v for a loop in the background = %v:\n%s", steps, detached, got.out)
			}
			if !strings.Contains(listing, "idle · idle") || !strings.Contains(listing, "    Last loop: No tasks ready to work on.\n") {
				t.Errorf("the listing does not say why the fork is idle:\n%s", listing)
			}
		})
	}

	t.Run("only a decision is left", func(t *testing.T) {
		repo := initRepo(t)
		queue := filepath.Join(repo, ".agent", "tasks")
		seedForkDecisionTaskIn(t, queue, "2026-11-06-fork-waits-a")
		if err := tasks.ScaffoldStateDirs(queue); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(queue, tasks.StateTodo, "2026-11-06-fork-waits-a"), filepath.Join(queue, tasks.StateBlocked, "2026-11-06-fork-waits-a")); err != nil {
			t.Fatal(err)
		}
		a := forkNetworkPreflightApp(t, repo)
		listing, got := run(t, a, repo, "waits", true)
		if got.code != 3 || !strings.Contains(got.out, "No tasks ready to work on. 1 task needs your decision.") ||
			!strings.Contains(got.out, "Answer it: coop tasks decisions -i") {
			t.Errorf("a queue with only a decision = (%d), want exit 3 as coop loop gives\n%s", got.code, got.out)
		}
		if !strings.Contains(listing, "    Last loop: No tasks ready to work on. 1 task needs your decision.\n") {
			t.Errorf("the listing does not say why the fork is idle:\n%s", listing)
		}
	})

	t.Run("another fork holds what is left", func(t *testing.T) {
		repo := initRepo(t)
		seedForkDecisionTask(t, repo, "2026-11-06-fork-held-a")
		a := forkNetworkPreflightApp(t, repo)
		// The first fork takes the task and its pass leaves it in progress, so it stays that fork's.
		a.forkLoopRun = func(loop.RunSpec) (int, error) { return 0, nil }
		ws, identity := setUpForkForDecision(t, repo, "holder")
		if code, out, err := runForkLoopAs(t, a, repo, ws, identity, false); code != 0 || err != nil {
			t.Fatalf("the first fork's loop = (%d, %v)\n%s", code, err, out)
		}
		a.forkLoopRun = func(loop.RunSpec) (int, error) {
			t.Error("a loop pass ran for a task another fork holds")
			return 1, nil
		}
		listing, got := run(t, a, repo, "waiting", true)
		if got.code != 0 || !strings.Contains(got.out, "No task is available for fork waiting.") ||
			!strings.Contains(got.out, "held by other agents") {
			t.Errorf("a fork with only another fork's task = (%d)\n%s", got.code, got.out)
		}
		if !strings.Contains(listing, "    Last loop: No task is available for fork waiting.\n") {
			t.Errorf("the listing does not say why the fork is idle:\n%s", listing)
		}
	})
}

// The real background start on an empty queue, with the worker re-executed as coop: it used to print
// "Started" and leave a log of 0 bytes. The worker's log now says why it stopped, `coop fork logs`
// shows it, and so does the idle fork's row in `coop fork ls`.
func TestDetachedStartOnAnEmptyQueueLogsWhyTheWorkerStopped(t *testing.T) {
	repo := initRepo(t)
	if err := tasks.ScaffoldStateDirs(filepath.Join(repo, ".agent", "tasks")); err != nil {
		t.Fatal(err)
	}
	a := forkNetworkPreflightApp(t, repo)
	name := "quiet"
	t.Setenv(detachedChildEnv, "1") // the worker is this test binary run as coop
	t.Cleanup(func() {
		if pathExists(forkspace.PidPath(repo, name)) {
			if code, err := a.forkctl().ForkStop([]string{name}); code != 0 || err != nil {
				t.Errorf("cleanup fork stop = (%d, %v)", code, err)
			}
		}
	})
	var code int
	var err error
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code, err = a.forkCreate([]string{name, "claude", "--loop", "-d", "--egress", "open"})
		})
	})
	if code != 0 || err != nil || !strings.Contains(stdout+stderr, "Started fork quiet in the background") {
		t.Fatalf("detached start = (%d, %v)\n%s%s", code, err, stdout, stderr)
	}
	wait.For(t, "the worker to end and clear its record", func() bool { return !pathExists(forkspace.PidPath(repo, name)) })
	logs := captureStdout(t, func() {
		if code, err := a.forkctl().ForkLogs([]string{name}); code != 0 || err != nil {
			t.Fatalf("fork logs = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(logs, "No tasks ready to work on.") {
		t.Errorf("the worker's log does not say why it stopped:\n%q", logs)
	}
	listing := captureStdout(t, func() {
		if code, err := a.forkctl().ForkLs(nil); code != 0 || err != nil {
			t.Fatalf("fork ls = (%d, %v)", code, err)
		}
	})
	if !strings.Contains(listing, "quiet · idle") || !strings.Contains(listing, "    Last loop: No tasks ready to work on.\n") {
		t.Errorf("the listing does not say why the fork is idle:\n%s", listing)
	}
}

// A background loop on a fork that is already ready to merge has nothing more to do, and says so in its
// log instead of leaving it empty. Nothing runs again.
func TestADetachedLoopOnAReadyForkSaysSo(t *testing.T) {
	repo := initRepo(t)
	seedForkDecisionTask(t, repo, "2026-11-06-fork-ready-a")
	a := forkNetworkPreflightApp(t, repo)
	var ran []string
	reviews := 0
	a.forkLoopRun = completingForkPass(t, &ran, &reviews)
	ws, identity := setUpForkForDecision(t, repo, "ready")
	if code, out, err := runForkLoopAs(t, a, repo, ws, identity, false); code != 0 || err != nil || !strings.Contains(out, "Fork ready is ready to merge") {
		t.Fatalf("the first loop = (%d, %v)\n%s", code, err, out)
	}
	code, out, err := runForkLoopAs(t, a, repo, ws, identity, true)
	if code != 0 || err != nil {
		t.Fatalf("the loop on a ready fork = (%d, %v)\n%s", code, err, out)
	}
	for _, want := range []string{"Fork ready is ready to merge", "Review: coop fork review ready", "Merge:  coop fork merge ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("the background loop on a ready fork lacks %q:\n%s", want, out)
		}
	}
	if len(ran) != 1 || reviews != 1 {
		t.Errorf("the second loop did work again: %v, %d reviews", ran, reviews)
	}
}
