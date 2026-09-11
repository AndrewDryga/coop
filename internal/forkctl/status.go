package forkctl

import (
	"fmt"
	"strings"

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
	// Phases are the fork's assignments as their OWN phase records them. Counts folds reviewing
	// and ready into Done, so it cannot prove a review passed; these can.
	Doing, Reviewing, ReadyTasks, Blocked int
	Problems                              []string
	Cost                                  float64 // total loop spend across the fork's runs, 0 if it never ran
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
	var doing, reviewing, readyTasks, blockedTasks int
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
					blockedTasks++
				case tasks.ForkAssignmentReviewing:
					counts.Done++
					reviewing++
				case tasks.ForkAssignmentReady:
					counts.Done++
					readyTasks++
				default:
					counts.Doing++
					doing++
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
		Doing:               doing,
		Reviewing:           reviewing,
		ReadyTasks:          readyTasks,
		Blocked:             blockedTasks,
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

// stateWords translates one machine state into what it MEANS. The machine value is the JSON
// projection's contract and never changes; this is the sentence a person reads instead of
// decoding a one-word enum.
func stateWords(state string) string {
	switch state {
	case "running":
		return "background loop running"
	case "active":
		return "agent running"
	case "session":
		return "reserved for a session"
	case "parked":
		return "waiting"
	case "landing":
		return "merging"
	case "ready":
		return "ready to merge"
	case "legacy":
		return "older fork format"
	case "cleanup":
		return "cleanup needed"
	case "unknown":
		return "status unavailable"
	}
	return state
}

// tasksLine is the fork's assignments in their OWN phases. Counts folds reviewing and ready into
// Done, so rendering that number would claim a review passed; each phase speaks for itself here.
// Empty when the fork tracks no task — a dash is a ledger, not information.
func (s forkStatus) tasksLine() string {
	var parts []string
	if s.Doing > 0 {
		parts = append(parts, fmt.Sprintf("%d in progress", s.Doing))
	}
	if s.Reviewing > 0 {
		parts = append(parts, fmt.Sprintf("%d being reviewed", s.Reviewing))
	}
	if s.ReadyTasks > 0 {
		parts = append(parts, fmt.Sprintf("%d ready to merge", s.ReadyTasks))
	}
	if s.Blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", s.Blocked))
	}
	return strings.Join(parts, " · ")
}

// changesLine is the diff against the parent branch, saying an uncommitted tree in words rather
// than as an unexplained glyph.
func (s forkStatus) changesLine() string {
	cell := fmt.Sprintf("+%d −%d", s.Ins, s.Del)
	if s.Dirty {
		cell += " · uncommitted changes"
	}
	return cell
}

// costLine is the fork's reported loop spend, or "" when nothing was reported — never $0.00,
// which would read as a free fork.
func (s forkStatus) costLine() string {
	if s.Cost == 0 {
		return ""
	}
	return fmt.Sprintf("$%.2f", s.Cost)
}
