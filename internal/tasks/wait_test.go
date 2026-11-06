package tasks

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// lintedTask writes a task.md lint has nothing to say about, waiting for waitsOn when it is set.
func lintedTask(t *testing.T, root, state, id, waitsOn string) {
	t.Helper()
	front := "---\nid: " + id + "\ntitle: " + id + "\n"
	if waitsOn != "" {
		front += "waits_on: " + waitsOn + "\n"
	}
	writeTaskFile(t, filepath.Join(root, state, id, "task.md"),
		front+"---\n\n# "+id+"\n\n**Context:** c\n\n**Acceptance criteria:** a\n\n**Approach:** p\n\n## Subtasks\n- [ ] s\n")
}

// lint names a waits_on line that names no task in any queue or the backlog, a wait the loop could
// never meet, and leaves alone a wait on a task in any state, in another linted queue too.
func TestLintNamesAWaitThatNamesNoTask(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	lintedTask(t, root, StateTodo, "lost", "2026-11-06-no-such-task")
	for state, on := range map[string]string{StateInProgress: "doing", StateBlocked: "asked", StateDone: "shipped", StateBacklog: "idea"} {
		lintedTask(t, root, state, on, "")
		lintedTask(t, root, StateTodo, "after-"+on, on)
	}
	if err := WriteDecision(filepath.Join(root, StateBlocked, "asked"), "asked", "asked", Decision{Question: "Which?", Why: "W", Options: []string{"A — a"}, Recommendation: "A"}); err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStdout(t, func() { _ = captureStderr(t, func() { code, _ = tasksFolderLint(root) }) })
	want := "  lost: waits_on names 2026-11-06-no-such-task, but no task has that id — fix the id, or delete the waits_on line\n"
	if code != 1 || out != want {
		t.Fatalf("lint = %d:\n%q\nwant:\n%q", code, out, want)
	}

	// Two queues linted together: a wait on the other queue's task is one the loop can meet.
	repo := t.TempDir()
	rels := []string{filepath.Join("a", ".agent", "tasks"), filepath.Join("b", ".agent", "tasks")}
	for _, rel := range rels {
		if err := ScaffoldStateDirs(filepath.Join(repo, rel)); err != nil {
			t.Fatal(err)
		}
	}
	lintedTask(t, filepath.Join(repo, rels[0]), StateTodo, "first", "second")
	lintedTask(t, filepath.Join(repo, rels[1]), StateTodo, "second", "")
	out = captureStdout(t, func() { _ = captureStderr(t, func() { code, _ = tasksLintAll(repo, rels) }) })
	if code != 0 || strings.Contains(out, "waits_on") {
		t.Fatalf("lint across queues = %d:\n%s", code, out)
	}
}

func readWaitFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A task told to wait leaves the run's way, the task it waits for is worked first, and finishing that
// task returns it to ordinary work with its wait removed.
func TestTaskWaitIsReleasedWhenTheNamedTaskIsDone(t *testing.T) {
	root := t.TempDir()
	// The waiting task sorts first, so only the wait keeps the loop from taking it.
	writeTaskFile(t, filepath.Join(root, StateInProgress, "alpha-docs", "task.md"), "---\nid: alpha-docs\ntitle: Docs\n---\n\n# Docs\n- [ ] write\n")
	writeTaskFile(t, filepath.Join(root, StateInProgress, "alpha-docs", "state.md"), "# State — Docs\n\n**Status:** in progress\n**Done so far:** drafted the example\n**Next action:** apply tmp/example.patch, then run the docs check\n**Traps:** —\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "zeta-bridge", "task.md"), "# Bridge\n- [ ] fix\n")
	const resume = "apply tmp/example.patch, then run the docs check"
	docs, _ := mustCurrentTask(t, root, "alpha-docs")
	if err := WaitForTask([]string{root}, QueuedTask{Root: root, Item: docs}, "zeta-bridge", "the example must not land before\tthe bridge fix"); err != nil {
		t.Fatalf("wait: %v", err)
	}
	waiting, ok := mustCurrentTask(t, root, "alpha-docs")
	if !ok || waiting.State != StateTodo || waiting.WaitsOn != "zeta-bridge" {
		t.Fatalf("waiting task = %+v, exists=%v", waiting, ok)
	}
	if body := readWaitFile(t, filepath.Join(waiting.Dir, "task.md")); !strings.HasPrefix(body, "---\nid: alpha-docs\ntitle: Docs\nwaits_on: zeta-bridge\n---\n") {
		t.Fatalf("task.md frontmatter = %q", body)
	}
	if log := readWaitFile(t, filepath.Join(waiting.Dir, "log.md")); !strings.Contains(log, "waits for zeta-bridge before it can go on: the example must not land before the bridge fix") {
		t.Fatalf("log.md = %q", log)
	}
	if state, err := ReadTaskState(waiting.Dir); err != nil || !strings.Contains(state.Status, "must not start before zeta-bridge is done") ||
		state.NextAction != resume || state.DoneSoFar != "drafted the example" {
		t.Fatalf("state = %+v, %v", state, err)
	}
	if waits, err := WaitsForAnotherTask(QueuedTask{Root: root, Item: docs}); err != nil || !waits {
		t.Fatalf("a task handed back to wait reads as waiting = %v, %v", waits, err)
	}

	assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != "zeta-bridge" {
		t.Fatalf("assignment while docs waits = %+v, %v", assignment, err)
	}
	bridge := assignment.Task.Item
	// While the bridge is held, the waiting task is not handed out either, and the busy queue says so.
	held, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || held.Outcome != AssignmentUnavailable || held.Busy.Waiting != 1 || held.Lease != nil {
		t.Fatalf("assignment while the bridge is held = %+v, %v", held, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	if settled, err := SettleTaskWaits([]string{root}, testLeaseOwner()); err != nil || len(settled) != 0 {
		t.Fatalf("settle before bridge finished = %+v, %v", settled, err)
	}

	if err := MoveTaskDir(root, bridge, StateDone); err != nil {
		t.Fatal(err)
	}
	settled, err := SettleTaskWaits([]string{root}, testLeaseOwner())
	if err != nil || len(settled) != 1 || settled[0].Task.Item.ID != "alpha-docs" || settled[0].WaitsOn != "zeta-bridge" || settled[0].Reason != "" {
		t.Fatalf("settle after bridge finished = %+v, %v", settled, err)
	}
	released, _ := mustCurrentTask(t, root, "alpha-docs")
	if released.State != StateTodo || released.WaitsOn != "" || strings.Contains(readWaitFile(t, filepath.Join(released.Dir, "task.md")), "waits_on") {
		t.Fatalf("released task = %+v", released)
	}
	if state, err := ReadTaskState(released.Dir); err != nil || !strings.Contains(state.Status, "zeta-bridge, the task it waited for, is done") || state.NextAction != resume {
		t.Fatalf("released state = %+v, %v", state, err)
	}
	assignment, err = AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
	if err != nil || assignment.Outcome != assignmentReady || assignment.Task.Item.ID != "alpha-docs" {
		t.Fatalf("assignment after the wait was met = %+v, %v", assignment, err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// A wait the tool can tell is wrong is refused with nothing changed. One that can never be met later,
// on a task that disappears or is blocked, goes to a person instead of being skipped forever.
func TestTaskWaitThatCannotBeMetNeedsADecision(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateInProgress, "docs", "task.md"), "# Docs\n- [ ] write\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "loop", "task.md"), "---\nwaits_on: docs\n---\n# Loop\n- [ ] fix\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "shipped", "task.md"), "# Shipped\n- [x] done\n")
	docs, _ := mustCurrentTask(t, root, "docs")
	for on, want := range map[string]string{
		"missing": "no task missing is in this loop's queues",
		"docs":    "cannot wait for itself",
		"shipped": "already done",
		"loop":    "loop waits for this task in turn",
	} {
		if err := WaitForTask([]string{root}, QueuedTask{Root: root, Item: docs}, on, "order"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("wait on %s = %v, want %q", on, err, want)
		}
	}
	if current, _ := mustCurrentTask(t, root, "docs"); current.State != StateInProgress || current.WaitsOn != "" || pathExists(filepath.Join(current.Dir, "log.md")) {
		t.Fatalf("refused waits changed the task: %+v", current)
	}

	writeTaskFile(t, filepath.Join(root, StateTodo, "renamed", "task.md"), "---\ntitle: Renamed\nwaits_on: gone\n---\n# Renamed\n- [ ] work\n")
	writeTaskFile(t, filepath.Join(root, StateBlocked, "question", "task.md"), "# Question\n- [ ] ask\n")
	writeTaskFile(t, filepath.Join(root, StateBlocked, "question", "decision.md"), "# Decision: which way?\n\n**Resolution:** <your answer>\n")
	if err := WaitForTask([]string{root}, QueuedTask{Root: root, Item: docs}, "question", "needs the answer first"); err != nil {
		t.Fatalf("wait on a blocked task: %v", err)
	}
	// loop waits for docs, which this same call parks, so it reaches a person in the same call too
	// instead of being skipped until a later run.
	settled, err := SettleTaskWaits([]string{root}, testLeaseOwner())
	if err != nil || len(settled) != 3 {
		t.Fatalf("settle = %+v, %v", settled, err)
	}
	for id, why := range map[string]string{
		"renamed": "gone is not in the task queues this loop works",
		"docs":    "question is blocked, waiting for a person's decision",
		"loop":    "docs is blocked, waiting for a person's decision",
	} {
		parked, _ := mustCurrentTask(t, root, id)
		if parked.State != StateBlocked || !parked.HasDecision || parked.WaitsOn == "" {
			t.Fatalf("%s after settle = %+v", id, parked)
		}
		decision := readWaitFile(t, filepath.Join(parked.Dir, "decision.md"))
		if !strings.Contains(decision, why) || !strings.Contains(decision, "delete the waits_on line") {
			t.Fatalf("%s decision = %q, want %q", id, decision, why)
		}
	}
	if settled, err := SettleTaskWaits([]string{root}, testLeaseOwner()); err != nil || len(settled) != 0 {
		t.Fatalf("second settle = %+v, %v", settled, err)
	}
	// A run scoped to a waiting task finds nothing to work rather than reporting the queue busy.
	writeTaskFile(t, filepath.Join(root, StateTodo, "scoped", "task.md"), "---\nwaits_on: shipped-later\n---\n# Scoped\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "shipped-later", "task.md"), "# Later\n")
	if scoped, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "scoped"); err != nil || scoped.Outcome != AssignmentDrained {
		t.Fatalf("scoped assignment of a waiting task = %+v, %v", scoped, err)
	}
}

func TestTaskListShowsWhatEachTaskWaitsFor(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "docs", "task.md"), "---\ntitle: Docs\nwaits_on: bridge\n---\n# Docs\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "bridge", "task.md"), "# Bridge\n")
	out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	if !strings.Contains(out, "Waits for bridge") || strings.Count(out, "Waits for") != 1 {
		t.Fatalf("listing = %q", out)
	}
}

// The listing says a task waits only while the loop would skip it, as the board does: once the task
// it names is done the wait is met, and neither shows a marker.
func TestTaskListShowsNoWaitOnceTheTaskItWaitsForIsDone(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "shop")
	root := filepath.Join(repo, TasksRoot)
	writeTaskFile(t, filepath.Join(root, StateTodo, "docs", "task.md"), "---\ntitle: Docs\nwaits_on: bridge\n---\n# Docs\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "bridge", "task.md"), "# Bridge\n")
	listing := func() string { return captureStdout(t, func() { _, _ = tasksFolderList(root, false, StateTodo) }) }
	board := func() string { return captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, []string{root}) }) }

	for what, out := range map[string]string{"listing": listing(), "board": board()} {
		if !strings.Contains(out, "Waits for bridge") {
			t.Fatalf("the %s of a task whose wait is pending lacks the marker:\n%s", what, out)
		}
	}
	bridge, _ := mustCurrentTask(t, root, "bridge")
	if err := MoveTaskDir(root, bridge, StateDone); err != nil {
		t.Fatal(err)
	}
	for what, out := range map[string]string{"listing": listing(), "board": board()} {
		if !strings.Contains(out, "Docs") || strings.Contains(out, "Waits for") {
			t.Fatalf("the %s of a task whose wait is met:\n%s\nwant Docs with no marker", what, out)
		}
	}
}

// A task may wait for one in another queue, so the roll-up over several queues reads every wait
// against all of them: a done task in another queue meets it, a todo one does not.
func TestTaskRollUpReadsAWaitAcrossQueues(t *testing.T) {
	repo := t.TempDir()
	writeTaskFile(t, filepath.Join(repo, "web", StateTodo, "ship", "task.md"), "---\ntitle: Ship\nwaits_on: built\n---\n# Ship\n")
	writeTaskFile(t, filepath.Join(repo, "web", StateTodo, "later", "task.md"), "---\ntitle: Later\nwaits_on: tested\n---\n# Later\n")
	writeTaskFile(t, filepath.Join(repo, "api", StateDone, "built", "task.md"), "# Built\n")
	writeTaskFile(t, filepath.Join(repo, "api", StateTodo, "tested", "task.md"), "# Tested\n")
	var code int
	var err error
	out := captureStdout(t, func() { code, err = tasksListAll(repo, []string{"web", "api"}, []string{"--todo"}) })
	if code != 0 || err != nil {
		t.Fatalf("roll-up = %d, %v", code, err)
	}
	if !strings.Contains(out, "Waits for tested") || strings.Contains(out, "Waits for built") || strings.Count(out, "Waits for") != 1 {
		t.Fatalf("roll-up =\n%s\nwant only the wait on the task still todo", out)
	}
}

func TestWithFrontmatterFieldKeepsEveryOtherLine(t *testing.T) {
	for _, tc := range []struct{ name, in, value, want string }{
		{"add", "---\nid: a\n---\n# A\n", "b", "---\nid: a\nwaits_on: b\n---\n# A\n"},
		{"replace", "---\nwaits_on: x\nid: a\n---\n# A\n", "b", "---\nwaits_on: b\nid: a\n---\n# A\n"},
		{"remove", "---\nid: a\nwaits_on: b\n---\n# A\n", "", "---\nid: a\n---\n# A\n"},
		{"header", "<!-- note -->\n\n---\nid: a\n---\n", "b", "<!-- note -->\n\n---\nid: a\nwaits_on: b\n---\n"},
		{"none", "# A\n", "b", "---\nwaits_on: b\n---\n\n# A\n"},
		{"none removed", "# A\n", "", "# A\n"},
	} {
		if got := withFrontmatterField(tc.in, waitsOnField, tc.value); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if tc.value != "" {
			if fields, _ := SplitFrontmatter(withFrontmatterField(tc.in, waitsOnField, tc.value)); fields[waitsOnField] != tc.value {
				t.Errorf("%s: written field reads back as %q", tc.name, fields[waitsOnField])
			}
		}
	}
}

// The board's Up next follows the loop's picker: a task that waits for another comes last, marked
// with what it waits for, so the first row is always the task the loop takes next.
func TestTaskWatchListsAWaitingTaskLast(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "shop")
	root := filepath.Join(repo, TasksRoot)
	// The waiting task sorts first, so only its wait keeps the loop from taking it.
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-alpha", "task.md"), "---\ntitle: Alpha\nwaits_on: 2026-01-03-charlie\n---\n# Alpha\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-02-bravo", "task.md"), "---\ntitle: Bravo\n---\n# Bravo\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-03-charlie", "task.md"), "---\ntitle: Charlie\n---\n# Charlie\n")
	upNext := func() []string {
		board := captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, []string{root}) })
		var rows []string
		for _, line := range strings.Split(board, "\n") {
			if strings.HasPrefix(line, "  ○ ") {
				rows = append(rows, line)
			}
		}
		return rows
	}
	if got, want := upNext(), []string{"  ○ Bravo", "  ○ Charlie", "  ○ Alpha · Waits for 2026-01-03-charlie"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Up next = %q, want %q", got, want)
	}
	var order []string
	for range 3 {
		rows := upNext()
		assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
		if err != nil || assignment.Outcome != assignmentReady {
			t.Fatalf("assignment = (%+v, %v)", assignment, err)
		}
		order = append(order, assignment.Task.Item.ID)
		if len(rows) == 0 || rows[0] != "  ○ "+assignment.Task.Item.Title {
			t.Errorf("the board listed %q, but the loop claimed %s", rows, assignment.Task.Item.ID)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		if err := MoveTaskDir(root, assignment.Task.Item, StateDone); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"2026-01-02-bravo", "2026-01-03-charlie", "2026-01-01-alpha"}; !reflect.DeepEqual(order, want) {
		t.Errorf("the loop claimed %v, want %v", order, want)
	}
}

// A fork takes no task that waits for another; the plain loop settles those waits.
func TestForkSchedulerSkipsAWaitingTask(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "alpha-docs", "task.md"), "---\nwaits_on: zeta-bridge\n---\n# Docs\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "zeta-bridge", "task.md"), "# Bridge\n")
	_, unowned, _, err := forkAssignmentCandidates([]string{root})
	if err != nil || len(unowned) != 1 || unowned[0].Item.ID != "zeta-bridge" {
		t.Fatalf("fork candidates = %+v, %v", unowned, err)
	}
}

// A refused wait returns the task to in progress with its saved work and the resume step its worker
// left, and only from the waiting folder the worker left: a task that is not waiting is never moved.
// A worker that left no step is told to finish the task or block it, not the wait's own "continue
// once the other task is done".
func TestRestoreRejectedWaitKeepsTheWorkAndNeedsTheWaitingFolder(t *testing.T) {
	for _, tc := range []struct{ name, resume, want string }{
		{name: "worker's step", resume: "apply tmp/example.patch, then run the docs check", want: "apply tmp/example.patch, then run the docs check"},
		{name: "no step", want: "finish this task, or use tasks_block if it must not land before zeta-bridge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTaskFile(t, filepath.Join(root, StateInProgress, "alpha-docs", "task.md"), "---\nid: alpha-docs\n---\n# Docs\n- [ ] write\n")
			if tc.resume != "" {
				writeTaskFile(t, filepath.Join(root, StateInProgress, "alpha-docs", "state.md"), "# State — Docs\n\n**Status:** in progress\n**Done so far:** drafted the example\n**Next action:** "+tc.resume+"\n**Traps:** —\n")
			}
			writeTaskFile(t, filepath.Join(root, StateInProgress, "alpha-docs", "tmp", "example.patch"), "saved work\n")
			writeTaskFile(t, filepath.Join(root, StateTodo, "zeta-bridge", "task.md"), "# Bridge\n- [ ] fix\n")
			docs, _ := mustCurrentTask(t, root, "alpha-docs")
			task := QueuedTask{Root: root, Item: docs}
			if err := RestoreRejectedWait(task, "history changed"); err == nil || !strings.Contains(err.Error(), "no longer present") {
				t.Fatalf("restore of a task that never waited = %v", err)
			}
			if err := WaitForTask([]string{root}, task, "zeta-bridge", "the example must not land before the bridge fix"); err != nil {
				t.Fatal(err)
			}
			if err := RestoreRejectedWait(task, "commit aaa was made after the wait began"); err != nil {
				t.Fatalf("restore: %v", err)
			}
			restored, ok := mustCurrentTask(t, root, "alpha-docs")
			if !ok || restored.State != StateInProgress || restored.WaitsOn != "" || strings.Contains(readWaitFile(t, filepath.Join(restored.Dir, "task.md")), "waits_on") {
				t.Fatalf("restored task = %+v, exists=%v", restored, ok)
			}
			if saved := readWaitFile(t, filepath.Join(restored.Dir, "tmp", "example.patch")); saved != "saved work\n" {
				t.Fatalf("tmp/example.patch = %q", saved)
			}
			if log := readWaitFile(t, filepath.Join(restored.Dir, "log.md")); !strings.Contains(log, "wait for zeta-bridge rejected: commit aaa was made after the wait began") {
				t.Fatalf("log.md = %q", log)
			}
			if state, err := ReadTaskState(restored.Dir); err != nil || !strings.Contains(state.Status, "wait rejected") || state.NextAction != tc.want {
				t.Fatalf("state = %+v, %v; want next action %q", state, err, tc.want)
			}
			if err := RestoreRejectedWait(task, "again"); err == nil {
				t.Fatal("a second restore moved a task that no longer waits")
			}
		})
	}
}

// The box can write the queue, so a queue Coop cannot read must not read as "this task does not wait".
func TestWaitsForAnotherTaskReportsAnUnreadableQueue(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "alpha-docs", "task.md"), "---\nwaits_on: zeta-bridge\n---\n# Docs\n")
	docs, _ := mustCurrentTask(t, root, "alpha-docs")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, StateTodo, "planted")); err != nil {
		t.Fatal(err)
	}
	if waits, err := WaitsForAnotherTask(QueuedTask{Root: root, Item: docs}); err == nil {
		t.Fatalf("an unreadable queue read as waits=%v with no error", waits)
	}
}

// A person claims a waiting task while the loop settles its wait. The claim records itself under the
// task's owner lock before it moves the folder, and settlement holds that lock from its checks to its
// write, so the claim waits for the settlement instead of moving the folder out from under it.
func TestSettleTaskWaitsLetsAConcurrentClaimWaitItsTurn(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "alpha-docs", "task.md"), "---\nid: alpha-docs\nwaits_on: zeta-bridge\n---\n\n# Docs\n- [ ] write\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "zeta-bridge", "task.md"), "# Bridge\n- [x] fix\n")
	claimed := make(chan error, 1)
	settleWaitBeforeWrite = func(task QueuedTask) {
		go func() {
			if _, err := claimTaskOwnerRecord(root, task.Item.ID, claimOptions{}); err != nil {
				claimed <- err
				return
			}
			current, _, err := CurrentTask(root, task.Item.ID)
			if err == nil {
				err = MoveTaskDir(root, current, StateInProgress)
			}
			claimed <- err
		}()
		select { // without the owner lock the claim finishes here and moves the folder away
		case err := <-claimed:
			claimed <- err
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { settleWaitBeforeWrite = nil })

	settled, err := SettleTaskWaits([]string{root}, testLeaseOwner())
	if err != nil || len(settled) != 1 || settled[0].Task.Item.ID != "alpha-docs" {
		t.Fatalf("settle beside a claim = %+v, %v", settled, err)
	}
	if err := <-claimed; err != nil {
		t.Fatalf("the claim after settlement: %v", err)
	}
	task, ok := mustCurrentTask(t, root, "alpha-docs")
	if !ok || task.State != StateInProgress || task.WaitsOn != "" {
		t.Fatalf("task after settle and claim = %+v", task)
	}
	if _, owned, err := ReadTaskOwnerRecord(root, "alpha-docs"); err != nil || !owned {
		t.Fatalf("claim ownership = %v, %v", owned, err)
	}
}
