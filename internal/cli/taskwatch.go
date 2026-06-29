package cli

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/ui"
)

// watchQueue is one configured task queue's live state for the board.
type watchQueue struct {
	rel    string // display path, e.g. ".agent/tasks" or ".agent/tasks.api"
	items  []taskItem
	counts taskCounts
}

// tasksWatch is the live `coop tasks watch` board: the task queue(s) themselves — in progress,
// todo, and blocked — refreshed in place, so you can watch agents drain the backlog. Unlike the
// per-fork fleet board (`coop fleet watch`), this is task-centric. It auto-exits ONLY when the
// queue is fully drained — every task done — never on a partially-blocked or merely idle queue,
// which it keeps watching until Ctrl-C. Without a TTY it prints the list once (pipe-safe).
func (a *app) tasksWatch(repo string, rels []string) (int, error) {
	if !ui.IsTerminal(os.Stdout) || !ui.IsTerminal(os.Stderr) {
		// Not a terminal: one-shot list, pipe-safe — exactly what `coop tasks ls` prints.
		if len(rels) == 1 {
			return tasksFolderList(filepath.Join(repo, rels[0]))
		}
		return tasksListAll(repo, rels)
	}
	read := func() []watchQueue {
		qs := make([]watchQueue, len(rels))
		for i, rel := range rels {
			items := readTaskTree(filepath.Join(repo, rel))
			c, _ := taskTreeCounts(items)
			qs[i] = watchQueue{rel: rel, items: items, counts: c}
		}
		return qs
	}
	if sumCounts(read()).total() == 0 {
		ui.Note("no tasks yet — add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	}

	screen := ui.NewAltScreen(os.Stdout, func() int { return ui.TermWidth(os.Stdout) })
	screen.Enter()
	// finalFrame prints AFTER screen.Leave (defers run LIFO — registered first, runs last) so the
	// closing summary lands on the normal screen. A Ctrl-C exit leaves it nil and prints nothing.
	var finalFrame []string
	defer func() {
		if finalFrame == nil {
			return
		}
		for _, l := range finalFrame {
			fmt.Println(l)
		}
		ui.OK("queue drained — every task is done")
	}()
	defer screen.Leave()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	t := time.NewTicker(fleetPoll)
	defer t.Stop()

	settled := 0
	for spin := 0; ; spin++ {
		qs := read()
		screen.Frame(tasksWatchFrame(qs, spin))
		// Auto-exit only when the queue is fully drained — nothing todo, in progress, or blocked,
		// so every task is done. A blocked or unfinished-but-idle queue keeps watching (the work
		// isn't done); held a few ticks so a torn read of a task move can't end it early.
		if tasksDrained(sumCounts(qs)) {
			settled++
		} else {
			settled = 0
		}
		if settled >= fleetIdleExit {
			finalFrame = tasksWatchFrame(qs, spin)
			return 0, nil
		}
		select {
		case <-sig:
			return 0, nil
		case <-t.C:
		}
	}
}

// sumCounts totals the per-queue counts (a monorepo can configure several; the common case is one).
func sumCounts(qs []watchQueue) taskCounts {
	var c taskCounts
	for _, q := range qs {
		c.Todo += q.counts.Todo
		c.Doing += q.counts.Doing
		c.Done += q.counts.Done
		c.Blocked += q.counts.Blocked
	}
	return c
}

// tasksDrained reports whether the queue has no work left — nothing todo, in progress, or blocked,
// so every task is done (or there are none). It's the auto-exit condition for `coop tasks watch`:
// a blocked or unfinished-but-idle queue is NOT drained, so the watch keeps running.
func tasksDrained(c taskCounts) bool {
	return c.Todo == 0 && c.Doing == 0 && c.Blocked == 0
}

// tasksWatchFrame renders the live task board. A single queue leads with just the progress bar (no
// label — you already typed `coop tasks watch`); several queues each get a progress line labeled
// with their path, so you can tell them apart. Below, the actionable tasks group by state — in
// progress (spinner), todo, blocked; done is the header count, not a list (it would only grow).
// Pure (takes the gathered queues), so it unit-tests without a terminal.
func tasksWatchFrame(qs []watchQueue, spin int) []string {
	p := ui.For(os.Stdout)
	if len(qs) == 1 {
		out := []string{tasksProgressLine(p, "", qs[0].counts, spin), ""}
		return append(out, taskSections(p, qs[0].items, spin)...)
	}
	w := 0
	for _, q := range qs {
		if len(q.rel) > w {
			w = len(q.rel)
		}
	}
	var out []string
	var all []taskItem
	for _, q := range qs {
		out = append(out, tasksProgressLine(p, padRight(q.rel, w), q.counts, spin))
		all = append(all, q.items...)
	}
	out = append(out, "")
	return append(out, taskSections(p, all, spin)...)
}

// tasksProgressLine is one queue's header: a state glyph, an optional path label (only when several
// queues need telling apart), the progress bar, and the done/total count.
func tasksProgressLine(p ui.Palette, label string, c taskCounts, spin int) string {
	frac := 0.0
	if c.total() > 0 {
		frac = float64(c.Done) / float64(c.total())
	}
	left := taskWatchGlyph(p, c, spin)
	if label != "" {
		left += " " + p.Bold(label)
	}
	return fmt.Sprintf("%s  %s  %s/%d done", left, ui.ProgressBar(frac, 22), p.Green(fmt.Sprintf("%d", c.Done)), c.total())
}

// taskSections renders the actionable tasks grouped by state — in progress, todo, blocked — each
// capped so a big backlog stays glanceable. Done tasks are omitted (they're the header count).
func taskSections(p ui.Palette, items []taskItem, spin int) []string {
	byState := map[string][]taskItem{}
	for _, t := range items {
		byState[t.State] = append(byState[t.State], t)
	}
	const perState = 8
	var out []string
	for _, state := range []string{stateInProgress, stateTodo, stateBlocked} {
		ts := byState[state]
		if len(ts) == 0 {
			continue
		}
		out = append(out, p.Bold(paintState(p, state, stateLabel(state)))+p.Dim(fmt.Sprintf(" (%d)", len(ts))))
		for i, t := range ts {
			if i >= perState {
				out = append(out, p.Dim(fmt.Sprintf("    … +%d more", len(ts)-perState)))
				break
			}
			out = append(out, "  "+taskWatchMarker(p, state, spin)+" "+truncate(oneLineTitle(t.Title), 66))
		}
		out = append(out, "")
	}
	return out
}

// taskWatchGlyph leads a queue's line: a spinner while anything's in progress, a green ✓ when every
// task is done, else the idle pause mark.
func taskWatchGlyph(p ui.Palette, c taskCounts, spin int) string {
	switch {
	case c.Doing > 0:
		return p.Cyan(ui.SpinFrames[spin%len(ui.SpinFrames)])
	case c.total() > 0 && c.Done == c.total():
		return p.Green("✓")
	default:
		return "‖"
	}
}

// taskWatchMarker is the per-task bullet: a spinner for in-progress (it's being worked), a red flag
// for blocked, a faint dot for todo.
func taskWatchMarker(p ui.Palette, state string, spin int) string {
	switch state {
	case stateInProgress:
		return p.Cyan(ui.SpinFrames[spin%len(ui.SpinFrames)])
	case stateBlocked:
		return p.Red("⚑")
	default:
		return p.Dim("·")
	}
}

// oneLineTitle collapses any internal whitespace (a wrapped or multi-line title) to a single line,
// so a task occupies exactly one row in the live board.
func oneLineTitle(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
