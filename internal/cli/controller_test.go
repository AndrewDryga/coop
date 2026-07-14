package cli

import (
	"os"
	"os/exec"
	"path/filepath"
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

	c, got, ok, err := assignLoopTask([]string{q1, q2})
	if err != nil || !ok {
		t.Fatalf("assignLoopTask resume = ok %v, err %v", ok, err)
	}
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

	c, got, ok, err := assignLoopTask([]string{q})
	if err != nil || !ok {
		t.Fatalf("assignLoopTask = ok %v, err %v", ok, err)
	}
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
}

func TestAssignLoopTaskEmptyIsNoOp(t *testing.T) {
	q := filepath.Join(t.TempDir(), ".agent", "tasks")
	writeTaskFile(t, filepath.Join(q, stateDone, "done", "task.md"), "# Done\n")
	c, _, ok, err := assignLoopTask([]string{q})
	if err != nil || ok {
		t.Fatalf("empty actionable queue = ok %v, err %v", ok, err)
	}
	if c.Done != 1 || c.Todo+c.Doing != 0 {
		t.Fatalf("empty actionable counts = %+v", c)
	}
}

// TestCommitsForTaskAndUntrailered drives the real git trailer read: a commit tagged Coop-Task is
// found by id; the untrailered check flags a finished task whose range has no such commit.
func TestCommitsForTaskAndUntrailered(t *testing.T) {
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
	// landedTasks sees the trailer across all history.
	if !landedTasks(repo)["task-42"] {
		t.Error("landedTasks should include task-42")
	}
}

func TestIsGateGuardPath(t *testing.T) {
	guarded := []string{"Makefile", "sub/Makefile", ".agent/project.yaml", ".agent/loop.yaml",
		".agent/skills/sweep/SKILL.md", ".agent/skills/sweep/queue-guard.sh",
		".claude/settings.json", ".github/workflows/ci.yml"}
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
	writeTaskFile(t, filepath.Join(q, stateInProgress, "wip1", "task.md"), "# wip1\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "task.md"), "# blk1\n")
	writeTaskFile(t, filepath.Join(q, stateBlocked, "blk1", "decision.md"), "# blocked\n")
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
