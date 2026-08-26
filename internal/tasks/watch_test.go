package tasks

import (
	"strings"
	"testing"

	"github.com/AndrewDryga/coop/internal/ui"
)

// A per-source (per-queue) line shows its blocked count when any — so a parked queue is visible in the
// breakdown, not just the overall header; with none, the blocked tail is omitted.
func TestSourceLineShowsBlocked(t *testing.T) {
	line := sourceLine(ui.Palette{}, "api", 3, TaskCounts{Todo: 1, Done: 5, Blocked: 2})
	if !strings.Contains(line, "5/8") { // 5 done of 8 total
		t.Errorf("sourceLine should show done/total (5/8): %q", line)
	}
	if !strings.Contains(line, "2 blocked") {
		t.Errorf("sourceLine should show the blocked count: %q", line)
	}
	if l := sourceLine(ui.Palette{}, "api", 3, TaskCounts{Todo: 1, Done: 3}); strings.Contains(l, "blocked") {
		t.Errorf("sourceLine with 0 blocked should omit the blocked tail: %q", l)
	}
}

func TestTaskWatchBarsShowTinyActiveShare(t *testing.T) {
	c := TaskCounts{Todo: 99, Doing: 1}
	for _, line := range []string{tasksProgressLine(ui.Palette{}, c), sourceLine(ui.Palette{}, "api", 3, c)} {
		if !strings.Contains(line, "█") {
			t.Errorf("task-watch overall and source bars should keep a visible active cell: %q", line)
		}
	}
}

func merge(items []Item) []mergedTask {
	out := make([]mergedTask, len(items))
	for i, t := range items {
		out[i] = mergedTask{Item: t}
	}
	return out
}

// A single source leads with just the progress bar + per-state colored counter, then the actionable
// tasks grouped by state. Done tasks are a header count, never a list; nothing is fork-attributed.
func TestTasksWatchFrame(t *testing.T) {
	items := []Item{
		{ID: "a", Title: "Wire auth", State: StateInProgress},
		{ID: "b", Title: "Add retries", State: StateTodo},
		{ID: "c", Title: "Bump deps", State: StateTodo},
		{ID: "d", Title: "Pick a queue backend", State: StateBlocked},
		{ID: "e", Title: "shipped thing", State: StateDone},
		{ID: "f", Title: "another done", State: StateDone},
	}
	c, _ := TaskTreeCounts(items)
	joined := strings.Join(tasksWatchFrame([]watchSource{{label: ".agent/tasks", counts: c}}, merge(items), 0, 80), "\n")

	for _, want := range []string{"2 todo", "1 in_progress", "1 blocked", "2 done"} {
		if !strings.Contains(joined, want) {
			t.Errorf("counter should show %q:\n%s", want, joined)
		}
	}
	// A single source carries no path label and no fork attribution — the bar leads.
	if strings.Contains(joined, ".agent/tasks") || strings.Contains(joined, "←") {
		t.Errorf("a single source should show no label and no attribution:\n%s", joined)
	}
	for _, want := range []string{"in_progress", "todo", "blocked", "Wire auth", "Add retries", "Pick a queue backend"} {
		if !strings.Contains(joined, want) {
			t.Errorf("frame missing %q:\n%s", want, joined)
		}
	}
	for _, gone := range []string{"shipped thing", "another done"} {
		if strings.Contains(joined, gone) {
			t.Errorf("done task %q must not be listed:\n%s", gone, joined)
		}
	}
}

func TestTaskWatchMarkersStayCompact(t *testing.T) {
	p := ui.Palette{}
	t.Setenv("COOP_SPINNER", "1")
	for spin, want := range ui.CompactSpinFrames {
		if got := taskWatchMarker(p, StateInProgress, spin); got != want {
			t.Errorf("in-progress marker at spin %d = %q, want Corner Run %q", spin, got, want)
		}
	}
	for _, tc := range []struct {
		state string
		want  string
	}{
		{StateBlocked, "⚑"},
		{StateTodo, "○"},
	} {
		if got := taskWatchMarker(p, tc.state, 0); got != tc.want || len([]rune(got)) != 1 {
			t.Errorf("taskWatchMarker(%q) = %q, want one-column %q", tc.state, got, tc.want)
		}
	}

	line := mergedQueue(p, []mergedTask{{Item: Item{Title: "Task title", State: StateInProgress}}}, 0, 80)[0]
	if line != "  ◰ Task title · unleased" {
		t.Errorf("compact task row = %q, want %q", line, "  ◰ Task title · unleased")
	}
	for _, tc := range []struct {
		lease TaskLeaseObservation
		want  string
	}{
		{TaskLeaseObservation{State: leaseBusy, Provider: "claude"}, "busy claude"},
		{TaskLeaseObservation{State: leaseStalled, Provider: "codex"}, "stalled codex"},
	} {
		line := mergedQueue(p, []mergedTask{{
			Item: Item{Title: "Task title", State: StateInProgress}, lease: tc.lease,
		}}, 0, 80)[0]
		if !strings.Contains(line, tc.want) {
			t.Errorf("lease row = %q, want %q", line, tc.want)
		}
	}

	t.Setenv("COOP_SPINNER", "0")
	if got := taskWatchMarker(p, StateInProgress, 4); got != ui.CompactSpinFrames[0] {
		t.Errorf("frozen task marker = %q, want %q", got, ui.CompactSpinFrames[0])
	}
}

func TestTaskWatchTitleUsesAvailableTerminalWidth(t *testing.T) {
	title := "Log every cloud error envelope instead of silently swallowing the useful diagnostic details"
	merged := []mergedTask{{
		Item:  Item{ID: "errors", Title: title, State: StateInProgress},
		fork:  "worker",
		lease: TaskLeaseObservation{State: leaseBusy, Provider: "claude"},
	}}
	rowAt := func(width int) string {
		frame := tasksWatchFrame(nil, merged, 0, width)
		return frame[len(frame)-1]
	}

	wide := rowAt(120)
	if !strings.Contains(wide, title) {
		t.Errorf("wide task row should use columns beyond the old fixed title cap: %q", wide)
	}

	const narrowWidth = 52
	narrow := rowAt(narrowWidth)
	if strings.Contains(narrow, title) || !strings.Contains(narrow, "…") {
		t.Errorf("narrow task row should elide its title: %q", narrow)
	}
	if want := "  ← worker · busy claude"; !strings.HasSuffix(narrow, want) {
		t.Errorf("narrow task row should preserve suffix %q: %q", want, narrow)
	}
	if got, max := len([]rune(narrow)), narrowWidth-1; got > max {
		t.Errorf("narrow task row width = %d, want at most %d: %q", got, max, narrow)
	}
}

// Several sources — a local queue and a fork — each get a labeled progress line, and an in-progress
// task a fork claimed is tagged with it.
func TestTasksWatchFrameMergesForks(t *testing.T) {
	local := []Item{{ID: "a", Title: "Local thing", State: StateTodo}}
	forked := []Item{{ID: "b", Title: "Wire auth", State: StateInProgress}}
	cl, _ := TaskTreeCounts(local)
	cf, _ := TaskTreeCounts(forked)
	sources := []watchSource{{label: ".agent/tasks", counts: cl}, {label: "api", counts: cf}}
	merged := []mergedTask{{Item: local[0]}, {Item: forked[0], fork: "api"}}
	joined := strings.Join(tasksWatchFrame(sources, merged, 0, 80), "\n")

	for _, want := range []string{".agent/tasks", "api", "Local thing", "Wire auth", "← api"} {
		if !strings.Contains(joined, want) {
			t.Errorf("multi-source frame missing %q:\n%s", want, joined)
		}
	}
}

// A long backlog is capped per state so the board stays glanceable, with a "+N more" tail.
func TestTasksWatchFrameCapsLongBacklog(t *testing.T) {
	var items []Item
	for i := 0; i < 11; i++ {
		items = append(items, Item{ID: string(rune('a' + i)), Title: "task " + string(rune('A'+i)), State: StateTodo})
	}
	c, _ := TaskTreeCounts(items)
	joined := strings.Join(tasksWatchFrame([]watchSource{{label: ".agent/tasks", counts: c}}, merge(items), 0, 80), "\n")
	if !strings.Contains(joined, "+3 more") { // 11 todo, cap 8 → 3 elided
		t.Errorf("a >8 backlog should elide with '+3 more':\n%s", joined)
	}
	if strings.Contains(joined, "task K") { // the 11th (index 10) is past the cap
		t.Errorf("tasks past the cap must not be listed:\n%s", joined)
	}
}

// The board is one queue-ordered list (no per-state group headers), and the cap NEVER hides active
// work: with a long todo backlog plus an in-progress and a blocked task, both of those always show
// and only the cold todo tail elides.
func TestTasksWatchQueueNeverElidesActive(t *testing.T) {
	items := []Item{
		{ID: "run", Title: "RUNNING NOW", State: StateInProgress},
		{ID: "blk", Title: "BLOCKED DECISION", State: StateBlocked},
	}
	for i := 0; i < 30; i++ { // a backlog well past the todo cap
		items = append(items, Item{ID: string(rune('a' + i)), Title: "todo " + string(rune('A'+i)), State: StateTodo})
	}
	c, _ := TaskTreeCounts(items)
	joined := strings.Join(tasksWatchFrame([]watchSource{{label: ".agent/tasks", counts: c}}, merge(items), 0, 80), "\n")
	if !strings.Contains(joined, "RUNNING NOW") {
		t.Errorf("in-progress task must never be elided behind the cap:\n%s", joined)
	}
	if !strings.Contains(joined, "BLOCKED DECISION") {
		t.Errorf("blocked task must never be elided behind the cap:\n%s", joined)
	}
	if !strings.Contains(joined, "more") { // 30 todo > cap → the todo tail elides
		t.Errorf("the todo backlog should still elide with a +N more tail:\n%s", joined)
	}
	// One flat list — no "todo (30)" / "in_progress (1)" group-header format.
	for _, hdr := range []string{"todo (30)", "in_progress (1)", "blocked (1)"} {
		if strings.Contains(joined, hdr) {
			t.Errorf("expected a single list, found a group header %q:\n%s", hdr, joined)
		}
	}
}

// mergedCounts tallies the deduped set; tasksDrained is the auto-exit condition (nothing todo, in
// progress, or blocked — every task done, or none). A blocked or unfinished queue is NOT drained.
func TestTasksDrained(t *testing.T) {
	if c := mergedCounts([]mergedTask{
		{Item: Item{State: StateDone}},
		{Item: Item{State: StateBlocked}},
	}); c.Done != 1 || c.Blocked != 1 {
		t.Errorf("mergedCounts = %+v, want Done=1 Blocked=1", c)
	}
	for _, c := range []TaskCounts{{}, {Done: 5}} {
		if !tasksDrained(c) {
			t.Errorf("tasksDrained(%+v) = false, want true", c)
		}
	}
	for _, c := range []TaskCounts{{Todo: 1}, {Doing: 1}, {Blocked: 1}, {Done: 5, Blocked: 1}} {
		if tasksDrained(c) {
			t.Errorf("tasksDrained(%+v) = true, want false", c)
		}
	}
}

func TestTasksWatchSettling(t *testing.T) {
	drained := TaskCounts{Done: 3}       // nothing todo/in-progress/blocked
	live := TaskCounts{Todo: 1, Done: 2} // work remains
	cases := []struct {
		name               string
		c                  TaskCounts
		running            int
		sawActive, sawFork bool
		want               bool
	}{
		{"local queue all done, no forks → settle (exit)", drained, 0, false, false, true},
		{"work remains → keep watching", live, 0, true, true, false},
		{"a fork is running → keep watching", drained, 1, true, true, false},
		{"fork launched but not working yet (startup window) → keep watching", drained, 0, false, true, false},
		{"fork worked, then finished → settle (exit)", drained, 0, true, true, true},
	}
	for _, tc := range cases {
		if got := tasksWatchSettling(tc.c, tc.running, tc.sawActive, tc.sawFork); got != tc.want {
			t.Errorf("%s: tasksWatchSettling = %v, want %v", tc.name, got, tc.want)
		}
	}
}
