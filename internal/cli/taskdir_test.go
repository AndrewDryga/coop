package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTask creates <root>/<state>/<id>/<file>=content for each entry, making dirs.
func writeTaskFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSplitFrontmatter(t *testing.T) {
	fields, body := splitFrontmatter("---\nid: x-1\ntitle: Do the thing\nlabels: [a, b]\n# a comment\n---\n\n# Heading\nbody text\n")
	if fields["id"] != "x-1" || fields["title"] != "Do the thing" || fields["labels"] != "[a, b]" {
		t.Fatalf("fields = %v", fields)
	}
	if _, ok := fields["a comment"]; ok {
		t.Errorf("comment line leaked into fields: %v", fields)
	}
	if got := firstH1(body); got != "Heading" {
		t.Errorf("body H1 = %q", got)
	}

	// No header → all body, no fields.
	f2, b2 := splitFrontmatter("# Just a title\ntext")
	if len(f2) != 0 || b2 != "# Just a title\ntext" {
		t.Errorf("no-header parse: fields=%v body=%q", f2, b2)
	}
	// Unterminated header → treat as body, don't hang/panic.
	f3, b3 := splitFrontmatter("---\nid: x\nno closing fence\n")
	if len(f3) != 0 || b3 == "" {
		t.Errorf("unterminated header should fall back to body: fields=%v", f3)
	}
}

func TestScanSubtasksSkipsFences(t *testing.T) {
	body := "## Subtasks\n- [ ] one\n- [x] two\n  - [X] nested done\n```\n- [ ] fenced, not a subtask\n```\n- [w] in progress\n"
	subs := scanSubtasks(body)
	if len(subs) != 4 {
		t.Fatalf("want 4 subtasks (fenced one excluded), got %d: %v", len(subs), subs)
	}
	done := 0
	for _, d := range subs {
		if d {
			done++
		}
	}
	if done != 2 { // [x] and [X]; [ ] and [w] are not done
		t.Errorf("done subtasks = %d, want 2", done)
	}
}

func TestParseTaskFolderTitleResolution(t *testing.T) {
	dir := t.TempDir()
	// frontmatter title wins
	a := filepath.Join(dir, stateTodo, "2026-01-01-a")
	writeTaskFile(t, filepath.Join(a, "task.md"), "---\ntitle: From frontmatter\n---\n# From H1\n")
	if it, ok := parseTaskFolder(a, stateTodo); !ok || it.Title != "From frontmatter" || it.ID != "2026-01-01-a" {
		t.Fatalf("frontmatter title: ok=%v item=%+v", ok, it)
	}
	// no frontmatter title → H1
	b := filepath.Join(dir, stateTodo, "2026-01-01-b")
	writeTaskFile(t, filepath.Join(b, "task.md"), "# Heading title\nbody")
	if it, _ := parseTaskFolder(b, stateTodo); it.Title != "Heading title" {
		t.Errorf("H1 title = %q", it.Title)
	}
	// neither → id
	c := filepath.Join(dir, stateTodo, "2026-01-01-c")
	writeTaskFile(t, filepath.Join(c, "task.md"), "just prose, no heading")
	if it, _ := parseTaskFolder(c, stateTodo); it.Title != "2026-01-01-c" {
		t.Errorf("id fallback title = %q", it.Title)
	}
	// no task.md → not a task
	empty := filepath.Join(dir, stateTodo, "not-a-task")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := parseTaskFolder(empty, stateTodo); ok {
		t.Errorf("folder without task.md should not parse as a task")
	}
}

func TestReadTaskTreeAndCounts(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-02-second", "task.md"), "# Second todo\n- [ ] a\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-first", "task.md"), "# First todo\n")
	writeTaskFile(t, filepath.Join(root, stateInProgress, "2026-01-03-active", "task.md"), "# Active one\n- [x] done\n- [ ] todo\n")
	writeTaskFile(t, filepath.Join(root, stateBlocked, "2026-01-04-stuck", "task.md"), "# Stuck\n")
	writeTaskFile(t, filepath.Join(root, stateBlocked, "2026-01-04-stuck", "decision.md"), "# Decision: ?\n")
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-05-shipped", "task.md"), "# Shipped\n")
	// a stray non-task folder is ignored
	if err := os.MkdirAll(filepath.Join(root, stateTodo, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}

	items := readTaskTree(root)
	if len(items) != 5 {
		t.Fatalf("want 5 tasks, got %d", len(items))
	}
	// sorted by state (todo first), then ID within state
	if items[0].ID != "2026-01-01-first" || items[1].ID != "2026-01-02-second" {
		t.Errorf("todo not sorted by id: %s, %s", items[0].ID, items[1].ID)
	}
	if items[0].State != stateTodo || items[4].State != stateDone {
		t.Errorf("state ordering wrong: %s … %s", items[0].State, items[4].State)
	}

	c, active := taskTreeCounts(items)
	if c.Todo != 2 || c.Doing != 1 || c.Blocked != 1 || c.Done != 1 {
		t.Errorf("counts = %+v", c)
	}
	if active != "Active one" { // first in_progress wins over todo
		t.Errorf("active = %q, want the in_progress task", active)
	}

	// blocked task carries its decision; the in_progress one has 1/2 subtasks done
	var stuck, act taskItem
	for _, it := range items {
		switch it.ID {
		case "2026-01-04-stuck":
			stuck = it
		case "2026-01-03-active":
			act = it
		}
	}
	if !stuck.HasDecision {
		t.Errorf("blocked task should report HasDecision")
	}
	if len(act.Subtasks) != 2 || act.doneSubtasks() != 1 {
		t.Errorf("active subtasks = %v (done %d)", act.Subtasks, act.doneSubtasks())
	}
}

// TestReadTaskTreeDedupesTornMove: a task read in two state dirs (a torn read of an in-flight
// os.Rename) is counted ONCE, at its earliest-lifecycle state — so counts can't inflate and a
// finishing task can't flash a false "✓ done" in the dashboard.
func TestReadTaskTreeDedupesTornMove(t *testing.T) {
	root := t.TempDir()
	// the SAME id present in both 10_in_progress and xx_done, as during a mid-read move
	writeTaskFile(t, filepath.Join(root, stateInProgress, "2026-01-01-x", "task.md"), "# X\n")
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-01-x", "task.md"), "# X\n")
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-02-a", "task.md"), "# A\n")
	writeTaskFile(t, filepath.Join(root, stateDone, "2026-01-03-b", "task.md"), "# B\n")

	items := readTaskTree(root)
	if len(items) != 3 {
		t.Fatalf("torn move double-counted: %d items, want 3 distinct ids", len(items))
	}
	if c, _ := taskTreeCounts(items); c.total() != 3 {
		t.Errorf("counts inflated by a torn read: total=%d, want 3", c.total())
	}
	var x taskItem
	for _, it := range items {
		if it.ID == "2026-01-01-x" {
			x = it
		}
	}
	if x.State != stateInProgress {
		t.Errorf("torn-move task attributed to %q, want %q (earliest-lifecycle)", x.State, stateInProgress)
	}
}

func TestQueueCountsAndSource(t *testing.T) {
	// queueCounts/queueHasTodo/wsTaskSource all read the .agent/tasks tree.
	ws := t.TempDir()
	dir := filepath.Join(ws, tasksRoot)
	writeTaskFile(t, filepath.Join(dir, stateTodo, "2026-01-01-a", "task.md"), "# one\n")
	writeTaskFile(t, filepath.Join(dir, stateInProgress, "2026-01-02-b", "task.md"), "# two\n")
	if got := wsTaskSource(ws); got != dir {
		t.Fatalf("wsTaskSource = %q, want %q", got, dir)
	}
	c, active := queueCounts(dir)
	if c.Todo != 1 || c.Doing != 1 {
		t.Errorf("counts = %+v", c)
	}
	if active != "two" {
		t.Errorf("active = %q", active)
	}
	if !queueHasTodo(dir) {
		t.Errorf("queueHasTodo should be true with a todo/ task")
	}
	// A missing/empty tree reads as all-zero, no panic.
	if c0, a0 := queueCounts(filepath.Join(t.TempDir(), "nope")); c0.total() != 0 || a0 != "" {
		t.Errorf("missing tree = %+v %q, want zero/empty", c0, a0)
	}
}

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	writeTaskFile(t, filepath.Join(src, "a", "x.md"), "hello")
	writeTaskFile(t, filepath.Join(src, "b.txt"), "world")
	dst := filepath.Join(t.TempDir(), "out")
	if err := copyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(filepath.Join(dst, "a", "x.md")); got != "hello" {
		t.Errorf("nested file = %q", got)
	}
	if got := readFileString(filepath.Join(dst, "b.txt")); got != "world" {
		t.Errorf("top file = %q", got)
	}
}

func TestSplitTodoFolders(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, ".agent", "tasks")
	for _, id := range []string{"2026-01-01-a", "2026-01-02-b", "2026-01-03-c", "2026-01-04-d", "2026-01-05-e"} {
		writeTaskFile(t, filepath.Join(root, stateTodo, id, "task.md"), "# "+id+"\n")
	}
	// an in_progress task must be excluded from the split
	writeTaskFile(t, filepath.Join(root, stateInProgress, "2026-01-06-active", "task.md"), "# active\n")

	written, counts, total, err := splitTodoFolders(repo, root, []string{"1", "2"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5 (in_progress excluded)", total)
	}
	if counts[0] != 3 || counts[1] != 2 {
		t.Errorf("round-robin counts = %v, want [3 2]", counts)
	}
	if written[0] != filepath.Join(".agent", "tasks.1") || written[1] != filepath.Join(".agent", "tasks.2") {
		t.Errorf("written slice dirs = %v", written)
	}
	if !fileExists(filepath.Join(repo, ".agent", "tasks.1", stateTodo, "2026-01-01-a", "task.md")) {
		t.Error("slice 1 missing the first round-robin task's copied task.md")
	}
	// source is untouched (the slices are copies)
	if c, _ := taskTreeCounts(readTaskTree(root)); c.Todo != 5 || c.Doing != 1 {
		t.Errorf("source tree changed by split: %+v", c)
	}
	// each slice is itself a valid folder-mode queue
	if c, _ := taskTreeCounts(readTaskTree(filepath.Join(repo, ".agent", "tasks.1"))); c.Todo != 3 {
		t.Errorf("slice 1 todo = %d, want 3", c.Todo)
	}

	// more slices than tasks → the trailing bucket is empty (written == "")
	repo2 := t.TempDir()
	root2 := filepath.Join(repo2, ".agent", "tasks")
	writeTaskFile(t, filepath.Join(root2, stateTodo, "only", "task.md"), "# only\n")
	w, c, tot, _ := splitTodoFolders(repo2, root2, []string{"1", "2"})
	if tot != 1 || c[0] != 1 || c[1] != 0 || w[0] == "" || w[1] != "" {
		t.Errorf("uneven split: written=%v counts=%v total=%d", w, c, tot)
	}
}

func TestTaskTreeCountsActiveFallsBackToTodo(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, stateTodo, "2026-01-01-only", "task.md"), "# Only todo\n")
	_, active := taskTreeCounts(readTaskTree(root))
	if active != "Only todo" {
		t.Errorf("active = %q, want the todo task when none in progress", active)
	}
}
