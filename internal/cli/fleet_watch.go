package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	fleetNameW = 14 // fork-name column width
	fleetBarW  = 10 // per-fork progress bar width
)

// fleetTotalBarW sizes the bottom roll-up bar so its right edge lines up with the per-fork
// bars above it. Both prefixes start with the same fixed-width state mark, so it cancels; the
// per-fork row then adds an agent badge, two spaces, and the name (nameW + 3 columns).
const fleetTotalBarW = fleetNameW + fleetBarW + 3

// fleetRow is one fork's fast-changing state for the live dashboard. It reads only the cheap
// sources (the queue file, the pidfile, the log tail) so the dashboard can refresh several
// times a second without the per-tick git subprocesses the fleetSnapshot roll-up runs.
type fleetRow struct {
	name    string
	agent   string
	running bool
	cleanup bool
	ran     bool // its loop produced log output — tells a stopped-incomplete fork from a never-started one
	counts  taskCounts
	active  string
	lastLog string
	cost    float64 // the fork's total spend across its loop runs (from its COOP_RUN_ID telemetry), 0 if none
}

func gatherFleetRow(repo, name string) fleetRow {
	ws := forkWorkspace(repo, name)
	counts, active := queueCounts(wsTaskSource(ws))
	running := forkRunningPid(repo, name) != 0
	return fleetRow{
		name:    name,
		agent:   readForkAgent(ws),
		running: running,
		cleanup: !running && pathExists(forkPid(repo, name)),
		ran:     forkRan(repo, name),
		counts:  counts,
		active:  active,
		lastLog: lastLogLine(forkLog(repo, name)),
		cost:    costForRepo(ws).total.usd,
	}
}

// forkRan reports whether a fork's loop has written any log output, so the dashboard can tell a
// fork that started and then stopped with work left (a "stopped" fork, worth surfacing) from one
// that's merely idle and never started (which recedes).
func forkRan(repo, name string) bool {
	fi, err := os.Stat(forkLog(repo, name))
	return err == nil && fi.Size() > 0
}

// keepLastGood rides out a torn read of a fork's task tree (the agent moves task folders as it
// works; a read that lands mid-move can come back empty). A populated queue never legitimately
// drops to zero tasks, so when the fresh read shows none but the last one had some, keep the
// previous counts/active. Everything not derived from the tree (running, lastLog, ran) stays fresh.
func keepLastGood(fresh, prev fleetRow) fleetRow {
	if fresh.counts.total() == 0 && prev.counts.total() > 0 {
		fresh.counts = prev.counts
		fresh.active = prev.active
	}
	return fresh
}

// fleetWatch renders the live `coop fleet watch` board — every fork's progress, refreshed by polling
// the same per-fork queue/pidfiles the snapshot reads plus the tail of each fork's log. It is
// read-only: it auto-exits with a final summary frame once the fleet is finished (nothing running,
// nothing left to start), and Ctrl-C exits 0 anytime. Without a TTY to animate, or with no forks to
// watch, it prints a single fleetSnapshot roll-up instead — so it stays pipe-safe and useful solo.
func (a *app) fleetWatch() (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// No TTY to animate, or no forks to watch (a lone local loop) → the one-shot roll-up, which
	// still reports the local queue. Keeps `coop fleet watch` pipe-safe and useful before a fleet.
	if !ui.IsTerminal(os.Stdout) || !ui.IsTerminal(os.Stderr) || len(forkLifecycleNames(repo)) == 0 {
		return a.fleetSnapshot(repo)
	}

	// Render on the alternate screen (like top/htop). A bottom-pinned region repaints by counting
	// lines up from the bottom, so once the dashboard is taller than the terminal pane each refresh
	// scrolls the top line ("coop fleet — N running") into scrollback — the reported spam. The alt
	// buffer has no scrollback to pollute and is restored on exit.
	screen := ui.NewAltScreen(os.Stdout, func() int { return ui.TermWidth(os.Stdout) })
	name := filepath.Base(repo)
	prev := map[string]fleetRow{} // last good row per fork, to ride out a torn task-tree read
	sawRunning := false           // seen any fork running? — so we don't auto-exit during the startup window
	tick := func(spin int) ([]string, bool) {
		names := forkLifecycleNames(repo) // re-read so a fork added/removed mid-watch shows up
		rows := make([]fleetRow, len(names))
		next := make(map[string]fleetRow, len(names)) // rebuilt each tick so a removed fork's row drops out
		running := 0
		for i, n := range names {
			row := keepLastGood(gatherFleetRow(repo, n), prev[n])
			next[n] = row
			rows[i] = row
			if row.running {
				running++
			}
		}
		prev = next
		frame := fleetDashboard(name, rows, spin)
		screen.Frame(frame)
		// Conclude "finished" — nothing running, nothing left to start — only after a fork's been seen
		// running (the live case), or every fork is already terminal at startup (fleetSettled); never
		// during the startup window, where loops are still launching and momentarily read 0 running.
		if running > 0 {
			sawRunning = true
		}
		return frame, running == 0 && (sawRunning || fleetSettled(rows))
	}
	return runWatchLoop(screen, tick, func() {
		ui.Note("fleet idle — every fork is done, stopped, or blocked; watch exited")
	})
}

// fleetSettled reports whether every fork has finished and nothing is left to start: none is
// running and none is still seeding/pending its first run. It's the startup-safe way to auto-exit
// when watch is launched on an already-finished fleet (nothing was ever seen running) — a fork
// that hasn't seeded a queue and never ran (total 0, !ran) might just be starting, so it blocks the
// conclusion until it either runs or finishes.
func fleetSettled(rows []fleetRow) bool {
	if len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if r.running {
			return false
		}
		if r.cleanup {
			return false
		}
		if r.counts.total() == 0 && !r.ran {
			return false // not yet seeded (startup) or an interactive fork — can't conclude "finished"
		}
	}
	return true
}

// fleetDashboard renders the watch view: a header, one row per fork, and a global progress bar.
// Pure (it takes the already-gathered rows) so it unit-tests without a real fleet.
func fleetDashboard(name string, rows []fleetRow, spin int) []string {
	var running, doing, blocked, done, total int
	var totalCost float64
	// Size the done/total column to the widest count actually present (min "0/0"), so every count
	// sits one space off its bar and the column that follows still lines up — instead of a fixed
	// gap wide enough for "999/999" that no one has.
	countW := len("0/0")
	for _, r := range rows {
		if r.running {
			running++
		}
		doing += r.counts.Doing
		blocked += r.counts.Blocked
		done += r.counts.Done
		total += r.counts.total()
		totalCost += r.cost
		if w := len(fmt.Sprintf("%d/%d", r.counts.Done, r.counts.total())); w > countW {
			countW = w
		}
	}
	body := make([]string, 0, len(rows))
	for _, r := range rows {
		body = append(body, fleetRowLine(r, spin, countW))
	}
	header := fmt.Sprintf("%s — %d running, %s blocked", ui.Bold(name+" fleet"), running, paintCount(blocked, ui.Red))
	bar := fmt.Sprintf("%s %s %s tasks · %d running · %s blocked",
		stateGlyph(running > 0, done, total, spin), ui.ProgressBarStates(done, doing, blocked, total, fleetTotalBarW), fmt.Sprintf("%d/%d", done, total), running, paintCount(blocked, ui.Red))
	if totalCost > 0 {
		bar += fmt.Sprintf(" · $%.2f", totalCost)
	}

	out := make([]string, 0, len(body)+4)
	out = append(out, header, "")
	out = append(out, body...)
	out = append(out, "", bar)
	return out
}

// stateGlyph is the fixed-width status mark shared by the per-fork rows and the roll-up bar: an
// animated spinner only while something is running, a green ✓ when every task is done, else the
// idle/paused mark. Keeping it shared means a still fleet shows no spinner anywhere — the spinner
// implies motion, so it must not run next to a "0 running" bar.
func stateGlyph(running bool, done, total, spin int) string {
	switch {
	case running:
		return padRight(ui.SpinFrame(spin), ui.SpinnerWidth)
	case total > 0 && done == total:
		return ui.Green(padRight("✓", ui.SpinnerWidth))
	default:
		return padRight("‖", ui.SpinnerWidth) // ⏸ renders two cells in common terminals
	}
}

// fleetRowLine renders one fork's row: a state glyph (spinner running / ‖ idle / ✓ done), a
// small progress bar, the done/total count, what it's working on, and the last line of its log.
func fleetRowLine(r fleetRow, spin, countW int) string {
	total := r.counts.total()
	allDone := total > 0 && r.counts.Done == total // "done" = every task in done/, not just "no todo/ left"
	// stopped: the loop exited (not running) with tasks unfinished — it ran and quit at N/M. Distinct
	// from a fork merely idle and never started (no log), which recedes below.
	stopped := !r.running && !r.cleanup && !allDone && r.ran && total > 0
	// blocked: unfinished, but nothing is actionable (no todo/ or in_progress/ task) — the remainder is
	// all blocked/. taskTreeCounts returns active=="" for this exactly as it does for all-done, so it
	// must NOT read as "done".
	blocked := !r.cleanup && !allDone && !stopped && total > 0 && r.active == ""
	glyph := stateGlyph(r.running, r.counts.Done, total, spin)
	switch {
	case r.cleanup:
		glyph = ui.Red(glyph)
	case stopped:
		glyph = ui.Yellow(glyph) // stopped-incomplete: a yellow mark vs a dim ‖ idle one
	case blocked:
		glyph = ui.Red(glyph) // blocked: needs a human to clear the [B]
	}
	var doing string // a terminal/non-actionable state wins; else the task it's on or will take next
	switch {
	case r.cleanup:
		doing = ui.Red("cleanup")
	case total == 0 && r.running:
		doing = "starting" // loop is alive and still seeding its queue (the --tasks copy) — transient
	case total == 0:
		doing = "(no queue)"
	case allDone:
		doing = ui.Green("✓ done")
	case stopped:
		doing = ui.Yellow("stopped") // it quit, isn't working its next task
	case blocked:
		doing = ui.Red("blocked") // every remaining task is [B]
	default:
		doing = truncate(r.active, 32)
	}
	line := fmt.Sprintf("%s %s %-*s %s %-*s  %s",
		glyph, agentBadge(r.agent), fleetNameW, truncate(r.name, fleetNameW), ui.ProgressBarStates(r.counts.Done, r.counts.Doing, r.counts.Blocked, total, fleetBarW), countW, fmt.Sprintf("%d/%d", r.counts.Done, total), doing)
	if r.cost > 0 {
		line += "  " + ui.Dim(fmt.Sprintf("$%.2f", r.cost))
	}
	if r.lastLog != "" {
		line += "  " + ui.Dim(truncate(r.lastLog, 44))
	}
	if !r.running && !allDone && !stopped && !blocked {
		line = ui.DimLine(line) // only a quiet, never-started fork with todos left recedes
	}
	return line
}

// agentBadge is a 1-cell colored letter naming a fork's agent, so the dashboard shows who runs
// each fork without spending the name column on it. Each registered agent owns its own badge
// (Agent.Badge); an empty or unknown agent falls back to a dim initial here.
func agentBadge(agent string) string {
	if ag, ok := agents.Get(agent); ok {
		return ag.Badge()
	}
	if agent == "" {
		return ui.Dim("?")
	}
	// An unknown agent's initial, but only if it's a 1-cell ASCII letter — a wide (e.g. CJK)
	// rune would render 2 cells and shove the whole row out of column. Fall back to "?".
	if r := []rune(agent)[0]; r < 128 {
		return ui.Dim(string(r))
	}
	return ui.Dim("?")
}

// lastLogLine returns the last non-empty line of a fork's log (reading only the tail, since the
// log can be large), for an at-a-glance "what it just did". Empty if the log is missing/empty.
func lastLogLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tailMax = 8 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > tailMax {
		_, _ = f.Seek(fi.Size()-tailMax, io.SeekStart)
	}
	data, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" || strings.HasPrefix(s, "coop:") {
			continue // skip blanks and coop's own banners — the bar and task name already show those
		}
		return s
	}
	return ""
}
