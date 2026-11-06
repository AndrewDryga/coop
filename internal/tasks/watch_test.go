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
	"github.com/AndrewDryga/coop/internal/processidentity"
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
	for _, line := range []string{tasksProgressLine(ui.Colored(), b, 120), sourceLine(ui.Colored(), "api", 3, c)} {
		if !strings.Contains(line, "█") {
			t.Errorf("task-watch overall and source bars should keep a visible active cell: %q", line)
		}
	}
	// Without color every segment is the same block, so the counts carry the states alone.
	for _, line := range []string{tasksProgressLine(ui.Palette{}, b, 120), sourceLine(ui.Palette{}, "api", 3, c)} {
		if strings.ContainsAny(line, "█░[") {
			t.Errorf("a colorless line draws a bar it cannot tell apart: %q", line)
		}
	}
}

// renderSections is each section the board shows for merged, as a pipe shows it.
func renderSections(p ui.Palette, merged []mergedTask, spin, width int) [][]string {
	return renderBoardSections(p, merged, nil, spin, width)
}

func renderBoardSections(p ui.Palette, merged []mergedTask, merges []forkMerge, spin, width int) [][]string {
	var out [][]string
	sections := boardSections(p, merged, merges, spin, width)
	for i, shown := range sectionShown(sections, 0) {
		out = append(out, sections[i].render(shown))
	}
	return out
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

// The board the owner approved on 2026-11-05, line for line, on a project with one queue: Now,
// Stopped, Needs you and Up next, each with its count; a detail line under every task that is not a
// plain list entry and a fix line under every stopped one; the full target of a worker that recorded
// one; and no row names a queue.
func TestTaskWatchBoardMatchesTheApprovedLayout(t *testing.T) {
	t.Setenv("COOP_SPINNER", "1")
	network := "Codex cannot start because this project asks for network access that has not been approved"
	merged := []mergedTask{
		{Item: inProgress("a", "Hand a task to a human when the worker re-closes the same unmet finding", 1, 3),
			lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex", Target: "codex:gpt-6-astra/xhigh@personal", Held: 6*time.Minute + 30*time.Second}},
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
		"Now (2)",
		"  ◳ Hand a task to a human when the worker re-closes the same unmet finding",
		"    1/3 · codex:gpt-6-astra/xhigh@personal · working for 6m",
		"  ◳ Accept a JSON-string options list in tasks_block",
		"    1/3 · codex in fork sweep-a · final review",
		"",
		"Stopped (3)",
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
		"Needs you (3) · answer them: coop tasks decisions -i",
		"  ⚑ Run the provider guides on Linux with real agents in Cloud Shell",
		"    decision pending for 5h",
		"  ⚑ Move co-op to coop.emisar.dev",
		"    decision pending for 16h",
		"  ⚑ Prepare Protectorate repository and website migration",
		"    decision pending for 19h",
		"",
		"Up next (32)",
		"  ○ Keep template placeholders out of a completed task's state.md and log.md",
		"  ○ Keep the loop running when a review subject left the archive",
		"  ○ Later work",
		"  + 29 more",
	}
	got := tasksWatchFrame(boardRead{sources: []watchSource{{label: ".agent/tasks"}}, merged: merged}, 1, 120, 0)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("board =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The second board the owner approved on 2026-11-05 (task 2026-11-05-show-fork-merges-once-name-
// every-row-s-queue-siz, spec.md), from real emisar data: several queues, so every row names its
// queue first, the project's own queue by the project's folder name; a fork's finished tasks are one
// merge row, under Now while the merge runs; a command too long for its line gets a line of its own.
func TestTaskWatchMultiQueueBoardMatchesTheApprovedLayout(t *testing.T) {
	t.Setenv("COOP_SPINNER", "1")
	release := "2026-11-05-fall-back-to-the-live-debian-archives-wh"
	merged := []mergedTask{
		{Item: inProgress("a", "Drain only this node's runner sockets on shutdown", 4, 4), queue: "portal",
			fork: "portal-a", phase: ForkAssignmentReviewing, live: true, agent: "claude", since: ago(12 * time.Minute)},
		{Item: inProgress("b", "Accept other Emisar server tables in Codex and Grok configs", 0, 0), queue: "mcp",
			fork: "mcp", phase: ForkAssignmentReviewing, live: true, agent: "claude", since: ago(8 * time.Minute)},
		{Item: inProgress(release, "Fall back to the live Debian archives when the snapshot fails", 0, 1), queue: "infra",
			owner: "claimed by claude", agent: "claude", since: ago(40 * time.Minute)},
		{Item: Item{ID: "d", Title: "Decide whether install-mcp.ps1 keeps -ConnectAll", State: StateBlocked}, queue: "mcp", since: ago(6 * time.Hour)},
		{Item: Item{ID: "e", Title: "Submit Emisar to the Claude Connectors Directory", State: StateBlocked}, queue: "emisar", since: ago(50 * time.Hour)},
		{Item: Item{ID: "f", Title: "Accept any Redis database index the server has", State: StateTodo}, queue: "packs"},
	}
	for i := range 3 {
		merged = append(merged, mergedTask{Item: inProgress(fmt.Sprintf("merged-%d", i), "Merged", 1, 1), queue: "portal/tasks-b",
			fork: "portal-b", phase: ForkAssignmentReady, folded: true})
	}
	merges := []forkMerge{{fork: "portal-b", queue: "portal/tasks-b", tasks: 3, landing: true, since: ago(2 * time.Minute), ins: 80, del: 14}}
	want := []string{
		"1 todo · 3 working · 1 stopped · 2 blocked · 0 done today",
		"",
		"Now (3)",
		"  ◳ Drain only this node's runner sockets on shutdown",
		"    portal · 4/4 · claude in fork portal-a · final review for 12m",
		"  ◳ Accept other Emisar server tables in Codex and Grok configs",
		"    mcp · claude in fork mcp · final review for 8m",
		"  ◳ Merge fork portal-b (3 tasks)",
		"    portal/tasks-b · fork portal-b merging for 2m",
		"    +80 −14",
		"",
		"Stopped (1)",
		"  ✕ Fall back to the live Debian archives when the snapshot fails",
		"    infra · 0/1 · claude · claim held by a session that has ended · idle for 40m",
		"    to free it, run:",
		"    coop tasks release " + release,
		"",
		"Needs you (2) · answer them: coop tasks decisions -i",
		"  ⚑ Decide whether install-mcp.ps1 keeps -ConnectAll",
		"    mcp · decision pending for 6h",
		"  ⚑ Submit Emisar to the Claude Connectors Directory",
		"    emisar · decision pending for 2d",
		"",
		"Up next (1)",
		"  ○ Accept any Redis database index the server has · packs",
	}
	got := tasksWatchFrame(boardRead{merged: merged, merges: merges}, 1, 90, 0)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("board =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Before its merge starts, the fork's finished work waits on a person, with the command that merges it.
	merges[0].landing, merges[0].since = false, ago(23*time.Minute)
	sections := renderBoardSections(ui.Palette{}, nil, merges, 0, 80)
	if want := []string{
		"Needs you (1)",
		"  ⚑ Merge fork portal-b (3 tasks)",
		"    portal/tasks-b · fork portal-b ready for 23m",
		"    +80 −14 · to merge, run: coop fork merge portal-b",
	}; len(sections) != 1 || !reflect.DeepEqual(sections[0], want) {
		t.Errorf("ready merge =\n%q\nwant\n%q", sections, want)
	}
	// Its tasks are that one row, never a row each, and a merge waiting on a person is not work.
	if b := countBoard(merged[len(merged)-3:], merges); b.Working != 0 || b.Stopped != 0 {
		t.Errorf("folded tasks counted as rows: %+v", b)
	}
}

// Every situation the board can derive from what coop records, as its title, detail and fix lines,
// in the section it belongs to (the spec's edge-case table).
func TestTaskWatchEdgeCaseRows(t *testing.T) {
	const title = "Wire auth"
	task := inProgress("2026-11-05-wire-auth", title, 0, 3)
	busy := TaskLeaseObservation{State: leaseBusy, Provider: "codex", Held: 6 * time.Minute}
	back := time.Now().Add(12*time.Minute + 30*time.Second)
	for _, tc := range []struct {
		name    string
		task    mergedTask
		section string
		lines   []string
	}{
		{"worker gone", mergedTask{Item: task, agent: "codex", since: ago(25 * time.Minute)}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex · worker gone 25m ago", "    to resume, run: coop loop"}},
		{"worker gone, never seen", mergedTask{Item: task}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · worker gone", "    to resume, run: coop loop"}},
		{"stopped on purpose", mergedTask{Item: task, agent: "codex", stoppedByYou: true, since: ago(25 * time.Minute)}, "Stopped (1)",
			[]string{"  ‖ " + title, "    0/3 · codex · stopped by you", "    to resume, run: coop loop"}},
		{"usage limit", mergedTask{Item: task, agent: "codex", limitUntil: back, since: ago(3 * time.Minute)}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex · usage limit, back in 12m"}},
		{"usage limit that has passed", mergedTask{Item: task, agent: "codex", limitUntil: ago(time.Minute), since: ago(25 * time.Minute)}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex · worker gone 25m ago", "    to resume, run: coop loop"}},
		{"waiting on the service launch lock", mergedTask{Item: task, lease: busy, startingServices: true}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex · waiting to start its services"}},
		{"no heartbeat while the process exists",
			mergedTask{Item: task, lease: TaskLeaseObservation{State: leaseStalled, Provider: "codex", Held: time.Hour, Quiet: 6*time.Minute + 10*time.Second}}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex · no heartbeat for 6m"}},
		{"held lease", mergedTask{Item: task, lease: TaskLeaseObservation{State: leaseBusy, Provider: "claude", Held: 2*time.Hour + 5*time.Minute}}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · claude · working for 2h 5m"}},
		{"held lease with a full target",
			mergedTask{Item: task, lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex", Target: "codex:gpt-6-astra/xhigh@personal", Held: 6 * time.Minute}}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex:gpt-6-astra/xhigh@personal · working for 6m"}},
		{"held lease with a target that is not one",
			mergedTask{Item: task, lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex", Target: "codex\x1b[2J; rm -rf", Held: 6 * time.Minute}}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex · working for 6m"}},
		{"live claim", mergedTask{Item: task, owner: "claimed by alice", live: true, agent: "alice", since: ago(6 * time.Minute)}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · alice · working for 6m"}},
		{"claim left by a closed session", mergedTask{Item: task, owner: "claimed by codex", agent: "codex", since: ago(40 * time.Minute)}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex · claim held by a session that has ended · idle for 40m", "    to free it, run: coop tasks release 2026-11-05-wire-auth"}},
		{"claim left by a closed session, never touched", mergedTask{Item: task, owner: "claimed by codex", agent: "codex"}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex · claim held by a session that has ended", "    to free it, run: coop tasks release 2026-11-05-wire-auth"}},
		{"claimant on another system", mergedTask{Item: task, owner: "claimed by codex", agent: "codex", system: "Linux"}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex · running on Linux"}},
		{"fork working", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentWorking, live: true, agent: "codex", since: ago(6 * time.Minute)}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex in fork sweep-a · working for 6m"}},
		{"fork final review", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentReviewing, live: true, agent: "codex", since: ago(12 * time.Minute)}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex in fork sweep-a · final review for 12m"}},
		{"fork loop between iterations, its lease held",
			mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentPaused, live: true, agent: "codex@personal", since: ago(time.Hour),
				lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex", Held: 14 * time.Minute}}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex@personal in fork sweep-a · working for 14m"}},
		{"fork loop on another system", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentWorking, live: true, system: "Linux"}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · in fork sweep-a · running on Linux"}},
		{"fork loop waits out a usage limit", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentWorking, live: true, agent: "codex", limitUntil: back}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex in fork sweep-a · usage limit, back in 12m"}},
		{"fork box waits on the service launch lock",
			mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentWorking, live: true, agent: "codex", lease: busy, startingServices: true}, "Now (1)",
			[]string{"  ◳ " + title, "    0/3 · codex in fork sweep-a · waiting to start its services"}},
		{"fork loop stopped on purpose", mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentWorking, agent: "codex", stoppedByYou: true}, "Stopped (1)",
			[]string{"  ‖ " + title, "    0/3 · codex in fork sweep-b · stopped by you", "    to resume, run: coop fork sweep-b --loop -d"}},
		{"fork loop stopped on purpose, then failed",
			mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentWorking, agent: "codex", stoppedByYou: true, stopReason: "Codex cannot start because the box image is missing"}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · the box image is missing", "    to resume, run: coop fork sweep-b --loop -d"}},
		{"fork loop refused, reason recorded",
			mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentPaused, agent: "codex", stopReason: "this project asks for unrestricted internet access, which has not been approved"}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · network access not approved", "    to fix, run: coop approve, then coop fork sweep-b --loop -d"}},
		{"fork loop lost its sign-in",
			mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentWorking, agent: "codex", stopReason: "the provider no longer accepts this account's saved sign-in; run coop login"}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · sign-in no longer accepted", "    to fix, run: coop login, then coop fork sweep-b --loop -d"}},
		{"fork loop ended, reason only in its log",
			mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentWorking, agent: "codex", stopReason: "Codex cannot start because the box image is missing"}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · the box image is missing", "    to resume, run: coop fork sweep-b --loop -d"}},
		{"fork loop ended, no reason anywhere", mergedTask{Item: task, fork: "sweep-b", phase: ForkAssignmentPreparing, agent: "codex"}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-b · loop ended unexpectedly", "    to resume, run: coop fork sweep-b --loop -d"}},
		{"fork finished, nothing published, loop gone", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentReady, agent: "codex", since: ago(2 * time.Hour)}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-a · loop ended unexpectedly", "    to resume, run: coop fork sweep-a --loop -d"}},
		{"fork final review with its loop gone", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentReviewing, agent: "codex", stopReason: "task x projection changed after review"}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-a · loop failed", "    task x projection changed after review", "    to resume, run: coop fork sweep-a --loop -d"}},
		{"blocked a day ago", mergedTask{Item: Item{ID: "b", Title: title, State: StateBlocked}, since: ago(50 * time.Hour)}, "Needs you (1) · answer them: coop tasks decisions -i",
			[]string{"  ⚑ " + title, "    decision pending for 2d"}},
		{"answered, not resumed", mergedTask{Item: Item{ID: "b", Title: title, State: StateBlocked}, answered: true, since: ago(3 * time.Hour)}, "Needs you (1) · answer them: coop tasks decisions -i",
			[]string{"  ⚑ " + title, "    answered, not yet resumed for 3h"}},
		{"blocked in a fork", mergedTask{Item: task, fork: "sweep-a", phase: ForkAssignmentBlocked, agent: "codex", since: ago(5 * time.Hour)}, "Needs you (1) · answer them: coop tasks decisions -i",
			[]string{"  ⚑ " + title, "    decision pending for 5h"}},
		// The fork's loop finished this task, then stopped for a decision on its other task: nothing
		// failed, and answering that decision is what moves this one on.
		{"done in a fork that stopped for its other task's decision",
			mergedTask{Item: task, fork: "sweep-c", phase: ForkAssignmentReviewing, agent: "codex", forkBlocked: true, since: ago(time.Hour)}, "Needs you (1) · answer them: coop tasks decisions -i",
			[]string{"  ⚑ " + title, "    done in fork sweep-c, waiting for the fork's other task"}},
		{"working in a fork whose other task is blocked, its loop gone",
			mergedTask{Item: task, fork: "sweep-c", phase: ForkAssignmentWorking, agent: "codex", forkBlocked: true}, "Stopped (1)",
			[]string{"  ✕ " + title, "    0/3 · codex in fork sweep-c · loop ended unexpectedly", "    to resume, run: coop fork sweep-c --loop -d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COOP_SPINNER", "1")
			sections := renderSections(ui.Palette{}, []mergedTask{tc.task}, 1, 120)
			want := append([]string{tc.section}, tc.lines...)
			if len(sections) != 1 || !reflect.DeepEqual(sections[0], want) {
				t.Errorf("board =\n%q\nwant one section\n%q", sections, want)
			}
		})
	}
}

// Detail lines are dim gray; a task waiting but alive is yellow; a stopped one has a red marker and
// state and its commands bold, unless a person stopped it, which reads dim; a list header is gray
// after its dot, and a wait past a day is yellow.
func TestTaskWatchRowColors(t *testing.T) {
	p := ui.Colored()
	board := strings.Join(slices.Concat(renderSections(p, []mergedTask{
		{Item: inProgress("a", "Busy", 0, 1), lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex", Held: time.Minute}},
		{Item: inProgress("b", "Quiet", 0, 1), lease: TaskLeaseObservation{State: leaseStalled, Provider: "codex", Quiet: 6 * time.Minute}},
		{Item: inProgress("c", "Gone", 0, 1)},
		{Item: Item{ID: "d", Title: "Fresh question", State: StateBlocked}, since: ago(5 * time.Hour)},
		{Item: Item{ID: "e", Title: "Old question", State: StateBlocked}, since: ago(30 * time.Hour)},
		{Item: inProgress("f", "Paused", 0, 1), stoppedByYou: true},
		{Item: inProgress("g", "Limited", 0, 1), limitUntil: time.Now().Add(12*time.Minute + 30*time.Second)},
		{Item: inProgress("h", "Starting", 0, 1), startingServices: true},
	}, 0, 120)...), "\n")
	for _, want := range []string{
		p.Faint("0/1 · codex · ") + p.Faint("working for 1m"),
		p.Yellow("no heartbeat for 6m"),
		p.Red(stoppedMarker) + " Gone", p.Red("worker gone"),
		p.Dim(pausedMarker) + " Paused", p.Faint("stopped by you"), "to resume them, run: " + p.Bold("coop loop"),
		p.Yellow("usage limit, back in 12m"), p.Yellow("waiting to start its services"),
		"Needs you (2)" + p.Gray(" · answer them: coop tasks decisions -i"),
		p.Faint(" for ") + p.Faint("5h"), p.Faint(" for ") + p.Yellow("30h"),
	} {
		if !strings.Contains(board, want) {
			t.Errorf("board is missing %q:\n%q", want, board)
		}
	}
}

// A title is a terminal link to what a person opens next: a blocked task's decision, another task's
// folder, a merging fork's folder. A pipe gets the plain title.
func TestTaskWatchLinksTitles(t *testing.T) {
	dir := t.TempDir()
	merged := []mergedTask{
		{Item: Item{ID: "ask", Title: "Pick a region", State: StateBlocked, Dir: filepath.Join(dir, "ask"), HasDecision: true}},
		{Item: Item{ID: "next", Title: "Wire auth", State: StateTodo, Dir: filepath.Join(dir, "next dir")}},
	}
	merges := []forkMerge{{fork: "portal-b", dir: filepath.Join(dir, "portal-b"), tasks: 1}}
	p := ui.Colored()
	board := strings.Join(slices.Concat(renderBoardSections(p, merged, merges, 0, 120)...), "\n")
	for _, want := range []string{
		p.Link("file://"+filepath.ToSlash(filepath.Join(dir, "ask", "decision.md")), "Pick a region"),
		p.Link("file://"+filepath.ToSlash(filepath.Join(dir, "next%20dir")), "Wire auth"),
		p.Link("file://"+filepath.ToSlash(filepath.Join(dir, "portal-b")), "Merge fork portal-b (1 task)"),
	} {
		if !strings.Contains(board, want) {
			t.Errorf("board is missing the link %q:\n%q", want, board)
		}
	}
	plain := strings.Join(slices.Concat(renderBoardSections(ui.Palette{}, merged, merges, 0, 120)...), "\n")
	if strings.Contains(plain, "\x1b") || !strings.Contains(plain, "  ⚑ Pick a region\n") {
		t.Errorf("a pipe's board carries escapes or lost a title:\n%q", plain)
	}
}

// Active work always shows in full; a pipe shows three of Needs you and of Up next, then how many more.
func TestTaskWatchCapsTheListsNeverTheWork(t *testing.T) {
	var merged []mergedTask
	for i := range 6 {
		merged = append(merged,
			mergedTask{Item: inProgress(fmt.Sprintf("now-%d", i), fmt.Sprintf("Now %d", i), 0, 1), lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex"}},
			mergedTask{Item: inProgress(fmt.Sprintf("stop-%d", i), fmt.Sprintf("Stopped %d", i), 0, 1)},
			mergedTask{Item: Item{ID: fmt.Sprintf("ask-%d", i), Title: fmt.Sprintf("Ask %d", i), State: StateBlocked}},
			mergedTask{Item: Item{ID: fmt.Sprintf("todo-%d", i), Title: fmt.Sprintf("Todo %d", i), State: StateTodo}})
	}
	board := strings.Join(tasksWatchFrame(boardRead{merged: merged}, 0, 120, 0), "\n")
	for i := range 6 {
		for _, title := range []string{"Now %d\n", "Stopped %d\n"} {
			if want := fmt.Sprintf(title, i); !strings.Contains(board, want) {
				t.Errorf("active task %q was capped:\n%s", want, board)
			}
		}
	}
	for _, want := range []string{"Ask 2\n    decision pending\n  + 3 more\n", "Todo 2\n  + 3 more"} {
		if !strings.Contains(board, want) {
			t.Errorf("board is missing %q:\n%s", want, board)
		}
	}
	for _, capped := range []string{"Ask 3", "Todo 3"} {
		if strings.Contains(board, capped) {
			t.Errorf("%q is past its section's cap:\n%s", capped, board)
		}
	}
}

// A live board fills the terminal and never runs past its bottom: Needs you and Up next take the
// rows the work leaves, Needs you first, after three of Needs you and two of Up next; a terminal
// too short for those takes Up next away first.
func TestTaskWatchFillsTheTerminalHeight(t *testing.T) {
	var merged []mergedTask
	merged = append(merged, mergedTask{Item: inProgress("now", "Now", 0, 1), lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex"}})
	for i := range 10 {
		merged = append(merged,
			mergedTask{Item: Item{ID: fmt.Sprintf("ask-%d", i), Title: fmt.Sprintf("Ask %d", i), State: StateBlocked}},
			mergedTask{Item: Item{ID: fmt.Sprintf("todo-%d", i), Title: fmt.Sprintf("Todo %d", i), State: StateTodo}})
	}
	shown := func(frame []string, prefix string) int {
		n := 0
		for _, line := range frame {
			if strings.HasPrefix(line, prefix) {
				n++
			}
		}
		return n
	}
	for _, tc := range []struct {
		height, asks, todos int
	}{
		{0, 3, 3},   // a pipe
		{10, 1, 0},  // too short even for the minimums
		{19, 3, 2},  // the minimums
		{26, 6, 3},  // Needs you takes the room first
		{36, 10, 6}, // then Up next
		{60, 10, 10},
	} {
		frame := tasksWatchFrame(boardRead{merged: merged}, 0, 120, tc.height)
		if asks, todos := shown(frame, "  ⚑ Ask"), shown(frame, "  ○ Todo"); asks != tc.asks || todos != tc.todos {
			t.Errorf("height %d shows %d asks and %d todos, want %d and %d:\n%s", tc.height, asks, todos, tc.asks, tc.todos, strings.Join(frame, "\n"))
		}
		if tc.height > 0 && len(frame) > tc.height {
			t.Errorf("height %d: frame has %d lines:\n%s", tc.height, len(frame), strings.Join(frame, "\n"))
		}
	}
}

// A narrow terminal gets a shorter header, never a cut one: "today" goes first, then the labels
// shorten, then the bar.
func TestTaskWatchHeaderShortensInsteadOfBeingCut(t *testing.T) {
	b := boardCounts{TaskCounts: TaskCounts{Todo: 34, Blocked: 11}, Working: 4, Stopped: 8, DoneToday: 58}
	for _, tc := range []struct {
		width int
		want  string
	}{
		{120, "34 todo · 4 working · 8 stopped · 11 blocked · 58 done today"},
		{60, "34 todo · 4 working · 8 stopped · 11 blocked · 58 done"},
		{50, "34 todo · 4 work · 8 stop · 11 block · 58 done"},
	} {
		if got := tasksProgressLine(ui.Palette{}, b, tc.width); got != tc.want {
			t.Errorf("width %d header = %q, want %q", tc.width, got, tc.want)
		}
	}
	p := ui.Colored()
	strip := func(s string) string { return ansiSequence.ReplaceAllString(s, "") }
	if got := strip(tasksProgressLine(p, b, 120)); !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "58 done today") {
		t.Errorf("wide colored header = %q, want the bar and every word", got)
	}
	if got := strip(tasksProgressLine(p, b, 60)); got != "34 todo · 4 work · 8 stop · 11 block · 58 done" {
		t.Errorf("narrow colored header = %q, want the short labels without the bar", got)
	}
}

// The single placement rule: a fork's task names its fork only as "in fork <name>" in a detail line
// (and in the command that restarts it); a fork's merge row names it in its title, its detail line
// and its merge command. Nothing is right-aligned. Section headers carry their count and no reason
// text, every section's first row sits right under its header, and only Stopped rows are set apart
// by a blank line. Nothing sends the reader to the fork's logs.
func TestTaskWatchPlacesEveryFactByOneRule(t *testing.T) {
	merged := []mergedTask{
		{Item: inProgress("a", "Working in a fork", 1, 2), fork: "alpha", phase: ForkAssignmentWorking, live: true, agent: "codex"},
		{Item: inProgress("b", "Stopped in a fork", 0, 2), fork: "beta", phase: ForkAssignmentPaused, agent: "codex"},
		{Item: inProgress("c", "Stopped here", 0, 2)},
		{Item: inProgress("d", "Ready in a fork", 2, 2), fork: "gamma", phase: ForkAssignmentReady, folded: true},
		{Item: Item{ID: "e", Title: "Blocked in a fork", State: StateBlocked}, fork: "delta", phase: ForkAssignmentBlocked},
		{Item: Item{ID: "f", Title: "Queued for a fork", State: StateTodo}, fork: "epsilon", phase: ForkAssignmentBlocked},
	}
	merges := []forkMerge{{fork: "gamma", tasks: 1, since: ago(time.Hour), ins: 3, del: 1}}
	frame := tasksWatchFrame(boardRead{merged: merged, merges: merges}, 0, 120, 0)
	headers := map[string]bool{"Now (1)": true, "Stopped (2)": true, "Needs you (2) · answer them: coop tasks decisions -i": true, "Up next (1)": true}
	seen := 0
	for i, line := range frame {
		if strings.Contains(line, "coop fork logs") || strings.Contains(line, "←") {
			t.Errorf("line %d sends the reader elsewhere or points at a fork: %q", i, line)
		}
		if strings.Contains(strings.TrimLeft(line, " "), "  ") {
			t.Errorf("line %d pads text into a column: %q", i, line)
		}
		for _, fork := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
			rest := line
			for _, named := range []string{"in fork " + fork, "coop fork " + fork + " --loop -d", "  ⚑ Merge fork " + fork + " (",
				"    fork " + fork + " ready", "coop fork merge " + fork} {
				rest = strings.ReplaceAll(rest, named, "")
			}
			if strings.Contains(rest, fork) || (strings.Contains(line, "in fork "+fork) && !strings.HasPrefix(line, "    ")) {
				t.Errorf("line %d names fork %s outside its places: %q", i, fork, line)
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
	for _, want := range []string{"    1/2 · codex in fork alpha · working", "    0/2 · codex in fork beta · loop ended unexpectedly",
		"  ⚑ Merge fork gamma (1 task)", "    +3 −1 · to merge, run: coop fork merge gamma"} {
		if !slices.Contains(frame, want) {
			t.Errorf("frame lost %q:\n%s", want, strings.Join(frame, "\n"))
		}
	}
	if strings.Contains(strings.Join(frame, "\n"), "Ready in a fork") {
		t.Errorf("a task in its fork's merge has a row of its own:\n%s", strings.Join(frame, "\n"))
	}
}

// A title is cut only by the terminal's width, at the end of its line: the facts after it go first,
// and no row is wider than the board leaves room for. A command is never cut: one that does not fit
// goes on a line of its own.
func TestTaskWatchTitleUsesAvailableTerminalWidth(t *testing.T) {
	title := "Log every cloud error envelope instead of silently swallowing the useful diagnostic details"
	merged := []mergedTask{{Item: Item{ID: "errors", Title: title, State: StateTodo}, queue: "api"}}
	rowAt := func(width int) string { return renderSections(ui.Palette{}, merged, 0, width)[0][1] }
	if wide := rowAt(132); wide != "  ○ "+title+" · api" {
		t.Errorf("wide row = %q, want the title and its queue", wide)
	}
	const narrowWidth = 52
	narrow := rowAt(narrowWidth)
	if want := "  ○ " + title[:narrowWidth-1-4-1] + "…"; narrow != want {
		t.Errorf("narrow row = %q, want the title cut at the width %q", narrow, want)
	}
	stopped := []mergedTask{{Item: inProgress("x", title, 0, 2), fork: "sweep-b", phase: ForkAssignmentWorking, stopReason: "this project asks for network access that has not been approved"}}
	lines := renderSections(ui.Palette{}, stopped, 0, narrowWidth)[0]
	for _, line := range lines {
		if got := len([]rune(line)); got > narrowWidth-1 {
			t.Errorf("row width = %d, want at most %d: %q", got, narrowWidth-1, line)
		}
	}
	if want := []string{"    to fix, run: coop approve", "    then coop fork sweep-b --loop -d"}; !reflect.DeepEqual(lines[3:], want) {
		t.Errorf("fix lines = %q, want each command whole %q", lines[3:], want)
	}
	long := []mergedTask{{Item: inProgress("2026-11-05-fall-back-to-the-live-debian-archives-when-the-s", title, 0, 1), owner: "claimed by claude"}}
	lines = renderSections(ui.Palette{}, long, 0, narrowWidth)[0]
	if want := []string{"    to free it, run:", "    coop tasks release 2026-11-05-fall-back-to-the-live-debian-archives-when-the-s"}; !reflect.DeepEqual(lines[3:], want) {
		t.Errorf("fix lines = %q, want the command whole, on its own line %q", lines[3:], want)
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
	joined := strings.Join(tasksWatchFrame(boardRead{sources: sources, merged: merged}, 0, 80, 0), "\n")
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
	frame := strings.Join(tasksWatchFrame(boardRead{snapshot: snapshot}, 0, 120, 0), "\n")
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
	frame := strings.Join(tasksWatchFrame(boardRead{snapshot: snapshot}, 0, 120, 0), "\n")
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
		{name: "candidate under review", fork: ProjectForkSnapshot{Name: "worker", CandidatePhase: forkCandidateReviewing, CandidateRound: 2}, want: "reviewing round 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{tt.fork}}
			if !snapshotHasVisibleActivity(snapshot) {
				t.Fatal("fork work was not treated as visible activity")
			}
			frame := strings.Join(tasksWatchFrame(boardRead{snapshot: snapshot}, 0, 120, 0), "\n")
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
		task.queue = "api"
		lines := renderSections(ui.Palette{}, []mergedTask{task}, 0, 80)[0]
		if want := []string{"Up next (1)", "  ○ Queued work · api"}; !reflect.DeepEqual(lines, want) {
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
					if want := "  ✕ Queued work\n    " + who + " · claim held by a session that has ended · idle for <1m\n    to free it, run: coop tasks release " + id + "\n"; !strings.Contains(out, want) {
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

// A queue is named by the folder that holds it, without the word "queue" or the .agent folder; the
// project's own queue by the project's folder name, never "root". Other labels stay as they are.
func TestTaskWatchCompactQueueLabels(t *testing.T) {
	for label, want := range map[string]string{
		"":                             "",
		".agent/tasks":                 "emisar",
		".agent/tasks-b":               "tasks-b",
		"portal/.agent/tasks":          "portal",
		"portal/.agent/tasks-b":        "portal/tasks-b",
		"apps/api/.agent/tasks":        "apps/api",
		"custom/tasks":                 "custom/tasks",
		"portal/.agent/tasks/archive":  "portal/.agent/tasks/archive",
		"/shared/project/.agent/tasks": "/shared/project",
	} {
		if got := queueName(label, "emisar"); got != want {
			t.Errorf("queueName(%q) = %q, want %q", label, got, want)
		}
	}
	// The queue leads a detail line, and follows a list entry's title.
	for state, want := range map[string][]string{
		StateTodo:       {"  ○ Apply schema · portal"},
		StateBlocked:    {"  ⚑ Apply schema", "    portal · decision pending"},
		StateInProgress: {"  ◳ Apply schema", "    portal · 1/2 · claude · working"},
	} {
		t.Setenv("COOP_SPINNER", "1")
		m := mergedTask{Item: Item{Title: "Apply schema", State: state, Subtasks: []bool{true, false}}, queue: "portal",
			lease: TaskLeaseObservation{State: leaseBusy, Provider: "claude"}}
		if got := renderSections(ui.Palette{}, []mergedTask{m}, 1, 100)[0][1:]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s row = %q, want %q", state, got, want)
		}
	}
}

// On a board of several queues every row names its queue, the project's own by the project's folder
// name; on a board of one, none does.
func TestTaskWatchNamesEveryRowsQueueOnlyWhenThereAreSeveral(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "shop")
	root, api := filepath.Join(repo, TasksRoot), filepath.Join(repo, "api", TasksRoot)
	writeTaskFile(t, filepath.Join(root, StateBlocked, "ask", "task.md"), "# Pick a region\n")
	writeTaskFile(t, filepath.Join(root, StateTodo, "next", "task.md"), "# Wire auth\n")
	writeTaskFile(t, filepath.Join(api, StateInProgress, "doing", "task.md"), "# Cache the probe\n")
	watch := func(roots ...string) string {
		return captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, roots) })
	}
	several := watch(root, api)
	for _, want := range []string{"  ⚑ Pick a region\n    shop · decision pending for <1m\n", "  ○ Wire auth · shop\n",
		"  ✕ Cache the probe\n    api · worker gone <1m ago\n", "  shop  0/2 · 1 blocked\n"} {
		if !strings.Contains(several, want) {
			t.Errorf("several queues: watch is missing %q:\n%s", want, several)
		}
	}
	if strings.Contains(several, "root") || strings.Contains(several, ".agent") {
		t.Errorf("several queues: a queue is named by its path or as root:\n%s", several)
	}
	one := watch(root)
	for _, want := range []string{"  ⚑ Pick a region\n    decision pending for <1m\n", "  ○ Wire auth\n"} {
		if !strings.Contains(one, want) {
			t.Errorf("one queue: watch is missing %q:\n%s", want, one)
		}
	}
	if strings.Contains(one, "shop") {
		t.Errorf("one queue: a row names its queue:\n%s", one)
	}
}

// A fork with a task on the board shows only in that task's detail line, not again in a forks
// section; a fork with no row (cleaning up, between tasks) keeps its own line, and a fork ready to
// merge or merging is its merge row.
func TestTaskWatchListsAForkOnceOnItsTaskRow(t *testing.T) {
	snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{
		{Name: "sweep-a", Assignments: 1},
		{Name: "cleaner", CleanupPending: true},
		{Name: "lander", PendingLand: true},
	}}
	merged := []mergedTask{{Item: Item{ID: "t", Title: "Wire auth", State: StateInProgress}, fork: "sweep-a", phase: ForkAssignmentWorking, live: true}}
	merges := []forkMerge{{fork: "lander", landing: true}}
	frame := strings.Join(tasksWatchFrame(boardRead{merged: merged, merges: merges, snapshot: snapshot}, 0, 120, 0), "\n")
	if strings.Contains(frame, "sweep-a · 1 assigned task") || strings.Count(frame, "sweep-a") != 1 {
		t.Errorf("a fork with a task row was listed again:\n%s", frame)
	}
	if !strings.Contains(frame, "\nforks\n  cleaner · cleanup-pending") {
		t.Errorf("a fork without a row lost its line:\n%s", frame)
	}
	if strings.Count(frame, "lander") != 2 || !strings.Contains(frame, "Merge fork lander\n    fork lander merging\n") {
		t.Errorf("a merging fork is not one merge row:\n%s", frame)
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
		{"in its fork's merge", mergedTask{Item: Item{ID: "t", State: StateInProgress}, fork: "sweep-a", phase: ForkAssignmentReady, folded: true}, 0, 0},
		{"in final review, loop gone", task(ForkAssignmentReviewing, false), 0, 1},
		{"final review running", task(ForkAssignmentReviewing, true), 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if b := countBoard([]mergedTask{tc.task}, nil); b.Working != tc.working || b.Stopped != tc.stopped || b.Doing != 1 {
				t.Errorf("counts = %+v, want %d working and %d stopped of 1 in progress", b, tc.working, tc.stopped)
			}
		})
	}
}

// A blocked task is under Needs you with whether it was answered and how long it has waited; its
// question stays in coop tasks decisions, which the header names. A task moved into done today is
// today's work.
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
		"\nNeeds you (3) · answer them: coop tasks decisions -i\n",
		"  ⚑ Task open\n    decision pending for <1m\n", "  ⚑ Task answered\n    answered, not yet resumed for <1m\n",
		"  ⚑ Task unwritten\n    decision pending for <1m\n",
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
	if b := countBoard([]mergedTask{claimed, stopped}, nil); b.Working != 1 || b.Stopped != 1 || b.Doing != 2 {
		t.Errorf("counts = %+v, want the live claim working and the ended one stopped", b)
	}
}

// Why a fork's loop ended comes from the failure it recorded for this generation, else the last
// error line in the tail of its log (color and all), else nothing — the board then says the loop
// ended unexpectedly. The record reads as one bounded line with its hidden characters escaped.
func TestForkStopReasonReadsTheRecordThenTheLog(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "sweep-b")
	reason := func(stoppedByYou bool) string {
		return forkStopReason(repo, "sweep-b", forkLoopFailure(repo, identity), stoppedByYou)
	}
	if got := reason(false); got != "" {
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
	if got := reason(false); got != network {
		t.Errorf("log only: reason = %q, want its last error line %q", got, network)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".coop"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(workspace, ".coop", "loop-error"), "an-older-generation\nthe old loop's failure\n")
	if got := reason(false); got != network {
		t.Errorf("another generation's record: reason = %q, want the log's %q", got, network)
	}
	// A person stopped the loop: tearing its attempt down is no failure, whatever the log says last.
	if got := reason(true); got != "" {
		t.Errorf("stopped by you: reason = %q, want none from the log", got)
	}
	write(filepath.Join(workspace, ".coop", "loop-error"), string(identity.Generation)+"\nthe recorded failure\n")
	if got := reason(false); got != "the recorded failure" {
		t.Errorf("recorded failure: reason = %q, want it before the log", got)
	}
	if got := reason(true); got != "the recorded failure" {
		t.Errorf("recorded failure of a stopped loop: reason = %q, want it", got)
	}
	write(filepath.Join(workspace, ".coop", "loop-error"), string(identity.Generation)+"\nboom\x1b[2J  then\t"+strings.Repeat("x", 400)+"\n")
	if got := forkLoopFailure(repo, identity); !strings.HasPrefix(got, `boom\u001B[2J then x`) || len([]rune(got)) != loopFailureLimit || !strings.HasSuffix(got, "…") {
		t.Errorf("an agent-written record = %q, want one escaped line of %d runes", got, loopFailureLimit)
	}
	// Only the log's tail is read: an error line further back than that is not this stop's reason.
	write(log, "✗ An ancient failure\n"+strings.Repeat("progress\n", forkLogTail/9+1))
	if got := lastLoggedError(log); got != "" {
		t.Errorf("an error before the tail was read: %q", got)
	}
	// The agent's own failed commands are indented and quoted: they say nothing about the loop.
	write(log, "\x1b[31m✗ The loop's own failure\x1b[0m\n⚙ \"sh -c 'make check-local'\"\n  \x1b[31m✗\x1b[0m \"sh -c 'cd /x' (exit 2): make: *** [Makefile:260: check-local] Error 1\n✗ 'make' (exit 2): failed\n")
	if got := lastLoggedError(log); got != "The loop's own failure" {
		t.Errorf("agent tool lines: reason = %q, want the loop's own error line", got)
	}
	// A loop whose last word is that it stopped for a decision did not fail.
	write(log, "✗ The loop's own failure\nmore output\n\x1b[1mStopped for your decision · 0/1 tasks done\x1b[0m\n\n  Answer it: coop tasks decisions -i\n")
	if got := lastLoggedError(log); got != "" {
		t.Errorf("a loop that stopped for a decision: reason = %q, want none", got)
	}
}

// A fork's published candidate is one row for the fork, from its real records: how many tasks it
// carries, when it was published and the command that merges it. Its tasks get no rows of their own,
// and the fork no line under forks.
func TestTaskWatchShowsAForksMergeOnce(t *testing.T) {
	f := newCandidateRoundFixture(t, "alpha", "beta")
	head, tree := f.headTree()
	review, _, err := BeginForkCandidateReview(f.repo, f.identity, head, tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeForkCandidateReview(f.repo, f.identity, review.Candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, published, err := PublishForkCandidate(f.repo, f.identity, head, tree); err != nil || !published {
		t.Fatalf("publish = %v, %v", published, err)
	}
	out := captureStdout(t, func() { _, _ = TasksWatch(Host{}, f.repo, []string{f.root}) })
	want := "\nNeeds you (1)\n  ⚑ Merge fork rounds (2 tasks)\n    fork rounds ready for <1m\n    to merge, run: coop fork merge rounds\n"
	if !strings.Contains(out, want) {
		t.Errorf("watch is missing the merge row %q:\n%s", want, out)
	}
	if !strings.HasPrefix(out, "0 todo · 0 working · 0 stopped · 0 blocked") {
		t.Errorf("a merge waiting on a person counts as work:\n%s", out)
	}
	for _, gone := range []string{"alpha", "beta", "forks", "ready to merge"} {
		if strings.Contains(out, gone) {
			t.Errorf("watch shows %q beside the fork's merge row:\n%s", gone, out)
		}
	}
}

// A row names its agent by the full target the worker recorded in its lease: a local loop's lease
// on the task, and once it let go, the record of the last holder, so a task left in progress still
// names who worked it.
func TestTaskWatchNamesTheFullTargetTheWorkerRecorded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project", "tasks")
	task := taskWithCompletedChecklist(t, root, StateInProgress, "wire-auth")
	owner := testLeaseOwner()
	owner.Target = "codex:gpt-6-astra/xhigh@personal"
	lease, _, err := TryTaskLease(root, task, owner)
	if err != nil || lease == nil {
		t.Fatalf("lease = %v, %v", lease, err)
	}
	if got := observeLeaseAt(root, task.ID, time.Now()); got.State == leaseUnleased || knownTarget(got.Target, got.Provider) != owner.Target {
		t.Errorf("held lease = %+v, want it held by %s", got, owner.Target)
	}
	if got, _ := goneWorker(task); got != owner.Target {
		t.Errorf("crashed worker = %q, want %q", got, owner.Target)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got := observeLeaseAt(root, task.ID, time.Now()); got.State != leaseUnleased {
		t.Errorf("released lease = %+v, want unleased", got)
	}
	if got, at := goneWorker(task); got != owner.Target || at.IsZero() {
		t.Errorf("worker that let go = %q at %v, want %q", got, at, owner.Target)
	}
	for target, want := range map[string]string{"claude@personal": "claude@personal", "codex:o3/high@a,b": "codex:o3/high@a,b", "": "codex", "x y": "codex"} {
		if got := knownTarget(target, "codex"); got != want {
			t.Errorf("knownTarget(%q) = %q, want %q", target, got, want)
		}
	}
}

// Every task of a fork names the same agent: the target the fork's loop last recorded in a lease on
// its copy of any of its tasks, else the target its loop noted, else the agent coop fork noted.
// One task held right now and one between leases never read as two different agents.
func TestTaskWatchNamesEveryTaskOfAForkTheSameWay(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	workspace, identity := testAssignmentFork(t, repo, "sweep-c")
	projection := filepath.Join(workspace, ".coop", "task-executions", "x", "tasks")
	held := taskWithCompletedChecklist(t, projection, StateInProgress, "held")
	taskWithCompletedChecklist(t, projection, StateInProgress, "between")
	owner := testLeaseOwner()
	owner.Target = "codex:gpt-6-astra/xhigh@personal"
	lease, _, err := TryTaskLease(projection, held, owner)
	if err != nil || lease == nil {
		t.Fatalf("lease = %v, %v", lease, err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	snapshot := ProjectSnapshot{}
	for _, id := range []string{"held", "between"} {
		fork := identity
		snapshot.Tasks = append(snapshot.Tasks, ProjectTaskSnapshot{Fork: &fork, Item: Item{ID: id},
			ownerRecord: &TaskOwnerRecord{Fork: &ForkTaskOwner{Fork: identity, Projection: projection}}})
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(workspace, ".coop", name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agent", "codex\n")
	if got := readForkTargets(snapshot).of(repo, identity); got != owner.Target {
		t.Errorf("fork target = %q, want the loop's lease %q for every task", got, owner.Target)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if got := readForkTargets(snapshot).of(repo, identity); got != owner.Target {
		t.Errorf("fork target between leases = %q, want the last holder %q", got, owner.Target)
	}
	otherWorkspace, otherIdentity := testAssignmentFork(t, repo, "other")
	if err := os.MkdirAll(filepath.Join(otherWorkspace, ".coop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherWorkspace, ".coop", "agent"), []byte("codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readForkTargets(snapshot).of(repo, otherIdentity); got != "codex" {
		t.Errorf("fork with only its agent note = %q, want codex", got)
	}
	if err := os.WriteFile(filepath.Join(otherWorkspace, ".coop", "target"), []byte("codex:gpt-6-astra/xhigh@personal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readForkTargets(snapshot).of(repo, otherIdentity); got != "codex:gpt-6-astra/xhigh@personal" {
		t.Errorf("fork with its loop's target note = %q, want the noted target", got)
	}
}

// A fork whose loop is not running carries the failure its loop recorded, in --json as coop fork ls
// shows it and on its task's row as the commands that move it on. Once its loop runs again, the old
// failure is gone from both.
func TestTaskWatchShowsAForksRecordedLoopFailure(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "project")
	root := filepath.Join(repo, ".agent", "tasks")
	taskWithCompletedChecklist(t, root, StateTodo, "wire-auth")
	workspace, identity := testAssignmentFork(t, repo, "sweep-a")
	assignment, err := AssignForkTask([]string{root}, ForkAssignmentRequest{
		AuthorityRepo: repo, Fork: identity, WorkspaceRoot: workspace, BaselineHead: strings.Repeat("a", 40), LeaseOwner: testLeaseOwner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := MaterializeForkProjection(assignment); err != nil {
		t.Fatal(err)
	}
	if err := assignment.Lease.Release(); err != nil {
		t.Fatal(err)
	}
	refusal := "Codex cannot start because this project asks for network access that has not been approved"
	if err := os.MkdirAll(filepath.Join(workspace, ".coop"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".coop", "loop-error"), []byte(string(identity.Generation)+"\n"+refusal+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	watch := func(jsonOutput bool) string {
		return captureStdout(t, func() { _, _ = TasksWatch(Host{}, repo, []string{root}, jsonOutput) })
	}
	loopError := func() string {
		t.Helper()
		var snapshot ProjectSnapshot
		if err := json.Unmarshal([]byte(watch(true)), &snapshot); err != nil {
			t.Fatal(err)
		}
		for _, fork := range snapshot.Forks {
			if fork.Name == "sweep-a" {
				return fork.LoopError
			}
		}
		return ""
	}
	if got := loopError(); got != refusal {
		t.Errorf("--json loop_error = %q, want %q", got, refusal)
	}
	if out := watch(false); !strings.Contains(out, " in fork sweep-a · network access not approved\n    to fix, run: coop approve, then coop fork sweep-a --loop -d\n") {
		t.Errorf("the stopped fork's row:\n%s", out)
	}
	worker := forkspace.WorkerState{Pid: os.Getpid(), Token: processidentity.StartToken(os.Getpid()), Generation: identity.Generation}
	if err := forkspace.WriteWorkerState(repo, "sweep-a", worker); err != nil {
		t.Fatal(err)
	}
	if got := loopError(); got != "" {
		t.Errorf("a running fork's loop_error = %q, want none", got)
	}
	if out := watch(false); strings.Contains(out, "network access") || !strings.Contains(out, "Now (1)") {
		t.Errorf("the running fork's row:\n%s", out)
	}
}

// A stopped fork loop's error is never cut into its detail line: the state says the loop failed,
// and the error follows on at most two lines, wrapped between words, then an ellipsis.
func TestTaskWatchShowsAForkLoopsErrorOnItsOwnLines(t *testing.T) {
	reason := `mcp__coop-tasks__tasks_set_subtasks: invalid arguments: json: unknown field "title" in the request the agent sent while it was setting the subtasks of its task for the second time today`
	task := mergedTask{Item: inProgress("x", "Drain sockets", 4, 4), fork: "portal-a", phase: ForkAssignmentPaused, agent: "claude", stopReason: reason}
	got := renderSections(ui.Palette{}, []mergedTask{task}, 0, 64)[0]
	want := []string{
		"Stopped (1)",
		"  ✕ Drain sockets",
		"    4/4 · claude in fork portal-a · loop failed",
		"    mcp__coop-tasks__tasks_set_subtasks: invalid arguments:",
		"    json: unknown field \"title\" in the request the agent sent…",
		"    to resume, run: coop fork portal-a --loop -d",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stopped row =\n%q\nwant\n%q", got, want)
	}
	p := ui.Colored()
	if colored := renderSections(p, []mergedTask{task}, 0, 64)[0]; colored[3] != "    "+p.Dim(p.Red("mcp__coop-tasks__tasks_set_subtasks: invalid arguments:")) {
		t.Errorf("error line = %q, want it dim red", colored[3])
	}
	if lines := failureLines("short error", 64); !reflect.DeepEqual(lines, []string{"short error"}) {
		t.Errorf("short error = %q", lines)
	}
	// A wrap that would leave one short word on a line of its own ends the line before it instead.
	if lines := failureLines("make: *** [Makefile:260: check-local] Error 1", 44); !reflect.DeepEqual(lines, []string{"make: *** [Makefile:260: check-local] Error…"}) {
		t.Errorf("lone token = %q", lines)
	}
	if lines := failureLines("the box image is missing and could not be built today", 42); !reflect.DeepEqual(lines, []string{"the box image is missing and could not be", "built today"}) {
		t.Errorf("a two-word last line = %q", lines)
	}
}

// Stopped tasks one set of commands moves on form a group: each keeps its title and detail lines,
// the commands come once after the last, worded for several, and the rows that do not fit read as
// how many more and what they share. A different fix starts a new group after one blank line.
func TestTaskWatchGroupsStoppedTasksByTheirFix(t *testing.T) {
	release := "2026-11-05-build-subscriptions-and-competitive-bot-features"
	var merged []mergedTask
	for i, title := range []string{"Verify Gateway takeover by a waiting bot replica", "Verify interrupted log sends across packaged app restart"} {
		merged = append(merged, mergedTask{Item: inProgress(fmt.Sprintf("a-%d", i), title, 3, 3), since: ago(time.Duration(101+9*i) * time.Hour)})
	}
	for i := range 8 {
		merged = append(merged, mergedTask{Item: inProgress(fmt.Sprintf("b-%d", i), "Later", 3, 3), since: ago(time.Hour)})
	}
	merged = append(merged, mergedTask{Item: inProgress(release, "Build subscriptions and competitive bot features", 19, 20), owner: "claimed by claude", agent: "claude"})
	sections := boardSections(ui.Palette{}, merged, nil, 0, 120)
	want := []string{
		"Stopped (11)",
		"  ✕ Verify Gateway takeover by a waiting bot replica",
		"    3/3 · worker gone 4d ago",
		"  ✕ Verify interrupted log sends across packaged app restart",
		"    3/3 · worker gone 4d ago",
		"  + 8 more with no worker",
		"    to resume them, run: coop loop",
		"",
		"  ✕ Build subscriptions and competitive bot features",
		"    19/20 · claude · claim held by a session that has ended",
		"    to free it, run: coop tasks release " + release,
	}
	if got := sections[0].render(3); !reflect.DeepEqual(got, want) {
		t.Errorf("stopped =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Three tasks of one fork whose loop failed: the error once, then the restart.
	var fork []mergedTask
	for _, id := range []string{"x", "y", "z"} {
		fork = append(fork, mergedTask{Item: inProgress(id, "Task "+id, 3, 3), fork: "mcp", phase: ForkAssignmentReviewing, agent: "claude", stopReason: "task x projection changed after review"})
	}
	got := renderSections(ui.Palette{}, fork, 0, 120)[0]
	want = []string{
		"Stopped (3)",
		"  ✕ Task x", "    3/3 · claude in fork mcp · loop failed",
		"  ✕ Task y", "    3/3 · claude in fork mcp · loop failed",
		"  ✕ Task z", "    3/3 · claude in fork mcp · loop failed",
		"    task x projection changed after review",
		"    to resume them, run: coop fork mcp --loop -d",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("one fork's stopped tasks =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := sectionShown(boardSections(ui.Palette{}, fork, nil, 0, 120), 8); got[0] != 1 {
		t.Errorf("a short terminal shows %d of the fork's rows, want 1", got)
	}
}

// The live board never runs past the bottom of the terminal: eleven stopped tasks on 24 rows keep
// the header, a group's commands and the Ctrl-C footer, and the rows that do not fit are counted.
func TestTaskWatchLiveFrameFitsTheTerminal(t *testing.T) {
	var merged []mergedTask
	for i := range 11 {
		merged = append(merged, mergedTask{Item: inProgress(fmt.Sprintf("stop-%02d", i), fmt.Sprintf("Stopped %d", i), 3, 3), agent: "codex", since: ago(time.Hour)})
	}
	merged = append(merged, mergedTask{Item: inProgress("now", "Busy", 0, 1), lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex"}})
	for i := range 6 {
		merged = append(merged,
			mergedTask{Item: Item{ID: fmt.Sprintf("ask-%d", i), Title: fmt.Sprintf("Ask %d", i), State: StateBlocked}},
			mergedTask{Item: Item{ID: fmt.Sprintf("todo-%d", i), Title: fmt.Sprintf("Todo %d", i), State: StateTodo}})
	}
	for _, height := range []int{24, 12, 40} {
		frame := liveFrame(boardRead{merged: merged}, 0, 80, height)
		board := strings.Join(frame, "\n")
		if len(frame) > height {
			t.Errorf("height %d: frame has %d lines:\n%s", height, len(frame), board)
		}
		if !strings.HasPrefix(frame[0], "6 todo · 1 working · 11 stopped") || frame[len(frame)-1] != watchExitFooter || !strings.Contains(board, "Now (1)\n  ") || !strings.Contains(board, " Busy\n") {
			t.Errorf("height %d lost the header, Now or the footer:\n%s", height, board)
		}
		if height == 24 && (!strings.Contains(board, "more with no worker\n    to resume them, run: coop loop") || strings.Contains(board, "Stopped (11)\n\n")) {
			t.Errorf("height %d: stopped group =\n%s", height, board)
		}
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
	joined := strings.Join(tasksWatchFrame(boardRead{snapshot: snapshot}, 0, 120, 0), "\n")
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
		2 * time.Hour:                 "2h",
		26*time.Hour + 59*time.Minute: "26h",
		101 * time.Hour:               "4d",
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
