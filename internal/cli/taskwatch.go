package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AndrewDryga/coop/internal/ui"
)

// watchSource is one source feeding the board — a configured queue (labeled by its path) or an
// active fork (labeled by its name) — with that source's own task counts.
type watchSource struct {
	label  string
	counts taskCounts
}

// mergedTask is a task in the unified view: the task plus the fork that owns it (claimed or worked
// it), or "" when it lives in the local queue. Sources are deduped by task id.
type mergedTask struct {
	taskItem
	fork string
}

// stateRank orders task states by advancement, so deduping by id keeps the truest state when the
// same task shows up in several sources (a fork's live copy vs the local seed): done > in progress
// > blocked > todo.
var stateRank = map[string]int{stateTodo: 0, stateBlocked: 1, stateInProgress: 2, stateDone: 3}

// tasksWatch is the live `coop tasks watch` board: every task across the configured queue(s) AND
// any active fork, merged into one view and deduped by id — so you see the whole backlog and who's
// on what (in progress with the fork that claimed it, then todo, blocked), refreshed in place.
// Unlike the per-fork fleet board, this is task-centric. It auto-exits only when everything is
// drained; without a TTY it prints the list once (pipe-safe).
func (a *app) tasksWatch(repo string, rels []string) (int, error) {
	read := func() ([]watchSource, []mergedTask, int, int) {
		var sources []watchSource
		merged := map[string]mergedTask{}
		// add merges a source's tasks, keeping the most-advanced state per id; processed in order
		// (configured queues, then forks) so a fork's live copy wins ties over the local seed.
		add := func(label string, items []taskItem, fork string) {
			c, _ := taskTreeCounts(items)
			sources = append(sources, watchSource{label: label, counts: c})
			for _, t := range items {
				if ex, ok := merged[t.ID]; !ok || stateRank[t.State] >= stateRank[ex.State] {
					merged[t.ID] = mergedTask{taskItem: t, fork: fork}
				}
			}
		}
		for _, rel := range rels {
			if items := readTaskTree(filepath.Join(repo, rel)); len(items) > 0 {
				add(rel, items, "")
			}
		}
		names := forkNames(repo)
		running := 0
		for _, name := range names {
			pid := forkRunningPid(repo, name)
			if pid != 0 {
				running++
			}
			items := readTaskTree(filepath.Join(forkWorkspace(repo, name), tasksRoot))
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
		return tasksListAll(repo, rels)
	}
	if _, merged, _, _ := read(); len(merged) == 0 {
		ui.Note("no tasks yet — add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	}

	screen := ui.NewAltScreen(os.Stdout, func() int { return ui.TermWidth(os.Stdout) })
	sawActive, sawFork := false, false // the fleet's startup guard — see tasksWatchSettling
	tick := func(spin int) ([]string, bool) {
		sources, merged, running, nForks := read()
		c := mergedCounts(merged)
		frame := tasksWatchFrame(sources, merged, spin)
		screen.Frame(frame)
		if running > 0 || c.Doing > 0 {
			sawActive = true // a fork/loop is on it — work has started
		}
		if nForks > 0 {
			sawFork = true // a fleet is in play, so an idle tick may be its startup window
		}
		// tasksWatchSettling holds the auto-exit a few ticks against a torn read and adds the startup
		// guard so a just-launched fleet doesn't conclude "drained" before it claims.
		return frame, tasksWatchSettling(c, running, sawActive, sawFork)
	}
	return runWatchLoop(screen, tick, func() {
		ui.OK("queue drained — every task is done")
	})
}

// mergedCounts tallies the deduped task set — each task counted once, by its winning state.
func mergedCounts(merged []mergedTask) taskCounts {
	items := make([]taskItem, len(merged))
	for i, m := range merged {
		items[i] = m.taskItem
	}
	c, _ := taskTreeCounts(items)
	return c
}

// tasksDrained reports whether the queue has no work left — nothing todo, in progress, or blocked,
// so every task is done (or there are none). It's the auto-exit condition for `coop tasks watch`:
// a blocked or unfinished-but-idle queue is NOT drained, so the watch keeps running.
func tasksDrained(c taskCounts) bool {
	return c.Todo == 0 && c.Doing == 0 && c.Blocked == 0
}

// tasksWatchSettling reports whether this tick counts toward auto-exit: the queue is drained AND no
// fork is running, AND — mirroring the fleet board's sawRunning guard — either work has already been
// seen (sawActive) or no fork ever appeared (a plain local watch, nothing to wait for). The guard
// stops a just-launched fleet, whose boxes are still spawning and whose queue reads idle for a tick,
// from concluding "drained" and exiting in its startup window (watchIdleExit is only ~1s of ticks).
func tasksWatchSettling(c taskCounts, running int, sawActive, sawFork bool) bool {
	return tasksDrained(c) && running == 0 && (sawActive || !sawFork)
}

// tasksWatchFrame renders the unified board. A single source leads with just the progress bar (no
// label); several sources — configured queues and/or active forks — each get a labeled progress
// line, so they're tellable apart. Below, the deduped tasks group by state — in progress (with the
// fork that claimed it), todo, blocked; done is the header count. Pure, so it unit-tests headless.
func tasksWatchFrame(sources []watchSource, merged []mergedTask, spin int) []string {
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
	return append(out, mergedQueue(p, merged, spin)...)
}

// tasksProgressLine is the overall header: the merged progress bar and the per-state counts (each in
// the state's color). No status glyph — the bar and counts already convey state.
func tasksProgressLine(p ui.Palette, c taskCounts) string {
	return fmt.Sprintf("%s  %s", ui.ProgressBar(fracDone(c), 22), tasksCountSummary(p, c))
}

// sourceLine is one source's compact breakdown — its label (queue path or fork name), a small bar,
// and done/total — so several queues/forks each fit on one line under the overall header.
func sourceLine(p ui.Palette, label string, w int, c taskCounts) string {
	return fmt.Sprintf("  %s  %s  %s/%d", p.Bold(padRight(label, w)), ui.ProgressBar(fracDone(c), 14), p.Green(fmt.Sprintf("%d", c.Done)), c.total())
}

func fracDone(c taskCounts) float64 {
	if c.total() == 0 {
		return 0
	}
	return float64(c.Done) / float64(c.total())
}

// tasksCountSummary is the per-state breakdown shown after the bar — todo · in_progress · blocked ·
// done — each painted by the shared state key (cyan / yellow / red / green), so a glance maps color
// to state. Every state shows, even at zero, so the colors read as a consistent legend.
func tasksCountSummary(p ui.Palette, c taskCounts) string {
	cells := []struct {
		state string
		n     int
	}{
		{stateTodo, c.Todo},
		{stateInProgress, c.Doing},
		{stateBlocked, c.Blocked},
		{stateDone, c.Done},
	}
	out := make([]string, len(cells))
	for i, cell := range cells {
		out[i] = paintState(p, cell.state, fmt.Sprintf("%d %s", cell.n, stateLabel(cell.state)))
	}
	return strings.Join(out, p.Dim(" · "))
}

// mergedQueue renders the deduped tasks as ONE queue-ordered list — in_progress (being worked), then
// todo (up next), then blocked (parked) — with no per-state group headers: each row's icon+color
// (taskWatchMarker) carries its state, matching the top counter legend. Active work (in_progress and
// blocked) is never elided; only the cold todo backlog tail is capped so the board stays glanceable.
// An in-progress task claimed by a fork is tagged (← name). Done tasks are omitted (header count).
func mergedQueue(p ui.Palette, merged []mergedTask, spin int) []string {
	byState := map[string][]mergedTask{}
	for _, m := range merged {
		byState[m.State] = append(byState[m.State], m)
	}
	const todoCap = 8 // cap only the cold todo backlog; in_progress + blocked always show in full
	var out []string
	emit := func(m mergedTask) {
		line := "  " + taskWatchMarker(p, m.State, spin) + " " + truncate(oneLineTitle(m.Title), 58)
		if m.fork != "" && m.State == stateInProgress {
			line += p.Dim("  ← " + m.fork)
		}
		out = append(out, line)
	}
	for _, m := range byState[stateInProgress] { // being worked — never elided
		emit(m)
	}
	todo := byState[stateTodo]
	for i, m := range todo {
		if i >= todoCap {
			out = append(out, p.Dim(fmt.Sprintf("  … +%d more", len(todo)-todoCap)))
			break
		}
		emit(m)
	}
	for _, m := range byState[stateBlocked] { // parked on a decision — never elided
		emit(m)
	}
	return out
}

// taskWatchMarker is the per-task icon, colored to match the top counter legend (paintState): a
// yellow spinner for in-progress (being worked), a red flag for blocked, a cyan hollow dot for todo.
func taskWatchMarker(p ui.Palette, state string, spin int) string {
	switch state {
	case stateInProgress:
		return p.Yellow(ui.SpinFrames[spin%len(ui.SpinFrames)])
	case stateBlocked:
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
