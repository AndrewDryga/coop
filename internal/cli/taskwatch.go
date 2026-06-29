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

// tasksWatch is the live `coop tasks watch` board: the task queue(s) themselves — in progress,
// todo, and blocked — refreshed in place, so you can watch agents drain the backlog. Unlike the
// per-fork fleet board (`coop fleet watch`), this is task-centric. It auto-exits ONLY when the
// queue is fully drained — every task done — never on a partially-blocked or merely idle queue,
// which it keeps watching until Ctrl-C. Without a TTY it prints the list once (pipe-safe).
func (a *app) tasksWatch(repo string, rels []string) (int, error) {
	roots := make([]string, len(rels))
	for i, r := range rels {
		roots[i] = filepath.Join(repo, r)
	}
	if !ui.IsTerminal(os.Stdout) || !ui.IsTerminal(os.Stderr) {
		// Not a terminal: one-shot list, pipe-safe — exactly what `coop tasks ls` prints.
		if len(roots) == 1 {
			return tasksFolderList(roots[0])
		}
		return tasksListAll(repo, rels)
	}
	if c, _ := taskTreeCounts(readAllTasks(roots)); c.total() == 0 {
		ui.Note("no tasks yet — add one with 'coop tasks add \"<title>\"'")
		return 0, nil
	}

	name := filepath.Base(repo)
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
		items := readAllTasks(roots)
		c, _ := taskTreeCounts(items)
		screen.Frame(tasksWatchFrame(name, items, c, spin))
		// Auto-exit only when the queue is fully drained — nothing todo, in progress, or blocked,
		// so every task is done. A blocked or unfinished-but-idle queue keeps watching (the work
		// isn't done); held a few ticks so a torn read of a task move can't end it early.
		if tasksDrained(c) {
			settled++
		} else {
			settled = 0
		}
		if settled >= fleetIdleExit {
			finalFrame = tasksWatchFrame(name, items, c, spin)
			return 0, nil
		}
		select {
		case <-sig:
			return 0, nil
		case <-t.C:
		}
	}
}

// readAllTasks reads every configured queue's task tree into one combined list — a monorepo can
// configure several (the common case is the single .agent/tasks).
func readAllTasks(roots []string) []taskItem {
	var all []taskItem
	for _, root := range roots {
		all = append(all, readTaskTree(root)...)
	}
	return all
}

// tasksDrained reports whether the queue has no work left — nothing todo, in progress, or blocked,
// so every task is done (or there are none). It's the auto-exit condition for `coop tasks watch`:
// a blocked or unfinished-but-idle queue is NOT drained, so the watch keeps running.
func tasksDrained(c taskCounts) bool {
	return c.Todo == 0 && c.Doing == 0 && c.Blocked == 0
}

// tasksWatchFrame renders the live task board: a header (repo + progress bar + done/total), then
// the actionable tasks grouped by state — in progress (spinner), todo, blocked. Done is the header
// count, not a list (it would only grow). Pure (takes the gathered items), so it unit-tests without
// a terminal.
func tasksWatchFrame(name string, items []taskItem, c taskCounts, spin int) []string {
	p := ui.For(os.Stdout)
	frac := 0.0
	if c.total() > 0 {
		frac = float64(c.Done) / float64(c.total())
	}
	header := fmt.Sprintf("%s %s  %s  %s/%d done",
		taskWatchGlyph(p, c, spin), p.Bold(name+" tasks"), ui.ProgressBar(frac, 22),
		p.Green(fmt.Sprintf("%d", c.Done)), c.total())
	out := []string{header, ""}

	byState := map[string][]taskItem{}
	for _, t := range items {
		byState[t.State] = append(byState[t.State], t)
	}
	const perState = 8 // cap each section so a big backlog stays glanceable
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

// taskWatchGlyph leads the header: a spinner while anything's in progress, a green ✓ when every
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
