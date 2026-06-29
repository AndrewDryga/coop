package cli

import (
	"fmt"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/ui"
)

// forkStatus is the at-a-glance state of one fork, gathered from sources that already
// exist — the fork's queue, git, and the loop pidfile — with no daemon or new bookkeeping.
type forkStatus struct {
	Name, Agent, Branch, Updated, Active string
	Running                              bool
	Ins, Del                             int
	Dirty                                bool
	Counts                               taskCounts
}

// gatherForkStatus reads one fork's state. Git runs through the hardened fork helpers
// because the tree is agent-controlled (see forkBranch/forkUpdated for why).
func gatherForkStatus(repo, name string) forkStatus {
	ws := forkWorkspace(repo, name)
	agent := readForkAgent(ws)
	if agent == "" {
		agent = "?" // a fork made before agents were remembered
	}
	ins, del := parseShortstat(gitOut(ws, "diff", "--shortstat", "origin/HEAD"))
	counts, active := queueCounts(wsTaskSource(ws))
	return forkStatus{
		Name:    name,
		Agent:   agent,
		Branch:  forkBranch(ws),
		Updated: forkUpdated(ws),
		Active:  active,
		Running: forkRunningPid(repo, name) != 0,
		Ins:     ins,
		Del:     del,
		Dirty:   gitDirty(ws),
		Counts:  counts,
	}
}

func (s forkStatus) stateCell() string {
	if s.Running {
		return "running"
	}
	return "idle"
}

// tasksCell renders task progress compactly: done/total, plus a blocked flag.
func (s forkStatus) tasksCell() string {
	if s.Counts.total() == 0 {
		return "—"
	}
	cell := fmt.Sprintf("%d/%d", s.Counts.Done, s.Counts.total())
	if s.Counts.Blocked > 0 {
		cell += fmt.Sprintf(" ⚠%d", s.Counts.Blocked)
	}
	return cell
}

// changesCell renders the diff against origin/HEAD, flagging an uncommitted tree.
func (s forkStatus) changesCell() string {
	cell := fmt.Sprintf("+%d -%d", s.Ins, s.Del)
	if s.Dirty {
		cell += " ⚑"
	}
	return cell
}

// activeCell names what the fork is working on: the in-flight (or next) task, or that
// the queue is drained / absent.
func (s forkStatus) activeCell() string {
	switch {
	case s.Counts.total() == 0:
		return "(no queue)"
	case s.Active != "":
		return truncate(s.Active, 44)
	case s.Counts.Blocked > 0:
		return "blocked" // nothing actionable, but tasks are parked on a decision — NOT done
	default:
		return "✓ done"
	}
}

// fleetSnapshot rolls up where the work stands: one progress line per fork plus totals (or the
// local queue when there are no forks), so an overnight run can be checked at a glance without
// tailing N logs. It's the one-shot fallback for the live `coop tasks watch` board — printed when
// there's no TTY to animate, or no forks to watch.
func (a *app) fleetSnapshot(repo string) (int, error) {
	names := forkNames(repo)
	if len(names) == 0 {
		// No forks — but in the single-loop workflow there's still a local queue to report.
		// Show its progress instead of a bare "no forks", so the snapshot is useful either way.
		if rels, err := taskQueues(a.cfg, repo, nil); err == nil && len(rels) > 0 {
			abs := make([]string, len(rels))
			for i, r := range rels {
				abs[i] = filepath.Join(repo, r)
			}
			if c, active := queueProgress(abs); c.total() > 0 {
				ui.Note("%s — local queue: %s", ui.Bold(filepath.Base(repo)), progressLine(c, active))
				ui.Note("  no forks yet — 'coop fork <name>' or 'coop fleet up' to run several in parallel")
				return 0, nil
			}
		}
		ui.Note("no forks yet — open one with 'coop fork <name>' or a fleet with 'coop fleet up'")
		return 0, nil
	}

	statuses := make([]forkStatus, len(names))
	var running, blocked, doneTasks, totalTasks int
	for i, n := range names {
		s := gatherForkStatus(repo, n)
		statuses[i] = s
		if s.Running {
			running++
		}
		blocked += s.Counts.Blocked
		doneTasks += s.Counts.Done
		totalTasks += s.Counts.total()
	}

	fmt.Printf("%s — %s, %d running, %d blocked\n\n",
		ui.Bold(filepath.Base(repo)+" fleet"), ui.Count(len(names), "fork"), running, blocked)
	// Size the NAME column to the longest fork name (clamped + rune-padded) so a long name keeps
	// every later column aligned under its header — see forkLs.
	nw := colWidth(names, len("NAME"), 24)
	// Rune-pad every cell (padRight) instead of %-Ns: a glyph like ⚠/⚑ in TASKS/CHANGES is multi-byte,
	// so %-Ns (which counts bytes) would short-pad and shove later columns out from under their headers.
	format := "  %s %s %s %s %s\n"
	// Bold the rendered line, not each cell (see forkLs): per-cell bold would count the
	// ANSI bytes inside the widths and misalign the header against the rows.
	fmt.Print(ui.Bold(fmt.Sprintf(format, padRight("NAME", nw), padRight("STATE", 8), padRight("TASKS", 10), padRight("CHANGES", 14), "ACTIVE")))
	for _, s := range statuses {
		fmt.Printf(format, padRight(truncate(s.Name, nw), nw), padRight(s.stateCell(), 8), padRight(s.tasksCell(), 10), padRight(s.changesCell(), 14), s.activeCell())
	}
	fmt.Printf("\n  %d/%d tasks done · %d running · %d blocked\n", doneTasks, totalTasks, running, blocked)
	return 0, nil
}
