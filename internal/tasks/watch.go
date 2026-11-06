package tasks

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	// What the loop that let go of the task said (ProjectTaskLoop): a person stopped it, or it waits
	// out a usage limit until limitUntil. startingServices: the task's box waits to start its services.
	stoppedByYou     bool
	limitUntil       time.Time
	startingServices bool
	// doneToday: a done task finished today. The board's progress is today's, not the archive's.
	doneToday bool
	// answered: a blocked task's decision carries its answer; it waits only to be resumed.
	answered bool
	// folded: the task is in its fork's merge candidate, so the fork's one merge row stands for it.
	folded bool
	// forkBlocked: another task of the same fork waits on a decision, which is where its loop stopped.
	forkBlocked bool
}

// forkMerge is a fork's finished work as one row: ready to merge (Needs you) or merging (Now).
type forkMerge struct {
	fork     string
	dir      string // the fork's folder, which the row's title links to
	queue    string // the queues its tasks came from, on a board of several queues
	tasks    int
	landing  bool
	since    time.Time // when the candidate was published, or when its merge started
	ins, del int
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
		project := filepath.Base(snapshot.Repository)
		liveForks := liveForkGenerations(snapshot.Forks)
		forkSystems := map[forkspace.Identity]string{}
		merging := map[forkspace.Identity]*forkMerge{}
		var merges []*forkMerge
		for _, fork := range snapshot.Forks {
			if fork.Identity != nil && fork.DetachedSystem != "" {
				forkSystems[*fork.Identity] = fork.DetachedSystem
			}
			if fork.Identity != nil && (fork.Candidate || fork.PendingLand) {
				ws := forkspace.Workspace(repo, fork.Name)
				merge := &forkMerge{fork: fork.Name, dir: ws, tasks: fork.candidateTasks, landing: fork.PendingLand, since: fork.candidateAt}
				if merge.landing {
					merge.since = fork.landingAt
				}
				merge.ins, merge.del = forkspace.WorkspaceChanges(ws)
				merging[*fork.Identity] = merge
				merges = append(merges, merge)
			}
		}
		blockedForks := map[forkspace.Identity]bool{}
		for _, task := range snapshot.Tasks {
			if task.Fork != nil && task.Phase == ForkAssignmentBlocked {
				blockedForks[*task.Fork] = true
			}
		}
		folded := map[*forkMerge]int{}
		mergeQueues := map[*forkMerge][]string{}
		forkTargets := readForkTargets(snapshot)
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
				if !m.live {
					// The claim says when it was made, not when its session ended: the task's own
					// files are the last sign anyone worked on it.
					m.since = lastTouched(task.Item)
				}
			}
			if showQueues {
				m.queue = queueName(task.QueueLabel, project)
			}
			if task.Loop != nil {
				m.stoppedByYou, m.limitUntil = !task.Loop.StoppedAt.IsZero(), task.Loop.UsageLimitUntil
			}
			m.startingServices = executionsWaitingForServices(task.Executions)
			if task.Fork != nil {
				m.fork = task.Fork.Name
				m.live = liveForks[*task.Fork] || executionsRunning(task.Executions)
				m.system = forkSystems[*task.Fork]
				m.agent = forkTargets.of(repo, *task.Fork)
				m.forkBlocked = blockedForks[*task.Fork] && m.phase != ForkAssignmentBlocked
				if task.ownerRecord != nil && task.ownerRecord.Fork != nil {
					m.since = task.ownerRecord.Fork.UpdatedAt
					if projection := task.ownerRecord.Fork.Projection; projection != "" && m.live {
						m.lease = observeLeaseAt(projection, task.Item.ID, now)
					}
				}
				if merge := merging[*task.Fork]; merge != nil && (m.phase == ForkAssignmentReady || m.phase == ForkAssignmentReviewing) {
					m.folded = true
					folded[merge]++
					if m.queue != "" && !slices.Contains(mergeQueues[merge], m.queue) {
						mergeQueues[merge] = append(mergeQueues[merge], m.queue)
					}
				}
				if task.Item.State == StateInProgress && classifyTask(m).section == sectionStopped {
					m.stopReason = forkStopReason(repo, *task.Fork, m.stoppedByYou)
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
				m.answered = answeredDecision(task.Item)
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
			sources = append(sources, watchSource{label: queueName(queue.Label, project), counts: counts})
		}
		board := boardRead{snapshot: snapshot, sources: sources, merged: merged}
		for _, merge := range merges {
			if merge.tasks == 0 {
				merge.tasks = folded[merge]
			}
			sort.Strings(mergeQueues[merge])
			merge.queue = strings.Join(mergeQueues[merge], ", ")
			board.merges = append(board.merges, *merge)
		}
		sort.Slice(merged, func(i, j int) bool {
			if merged[i].ID != merged[j].ID {
				return merged[i].ID < merged[j].ID
			}
			return merged[i].Dir < merged[j].Dir
		})
		board.running = snapshot.ActiveExecutions()
		for _, fork := range snapshot.Forks {
			if fork.DetachedRunning {
				board.running++
			}
		}
		return board
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
		for _, line := range tasksWatchFrame(board, 0, 120, 0) {
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
		// The Ctrl-C footer belongs to the LIVE view only: a pipe or --json has no keyboard, and a
		// captured snapshot must not carry an instruction nobody can follow.
		frame := liveFrame(board, spin, width(), ui.TermHeight(os.Stdout))
		screen.Frame(frame)
		if queueErr = board.snapshot.QueueError(); queueErr != nil {
			return frame, true // settle on the failure so the loop's debounce still bounds the exit
		}
		return frame, tasksWatchSettling(mergedCounts(board.merged), board.running)
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
// nothing holding it. A worker that crashed leaves its lease record behind with its last heartbeat,
// and one that let go leaves the record of its release; the task's own files may say someone
// worked on it later.
func goneWorker(item Item) (string, time.Time) {
	last := lastTouched(item)
	if meta, ok := lastLeaseHolder(filepath.Dir(filepath.Dir(item.Dir)), item.ID); ok && !meta.HeartbeatAt.IsZero() {
		if meta.HeartbeatAt.After(last) {
			last = meta.HeartbeatAt
		}
		return knownTarget(meta.Target, meta.Provider), last
	}
	return "", last
}

// lastTouched is when anyone last worked on a task by its own files: its folder or the files a
// worker writes, whichever changed last.
func lastTouched(item Item) time.Time {
	last := folderChanged(item.Dir)
	for _, name := range []string{"task.md", "state.md", "log.md"} {
		if info, err := os.Lstat(filepath.Join(item.Dir, name)); err == nil && info.ModTime().After(last) {
			last = info.ModTime()
		}
	}
	return last
}

// knownAgent is an agent name safe to print, or "" when there is none to name.
func knownAgent(name string) string {
	if agent := leaseProvider(name); agent != "unknown" {
		return agent
	}
	return ""
}

// knownTarget is the full target a worker recorded, in coop's own grammar
// (provider:model/effort@account), when it reads as one; else its agent name, or "".
func knownTarget(target, provider string) string {
	t := sanitizeCell(strings.TrimSpace(target))
	ok := t != "" && len(t) <= 80
	for _, r := range t {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("-_@.:/,", r) {
			ok = false
		}
	}
	if ok {
		return t
	}
	return knownAgent(provider)
}

// forkTargetSet is the one target the board names each fork generation by, so every task of a fork
// reads the same: the target its loop most recently recorded in a lease on its own copy of a task
// (the assignment names that copy).
type forkTargetSet map[forkspace.Identity]forkTarget

type forkTarget struct {
	target string
	at     time.Time
}

func readForkTargets(snapshot ProjectSnapshot) forkTargetSet {
	set := forkTargetSet{}
	for _, task := range snapshot.Tasks {
		if task.Fork == nil || task.ownerRecord == nil || task.ownerRecord.Fork == nil || task.ownerRecord.Fork.Projection == "" {
			continue
		}
		meta, ok := lastLeaseHolder(task.ownerRecord.Fork.Projection, task.Item.ID)
		if !ok {
			continue
		}
		if target := knownTarget(meta.Target, meta.Provider); target != "" && !meta.HeartbeatAt.Before(set[*task.Fork].at) {
			set[*task.Fork] = forkTarget{target: target, at: meta.HeartbeatAt}
		}
	}
	return set
}

// of is the fork's target: from its loop's latest lease, else the target its loop noted when it
// last took a task (forkctl.SaveForkTarget), else the agent coop fork noted (forkctl.SaveForkAgent).
func (s forkTargetSet) of(repo string, fork forkspace.Identity) string {
	if t := s[fork].target; t != "" {
		return t
	}
	if t := knownTarget(forkNote(repo, fork.Name, "target"), ""); t != "" {
		return t
	}
	return knownAgent(forkNote(repo, fork.Name, "agent"))
}

// queueName is how the board names a queue: the folder that holds it, so `portal/.agent/tasks`
// reads `portal` and `portal/.agent/tasks-b` reads `portal/tasks-b`, and the project's own queue
// reads as the project's folder name, never "root". Any other label is kept as it is.
func queueName(label, project string) string {
	dir, name := filepath.Split(filepath.Clean(label))
	dir = strings.TrimSuffix(dir, "/")
	switch {
	case label == "":
		return ""
	case dir == ".agent" && name == "tasks":
		return project
	case dir == ".agent":
		return name
	case strings.HasSuffix(dir, "/.agent") && name == "tasks":
		return strings.TrimSuffix(dir, "/.agent")
	case strings.HasSuffix(dir, "/.agent"):
		return strings.TrimSuffix(dir, ".agent") + name
	}
	return label
}

// forkNote is one of the notes coop fork keeps in a fork's workspace (its agent, its target). The
// workspace is agent-writable, so a note is read through the no-follow metadata root, and its
// caller prints it only when it reads as a name or a target.
func forkNote(repo, name, file string) string {
	root, err := OpenTaskMetadataRoot(filepath.Join(forkspace.Workspace(repo, name), ".coop"))
	if err != nil {
		return ""
	}
	defer root.Close()
	data, err := ReadTaskMetadataFile(root, file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// forkStopReason is why a fork's loop ended: the failure the loop recorded for this generation
// (forkctl.NoteForkLoopExit), else, unless a person stopped it, the last error line of its log (what
// coop fork logs shows; a stop tears an attempt down, and its error says nothing), else "".
func forkStopReason(repo string, identity forkspace.Identity, stoppedByYou bool) string {
	if root, err := OpenTaskMetadataRoot(filepath.Join(forkspace.Workspace(repo, identity.Name), ".coop")); err == nil {
		data, readErr := ReadTaskMetadataFile(root, "loop-error")
		root.Close()
		if generation, reason, ok := strings.Cut(strings.TrimSpace(string(data)), "\n"); readErr == nil && ok && generation == string(identity.Generation) {
			if reason = oneLineTitle(sanitizeCell(reason)); reason != "" {
				return reason
			}
		}
	}
	if stoppedByYou {
		return ""
	}
	return lastLoggedError(forkspace.LogPath(repo, identity.Name))
}

// forkLogTail bounds how much of a fork's log the board reads to find its last error.
const forkLogTail = 16 << 10

var ansiSequence = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// decisionStop is how a loop that stopped for a person's decision says so (loop.printFinalVerdict).
const decisionStop = "Stopped for your decision"

// lastLoggedError is the last error the loop itself printed (ui.Error: "✗ " at the start of the
// line) in the tail of a fork's log, or "". The agent's own failed commands show indented and quoted
// ("  ✗ \"make check\" (exit 2): …") and say nothing about why the loop ended, and a loop whose last
// word is that it stopped for a decision did not fail.
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
		line := strings.TrimRight(ansiSequence.ReplaceAllString(lines[i], ""), " \t\r")
		if strings.HasPrefix(line, decisionStop) {
			return ""
		}
		if reason, ok := strings.CutPrefix(line, "✗ "); ok && !strings.HasPrefix(reason, `"`) && !strings.HasPrefix(reason, "'") {
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

func executionsWaitingForServices(executions []forkspace.ExecutionObservation) bool {
	for _, e := range executions {
		if e.WaitingForServices {
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
	merges   []forkMerge
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
	stamp := func(path string) { stampPath(&b, path) }
	queues := append([]string(nil), roots...)
	for _, queue := range board.snapshot.Queues {
		queues = append(queues, queue.Root)
	}
	b.WriteString(QueueStamps(queues))
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

// tasksWatchFrame renders the board: the progress header (and one line per queue when there are
// several), then Now, Stopped, Needs you and Up next, each left out when empty, then the forks with
// no row of their own and the problems the snapshot found. The board is about TASKS: a box or
// session running without a task of its own is execution data the JSON snapshot still carries (and
// activity accounting and auto-exit still read), not a second inventory on screen. height is how
// many rows the board may use, and it never uses more (see sectionShown). Without one (0: a pipe, or
// a terminal that cannot say) the lists show fixed caps. Pure, so it unit-tests headless.
func tasksWatchFrame(board boardRead, spin, width, height int) []string {
	p := ui.For(os.Stdout)
	out := []string{tasksProgressLine(p, countBoard(board.merged, board.merges), width)}
	// With several queues, break them down compactly, so a glance shows which is how far along.
	if len(board.sources) > 1 {
		w := 0
		for _, s := range board.sources {
			w = max(w, len([]rune(s.label)))
		}
		for _, s := range board.sources {
			out = append(out, sourceLine(p, s.label, w, s.counts))
		}
	}
	tail := boardTail(board, width)
	sections := boardSections(p, board.merged, board.merges, spin, width)
	shown := sectionShown(sections, height-len(out)-len(tail))
	for i, section := range sections {
		out = append(out, "")
		out = append(out, section.render(shown[i])...)
	}
	out = append(out, tail...)
	if height > 0 && len(out) > height {
		// Even the fewest rows do not fit: cut the bottom rather than scroll the header away.
		out = out[:height]
	}
	return out
}

// liveFrame is the live board for a terminal of termHeight rows: the board, then a blank line and
// the Ctrl-C footer, never taller than the terminal.
func liveFrame(board boardRead, spin, width, termHeight int) []string {
	height := 0
	if termHeight > 0 {
		height = max(termHeight-2, 1)
	}
	return append(tasksWatchFrame(board, spin, width, height), "", watchExitFooter)
}

// boardTail is what follows the sections: a fork with no row on the board (cleaning up, or between
// tasks) gets a line of its own, and every problem the snapshot found is listed.
func boardTail(board boardRead, width int) []string {
	onBoard := map[string]bool{}
	for _, m := range board.merged {
		if m.fork != "" && m.State != StateTodo && m.State != StateDone {
			onBoard[m.fork] = true
		}
	}
	var visibleForks []ProjectForkSnapshot
	for _, fork := range board.snapshot.Forks {
		// A fork ready to merge or merging is its merge row.
		if len(fork.Executions) == 0 && forkHasVisibleActivity(fork) && !onBoard[fork.Name] && !fork.Candidate && !fork.PendingLand {
			visibleForks = append(visibleForks, fork)
		}
	}
	var out []string
	if len(visibleForks) > 0 {
		out = append(out, "", "forks")
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
		case fork.CandidatePhase == forkCandidateSuperseding:
			state = fmt.Sprintf("review round %d pending", fork.CandidateRound)
		case fork.CandidatePhase == forkCandidateReviewing:
			state = fmt.Sprintf("reviewing round %d", fork.CandidateRound)
		case fork.CandidatePhase == forkCandidatePublishing:
			state = fmt.Sprintf("publishing round %d", fork.CandidateRound)
		case fork.Assignments > 0:
			state = ui.Count(fork.Assignments, "assigned task")
		}
		out = append(out, "  "+fork.Name+" · "+state)
	}
	if len(board.snapshot.Problems) > 0 {
		out = append(out, "", "problems")
		for _, problem := range board.snapshot.Problems {
			out = append(out, "  "+truncate(oneLineTitle(problem), width-3))
		}
	}
	return out
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

// boardCounts is what the header reports: the state counts, how many rows are under Now and under
// Stopped, and how many tasks finished today.
type boardCounts struct {
	TaskCounts
	Working   int
	Stopped   int
	DoneToday int
}

func countBoard(merged []mergedTask, merges []forkMerge) boardCounts {
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
	for _, merge := range merges {
		if merge.landing { // a merge running is a row under Now
			b.Working++
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
	sectionHidden   boardSection = iota // done, or in its fork's merge row: a header count, never a row
	sectionNow                          // something is on it
	sectionStopped                      // in progress, nothing on it, and it will not move by itself
	sectionNeedsYou                     // waits on a person's decision
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

// taskRow is what the board says about one task: the state that ends its detail line, and for a
// Stopped row the commands that move it on.
type taskRow struct {
	section boardSection
	state   string
	failure string // the error a stopped fork loop reported, shown under its detail line
	tone    rowTone
	paused  bool     // a Stopped row a person stopped on purpose: a dim marker, nothing failed
	waits   bool     // a Needs you row waiting on another task's decision: it has no age of its own
	fixVerb string   // what the commands do: "resume", "fix" or "free"
	fix     []string // the complete commands, in order
	// others is what the stopped rows sharing this fix have in common, in plain words, for the line
	// that stands for those that do not fit: "+ 8 more with no worker".
	others string
}

// classifyTask places a task on the board and says what its detail and fix lines read. Every
// reason it gives comes from what coop already records: the lease and its heartbeat, the claim and
// its process, the fork's phase, its loop and the failure the loop recorded or logged.
func classifyTask(m mergedTask) taskRow {
	switch m.State {
	case StateTodo:
		return taskRow{section: sectionUpNext}
	case StateBlocked:
		if m.answered {
			return taskRow{section: sectionNeedsYou, state: "answered, not yet resumed"}
		}
		return taskRow{section: sectionNeedsYou, state: "decision pending"}
	case StateInProgress:
	default:
		return taskRow{section: sectionHidden}
	}
	if m.fork != "" && m.phase != "" {
		return classifyForkTask(m)
	}
	switch {
	case m.startingServices:
		return startingServicesRow
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
		// Nothing records when the session ended; the task's files say how long nobody touched it.
		state := "claim held by a session that has ended"
		if !m.since.IsZero() {
			state += " · idle for " + shortElapsed(sinceOrZero(m.since))
		}
		return taskRow{section: sectionStopped, state: state, tone: toneStopped,
			fixVerb: "free", fix: []string{"coop tasks release " + m.ID}}
	case m.limitUntil.After(time.Now()):
		return usageLimitRow(m.limitUntil)
	case m.stoppedByYou:
		return stoppedByYouRow("coop loop")
	}
	state := "worker gone"
	if !m.since.IsZero() {
		state += " " + shortElapsed(sinceOrZero(m.since)) + " ago"
	}
	return taskRow{section: sectionStopped, state: state, tone: toneStopped, fixVerb: "resume", fix: []string{"coop loop"}, others: "with no worker"}
}

// startingServicesRow is a task whose box waits for the project's service launch lock: other boxes
// are starting services or hold the lock, and it goes on by itself once they let go.
var startingServicesRow = taskRow{section: sectionNow, state: "waiting to start its services", tone: toneWaiting}

// usageLimitRow is a task whose loop waits out a usage limit until until, then goes on by itself.
func usageLimitRow(until time.Time) taskRow {
	return taskRow{section: sectionNow, state: "usage limit, back in " + shortElapsed(time.Until(until)), tone: toneWaiting}
}

// stoppedByYouRow is a task a person stopped the loop on, and the command that starts it again.
func stoppedByYouRow(restart string) taskRow {
	return taskRow{section: sectionStopped, state: "stopped by you", paused: true, fixVerb: "resume", fix: []string{restart}, others: "stopped by you"}
}

// classifyForkTask places a task a fork holds. A task in the fork's merge candidate shows only as
// the fork's merge row. One that finished or is in its final review while the fork's loop is gone
// and nothing was published is stopped: the loop has to run again to finish it.
func classifyForkTask(m mergedTask) taskRow {
	switch {
	case m.folded:
		return taskRow{section: sectionHidden}
	case m.phase == ForkAssignmentBlocked:
		return taskRow{section: sectionNeedsYou, state: "decision pending"}
	case m.system != "":
		return taskRow{section: sectionNow, state: "running on " + m.system}
	case m.live:
		// Between two of its loop's iterations the task reads paused; the lease the loop holds on its
		// copy of the task says it is being worked.
		switch {
		case m.startingServices:
			return startingServicesRow
		case m.phase == ForkAssignmentReviewing:
			return taskRow{section: sectionNow, state: "final review" + forElapsed(m.since)}
		case m.lease.State == leaseBusy:
			return taskRow{section: sectionNow, state: workingFor(m.lease.Held)}
		case m.lease.State == leaseStalled:
			return taskRow{section: sectionNow, state: "no heartbeat for " + shortElapsed(m.lease.Quiet), tone: toneWaiting}
		case m.limitUntil.After(time.Now()):
			return usageLimitRow(m.limitUntil)
		case m.phase == ForkAssignmentWorking:
			return taskRow{section: sectionNow, state: workingFor(sinceOrZero(m.since))}
		}
		return taskRow{section: sectionNow, state: string(m.phase) + forElapsed(m.since)}
	case m.forkBlocked && (m.phase == ForkAssignmentReady || m.phase == ForkAssignmentReviewing):
		// The loop finished this task, then stopped for a decision on another of the fork's tasks:
		// nothing failed, and answering that decision is what moves this one on.
		return taskRow{section: sectionNeedsYou, state: "done in fork " + m.fork + ", waiting for the fork's other task", waits: true}
	case m.stoppedByYou && m.stopReason == "":
		row := stoppedByYouRow("coop fork " + m.fork + " --loop -d")
		row.others = "in fork " + m.fork
		return row
	}
	state, failure, verb, fix := forkStopNext(m.fork, m.stopReason)
	return taskRow{section: sectionStopped, state: state, failure: failure, tone: toneStopped, fixVerb: verb, fix: fix, others: "in fork " + m.fork}
}

// forkStopNext turns why a fork's loop ended into the state a person reads and the complete set of
// commands that move its task on. A known refusal names its fix and then the restart, because
// fixing the cause alone does not start the loop again, and a start refused for a stated reason
// reads as that reason. Any other failure is an error as the loop or its log put it: the state
// says the loop failed, and the error comes as written, on lines of its own.
func forkStopNext(fork, reason string) (state, failure, verb string, fix []string) {
	restart := "coop fork " + fork + " --loop -d"
	switch {
	case reason == "":
		return "loop ended unexpectedly", "", "resume", []string{restart}
	case strings.Contains(reason, "has not been approved"):
		return "network access not approved", "", "fix", []string{"coop approve", restart}
	case strings.Contains(reason, "run coop login"):
		return "sign-in no longer accepted", "", "fix", []string{"coop login", restart}
	}
	if _, clause, ok := strings.Cut(reason, " cannot start because "); ok && clause != "" {
		return clause, "", "resume", []string{restart}
	}
	return "loop failed", reason, "resume", []string{restart}
}

func workingFor(d time.Duration) string {
	if d <= 0 {
		return "working"
	}
	return "working for " + shortElapsed(d)
}

// forElapsed is " for <how long>" since t, or "" when t is unknown.
func forElapsed(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " for " + shortElapsed(sinceOrZero(t))
}

func sinceOrZero(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return max(time.Since(t), 0)
}

// listMin is how many rows Needs you and Up next show in a pipe, which has no height to fill.
const listMin = 3

// liveMin is how many task rows a section keeps on a live board before the bottom of the terminal
// cuts anything: three of Stopped and of Needs you, two of Up next. Now always shows in full.
var liveMin = map[boardSection]int{sectionStopped: 3, sectionNeedsYou: 3, sectionUpNext: 2}

// headerForm is one way to write the header's counts, from the fullest to the shortest.
type headerForm struct {
	bar                             bool
	working, stopped, blocked, done string
}

var headerForms = []headerForm{
	{true, "working", "stopped", "blocked", "done today"},
	{true, "working", "stopped", "blocked", "done"},
	{true, "work", "stop", "block", "done"},
	{false, "work", "stop", "block", "done"},
}

// tasksProgressLine is the overall header: a bar and the per-state counts. Both describe the live
// queue and today's finished work. The archive of done tasks is left out: hundreds of them filled the
// bar, so it never moved during a run. A narrow terminal gets a shorter header, never a cut one.
func tasksProgressLine(p ui.Palette, b boardCounts, width int) string {
	today := TaskCounts{Todo: b.Todo, Doing: b.Doing, Blocked: b.Blocked, Done: b.DoneToday}
	var line string
	for _, form := range headerForms {
		line = tasksCountSummary(p, b, form)
		plain := len([]rune(tasksCountSummary(ui.Palette{}, b, form)))
		if bar := progressBar(p, today, 22); form.bar && bar != "" {
			line, plain = bar+line, plain+22+4 // the bar's cells, its brackets and the gap after it
		}
		if plain <= width-1 {
			break
		}
	}
	return line
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
func tasksCountSummary(p ui.Palette, b boardCounts, form headerForm) string {
	cells := []string{
		paintState(p, StateTodo, fmt.Sprintf("%d todo", b.Todo)),
		paintState(p, StateInProgress, fmt.Sprintf("%d %s", b.Working, form.working)),
		p.Red(fmt.Sprintf("%d %s", b.Stopped, form.stopped)),
		paintState(p, StateBlocked, fmt.Sprintf("%d %s", b.Blocked, form.blocked)),
		paintState(p, StateDone, fmt.Sprintf("%d %s", b.DoneToday, form.done)),
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

// paintSpans paints spans in order, whole: for a line that must never be cut.
func paintSpans(spans []span) string {
	var b strings.Builder
	for _, s := range spans {
		if s.paint != nil {
			b.WriteString(s.paint(s.text))
		} else {
			b.WriteString(s.text)
		}
	}
	return b.String()
}

// watchSection is one of the board's sections: its kind, how many task rows it has, and how it
// reads with only the first of them shown.
type watchSection struct {
	kind   boardSection
	total  int
	render func(shown int) []string // its header, then its first shown rows and what stands for the rest
}

// height is how many lines the section takes with its first shown rows, the blank line before it
// included.
func (s watchSection) height(shown int) int { return 1 + len(s.render(shown)) }

// sectionShown is how many task rows each section shows within budget lines. Now always shows in
// full. Stopped and Needs you keep liveMin rows each and Up next its own, then Stopped and Needs you
// share the room left, a row at a time, and Up next takes what they leave. When even the minimums do
// not fit, Up next gives way first, then Needs you, then Stopped, down to a row each. Without a
// budget (a pipe) the work shows in full and each list listMin rows.
func sectionShown(sections []watchSection, budget int) []int {
	shown := make([]int, len(sections))
	for i, s := range sections {
		shown[i] = s.total
		if s.kind == sectionNeedsYou || s.kind == sectionUpNext {
			shown[i] = min(listMin, s.total)
		}
	}
	if budget <= 0 {
		return shown
	}
	used := 0
	for i, s := range sections {
		if n, ok := liveMin[s.kind]; ok {
			shown[i] = min(n, s.total)
		}
		used += s.height(shown[i])
	}
	resize := func(i, n int) {
		used += sections[i].height(n) - sections[i].height(shown[i])
		shown[i] = n
	}
	for _, kind := range []boardSection{sectionUpNext, sectionNeedsYou, sectionStopped, sectionNow} {
		for i, s := range sections {
			for s.kind == kind && used > budget && shown[i] > 1 {
				resize(i, shown[i]-1)
			}
		}
	}
	grow := func(kinds ...boardSection) {
		for grew := true; grew; {
			grew = false
			for i, s := range sections {
				if slices.Contains(kinds, s.kind) && shown[i] < s.total &&
					used-s.height(shown[i])+s.height(shown[i]+1) <= budget {
					resize(i, shown[i]+1)
					grew = true
				}
			}
		}
	}
	grow(sectionStopped, sectionNeedsYou)
	grow(sectionUpNext)
	return shown
}

// listSection is a section whose rows show in order, then how many more there are.
func listSection(p ui.Palette, kind boardSection, header string, rows [][]string) watchSection {
	return watchSection{kind: kind, total: len(rows), render: func(shown int) []string {
		out := []string{header}
		for _, row := range rows[:shown] {
			out = append(out, row...)
		}
		if more := len(rows) - shown; more > 0 {
			out = append(out, p.Dim(fmt.Sprintf("  + %d more", more)))
		}
		return out
	}}
}

// stopGroup is the stopped tasks one set of commands moves on: their rows, then the error they
// share and the commands, once.
type stopGroup struct {
	rows    [][]string
	others  string
	failure []string
	fix     func(n int) []string // the fix line, worded for n tasks
}

// fixPhrase is what a fix line says before its commands, for n tasks.
func fixPhrase(verb string, n int) string {
	switch {
	case n > 1:
		return "to " + verb + " them, run:"
	case verb == "free":
		return "to free it, run:"
	}
	return "to " + verb + ", run:"
}

// stoppedSection groups stopped tasks by the commands that move them on. Every group shows at least
// one row while there is room for one each, the first groups then fill first; a group's rows that
// do not fit read as "+ N more" and what they share. Groups are set apart by one blank line.
func stoppedSection(p ui.Palette, groups []*stopGroup) watchSection {
	total := 0
	for _, g := range groups {
		total += len(g.rows)
	}
	return watchSection{kind: sectionStopped, total: total, render: func(shown int) []string {
		take := make([]int, len(groups))
		left := shown
		for i := range groups {
			if left > 0 {
				take[i], left = 1, left-1
			}
		}
		for i, g := range groups {
			n := min(left, len(g.rows)-take[i])
			if take[i] > 0 {
				take[i], left = take[i]+n, left-n
			}
		}
		out := []string{fmt.Sprintf("Stopped (%d)", total)}
		hidden := 0
		for i, g := range groups {
			if take[i] == 0 {
				hidden += len(g.rows)
				continue
			}
			if len(out) > 1 {
				out = append(out, "")
			}
			for _, row := range g.rows[:take[i]] {
				out = append(out, row...)
			}
			if more := len(g.rows) - take[i]; more > 0 {
				out = append(out, p.Dim(strings.TrimRight(fmt.Sprintf("  + %d more %s", more, g.others), " ")))
			}
			out = append(out, g.failure...)
			out = append(out, g.fix(len(g.rows))...)
		}
		if hidden > 0 {
			out = append(out, "", p.Dim(fmt.Sprintf("  + %d more", hidden)))
		}
		return out
	}}
}

// failureLimit is how many lines a stopped loop's error may take on the board.
const failureLimit = 2

// failureLines is an error wrapped to room at word boundaries, at most failureLimit lines; a
// longer one ends its last line with an ellipsis after a whole word. A last line that would hold
// only one short word (the "1" of "Error 1") is left off too, and the line before it ends with the
// ellipsis: a lone token on a line of its own reads as a separate message.
func failureLines(text string, room int) []string {
	if text = oneLineTitle(text); text == "" {
		return nil
	}
	lines := ui.WrapLines(text, room)
	keep := min(len(lines), failureLimit)
	if last := strings.Fields(lines[keep-1]); keep == len(lines) && keep > 1 && len(last) == 1 && len([]rune(last[0])) < room/3 {
		keep--
	}
	if keep == len(lines) {
		return lines
	}
	lines = lines[:keep]
	last := strings.Fields(lines[keep-1])
	for len(last) > 1 && len([]rune(strings.Join(last, " ")))+1 > room {
		last = last[:len(last)-1]
	}
	lines[keep-1] = truncate(strings.Join(last, " "), room-1) + "…"
	return lines
}

// fileLink is the URL a title links to: a task's folder or decision, or a fork's folder.
func fileLink(path string) string {
	if path == "" {
		return ""
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// boardSections renders Now, Stopped, Needs you and Up next, leaving out an empty one. Each header
// carries its count, and Needs you the command that answers its decisions. A section's first row
// starts right under its header; Stopped tasks that one set of commands moves on form a group, the
// commands printed once after its last row, and only groups are set apart by a blank line. Every row that is
// not a plain list entry has a detail line in one order:
// `<queue> · [<done>/<total> · ][<agent>[ in fork <name>] · ]<state> for <duration>`, the queue named
// only on a board of several. Titles link to the task's folder, a blocked task's decision, or the
// merging fork's folder; commands are never cut.
func boardSections(p ui.Palette, merged []mergedTask, merges []forkMerge, spin, width int) []watchSection {
	const (
		rowIndent  = 4 // two-space indent + one-column marker + separating space
		lineIndent = "    "
	)
	// AltScreen leaves the terminal's final column empty so a full row cannot auto-wrap.
	room, lineRoom := width-1-rowIndent, width-1-len(lineIndent)
	type listed struct {
		since time.Time
		lines []string
	}
	var now [][]string
	var stopped []*stopGroup
	stopIndex := map[string]*stopGroup{}
	var needsYou []listed
	var upNext [][]string
	decisions := false

	// A title is cut only by the terminal's width, at the end of its line: what follows it goes first.
	titleLine := func(marker, title, link string, extra ...span) string {
		spans := []span{{oneLineTitle(title), func(s string) string { return p.Link(link, s) }}}
		return "  " + marker + " " + fitSpans(append(spans, extra...), room)
	}
	detailLine := func(queue string, facts []string, state ...span) string {
		if queue != "" {
			facts = append([]string{queue}, facts...)
		}
		var spans []span
		if len(facts) > 0 {
			spans = append(spans, span{strings.Join(facts, " · ") + " · ", p.Faint})
		}
		return lineIndent + fitSpans(append(spans, state...), lineRoom)
	}
	// commandLines is lead facts, then a verb and its commands. A command that does not fit on the
	// line goes on one of its own, so none is ever cut.
	commandLines := func(lead []span, verb string, commands []string) []string {
		var lines []string
		var line []span
		used := 0
		add := func(s span) {
			line = append(line, s)
			used += len([]rune(s.text))
		}
		for _, s := range lead {
			add(s)
		}
		if verb != "" {
			if used > 0 {
				add(span{" · ", p.Faint})
			}
			add(span{verb, nil})
		}
		for i, command := range commands {
			sep := " "
			if i > 0 {
				sep = ", then "
			}
			if used > 0 && used+len([]rune(sep+command)) > lineRoom {
				lines = append(lines, lineIndent+paintSpans(line))
				line, used, sep = nil, 0, ""
				if i > 0 {
					sep = "then "
				}
			}
			if sep != "" {
				add(span{sep, nil})
			}
			add(span{command, p.Bold})
		}
		if len(line) > 0 {
			lines = append(lines, lineIndent+paintSpans(line))
		}
		return lines
	}
	stateSpan := func(row taskRow) span {
		switch row.tone {
		case toneWaiting:
			return span{row.state, p.Yellow}
		case toneStopped:
			return span{row.state, p.Red}
		}
		return span{row.state, p.Faint}
	}
	// ageSpans is " for <age>" after a waiting row's state, yellow past a day.
	ageSpans := func(since time.Time) []span {
		if since.IsZero() {
			return nil
		}
		age, paint := sinceOrZero(since), p.Faint
		if age >= 24*time.Hour {
			paint = p.Yellow
		}
		return []span{{" for ", p.Faint}, {shortAge(age), paint}}
	}
	taskFacts := func(m mergedTask) []string {
		var facts []string
		if n := len(m.Subtasks); n > 0 {
			facts = append(facts, fmt.Sprintf("%d/%d", m.doneSubtasks(), n))
		}
		agent := m.agent
		if m.fork == "" && m.lease.State != leaseUnleased {
			agent = knownTarget(m.lease.Target, m.lease.Provider)
		}
		if m.fork != "" { // the one place a fork's task names its fork
			agent = strings.TrimSpace(agent + " in fork " + m.fork)
		}
		if agent != "" {
			facts = append(facts, agent)
		}
		return facts
	}
	taskLink := func(m mergedTask) string {
		if (m.State == StateBlocked || m.phase == ForkAssignmentBlocked) && m.HasDecision {
			return fileLink(filepath.Join(m.Dir, "decision.md"))
		}
		return fileLink(m.Dir)
	}

	for _, m := range merged {
		row := classifyTask(m)
		switch row.section {
		case sectionNow:
			now = append(now, []string{titleLine(p.Yellow(ui.CompactSpinFrame(spin)), m.Title, taskLink(m)),
				detailLine(m.queue, taskFacts(m), stateSpan(row))})
		case sectionStopped:
			key := strings.Join(append([]string{row.fixVerb, row.failure}, row.fix...), "\x00")
			group := stopIndex[key]
			if group == nil {
				group = &stopGroup{others: row.others, fix: func(n int) []string {
					return commandLines(nil, fixPhrase(row.fixVerb, n), row.fix)
				}}
				for _, line := range failureLines(row.failure, lineRoom) {
					group.failure = append(group.failure, lineIndent+p.Dim(p.Red(line)))
				}
				stopIndex[key] = group
				stopped = append(stopped, group)
			}
			if group.others != row.others { // the rows that do not fit share only their fix
				group.others = ""
			}
			marker := p.Red(stoppedMarker)
			if row.paused {
				marker = p.Dim(pausedMarker)
			}
			group.rows = append(group.rows, []string{titleLine(marker, m.Title, taskLink(m)), detailLine(m.queue, taskFacts(m), stateSpan(row))})
		case sectionNeedsYou:
			decisions = true
			state := []span{stateSpan(row)}
			if !row.waits {
				state = append(state, ageSpans(m.since)...)
			}
			needsYou = append(needsYou, listed{m.since, []string{titleLine(p.Red("⚑"), m.Title, taskLink(m)), detailLine(m.queue, nil, state...)}})
		case sectionUpNext:
			var extra []span
			if m.queue != "" {
				extra = append(extra, span{" · " + m.queue, p.Dim})
			}
			upNext = append(upNext, []string{titleLine(p.Cyan("○"), m.Title, taskLink(m), extra...)})
		}
	}
	for _, merge := range merges {
		title := "Merge fork " + merge.fork
		if merge.tasks > 0 {
			title += " (" + ui.Count(merge.tasks, "task") + ")"
		}
		var stat []span
		if merge.ins != 0 || merge.del != 0 {
			stat = append(stat, span{fmt.Sprintf("+%d −%d", merge.ins, merge.del), p.Faint})
		}
		if merge.landing {
			lines := []string{titleLine(p.Yellow(ui.CompactSpinFrame(spin)), title, fileLink(merge.dir)),
				detailLine(merge.queue, nil, span{"fork " + merge.fork + " merging" + forElapsed(merge.since), p.Faint})}
			now = append(now, append(lines, commandLines(stat, "", nil)...))
			continue
		}
		lines := []string{titleLine(p.Red("⚑"), title, fileLink(merge.dir)),
			detailLine(merge.queue, nil, append([]span{{"fork " + merge.fork + " ready", p.Faint}}, ageSpans(merge.since)...)...)}
		needsYou = append(needsYou, listed{merge.since, append(lines, commandLines(stat, "to merge, run:", []string{"coop fork merge " + merge.fork})...)})
	}

	var sections []watchSection
	if len(now) > 0 {
		sections = append(sections, listSection(p, sectionNow, fmt.Sprintf("Now (%d)", len(now)), now))
	}
	if len(stopped) > 0 {
		sections = append(sections, stoppedSection(p, stopped))
	}
	if len(needsYou) > 0 {
		// Newest first, as the ages read; a row whose age is unknown goes last.
		sort.SliceStable(needsYou, func(i, j int) bool {
			a, b := needsYou[i].since, needsYou[j].since
			return !a.IsZero() && (b.IsZero() || a.After(b))
		})
		header := fmt.Sprintf("Needs you (%d)", len(needsYou))
		if decisions {
			header += p.Gray(" · answer them: coop tasks decisions -i")
		}
		rows := make([][]string, len(needsYou))
		for i, row := range needsYou {
			rows[i] = row.lines
		}
		sections = append(sections, listSection(p, sectionNeedsYou, header, rows))
	}
	if len(upNext) > 0 {
		sections = append(sections, listSection(p, sectionUpNext, fmt.Sprintf("Up next (%d)", len(upNext)), upNext))
	}
	return sections
}

// stoppedMarker marks a task in progress that nothing is working and that will not move by itself;
// pausedMarker one a person stopped on purpose.
const (
	stoppedMarker = "✕"
	pausedMarker  = "‖"
)

// shortElapsed is a compact duration for a detail line: "<1m", "14m", "2h 5m".
func shortElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 10*time.Hour && int(d.Minutes())%60 != 0:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		// Past a few hours the minutes are noise ("worker gone 101h 0m ago"): say it in the
		// largest unit, as a waiting row does.
		return shortAge(d)
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
