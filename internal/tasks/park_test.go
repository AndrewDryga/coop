package tasks

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// addBlocked creates a task and blocks it, which writes an unanswered decision.md.
func addBlocked(t *testing.T, root, title string) string {
	t.Helper()
	if code, err := tasksFolderAdd(root, []string{title}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	var id string
	for _, it := range mustReadTaskTree(t, root) {
		if it.Title == title {
			id = it.ID
		}
	}
	if code, err := tasksFolderBlock(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("block: code=%d err=%v", code, err)
	}
	return id
}

// Parking answers a decision with "not now": the folder moves to the backlog with its log and open
// decision, preflight leaves it there, and promote brings it back blocked with the question.
func TestParkMovesABlockedTaskToTheBacklogAndPromoteBringsTheQuestionBack(t *testing.T) {
	root := t.TempDir()
	id := addBlocked(t, root, "Choose the storage")

	if code, err := tasksFolderPark(root, []string{id, "not this quarter"}); code != 0 || err != nil {
		t.Fatalf("park: code=%d err=%v", code, err)
	}
	dir := filepath.Join(root, StateBacklog, id)
	if !pathExists(filepath.Join(dir, "decision.md")) || pathExists(filepath.Join(root, StateBlocked, id)) {
		t.Fatal("the task should sit in the backlog with its decision.md, and nowhere else")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "log.md")); !strings.Contains(string(data), "Parked in the backlog by the owner: not this quarter") {
		t.Errorf("log.md should record the owner's reason:\n%s", data)
	}
	if ids := mustUnblockResolved(t, []string{root}); len(ids) != 0 {
		t.Fatalf("preflight re-queued %v, want the parked task left alone", ids)
	}

	if code, err := backlogFolderPromote(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("promote: code=%d err=%v", code, err)
	}
	if !pathExists(filepath.Join(root, StateBlocked, id)) {
		t.Error("an open decision should bring the promoted task back to blocked, not todo")
	}
}

// The owner's earlier answer can be "keep it parked". Preflight unblocks any blocked task with a
// filled Resolution, which is exactly why such a task must live in the backlog instead.
func TestPreflightLeavesAParkedTaskWithAnAnswerAlone(t *testing.T) {
	root := t.TempDir()
	id := addBlocked(t, root, "Build the eval suites")
	if err := recordResolution(filepath.Join(root, StateBlocked, id, "decision.md"), "keep it parked"); err != nil {
		t.Fatal(err)
	}
	if code, err := tasksFolderPark(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("park: code=%d err=%v", code, err)
	}
	if ids := mustUnblockResolved(t, []string{root}); len(ids) != 0 {
		t.Fatalf("preflight re-queued %v from the backlog", ids)
	}
	if !pathExists(filepath.Join(root, StateBacklog, id)) {
		t.Fatal("the answered, parked task should still be in the backlog")
	}
	if code, err := backlogFolderPromote(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("promote: code=%d err=%v", code, err)
	}
	if !pathExists(filepath.Join(root, StateTodo, id)) {
		t.Error("a task whose decision has an answer should be promoted to todo")
	}
}

func TestParkRefusesATaskThatIsNotBlocked(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"Fix login retries"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := mustReadTaskTree(t, root)[0].ID
	code, err := tasksFolderPark(root, []string{id})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "not blocked") {
		t.Fatalf("park of a todo task = (%d, %v), want 1 and a 'not blocked' error", code, err)
	}
	if !pathExists(filepath.Join(root, StateTodo, id)) {
		t.Error("a refused park must leave the task where it was")
	}
	if code, _ := tasksFolderPark(root, nil); code != 2 {
		t.Errorf("park with no id = %d, want 2 (usage)", code)
	}
}

// :b in the decisions browser parks the question on screen and moves on.
func TestDecisionBrowserParksWithB(t *testing.T) {
	root := t.TempDir()
	id := addBlocked(t, root, "Pick a queue")
	var decisions []Item
	for _, it := range mustReadTaskTree(t, root) {
		if it.State == StateBlocked {
			decisions = append(decisions, it)
		}
	}
	var out bytes.Buffer
	if code, err := runDecisionBrowser(decisionRefs(root, "", decisions), strings.NewReader(":b\n"), &out); code != 0 || err != nil {
		t.Fatalf("browser: code=%d err=%v", code, err)
	}
	if !strings.Contains(out.String(), ":b park in backlog") {
		t.Errorf("the browser should offer :b:\n%s", out.String())
	}
	if !pathExists(filepath.Join(root, StateBacklog, id)) {
		t.Error(":b should park the task in the backlog")
	}
}
