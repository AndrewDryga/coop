package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/testutil/starttoken"
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
	b := boardCounts{TaskCounts: c}
	for _, line := range []string{tasksProgressLine(ui.Colored(), b), sourceLine(ui.Colored(), "api", 3, c)} {
		if !strings.Contains(line, "█") {
			t.Errorf("task-watch overall and source bars should keep a visible active cell: %q", line)
		}
	}
	// Without color every segment is the same block, so the counts carry the states alone.
	for _, line := range []string{tasksProgressLine(ui.Palette{}, b), sourceLine(ui.Palette{}, "api", 3, c)} {
		if strings.ContainsAny(line, "█░[") {
			t.Errorf("a colorless line draws a bar it cannot tell apart: %q", line)
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

// ago is a moment d before now, half a minute further back so a minute count cannot tick over
// while a test runs.
func ago(d time.Duration) time.Time { return time.Now().Add(-d - 30*time.Second) }

func subtasks(done, total int) []bool {
	s := make([]bool, total)
	for i := range done {
		s[i] = true
	}
	return s
}

func inProgress(id, title string, done, total int) Item {
	return Item{ID: id, Title: title, State: StateInProgress, Subtasks: subtasks(done, total)}
}

// The board the owner approved on 2026-11-05 (the task's spec.md), line for line: Now, Stopped,
// Needs you and Up next; a detail line under every task being worked or stopped and a fix line
// under every stopped one; one line for each task waiting in a list.
func TestTaskWatchBoardMatchesTheApprovedLayout(t *testing.T) {
	t.Setenv("COOP_SPINNER", "1")
	network := "Codex cannot start because this project asks for network access that has not been approved"
	merged := []mergedTask{
		{Item: inProgress("a", "Hand a task to a human when the worker re-closes the same unmet finding", 1, 3),
			lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex", Held: 6*time.Minute + 30*time.Second}},
		{Item: inProgress("b", "Accept a JSON-string options list in tasks_block", 1, 3),
			fork: "sweep-a", phase: ForkAssignmentReviewing, live: true, agent: "codex"},
		{Item: inProgress("c", "Ask loop workers for a why-body in commits", 0, 2),
			fork: "sweep-b", phase: ForkAssignmentPaused, agent: "codex", stopReason: network},
		{Item: inProgress("d", "Document that secret-shaped paths are unreadable inside a box", 0, 2),
			fork: "sweep-c", phase: ForkAssignmentWorking, agent: "codex", stopReason: network},
		{Item: inProgress("e", "Keep Go formatting gates out of ignored task environments", 0, 3), agent: "codex", since: ago(25 * time.Minute)},
		{Item: Item{ID: "f", Title: "Move co-op to coop.emisar.dev", State: StateBlocked}, since: ago(16 * time.Hour)},
		{Item: Item{ID: "g", Title: "Prepare Protectorate repository and website migration", State: StateBlocked}, since: ago(19 * time.Hour)},
		{Item: Item{ID: "h", Title: "Run the provider guides on Linux with real agents in Cloud Shell", State: StateBlocked}, since: ago(5 * time.Hour)},
		{Item: Item{ID: "i", Title: "Keep template placeholders out of a completed task's state.md and log.md", State: StateTodo}},
		{Item: Item{ID: "j", Title: "Keep the loop running when a review subject left the archive", State: StateTodo}},
	}
	for i := range 30 {
		merged = append(merged, mergedTask{Item: Item{ID: fmt.Sprintf("later-%02d", i), Title: "Later work", State: StateTodo}})
	}
	for i := range 22 {
		merged = append(merged, mergedTask{Item: Item{ID: fmt.Sprintf("done-%02d", i), Title: "Shipped", State: StateDone}, doneToday: true})
	}
	merged = append(merged, mergedTask{Item: Item{ID: "archive", Title: "Shipped long ago", State: StateDone}})

	want := []string{
		"32 todo · 2 working · 3 stopped · 3 blocked · 22 done today",
		"",
		"Now",
		"  ◳ Hand a task to a human when the worker re-closes the same unmet finding",
		"    1/3 · codex · working for 6m",
		"  ◳ Accept a JSON-string options list in tasks_block",
		"    1/3 · codex in fork sweep-a · final review",
		"",
		"Stopped",
		"  ✕ Ask loop workers for a why-body in commits",
		"    0/2 · codex in fork sweep-b · network access not approved",
		"    to fix, run: coop approve, then coop fork sweep-b --loop -d",
		"",
		"  ✕ Document that secret-shaped paths are unreadable inside a box",
		"    0/2 · codex in fork sweep-c · network access not approved",
		"    to fix, run: coop approve, then coop fork sweep-c --loop -d",
		"",
		"  ✕ Keep Go formatting gates out of ignored task environments",
		"    0/3 · codex · worker gone 25m ago",
		"    to resume, run: coop loop",
		"",
		"Needs you · answer them: coop tasks decisions -i",
		"  ⚑ Run the provider guides on Linux with real agents in Cloud Shell · 5h",
		"  ⚑ Move co-op to coop.emisar.dev · 16h",
		"  ⚑ Prepare Protectorate repository and website migration · 19h",
		"",
		"Up next · 32",
		"  ○ Keep template placeholders out of a completed task's state.md and log.md",
		"  ○ Keep the loop running when a review subject left the archive",
		"  + 30 more",
	}
	got := tasksWatchFrame([]watchSource{{label: ".agent/tasks"}}, merged, 1, 120)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("board =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Every situation the board can derive from what coop already records, as its title, detail and fix
// lines, in the section it belongs to. Stopped on purpose, a usage-limit wait and the service-launch
// wait are not recorded yet, so they have no rows here.
func TestTaskWatchEdgeCaseRows(t *testing.T) {
	const title = "Wire auth"
	task := inProgress("2026-11-05-wire-auth", title, 0, 3)
	for _, tc := range []struct {
		name    string
		task    mergedTask
		section string
		lines   []string
	}{
		{"worker gone", mergedTask{Item: task, agent: "codex", since: ago(25 * time.Minute)}, "Stopped",
			[]string{"  ✕ " + title, "    0/3 · codex · worker gone 25m ago", "    to resume, run: coop loop"}},
		{"worker gone, never seen", mergedTask{Item: task}, "Stopped",
			[]string{"  ✕ " + title, "    0/3 · worker gone", "    to resume, run: coop loop"}},
		{"no heartbeat while the process exists",
			mergedTask{Item: task, lease: TaskLeaseObservation{State: leaseStalled, Provider: "codex", Held: time.Hour, Quiet: 6*time.Minute + 10*time.Second}}, "Now",
			[]string{"  ◳ " + title, "    0/3 · codex · no heartbeat for 6m"}},
		{"held lease", mergedTask{Item: task, lease: TaskLeaseObservation{State: leaseBusy, Provider: "claude", Held: 2*time.Hour + 5*time.Minute}}, "Now",
			[]string{"  ◳ " + title, "    0/3 · claude · working for 2h 5m"}},
		{"live claim", mergedTask{Item: task, owner: "claimed by alice", live: true, agent: "alice", since: ago(6 * time.Minute)}, "Now",
			[]string{"  ◳ " + title, "    0/3 · alice · working for 6m"}},
		{"claim left by a closed session", mergedTask{Item: task, owner: "claimed by codex", agent: "codex"}, "Stopped",
			[]string{"  ✕ " + title, "    0/3 · codex · claimed by a session that has ended", "    to free it, run: coop tasks release 2026-11-05-wire-auth"}},
		{"claimant on another system", mergedTask{Item: task, owner: "claimed by codex", agent: "codex", system: "Linux"}, "Now",
			[]string{"  ◳ " + title, "    0/3 · codex · running on Linux"}},
		{"fork working", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentWorking, live: true, agent: "codex", since: ago(6 * time.Minute)}, "Now",
			[]string{"  ◳ " + title, "    0/3 · codex in fork sweep-a · working for 6m"}},
		{"fork final review", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentReviewing, live: true, agent: "codex"}, "Now",
			[]string{"  ◳ " + title, "    0/3 · codex in fork sweep-a · final review"}},
		{"fork loop on another system", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentWorking, live: true, system: "Linux"}, "Now",
			[]string{"  ◳ " + title, "    0/3 · in fork sweep-a · running on Linux"}},
		{"fork loop refused, reason recorded",
			mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentPaused, agent: "codex", stopReason: "this project asks for unrestricted internet access, which has not been approved"}, "Stopped",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · network access not approved", "    to fix, run: coop approve, then coop fork sweep-b --loop -d"}},
		{"fork loop lost its sign-in",
			mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentWorking, agent: "codex", stopReason: "the provider no longer accepts this account's saved sign-in; run coop login"}, "Stopped",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · sign-in no longer accepted", "    to fix, run: coop login, then coop fork sweep-b --loop -d"}},
		{"fork loop ended, reason only in its log",
			mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentWorking, agent: "codex", stopReason: "Codex cannot start because the box image is missing"}, "Stopped",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · the box image is missing", "    to resume, run: coop fork sweep-b --loop -d"}},
		{"fork loop ended, no reason anywhere", mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentPreparing, agent: "codex"}, "Stopped",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · loop ended unexpectedly", "    to resume, run: coop fork sweep-b --loop -d"}},
		{"fork finished, waiting for merge", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentReady, since: ago(2 * time.Hour)}, "Needs you · answer them: coop tasks decisions -i",
			[]string{"  ⚑ " + title + " · ready to merge · 2h"}},
		{"fork final review with its loop gone", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentReviewing}, "Needs you · answer them: coop tasks decisions -i",
			[]string{"  ⚑ " + title + " · ready to merge"}},
		{"blocked a day ago", mergedTask{Item: Item{ID: "b", Title: title, State: StateBlocked}, since: ago(50 * time.Hour)}, "Needs you · answer them: coop tasks decisions -i",
			[]string{"  ⚑ " + title + " · 2d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COOP_SPINNER", "1")
			sections := boardSections(ui.Palette{}, []mergedTask{tc.task}, 1, 120)
			want := append([]string{tc.section}, tc.lines...)
			if len(sections) != 1 || !reflect.DeepEqual(sections[0], want) {
				t.Errorf("board =\n%q\nwant one section\n%q", sections, want)
			}
		})
	}
}

// Detail lines are dim gray; a task waiting but alive is yellow; a stopped one has a red marker and
// state and its commands bold; a list header is gray after its dot, and a wait past a day is yellow.
func TestTaskWatchRowColors(t *testing.T) {
	p := ui.Colored()
	board := strings.Join(slices.Concat(boardSections(p, []mergedTask{
		{Item: inProgress("a", "Busy", 0, 1), lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex", Held: time.Minute}},
		{Item: inProgress("b", "Quiet", 0, 1), lease: TaskLeaseObservation{State: leaseStalled, Provider: "codex", Quiet: 6 * time.Minute}},
		{Item: inProgress("c", "Gone", 0, 1)},
		{Item: Item{ID: "d", Title: "Fresh question", State: StateBlocked}, since: ago(5 * time.Hour)},
		{Item: Item{ID: "e", Title: "Old question", State: StateBlocked}, since: ago(30 * time.Hour)},
	}, 0, 120)...), "\n")
	for _, want := range []string{
		p.Faint("0/1 · codex · ") + p.Faint("working for 1m"),
		p.Yellow("no heartbeat for 6m"),
		p.Red(stoppedMarker) + " Gone", p.Red("worker gone"), "to resume, run: " + p.Bold("coop loop"),
		"Needs you" + p.Gray(" · answer them: coop tasks decisions -i"),
		p.Dim(" · ") + p.Dim("5h"), p.Dim(" · ") + p.Yellow("30h"),
	} {
		if !strings.Contains(board, want) {
			t.Errorf("board is missing %q:\n%q", want, board)
		}
	}
}

// Active work always shows in full; Needs you shows three and Up next two, then how many more.
func TestTaskWatchCapsTheListsNeverTheWork(t *testing.T) {
	var merged []mergedTask
	for i := range 6 {
		merged = append(merged,
			mergedTask{Item: inProgress(fmt.Sprintf("now-%d", i), fmt.Sprintf("Now %d", i), 0, 1), lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex"}},
			mergedTask{Item: inProgress(fmt.Sprintf("stop-%d", i), fmt.Sprintf("Stopped %d", i), 0, 1)},
			mergedTask{Item: Item{ID: fmt.Sprintf("ask-%d", i), Title: fmt.Sprintf("Ask %d", i), State: StateBlocked}},
			mergedTask{Item: Item{ID: fmt.Sprintf("todo-%d", i), Title: fmt.Sprintf("Todo %d", i), State: StateTodo}})
	}
	board := strings.Join(tasksWatchFrame(nil, merged, 0, 120), "\n")
	for i := range 6 {
		for _, title := range []string{"Now %d\n", "Stopped %d\n"} {
			if want := fmt.Sprintf(title, i); !strings.Contains(board, want) {
				t.Errorf("active task %q was capped:\n%s", want, board)
			}
		}
	}
	for _, want := range []string{"Ask 2\n  + 3 more\n", "Todo 1\n  + 4 more"} {
		if !strings.Contains(board, want) {
			t.Errorf("board is missing %q:\n%s", want, board)
		}
	}
	for _, capped := range []string{"Ask 3", "Todo 2"} {
		if strings.Contains(board, capped) {
			t.Errorf("%q is past its section's cap:\n%s", capped, board)
		}
	}
}

// The single placement rule: a fork is named only as "in fork <name>" in a detail line (and in the
// command that restarts it), never on a title, a header or a list row, and nothing is right-aligned.
// Section headers carry no reason text, every section's first task sits right under its header, and
// only Stopped tasks are set apart by a blank line. Nothing sends the reader to the fork's logs.
func TestTaskWatchPlacesEveryFactByOneRule(t *testing.T) {
	merged := []mergedTask{
		{Item: inProgress("a", "Working in a fork", 1, 2), fork: "alpha", phase: ForkAssignmentWorking, live: true, agent: "codex"},
		{Item: inProgress("b", "Stopped in a fork", 0, 2), fork: "beta", phase: ForkAssignmentPaused, agent: "codex"},
		{Item: inProgress("c", "Stopped here", 0, 2)},
		{Item: inProgress("d", "Ready in a fork", 2, 2), fork: "gamma", phase: ForkAssignmentReady, since: ago(time.Hour)},
		{Item: Item{ID: "e", Title: "Blocked in a fork", State: StateBlocked}, fork: "delta", phase: ForkAssignmentBlocked},
		{Item: Item{ID: "f", Title: "Queued for a fork", State: StateTodo}, fork: "epsilon", phase: ForkAssignmentBlocked},
	}
	frame := tasksWatchFrame(nil, merged, 0, 120)
	headers := map[string]bool{"Now": true, "Stopped": true, "Needs you · answer them: coop tasks decisions -i": true, "Up next · 1": true}
	seen := 0
	for i, line := range frame {
		if strings.Contains(line, "coop fork logs") || strings.Contains(line, "←") {
			t.Errorf("line %d sends the reader elsewhere or points at a fork: %q", i, line)
		}
		if strings.Contains(strings.TrimLeft(line, " "), "  ") {
			t.Errorf("line %d pads text into a column: %q", i, line)
		}
		for _, fork := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
			rest := strings.ReplaceAll(strings.ReplaceAll(line, "in fork "+fork, ""), "coop fork "+fork+" --loop -d", "")
			if strings.Contains(rest, fork) || (strings.Contains(line, "in fork "+fork) && !strings.HasPrefix(line, "    ")) {
				t.Errorf("line %d names fork %s outside a detail line's \"in fork\": %q", i, fork, line)
			}
		}
		switch {
		case i == 0 || strings.HasPrefix(line, "  "):
		case line == "":
			// A blank line ends a section, or sets one Stopped task apart from the next.
			next, prev := frame[min(i+1, len(frame)-1)], frame[i-1]
			if !headers[next] && !(strings.HasPrefix(prev, "    to ") && strings.HasPrefix(next, "  ✕ ")) {
				t.Errorf("blank line %d is neither before a header nor between two Stopped tasks", i)
			}
		case !headers[line]:
			t.Errorf("line %d is a header with more than a count or its command: %q", i, line)
		default:
			seen++
			if i+1 >= len(frame) || !strings.HasPrefix(frame[i+1], "  ") {
				t.Errorf("header %q is not followed by its first task", line)
			}
		}
	}
	if seen != len(headers) {
		t.Errorf("frame has %d headers, want %d:\n%s", seen, len(headers), strings.Join(frame, "\n"))
	}
	if !slices.Contains(frame, "    1/2 · codex in fork alpha · working") || !slices.Contains(frame, "    0/2 · codex in fork beta · loop ended unexpectedly") {
		t.Errorf("a fork task lost its detail line:\n%s", strings.Join(frame, "\n"))
	}
}

// A title is cut only by the terminal's width, at the end of its line: the facts after it go first,
// and no row is wider than the board leaves room for.
func TestTaskWatchTitleUsesAvailableTerminalWidth(t *testing.T) {
	title := "Log every cloud error envelope instead of silently swallowing the useful diagnostic details"
	merged := []mergedTask{{
		Item:  Item{ID: "errors", Title: title, State: StateBlocked},
		queue: "api/.agent/tasks", since: ago(5 * time.Hour),
	}}
	rowAt := func(width int) string { return boardSections(ui.Palette{}, merged, 0, width)[0][1] }
	if wide := rowAt(132); wide != "  ⚑ "+title+" · api · 5h" {
		t.Errorf("wide row = %q, want the title, its queue and its age", wide)
	}
	const narrowWidth = 52
	narrow := rowAt(narrowWidth)
	if want := "  ⚑ " + title[:narrowWidth-1-4-1] + "…"; narrow != want {
		t.Errorf("narrow row = %q, want the title cut at the width %q", narrow, want)
	}
	stopped := []mergedTask{{Item: inProgress("x", title, 0, 2), fork: "sweep-b", phase: ForkAssignmentWorking, stopReason: "this project asks for network access that has not been approved"}}
	for _, line := range boardSections(ui.Palette{}, stopped, 0, narrowWidth)[0] {
		if got := len([]rune(line)); got > narrowWidth-1 {
			t.Errorf("row width = %d, want at most %d: %q", got, narrowWidth-1, line)
		}
	}
}

func TestTaskWatchReadsUpdatedChecklistCounts(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, TasksRoot)
	path := filepath.Join(root, StateInProgress, "checklist", "task.md")
	for _, marker := range []string{" ", "x"} {
		writeTaskFile(t, path, "# Checklist task\n\n## Subtasks\n- ["+marker+"] implement\n- [ ] verify\n```\n- [x] example only\n```\n")
		out := captureStdout(t, func() {
			if code, err := TasksWatch(Host{}, repo, []string{root}); code != 0 || err != nil {
				t.Errorf("watch = (%d, %v)", code, err)
			}
		})
		// Nothing holds it, so its worker is gone; the task's own files say when it was last touched.
		want := "\n  ✕ Checklist task\n    0/2 · worker gone <1m ago\n    to resume, run: coop loop\n"
		if marker == "x" {
			want = strings.Replace(want, "0/2", "1/2", 1)
		}
		if !strings.Contains(out, want) {
			t.Errorf("watch did not read the current checklist %q:\n%s", want, out)
		}
	}
}

// Several sources — a local queue and a fork — each get a labeled progress line.
func TestTasksWatchFrameMergesForks(t *testing.T) {
	local := []Item{{ID: "a", Title: "Local thing", State: StateTodo}}
	forked := []Item{{ID: "b", Title: "Wire auth", State: StateInProgress}}
	cl, _ := TaskTreeCounts(local)
	cf, _ := TaskTreeCounts(forked)
	sources := []watchSource{{label: ".agent/tasks", counts: cl}, {label: "api", counts: cf}}
	merged := []mergedTask{{Item: local[0]}, {Item: forked[0], fork: "api", phase: ForkAssignmentWorking, live: true}}
	joined := strings.Join(tasksWatchFrame(sources, merged, 0, 80), "\n")
	for _, want := range []string{"  .agent/tasks  0/1", "  api           0/1", "Local thing", "Wire auth\n    in fork api · working"} {
		if !strings.Contains(joined, want) {
			t.Errorf("multi-source frame missing %q:\n%s", want, joined)
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
		name    string
		c       TaskCounts
		running int
		want    bool
	}{
		{"queue all done, nothing running → settle (exit)", drained, 0, true},
		{"work remains → keep watching", live, 0, false},
		{"a fork or box is running → keep watching", drained, 1, false},
	}
	for _, tc := range cases {
		if got := tasksWatchSettling(tc.c, tc.running); got != tc.want {
			t.Errorf("%s: tasksWatchSettling = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIdlePersistentForkDoesNotHoldTheWatcherOpen(t *testing.T) {
	snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{{Name: "idle", WorkspaceValid: true}}}
	if snapshotHasVisibleActivity(snapshot) {
		t.Fatal("an idle persistent fork was presented as current work")
	}
	if !tasksWatchSettling(TaskCounts{Done: 1}, 0) {
		t.Fatal("an idle persistent fork would keep a drained watcher open forever")
	}
}

func TestReservationOnlyForkIsNotTaskWatchActivity(t *testing.T) {
	snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{{
		Name: "idle-session",
		Reservation: &forkspace.WorkspaceReservation{
			Kind: forkspace.WorkspaceReservationRemoteSession, OwnerID: "session-owner",
		},
	}}}
	if snapshotHasVisibleActivity(snapshot) {
		t.Fatal("durable session ownership was presented as current task work")
	}
	frame := strings.Join(tasksWatchFrameWithSnapshot(nil, nil, snapshot, 0, 120), "\n")
	for _, unwanted := range []string{"forks", "idle-session", "session-owner"} {
		if strings.Contains(frame, unwanted) {
			t.Fatalf("reservation-only fork rendered %q:\n%s", unwanted, frame)
		}
	}
}

func TestReservedForkWithAssignmentRendersTaskActivity(t *testing.T) {
	snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{{
		Name: "worker", Assignments: 1,
		Reservation: &forkspace.WorkspaceReservation{
			Kind: forkspace.WorkspaceReservationRemoteSession, OwnerID: "session-owner",
		},
	}}}
	if !snapshotHasVisibleActivity(snapshot) {
		t.Fatal("a fork assignment was hidden by its session reservation")
	}
	frame := strings.Join(tasksWatchFrameWithSnapshot(nil, nil, snapshot, 0, 120), "\n")
	if !strings.Contains(frame, "worker · 1 assigned task") {
		t.Fatalf("assigned fork did not render its task activity:\n%s", frame)
	}
	for _, unwanted := range []string{"remote-session", "session-owner"} {
		if strings.Contains(frame, unwanted) {
			t.Fatalf("assigned fork exposed reservation detail %q:\n%s", unwanted, frame)
		}
	}
}

func TestTaskWatchKeepsForkWorkVisibleWithoutAnExecution(t *testing.T) {
	tests := []struct {
		name string
		fork ProjectForkSnapshot
		want string
	}{
		{name: "detached loop", fork: ProjectForkSnapshot{Name: "worker", DetachedRunning: true}, want: "detached loop running"},
		{name: "loop another system started", fork: ProjectForkSnapshot{Name: "worker", DetachedSystem: "Linux"}, want: "background loop started on Linux"},
		{name: "cleanup", fork: ProjectForkSnapshot{Name: "worker", CleanupPending: true}, want: "cleanup-pending"},
		{name: "assignment", fork: ProjectForkSnapshot{Name: "worker", Assignments: 2}, want: "2 assigned tasks"},
		{name: "candidate", fork: ProjectForkSnapshot{Name: "worker", Candidate: true}, want: "ready"},
		{name: "landing", fork: ProjectForkSnapshot{Name: "worker", PendingLand: true}, want: "landing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{tt.fork}}
			if !snapshotHasVisibleActivity(snapshot) {
				t.Fatal("fork work was not treated as visible activity")
			}
			frame := strings.Join(tasksWatchFrameWithSnapshot(nil, nil, snapshot, 0, 120), "\n")
			if !strings.Contains(frame, "worker · "+tt.want) {
				t.Fatalf("fork work did not render %q:\n%s", tt.want, frame)
			}
		})
	}
}

// A fork loop that a coop on another operating system started, as one in a Linux container sharing
// this folder with macOS, is recorded with a pid this kernel cannot check, whether that pid is
// missing here or belongs to a process here, and coop fork stop refuses to clean it up from here.
// The board called it cleanup-pending and --json set cleanup_pending, while coop fork ls named the
// system. Both now name the system as fork ls does; a record this system wrote whose worker is gone
// or replaced is still left for cleanup.
func TestTaskWatchNamesTheSystemThatStartedAForkLoop(t *testing.T) {
	system := forkspace.ProcTokenSystem(starttoken.Foreign())
	const gone = 2147483645 // above any pid_max, so no process here has it
	for _, tc := range []struct {
		name  string
		pid   int
		token string
	}{
		{name: "another system's record, pid missing here", pid: gone, token: starttoken.Foreign()},
		{name: "another system's record, pid alive here", pid: os.Getpid(), token: starttoken.Foreign()},
		{name: "this system's record, worker gone", pid: gone, token: starttoken.Stale()},
		{name: "this system's record, pid reused", pid: os.Getpid(), token: starttoken.Stale()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, status := "cleanup-pending", `"detached_running":false,"cleanup_pending":true`
			if tc.token == starttoken.Foreign() {
				state = "background loop started on " + system
				status = `"detached_running":false,"detached_system":"` + system + `","cleanup_pending":false`
			}
			repo := filepath.Join(t.TempDir(), "project")
			root := filepath.Join(repo, TasksRoot)
			if err := ScaffoldStateDirs(root); err != nil {
				t.Fatal(err)
			}
			_, identity := testAssignmentFork(t, repo, "perf")
			worker := forkspace.WorkerState{Pid: tc.pid, Token: tc.token, Generation: identity.Generation}
			wantFork := `{"name":"perf","identity":{"name":"perf","generation":"` + string(identity.Generation) + `"},"workspace_valid":true,`
			wantFork += status + `,"assignments":0,"candidate":false,"pending_land":false,"active_executions":0}`
			if err := forkspace.WriteWorkerState(repo, "perf", worker); err != nil {
				t.Fatal(err)
			}
			watch := func(jsonOutput bool) string {
				return captureStdout(t, func() {
					if code, err := TasksWatch(Host{}, repo, []string{root}, jsonOutput); code != 0 || err != nil {
						t.Errorf("watch = (%d, %v)", code, err)
					}
				})
			}

			if board, want := watch(false), "\nforks\n  perf · "+state+"\n"; !strings.HasSuffix(board, want) {
				t.Errorf("watch printed:\n%s\nwant it to end with:%s", board, want)
			}
			out := watch(true)
			var snapshot struct {
				Forks    []json.RawMessage `json:"forks"`
				Problems []string          `json:"problems"`
			}
			if err := json.Unmarshal([]byte(out), &snapshot); err != nil {
				t.Fatalf("bad json: %v\n%s", err, out)
			}
			var forks []string
			for _, fork := range snapshot.Forks {
				var compact bytes.Buffer
				if err := json.Compact(&compact, fork); err != nil {
					t.Fatal(err)
				}
				forks = append(forks, compact.String())
			}
			if len(forks) != 1 || forks[0] != wantFork || len(snapshot.Problems) != 0 {
				t.Errorf("watch --json forks =\n%s\nproblems: %q\nwant the one fork\n%s", strings.Join(forks, "\n"), snapshot.Problems, wantFork)
			}
		})
	}
}

func TestTaskWatchHidesTodoClaims(t *testing.T) {
	for _, task := range []mergedTask{
		{owner: "claimed by alice", agent: "alice"},
		{fork: "worker", phase: ForkAssignmentBlocked, agent: "codex"},
	} {
		task.Item = Item{Title: "Queued work", State: StateTodo}
		task.queue = "api/.agent/tasks"
		lines := boardSections(ui.Palette{}, []mergedTask{task}, 0, 80)[0]
		if want := []string{"Up next · 1", "  ○ Queued work · api"}; !reflect.DeepEqual(lines, want) {
			t.Errorf("TODO rows = %q, want %q", lines, want)
		}
	}
}

func TestTaskWatchOmitsClaimProcessIDs(t *testing.T) {
	for _, state := range []string{StateTodo, StateInProgress} {
		for _, process := range []string{"live", "stopped", "unbound"} {
			t.Run(state+"/"+process, func(t *testing.T) {
				repo := t.TempDir()
				root := filepath.Join(repo, TasksRoot)
				const id = "claimed-task"
				writeTaskFile(t, filepath.Join(root, state, id, "task.md"), "# Queued work\n")
				record := humanOwnerRecordForTest(id)
				item, _ := mustCurrentTask(t, root, id)
				instance, err := EnsureTaskInstance(root, item)
				if err != nil {
					t.Fatal(err)
				}
				record.Task = &instance
				if process != "unbound" {
					actor := selfActor(t, "codex")
					if process == "stopped" {
						var stop func()
						actor, stop = sleeperActor(t, "codex")
						stop()
					}
					record.Actor, record.ActorPID, record.ActorStart = actor.Label, actor.PID, actor.StartToken
				}
				if err := writeTaskOwnerRecord(root, record); err != nil {
					t.Fatal(err)
				}
				before, _ := taskOwned(t, root, id)
				watch := func(jsonOutput bool) string {
					return captureStdout(t, func() {
						if code, err := TasksWatch(Host{}, repo, []string{root}, jsonOutput); code != 0 || err != nil {
							t.Errorf("watch = (%d, %v)", code, err)
						}
					})
				}
				out := watch(false)
				if strings.Contains(out, "PID") || (record.ActorPID != 0 && strings.Contains(out, fmt.Sprint(record.ActorPID))) {
					t.Errorf("watch exposed a process ID:\n%s", out)
				}
				who := record.Actor
				if who == "" {
					who = record.User
				}
				switch {
				case state == StateTodo:
					if strings.Contains(out, who) || strings.Contains(out, "session") {
						t.Errorf("TODO claim appeared in watch:\n%s", out)
					}
				case process == "stopped":
					if want := "  ✕ Queued work\n    " + who + " · claimed by a session that has ended\n    to free it, run: coop tasks release " + id + "\n"; !strings.Contains(out, want) {
						t.Errorf("watch lost the ended claim; want\n%s\ngot:\n%s", want, out)
					}
				default:
					if want := "  ◰ Queued work\n    " + who + " · working"; !strings.Contains(out, want) {
						t.Errorf("watch lost the live claim; want\n%s\ngot:\n%s", want, out)
					}
				}
				var snapshot ProjectSnapshot
				if err := json.Unmarshal([]byte(watch(true)), &snapshot); err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Tasks) != 1 || snapshot.Tasks[0].Owner != TaskOwnerLabel(record) {
					t.Errorf("watch changed diagnostic ownership: %+v", snapshot.Tasks)
				}
				if got, owned := taskOwned(t, root, id); !owned || !reflect.DeepEqual(got, before) {
					t.Errorf("watch changed stored ownership: %+v, owned=%t", got, owned)
				}
			})
		}
	}
}

func TestTaskWatchCompactQueueLabels(t *testing.T) {
	for _, tt := range []struct {
		queue, suffix string
	}{
		{"", ""},
		{".agent/tasks", ""},
		{"portal/.agent/tasks", " · portal"},
		{"packs/.agent/tasks", " · packs"},
		{"apps/api/.agent/tasks", " · apps/api"},
		{"custom/tasks", " · custom/tasks"},
		{"portal/.agent/tasks-extra", " · portal/.agent/tasks-extra"},
		{"portal/.agent/tasks/archive", " · portal/.agent/tasks/archive"},
		{"/shared/project/.agent/tasks", " · /shared/project"},
	} {
		t.Run(tt.queue, func(t *testing.T) {
			for _, state := range []string{StateTodo, StateBlocked, StateInProgress} {
				m := mergedTask{
					Item:  Item{Title: "Apply schema", State: state, Subtasks: []bool{true, false}},
					queue: tt.queue,
					lease: TaskLeaseObservation{State: leaseBusy, Provider: "claude"},
				}
				line := boardSections(ui.Palette{}, []mergedTask{m}, 0, 100)[0][1]
				if want := " Apply schema" + tt.suffix; !strings.HasSuffix(line, want) {
					t.Errorf("%s row = %q, want title and suffix %q", state, line, want)
				}
			}
		})
	}
}

// A fork with a task on the board shows only in that task's detail line, not again in a forks
// section; a fork with no task row (landing, cleaning up, between tasks) keeps its own line.
func TestTaskWatchListsAForkOnceOnItsTaskRow(t *testing.T) {
	snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{
		{Name: "sweep-a", Assignments: 1},
		{Name: "lander", PendingLand: true},
	}}
	merged := []mergedTask{{Item: Item{ID: "t", Title: "Wire auth", State: StateInProgress}, fork: "sweep-a", phase: ForkAssignmentWorking, live: true}}
	frame := strings.Join(tasksWatchFrameWithSnapshot(nil, merged, snapshot, 0, 120), "\n")
	if strings.Contains(frame, "sweep-a · 1 assigned task") || strings.Count(frame, "sweep-a") != 1 {
		t.Errorf("a fork with a task row was listed again:\n%s", frame)
	}
	if !strings.Contains(frame, "\nforks\n  lander · landing") {
		t.Errorf("a fork without a task row lost its line:\n%s", frame)
	}
}

// A fork-held task is work only while its fork's loop or session runs; the header's working and
// stopped counts are the Now and Stopped sections. On 2026-11-05 three forks that died at start read
// "(paused)" like busy work, under "4 in progress" when one task was being worked.
func TestTaskWatchSaysWhatAForkIsDoingWithItsTask(t *testing.T) {
	task := func(phase ForkAssignmentPhase, live bool) mergedTask {
		return mergedTask{Item: Item{ID: "t", Title: "Wire auth", State: StateInProgress}, fork: "sweep-a", phase: phase, live: live}
	}
	for _, tc := range []struct {
		name             string
		task             mergedTask
		working, stopped int
	}{
		{"working loop", task(ForkAssignmentWorking, true), 1, 0},
		{"loop died while working", task(ForkAssignmentWorking, false), 0, 1},
		{"paused, loop gone", task(ForkAssignmentPaused, false), 0, 1},
		{"waiting for merge", task(ForkAssignmentReady, false), 0, 0},
		{"finished, loop gone", task(ForkAssignmentReviewing, false), 0, 0},
		{"final review running", task(ForkAssignmentReviewing, true), 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b := countBoard([]mergedTask{tc.task}); b.Working != tc.working || b.Stopped != tc.stopped || b.Doing != 1 {
				t.Errorf("counts = %+v, want %d working and %d stopped of 1 in progress", b, tc.working, tc.stopped)
			}
		})
	}
}

// A blocked task is one line under Needs you with how long it has waited; its question stays in
// coop tasks decisions, which the header names. A task moved into done today is today's work.
func TestTaskWatchShowsDecisionsAndTodaysWork(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, TasksRoot)
	for id, decision := range map[string]string{
		"open":      "# Decision: Which database should uploads use?\n\n**Resolution:**\n",
		"answered":  "# Decision: Which region?\n\n**Resolution:** us-east\n",
		"unwritten": "",
	} {
		dir := filepath.Join(root, StateBlocked, id)
		writeTaskFile(t, filepath.Join(dir, "task.md"), "# Task "+id+"\n")
		if decision != "" {
			writeTaskFile(t, filepath.Join(dir, "decision.md"), decision)
		}
	}
	// A real completion moves an old folder into done without writing inside it: only the move says
	// when it finished.
	moved := filepath.Join(root, StateInProgress, "today")
	writeTaskFile(t, filepath.Join(moved, "task.md"), "# Shipped today\n")
	yesterday := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(moved, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, StateDone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, filepath.Join(root, StateDone, "today")); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, []string{root}) })
	for _, want := range []string{
		"0 todo · 0 working · 0 stopped · 3 blocked · 1 done today\n",
		"\nNeeds you · answer them: coop tasks decisions -i\n",
		"  ⚑ Task open · <1m\n", "  ⚑ Task answered · <1m\n", "  ⚑ Task unwritten · <1m\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("watch is missing %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"Which database", "Answered", "Decision not written", "Answer decisions"} {
		if strings.Contains(out, gone) {
			t.Errorf("watch repeats %q under a blocked task:\n%s", gone, out)
		}
	}
}

// A person's claim with no process to check (coop tasks claim from a terminal) is someone working
// the task; a claimant whose process stopped left it stopped.
func TestTaskWatchCountsAPersonsClaimAsWork(t *testing.T) {
	claimed := mergedTask{Item: Item{ID: "t", Title: "Wire auth", State: StateInProgress}, owner: "claimed by alice", live: true}
	stopped := claimed
	stopped.live = false
	if b := countBoard([]mergedTask{claimed, stopped}); b.Working != 1 || b.Stopped != 1 || b.Doing != 2 {
		t.Errorf("counts = %+v, want the live claim working and the ended one stopped", b)
	}
}

// Why a fork's loop ended comes from the failure it recorded for this generation, else the last
// error line in the tail of its log (color and all), else nothing — the board then says the loop
// ended unexpectedly.
func TestForkStopReasonReadsTheRecordThenTheLog(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "sweep-b")
	if got := forkStopReason(repo, identity); got != "" {
		t.Errorf("no record and no log: reason = %q, want none", got)
	}
	if err := forkspace.EnsureStateDir(repo); err != nil {
		t.Fatal(err)
	}
	log := forkspace.LogPath(repo, "sweep-b")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(log, "Using .agent/loop.yaml\n\x1b[31m✗ An older failure\x1b[0m\nmore output\n\x1b[31m✗ Codex cannot start because this project asks for network access that has not been approved\x1b[0m\n\n  Review it: coop approve\n")
	network := "Codex cannot start because this project asks for network access that has not been approved"
	if got := forkStopReason(repo, identity); got != network {
		t.Errorf("log only: reason = %q, want its last error line %q", got, network)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".coop"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(workspace, ".coop", "loop-error"), "an-older-generation\nthe old loop's failure\n")
	if got := forkStopReason(repo, identity); got != network {
		t.Errorf("another generation's record: reason = %q, want the log's %q", got, network)
	}
	write(filepath.Join(workspace, ".coop", "loop-error"), string(identity.Generation)+"\nthe recorded failure\n")
	if got := forkStopReason(repo, identity); got != "the recorded failure" {
		t.Errorf("recorded failure: reason = %q, want it before the log", got)
	}
	// Only the log's tail is read: an error line further back than that is not this stop's reason.
	write(log, "✗ An ancient failure\n"+strings.Repeat("progress\n", forkLogTail/9+1))
	if got := lastLoggedError(log); got != "" {
		t.Errorf("an error before the tail was read: %q", got)
	}
}

// A held lease reports how long its heartbeat has been quiet, which a stalled row shows.
func TestLeaseObservationReportsHeartbeatQuiet(t *testing.T) {
	now := time.Now()
	meta := taskLeaseMetadata{Provider: "codex", AcquiredAt: now.Add(-time.Hour), HeartbeatAt: now.Add(-6 * time.Minute)}
	if got := leaseObservationFromMetadata(meta, true, now); got.State != leaseStalled || got.Quiet != 6*time.Minute || got.Held != time.Hour {
		t.Errorf("observation = %+v, want stalled, quiet 6m, held 1h", got)
	}
}

// The board shows TASKS. A box or session is execution data the snapshot still carries — activity
// accounting, auto-exit and the JSON projection all read it — but it is not a second
// inventory painted under the queue. The fork/problem cues that DO belong to a task stay.
func TestTaskWatchDropsTheStandaloneBoxInventory(t *testing.T) {
	snapshot := ProjectSnapshot{
		Executions: []forkspace.ExecutionObservation{{
			Record: forkspace.ExecutionRecord{
				Kind: forkspace.ExecutionRemoteSession, Role: forkspace.ExecutionRoleActiveTurn,
				Workspace: "/project-forks/api", SourceID: "session-run",
				Task: &forkspace.ExecutionTaskRef{ID: "wire-auth"},
			},
			Running: true, Active: true,
		}},
		Forks:    []ProjectForkSnapshot{{Name: "api", DetachedRunning: true}},
		Problems: []string{"queue web/.agent/tasks is unreadable"},
	}
	joined := strings.Join(tasksWatchFrameWithSnapshot(nil, nil, snapshot, 0, 120), "\n")
	for _, gone := range []string{"sandboxes", "remote-session · running", "role: active-turn", "source: session-run"} {
		if strings.Contains(joined, gone) {
			t.Errorf("the board still paints the box inventory (%q):\n%s", gone, joined)
		}
	}
	for _, want := range []string{"forks", "api · detached loop running", "problems", "queue web/.agent/tasks is unreadable"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the board lost %q:\n%s", want, joined)
		}
	}
	// The execution data itself is untouched: the JSON snapshot and the activity accounting read it.
	if !snapshotHasVisibleActivity(snapshot) {
		t.Error("a running box must still count as activity for the auto-exit guard")
	}
}

func TestTasksWatchExitsNonzeroOnAnUnreadableQueue(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(repo, TasksRoot)
	if err := ScaffoldStateDirs(root); err != nil {
		t.Fatal(err)
	}
	captured, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = captured
	t.Cleanup(func() { os.Stdout = stdout })

	// A healthy, empty queue is drained: both single-shot views exit 0.
	if code, err := TasksWatch(Host{}, repo, []string{root}, true); code != 0 || err != nil {
		t.Fatalf("healthy --json watch = (%d, %v); want (0, nil)", code, err)
	}
	if code, err := TasksWatch(Host{}, repo, []string{root}); code != 0 || err != nil {
		t.Fatalf("healthy piped watch = (%d, %v); want (0, nil)", code, err)
	}

	// An entry the task reader refuses makes the queue unreadable: zero counts are unknown work,
	// so the watch fails instead of reporting a drain — while the snapshot still names the failure.
	if err := os.Symlink(t.TempDir(), filepath.Join(root, StateTodo, "redirected")); err != nil {
		t.Fatal(err)
	}
	if err := captured.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := captured.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	code, err := TasksWatch(Host{}, repo, []string{root}, true)
	if code != 1 || err == nil || !strings.Contains(err.Error(), "queue "+root+" is unreadable") {
		t.Fatalf("unreadable --json watch = (%d, %v); want exit 1 naming the unreadable queue", code, err)
	}
	output, err := os.ReadFile(captured.Name())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot ProjectSnapshot
	if err := json.Unmarshal(output, &snapshot); err != nil {
		t.Fatalf("--json output is not one snapshot: %v\n%s", err, output)
	}
	if len(snapshot.Queues) != 1 || !strings.Contains(snapshot.Queues[0].Error, "is not a real directory") ||
		snapshot.Queues[0].Counts != (TaskCounts{}) {
		t.Fatalf("snapshot queues = %+v; want the read failure recorded beside zero counts", snapshot.Queues)
	}
	if code, err := TasksWatch(Host{}, repo, []string{root}); code != 1 || err == nil {
		t.Fatalf("unreadable piped watch = (%d, %v); want exit 1", code, err)
	}
}

// A live board re-reads only when something it lists changed on disk, or after watchRefresh: one
// full read opens every task's records, and re-reading every 400 ms tick kept a third of a CPU busy
// on a 650-task queue that wasn't changing.
func TestBoardCacheRereadsOnlyWhenTheBoardCouldChange(t *testing.T) {
	t.Setenv(TestLeaseAuthorityRootEnv, t.TempDir())
	root := t.TempDir()
	todo := taskForLease(t, root, StateTodo, "listed")
	done := taskForLease(t, root, StateDone, "finished")
	if err := os.MkdirAll(filepath.Join(root, StateBlocked), 0o755); err != nil {
		t.Fatal(err)
	}
	reads := 0
	cache := &boardCache{roots: []string{root}, read: func() boardRead {
		reads++
		var merged []mergedTask
		for _, item := range mustReadTaskTree(t, root) {
			merged = append(merged, mergedTask{Item: item})
		}
		return boardRead{merged: merged}
	}}
	now := time.Now()
	// the first read finds the tasks the stamps then cover; the next settles on them
	cache.get(now)
	cache.get(now)
	settled := reads
	tick := func(what string, want int) {
		t.Helper()
		now = now.Add(time.Second)
		before := reads
		cache.get(now)
		if reads-before != want {
			t.Errorf("%s: %d reads, want %d", what, reads-before, want)
		}
	}
	tick("nothing changed", 0)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(todo.Dir, "task.md"), later, later); err != nil {
		t.Fatal(err)
	}
	tick("a listed task's file changed", 1)
	tick("nothing changed since", 0)
	if err := os.Chtimes(filepath.Join(done.Dir, "task.md"), later, later); err != nil {
		t.Fatal(err)
	}
	tick("a done task's file changed", 0)
	if err := os.Rename(todo.Dir, filepath.Join(root, StateBlocked, "listed")); err != nil {
		t.Fatal(err)
	}
	tick("a task moved to another state", 1)
	cache.get(now)
	now = now.Add(watchRefresh)
	before := reads
	cache.get(now)
	if reads != before+1 {
		t.Errorf("no read after %s of nothing changing; a fork or run could have", watchRefresh)
	}
	if settled > 3 {
		t.Errorf("a new board took %d reads to settle", settled)
	}
}

func TestShortElapsed(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second:              "<1m",
		14 * time.Minute:              "14m",
		2*time.Hour + 5*time.Minute:   "2h 5m",
		26*time.Hour + 59*time.Minute: "26h 59m",
	} {
		if got := shortElapsed(d); got != want {
			t.Errorf("shortElapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

// Only a running generation of a fork is live: a task held by an older generation of a recreated
// fork must not borrow the new generation's loop.
func TestLiveForkGenerations(t *testing.T) {
	current := forkspace.Identity{Name: "sweep-a", Generation: "g2"}
	live := liveForkGenerations([]ProjectForkSnapshot{
		{Name: "sweep-a", Identity: &current, DetachedRunning: true},
		{Name: "idle", Identity: &forkspace.Identity{Name: "idle", Generation: "g1"}},
	})
	if !live[current] || live[forkspace.Identity{Name: "sweep-a", Generation: "g1"}] || live[forkspace.Identity{Name: "idle", Generation: "g1"}] {
		t.Errorf("live generations = %v, want only sweep-a g2", live)
	}
}

// Moving a folder into done leaves its modification time alone and updates only its change time, so
// "done today" must read the change time: an old task finished today otherwise counted as archive.
func TestChangedSinceSeesAMoveIntoDone(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "doing"), filepath.Join(dir, "done")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().Add(-30 * time.Hour)
	if err := os.Chtimes(src, yesterday, yesterday); err != nil {
		t.Fatal(err)
	}
	beforeMove := time.Now().Add(-time.Second)
	if err := os.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dst); err != nil || !info.ModTime().Before(beforeMove) {
		t.Fatalf("the move changed the folder's modification time (%v); this test no longer proves the change-time path", err)
	}
	if !changedSince(dst, beforeMove) {
		t.Error("a folder moved in after the threshold did not count")
	}
	if changedSince(dst, time.Now().Add(time.Hour)) {
		t.Error("a folder counted as changed after a threshold in the future")
	}
}
