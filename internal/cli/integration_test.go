package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// End-to-end tests for the folder task system: they drive the real `coop tasks` dispatcher
// and the shared readers across the full feature set (lifecycle, ordered dirs, remove,
// multiple queues, splitting) and assert the cross-cutting invariants the unit tests don't:
// the on-disk dirs are the numeric-prefixed ones, they sort in lifecycle order, and a finished
// task is MOVED (never deleted) by any automated path.

// tasksRoot is cli's own alias for tasks.TasksRoot (".agent/tasks") — kept short since cli's own
// tests reach for it constantly; internal/tasks keeps the canonical, exported constant.
const tasksRoot = tasks.TasksRoot

// Test-only compatibility aliases: before the extraction, every cli test in this package could
// reach the folder-task/lease/audit primitives directly (same package). These pure re-exports (a
// type alias, a handful of package-level func values) let that huge existing test surface keep
// calling the pre-move short names instead of hand-qualifying hundreds of call sites across a
// dozen files — production code always uses the tasks.X qualified form; only tests use these.
const (
	stateTodo       = tasks.StateTodo
	stateInProgress = tasks.StateInProgress
	stateBlocked    = tasks.StateBlocked
	stateDone       = tasks.StateDone
)

var taskStates = tasks.TaskStates

type taskItem = tasks.Item

var (
	readTaskTree          = tasks.ReadTaskTree
	isTaskDir             = tasks.IsTaskDir
	moveTaskDir           = tasks.MoveTaskDir
	currentTask           = tasks.CurrentTask
	queueProgress         = tasks.QueueProgress
	stateLabel            = tasks.StateLabel
	stateOrder            = tasks.StateOrder
	findTask              = tasks.FindTask
	tryTaskLease          = tasks.TryTaskLease
	completeTrustedTask   = tasks.CompleteTrustedTask
	taskTreeCounts        = tasks.TaskTreeCounts
	cmdTasksFolder        = tasks.CmdTasksFolder
	readAuditReopenRecord = tasks.ReadAuditReopenRecord
	openLeaseAuthority    = tasks.OpenLeaseAuthority
)

var tasksVerbs = tasks.TasksVerbs

// readFileString is a trivial, self-contained leaf helper — internal/tasks keeps its own copy (see
// its git.go for why: the same "local-redeclare" shape as gitOut).
func readFileString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// taskForLease and testLeaseOwner mirror internal/tasks's own (unexported, test-only) helpers of
// the same name — a fresh task folder and a fixed test lease owner, respectively.
func taskForLease(t *testing.T, root, state, id string) tasks.Item {
	t.Helper()
	writeTaskFile(t, filepath.Join(root, state, id, "task.md"), "# "+id+"\n")
	item, ok := currentTask(root, id)
	if !ok {
		t.Fatalf("could not read task %s", id)
	}
	return item
}

func testLeaseOwner() tasks.TaskLeaseOwner {
	return tasks.TaskLeaseOwner{
		RunID: "test-run", PID: 4242, Provider: "codex", Target: "codex:gpt-test@work",
		Now: func() time.Time { return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC) },
	}
}

// appFor builds an app rooted at repo with the default single .agent/tasks queue. A
// RepoOverride short-circuits git detection, so a plain temp dir works.
func appFor(repo string) *app {
	return &app{cfg: &config.Config{RepoOverride: repo, TasksFiles: []string{tasksRoot}}}
}

func appForDerivedQueues(repo string) *app {
	return &app{cfg: &config.Config{RepoOverride: repo}}
}

// writeTaskFile creates path (making its parent dirs) with content — the cli-side equivalent of
// internal/tasks's own (unexported, test-only) writeTaskFile, used to seed a task folder as a
// fixture without going through the real `coop tasks add` dispatcher.
func writeTaskFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeUmbrellaProject(t *testing.T, repo string, subs ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("subprojects:\n")
	for _, sub := range subs {
		fmt.Fprintf(&b, "  - %s\n", sub)
	}
	if err := os.WriteFile(filepath.Join(repo, ".agent", "project.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureStdout returns whatever fn writes to os.Stdout (list/decisions print there;
// ui.Info goes to stderr). Colors are off under `go test` (no tty), so output is plain.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// captureStderr returns whatever fn writes to os.Stderr (ui.Info/ui.Warn go there).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// TestStateDirOrderingInvariant pins the whole point of the numeric prefix: a plain lexical
// sort of the state dir names (what `ls .agent/tasks` does) must equal the lifecycle order,
// done must sort last, and stateLabel must strip the prefix back to the clean display name.
func TestStateDirOrderingInvariant(t *testing.T) {
	sorted := append([]string(nil), taskStates...)
	sort.Strings(sorted)
	for i := range taskStates {
		if sorted[i] != taskStates[i] {
			t.Fatalf("state dirs don't sort in lifecycle order:\n sorted   = %v\n lifecycle= %v", sorted, taskStates)
		}
	}
	// done uses the highest prefix (99_) so it always sorts last.
	if taskStates[len(taskStates)-1] != stateDone || stateDone != "99_done" {
		t.Errorf("done (%q) must sort last", stateDone)
	}
	want := map[string]string{stateTodo: "todo", stateInProgress: "in_progress", stateBlocked: "blocked", stateDone: "done"}
	for st, lbl := range want {
		if got := stateLabel(st); got != lbl {
			t.Errorf("stateLabel(%q) = %q, want %q", st, got, lbl)
		}
		if !strings.Contains(st, "_") {
			t.Errorf("state %q is missing its sort prefix", st)
		}
	}
	for i, st := range taskStates {
		if stateOrder(st) != i {
			t.Errorf("stateOrder(%q) = %d, want %d", st, stateOrder(st), i)
		}
	}
}

// TestIntegrationLifecycleViaDispatcher walks add → claim → done → remove through the real
// (*app).cmdTasks entry point, asserting each step lands the folder under the PREFIXED state
// dir (never a bare one) and that `done` MOVES the task (it survives) while only `remove`
// deletes it.
func TestIntegrationLifecycleViaDispatcher(t *testing.T) {
	repo := t.TempDir()
	a := appFor(repo)
	root := filepath.Join(repo, tasksRoot)

	// add bootstraps the queue on demand (no `coop init` needed) and lands in 00_todo.
	if code, err := a.cmdTasks([]string{"add", "wire the portal auth callback"}); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	if !isTaskDir(filepath.Join(root, "00_todo")) {
		t.Fatal("add did not create the prefixed 00_todo/ dir")
	}
	items := readTaskTree(root)
	if len(items) != 1 || items[0].State != stateTodo {
		t.Fatalf("after add: %+v", items)
	}
	id := items[0].ID

	if code, err := a.cmdTasks([]string{"claim", id}); code != 0 || err != nil {
		t.Fatalf("claim: code=%d err=%v", code, err)
	}
	if !isTaskDir(filepath.Join(root, "10_in_progress", id)) {
		t.Error("claim did not move the folder to 10_in_progress/")
	}

	if code, err := a.cmdTasks([]string{"done", id}); code != 0 || err != nil {
		t.Fatalf("done: code=%d err=%v", code, err)
	}
	if !isTaskDir(filepath.Join(root, "99_done", id)) {
		t.Error("done did not move the folder to 99_done/")
	}
	// move-don't-delete: done leaves the task on disk (the shipped record).
	if got := readTaskTree(root); len(got) != 1 || got[0].State != stateDone {
		t.Fatalf("done should MOVE, not delete: tree=%+v", got)
	}

	// rm --all-done is the only thing that deletes it (--yes skips the gate in this non-TTY test).
	if code, err := a.cmdTasks([]string{"rm", "--all-done", "--yes"}); code != 0 || err != nil {
		t.Fatalf("rm --all-done: code=%d err=%v", code, err)
	}
	if len(readTaskTree(root)) != 0 {
		t.Error("rm --all-done left a done task behind")
	}
	// No bare (unprefixed) state dir was ever created.
	for _, bare := range []string{"todo", "in_progress", "blocked", "done"} {
		if isTaskDir(filepath.Join(root, bare)) {
			t.Errorf("a bare %q/ dir leaked — the prefix wasn't applied somewhere", bare)
		}
	}
}

// TestIntegrationSecondaryQueueBootstrap covers the monorepo path: `add` bootstraps a
// secondary --tasks queue on demand (since `coop init` only scaffolds the root), while a
// non-add command on a missing queue errors instead of silently creating it, and an id
// command across several queues errors cleanly when the id exists in none of them.
func TestIntegrationSecondaryQueueBootstrap(t *testing.T) {
	repo := t.TempDir()
	a := appFor(repo)

	if code, err := a.cmdTasks([]string{"--tasks", "portal/.agent/tasks", "add", "portal auth"}); code != 0 || err != nil {
		t.Fatalf("bootstrap add on a secondary queue: code=%d err=%v", code, err)
	}
	if !isTaskDir(filepath.Join(repo, "portal", ".agent", "tasks", "00_todo")) {
		t.Error("add did not bootstrap the secondary queue's 00_todo/")
	}

	// A non-add command on a queue that doesn't exist must fail and must NOT create it.
	if code, err := a.cmdTasks([]string{"--tasks", "runner/.agent/tasks", "ls"}); code == 0 || err == nil {
		t.Errorf("ls on a missing queue should error, got code=%d err=%v", code, err)
	}
	if isTaskDir(filepath.Join(repo, "runner", ".agent", "tasks")) {
		t.Error("ls wrongly created the missing queue dir")
	}

	// An id command across two queues resolves the task itself; an id found NOWHERE errors
	// (naming the queue count) without creating anything. (`add`, which creates into a queue,
	// still needs one target; see TestIntegrationMultiQueueRollup.)
	if code, err := a.cmdTasks([]string{"--tasks", "a/.agent/tasks", "--tasks", "b/.agent/tasks", "done", "x"}); code != 1 || err == nil || !strings.Contains(err.Error(), "2 configured queues") {
		t.Errorf("cross-queue done on a missing id should error naming the queues, got code=%d err=%v", code, err)
	}

	// `coop tasks --tasks done` swallows `done` as the queue path; rather than silently
	// showing help + exit 0, it errors and points at the likely-intended subcommand.
	code, err := a.cmdTasks([]string{"--tasks", "done"})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "coop tasks done") {
		t.Errorf("`tasks --tasks done` should be a usage error suggesting `coop tasks done`, got code=%d err=%v", code, err)
	}
}

func TestIntegrationUmbrellaAddRequiresProject(t *testing.T) {
	repo := t.TempDir()
	writeUmbrellaProject(t, repo, "portal", "runner", "packs")
	code, err := appForDerivedQueues(repo).cmdTasks([]string{"add", "Fix auth retry"})
	if code != 2 || err == nil {
		t.Fatalf("umbrella add without --project = (%d, %v), want usage error", code, err)
	}
	msg := err.Error()
	for _, want := range []string{
		"this umbrella project has multiple task queues; use --project root for repo-wide work or --project <subproject> for project-local work",
		"usage:\n  coop tasks add --project <project> \"<title>\"",
		"root    .agent/tasks",
		"repo-wide or cross-project work",
		"portal  portal/.agent/tasks",
		"runner  runner/.agent/tasks",
		"coop tasks add --project portal \"Fix auth retry\"",
		"--context \"portal auth retries can loop forever\"",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("umbrella add error missing %q:\n%s", want, msg)
		}
	}
	for _, rel := range []string{tasksRoot, "portal/" + tasksRoot, "runner/" + tasksRoot, "packs/" + tasksRoot} {
		if isTaskDir(filepath.Join(repo, rel)) {
			t.Errorf("refused add should not create queue %s", rel)
		}
	}
}

func TestIntegrationUmbrellaAddProjectTargetsRootAndSubproject(t *testing.T) {
	repo := t.TempDir()
	writeUmbrellaProject(t, repo, "portal", "runner")
	a := appForDerivedQueues(repo)

	if code, err := a.cmdTasks([]string{"add", "--project", "root", "Update shared release checklist"}); code != 0 || err != nil {
		t.Fatalf("add root project: code=%d err=%v", code, err)
	}
	rootItems := readTaskTree(filepath.Join(repo, tasksRoot))
	if len(rootItems) != 1 || rootItems[0].Title != "Update shared release checklist" {
		t.Fatalf("root add wrote %+v, want one root task", rootItems)
	}

	if code, err := a.cmdTasks([]string{"add", "--project=runner", "Fix auth retry",
		"--context", "runner auth retries can loop forever",
		"--acceptance", "make check green + retry cap test",
		"--approach", "cap retries and surface the final error",
		"--subtask", "add retry cap",
		"--subtask", "test the failure path"}); code != 0 || err != nil {
		t.Fatalf("add runner project: code=%d err=%v", code, err)
	}
	runnerRoot := filepath.Join(repo, "runner", tasksRoot)
	runnerItems := readTaskTree(runnerRoot)
	if len(runnerItems) != 1 || runnerItems[0].Title != "Fix auth retry" {
		t.Fatalf("runner add wrote %+v, want one runner task", runnerItems)
	}
	body := readFileString(filepath.Join(runnerItems[0].Dir, "task.md"))
	for _, want := range []string{
		"**Context:** runner auth retries can loop forever",
		"**Acceptance criteria:** make check green + retry cap test",
		"**Approach:** cap retries and surface the final error",
		"- [ ] add retry cap",
		"- [ ] test the failure path",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runner task missing %q:\n%s", want, body)
		}
	}
	if portalItems := readTaskTree(filepath.Join(repo, "portal", tasksRoot)); len(portalItems) != 0 {
		t.Fatalf("runner add should not write portal queue, got %+v", portalItems)
	}
}

func TestIntegrationUmbrellaAddRejectsUnknownProject(t *testing.T) {
	repo := t.TempDir()
	writeUmbrellaProject(t, repo, "portal", "runner")
	code, err := appForDerivedQueues(repo).cmdTasks([]string{"add", "--project", "myproject", "Fix auth retry"})
	if code != 2 || err == nil {
		t.Fatalf("unknown project = (%d, %v), want usage error", code, err)
	}
	for _, want := range []string{`unknown project "myproject"`, "projects:", "portal  portal/.agent/tasks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("unknown project error missing %q:\n%s", want, err)
		}
	}
}

// TestIntegrationMultiQueueRollup: `coop tasks ls`/`decisions`/`lint` roll up across several
// configured queues (a monorepo with a per-project .agent/tasks), each under its header, and the
// id-addressed commands find their task in whichever queue holds it — erroring only when the id
// matches in more than one queue, or the target queue is genuinely ambiguous (add).
func TestIntegrationMultiQueueRollup(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, "a", tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# X\n")
	writeTaskFile(t, filepath.Join(repo, "b", tasksRoot, stateDone, "2026-01-02-y", "task.md"), "# Y\n")
	a := appFor(repo)
	twoQueues := []string{"--tasks", "a/" + tasksRoot, "--tasks", "b/" + tasksRoot}
	run := func(args ...string) (int, error) {
		return a.cmdTasks(append(append([]string{}, twoQueues...), args...))
	}

	out := captureStdout(t, func() {
		if code, err := run("ls"); code != 0 || err != nil {
			t.Fatalf("multi-queue ls: code=%d err=%v", code, err)
		}
	})
	for _, want := range []string{"a/" + tasksRoot, "b/" + tasksRoot, "2026-01-01-x", "2026-01-02-y"} {
		if !strings.Contains(out, want) {
			t.Errorf("multi-queue ls missing %q:\n%s", want, out)
		}
	}

	// An id-addressed command finds its task across the queues and acts on the right one.
	if code, err := run("claim", "x"); code != 0 || err != nil {
		t.Errorf("multi-queue claim should resolve the queue holding the id, got code=%d err=%v", code, err)
	}
	if it, err := findTask(filepath.Join(repo, "a", tasksRoot), "2026-01-01-x"); err != nil || it.State != stateInProgress {
		t.Errorf("claim should have moved the task in queue a: state=%v err=%v", it.State, err)
	}
	// An id present in BOTH queues (split makes such copies) is refused, naming the queues.
	writeTaskFile(t, filepath.Join(repo, "b", tasksRoot, stateTodo, "2026-01-01-x", "task.md"), "# X copy\n")
	if code, err := run("done", "2026-01-01-x"); code != 1 || err == nil || !strings.Contains(err.Error(), "a/"+tasksRoot) {
		t.Errorf("ambiguous cross-queue id should error naming the queues, got code=%d err=%v", code, err)
	}
	// lint rolls up, and the exit code is the worst queue's (the seeded tasks lack acceptance
	// criteria, so both queues flag issues → exit 1).
	lintOut := captureStdout(t, func() {
		if code, err := run("lint"); code != 1 || err != nil {
			t.Errorf("multi-queue lint should aggregate to exit 1, got code=%d err=%v", code, err)
		}
	})
	if !strings.Contains(lintOut, "2026-01-01-x") {
		t.Errorf("multi-queue lint missing findings:\n%s", lintOut)
	}
	// add still needs one unambiguous target queue.
	if code, err := run("add", "new thing"); code != 2 || err == nil {
		t.Errorf("multi-queue add should still require a single --tasks, got code=%d err=%v", code, err)
	}
}

func TestIntegrationMultiQueueClearReportsPartialRemoval(t *testing.T) {
	repo := t.TempDir()
	rootA := filepath.Join(repo, "a", tasksRoot)
	rootB := filepath.Join(repo, "b", tasksRoot)
	writeTaskFile(t, filepath.Join(rootA, stateDone, "2026-01-01-a", "task.md"), "# A\n")
	busy := taskForLease(t, rootB, stateInProgress, "2026-01-02-b")
	lease, _, err := tryTaskLease(rootB, busy, testLeaseOwner())
	if err != nil {
		t.Fatal(err)
	}
	if err := moveTaskDir(rootB, busy, stateDone); err != nil {
		t.Fatal(err)
	}
	done, ok := currentTask(rootB, busy.ID)
	if !ok {
		t.Fatal("busy completed task disappeared")
	}
	if err := lease.MarkCompleted(done.Dir); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--tasks", "a/" + tasksRoot,
		"--tasks", "b/" + tasksRoot,
		"rm", "--all-done", "--yes",
	}
	code, err := appFor(repo).cmdTasks(args)
	if code != -1 || err == nil {
		t.Fatalf("multi-queue rm with later live lease = (%d, %v), want failure", code, err)
	}
	for _, want := range []string{
		busy.ID,
		"1 task removed before stop",
		"across configured queues",
		"coop tasks rm --all-done --yes",
		"still leased by a live controller",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("multi-queue bulk failure missing %q:\n%s", want, err)
		}
	}
	if _, ok := currentTask(rootA, "2026-01-01-a"); ok {
		t.Fatal("multi-queue bulk failure restored the task removed from the earlier queue")
	}
	if current, ok := currentTask(rootB, busy.ID); !ok || current.State != stateDone {
		t.Fatal("multi-queue bulk failure removed or moved the busy task")
	}

	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if code, err := appFor(repo).cmdTasks(args); code != 0 || err != nil {
		t.Fatalf("multi-queue bulk retry after lease release = (%d, %v), want success", code, err)
	}
}

// TestIntegrationListShowsCleanLabels confirms the prefix never leaks into output: the list
// groups by the clean state name (todo/in_progress/…), not the on-disk 00_todo/ dir name.
func TestIntegrationListShowsCleanLabels(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, tasksRoot)
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-a", "task.md"), "# A\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, "2026-01-02-b", "task.md"), "# B\n")

	out := captureStdout(t, func() { _, _ = appFor(repo).cmdTasks([]string{"ls"}) })
	if !strings.Contains(out, "todo (1)") || !strings.Contains(out, "in_progress (1)") {
		t.Errorf("ls should head groups with clean labels:\n%s", out)
	}
	for _, leaked := range []string{"00_todo", "10_in_progress", "50_blocked", "99_done"} {
		if strings.Contains(out, leaked) {
			t.Errorf("on-disk prefix %q leaked into list output:\n%s", leaked, out)
		}
	}
}

// A bare leading flag on `coop tasks` is the default listing with that flag — `coop tasks
// --blocked` means `coop tasks ls --blocked`, not a subcommand named "--blocked". This must hold
// for a single queue AND the umbrella roll-up (the reported "super inconvenient" case).
func TestTasksBareLeadingFlagListsFiltered(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateBlocked, "2026-01-01-b", "task.md"), "# Blocked one\n")
	writeTaskFile(t, filepath.Join(repo, tasksRoot, stateTodo, "2026-01-02-t", "task.md"), "# Todo one\n")

	var code int
	var err error
	out := captureStdout(t, func() { code, err = appFor(repo).cmdTasks([]string{"--blocked"}) })
	if code != 0 || err != nil {
		t.Fatalf("coop tasks --blocked: got (%d, %v), want (0, nil)", code, err)
	}
	if !strings.Contains(out, "Blocked one") || strings.Contains(out, "Todo one") {
		t.Errorf("bare --blocked should list only the blocked task:\n%s", out)
	}

	// Umbrella (several queues): the same shorthand rolls up, still filtered.
	writeTaskFile(t, filepath.Join(repo, "sub", tasksRoot, stateBlocked, "2026-01-03-c", "task.md"), "# Sub blocked\n")
	multi := &app{cfg: &config.Config{RepoOverride: repo, TasksFiles: []string{tasksRoot, filepath.Join("sub", tasksRoot)}}}
	out2 := captureStdout(t, func() { code, err = multi.cmdTasks([]string{"--blocked"}) })
	if code != 0 || err != nil {
		t.Fatalf("umbrella coop tasks --blocked: got (%d, %v), want (0, nil)", code, err)
	}
	if !strings.Contains(out2, "Blocked one") || !strings.Contains(out2, "Sub blocked") || strings.Contains(out2, "Todo one") {
		t.Errorf("umbrella bare --blocked should roll up blocked only:\n%s", out2)
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
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: "false"}}

	code, err := a.loop(repo, "no-such-image", "claude", "", nil, []string{tasksRoot}, io.Discard, nil, false, false, 0)
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
	a := &app{cfg: &config.Config{RepoOverride: repo}, rt: runtime.Runtime{Name: "false"}}

	code, err := a.loop(repo, "no-such-image", "claude", "", nil, []string{tasksRoot}, io.Discard, nil, false, true, 3)
	if err != nil || code != 0 {
		t.Fatalf("idle task-limited loop = (%d, %v), want success without an image", code, err)
	}
}

// TestIntegrationDoneTasksAreNotActionable is the loop-safety side of move-don't-delete:
// 99_done/ grows without bound, but only todo/in_progress count as actionable, so the loop's
// stop condition (commands.go: c.Todo+c.Doing == 0) still fires.
func TestIntegrationDoneTasksAreNotActionable(t *testing.T) {
	root := t.TempDir()
	for i, st := range []string{stateDone, stateDone, stateDone, stateBlocked} {
		writeTaskFile(t, filepath.Join(root, st, fmt.Sprintf("t%d", i), "task.md"), "# x\n")
	}
	c, active := taskTreeCounts(readTaskTree(root))
	if c.Todo+c.Doing != 0 {
		t.Errorf("a done+blocked queue must be non-actionable, got Todo=%d Doing=%d", c.Todo, c.Doing)
	}
	if c.Done != 3 || c.Blocked != 1 {
		t.Errorf("counts = %+v, want Done3 Blocked1", c)
	}
	if active != "" {
		t.Errorf("nothing actionable, so active should be empty, got %q", active)
	}
}
