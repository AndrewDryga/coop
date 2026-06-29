package cli

import (
	"strings"
	"testing"
)

// A single queue leads with just the progress bar (no path label), then the actionable tasks
// grouped by state. Done tasks are a header count, never a list.
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
	joined := strings.Join(tasksWatchFrame([]watchQueue{{rel: ".agent/tasks", items: items, counts: c}}, 0), "\n")

	if !strings.Contains(joined, "2/6 done") {
		t.Errorf("header should show 2/6 done:\n%s", joined)
	}
	// A single queue carries no path label — the bar leads, no text to its left.
	if strings.Contains(joined, ".agent/tasks") {
		t.Errorf("a single queue should not show its path label:\n%s", joined)
	}
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

// Several queues each get a progress line labeled with their path (left of the bar), so a monorepo
// watcher can tell them apart.
func TestTasksWatchFrameMultiQueue(t *testing.T) {
	api := []taskItem{{ID: "a", Title: "API thing", State: stateTodo}}
	docs := []taskItem{{ID: "b", Title: "Docs thing", State: stateDone}}
	ca, _ := taskTreeCounts(api)
	cd, _ := taskTreeCounts(docs)
	joined := strings.Join(tasksWatchFrame([]watchQueue{
		{rel: ".agent/tasks.api", items: api, counts: ca},
		{rel: ".agent/tasks.docs", items: docs, counts: cd},
	}, 0), "\n")
	for _, want := range []string{".agent/tasks.api", ".agent/tasks.docs", "0/1 done", "1/1 done", "API thing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("multi-queue frame missing %q:\n%s", want, joined)
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
	joined := strings.Join(tasksWatchFrame([]watchQueue{{rel: ".agent/tasks", items: items, counts: c}}, 0), "\n")
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
	for _, c := range []taskCounts{{}, {Done: 5}} {
		if !tasksDrained(c) {
			t.Errorf("tasksDrained(%+v) = false, want true", c)
		}
	}
	for _, c := range []taskCounts{{Todo: 1}, {Doing: 1}, {Blocked: 1}, {Done: 5, Blocked: 1}} {
		if tasksDrained(c) {
			t.Errorf("tasksDrained(%+v) = true, want false", c)
		}
	}
}
