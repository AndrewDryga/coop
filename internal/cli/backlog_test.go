package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// The backlog drawer (xx_backlog) must be INVISIBLE to the lifecycle machinery: `coop backlog add`
// creates it, but readTaskTree (which the loop, counters, `coop tasks`, and the Stop hook all walk)
// must never surface it — that's the whole point of keeping Backlog out of taskstate.All.
func TestBacklogAddIsolatedFromQueue(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"a shiny idea"}, stateBacklog, "backlog add"); code != 0 || err != nil {
		t.Fatalf("backlog add: code=%d err=%v", code, err)
	}
	// It's really in xx_backlog, with a task.md.
	bl := readBacklog(root)
	if len(bl) != 1 || bl[0].State != stateBacklog {
		t.Fatalf("readBacklog = %+v, want one xx_backlog item", bl)
	}
	if !strings.HasSuffix(bl[0].ID, "-a-shiny-idea") {
		t.Errorf("backlog id slug = %q", bl[0].ID)
	}
	if !fileExists(filepath.Join(bl[0].Dir, "task.md")) {
		t.Errorf("backlog item has no task.md at %s", bl[0].Dir)
	}
	// The lifecycle tree, and therefore every counter/lister/loop check, ignores it entirely.
	if items := readTaskTree(root); len(items) != 0 {
		t.Fatalf("readTaskTree sees %d items, want 0 — backlog must be invisible to the queue", len(items))
	}
	if c, _ := taskTreeCounts(readTaskTree(root)); c.Todo+c.Doing+c.Blocked+c.Done != 0 {
		t.Errorf("counts should be all-zero with only a backlog item, got %+v", c)
	}
}

// promote is the reason the drawer exists: an idea becomes live work by a folder move to 00_todo/
// (id and files intact), after which it's a normal, claimable task. And a bad id fails loudly.
func TestBacklogPromote(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"promote me"}, stateBacklog, "backlog add"); code != 0 || err != nil {
		t.Fatalf("backlog add: code=%d err=%v", code, err)
	}
	id := readBacklog(root)[0].ID

	if code, err := backlogFolderPromote(root, []string{"promote"}); code != 0 || err != nil { // substring match
		t.Fatalf("promote: code=%d err=%v", code, err)
	}
	if len(readBacklog(root)) != 0 {
		t.Error("after promote, backlog should be empty")
	}
	items := readTaskTree(root)
	if len(items) != 1 || items[0].State != stateTodo || items[0].ID != id {
		t.Fatalf("after promote: %+v, want the same id in todo", items)
	}
	// The promoted task is now a normal task — claimable.
	if code, err := tasksFolderMove(root, []string{id}, stateInProgress, "claim", "claimed"); code != 0 || err != nil {
		t.Fatalf("claim after promote: code=%d err=%v", code, err)
	}

	// Failure paths: no id → usage (2); absent id → not-found (1).
	if code, _ := backlogFolderPromote(root, nil); code != 2 {
		t.Errorf("promote with no id = %d, want 2 (usage)", code)
	}
	if code, err := backlogFolderPromote(root, []string{"nope"}); code == 0 || err == nil {
		t.Errorf("promote of an absent id = (%d, %v), want an error", code, err)
	}
}

// The drawer is walled off from the active id-commands: a backlog id is invisible to findTask/claim
// (so an un-promoted idea can't be accidentally worked), yet resolvable via findBacklogTask.
func TestBacklogIsolatedFromActiveCommands(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"parked"}, stateBacklog, "backlog add"); code != 0 || err != nil {
		t.Fatalf("backlog add: code=%d err=%v", code, err)
	}
	id := readBacklog(root)[0].ID

	if _, err := findTask(root, id); err == nil {
		t.Error("findTask must NOT resolve a backlog id (the active tree excludes xx_backlog)")
	}
	if code, err := tasksFolderMove(root, []string{id}, stateInProgress, "claim", "claimed"); code == 0 && err == nil {
		t.Error("claim must refuse a backlog id — it isn't live work until promoted")
	}
	if _, err := findBacklogTask(root, id); err != nil {
		t.Errorf("findBacklogTask should resolve the backlog id: %v", err)
	}
}

// rm drops an idea; the destroyGate refuses without --yes on a non-TTY (this test), and a bad id fails.
func TestBacklogRemove(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"drop me"}, stateBacklog, "backlog add"); code != 0 || err != nil {
		t.Fatalf("backlog add: code=%d err=%v", code, err)
	}
	id := readBacklog(root)[0].ID

	// No --yes and no TTY → refused, item survives.
	if code, err := backlogFolderRemove(root, []string{id}); code != 2 || err == nil {
		t.Errorf("rm without --yes = (%d, %v), want a refusal (2)", code, err)
	}
	if len(readBacklog(root)) != 1 {
		t.Fatal("a refused rm must not delete the item")
	}
	// With --yes → gone.
	if code, err := backlogFolderRemove(root, []string{id, "--yes"}); code != 0 || err != nil {
		t.Fatalf("rm --yes: code=%d err=%v", code, err)
	}
	if len(readBacklog(root)) != 0 {
		t.Error("after rm --yes, backlog should be empty")
	}
	// No id → usage.
	if code, _ := backlogFolderRemove(root, []string{"--yes"}); code != 2 {
		t.Errorf("rm with no id = %d, want 2 (usage)", code)
	}
}

// An id is globally unique across the whole tree: adding to the backlog an id that already lives in a
// lifecycle state (or vice-versa) is refused, so a later promote can't collide and no lookup shadows.
func TestBacklogAddIDCollision(t *testing.T) {
	root := t.TempDir()
	// Pre-place a todo task, then try to backlog the same id (same day + slug ⇒ same id).
	if code, err := tasksFolderAdd(root, []string{"same title"}, stateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("todo add: code=%d err=%v", code, err)
	}
	if code, err := tasksFolderAdd(root, []string{"same title"}, stateBacklog, "backlog add"); code == 0 || err == nil {
		t.Errorf("backlog add of an id already in todo should collide, got (%d, %v)", code, err)
	}
	// And the reverse: an id already in the backlog can't be re-added to todo.
	root2 := t.TempDir()
	if code, err := tasksFolderAdd(root2, []string{"idea"}, stateBacklog, "backlog add"); code != 0 || err != nil {
		t.Fatalf("backlog add: code=%d err=%v", code, err)
	}
	if code, err := tasksFolderAdd(root2, []string{"idea"}, stateTodo, "tasks add"); code == 0 || err == nil {
		t.Errorf("todo add of an id already in the backlog should collide, got (%d, %v)", code, err)
	}
}

// In a monorepo, `coop backlog rm|promote <id>` finds the item in whichever queue's drawer holds it —
// and a duplicate id across queues is an error, not a silent pick.
func TestQueueOfBacklogTask(t *testing.T) {
	repo := t.TempDir()
	a := filepath.Join(repo, "svc-a", ".agent", "tasks")
	b := filepath.Join(repo, "svc-b", ".agent", "tasks")
	writeTaskFile(t, filepath.Join(a, stateBacklog, "2026-01-01-only-a", "task.md"), "# a\n")
	writeTaskFile(t, filepath.Join(b, stateBacklog, "2026-01-01-only-b", "task.md"), "# b\n")
	writeTaskFile(t, filepath.Join(a, stateBacklog, "2026-01-01-dup", "task.md"), "# dupa\n")
	writeTaskFile(t, filepath.Join(b, stateBacklog, "2026-01-01-dup", "task.md"), "# dupb\n")
	rels := []string{"svc-a/.agent/tasks", "svc-b/.agent/tasks"}

	if rel, err := queueOfBacklogTask(repo, rels, "only-b"); err != nil || rel != "svc-b/.agent/tasks" {
		t.Errorf("queueOfBacklogTask(only-b) = (%q, %v), want svc-b", rel, err)
	}
	if _, err := queueOfBacklogTask(repo, rels, "2026-01-01-dup"); err == nil {
		t.Error("a duplicate backlog id across queues should error, not pick one")
	}
	if _, err := queueOfBacklogTask(repo, rels, "missing"); err == nil {
		t.Error("an absent backlog id should error")
	}
}
