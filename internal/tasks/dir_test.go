package tasks

import (
	"os"
	"path/filepath"
	"strings"
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
	fields, body := SplitFrontmatter("---\nid: x-1\ntitle: Do the thing\nlabels: [a, b]\n# a comment\n---\n\n# Heading\nbody text\n")
	if fields["id"] != "x-1" || fields["title"] != "Do the thing" || fields["labels"] != "[a, b]" {
		t.Fatalf("fields = %v", fields)
	}
	if _, ok := fields["a comment"]; ok {
		t.Errorf("comment line leaked into fields: %v", fields)
	}
	if got := firstH1(body); got != "Heading" {
		t.Errorf("body H1 = %q", got)
	}

	// A YAML-quoted scalar is unquoted — a title opening with a flow indicator ([) MUST be quoted,
	// so it arrives with quotes and must be stripped (else the list shows literal "…").
	if fq, _ := SplitFrontmatter("---\ntitle: \"[DEAD] approvals reason\"\n---\nbody\n"); fq["title"] != "[DEAD] approvals reason" {
		t.Errorf("quoted title not unquoted: %q", fq["title"])
	}

	// No header → all body, no fields.
	f2, b2 := SplitFrontmatter("# Just a title\ntext")
	if len(f2) != 0 || b2 != "# Just a title\ntext" {
		t.Errorf("no-header parse: fields=%v body=%q", f2, b2)
	}
	// Unterminated header → treat as body, don't hang/panic.
	f3, b3 := SplitFrontmatter("---\nid: x\nno closing fence\n")
	if len(f3) != 0 || b3 == "" {
		t.Errorf("unterminated header should fall back to body: fields=%v", f3)
	}
}

func TestUnquoteScalar(t *testing.T) {
	cases := map[string]string{
		`"[DEAD] foo"`: `[DEAD] foo`, // double-quoted (required for a leading [)
		`'it''s ok'`:   `it's ok`,    // single-quoted: '' is an escaped '
		`plain`:        `plain`,      // unquoted → as-is
		`[a, b]`:       `[a, b]`,     // a bare flow value isn't quoted → untouched
		`""`:           ``,           // empty quoted
	}
	for in, want := range cases {
		if got := unquoteScalar(in); got != want {
			t.Errorf("unquoteScalar(%q) = %q, want %q", in, got, want)
		}
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
	a := filepath.Join(dir, StateTodo, "2026-01-01-a")
	writeTaskFile(t, filepath.Join(a, "task.md"), "---\ntitle: From frontmatter\n---\n# From H1\n")
	if it, ok := mustParseTaskFolder(t, a, StateTodo); !ok || it.Title != "From frontmatter" || it.ID != "2026-01-01-a" {
		t.Fatalf("frontmatter title: ok=%v item=%+v", ok, it)
	}
	// no frontmatter title → H1
	b := filepath.Join(dir, StateTodo, "2026-01-01-b")
	writeTaskFile(t, filepath.Join(b, "task.md"), "# Heading title\nbody")
	if it, _ := mustParseTaskFolder(t, b, StateTodo); it.Title != "Heading title" {
		t.Errorf("H1 title = %q", it.Title)
	}
	// neither → id
	c := filepath.Join(dir, StateTodo, "2026-01-01-c")
	writeTaskFile(t, filepath.Join(c, "task.md"), "just prose, no heading")
	if it, _ := mustParseTaskFolder(t, c, StateTodo); it.Title != "2026-01-01-c" {
		t.Errorf("id fallback title = %q", it.Title)
	}
	// no task.md → not a task
	empty := filepath.Join(dir, StateTodo, "not-a-task")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := mustParseTaskFolder(t, empty, StateTodo); ok {
		t.Errorf("folder without task.md should not parse as a task")
	}
}

func TestReadTaskTreeAndCounts(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-02-second", "task.md"), "# Second todo\n- [ ] a\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-first", "task.md"), "# First todo\n")
	writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-01-03-active", "task.md"), "# Active one\n- [x] done\n- [ ] todo\n")
	writeTaskFile(t, filepath.Join(root, StateBlocked, "2026-01-04-stuck", "task.md"), "# Stuck\n")
	writeTaskFile(t, filepath.Join(root, StateBlocked, "2026-01-04-stuck", "decision.md"), "# Decision: ?\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-05-shipped", "task.md"), "# Shipped\n")
	// a stray non-task folder is ignored
	if err := os.MkdirAll(filepath.Join(root, StateTodo, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}

	items := mustReadTaskTree(t, root)
	if len(items) != 5 {
		t.Fatalf("want 5 tasks, got %d", len(items))
	}
	// sorted by state (todo first), then ID within state
	if items[0].ID != "2026-01-01-first" || items[1].ID != "2026-01-02-second" {
		t.Errorf("todo not sorted by id: %s, %s", items[0].ID, items[1].ID)
	}
	if items[0].State != StateTodo || items[4].State != StateDone {
		t.Errorf("state ordering wrong: %s … %s", items[0].State, items[4].State)
	}

	c, active := TaskTreeCounts(items)
	if c.Todo != 2 || c.Doing != 1 || c.Blocked != 1 || c.Done != 1 {
		t.Errorf("counts = %+v", c)
	}
	if active != "Active one" { // first in_progress wins over todo
		t.Errorf("active = %q, want the in_progress task", active)
	}

	// blocked task carries its decision; the in_progress one has 1/2 subtasks done
	var stuck, act Item
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

func TestReadTaskTreeRejectsPersistentDuplicate(t *testing.T) {
	root := t.TempDir()
	// the SAME id present in both 10_in_progress and 99_done, as during a mid-read move
	writeTaskFile(t, filepath.Join(root, StateInProgress, "2026-01-01-x", "task.md"), "# X\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-01-x", "task.md"), "# X\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-02-a", "task.md"), "# A\n")
	writeTaskFile(t, filepath.Join(root, StateDone, "2026-01-03-b", "task.md"), "# B\n")

	if _, err := ReadTaskTree(root); err == nil || !strings.Contains(err.Error(), "multiple lifecycle states") {
		t.Fatalf("persistent duplicate error = %v", err)
	}
}

func TestQueueCounts(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, TasksRoot)
	writeTaskFile(t, filepath.Join(dir, StateTodo, "2026-01-01-a", "task.md"), "# one\n")
	writeTaskFile(t, filepath.Join(dir, StateInProgress, "2026-01-02-b", "task.md"), "# two\n")
	c, active := mustQueueCounts(t, dir)
	if c.Todo != 1 || c.Doing != 1 {
		t.Errorf("counts = %+v", c)
	}
	if active != "two" {
		t.Errorf("active = %q", active)
	}
	// A missing/empty tree reads as all-zero, no panic.
	if c0, a0 := mustQueueCounts(t, filepath.Join(t.TempDir(), "nope")); c0.Total() != 0 || a0 != "" {
		t.Errorf("missing tree = %+v %q, want zero/empty", c0, a0)
	}
}

func TestTaskTreeCountsActiveFallsBackToTodo(t *testing.T) {
	root := t.TempDir()
	writeTaskFile(t, filepath.Join(root, StateTodo, "2026-01-01-only", "task.md"), "# Only todo\n")
	_, active := TaskTreeCounts(mustReadTaskTree(t, root))
	if active != "Only todo" {
		t.Errorf("active = %q, want the todo task when none in progress", active)
	}
}
