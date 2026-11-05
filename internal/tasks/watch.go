package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/processidentity"
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
	// stopped, and the board must not count that as work.
	live bool
	// agent is who works the task: the claimant, the fork's agent, or the agent a worker that has
	// gone last ran. A held lease names its own agent, read fresh every tick.
	agent string
	// system is the other operating system a fork loop or claimant runs on: this one cannot check it.
	system string
	// since is when the task's current state began: claimed, its fork phase last changed, blocked,
	// or its gone worker was last seen. Zero when unknown.
	since time.Time
	// stopReason is why a stopped fork's loop ended, as its loop recorded or logged it.
	stopReason string
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
		forkSystems := map[forkspace.Identity]string{}
		for _, fork := range snapshot.Forks {
			if fork.Identity != nil && fork.DetachedSystem != "" {
				forkSystems[*fork.Identity] = fork.DetachedSystem
			}
		}
		now := time.Now()
		doneToday := map[string]int{} // by queue label, for the per-source lines
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		for _, task := range snapshot.Tasks {
			m := mergedTask{Item: task.Item, phase: task.Phase}
			if task.Item.State != StateTodo && task.ownerRecord != nil && task.ownerRecord.Kind != TaskOwnerFork {
				record := *task.ownerRecord
				m.owner = taskOwnerLabel(record, false)
				m.live = record.ActorPID == 0 || ownerProcessLive(record)
				m.agent, m.since = record.User, record.ClaimedAt
				if record.Actor != "" {
					m.agent = record.Actor
				}
				if claimantElsewhere(record) {
					m.system = processidentity.System(record.ActorStart)
				}
			}
			if showQueues {
				m.queue = task.QueueLabel
			}
			if task.Fork != nil {
				m.fork = task.Fork.Name
				m.live = liveForks[*task.Fork] || executionsRunning(task.Executions)
				m.system = forkSystems[*task.Fork]
				m.agent = forkAgent(repo, task.Fork.Name)
				if task.ownerRecord != nil && task.ownerRecord.Fork != nil {
					m.since = task.ownerRecord.Fork.UpdatedAt
				}
				if task.Item.State == StateInProgress && classifyTask(m).section == sectionStopped {
					m.stopReason = forkStopReason(repo, *task.Fork)
				}
			} else if task.Item.State == StateInProgress {
				m.lease = observeTaskLease(task.Item, now)
				if m.owner == "" && m.lease.State == leaseUnleased {
					m.agent, m.since = goneWorker(task.Item)
				}
			}
			switch task.Item.State {
			case StateBlocked:
				m.since = folderChanged(task.Item.Dir)
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
	at := folderChanged(dir)
	return !at.IsZero() && !at.Before(t)
}

// folderChanged is when a task folder last arrived in its state directory or changed: the later of
// its modification and change times. Zero when it cannot be read.
func folderChanged(dir string) time.Time {
	info, err := os.Stat(dir)
	if err != nil {
		return time.Time{}
	}
	at := info.ModTime()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if changed := time.Unix(statChangeTime(stat)); changed.After(at) {
			at = changed
		}
	}
	return at
}

// goneWorker is the agent and last sign of life of the worker that left a task in progress with
// nothing holding it. A worker that crashed leaves its lease record behind with its last heartbeat;
// otherwise the task's own files say when anyone last worked on it.
func goneWorker(item Item) (string, time.Time) {
	if meta, ok := readLeaseAuthorityMetadata(filepath.Dir(filepath.Dir(item.Dir)), item.ID); ok && !meta.HeartbeatAt.IsZero() {
		return knownAgent(meta.Provider), meta.HeartbeatAt
	}
	last := folderChanged(item.Dir)
	for _, name := range []string{"task.md", "state.md", "log.md"} {
		if info, err := os.Lstat(filepath.Join(item.Dir, name)); err == nil && info.ModTime().After(last) {
			last = info.ModTime()
		}
	}
	return "", last
}

// knownAgent is an agent name safe to print, or "" when there is none to name.
func knownAgent(name string) string {
	if agent := leaseProvider(name); agent != "unknown" {
		return agent
	}
	return ""
}

// forkAgent is the agent a fork was last run with, from the note coop fork keeps in its workspace
// (forkctl.SaveForkAgent). The workspace is agent-writable, so the note is read through the no-follow
// metadata root and printed only when it reads as a name.
func forkAgent(repo, name string) string {
	root, err := OpenTaskMetadataRoot(filepath.Join(forkspace.Workspace(repo, name), ".coop"))
	if err != nil {
		return ""
	}
	defer root.Close()
	data, err := ReadTaskMetadataFile(root, "agent")
	if err != nil {
		return ""
	}
	return knownAgent(strings.TrimSpace(string(data)))
}

// forkStopReason is why a fork's loop ended: the failure the loop recorded for this generation
// (forkctl.NoteForkLoopExit), else the last error line of its log (what coop fork logs shows), else
// "" — nothing says why.
func forkStopReason(repo string, identity forkspace.Identity) string {
	if root, err := OpenTaskMetadataRoot(filepath.Join(forkspace.Workspace(repo, identity.Name), ".coop")); err == nil {
		data, readErr := ReadTaskMetadataFile(root, "loop-error")
		root.Close()
		if generation, reason, ok := strings.Cut(strings.TrimSpace(string(data)), "\n"); readErr == nil && ok && generation == string(identity.Generation) {
			if reason = oneLineTitle(sanitizeCell(reason)); reason != "" {
				return reason
			}
		}
	}
	return lastLoggedError(forkspace.LogPath(repo, identity.Name))
}

// forkLogTail bounds how much of a fork's log the board reads to find its last error.
const forkLogTail = 16 << 10

var ansiSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// lastLoggedError is the last "✗" line (ui.Error) in the tail of a fork's log, or "".
func lastLoggedError(path string) string {
	f, err := forkspace.OpenStateFile(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := max(info.Size()-forkLogTail, 0)
	buf := make([]byte, info.Size()-offset)
	n, _ := f.ReadAt(buf, offset)
	lines := strings.Split(string(buf[:n]), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(ansiSequence.ReplaceAllString(lines[i], ""))
		if reason, ok := strings.CutPrefix(line, "✗ "); ok {
			if reason = oneLineTitle(sanitizeCell(reason)); reason != "" {
				return reason
			}
		}
	}
	return ""
}

func executionsRunning(executions []forkspace.ExecutionObservation) bool {
	for _, e := range executions {
		if e.Running {
			return true
		}
	}
	return false
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

// boardCounts is what the header reports: the state counts, how many tasks are under Now and
// under Stopped, and how many finished today.
type boardCounts struct {
	TaskCounts
	Working   int
	Stopped   int
	DoneToday int
}

func countBoard(merged []mergedTask) boardCounts {
	b := boardCounts{TaskCounts: mergedCounts(merged)}
	for _, m := range merged {
		switch classifyTask(m).section {
		case sectionNow:
			b.Working++
		case sectionStopped:
			b.Stopped++
		}
		if m.State == StateDone && m.doneToday {
			b.DoneToday++
		}
	}
	return b
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

// boardSection is where a task shows on the board.
type boardSection int

const (
	sectionHidden   boardSection = iota // done: a header count, never a row
	sectionNow                          // something is on it
	sectionStopped                      // in progress, nothing on it, and it will not move by itself
	sectionNeedsYou                     // waits on a person: a decision, or a fork's finished work
	sectionUpNext                       // todo
)

// rowTone colors a detail line's state: dim while it moves, yellow while it waits but is alive,
// red when it has stopped.
type rowTone int

const (
	toneDim rowTone = iota
	toneWaiting
	toneStopped
)

// taskRow is what the board says about one task. Now and Stopped rows carry the state that ends
// their detail line, and a Stopped row the commands that move it on. A Needs you row may carry one
// fact for after its title.
type taskRow struct {
	section boardSection
	state   string
	tone    rowTone
	fixVerb string   // "to resume, run:", "to fix, run:" or "to free it, run:"
	fix     []string // the complete commands, in order
}

// classifyTask places a task on the board and says what its detail and fix lines read. Every
// reason it gives comes from what coop already records: the lease and its heartbeat, the claim and
// its process, the fork's phase, its loop and the failure the loop recorded or logged.
func classifyTask(m mergedTask) taskRow {
	switch m.State {
	case StateTodo:
		return taskRow{section: sectionUpNext}
	case StateBlocked:
		return taskRow{section: sectionNeedsYou}
	case StateInProgress:
	default:
		return taskRow{section: sectionHidden}
	}
	if m.fork != "" && m.phase != "" {
		return classifyForkTask(m)
	}
	switch {
	case m.lease.State == leaseBusy:
		return taskRow{section: sectionNow, state: workingFor(m.lease.Held)}
	case m.lease.State == leaseStalled:
		// A held lock means its process still exists; only its heartbeat went quiet.
		return taskRow{section: sectionNow, state: "no heartbeat for " + shortElapsed(m.lease.Quiet), tone: toneWaiting}
	case m.owner != "" && m.system != "":
		return taskRow{section: sectionNow, state: "running on " + m.system}
	case m.owner != "" && m.live:
		return taskRow{section: sectionNow, state: workingFor(sinceOrZero(m.since))}
	case m.owner != "":
		return taskRow{section: sectionStopped, state: "claimed by a session that has ended", tone: toneStopped,
			fixVerb: "to free it, run:", fix: []string{"coop tasks release " + m.ID}}
	}
	state := "worker gone"
	if !m.since.IsZero() {
		state += " " + shortElapsed(sinceOrZero(m.since)) + " ago"
	}
	return taskRow{section: sectionStopped, state: state, tone: toneStopped, fixVerb: "to resume, run:", fix: []string{"coop loop"}}
}

func classifyForkTask(m mergedTask) taskRow {
	switch {
	case m.phase == ForkAssignmentReady, m.phase == ForkAssignmentReviewing && !m.live:
		return taskRow{section: sectionNeedsYou, state: "ready to merge"}
	case m.phase == ForkAssignmentBlocked:
		return taskRow{section: sectionNeedsYou}
	case m.system != "":
		return taskRow{section: sectionNow, state: "running on " + m.system}
	case m.live:
		switch m.phase {
		case ForkAssignmentWorking:
			return taskRow{section: sectionNow, state: workingFor(sinceOrZero(m.since))}
		case ForkAssignmentReviewing:
			return taskRow{section: sectionNow, state: "final review"}
		}
		return taskRow{section: sectionNow, state: string(m.phase)}
	}
	state, verb, fix := forkStopNext(m.fork, m.stopReason)
	return taskRow{section: sectionStopped, state: state, tone: toneStopped, fixVerb: verb, fix: fix}
}

// forkStopNext turns why a fork's loop ended into the state a person reads and the complete set of
// commands that move its task on. A known refusal names its fix and then the restart, because
// fixing the cause alone does not start the loop again; an unknown one is shown as it was said.
func forkStopNext(fork, reason string) (state, verb string, fix []string) {
	restart := "coop fork " + fork + " --loop -d"
	switch {
	case reason == "":
		return "loop ended unexpectedly", "to resume, run:", []string{restart}
	case strings.Contains(reason, "has not been approved"):
		return "network access not approved", "to fix, run:", []string{"coop approve", restart}
	case strings.Contains(reason, "run coop login"):
		return "sign-in no longer accepted", "to fix, run:", []string{"coop login", restart}
	}
	if _, clause, ok := strings.Cut(reason, " cannot start because "); ok && clause != "" {
		reason = clause
	}
	return reason, "to resume, run:", []string{restart}
}

func workingFor(d time.Duration) string {
	if d <= 0 {
		return "working"
	}
	return "working for " + shortElapsed(d)
}

func sinceOrZero(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return max(time.Since(t), 0)
}

// Section caps keep the board glanceable: active work always shows in full, the lists do not.
const (
	needsYouCap = 3
	upNextCap   = 2
)

// tasksWatchFrame renders the unified board: the progress header (and one line per source when
// there are several), then Now, Stopped, Needs you and Up next, each left out when empty. Tasks
// being worked or stopped get a title line and a detail line in one fixed order —
// `<done>/<total> · <agent>[ in fork <name>] · <state>` — and a stopped one a line with the commands
// that move it on. Tasks waiting in a list get one line. Nothing is right-aligned. Pure, so it
// unit-tests headless.
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
	for _, section := range boardSections(p, merged, spin, width) {
		out = append(out, "")
		out = append(out, section...)
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

// tasksCountSummary is the breakdown after the bar — todo · working · stopped · blocked · done today
// — each painted like its section's marker, so a glance maps color to state. Every cell shows, even
// at zero, so the colors read as a consistent legend.
func tasksCountSummary(p ui.Palette, b boardCounts) string {
	cells := []string{
		paintState(p, StateTodo, fmt.Sprintf("%d todo", b.Todo)),
		paintState(p, StateInProgress, fmt.Sprintf("%d working", b.Working)),
		p.Red(fmt.Sprintf("%d stopped", b.Stopped)),
		paintState(p, StateBlocked, fmt.Sprintf("%d blocked", b.Blocked)),
		paintState(p, StateDone, fmt.Sprintf("%d done today", b.DoneToday)),
	}
	return strings.Join(cells, p.Dim(" · "))
}

// span is a piece of a row and how it is painted (nil: as is).
type span struct {
	text  string
	paint func(string) string
}

// fitSpans paints spans in order within room columns, cutting the first that does not fit with an
// ellipsis, so a row ends at the terminal's width and never wraps.
func fitSpans(spans []span, room int) string {
	var b strings.Builder
	for _, s := range spans {
		if room <= 0 {
			break
		}
		text := truncate(s.text, room)
		room -= len([]rune(text))
		if s.paint != nil {
			text = s.paint(text)
		}
		b.WriteString(text)
	}
	return b.String()
}

// boardSections renders Now, Stopped, Needs you and Up next, leaving out an empty one. A section's
// first task starts right under its header; only Stopped tasks, three lines each, are set apart by a
// blank line. A header carries no reason text: Up next its count, Needs you the command that answers.
func boardSections(p ui.Palette, merged []mergedTask, spin, width int) [][]string {
	const (
		rowIndent  = 4 // two-space indent + one-column marker + separating space
		lineIndent = "    "
	)
	// AltScreen leaves the terminal's final column empty so a full row cannot auto-wrap.
	room, lineRoom := width-1-rowIndent, width-1-len(lineIndent)
	var now, stopped, needsYou, upNext []mergedTask
	for _, m := range merged {
		switch classifyTask(m).section {
		case sectionNow:
			now = append(now, m)
		case sectionStopped:
			stopped = append(stopped, m)
		case sectionNeedsYou:
			needsYou = append(needsYou, m)
		case sectionUpNext:
			upNext = append(upNext, m)
		}
	}
	// A title is cut only by the terminal's width, at the end of its line: what follows it goes first.
	titleLine := func(marker string, m mergedTask, extra ...span) string {
		spans := []span{{oneLineTitle(m.Title), nil}}
		if queue := strings.TrimSuffix(m.queue, "/"+TasksRoot); queue != "" && queue != TasksRoot {
			spans = append(spans, span{" · " + queue, p.Dim})
		}
		return "  " + marker + " " + fitSpans(append(spans, extra...), room)
	}
	detailLine := func(m mergedTask, row taskRow) string {
		var parts []string
		if n := len(m.Subtasks); n > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d", m.doneSubtasks(), n))
		}
		agent := m.agent
		if m.fork == "" && m.lease.State != leaseUnleased {
			agent = knownAgent(m.lease.Provider)
		}
		if m.fork != "" { // the one place a fork is named
			agent = strings.TrimSpace(agent + " in fork " + m.fork)
		}
		if agent != "" {
			parts = append(parts, agent)
		}
		paint := p.Faint
		switch row.tone {
		case toneWaiting:
			paint = p.Yellow
		case toneStopped:
			paint = p.Red
		}
		var spans []span
		if len(parts) > 0 {
			spans = append(spans, span{strings.Join(parts, " · ") + " · ", p.Faint})
		}
		return lineIndent + fitSpans(append(spans, span{row.state, paint}), lineRoom)
	}
	fixLine := func(row taskRow) string {
		spans := []span{{row.fixVerb + " ", nil}}
		for i, command := range row.fix {
			if i > 0 {
				spans = append(spans, span{", then ", nil})
			}
			spans = append(spans, span{command, p.Bold})
		}
		return lineIndent + fitSpans(spans, lineRoom)
	}
	more := func(n int) string { return p.Dim(fmt.Sprintf("  + %d more", n)) }

	var sections [][]string
	if len(now) > 0 {
		lines := []string{"Now"}
		for _, m := range now {
			lines = append(lines, titleLine(p.Yellow(ui.CompactSpinFrame(spin)), m), detailLine(m, classifyTask(m)))
		}
		sections = append(sections, lines)
	}
	if len(stopped) > 0 {
		lines := []string{"Stopped"}
		for i, m := range stopped {
			if i > 0 {
				lines = append(lines, "")
			}
			row := classifyTask(m)
			lines = append(lines, titleLine(p.Red(stoppedMarker), m), detailLine(m, row), fixLine(row))
		}
		sections = append(sections, lines)
	}
	if len(needsYou) > 0 {
		// Newest first, as the ages read; a task whose age is unknown goes last.
		sort.SliceStable(needsYou, func(i, j int) bool {
			a, b := needsYou[i].since, needsYou[j].since
			return !a.IsZero() && (b.IsZero() || a.After(b))
		})
		lines := []string{"Needs you" + p.Gray(" · answer them: coop tasks decisions -i")}
		for i, m := range needsYou {
			if i == needsYouCap {
				lines = append(lines, more(len(needsYou)-needsYouCap))
				break
			}
			var extra []span
			if state := classifyTask(m).state; state != "" {
				extra = append(extra, span{" · " + state, p.Dim})
			}
			if !m.since.IsZero() {
				age, paint := sinceOrZero(m.since), p.Dim
				if age >= 24*time.Hour {
					paint = p.Yellow
				}
				extra = append(extra, span{" · ", p.Dim}, span{shortAge(age), paint})
			}
			lines = append(lines, titleLine(p.Red("⚑"), m, extra...))
		}
		sections = append(sections, lines)
	}
	if len(upNext) > 0 {
		lines := []string{"Up next" + p.Gray(fmt.Sprintf(" · %d", len(upNext)))}
		for i, m := range upNext {
			if i == upNextCap {
				lines = append(lines, more(len(upNext)-upNextCap))
				break
			}
			lines = append(lines, titleLine(p.Cyan("○"), m))
		}
		sections = append(sections, lines)
	}
	return sections
}

// stoppedMarker marks a task in progress that nothing is working and that will not move by itself.
const stoppedMarker = "✕"

// shortElapsed is a compact duration for a detail line: "<1m", "14m", "2h 5m".
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

// shortAge is how long a listed task has waited, in its largest unit: "<1m", "40m", "5h", "3d".
func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// oneLineTitle collapses any internal whitespace (a wrapped or multi-line title) to a single line,
// so a task occupies exactly one row in the live board.
func oneLineTitle(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
