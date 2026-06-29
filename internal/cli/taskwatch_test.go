package cli

import (
	"strings"
	"testing"
)

// tasksWatchFrame is task-centric: a header with overall progress, then the actionable tasks
// grouped by state (in progress / todo / blocked). Done tasks are a header count, never a list.
func TestTasksWatchFrame(t *testing.T) {
	items := []taskItem{
		{ID: "a", Title: "Wire auth", State: stateInProgress},
		{ID: "b", Title: "Add retries", State: stateTodo},
		{ID: "c", Title: "Bump deps", State: stateTodo},
		{ID: "d", Title: "Pick a queue backend", State: stateBlocked},
		{ID: "e", Title: "shipped thing", State: stateDone},
		{ID: "f", Title: "another done", State: stateDone},
	}
	c, _ := taskTreeCounts(items)
	joined := strings.Join(tasksWatchFrame("acme", items, c, 0), "\n")

	// Header: repo name + overall done/total (2 of 6).
	if !strings.Contains(joined, "acme tasks") || !strings.Contains(joined, "2/6 done") {
		t.Errorf("header should show repo + 2/6 done:\n%s", joined)
	}
	// The actionable sections appear with their counts; the in-flight + pending + blocked titles show.
	for _, want := range []string{"in_progress", "todo", "blocked", "Wire auth", "Add retries", "Bump deps", "Pick a queue backend"} {
		if !strings.Contains(joined, want) {
			t.Errorf("frame missing %q:\n%s", want, joined)
		}
	}
	// Done is a count, not a list — its titles never appear (they'd only grow the board).
	for _, gone := range []string{"shipped thing", "another done"} {
		if strings.Contains(joined, gone) {
			t.Errorf("done task %q must not be listed:\n%s", gone, joined)
		}
	}
}

// A long backlog is capped per state so the board stays glanceable, with a "+N more" tail.
func TestTasksWatchFrameCapsLongBacklog(t *testing.T) {
	var items []taskItem
	for i := 0; i < 11; i++ {
		items = append(items, taskItem{ID: string(rune('a' + i)), Title: "task " + string(rune('A'+i)), State: stateTodo})
	}
	c, _ := taskTreeCounts(items)
	joined := strings.Join(tasksWatchFrame("repo", items, c, 0), "\n")
	if !strings.Contains(joined, "+3 more") { // 11 todo, cap 8 → 3 elided
		t.Errorf("a >8 backlog should elide with '+3 more':\n%s", joined)
	}
	if strings.Contains(joined, "task K") { // the 11th (index 10) is past the cap
		t.Errorf("tasks past the cap must not be listed:\n%s", joined)
	}
}

// tasksDrained is the auto-exit condition: true only when nothing is todo, in progress, or blocked
// (every task done, or none). A blocked or unfinished queue is NOT drained — the watch keeps going.
func TestTasksDrained(t *testing.T) {
	drained := []taskCounts{{}, {Done: 5}}
	for _, c := range drained {
		if !tasksDrained(c) {
			t.Errorf("tasksDrained(%+v) = false, want true", c)
		}
	}
	notDrained := []taskCounts{{Todo: 1}, {Doing: 1}, {Blocked: 1}, {Done: 5, Blocked: 1}}
	for _, c := range notDrained {
		if tasksDrained(c) {
			t.Errorf("tasksDrained(%+v) = true, want false", c)
		}
	}
}
