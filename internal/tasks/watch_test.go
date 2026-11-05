package tasks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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

// A single source leads with just the progress bar + per-state colored counter, then the actionable
// tasks grouped by state. Done tasks are a header count, never a list; nothing is fork-attributed.
func TestTasksWatchFrame(t *testing.T) {
	items := []Item{
		{ID: "a", Title: "Wire auth", State: StateInProgress, Subtasks: []bool{true, false, false}},
		{ID: "b", Title: "Add retries", State: StateTodo, Subtasks: []bool{false, false}},
		{ID: "c", Title: "Bump deps", State: StateTodo},
		{ID: "d", Title: "Pick a queue backend", State: StateBlocked, Subtasks: []bool{true, true}},
		{ID: "e", Title: "shipped thing", State: StateDone},
		{ID: "f", Title: "another done", State: StateDone},
	}
	c, _ := TaskTreeCounts(items)
	merged := merge(items)
	merged[4].doneToday = true // finished today; the other done task is from the archive
	joined := strings.Join(tasksWatchFrame([]watchSource{{label: ".agent/tasks", counts: c}}, merged, 0, 80), "\n")

	for _, want := range []string{"2 todo", "0 working", "1 paused", "1 blocked", "1 done today", "Answer decisions: coop tasks decisions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("counter should show %q:\n%s", want, joined)
		}
	}
	// A single source carries no path label and no fork attribution — the bar leads.
	if strings.Contains(joined, ".agent/tasks") || strings.Contains(joined, "←") {
		t.Errorf("a single source should show no label and no attribution:\n%s", joined)
	}
	// Blocked (waiting on you) leads, then in progress, then todo; only work under way shows its
	// subtask count.
	order := []string{"Pick a queue backend\n", "Wire auth (1/3) · unleased", "Add retries\n", "Bump deps\n"}
	last := -1
	for _, want := range order {
		at := strings.Index(joined, want)
		if at < 0 {
			t.Errorf("frame missing %q:\n%s", want, joined)
		} else if at < last {
			t.Errorf("%q is out of order (blocked, in progress, todo):\n%s", want, joined)
		}
		last = at
	}
	if strings.Contains(joined, "(0/2)") || strings.Contains(joined, "(2/2)") {
		t.Errorf("todo and blocked rows show subtask counts:\n%s", joined)
	}
	for _, gone := range []string{"shipped thing", "another done"} {
		if strings.Contains(joined, gone) {
			t.Errorf("done task %q must not be listed:\n%s", gone, joined)
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
		want := "Checklist task (0/2)"
		if marker == "x" {
			want = "Checklist task (1/2)"
		}
		if !strings.Contains(out, want) {
			t.Errorf("watch did not read the current checklist %q:\n%s", want, out)
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
	// Nothing holds this task, so it is paused: a still marker, not the spinner.
	if line != "  "+pausedMarker+" Task title · unleased" {
		t.Errorf("compact task row = %q, want %q", line, "  "+pausedMarker+" Task title · unleased")
	}
	for _, tc := range []struct {
		lease TaskLeaseObservation
		want  string
	}{
		{TaskLeaseObservation{State: leaseBusy, Provider: "claude"}, "  ◰ Task title · busy claude"},
		{TaskLeaseObservation{State: leaseBusy, Provider: "claude", Held: 14*time.Minute + 30*time.Second}, "busy claude · 14m"},
		{TaskLeaseObservation{State: leaseStalled, Provider: "codex", Held: 2*time.Hour + 5*time.Minute}, "  ◻ Task title · stalled codex · 2h 5m"},
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
	subtasks := make([]bool, 12)
	subtasks[0] = true
	merged := []mergedTask{{
		Item:  Item{ID: "errors", Title: title, State: StateInProgress, Subtasks: subtasks},
		fork:  "worker",
		lease: TaskLeaseObservation{State: leaseBusy, Provider: "claude"},
	}}
	rowAt := func(width int) string {
		frame := tasksWatchFrame(nil, merged, 0, width)
		return frame[len(frame)-1]
	}

	wide := rowAt(132)
	if !strings.Contains(wide, title) {
		t.Errorf("wide task row should use columns beyond the old fixed title cap: %q", wide)
	}

	const narrowWidth = 52
	merged[0].queue = "api/.agent/tasks"
	narrow := rowAt(narrowWidth)
	if strings.Contains(narrow, title) || !strings.Contains(narrow, "…") {
		t.Errorf("narrow task row should elide its title: %q", narrow)
	}
	if want := " (1/12)  ← worker · busy claude · api"; !strings.HasSuffix(narrow, want) {
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

func TestForkAttributionRemainsVisibleOutsideInProgress(t *testing.T) {
	line := mergedQueue(ui.Palette{}, []mergedTask{{
		Item: Item{Title: "Needs a decision", State: StateBlocked}, fork: "worker", phase: ForkAssignmentBlocked,
	}}, 0, 80)[0]
	if !strings.Contains(line, "← worker · blocked") {
		t.Fatalf("blocked fork-owned row lost attribution: %q", line)
	}
}

func TestTaskWatchHidesTodoClaims(t *testing.T) {
	for _, task := range []mergedTask{
		{owner: "claimed by alice"},
		{fork: "worker", phase: ForkAssignmentBlocked},
	} {
		task.Item = Item{Title: "Queued work", State: StateTodo}
		task.queue = "api/.agent/tasks"
		line := mergedQueue(ui.Palette{}, []mergedTask{task}, 0, 80)[0]
		if want := "  ○ Queued work · api"; line != want {
			t.Errorf("TODO row = %q, want %q", line, want)
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
				if state == StateTodo {
					if strings.Contains(out, "claimed by") || strings.Contains(out, "owner process") {
						t.Errorf("TODO claim appeared in watch:\n%s", out)
					}
				} else {
					who := record.Actor
					if who == "" {
						who = record.User
					}
					if !strings.Contains(out, "claimed by "+who) {
						t.Errorf("watch lost owner identity:\n%s", out)
					}
					if strings.Contains(out, "owner process has stopped") != (process == "stopped") {
						t.Errorf("watch lost process liveness truth:\n%s", out)
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
				line := mergedQueue(ui.Palette{}, []mergedTask{m}, 0, 100)[0]
				want := "Apply schema" + tt.suffix
				if state == StateInProgress {
					want = "Apply schema (1/2)" + tt.suffix + " · busy claude"
				}
				if !strings.HasSuffix(line, " "+want) {
					t.Errorf("%s row = %q, want title and suffix %q", state, line, want)
				}
			}
		})
	}
}

func TestTaskWatchNamesHumanOwnerAndCanonicalQueue(t *testing.T) {
	line := mergedQueue(ui.Palette{}, []mergedTask{{
		Item:  Item{Title: "Apply schema", State: StateInProgress, Subtasks: []bool{true, false}},
		owner: "claimed by alice", queue: "api/.agent/tasks",
	}}, 0, 100)[0]
	for _, want := range []string{"Apply schema (1/2)  ← claimed by alice", " · api"} {
		if !strings.Contains(line, want) {
			t.Fatalf("human-owned multi-queue row lost %q: %q", want, line)
		}
	}
	// A claim is ownership; "unleased" beside it would wrongly suggest the loop may take the task.
	if strings.Contains(line, "unleased") {
		t.Fatalf("a claimed row must not read unleased: %q", line)
	}
	held := mergedQueue(ui.Palette{}, []mergedTask{{
		Item:  Item{Title: "Apply schema", State: StateInProgress},
		owner: "claimed by codex", lease: TaskLeaseObservation{State: leaseBusy, Provider: "codex"},
	}}, 0, 100)[0]
	if !strings.Contains(held, "claimed by codex") || !strings.Contains(held, "busy codex") {
		t.Fatalf("a claimed task with a held lease must show both: %q", held)
	}
	unclaimed := mergedQueue(ui.Palette{}, []mergedTask{{Item: Item{Title: "Apply schema", State: StateInProgress}}}, 0, 100)[0]
	if !strings.Contains(unclaimed, "unleased") {
		t.Fatalf("an unclaimed, unleased task still says so: %q", unclaimed)
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

// A fork-held task says what the fork is doing, and when nothing is (its loop died or stopped, or its
// work waits for review) the command that shows why or moves it on. Only a live fork in a working
// phase counts as work in the header; on 2026-11-05 three forks that died at start read "(paused)"
// like busy work, under "4 in progress" when one task was being worked.
func TestTaskWatchSaysWhatAForkIsDoingWithItsTask(t *testing.T) {
	task := func(phase ForkAssignmentPhase, live bool) mergedTask {
		return mergedTask{Item: Item{ID: "t", Title: "Wire auth", State: StateInProgress}, fork: "sweep-a", phase: phase, live: live}
	}
	for _, tc := range []struct {
		name    string
		task    mergedTask
		working bool
		suffix  string
	}{
		{"working loop", task(ForkAssignmentWorking, true), true, "← sweep-a · working"},
		{"loop died while working", task(ForkAssignmentWorking, false), false, "← sweep-a · stopped · coop fork logs sweep-a"},
		{"paused", task(ForkAssignmentPaused, false), false, "← sweep-a · paused · coop fork logs sweep-a"},
		{"waiting for review", task(ForkAssignmentReady, false), false, "← sweep-a · waiting for review · coop fork review sweep-a"},
		{"finished, loop gone", task(ForkAssignmentReviewing, false), false, "← sweep-a · waiting for review · coop fork review sweep-a"},
		{"final review running", task(ForkAssignmentReviewing, true), true, "← sweep-a · reviewing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := mergedQueue(ui.Palette{}, []mergedTask{tc.task}, 0, 120)[0]
			if !strings.HasSuffix(line, tc.suffix) {
				t.Errorf("row = %q, want it to end with %q", line, tc.suffix)
			}
			if paused := strings.HasPrefix(line, "  "+pausedMarker+" "); paused == tc.working {
				t.Errorf("row = %q: working=%t but paused marker=%t", line, tc.working, paused)
			}
			want := 0
			if tc.working {
				want = 1
			}
			if b := countBoard([]mergedTask{tc.task}); b.Working != want || b.Doing != 1 {
				t.Errorf("counts = %+v, want %d working of 1 in progress", b, want)
			}
		})
	}
}

// A fork with a task on the board shows there, not again in a forks section; a fork with no task row
// (landing, cleaning up, between tasks) keeps its own line.
func TestTaskWatchListsAForkOnceOnItsTaskRow(t *testing.T) {
	snapshot := ProjectSnapshot{Forks: []ProjectForkSnapshot{
		{Name: "sweep-a", Assignments: 1},
		{Name: "lander", PendingLand: true},
	}}
	merged := []mergedTask{{Item: Item{ID: "t", Title: "Wire auth", State: StateInProgress}, fork: "sweep-a", phase: ForkAssignmentPaused}}
	frame := strings.Join(tasksWatchFrameWithSnapshot(nil, merged, snapshot, 0, 120), "\n")
	if strings.Contains(frame, "sweep-a · 1 assigned task") || strings.Count(frame, "sweep-a") != 2 { // the row's name and its logs hint
		t.Errorf("a fork with a task row was listed again:\n%s", frame)
	}
	if !strings.Contains(frame, "\nforks\n  lander · landing") {
		t.Errorf("a fork without a task row lost its line:\n%s", frame)
	}
}

// A blocked task shows the question it waits on under its row, or why there is none to answer, and
// the board ends with the command that answers it. A task moved into done today is today's work.
func TestTaskWatchShowsDecisionsAndTodaysWork(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, TasksRoot)
	for id, decision := range map[string]string{
		"open":       "# Decision: Which database should uploads use?\n\n**Resolution:**\n",
		"answered":   "# Decision: Which region?\n\n**Resolution:** us-east\n",
		"unwritten":  "# Decision: x\n\n" + decisionScaffoldMarker + "\n",
		"undecided2": "",
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
		"Task open\n    Which database should uploads use?\n",
		"Task answered\n    Answered, still blocked\n",
		"Task unwritten\n    Decision not written yet\n",
		"Task undecided2\n    Decision not written yet\n",
		"1 done today",
		"Answer decisions: coop tasks decisions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("watch is missing %q:\n%s", want, out)
		}
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

// A person's claim with no process to check (coop tasks claim from a terminal) is someone working
// the task; a claimant whose process stopped is not.
func TestTaskWatchCountsAPersonsClaimAsWork(t *testing.T) {
	claimed := mergedTask{Item: Item{ID: "t", Title: "Wire auth", State: StateInProgress}, owner: "claimed by alice", live: true}
	stopped := claimed
	stopped.live = false
	if b := countBoard([]mergedTask{claimed, stopped}); b.Working != 1 || b.Doing != 2 {
		t.Errorf("counts = %+v, want the live claim working and the stopped one paused", b)
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
