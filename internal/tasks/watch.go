package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	fork  string
	queue string
	phase ForkAssignmentPhase
	owner string
	lease TaskLeaseObservation
	// live: something is on the task right now — its fork's loop or session is running, or the
	// process that claimed it is alive. A dead fork loop or owner leaves the task in progress but
	// paused, and the board must not count that as work.
	live bool
	// decision is a blocked task's one-line state: its question, or why there is none to answer.
	decision string
	// doneToday: a done task finished today. The board's progress is today's, not the archive's.
	doneToday bool
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
	read := func() boardRead {
		snapshot := ReadProjectSnapshot(repo, roots)
		merged := make([]mergedTask, 0, len(snapshot.Tasks))
		showQueues := len(snapshot.Queues) > 1
		liveForks := liveForkGenerations(snapshot.Forks)
		now := time.Now()
		doneToday := map[string]int{} // by queue label, for the per-source lines
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		for _, task := range snapshot.Tasks {
			m := mergedTask{Item: task.Item, phase: task.Phase}
			if task.Item.State != StateTodo && task.ownerRecord != nil {
				record := *task.ownerRecord
				m.owner = taskOwnerLabel(record, false)
				m.live = record.Kind != TaskOwnerFork && (record.ActorPID == 0 || ownerProcessLive(record))
			}
			if showQueues {
				m.queue = task.QueueLabel
			}
			if task.Fork != nil {
				m.fork = task.Fork.Name
				m.live = liveForks[*task.Fork] || executionsRunning(task.Executions)
			} else if task.Item.State == StateInProgress {
				m.lease = observeTaskLease(task.Item, now)
			}
			switch task.Item.State {
			case StateBlocked:
				m.decision = blockedDecisionNote(task.Item)
			case StateDone:
				if m.doneToday = changedSince(task.Item.Dir, midnight); m.doneToday {
					doneToday[task.QueueLabel]++
				}
			}
			merged = append(merged, m)
		}
		// Each source line describes today's work like the header: the live queue plus what finished
		// today, never the archive.
		var sources []watchSource
		for _, queue := range snapshot.Queues {
			counts := queue.Counts
			counts.Done = doneToday[queue.Label]
			sources = append(sources, watchSource{label: queue.Label, counts: counts})
		}
		sort.Slice(merged, func(i, j int) bool {
			if merged[i].ID != merged[j].ID {
				return merged[i].ID < merged[j].ID
			}
			return merged[i].Dir < merged[j].Dir
		})
		running := snapshot.ActiveExecutions()
		for _, fork := range snapshot.Forks {
			if fork.DetachedRunning {
				running++
			}
		}
		return boardRead{snapshot: snapshot, sources: sources, merged: merged, running: running}
	}

	// An unreadable queue is unknown work, never a drained one: every view still shows what it
	// could read (the snapshot names the failure), then exits 1 instead of reporting a drain.
	// ReadTaskTree already retries a torn read, so a failure here is durable, not a race.
	if len(jsonOutput) > 0 && jsonOutput[0] {
		snapshot := read().snapshot
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
		board := read()
		for _, line := range tasksWatchFrameWithSnapshot(board.sources, board.merged, board.snapshot, 0, 120) {
			fmt.Println(line)
		}
		if err := board.snapshot.QueueError(); err != nil {
			return 1, err
		}
		return 0, nil
	}
	if board := read(); len(board.merged) == 0 && !snapshotHasVisibleActivity(board.snapshot) {
		ui.Note("No tasks yet. Add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	} else if err := board.snapshot.QueueError(); err != nil {
		return 1, err
	}

	width := func() int { return ui.TermWidth(os.Stdout) }
	screen := ui.NewAltScreen(os.Stdout, width)
	var queueErr error // a queue that turned unreadable mid-watch ends the board with exit 1
	cache := &boardCache{roots: roots, read: read}
	tick := func(spin int) ([]string, bool) {
		board := cache.get(time.Now())
		snapshot, sources, merged, running := board.snapshot, board.sources, board.merged, board.running
		c := mergedCounts(merged)
		// The Ctrl-C footer belongs to the LIVE view only: a pipe or --json has no keyboard, and a
		// captured snapshot must not carry an instruction nobody can follow.
		frame := append(tasksWatchFrameWithSnapshot(sources, merged, snapshot, spin, width()), "", watchExitFooter)
		screen.Frame(frame)
		if queueErr = snapshot.QueueError(); queueErr != nil {
			return frame, true // settle on the failure so the loop's debounce still bounds the exit
		}
		return frame, tasksWatchSettling(c, running)
	}
	code, err := host.runWatchLoop(screen, tick, func() {
		if queueErr == nil {
			ui.OK("Every task is done")
		}
	})
	if err == nil && queueErr != nil {
		return 1, queueErr
	}
	return code, err
}

// liveForkGenerations is each fork generation whose loop or session is running. A task held by an
// older generation of a recreated fork must not borrow the new generation's running loop.
func liveForkGenerations(forks []ProjectForkSnapshot) map[forkspace.Identity]bool {
	live := map[forkspace.Identity]bool{}
	for _, fork := range forks {
		if fork.Identity != nil && (fork.DetachedRunning || fork.DetachedSystem != "" || fork.ActiveExecutions > 0) {
			live[*fork.Identity] = true
		}
	}
	return live
}

// changedSince reports whether a done task's folder arrived in or changed in its state directory
// since t. The move into done updates the folder's change time, not its modification time, and
// completion does not always write inside it, so the later of the two is when it finished.
func changedSince(dir string, t time.Time) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	at := info.ModTime()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if changed := time.Unix(statChangeTime(stat)); changed.After(at) {
			at = changed
		}
	}
	return !at.Before(t)
}

func executionsRunning(executions []forkspace.ExecutionObservation) bool {
	for _, e := range executions {
		if e.Running {
			return true
		}
	}
	return false
}

// blockedDecisionNote is the line under a blocked task: the question it waits on, so the board says
// what you are being asked without opening decision.md. An answered or unwritten decision has no
// question to weigh, so the note says that instead.
func blockedDecisionNote(item Item) string {
	path := filepath.Join(item.Dir, "decision.md")
	body, exists, err := readOptionalTaskMetadataPath(path)
	switch {
	case err != nil:
		return ""
	case !exists || decisionUnwritten(string(body)):
		return "Decision not written yet"
	}
	if answered, err := decisionResolved(path); err == nil && answered {
		return "Answered, still blocked"
	}
	return decisionQuestion(string(body))
}

// boardRead is one full read of what a board shows.
type boardRead struct {
	snapshot ProjectSnapshot
	sources  []watchSource
	merged   []mergedTask
	running  int
}

// watchRefresh bounds how stale a live board may get about what changes without a file changing:
// a fork starting or a run ending.
const watchRefresh = 10 * time.Second

// boardCache serves a live board's ticks. One full read opens every task's records, done tasks
// included: about 70 ms for a 650-task queue, so a board that re-read on every 400 ms tick spent a
// third of a CPU showing nothing new. A tick re-reads only when something the board lists changed
// on disk, or after watchRefresh. A held lease turns stalled by its heartbeat's age alone, so the
// lease of each task in progress is still looked at every tick.
type boardCache struct {
	roots  []string
	read   func() boardRead
	at     time.Time
	stamps string
	board  boardRead
}

func (c *boardCache) get(now time.Time) boardRead {
	stamps := watchFingerprint(c.roots, c.board)
	if c.at.IsZero() || stamps != c.stamps || now.Sub(c.at) >= watchRefresh {
		// the stamps are taken before the read, so a change during it is seen next tick
		c.board, c.at, c.stamps = c.read(), now, stamps
		return c.board
	}
	for i, m := range c.board.merged {
		if m.State == StateInProgress && m.fork == "" {
			c.board.merged[i].lease = observeTaskLease(m.Item, now)
		}
	}
	return c.board
}

// watchFingerprint changes when something a board lists could have: a task folder arriving in or
// leaving a state, or a listed task's folder or files changing. Claims and releases move a folder,
// so they show here too. Done tasks only add to a count, so their folders are left alone.
func watchFingerprint(roots []string, board boardRead) string {
	var b strings.Builder
	stamp := func(path string) {
		if info, err := os.Stat(path); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", path, info.ModTime().UnixNano(), info.Size())
		} else {
			fmt.Fprintf(&b, "%s -\n", path)
		}
	}
	queues := append([]string(nil), roots...)
	for _, queue := range board.snapshot.Queues {
		queues = append(queues, queue.Root)
	}
	for _, root := range queues {
		for _, state := range []string{"", StateTodo, StateInProgress, StateBlocked, StateDone} {
			stamp(filepath.Join(root, state))
		}
	}
	for _, m := range board.merged {
		if m.State == StateDone {
			continue
		}
		stamp(m.Dir)
		for _, name := range []string{"task.md", "state.md", "decision.md"} {
			stamp(filepath.Join(m.Dir, name))
		}
	}
	return b.String()
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
	return fork.DetachedRunning || fork.DetachedSystem != "" || fork.CleanupPending || fork.Assignments > 0 ||
		fork.Candidate || fork.PendingLand || fork.CandidatePhase == forkCandidateSuperseding ||
		fork.CandidatePhase == forkCandidateReviewing || fork.CandidatePhase == forkCandidatePublishing
}

// watchExitFooter closes the live board. It is NOT part of the frame renderer: only the interactive
// view can be left with Ctrl-C.
const watchExitFooter = "Press Ctrl-C to leave this view."

// tasksWatchFrameWithSnapshot is the task board plus the problems its snapshot found. The board is
// about TASKS: a box or session that is running without a task of its own is execution data the
// JSON snapshot still carries (and activity accounting and auto-exit still read), not a second
// inventory on screen. A fork shows on its task's row; only a fork with no task row on the board
// (cleaning up, landing, or between tasks) gets a line of its own.
func tasksWatchFrameWithSnapshot(sources []watchSource, merged []mergedTask, snapshot ProjectSnapshot, spin, width int) []string {
	frame := tasksWatchFrame(sources, merged, spin, width)
	onBoard := map[string]bool{}
	for _, m := range merged {
		if m.fork != "" && m.State != StateTodo && m.State != StateDone {
			onBoard[m.fork] = true
		}
	}
	var visibleForks []ProjectForkSnapshot
	for _, fork := range snapshot.Forks {
		if len(fork.Executions) == 0 && forkHasVisibleActivity(fork) && !onBoard[fork.Name] {
			visibleForks = append(visibleForks, fork)
		}
	}
	if len(visibleForks) > 0 {
		frame = append(frame, "", "forks")
	}
	for _, fork := range visibleForks {
		state := "idle"
		switch {
		case fork.DetachedRunning:
			state = "detached loop running"
		case fork.DetachedSystem != "": // coop fork ls's words: only that system can check the loop
			state = "background loop started on " + fork.DetachedSystem
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
			state = ui.Count(fork.Assignments, "assigned task")
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

// boardCounts is what the header reports: the state counts, how many tasks in progress something is
// actually working, and how many finished today.
type boardCounts struct {
	TaskCounts
	Working   int
	DoneToday int
}

func countBoard(merged []mergedTask) boardCounts {
	b := boardCounts{TaskCounts: mergedCounts(merged)}
	for _, m := range merged {
		if m.working() {
			b.Working++
		}
		if m.State == StateDone && m.doneToday {
			b.DoneToday++
		}
	}
	return b
}

// working reports whether something is on an in-progress task right now. A task a stopped fork,
// a dead claimant or nobody holds is still in progress, but paused: counting it as work made four
// tasks look busy when one was.
func (m mergedTask) working() bool {
	if m.State != StateInProgress {
		return false
	}
	if m.fork != "" && m.phase != "" {
		switch m.phase {
		case ForkAssignmentPreparing, ForkAssignmentWorking, ForkAssignmentReviewing, ForkAssignmentBlocking:
			return m.live
		}
		return false
	}
	return m.lease.State == leaseBusy || (m.owner != "" && m.live)
}

// tasksDrained reports whether the queue has no work left — nothing todo, in progress, or blocked,
// so every task is done (or there are none). It's the auto-exit condition for `coop tasks watch`:
// a blocked or unfinished-but-idle queue is NOT drained, so the watch keeps running.
func tasksDrained(c TaskCounts) bool {
	return c.Todo == 0 && c.Doing == 0 && c.Blocked == 0
}

// tasksWatchSettling reports whether this tick counts toward auto-exit: the queue is drained AND
// nothing is running. A detached fork counts as running from the moment `coop fork -d` returns,
// because its lifecycle record already names the live worker.
func tasksWatchSettling(c TaskCounts, running int) bool {
	return tasksDrained(c) && running == 0
}

// tasksWatchFrame renders the unified board. A single source leads with just the progress bar (no
// label); several sources — configured queues and/or active forks — each get a labeled progress
// line, so they're tellable apart. Below, the deduped tasks: blocked (waiting on you) first, then in
// progress, then todo; done is the header count. Pure, so it unit-tests headless.
func tasksWatchFrame(sources []watchSource, merged []mergedTask, spin, width int) []string {
	p := ui.For(os.Stdout)
	b := countBoard(merged)
	out := []string{tasksProgressLine(p, b)}
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
	out = append(out, mergedQueue(p, merged, spin, width)...)
	if b.Blocked > 0 {
		out = append(out, "", "Answer decisions: coop tasks decisions")
	}
	return out
}

// tasksProgressLine is the overall header: a bar and the per-state counts. Both describe the live
// queue and today's finished work. The archive of done tasks is left out: hundreds of them filled the
// bar, so it never moved during a run.
func tasksProgressLine(p ui.Palette, b boardCounts) string {
	today := TaskCounts{Todo: b.Todo, Doing: b.Doing, Blocked: b.Blocked, Done: b.DoneToday}
	return progressBar(p, today, 22) + tasksCountSummary(p, b)
}

// progressBar is a state bar of width cells and the gap after it, or "" without color: its segments
// differ only by color, and a pipe or NO_COLOR would show one solid bar that says nothing.
func progressBar(p ui.Palette, c TaskCounts, width int) string {
	if !p.Enabled() {
		return ""
	}
	return ui.ProgressBarStates(c.Done, c.Doing, c.Blocked, c.Total(), width) + "  "
}

// sourceLine is one source's compact breakdown — its label (queue path or fork name), a small bar
// (done cyan, in-progress yellow, blocked red), done/total, and the blocked count when any — so
// several queues/forks each fit on one line under the overall header and live/parked work is visible.
func sourceLine(p ui.Palette, label string, w int, c TaskCounts) string {
	line := fmt.Sprintf("  %s  %s%s/%d", p.Bold(padRight(label, w)), progressBar(p, c, 14), p.Green(fmt.Sprintf("%d", c.Done)), c.Total())
	if c.Blocked > 0 {
		line += p.Dim(" · ") + p.Red(fmt.Sprintf("%d blocked", c.Blocked))
	}
	return line
}

// tasksCountSummary is the breakdown after the bar — todo · working · paused · blocked · done today
// — each painted by the shared state key, so a glance maps color to state. Every cell shows, even at
// zero, so the colors read as a consistent legend.
func tasksCountSummary(p ui.Palette, b boardCounts) string {
	cells := []string{
		paintState(p, StateTodo, fmt.Sprintf("%d todo", b.Todo)),
		paintState(p, StateInProgress, fmt.Sprintf("%d working", b.Working)),
		p.Dim(fmt.Sprintf("%d paused", b.Doing-b.Working)),
		paintState(p, StateBlocked, fmt.Sprintf("%d blocked", b.Blocked)),
		paintState(p, StateDone, fmt.Sprintf("%d done today", b.DoneToday)),
	}
	return strings.Join(cells, p.Dim(" · "))
}

// mergedQueue renders the deduped tasks as ONE list — blocked (waiting on you), then in progress,
// then todo (up next) — with no per-state group headers: each row's icon+color (taskWatchMarker)
// carries its state. Blocked and in-progress rows are never elided; only the cold todo backlog tail is
// capped so the board stays glanceable. Done tasks are omitted (header count).
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
		// Only work under way has progress worth showing; a todo or blocked count is noise.
		if n := len(m.Subtasks); n > 0 && m.State == StateInProgress {
			suffix = fmt.Sprintf(" (%d/%d)", m.doneSubtasks(), n)
		}
		if m.State != StateTodo && m.fork != "" {
			suffix += "  ← " + m.fork
			if m.phase != "" {
				suffix += " · " + forkTaskStatus(m)
			} else if m.State == StateInProgress {
				suffix += " · " + leaseText(m.lease)
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
			suffix += " · " + leaseText(m.lease)
		}
		// AltScreen leaves the terminal's final column empty so a full row cannot auto-wrap. Give
		// the title everything before that safety column and the row's fixed prefix/suffix.
		titleWidth := width - 1 - taskRowPrefixWidth - len([]rune(suffix))
		marker := taskWatchMarker(p, m.State, spin)
		if m.State == StateInProgress && !m.working() {
			marker = p.Dim(pausedMarker)
		}
		line := "  " + marker + " " + truncate(oneLineTitle(m.Title), titleWidth)
		if suffix != "" {
			line += p.Dim(suffix)
		}
		out = append(out, line)
		if m.State == StateBlocked && m.decision != "" {
			out = append(out, "    "+p.Dim(truncate(oneLineTitle(m.decision), width-5)))
		}
	}
	for _, m := range byState[StateBlocked] { // waiting on you — never elided
		emit(m)
	}
	for _, m := range byState[StateInProgress] { // being worked or paused — never elided
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
	return out
}

// forkTaskStatus is what a fork is doing with its task and, when nothing is, the command that
// shows why or moves it on. A fork loop that died at start used to read "(paused)" like any other
// fork, with no hint that it would never resume on its own.
func forkTaskStatus(m mergedTask) string {
	switch {
	case m.working():
		return string(m.phase)
	case m.phase == ForkAssignmentReady, m.phase == ForkAssignmentReviewing:
		return "waiting for review · coop fork review " + m.fork
	case m.phase == ForkAssignmentBlocked:
		return "blocked"
	case m.phase == ForkAssignmentPaused:
		return "paused · coop fork logs " + m.fork
	case m.State == StateInProgress:
		return "stopped · coop fork logs " + m.fork
	}
	return string(m.phase)
}

// leaseText is a lease's label plus how long its holder has had the task, so a long-running task
// can be told from a just-started one.
func leaseText(o TaskLeaseObservation) string {
	if o.State == leaseUnleased || o.Held <= 0 {
		return o.label()
	}
	return o.label() + " · " + shortElapsed(o.Held)
}

// shortElapsed is a compact duration for a busy row: "<1m", "14m", "2h 5m".
func shortElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// pausedMarker marks a task in progress that nothing is working: the spinner's shape, standing still.
const pausedMarker = "◻"

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
