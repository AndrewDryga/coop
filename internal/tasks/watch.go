package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// watchSource is one source feeding the board — a configured queue (labeled by its path) or an
// active fork (labeled by its name) — with that source's own task counts.
type watchSource struct {
	label  string
	counts TaskCounts
}

// mergedTask is a task in the unified view: the task plus the fork that owns it (claimed or worked
// it), or "" when it lives in the local queue. Sources are deduped by task id.
type mergedTask struct {
	Item
	fork       string
	queue      string
	phase      ForkAssignmentPhase
	owner      string
	executions []forkspace.ExecutionObservation
	lease      TaskLeaseObservation
}

// TasksWatch is the live `coop tasks watch` board: every task across the configured queue(s) AND
// any active fork, merged into one view and deduped by id — so you see the whole backlog and who's
// on what (in progress with the fork that claimed it, then todo, blocked), refreshed in place.
// It auto-exits only when everything is drained; without a TTY it prints the list once
// (pipe-safe).
func TasksWatch(host Host, repo string, rels []string, jsonOutput ...bool) (int, error) {
	roots := make([]string, len(rels))
	for i, rel := range rels {
		if filepath.IsAbs(rel) {
			roots[i] = filepath.Clean(rel)
		} else {
			roots[i] = filepath.Join(repo, rel)
		}
	}
	read := func() (ProjectSnapshot, []watchSource, []mergedTask, int, bool) {
		snapshot := ReadProjectSnapshot(repo, roots)
		var sources []watchSource
		for _, queue := range snapshot.Queues {
			sources = append(sources, watchSource{label: queue.Label, counts: queue.Counts})
		}
		merged := make([]mergedTask, 0, len(snapshot.Tasks))
		showQueues := len(snapshot.Queues) > 1
		for _, task := range snapshot.Tasks {
			m := mergedTask{Item: task.Item, phase: task.Phase, executions: task.Executions}
			if task.State != StateTodo && task.ownerRecord != nil {
				m.owner = taskOwnerLabel(*task.ownerRecord, false)
			}
			if showQueues {
				m.queue = task.QueueLabel
			}
			if task.Fork != nil {
				m.fork = task.Fork.Name
			} else if task.State == StateInProgress {
				m.lease = observeTaskLease(task.Item, time.Now())
			}
			merged = append(merged, m)
		}
		sort.Slice(merged, func(i, j int) bool {
			if merged[i].ID != merged[j].ID {
				return merged[i].ID < merged[j].ID
			}
			return merged[i].Dir < merged[j].Dir
		})
		running := snapshot.ActiveExecutions()
		starting := false
		for _, fork := range snapshot.Forks {
			if fork.DetachedRunning {
				running++
			}
			starting = starting || fork.Starting
		}
		return snapshot, sources, merged, running, starting
	}

	// An unreadable queue is unknown work, never a drained one: every view still shows what it
	// could read (the snapshot names the failure), then exits 1 instead of reporting a drain.
	// ReadTaskTree already retries a torn read, so a failure here is durable, not a race.
	if len(jsonOutput) > 0 && jsonOutput[0] {
		snapshot, _, _, _, _ := read()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(snapshot); err != nil {
			return 0, err
		}
		if err := snapshot.QueueError(); err != nil {
			return 1, err
		}
		return 0, nil
	}

	if !ui.IsTerminal(os.Stdout) || !ui.IsTerminal(os.Stderr) {
		// Pipe output is the same snapshot and renderer as the live view, sampled once.
		snapshot, sources, merged, _, _ := read()
		for _, line := range tasksWatchFrameWithSnapshot(sources, merged, snapshot, 0, 120) {
			fmt.Println(line)
		}
		if err := snapshot.QueueError(); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if snapshot, _, merged, _, _ := read(); len(merged) == 0 && !snapshotHasVisibleActivity(snapshot) {
		ui.Note("no tasks yet — add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	} else if err := snapshot.QueueError(); err != nil {
		return 1, err
	}

	width := func() int { return ui.TermWidth(os.Stdout) }
	screen := ui.NewAltScreen(os.Stdout, width)
	sawActive, sawFork := false, false // concurrent-fork startup guard — see tasksWatchSettling
	var queueErr error                 // a queue that turned unreadable mid-watch ends the board with exit 1
	tick := func(spin int) ([]string, bool) {
		snapshot, sources, merged, running, starting := read()
		c := mergedCounts(merged)
		// The Ctrl-C footer belongs to the LIVE view only: a pipe or --json has no keyboard, and a
		// captured snapshot must not carry an instruction nobody can follow.
		frame := append(tasksWatchFrameWithSnapshot(sources, merged, snapshot, spin, width()), "", watchExitFooter)
		screen.Frame(frame)
		if queueErr = snapshot.QueueError(); queueErr != nil {
			return frame, true // settle on the failure so the loop's debounce still bounds the exit
		}
		if running > 0 || c.Doing > 0 {
			sawActive = true // a fork/loop is on it — work has started
		}
		if starting {
			sawFork = true // a detached launch reservation is the one real startup window
		}
		// tasksWatchSettling holds the auto-exit a few ticks against a torn read and adds the startup
		// guard so just-launched forks don't conclude "drained" before one claims.
		return frame, tasksWatchSettling(c, running, sawActive, sawFork)
	}
	code, err := host.runWatchLoop(screen, tick, func() {
		if queueErr == nil {
			ui.OK("queue drained — every task is done")
		}
	})
	if err == nil && queueErr != nil {
		return 1, queueErr
	}
	return code, err
}

func snapshotHasVisibleActivity(snapshot ProjectSnapshot) bool {
	if len(snapshot.Executions) > 0 || len(snapshot.Problems) > 0 {
		return true
	}
	for _, fork := range snapshot.Forks {
		if forkHasVisibleActivity(fork) {
			return true
		}
	}
	return false
}

func forkHasVisibleActivity(fork ProjectForkSnapshot) bool {
	// A reservation protects a persistent remote workspace; by itself it says nothing about
	// current task work and stays available in the JSON snapshot instead of filling the board.
	return fork.Starting || fork.DetachedRunning || fork.CleanupPending || fork.Assignments > 0 ||
		fork.Candidate || fork.PendingLand || fork.CandidatePhase == forkCandidateSuperseding ||
		fork.CandidatePhase == forkCandidateReviewing || fork.CandidatePhase == forkCandidatePublishing
}

// watchExitFooter closes the live board. It is NOT part of the frame renderer: only the interactive
// view can be left with Ctrl-C.
const watchExitFooter = "Press Ctrl-C to leave this view."

// tasksWatchFrameWithSnapshot is the task board plus the problems its snapshot found. The board is
// about TASKS: a box or session that is running without a task of its own is execution data the
// JSON snapshot still carries (and activity accounting and auto-exit still read), not a second
// inventory on screen — a task-linked fork keeps its own ← name cue on the task's row.
func tasksWatchFrameWithSnapshot(sources []watchSource, merged []mergedTask, snapshot ProjectSnapshot, spin, width int) []string {
	frame := tasksWatchFrame(sources, merged, spin, width)
	var visibleForks []ProjectForkSnapshot
	for _, fork := range snapshot.Forks {
		if len(fork.Executions) == 0 && forkHasVisibleActivity(fork) {
			visibleForks = append(visibleForks, fork)
		}
	}
	if len(visibleForks) > 0 {
		frame = append(frame, "", "forks")
	}
	for _, fork := range visibleForks {
		state := "idle"
		switch {
		case fork.Starting:
			state = "starting"
		case fork.DetachedRunning:
			state = "detached loop running"
		case fork.CleanupPending:
			state = "cleanup-pending"
		case fork.PendingLand:
			state = "landing"
		case fork.CandidatePhase == forkCandidateSuperseding:
			state = fmt.Sprintf("review round %d pending", fork.CandidateRound)
		case fork.CandidatePhase == forkCandidateReviewing:
			state = fmt.Sprintf("reviewing round %d", fork.CandidateRound)
		case fork.CandidatePhase == forkCandidatePublishing:
			state = fmt.Sprintf("publishing round %d", fork.CandidateRound)
		case fork.Candidate:
			state = fmt.Sprintf("ready · round %d", fork.CandidateRound)
		case fork.Assignments > 0:
			state = fmt.Sprintf("%d assignment(s)", fork.Assignments)
		}
		frame = append(frame, "  "+fork.Name+" · "+state)
	}
	if len(snapshot.Problems) > 0 {
		frame = append(frame, "", "problems")
		for _, problem := range snapshot.Problems {
			frame = append(frame, "  "+truncate(oneLineTitle(problem), width-3))
		}
	}
	return frame
}

// mergedCounts tallies the deduped task set — each task counted once, by its winning state.
func mergedCounts(merged []mergedTask) TaskCounts {
	items := make([]Item, len(merged))
	for i, m := range merged {
		items[i] = m.Item
	}
	c, _ := TaskTreeCounts(items)
	return c
}

// tasksDrained reports whether the queue has no work left — nothing todo, in progress, or blocked,
// so every task is done (or there are none). It's the auto-exit condition for `coop tasks watch`:
// a blocked or unfinished-but-idle queue is NOT drained, so the watch keeps running.
func tasksDrained(c TaskCounts) bool {
	return c.Todo == 0 && c.Doing == 0 && c.Blocked == 0
}

// tasksWatchSettling reports whether this tick counts toward auto-exit: the queue is drained AND no
// fork is running, AND either work has already been
// seen (sawActive) or no fork ever appeared (a plain local watch, nothing to wait for). The guard
// stops just-launched forks, whose boxes are still spawning and whose queues read idle for a tick,
// from concluding "drained" and exiting in its startup window (watchIdleExit is only ~1s of ticks).
func tasksWatchSettling(c TaskCounts, running int, sawActive, sawFork bool) bool {
	return tasksDrained(c) && running == 0 && (sawActive || !sawFork)
}

// tasksWatchFrame renders the unified board. A single source leads with just the progress bar (no
// label); several sources — configured queues and/or active forks — each get a labeled progress
// line, so they're tellable apart. Below, the deduped tasks group by state — in progress (with the
// fork that claimed it), todo, blocked; done is the header count. Pure, so it unit-tests headless.
func tasksWatchFrame(sources []watchSource, merged []mergedTask, spin, width int) []string {
	p := ui.For(os.Stdout)
	// Lead with the whole picture: the merged (deduped) progress bar + per-state counter.
	out := []string{tasksProgressLine(p, mergedCounts(merged))}
	// With several sources — the local queue and/or active forks — break them down compactly, so a
	// glance shows which queue or fork is how far along.
	if len(sources) > 1 {
		w := 0
		for _, s := range sources {
			if len(s.label) > w {
				w = len(s.label)
			}
		}
		for _, s := range sources {
			out = append(out, sourceLine(p, s.label, w, s.counts))
		}
	}
	out = append(out, "")
	return append(out, mergedQueue(p, merged, spin, width)...)
}

// tasksProgressLine is the overall header: the merged progress bar and the per-state counts (each in
// the state's color). No status glyph — the bar and counts already convey state.
func tasksProgressLine(p ui.Palette, c TaskCounts) string {
	return fmt.Sprintf("%s  %s", ui.ProgressBarStates(c.Done, c.Doing, c.Blocked, c.Total(), 22), tasksCountSummary(p, c))
}

// sourceLine is one source's compact breakdown — its label (queue path or fork name), a small bar
// (done cyan, in-progress yellow, blocked red), done/total, and the blocked count when any — so
// several queues/forks each fit on one line under the overall header and live/parked work is visible.
func sourceLine(p ui.Palette, label string, w int, c TaskCounts) string {
	line := fmt.Sprintf("  %s  %s  %s/%d", p.Bold(padRight(label, w)), ui.ProgressBarStates(c.Done, c.Doing, c.Blocked, c.Total(), 14), p.Green(fmt.Sprintf("%d", c.Done)), c.Total())
	if c.Blocked > 0 {
		line += p.Dim(" · ") + p.Red(fmt.Sprintf("%d blocked", c.Blocked))
	}
	return line
}

// tasksCountSummary is the per-state breakdown shown after the bar — todo · in_progress · blocked ·
// done — each painted by the shared state key (cyan / yellow / red / green), so a glance maps color
// to state. Every state shows, even at zero, so the colors read as a consistent legend.
func tasksCountSummary(p ui.Palette, c TaskCounts) string {
	cells := []struct {
		state string
		n     int
	}{
		{StateTodo, c.Todo},
		{StateInProgress, c.Doing},
		{StateBlocked, c.Blocked},
		{StateDone, c.Done},
	}
	out := make([]string, len(cells))
	for i, cell := range cells {
		out[i] = paintState(p, cell.state, fmt.Sprintf("%d %s", cell.n, StateLabel(cell.state)))
	}
	return strings.Join(out, p.Dim(" · "))
}

// mergedQueue renders the deduped tasks as ONE queue-ordered list — in_progress (being worked), then
// todo (up next), then blocked (parked) — with no per-state group headers: each row's icon+color
// (taskWatchMarker) carries its state, matching the top counter legend. Active work (in_progress and
// blocked) is never elided; only the cold todo backlog tail is capped so the board stays glanceable.
// An in-progress task claimed by a fork is tagged (← name). Done tasks are omitted (header count).
func mergedQueue(p ui.Palette, merged []mergedTask, spin, width int) []string {
	byState := map[string][]mergedTask{}
	for _, m := range merged {
		byState[m.State] = append(byState[m.State], m)
	}
	const (
		todoCap            = 8 // cap only the cold todo backlog; active work always shows in full
		taskRowPrefixWidth = 4 // two-space indent + one-column marker + separating space
	)
	var out []string
	emit := func(m mergedTask) {
		suffix := ""
		if n := len(m.Subtasks); n > 0 {
			suffix = fmt.Sprintf(" (%d/%d)", m.doneSubtasks(), n)
		}
		if m.State != StateTodo && m.fork != "" {
			suffix += "  ← " + m.fork
			if m.phase != "" {
				suffix += " (" + string(m.phase) + ")"
			} else if m.State == StateInProgress {
				suffix += " · " + m.lease.label()
			}
		} else if m.State != StateTodo && m.owner != "" {
			suffix += "  ← " + m.owner
		}
		if queue := strings.TrimSuffix(m.queue, "/"+TasksRoot); queue != "" && queue != TasksRoot {
			suffix += " · " + queue
		}
		// A claimed task nobody is actively holding a lock on would otherwise read "unleased" — a
		// word that, beside "claimed by", wrongly suggests the loop may take it (see inProgressMarker).
		// The lease label stays only while a lease is actually held: busy or stalled, never unleased.
		if m.State == StateInProgress && m.fork == "" && (m.owner == "" || m.lease.State != leaseUnleased) {
			if suffix == "" {
				suffix = " · " + m.lease.label()
			} else {
				suffix += " · " + m.lease.label()
			}
		}
		// AltScreen leaves the terminal's final column empty so a full row cannot auto-wrap. Give
		// the title everything before that safety column and the row's fixed prefix/suffix.
		titleWidth := width - 1 - taskRowPrefixWidth - len([]rune(suffix))
		line := "  " + taskWatchMarker(p, m.State, spin) + " " + truncate(oneLineTitle(m.Title), titleWidth)
		if suffix != "" {
			line += p.Dim(suffix)
		}
		out = append(out, line)
	}
	for _, m := range byState[StateInProgress] { // being worked — never elided
		emit(m)
	}
	todo := byState[StateTodo]
	for i, m := range todo {
		if i >= todoCap {
			out = append(out, p.Dim(fmt.Sprintf("  … +%d more", len(todo)-todoCap)))
			break
		}
		emit(m)
	}
	for _, m := range byState[StateBlocked] { // parked on a decision — never elided
		emit(m)
	}
	return out
}

// taskWatchMarker is the one-column per-task mark, colored to match the top counter legend
// (paintState): yellow Corner Run for in-progress, a red flag for blocked, a cyan dot for todo.
func taskWatchMarker(p ui.Palette, state string, spin int) string {
	switch state {
	case StateInProgress:
		return p.Yellow(ui.CompactSpinFrame(spin))
	case StateBlocked:
		return p.Red("⚑")
	default: // todo
		return p.Cyan("○")
	}
}

// oneLineTitle collapses any internal whitespace (a wrapped or multi-line title) to a single line,
// so a task occupies exactly one row in the live board.
func oneLineTitle(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
