package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestFinishedTasksAndReconcileDecision(t *testing.T) {
	// reconcileMerged: a landed todo/in_progress task moves; a landed blocked task is flagged (no
	// move); an unlanded task is ignored entirely.
	states := map[string]string{"todo1": stateTodo, "wip1": stateInProgress, "blk1": stateBlocked, "safe": stateTodo}
	landed := map[string]bool{"todo1": true, "wip1": true, "blk1": true} // "safe" did NOT land
	acts := reconcileMerged(states, landed)
	got := map[string]bool{}
	for _, a := range acts {
		got[a.ID] = a.Move
	}
	if len(acts) != 3 || !got["todo1"] || !got["wip1"] || got["blk1"] {
		t.Errorf("reconcileMerged = %+v; want todo1/wip1 move, blk1 flagged, safe absent", acts)
	}
	if _, present := got["safe"]; present {
		t.Error("an unlanded task must not be reconciled")
	}
}

func TestAggregateDuplicateTaskIDs(t *testing.T) {
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	writeTaskFile(t, filepath.Join(first, stateTodo, "actionable", "task.md"), "# actionable\n")
	writeTaskFile(t, filepath.Join(second, stateDone, "actionable", "task.md"), "# actionable archive\n")
	for _, root := range []string{first, second} {
		writeTaskFile(t, filepath.Join(root, stateBlocked, "blocked", "task.md"), "# blocked\n")
		writeTaskFile(t, filepath.Join(root, stateBlocked, "blocked", "decision.md"), "# decision\n")
		writeTaskFile(t, filepath.Join(root, stateDone, "archived", "task.md"), "# archived\n")
	}
	hosts := []string{first, second}
	if got, want := aggregateDuplicateTaskIDs(hosts), []string{"actionable", "archived", "blocked"}; !slices.Equal(got, want) {
		t.Fatalf("aggregate duplicate ids = %v, want %v", got, want)
	}
	if got, want := nonArchivedDuplicateTaskIDs(hosts), []string{"actionable", "blocked"}; !slices.Equal(got, want) {
		t.Fatalf("non-archived duplicate ids = %v, want %v", got, want)
	}
}

func TestCompletedAssignedTaskIgnoresUnrelatedHeldCompletion(t *testing.T) {
	root := t.TempDir()
	assigned := taskForLease(t, root, stateInProgress, "assigned")
	unrelated := taskForLease(t, root, stateInProgress, "unrelated")
	foreign, _, err := tryTaskLease(root, unrelated, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.release() })
	for _, task := range []taskItem{assigned, unrelated} {
		if err := moveTaskDir(root, task, stateDone); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := completedAssignedTask(root, assigned.ID)
	if !ok || got.Root != root || got.Item.ID != assigned.ID {
		t.Fatalf("assigned completion = (%+v, %v)", got, ok)
	}
	if !pathExists(filepath.Join(root, stateDone, unrelated.ID, "tmp", leaseLockName)) {
		t.Fatal("observing assigned completion touched the unrelated held task")
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
			queues := []string{"queue-a", "queue-b"}
			for _, queue := range queues {
				state := stateTodo
				if tc.crashDone {
					state = stateDone
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
			a := &app{cfg: &config.Config{}}
			code, err := a.loop(repo, "missing-image", "codex", "", nil, queues, nil, nil, false, false, 0)
			if code != 1 || err == nil || !strings.Contains(err.Error(), "same-id") || !strings.Contains(err.Error(), "multiple queues") {
				t.Fatalf("duplicate loop = code %d err %v", code, err)
			}
			if tc.crashDone {
				for _, queue := range queues {
					if !pathExists(filepath.Join(repo, queue, stateInProgress, "same-id")) {
						t.Fatalf("%s crash candidate was not restored before duplicate validation", queue)
					}
				}
			}
		})
	}
}

func TestFinalizeFinishedTasks(t *testing.T) {
	root := t.TempDir()
	doneID := "2026-01-01-done"
	strandedID := "2026-01-01-stranded" // a completion whose earlier tmp cleanup failed, now retried
	liveID := "2026-01-02-live"
	writeTaskFile(t, filepath.Join(root, stateDone, doneID, "task.md"), "# done\n")
	writeTaskFile(t, filepath.Join(root, stateDone, doneID, "state.md"), "# State — done\n\n**Status:** commit next\n**Done so far:** kept summary\n**Next action:** move to done\n**Traps:** kept trap\n")
	writeTaskFile(t, filepath.Join(root, stateDone, doneID, "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(root, stateDone, strandedID, "task.md"), "# stranded\n")
	writeTaskFile(t, filepath.Join(root, stateDone, strandedID, "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, liveID, "task.md"), "# live\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, liveID, "tmp", "scratch"), "retain\n")

	// The loop sweeps EVERY done task, so a leftover tmp from a prior run's failed cleanup is
	// reclaimed on a later run even though it is not part of any fresh done delta.
	if err := finalizeFinishedTasks([]string{root}); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(root, stateDone, doneID, "tmp")) {
		t.Error("observed done task kept its tmp")
	}
	if pathExists(filepath.Join(root, stateDone, strandedID, "tmp")) {
		t.Error("a stranded done task's leftover tmp was not retried")
	}
	if !fileExists(filepath.Join(root, stateInProgress, liveID, "tmp", "scratch")) {
		t.Error("cleanup touched an unfinished task's tmp")
	}
	for _, id := range []string{doneID, strandedID} {
		state := readFileString(filepath.Join(root, stateDone, id, "state.md"))
		if !strings.Contains(state, "**Status:** complete") || !strings.Contains(state, "**Next action:** none") {
			t.Errorf("done task %s was not finalized:\n%s", id, state)
		}
	}
	state := readFileString(filepath.Join(root, stateDone, doneID, "state.md"))
	if !strings.Contains(state, "**Done so far:** kept summary") || !strings.Contains(state, "**Traps:** kept trap") {
		t.Errorf("finalization discarded agent-authored fields:\n%s", state)
	}

}

func TestReconcileInterruptedCompletions(t *testing.T) {
	newRepo := func(t *testing.T) (string, func(...string)) {
		t.Helper()
		repo := t.TempDir()
		git := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		git("init", "-q")
		git("config", "user.email", "t@t")
		git("config", "user.name", "T")
		git("commit", "-q", "--allow-empty", "-m", "base")
		return repo, git
	}
	seedDone := func(t *testing.T, repo, id string) string {
		t.Helper()
		dir := filepath.Join(repo, tasksRoot, stateDone, id)
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# task\n")
		writeTaskFile(t, filepath.Join(dir, "log.md"), "# log\n")
		writeTaskFile(t, filepath.Join(dir, "state.md"), "# state\n\n**Status:** in progress\n**Next action:** finish\n")
		writeTaskFile(t, filepath.Join(dir, "tmp", "lease.lock"), "")
		writeTaskFile(t, filepath.Join(dir, "tmp", "lease.json"), "{}\n")
		return dir
	}

	t.Run("bound completion restores for range validation", func(t *testing.T) {
		repo, git := newRepo(t)
		id := "interrupted-bound"
		seedDone(t, repo, id)
		git("commit", "-q", "--allow-empty", "-m", "done\n\nCoop-Task: "+id)
		if err := reconcileInterruptedCompletions([]string{filepath.Join(repo, tasksRoot)}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(repo, tasksRoot, stateInProgress, id)
		state := readFileString(filepath.Join(dir, "state.md"))
		if !strings.Contains(state, "**Status:** in progress") || !strings.Contains(state, "**Next action:** repair the commit binding") {
			t.Fatalf("bound interrupted completion state:\n%s", state)
		}
	})

	t.Run("unbound completion restores", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "interrupted-unbound"
		seedDone(t, repo, id)
		if err := reconcileInterruptedCompletions([]string{filepath.Join(repo, tasksRoot)}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(repo, tasksRoot, stateInProgress, id)
		if !fileExists(filepath.Join(dir, "task.md")) || pathExists(filepath.Join(repo, tasksRoot, stateDone, id)) {
			t.Fatal("unbound interrupted completion was not restored")
		}
	})

	t.Run("ordinary archive is untouched", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "ordinary-archive"
		dir := filepath.Join(repo, tasksRoot, stateDone, id)
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# task\n")
		writeTaskFile(t, filepath.Join(dir, "tmp", "artifact"), "keep\n")
		if err := reconcileInterruptedCompletions([]string{filepath.Join(repo, tasksRoot)}); err != nil {
			t.Fatal(err)
		}
		if !fileExists(filepath.Join(dir, "tmp", "artifact")) {
			t.Fatal("startup reconciliation touched an archive without lease metadata")
		}
	})

	t.Run("duplicate ids restore the exact queue", func(t *testing.T) {
		first, _ := newRepo(t)
		second, _ := newRepo(t)
		id := "same-id"
		writeTaskFile(t, filepath.Join(first, tasksRoot, stateInProgress, id, "task.md"), "# active\n")
		seedDone(t, second, id)
		hosts := []string{filepath.Join(first, tasksRoot), filepath.Join(second, tasksRoot)}
		if err := reconcileInterruptedCompletions(hosts); err != nil {
			t.Fatal(err)
		}
		if pathExists(filepath.Join(second, tasksRoot, stateDone, id)) ||
			!fileExists(filepath.Join(second, tasksRoot, stateInProgress, id, "task.md")) {
			t.Fatal("startup reconciliation restored the same id from the wrong queue")
		}
	})

	t.Run("active completion lease is untouched", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "active-completion"
		dir := seedDone(t, repo, id)
		lock, err := openLeaseLock(dir, false)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		host := filepath.Join(repo, tasksRoot)
		if err := reconcileInterruptedCompletions([]string{host}); err != nil {
			t.Fatal(err)
		}
		if err := finalizeFinishedTasks([]string{host}); err != nil {
			t.Fatal(err)
		}
		if !pathExists(filepath.Join(host, stateDone, id)) {
			t.Fatal("startup reconciliation/finalization moved a completion while its lease was held")
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		if err := reconcileInterruptedCompletions([]string{host}); err != nil {
			t.Fatal(err)
		}
		if pathExists(filepath.Join(host, stateDone, id)) || !pathExists(filepath.Join(host, stateInProgress, id)) {
			t.Fatal("released crash-left completion was not restored")
		}
	})

	t.Run("recovery lock covers rename and bookkeeping window", func(t *testing.T) {
		repo, _ := newRepo(t)
		id := "serialized-recovery"
		dir := seedDone(t, repo, id)
		host := filepath.Join(repo, tasksRoot)
		item := readTaskTree(host)[0]
		lock, current, acquired, err := lockCrashCompletion(host, item)
		if err != nil || !acquired {
			t.Fatalf("lock crash completion = acquired %v, err %v", acquired, err)
		}
		if err := moveTaskDir(host, current, stateInProgress); err != nil {
			_ = lock.release()
			t.Fatal(err)
		}
		moved := readTaskTree(host)[0]
		lease, observed, err := tryTaskLease(host, moved, testLeaseOwner())
		if err != nil || lease != nil || observed.State != leaseBusy {
			_ = lock.release()
			t.Fatalf("contender during recovery = lease %v observed %+v err %v", lease, observed, err)
		}
		if err := lock.release(); err != nil {
			t.Fatal(err)
		}
		if !fileExists(filepath.Join(dir, "tmp", leaseLockName)) && !fileExists(filepath.Join(moved.Dir, "tmp", leaseLockName)) {
			t.Fatal("recovery lock inode disappeared")
		}
	})
}

func TestFinalizeFinishedTasksCleanupFailureRestoresActionableState(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-cleanup-obstructed"
	doneDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# done\n")
	writeTaskFile(t, filepath.Join(doneDir, "state.md"), "# State\n\n**Status:** complete\n**Done so far:** implementation complete\n**Next action:** none\n**Traps:** cleanup must succeed\n")
	writeTaskFile(t, filepath.Join(doneDir, "tmp", "scratch"), "retain\n")
	oldCleaner := taskTmpCleaner
	taskTmpCleaner = func(string) error { return errors.New("loop cleanup failed") }
	t.Cleanup(func() { taskTmpCleaner = oldCleaner })

	if err := finalizeFinishedTasks([]string{root}); err == nil || !strings.Contains(err.Error(), "loop cleanup failed") {
		t.Fatalf("loop cleanup failure = %v, want propagated error", err)
	}
	restored := filepath.Join(root, stateInProgress, id)
	if !fileExists(filepath.Join(restored, "tmp", "scratch")) {
		t.Fatal("cleanup failure did not restore the task with diagnostic scratch")
	}
	state := readFileString(filepath.Join(restored, "state.md"))
	for _, want := range []string{"**Status:** in progress — finalization failed", "**Done so far:** implementation complete", "**Next action:** fix the task metadata or cleanup obstruction", "**Traps:** cleanup must succeed"} {
		if !strings.Contains(state, want) {
			t.Errorf("restored cleanup state missing %q:\n%s", want, state)
		}
	}
}

func TestFinalizeFinishedTasksStateFailureIsRetryable(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-state-obstructed"
	taskDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(taskDir, "task.md"), "# done\n")
	writeTaskFile(t, filepath.Join(taskDir, "tmp", "scratch"), "retain\n")
	if err := os.MkdirAll(filepath.Join(taskDir, "state.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := finalizeFinishedTasks([]string{root}); err == nil || !strings.Contains(err.Error(), "state finalization failed") {
		t.Fatalf("loop finalization state failure = %v, want propagated error", err)
	}
	taskDir = filepath.Join(root, stateInProgress, id)
	if !fileExists(filepath.Join(taskDir, "tmp", "scratch")) {
		t.Fatal("loop state failure did not restore the actionable task with its tmp")
	}
	if err := os.RemoveAll(filepath.Join(taskDir, "state.md")); err != nil {
		t.Fatal(err)
	}
	doneDir := filepath.Join(root, stateDone, id)
	if err := os.Rename(taskDir, doneDir); err != nil {
		t.Fatal(err)
	}
	if err := finalizeFinishedTasks([]string{root}); err != nil {
		t.Fatalf("loop finalization retry: %v", err)
	}
	if pathExists(filepath.Join(doneDir, "tmp")) {
		t.Fatal("loop finalization retry left tmp")
	}
	state := readFileString(filepath.Join(doneDir, "state.md"))
	if !strings.Contains(state, "**Status:** complete") || !strings.Contains(state, "**Next action:** none") {
		t.Errorf("loop finalization retry did not create safe state:\n%s", state)
	}
}

func TestReopenedBySignoffUsesExactDoneDelta(t *testing.T) {
	before := map[string]string{
		"review-a": stateDone, "review-b": stateDone,
		"unrelated-doing": stateInProgress, "unrelated-todo": stateTodo,
	}
	after := map[string]string{
		"review-a": stateInProgress, "review-b": stateTodo,
		"unrelated-doing": stateInProgress, "unrelated-todo": stateTodo,
	}
	if got, want := reopenedBySignoff(before, after), []string{"review-a", "review-b"}; !slices.Equal(got, want) {
		t.Errorf("reopenedBySignoff = %v, want exact sorted delta %v", got, want)
	}
}

func TestResumeLine(t *testing.T) {
	// No landed commit → empty (blind-resume path stays byte-identical).
	if resumeLine("x", nil) != "" {
		t.Error("no commits should yield no resume line")
	}
	// A landed commit → a line that names the sha and BOTH cases (finish-the-move vs reopened-rework),
	// so it never falsely asserts the task is done.
	l := resumeLine("my-task", []string{"abc123"})
	for _, want := range []string{"my-task", "abc123", "log.md", "REOPENED", "Coop-Recovery", "finish the move"} {
		if !strings.Contains(l, want) {
			t.Errorf("resume line missing %q:\n%s", want, l)
		}
	}
}

func TestAssignLoopTaskSelectionAndClaim(t *testing.T) {
	q1 := filepath.Join(t.TempDir(), ".agent", "tasks")
	q2 := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q1, stateTodo, "todo-first", "task.md"), "# Todo first\n")
	writeTaskFile(t, filepath.Join(q2, stateInProgress, "resume", "task.md"), "# Resume me\n")

	assignment, err := assignLoopTask([]string{q1, q2}, testLeaseOwner())
	if err != nil || assignment.Outcome != assignmentReady {
		t.Fatalf("assignLoopTask resume = %+v, err %v", assignment, err)
	}
	defer assignment.Lease.release()
	if !assignment.Lease.legacy {
		t.Error("a legacy in-progress task with no lock should be marked as an adoption")
	}
	c, got := assignment.Counts, assignment.Task
	if got.Item.ID != "resume" || got.Root != q2 || got.Item.State != stateInProgress {
		t.Fatalf("assignLoopTask chose %+v, want the later queue's in_progress task", got)
	}
	if c.Todo != 1 || c.Doing != 1 {
		t.Fatalf("resume counts = %+v, want Todo=1 Doing=1", c)
	}
	if !pathExists(filepath.Join(q1, stateTodo, "todo-first")) {
		t.Fatal("selecting an interrupted task must not claim a different todo")
	}
}

func TestAssignLoopTaskClaimsBeforeReturningAndCanBlock(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateTodo, "b-task", "task.md"), "# B\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "a-task", "task.md"), "# A\n")

	assignment, err := assignLoopTask([]string{q}, testLeaseOwner())
	if err != nil || assignment.Outcome != assignmentReady {
		t.Fatalf("assignLoopTask = %+v, err %v", assignment, err)
	}
	c, got := assignment.Counts, assignment.Task
	if got.Item.ID != "a-task" || got.Item.State != stateInProgress {
		t.Fatalf("assignment = %+v, want first sorted todo claimed in_progress", got)
	}
	if c.Todo != 1 || c.Doing != 1 {
		t.Fatalf("post-claim counts = %+v, want Todo=1 Doing=1", c)
	}
	if pathExists(filepath.Join(q, stateTodo, "a-task")) || !pathExists(got.Item.Dir) {
		t.Fatal("assignment returned before the host-side todo to in_progress move was observable")
	}
	if _, active := queueProgress([]string{q}); active != got.Item.Title {
		t.Fatalf("banner active title = %q, assigned title = %q", active, got.Item.Title)
	}

	writeTaskFile(t, filepath.Join(got.Item.Dir, "decision.md"), "# Decision\n")
	if err := moveTaskDir(q, got.Item, stateBlocked); err != nil {
		t.Fatalf("assigned task should remain movable to blocked: %v", err)
	}
	if !pathExists(filepath.Join(q, stateBlocked, "a-task")) {
		t.Fatal("assigned task did not bounce to blocked")
	}
	if err := assignment.Lease.release(); err != nil {
		t.Fatalf("release moved task lease: %v", err)
	}
}

func TestAssignLoopTaskEmptyIsNoOp(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateDone, "done", "task.md"), "# Done\n")
	assignment, err := assignLoopTask([]string{q}, testLeaseOwner())
	if err != nil || assignment.Outcome != assignmentDrained {
		t.Fatalf("empty actionable queue = %+v, err %v", assignment, err)
	}
	c := assignment.Counts
	if c.Done != 1 || c.Todo+c.Doing != 0 {
		t.Fatalf("empty actionable counts = %+v", c)
	}
}

func TestAssignLoopTaskOnlyNeverSwitchesTasks(t *testing.T) {
	root := t.TempDir()
	targetID := "2026-01-01-target"
	otherID := "2026-01-01-other"
	writeTaskFile(t, filepath.Join(root, stateTodo, targetID, "task.md"), "# Target\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, otherID, "task.md"), "# Other\n")

	assignment, err := assignLoopTaskOnly([]string{root}, testLeaseOwner(), targetID)
	if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != targetID {
		t.Fatalf("scoped assignment = (%+v, %v), want target task", assignment, err)
	}
	if err := assignment.Lease.release(); err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, targetID)) {
		t.Fatal("scoped todo task was not claimed")
	}

	target := taskItem{ID: targetID, State: stateInProgress, Dir: filepath.Join(root, stateInProgress, targetID)}
	if err := moveTaskDir(root, target, stateDone); err != nil {
		t.Fatal(err)
	}
	settled, err := assignLoopTaskOnly([]string{root}, testLeaseOwner(), targetID)
	if err != nil || settled.Outcome != assignmentDrained {
		t.Fatalf("settled scoped assignment = (%+v, %v), want drained", settled, err)
	}
	if !pathExists(filepath.Join(root, stateInProgress, otherID)) {
		t.Fatal("task-limited assignment touched another in-progress task")
	}
}

// TestCommitsForTaskAndUntrailered drives the real git trailer parser. Fresh work binds only in its
// commit range; unchanged HEAD, malformed, duplicate, different-id, and substring values fail closed.
func TestCommitsForTaskAndUntrailered(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "did the work\n\nCoop-Task: task-42")
	head := gitOut(repo, "rev-parse", "HEAD")

	if c := commitsForTask(repo, "", "task-42"); len(c) != 1 {
		t.Errorf("commitsForTask(task-42) = %v, want 1", c)
	}
	if c := commitsForTask(repo, "", "task-99"); len(c) != 0 {
		t.Errorf("commitsForTask(task-99) = %v, want none", c)
	}
	// A finished task WITH a trailer commit in range is bindable (not untrailered); one WITHOUT is.
	if m := untrailered(repo, base, head, []string{"task-42"}); len(m) != 0 {
		t.Errorf("task-42 is trailered in range, should not be flagged: %v", m)
	}
	if m := untrailered(repo, base, head, []string{"task-42", "task-99"}); len(m) != 1 || m[0] != "task-99" {
		t.Errorf("untrailered = %v, want [task-99]", m)
	}
	bound := queuedTask{Root: "/first", Item: taskItem{ID: "task-42", State: stateDone}}
	if unbindableQueuedCompletion(repo, base, head, bound) {
		t.Error("valid assigned completion was rejected")
	}
	if !unbindableQueuedCompletion(repo, head, head, bound) {
		t.Error("unchanged-HEAD assigned completion was accepted")
	}
	// No-HEAD-change work must fail closed even if an old exact trailer is reachable. Crash-left
	// completion recovery restores the task and requires a new range-bound amend/recommit.
	if m := untrailered(repo, head, head, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unchanged HEAD used historical task binding: %v", m)
	}
	if m := untrailered(repo, head, head, []string{"task-4", "task"}); len(m) != 2 || m[0] != "task-4" || m[1] != "task" {
		t.Errorf("different ids and substrings must remain untrailered, got %v", m)
	}
	if m := untrailered(repo, "", head, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unknown iteration base must fail closed, got %v", m)
	}
	if m := untrailered(repo, head, "", []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("unknown iteration head must fail closed, got %v", m)
	}

	// Once HEAD changes, an older valid trailer cannot bless fresh unbound work.
	git("commit", "-q", "--allow-empty", "-m", "fresh rework without a trailer")
	unboundHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, head, unboundHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("historical-only binding after fresh work = %v, want [task-42]", m)
	}

	// A trailer-like line outside Git's final contiguous trailer block is not a trailer.
	git("commit", "-q", "--allow-empty", "-m", "malformed\n\nCoop-Task: task-42\n\nCo-authored-by: T <t@t>")
	malformedHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, unboundHead, malformedHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("malformed trailer binding = %v, want [task-42]", m)
	}

	// Multiple Coop-Task values are ambiguous even when both values happen to match.
	git("commit", "-q", "--allow-empty", "-m", "duplicate\n\nCoop-Task: task-42\nCoop-Task: task-42")
	duplicateHead := gitOut(repo, "rev-parse", "HEAD")
	if c := commitsForTask(repo, malformedHead+".."+duplicateHead, "task-42"); len(c) != 0 {
		t.Errorf("duplicate trailers must not bind, got commits %v", c)
	}
	if m := untrailered(repo, malformedHead, duplicateHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("duplicate trailer binding = %v, want [task-42]", m)
	}

	git("commit", "-q", "--allow-empty", "-m", "valid again\n\nCoop-Task: task-42")
	validHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, duplicateHead, validHead, []string{"task-42"}); len(m) != 0 {
		t.Errorf("single exact trailer should bind fresh work: %v", m)
	}
	// Two individually valid commits for one task are still ambiguous: one task must bind to one
	// commit in the iteration range, not merely find at least one matching trailer somewhere in it.
	git("commit", "-q", "--allow-empty", "-m", "second valid binding\n\nCoop-Task: task-42")
	twoBindingsHead := gitOut(repo, "rev-parse", "HEAD")
	if m := untrailered(repo, duplicateHead, twoBindingsHead, []string{"task-42"}); !slices.Equal(m, []string{"task-42"}) {
		t.Errorf("multiple matching commits must fail closed, got %v", m)
	}
	// landedTasks sees the trailer across all history.
	if !landedTasks(repo)["task-42"] {
		t.Error("landedTasks should include task-42")
	}
	git("commit", "-q", "--allow-empty", "-m", "ambiguous landed\n\nCoop-Task: duplicate-landed\nCoop-Task: duplicate-landed")
	if landedTasks(repo)["duplicate-landed"] {
		t.Error("landedTasks accepted a commit with duplicate Coop-Task trailers")
	}
}

func TestRestoreUnbindableCompletions(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-unbound"
	doneDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# Unbound\n")
	writeTaskFile(t, filepath.Join(doneDir, "log.md"), "# Log\n")

	item := readTaskTree(root)[0]
	if err := restoreQueuedCompletions([]queuedTask{{Root: root, Item: item}}); err != nil {
		t.Fatalf("restoreQueuedCompletions: %v", err)
	}
	inProgressDir := filepath.Join(root, stateInProgress, id)
	if !pathExists(inProgressDir) || pathExists(doneDir) {
		t.Fatalf("rejected completion was not restored: in_progress=%v done=%v", pathExists(inProgressDir), pathExists(doneDir))
	}
	log, err := os.ReadFile(filepath.Join(inProgressDir, "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"completion rejected", "expected exactly one commit", "git commit --amend --no-edit --trailer", "rewrite or squash", id} {
		if !strings.Contains(string(log), want) {
			t.Errorf("rejection log missing %q:\n%s", want, log)
		}
	}
	state, err := os.ReadFile(filepath.Join(inProgressDir, "state.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"**Status:** in progress", "completion rejected", "**Next action:** repair the commit binding"} {
		if !strings.Contains(string(state), want) {
			t.Errorf("rejection state missing %q:\n%s", want, state)
		}
	}

	rejectErr := unbindableCompletionError([]string{id}, nil)
	if rejectErr == nil {
		t.Fatal("unbindable completion must stop the controller")
	}
	for _, want := range []string{"completion rejected", "restored to in_progress", "git commit --amend --no-edit --trailer", "rewrite/squash", id} {
		if !strings.Contains(rejectErr.Error(), want) {
			t.Errorf("controller error missing %q: %v", want, rejectErr)
		}
	}
}

func TestAppendTaskLogStrictRejectsSymlinkedLog(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside-log")
	want := "outside log sentinel\n"
	if err := os.WriteFile(outside, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	taskDir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(taskDir, "log.md")); err != nil {
		t.Fatal(err)
	}
	if err := appendTaskLogStrict(taskDir, "must stay contained"); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
		t.Fatalf("symlinked log error = %v", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != want {
		t.Fatalf("outside log changed to %q, %v", data, err)
	}
}

func TestIsGateGuardPath(t *testing.T) {
	guarded := []string{"Makefile", "sub/Makefile", ".agent/project.yaml", ".agent/loop.yaml",
		".agent/skills/sweep/SKILL.md", ".agent/skills/sweep/queue-guard.sh",
		".claude/skills/workflow-sweep/queue-guard.sh",
		".claude/settings.json", ".claude/hooks/commit-gate.sh", ".github/workflows/ci.yml"}
	for _, f := range guarded {
		if !isGateGuardPath(f) {
			t.Errorf("%q should be gate-defining", f)
		}
	}
	// Ordinary source and test files are NOT gate-defining — only the checker's own definition is.
	for _, f := range []string{"internal/cli/sign.go", "internal/cli/sign_test.go", "README.md", "docs/cli.md",
		".claude/skills/workflow-sweep/helper.sh", ".claude/skills/workflow-sweep/queue-guard.sh.bak"} {
		if isGateGuardPath(f) {
			t.Errorf("%q should NOT be gate-defining (only the gate's own definition is)", f)
		}
	}
}

func TestProtectedGateChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"), "GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(p, s string) {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	write("code.go", "package x")
	write(".claude/skills/workflow-sweep/queue-guard.sh", "#!/bin/sh\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base := gitOut(repo, "rev-parse", "HEAD")
	// A commit that touches ordinary code → no protected change.
	write("code.go", "package x // edit")
	git("add", "-A")
	git("commit", "-q", "-m", "code edit")
	if hits := protectedGateChanges(repo, base, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 0 {
		t.Errorf("an ordinary code change is not protected: %v", hits)
	}
	// A commit that weakens the Makefile → flagged.
	mid := gitOut(repo, "rev-parse", "HEAD")
	write("Makefile", "check:\n\ttrue\n")
	git("add", "-A")
	git("commit", "-q", "-m", "loosen the gate")
	if hits := protectedGateChanges(repo, mid, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 1 || hits[0] != "Makefile" {
		t.Errorf("a Makefile change should be flagged, got %v", hits)
	}
	// Renaming a guard away must report the deleted protected path, not only its new name.
	renameBase := gitOut(repo, "rev-parse", "HEAD")
	git("mv", ".claude/skills/workflow-sweep/queue-guard.sh", ".claude/skills/workflow-sweep/disabled.sh")
	git("commit", "-q", "-m", "disable the adopted guard")
	if hits := protectedGateChanges(repo, renameBase, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 1 || hits[0] != ".claude/skills/workflow-sweep/queue-guard.sh" {
		t.Errorf("renaming an adopted guard should flag its old path, got %v", hits)
	}
	// NUL-delimited names prevent Git from quoting paths before basename matching.
	unicodeGuard := "\u00e9/queue-guard.sh"
	unicodeBase := gitOut(repo, "rev-parse", "HEAD")
	write(unicodeGuard, "#!/bin/sh\n")
	git("add", "-A")
	git("commit", "-q", "-m", "add guard below unicode directory")
	if hits := protectedGateChanges(repo, unicodeBase, gitOut(repo, "rev-parse", "HEAD")); len(hits) != 1 || hits[0] != unicodeGuard {
		t.Errorf("a protected basename below a unicode directory should be flagged, got %v", hits)
	}
}

func TestProtectedGateFiles(t *testing.T) {
	got := protectedGateFiles([]string{
		"internal/cli/commands.go", ".claude/settings.json", "Makefile", "Makefile", " .agent/skills/sweep/SKILL.md ",
	})
	want := []string{".agent/skills/sweep/SKILL.md", ".claude/settings.json", "Makefile"}
	if !slices.Equal(got, want) {
		t.Errorf("protectedGateFiles = %v, want %v", got, want)
	}
}

// TestReconcileQueueAfterMerge: a queued task whose Coop-Task trailer just landed moves to done;
// a blocked task with a landed trailer is NOT moved (flagged for a human); an unlanded task stays.
func TestReconcileQueueAfterMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "g"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "s"))
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	q := filepath.Join(repo, tasksRoot)
	q2Rel := filepath.Join(".agent", "other-tasks")
	q2 := filepath.Join(repo, q2Rel)
	writeTaskFile(t, filepath.Join(q, stateTodo, "todo1", "task.md"), "# todo1\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "todo1", "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "task.md"), "# wip1\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "task.md"), "# blk1\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "decision.md"), "# blocked\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "tmp", "scratch"), "retain\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "safe", "task.md"), "# safe\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "same-id", "task.md"), "# same root\n")
	writeTaskFile(t, filepath.Join(q2, stateTodo, "same-id", "task.md"), "# same second queue\n")
	git("init", "-q")
	git("config", "user.email", "t@t")
	git("config", "user.name", "T")
	// A landed commit for todo1, wip1, and blk1 (as a merged fork would carry); "safe" did not land.
	if err := os.WriteFile(filepath.Join(repo, "code.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "seed queue")
	git("commit", "-q", "--allow-empty", "-m", "todo1 work\n\nCoop-Task: todo1")
	git("commit", "-q", "--allow-empty", "-m", "wip1 work\n\nCoop-Task: wip1")
	git("commit", "-q", "--allow-empty", "-m", "blk1 work\n\nCoop-Task: blk1")
	git("commit", "-q", "--allow-empty", "-m", "ambiguous work\n\nCoop-Task: same-id")

	a := &app{cfg: &config.Config{TasksFiles: []string{tasksRoot, q2Rel}}}
	a.reconcileQueueAfterMerge(repo, "fork1")

	if !pathExists(filepath.Join(q, stateDone, "todo1")) || pathExists(filepath.Join(q, stateTodo, "todo1")) {
		t.Error("a landed todo task should have moved to done")
	}
	if !pathExists(filepath.Join(q, stateDone, "wip1")) {
		t.Error("a landed in_progress task should have moved to done")
	}
	if !pathExists(filepath.Join(q, stateBlocked, "blk1")) || pathExists(filepath.Join(q, stateDone, "blk1")) {
		t.Error("a blocked task must be flagged, never auto-moved")
	}
	if !pathExists(filepath.Join(q, stateTodo, "safe")) {
		t.Error("an unlanded task must stay put")
	}
	if !pathExists(filepath.Join(q, stateTodo, "same-id")) || !pathExists(filepath.Join(q2, stateTodo, "same-id")) {
		t.Error("an ambiguous landed id must be skipped in every queue")
	}
	if pathExists(filepath.Join(q, stateDone, "todo1", "tmp")) || pathExists(filepath.Join(q, stateDone, "wip1", "tmp")) {
		t.Error("fork reconciliation must clean completed task tmp")
	}
	for _, id := range []string{"todo1", "wip1"} {
		state := readFileString(filepath.Join(q, stateDone, id, "state.md"))
		if !strings.Contains(state, "**Status:** complete") || !strings.Contains(state, "**Next action:** none") {
			t.Errorf("fork reconciliation did not finalize %s state:\n%s", id, state)
		}
	}
	if !fileExists(filepath.Join(q, stateBlocked, "blk1", "tmp", "scratch")) {
		t.Error("fork reconciliation must retain blocked task tmp")
	}
	// The reconciled task got a note in its log.md.
	if data, _ := os.ReadFile(filepath.Join(q, stateDone, "todo1", "log.md")); !strings.Contains(string(data), "reconciled: landed by fork fork1") {
		t.Errorf("reconcile note missing from todo1 log.md: %q", data)
	}
}

// TestUnblockResolved: the host-side preflight returns a blocked task to todo only when its
// decision.md carries a filled-in Resolution by the SAME bar `coop tasks unblock` applies
// (decisionResolved) — the untouched stub, a missing decision.md, and a free-form file with no
// **Resolution:** marker all stay parked (parse-or-park: never act on a format we can't read).
func TestUnblockResolved(t *testing.T) {
	root := t.TempDir()
	mk := func(id, decision string) {
		dir := filepath.Join(root, stateBlocked, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("# "+id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if decision != "" {
			if err := os.WriteFile(filepath.Join(dir, "decision.md"), []byte(decision), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("answered", "# Decision\n\n**Resolution:** ship it as designed.\n")
	mk("stub", "# Decision\n\n**Resolution:** <!-- HUMAN: your answer here, then: coop tasks unblock stub -->\n")
	mk("no-decision", "")
	mk("freeform", "we talked and agreed to do X\n") // no **Resolution:** marker

	ids := unblockResolved([]string{root})
	if len(ids) != 1 || ids[0] != "answered" {
		t.Fatalf("unblockResolved = %v, want [answered]", ids)
	}
	// The answered task moved to todo and its log records why; the rest stayed parked.
	if !pathExists(filepath.Join(root, stateTodo, "answered")) {
		t.Error("answered task should have moved to todo")
	}
	if data, _ := os.ReadFile(filepath.Join(root, stateTodo, "answered", "log.md")); !strings.Contains(string(data), "unblocked") {
		t.Errorf("unblock note missing from log.md: %q", data)
	}
	for _, id := range []string{"stub", "no-decision", "freeform"} {
		if !pathExists(filepath.Join(root, stateBlocked, id)) {
			t.Errorf("%s should have stayed blocked", id)
		}
	}
}
