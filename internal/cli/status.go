package cli

import (
	"fmt"
	"path/filepath"

	"github.com/AndrewDryga/coop/internal/box"
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
	ins, del := parseShortstat(gitOutFork(ws, "diff", "--shortstat", "origin/HEAD"))
	counts, active := scanTasks(readFileString(filepath.Join(ws, ".agent", "TASKS.md")))
	return forkStatus{
		Name:    name,
		Agent:   agent,
		Branch:  forkBranch(ws),
		Updated: forkUpdated(ws),
		Active:  active,
		Running: forkRunningPid(repo, name) != 0,
		Ins:     ins,
		Del:     del,
		Dirty:   forkDirty(ws),
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
	case s.Active == "":
		return "✓ done"
	default:
		return truncate(s.Active, 44)
	}
}

// cmdStatus rolls up the fleet: one progress line per fork plus totals, so an overnight
// run can be checked at a glance without tailing N logs.
func (a *app) cmdStatus(_ []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Info("no forks yet — open one with 'coop fork <name>' or a fleet with 'coop fleet up'")
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

	fmt.Printf("%s — %d fork(s), %d running, %d blocked\n\n",
		ui.Bold(filepath.Base(repo)+" fleet"), len(names), running, blocked)
	const format = "  %-16s %-8s %-10s %-14s %s\n"
	// Bold the rendered line, not each cell (see forkLs): per-cell bold would count the
	// ANSI bytes inside the %-Ns widths and misalign the header against the rows.
	fmt.Print(ui.Bold(fmt.Sprintf(format, "NAME", "STATE", "TASKS", "CHANGES", "ACTIVE")))
	for _, s := range statuses {
		fmt.Printf(format, s.Name, s.stateCell(), s.tasksCell(), s.changesCell(), s.activeCell())
	}
	fmt.Printf("\n  %d/%d tasks done · %d running · %d blocked\n", doneTasks, totalTasks, running, blocked)
	return 0, nil
}
