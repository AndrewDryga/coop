package forkctl

import (
	"fmt"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// forkStatus is the at-a-glance state of one fork, gathered from sources that already
// exist — the fork's queue, git, and the loop pidfile — with no daemon or new bookkeeping.
type forkStatus struct {
	Name, Agent, Branch, Updated string
	Running                      bool
	Cleanup                      bool
	Ins, Del                     int
	Dirty                        bool
	Counts                       tasks.TaskCounts
	Cost                         float64 // total loop spend across the fork's runs, 0 if it never ran
}

// gatherForkStatus reads one fork's state. Git runs through the hardened fork helpers
// because the tree is agent-controlled (see forkBranch/forkUpdated for why).
func (c *Control) gatherForkStatus(repo, name string) forkStatus {
	ws := forkspace.Workspace(repo, name)
	agent := ReadForkAgent(ws)
	if agent == "" {
		agent = "?" // a fork made before agents were remembered
	}
	ins, del := parseShortstat(gitOut(ws, "diff", "--shortstat", "origin/HEAD"))
	counts, _ := tasks.QueueCounts(tasks.WsTaskSource(ws))
	running := forkspace.RunningPid(repo, name) != 0
	cost, _ := c.host.forkCost(ws)
	return forkStatus{
		Name:    name,
		Agent:   agent,
		Branch:  forkBranch(ws),
		Updated: forkUpdated(repo, ws),
		Running: running,
		Cleanup: !running && pathExists(forkspace.PidPath(repo, name)),
		Ins:     ins,
		Del:     del,
		Dirty:   gitDirty(ws),
		Counts:  counts,
		Cost:    cost,
	}
}

func (s forkStatus) stateCell() string {
	if s.Running {
		return "running"
	}
	if s.Cleanup {
		return "cleanup"
	}
	return "idle"
}

// tasksCell renders task progress compactly: done/total, plus a blocked flag.
func (s forkStatus) tasksCell() string {
	if s.Counts.Total() == 0 {
		return "—"
	}
	cell := fmt.Sprintf("%d/%d", s.Counts.Done, s.Counts.Total())
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

// costCell renders the fork's total loop spend, or — when it hasn't run (no cost telemetry yet).
func (s forkStatus) costCell() string {
	if s.Cost == 0 {
		return "—"
	}
	return fmt.Sprintf("$%.2f", s.Cost)
}
