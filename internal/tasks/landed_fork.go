package tasks

import (
	"errors"
	"fmt"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A fork's task completes when its reviewed work is merged. When that merge cannot happen — the
// fork's box keeps failing a check the project itself passes — a person can bring the fork's commit
// to the project's branch some other way and then run `coop tasks done <id> --from-fork`. Coop
// completes the task from the commit already on the branch, releases that one assignment and keeps
// the fork; a later merge of the fork drops the commits the branch already has.

// errFromForkNeedsTerminal keeps `coop tasks done --from-fork` a person's tool, as
// errForceCloseNeedsTerminal keeps --force one.
var errFromForkNeedsTerminal = &ui.UsageError{
	Headline: "coop tasks done --from-fork needs a person at a terminal",
	Cause:    "It takes a task away from the fork that holds it.\nAn agent leaves a fork's tasks to the fork.",
}

// LandedForkTask reports a completion taken from a fork: the fork, the commit on the project's
// branch that carries the task's trailer, whether the branch had it before the fork took the task,
// and the tasks the fork had found, which Coop added to the queue first.
type LandedForkTask struct {
	Fork       string
	Commit     string
	Predates   bool
	Discovered []string
}

// landedForkRelease is the exact assignment a completion takes from a fork, checked while the
// fork's lifecycle lock is held. index is nil only after an interrupted run already removed it.
type landedForkRelease struct {
	repo     string
	owner    ForkTaskOwner
	index    *ForkAssignmentIndex
	commit   string
	predates bool // commit was on the branch before the fork took the task
}

// CompleteLandedForkTask completes task, which a fork holds, because the one commit carrying its
// Coop-Task trailer is already reachable from project's HEAD. Only a person's `coop tasks done
// --from-fork` reaches it; see completeLandedForkTask for what it refuses.
func CompleteLandedForkTask(project, root string, task Item) (LandedForkTask, error) {
	_, landed, err := completeLandedForkTask(project, root, task, completionOptions{})
	return landed, err
}

// completeLandedForkTask refuses unless exactly one commit on the project's branch carries the
// task's trailer, the fork that holds it is stopped, and that fork has no candidate in review or
// ready to merge, whose merge completes the task instead. A commit the branch had before the fork
// took the task also needs the branch to have every commit the fork made since. Under the fork's
// lifecycle lock it first adds the tasks the fork found, which the released assignment could no
// longer import, then completes the task the way `coop tasks done` does and releases the
// assignment last. Each step can be repeated, so rerunning the command finishes an interrupted one.
func completeLandedForkTask(project, root string, task Item, opts completionOptions) (*IncompleteChecklistError, LandedForkTask, error) {
	id := ui.SafeInline(task.ID)
	record, owned, err := ReadTaskOwnerRecord(root, task.ID)
	if err != nil {
		return nil, LandedForkTask{}, err
	}
	if !owned || record.Kind != TaskOwnerFork || record.Fork == nil {
		return nil, LandedForkTask{}, ui.CommandFailed("Could not complete "+id+" from a fork",
			id+" is not assigned to a fork.", [2]string{"Complete it:", "coop tasks done " + id})
	}
	owner := *record.Fork
	name := owner.Fork.Name
	refused := func(cause string, rows ...[2]string) error {
		return ui.CommandFailed("Could not complete "+id+" from fork "+name, cause, rows...)
	}
	repo, head := auditHistory(project)
	if repo == "" || head == "" {
		return nil, LandedForkTask{}, refused("Coop could not read this project's Git history to find the task's commit.")
	}
	onBranch, err := rawReachableAuditCommits(repo, head)
	if err != nil {
		return nil, LandedForkTask{}, refused("Coop could not read this branch's history to find the task's commit.")
	}
	bindings := taskBindingsOf(onBranch)
	trailer := CoopTaskTrailer + ": " + task.ID
	switch shas := bindings[task.ID]; {
	case len(shas) == 0:
		return nil, LandedForkTask{}, refused(fmt.Sprintf("No commit on this branch carries %q.\nBring the fork's commit here first, or let the fork finish and merge it.", trailer))
	case len(shas) > 1:
		return nil, LandedForkTask{}, extraBindings(repo, task.ID, shas)
	}
	unlock, locked := forkspace.TryLockState(project, name)
	if !locked {
		return nil, LandedForkTask{}, refused("Another Coop command is using the fork.",
			[2]string{"Try again when it finishes:", "coop tasks done " + id + " --from-fork"})
	}
	defer unlock()
	release, err := landedForkAssignment(project, owner, refused)
	if err != nil {
		return nil, LandedForkTask{}, err
	}
	release.commit = bindings[task.ID][0]
	const unread = "Coop could not read the fork's history to check what it committed after taking the task."
	// The fork took the task at its baseline, its HEAD then. The project lacks that commit while the
	// fork has unmerged work, so the fork's own history must answer.
	started, ok := reachableCommits(repo, owner.BaselineHead)
	if !ok {
		if started, ok = forkReachable(project, owner.Fork, owner.BaselineHead); !ok {
			return nil, LandedForkTask{}, refused(unread)
		}
	}
	// A commit the fork was handed with its task, such as the loop's commit before its completion was
	// refused, is not the fork's work landing. It completes the task only while this branch has every
	// commit the fork made since, with or without a trailer.
	if release.predates = started[release.commit]; release.predates {
		forkHead, ok := forkReachable(project, owner.Fork, "HEAD")
		if !ok {
			return nil, LandedForkTask{}, refused(unread)
		}
		branch := commitSet(onBranch)
		missing := 0
		for commit := range forkHead {
			if !started[commit] && !branch[commit] {
				missing++
			}
		}
		if missing > 0 {
			return nil, LandedForkTask{}, refused(fmt.Sprintf("Commit %s carrying %q was already on the branch when fork %s took the task, and the fork has %s since then that this branch does not have.",
				shortCommit(release.commit), trailer, name, ui.Count(missing, "commit")),
				[2]string{"Merge the fork's work first:", "coop fork merge " + name})
		}
	}
	landed := LandedForkTask{Fork: name, Commit: release.commit, Predates: release.predates}
	imported, err := importForkProposalsLocked(project, owner.Fork)
	for _, proposal := range imported {
		landed.Discovered = append(landed.Discovered, proposal.TaskID)
	}
	if err != nil {
		return nil, landed, fmt.Errorf("add the tasks fork %s found: %w", name, err)
	}
	opts.landed = release
	forced, err := completeTrustedTask(project, root, task, opts)
	return forced, landed, err
}

// landedForkAssignment checks, under the fork's lifecycle lock, that the fork may give up owner's
// assignment: the same generation still exists, nothing runs in it, no merge or discard is pending,
// and no candidate is in review or ready to merge. refused draws a refusal.
func landedForkAssignment(repo string, owner ForkTaskOwner, refused func(string, ...[2]string) error) (*landedForkRelease, error) {
	identity, name := owner.Fork, owner.Fork.Name
	current, exists, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return nil, err
	}
	if !exists || current != identity {
		return nil, refused("The fork that holds it is gone or was recreated.")
	}
	if forkspace.NeedsStop(repo, name) {
		return nil, refused("The fork is running, or its loop has not been cleaned up.",
			[2]string{"Stop it first:", "coop fork stop " + name})
	}
	if err := forkspace.RequireNoForkExecutionsLocked(repo, identity); err != nil {
		return nil, err
	}
	if err := forkspace.RequireNoWorkspaceReservationLocked(repo, identity); err != nil {
		return nil, err
	}
	if err := requireNoForkLifecycleIntent(repo, identity); err != nil {
		return nil, err
	}
	if state, exists, err := readForkCandidateState(repo, identity); err != nil {
		return nil, err
	} else if exists && forkCandidateManifestActive(*state.manifest) {
		return nil, refused("The fork's work is in its final review or ready to merge, and the merge completes this task.",
			[2]string{"Merge it:", "coop fork merge " + name})
	}
	indexes, problems := IndexedForkAssignments(repo, identity)
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	release := &landedForkRelease{repo: repo, owner: owner}
	for i := range indexes {
		if indexes[i].AssignmentID == owner.AssignmentID {
			release.index = &indexes[i]
		}
	}
	return release, nil
}

// reachableCommits is every commit reachable from rev in repo. They are read as raw commit objects
// whose IDs Coop checks, so no shallow file, graft or commit-graph that a box writes can change
// them. It reports false when Coop cannot read the whole history.
func reachableCommits(repo, rev string) (map[string]bool, bool) {
	commits, err := rawReachableAuditCommits(repo, rev)
	if err != nil {
		return nil, false
	}
	return commitSet(commits), true
}

func commitSet(commits []rawReachableAuditCommit) map[string]bool {
	set := make(map[string]bool, len(commits))
	for _, commit := range commits {
		set[commit.sha] = true
	}
	return set
}

// forkReachable is reachableCommits in the workspace the fork's generation still names.
func forkReachable(repo string, fork forkspace.Identity, rev string) (map[string]bool, bool) {
	if forkspace.ValidateGenerationWorkspace(repo, fork) != nil {
		return nil, false
	}
	return reachableCommits(forkspace.Workspace(repo, fork.Name), rev)
}

// requireOwner is the check completeTrustedTask makes in place of refusing a fork's task: the task is
// still owned by exactly this assignment.
func (r *landedForkRelease) requireOwner(root, id string) error {
	record, owned, err := ReadTaskOwnerRecord(root, id)
	if err != nil {
		return err
	}
	if !owned || record.Kind != TaskOwnerFork || record.Fork == nil || !sameForkAssignment(*record.Fork, r.owner) {
		return fmt.Errorf("task %s changed owner before it could be taken from fork %s", id, r.owner.Fork.Name)
	}
	return nil
}

// release ends the assignment once the task is done: the fork's records of the tasks it found go
// first, then its index entry, then the owner record, so an interrupted run leaves the task still
// held by the fork and a rerun finishes it. It reports whether this call removed the owner record.
func (r *landedForkRelease) release(root, id string) (bool, error) {
	if r.index != nil {
		if err := finalizeForkProposalRecords(r.repo, *r.index); err != nil {
			return false, err
		}
		if err := RemoveForkAssignmentIndex(r.repo, *r.index); err != nil {
			return false, err
		}
	}
	lock, err := lockTaskOwner(root, id)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	record, owned, err := lock.Read()
	if err != nil || !owned {
		return false, err
	}
	if record.Kind != TaskOwnerFork || record.Fork == nil || !sameForkAssignment(*record.Fork, r.owner) {
		return false, fmt.Errorf("task %s changed owner before fork %s released it", id, r.owner.Fork.Name)
	}
	return true, removeTaskOwnerRecordFile(root, id)
}

// note is the log line that records where the work landed.
func (r *landedForkRelease) note() string {
	if r.predates {
		return fmt.Sprintf("Completed with coop tasks done --from-fork: commit %s carrying its trailer was on the project's branch before fork %s took this task, and the branch has every commit the fork made since, so the fork no longer holds this task.",
			shortCommit(r.commit), r.owner.Fork.Name)
	}
	return fmt.Sprintf("Completed with coop tasks done --from-fork: commit %s carrying its trailer was already on the project's branch, so fork %s no longer holds this task.",
		shortCommit(r.commit), r.owner.Fork.Name)
}
