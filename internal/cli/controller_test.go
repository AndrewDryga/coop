package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/config"
)

func TestFinishedTasksAndReconcileDecision(t *testing.T) {
	before := map[string]string{"a": stateInProgress, "b": stateTodo, "c": stateDone}
	after := map[string]string{"a": stateDone, "b": stateTodo, "c": stateDone}
	if got := finishedTasks(before, after); len(got) != 1 || got[0] != "a" {
		t.Errorf("finishedTasks = %v, want [a]", got)
	}
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

	oldCleaner := taskTmpCleaner
	taskTmpCleaner = func(string) error { return errors.New("loop cleanup failed") }
	t.Cleanup(func() { taskTmpCleaner = oldCleaner })
	if err := finalizeFinishedTasks([]string{root}); err == nil || !strings.Contains(err.Error(), "loop cleanup failed") {
		t.Errorf("loop cleanup failure = %v, want propagated error", err)
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
	if !fileExists(filepath.Join(taskDir, "tmp", "scratch")) {
		t.Fatal("loop state failure removed tmp before metadata was safe")
	}
	if err := os.RemoveAll(filepath.Join(taskDir, "state.md")); err != nil {
		t.Fatal(err)
	}
	if err := finalizeFinishedTasks([]string{root}); err != nil {
		t.Fatalf("loop finalization retry: %v", err)
	}
	if pathExists(filepath.Join(taskDir, "tmp")) {
		t.Fatal("loop finalization retry left tmp")
	}
	state := readFileString(filepath.Join(taskDir, "state.md"))
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
	for _, want := range []string{"my-task", "abc123", "log.md", "REOPENED", "finish the move"} {
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
		t.Fatal("one-task mode touched another in-progress task")
	}
}

// TestCommitsForTaskAndUntrailered drives the real git trailer parser. Fresh work binds only in its
// commit range; historical fallback is limited to unchanged-HEAD reconciliation; malformed,
// duplicate, different-id, and substring values fail closed.
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
	// A case-(a) resume may only move the folder after its trailer commit already landed, leaving
	// HEAD unchanged. Reachable history still binds that completion, but only by exact trailer id.
	if m := untrailered(repo, head, head, []string{"task-42"}); len(m) != 0 {
		t.Errorf("historically trailered task-42 should bind without HEAD movement: %v", m)
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
}

func TestRestoreUnbindableCompletions(t *testing.T) {
	root := t.TempDir()
	id := "2026-01-01-unbound"
	doneDir := filepath.Join(root, stateDone, id)
	writeTaskFile(t, filepath.Join(doneDir, "task.md"), "# Unbound\n")
	writeTaskFile(t, filepath.Join(doneDir, "log.md"), "# Log\n")

	if err := restoreUnbindableCompletions([]string{root}, []string{id}); err != nil {
		t.Fatalf("restoreUnbindableCompletions: %v", err)
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

func TestIsGateGuardPath(t *testing.T) {
	guarded := []string{"Makefile", "sub/Makefile", ".agent/project.yaml", ".agent/loop.yaml",
		".agent/skills/sweep/SKILL.md", ".agent/skills/sweep/queue-guard.sh",
		".claude/settings.json", ".claude/hooks/commit-gate.sh", ".github/workflows/ci.yml"}
	for _, f := range guarded {
		if !isGateGuardPath(f) {
			t.Errorf("%q should be gate-defining", f)
		}
	}
	// Ordinary source and test files are NOT gate-defining — only the checker's own definition is.
	for _, f := range []string{"internal/cli/sign.go", "internal/cli/sign_test.go", "README.md", "docs/cli.md"} {
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
	writeTaskFile(t, filepath.Join(q, stateTodo, "todo1", "task.md"), "# todo1\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "todo1", "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "task.md"), "# wip1\n")
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "tmp", "scratch"), "remove\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "task.md"), "# blk1\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "decision.md"), "# blocked\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "tmp", "scratch"), "retain\n")
	writeTaskFile(t, filepath.Join(q, stateTodo, "safe", "task.md"), "# safe\n")
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

	a := &app{cfg: &config.Config{}}
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
