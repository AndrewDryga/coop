package tasks

import (
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
	fork  string
	lease TaskLeaseObservation
}

// stateRank orders task states by advancement, so deduping by id keeps the truest state when the
// same task shows up in several sources (a fork's live copy vs the local seed): done > in progress
// > blocked > todo.
var stateRank = map[string]int{StateTodo: 0, StateBlocked: 1, StateInProgress: 2, StateDone: 3}

// TasksWatch is the live `coop tasks watch` board: every task across the configured queue(s) AND
// any active fork, merged into one view and deduped by id — so you see the whole backlog and who's
// on what (in progress with the fork that claimed it, then todo, blocked), refreshed in place.
// It auto-exits only when everything is drained; without a TTY it prints the list once
// (pipe-safe).
func TasksWatch(host Host, repo string, rels []string) (int, error) {
	read := func() ([]watchSource, []mergedTask, int, int) {
		var sources []watchSource
		merged := map[string]mergedTask{}
		// add merges a source's tasks, keeping the most-advanced state per id; processed in order
		// (configured queues, then forks) so a fork's live copy wins ties over the local seed.
		add := func(label string, items []Item, fork string) {
			c, _ := TaskTreeCounts(items)
			sources = append(sources, watchSource{label: label, counts: c})
			for _, t := range items {
				if ex, ok := merged[t.ID]; !ok || stateRank[t.State] >= stateRank[ex.State] {
					m := mergedTask{Item: t, fork: fork}
					if t.State == StateInProgress {
						m.lease = observeTaskLease(t, time.Now())
					}
					merged[t.ID] = m
				}
			}
		}
		for _, rel := range rels {
			if items := ReadTaskTree(filepath.Join(repo, rel)); len(items) > 0 {
				add(rel, items, "")
			}
		}
		names := forkspace.Names(repo)
		running := 0
		for _, name := range names {
			pid := forkspace.RunningPid(repo, name)
			if pid != 0 {
				running++
			}
			items := ReadTaskTree(filepath.Join(forkspace.Workspace(repo, name), TasksRoot))
			if len(items) == 0 && pid == 0 {
				continue // a dead, empty fork isn't part of the picture
			}
			add(name, items, name)
		}
		out := make([]mergedTask, 0, len(merged))
		for _, m := range merged {
			out = append(out, m)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return sources, out, running, len(names)
	}

	if !ui.IsTerminal(os.Stdout) || !ui.IsTerminal(os.Stderr) {
		// Not a terminal: one-shot list, pipe-safe — exactly what `coop tasks ls` prints.
		if len(rels) == 1 {
			return tasksFolderList(filepath.Join(repo, rels[0]), false)
		}
		return tasksListAll(repo, rels, nil)
	}
	if _, merged, _, _ := read(); len(merged) == 0 {
		ui.Note("no tasks yet — add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	}

	width := func() int { return ui.TermWidth(os.Stdout) }
	screen := ui.NewAltScreen(os.Stdout, width)
	sawActive, sawFork := false, false // concurrent-fork startup guard — see tasksWatchSettling
	tick := func(spin int) ([]string, bool) {
		sources, merged, running, nForks := read()
		c := mergedCounts(merged)
		frame := tasksWatchFrame(sources, merged, spin, width())
		screen.Frame(frame)
		if running > 0 || c.Doing > 0 {
			sawActive = true // a fork/loop is on it — work has started
		}
		if nForks > 0 {
			sawFork = true // a fork exists, so an idle tick may be its startup window
		}
		// tasksWatchSettling holds the auto-exit a few ticks against a torn read and adds the startup
		// guard so just-launched forks don't conclude "drained" before one claims.
		return frame, tasksWatchSettling(c, running, sawActive, sawFork)
	}
	return host.runWatchLoop(screen, tick, func() {
		ui.OK("queue drained — every task is done")
	})
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
		if m.fork != "" && m.State == StateInProgress {
			suffix += "  ← " + m.fork
		}
		if m.State == StateInProgress {
			suffix += " · " + m.lease.label()
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
