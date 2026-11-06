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

// parkNoted reports whether log holds a park note that ends with tail, whoever parked the task.
func parkNoted(log, tail string) bool {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, parkedBy) && strings.HasSuffix(strings.TrimSpace(line), tail) {
			return true
		}
	}
	return false
}

// A park names who parked the task the way a claim names its holder: an agent with its PID, or the
// person at the terminal as user@host. The note is Coop's own, never a worker's log entry.
func TestParkRecordsWhoParkedTheTask(t *testing.T) {
	t.Setenv("USER", "alice")
	user, host := taskOwnerIdentity()
	for _, tc := range []struct {
		name   string
		actor  ClaimActor
		reason string
		want   string
	}{
		{"an agent", ClaimActor{Label: "claude", PID: 4242}, "after the release", "Parked in the backlog by claude (PID 4242): after the release"},
		{"a person", ClaimActor{}, "", "Parked in the backlog by " + user + "@" + host + "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTaskFile(t, filepath.Join(root, StateTodo, "2026-11-06-login", "task.md"), "# Login\n")
			task, _ := mustCurrentTask(t, root, "2026-11-06-login")
			if err := parkTask(root, task, tc.reason, tc.actor); err != nil {
				t.Fatal(err)
			}
			log, _ := os.ReadFile(filepath.Join(root, StateBacklog, task.ID, "log.md"))
			if !strings.Contains(string(log), tc.want) {
				t.Errorf("log.md = %q, want the note %q", log, tc.want)
			}
			if !coopLogNote(tc.want) || LogHasEntry(string(log)) {
				t.Errorf("the park note %q reads as a worker's entry", tc.want)
			}
		})
	}
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
	if data, _ := os.ReadFile(filepath.Join(dir, "log.md")); !parkNoted(string(data), ": not this quarter") {
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

// Setting a todo task aside needs no question: park moves it to the backlog with the reason in its
// log and writes no decision.md, and promote returns it to todo.
func TestParkSetsATodoTaskAsideWithoutADecision(t *testing.T) {
	root := t.TempDir()
	if code, err := tasksFolderAdd(root, []string{"Fix login retries"}, StateTodo, "tasks add"); code != 0 || err != nil {
		t.Fatalf("add: code=%d err=%v", code, err)
	}
	id := mustReadTaskTree(t, root)[0].ID

	if code, err := tasksFolderPark(root, []string{id, "after", "the", "release"}); code != 0 || err != nil {
		t.Fatalf("park of a todo task = (%d, %v), want it parked", code, err)
	}
	dir := filepath.Join(root, StateBacklog, id)
	if !pathExists(dir) || pathExists(filepath.Join(root, StateTodo, id)) {
		t.Fatal("the task should sit in the backlog, and nowhere else")
	}
	if pathExists(filepath.Join(dir, "decision.md")) {
		t.Error("parking a todo task must not invent a decision")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "log.md")); !parkNoted(string(data), ": after the release") {
		t.Errorf("log.md should record the owner's reason:\n%s", data)
	}
	if items := mustReadTaskTree(t, root); len(items) != 0 {
		t.Errorf("the loop's queue should be empty once the task is parked, got %v", items)
	}

	if code, err := backlogFolderPromote(root, []string{id}); code != 0 || err != nil {
		t.Fatalf("promote: code=%d err=%v", code, err)
	}
	if !pathExists(filepath.Join(root, StateTodo, id)) {
		t.Error("a parked todo task should be promoted back to todo")
	}
}

// A task someone is working on is released first, so the handoff lands in its state.md; a done
// task is the archive. Both are refused and stay where they are.
func TestParkRefusesATaskInProgressOrDone(t *testing.T) {
	root := t.TempDir()
	body := "---\ntitle: Busy\n---\n# Busy\n"
	writeTaskFile(t, filepath.Join(root, StateInProgress, "busy", "task.md"), body)
	writeTaskFile(t, filepath.Join(root, StateDone, "shipped", "task.md"), body)

	code, err := tasksFolderPark(root, []string{"busy"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "release it first with coop tasks release busy") {
		t.Fatalf("park of an in-progress task = (%d, %v), want 1 and the release hint", code, err)
	}
	code, err = tasksFolderPark(root, []string{"shipped"})
	if code != 1 || err == nil || !strings.Contains(err.Error(), "only a todo or blocked task can be parked") {
		t.Fatalf("park of a done task = (%d, %v), want 1 and a refusal", code, err)
	}
	if !pathExists(filepath.Join(root, StateInProgress, "busy")) || !pathExists(filepath.Join(root, StateDone, "shipped")) {
		t.Error("a refused park must leave the task where it was")
	}
	if pathExists(filepath.Join(root, StateBacklog)) {
		t.Error("a refused park must not touch the backlog")
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
	if code, err := runDecisionBrowser("", decisionRefs(root, "", decisions), strings.NewReader(":b\n"), &out); code != 0 || err != nil {
		t.Fatalf("browser: code=%d err=%v", code, err)
	}
	if !strings.Contains(out.String(), ":b park in backlog") {
		t.Errorf("the browser should offer :b:\n%s", out.String())
	}
	if !pathExists(filepath.Join(root, StateBacklog, id)) {
		t.Error(":b should park the task in the backlog")
	}
}
