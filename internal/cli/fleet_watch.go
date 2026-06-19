package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	fleetPoll  = 400 * time.Millisecond // how often the watch dashboard re-reads each fork
	fleetNameW = 14                     // fork-name column width
	fleetBarW  = 10                     // per-fork progress bar width
)

// fleetTotalBarW sizes the bottom roll-up bar so its right edge lines up with the per-fork
// bars above it. A per-fork bar sits behind: state glyph + space + agent badge + space + name
// + space (one glyph, one badge, two spaces, the name, one space = nameW + 3 more than the
// roll-up bar's "spinner + space" prefix).
const fleetTotalBarW = fleetNameW + fleetBarW + 3

// fleetRow is one fork's fast-changing state for the live dashboard. It reads only the cheap
// sources (the queue file, the pidfile, the log tail) so the dashboard can refresh several
// times a second without the per-tick git subprocesses `coop status` runs for its snapshot.
type fleetRow struct {
	name    string
	agent   string
	running bool
	counts  taskCounts
	active  string
	lastLog string
}

func gatherFleetRow(repo, name string) fleetRow {
	ws := forkWorkspace(repo, name)
	counts, active := scanTasks(readFileString(filepath.Join(ws, ".agent", "TASKS.md")))
	return fleetRow{
		name:    name,
		agent:   readForkAgent(ws),
		running: forkRunningPid(repo, name) != 0,
		counts:  counts,
		active:  active,
		lastLog: lastLogLine(forkLog(repo, name)),
	}
}

// fleetWatch renders a live dashboard of every fork's progress, refreshed by polling the same
// per-fork queue/pidfiles `coop status` reads plus the tail of each fork's log — a live `coop
// status`. It is read-only: Ctrl-C clears the display and exits 0. Without a terminal it prints
// a single `coop status` snapshot instead, so it stays pipe-safe.
func (a *app) fleetWatch() (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if !ui.IsTerminal(os.Stdout) || !ui.IsTerminal(os.Stderr) {
		return a.cmdStatus(nil) // not a terminal: one snapshot
	}
	if len(forkNames(repo)) == 0 {
		ui.Info("no forks yet — open one with 'coop fork <name>' or a fleet with 'coop fleet up'")
		return 0, nil
	}

	region := ui.NewRegion(os.Stdout, func() int { return ui.TermWidth(os.Stdout) })
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	t := time.NewTicker(fleetPoll)
	defer t.Stop()

	name := filepath.Base(repo)
	for spin := 0; ; spin++ {
		names := forkNames(repo) // re-read so a fork added/removed mid-watch shows up
		rows := make([]fleetRow, len(names))
		for i, n := range names {
			rows[i] = gatherFleetRow(repo, n)
		}
		region.Update("", fleetDashboard(name, rows, spin))
		select {
		case <-sig:
			region.Clear()
			fmt.Fprintln(os.Stdout) // leave the cursor on a fresh line
			return 0, nil
		case <-t.C:
		}
	}
}

// fleetDashboard renders the watch view: a header, one row per fork, and a global progress bar.
// Pure (it takes the already-gathered rows) so it unit-tests without a real fleet.
func fleetDashboard(name string, rows []fleetRow, spin int) []string {
	var running, blocked, done, total int
	body := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.running {
			running++
		}
		blocked += r.counts.Blocked
		done += r.counts.Done
		total += r.counts.total()
		body = append(body, fleetRowLine(r, spin))
	}
	frac := 0.0
	if total > 0 {
		frac = float64(done) / float64(total)
	}
	header := fmt.Sprintf("%s — %d running, %s blocked", ui.Bold(name+" fleet"), running, paintCount(blocked, ui.Red))
	bar := fmt.Sprintf("%s %s  %d/%d tasks · %d running · %s blocked",
		ui.Cyan(ui.SpinFrames[spin%len(ui.SpinFrames)]), ui.ProgressBar(frac, fleetTotalBarW), done, total, running, paintCount(blocked, ui.Red))

	out := make([]string, 0, len(body)+4)
	out = append(out, header, "")
	out = append(out, body...)
	out = append(out, "", bar)
	return out
}

// fleetRowLine renders one fork's row: a state glyph (spinner running / ‖ idle / ✓ done), a
// small progress bar, the done/total count, what it's working on, and the last line of its log.
func fleetRowLine(r fleetRow, spin int) string {
	done := !r.running && r.counts.total() > 0 && r.counts.Done == r.counts.total()
	var glyph string
	switch {
	case r.running:
		glyph = ui.Cyan(ui.SpinFrames[spin%len(ui.SpinFrames)])
	case done:
		glyph = ui.Green("✓")
	default:
		glyph = "‖" // idle/paused — a 1-cell pause mark (⏸ rendered 2 wide and misaligned the bars)
	}
	frac := 0.0
	if t := r.counts.total(); t > 0 {
		frac = float64(r.counts.Done) / float64(t)
	}
	doing := truncate(r.active, 32) // the active task is plain; the empty cases are colored
	if r.active == "" {
		if r.counts.total() == 0 {
			doing = "(no queue)"
		} else {
			doing = ui.Green("✓ done")
		}
	}
	line := fmt.Sprintf("%s %s %-*s %s %5s  %s",
		glyph, agentBadge(r.agent), fleetNameW, truncate(r.name, fleetNameW), ui.ProgressBar(frac, fleetBarW), fmt.Sprintf("%d/%d", r.counts.Done, r.counts.total()), doing)
	if r.lastLog != "" {
		line += "  " + ui.Dim(truncate(r.lastLog, 44))
	}
	if !r.running && !done {
		line = ui.DimLine(line) // an idle/stopped fork recedes so the working ones stand out
	}
	return line
}

// agentBadge is a 1-cell colored letter naming a fork's agent (c=claude, x=codex, g=gemini),
// so the dashboard shows who runs each fork without spending the name column on it.
func agentBadge(agent string) string {
	switch agent {
	case "claude":
		return ui.Magenta("c")
	case "codex":
		return ui.Green("x")
	case "gemini":
		return ui.Yellow("g")
	case "":
		return ui.Dim("?")
	default:
		return ui.Dim(string([]rune(agent)[0]))
	}
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
