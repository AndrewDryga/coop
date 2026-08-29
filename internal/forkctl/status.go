package forkctl

import (
	"fmt"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
)

// forkStatus is the at-a-glance state of one fork, joined from its host-owned lifecycle,
// assignment, sandbox-activity, and Git records without relying on an agent-writable queue copy.
type forkStatus struct {
	Name, Agent, Branch, Updated string
	Running                      bool
	Cleanup                      bool
	Active                       int
	Parked                       int
	CleanupSandboxes             int
	UnverifiedSandboxes          int
	Reserved                     bool
	Ready                        bool
	Landing                      bool
	Ins, Del                     int
	Dirty                        bool
	Legacy                       bool
	Counts                       tasks.TaskCounts
	Problems                     []string
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
	var counts tasks.TaskCounts
	var problems []string
	identity, hasIdentity, generationErr := forkspace.ReadGeneration(repo, name)
	if generationErr != nil {
		problems = append(problems, "generation: "+generationErr.Error())
	} else if hasIdentity {
		if assignments, err := tasks.ForkAssignments(repo, identity); err == nil {
			for _, assignment := range assignments {
				switch assignment.Record.Fork.Phase {
				case tasks.ForkAssignmentBlocked:
					counts.Blocked++
				case tasks.ForkAssignmentReviewing, tasks.ForkAssignmentReady:
					counts.Done++
				default:
					counts.Doing++
				}
			}
		} else {
			problems = append(problems, "assignments: "+err.Error())
		}
	}
	running := forkspace.RunningPid(repo, name) != 0
	cleanup := !running && pathExists(forkspace.PidPath(repo, name))
	active, parked, cleanupSandboxes, unverifiedSandboxes := 0, 0, 0, 0
	reserved, ready, landing := false, false, false
	projectSnapshot := tasks.ReadProjectSnapshot(repo, nil)
	for _, observed := range projectSnapshot.Forks {
		if observed.Name != name || observed.Identity != nil && hasIdentity && *observed.Identity != identity {
			continue
		}
		if observed.Identity != nil && hasIdentity && *observed.Identity == identity && !observed.WorkspaceValid {
			problems = append(problems, "workspace: generation binding is missing, replaced, or unreadable")
		}
		executionActive, executionParked, executionCleanup, executionUnverified, executionProblems := summarizeForkExecutions(observed.Executions)
		active += executionActive
		parked += executionParked
		cleanupSandboxes += executionCleanup
		unverifiedSandboxes += executionUnverified
		problems = append(problems, executionProblems...)
		reserved = reserved || observed.Reservation != nil
		ready = ready || observed.Candidate
		landing = landing || observed.PendingLand
		cleanup = cleanup || observed.CleanupPending || executionCleanup > 0
	}
	cost, _ := c.host.forkCost(ws)
	return forkStatus{
		Name:                name,
		Agent:               agent,
		Branch:              forkBranch(ws),
		Updated:             forkUpdated(repo, ws),
		Running:             running,
		Cleanup:             cleanup,
		Active:              active,
		Parked:              parked,
		CleanupSandboxes:    cleanupSandboxes,
		UnverifiedSandboxes: unverifiedSandboxes,
		Reserved:            reserved,
		Ready:               ready,
		Landing:             landing,
		Ins:                 ins,
		Del:                 del,
		Dirty:               gitDirty(ws),
		Legacy:              generationErr == nil && !hasIdentity && pathExists(ws),
		Counts:              counts,
		Problems:            problems,
		Cost:                cost,
	}
}

func summarizeForkExecutions(observations []forkspace.ExecutionObservation) (active, parked, cleanup, unverified int, problems []string) {
	for _, observation := range observations {
		switch {
		case observation.Stale:
			cleanup++
		case !observation.Running:
			unverified++
			problems = append(problems, fmt.Sprintf("sandbox %s liveness is unverifiable", observation.Record.ID))
		case observation.Active:
			active++
		default:
			parked++
		}
	}
	return active, parked, cleanup, unverified, problems
}

func (s forkStatus) stateCell() string {
	if s.UnverifiedSandboxes > 0 {
		return "unknown"
	}
	// A stale worker/runtime has one safe, explicit next action even when its workspace or another
	// authority record is damaged. Keep rendering the problem separately, but do not hide cleanup
	// behind "unknown" or operators lose the command that can remove the exact stale authority.
	if s.Cleanup {
		return "cleanup"
	}
	if len(s.Problems) > 0 {
		return "unknown"
	}
	if s.Running {
		return "running"
	}
	if s.Active > 0 {
		return "active"
	}
	if s.Reserved {
		return "session"
	}
	if s.Parked > 0 {
		return "parked"
	}
	if s.Landing {
		return "landing"
	}
	if s.Ready {
		return "ready"
	}
	if s.Legacy {
		return "legacy"
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
