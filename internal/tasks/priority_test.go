package tasks

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// writePriorityTask writes a todo task whose frontmatter carries priority, or none when it is "".
func writePriorityTask(t *testing.T, root, id, priority string) {
	t.Helper()
	front := "---\ntitle: " + id + "\n"
	if priority != "" {
		front += "priority: " + priority + "\n"
	}
	writeTaskFile(t, filepath.Join(root, StateTodo, id, "task.md"), front+"---\n# "+id+"\n"+
		"**Context:** c\n**Acceptance criteria:** a\n**Approach:** p\n")
}

// A person's priority decides which todo task the loop claims next: high, then normal, then low,
// and folder order within each, so tasks without the field keep the order they have today.
func TestAssignLoopTaskClaimsByPriorityThenFolderOrder(t *testing.T) {
	root := t.TempDir()
	writePriorityTask(t, root, "2026-01-01-a", "low")
	writePriorityTask(t, root, "2026-01-02-b", "")
	writePriorityTask(t, root, "2026-01-03-c", "high")
	writePriorityTask(t, root, "2026-01-04-d", "normal")
	writePriorityTask(t, root, "2026-01-05-e", "high")
	writePriorityTask(t, root, "2026-01-06-f", "")

	if _, next, ok, err := QueueState([]string{root}); err != nil || !ok || next.Item.ID != "2026-01-03-c" {
		t.Fatalf("QueueState next = %s (%v, %v), want the first high-priority task", next.Item.ID, ok, err)
	}
	var order []string
	for range 6 {
		assignment, err := AssignLoopTaskOnly([]string{root}, testLeaseOwner(), "")
		if err != nil || assignment.Outcome != assignmentReady {
			t.Fatalf("assignment = (%+v, %v)", assignment, err)
		}
		order = append(order, assignment.Task.Item.ID)
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		if err := MoveTaskDir(root, assignment.Task.Item, StateDone); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"2026-01-03-c", "2026-01-05-e", "2026-01-02-b", "2026-01-04-d", "2026-01-06-f", "2026-01-01-a"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("the loop claimed %v, want %v", order, want)
	}
}

// The board's Up next is the loop's own order: before every claim the task it lists first is the
// one the loop takes, by priority, then the queues in the order they are configured, then folder
// order. A high or low task is marked as coop tasks ls marks it; an unknown priority is normal.
func TestTaskWatchListsUpNextInTheOrderTheLoopClaims(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "shop")
	root, web, api := filepath.Join(repo, TasksRoot), filepath.Join(repo, "web", TasksRoot), filepath.Join(repo, "api", TasksRoot)
	hosts := []string{root, web, api} // configured order, which is not the alphabetical one
	rows := map[string]string{}       // each task's Up next row
	for _, task := range []struct{ queue, id, priority, row string }{
		{api, "2026-01-01-a", "low", "  ○ 2026-01-01-a · api · Low priority"},
		{web, "2026-01-02-b", "", "  ○ 2026-01-02-b · web"},
		{api, "2026-01-03-c", "", "  ○ 2026-01-03-c · api"},
		{root, "2026-01-04-d", "", "  ○ 2026-01-04-d · shop"},
		{api, "2026-01-05-e", "high", "  ○ 2026-01-05-e · api · High priority"},
		{web, "2026-01-06-f", "high", "  ○ 2026-01-06-f · web · High priority"},
		{api, "2026-01-07-g", "urgent", "  ○ 2026-01-07-g · api"},
	} {
		writePriorityTask(t, task.queue, task.id, task.priority)
		rows[task.id] = task.row
	}
	var order []string
	for range len(rows) {
		board := captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, hosts) })
		_, upNext, _ := strings.Cut(board, "\nUp next (")
		first := strings.Split(upNext+"\n", "\n")[1]
		assignment, err := AssignLoopTaskOnly(hosts, testLeaseOwner(), "")
		if err != nil || assignment.Outcome != assignmentReady {
			t.Fatalf("assignment = (%+v, %v)", assignment, err)
		}
		id := assignment.Task.Item.ID
		if order = append(order, id); first != rows[id] {
			t.Errorf("the board listed %q first, but the loop claimed %s:\n%s", first, id, board)
		}
		if err := assignment.Lease.Release(); err != nil {
			t.Fatal(err)
		}
		if err := MoveTaskDir(assignment.Task.Root, assignment.Task.Item, StateDone); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"2026-01-06-f", "2026-01-05-e", "2026-01-04-d", "2026-01-02-b", "2026-01-03-c", "2026-01-07-g", "2026-01-01-a"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("the loop claimed %v, want %v", order, want)
	}

	// On a board of one queue the mark follows the title, the high one as bright as ls shows it.
	p := ui.Colored()
	board := strings.Join(renderSections(p, merge([]Item{
		{Title: "Ship", State: StateTodo, Priority: "high"}, {Title: "Tidy", State: StateTodo, Priority: "low"},
	}), 0, 80)[0], "\n")
	for _, want := range []string{"Ship" + p.Dim(" · ") + "High priority", "Tidy" + p.Dim(" · Low priority")} {
		if !strings.Contains(board, want) {
			t.Errorf("board is missing %q:\n%q", want, board)
		}
	}
}

// A fork loop takes its next task in the same order.
func TestForkAssignmentTakesAHighPriorityTaskFirst(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".agent", "tasks")
	writePriorityTask(t, root, "first", "")
	writePriorityTask(t, root, "second", "high")
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "a")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace,
		BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil || assignment.Outcome != ForkAssignmentSelected {
		t.Fatalf("assignment = (%+v, %v)", assignment, err)
	}
	defer func() { _ = assignment.Lease.Release() }()
	if assignment.Task.Item.ID != "second" {
		t.Errorf("the fork took %q, want the high-priority task", assignment.Task.Item.ID)
	}
}

// coop tasks ls lists todo tasks in the order the loop claims them and marks the two exceptions.
func TestTasksListShowsPriority(t *testing.T) {
	root := t.TempDir()
	writePriorityTask(t, root, "2026-01-01-a", "low")
	writePriorityTask(t, root, "2026-01-02-b", "")
	writePriorityTask(t, root, "2026-01-03-c", "high")
	out := captureStdout(t, func() { _, _ = tasksFolderList(root, false) })
	want := "TODO · 3\n" +
		"  2026-01-03-c\n    High priority\n    2026-01-03-c\n" +
		"  2026-01-02-b\n    2026-01-02-b\n" +
		"  2026-01-01-a\n    Low priority\n    2026-01-01-a\n"
	if !strings.Contains(out, want) {
		t.Errorf("ls =\n%s\nwant it to contain:\n%s", out, want)
	}
}

// The loop reads an unknown priority as normal, so lint names it instead of letting a typo pass.
func TestTasksFolderLintRejectsAnUnknownPriority(t *testing.T) {
	root := t.TempDir()
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	for _, p := range taskPriorities {
		writePriorityTask(t, root, "2026-01-01-"+p, p)
	}
	writePriorityTask(t, root, "2026-01-02-plain", "")
	if code, err := tasksFolderLint(root); code != 0 || err != nil {
		t.Fatalf("lint of known priorities = (%d, %v), want clean", code, err)
	}
	writePriorityTask(t, root, "2026-01-03-urgent", "urgent")
	var code int
	out := captureStdout(t, func() { _ = captureStderr(t, func() { code, _ = tasksFolderLint(root) }) })
	if want := `2026-01-03-urgent: has priority "urgent" — use high, normal or low`; code != 1 || !strings.Contains(out, want) {
		t.Errorf("lint = %d:\n%s\nwant %q", code, out, want)
	}
}
