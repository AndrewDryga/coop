package tasks

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

type ForkAssignmentOutcome uint8

const (
	ForkAssignmentExecutorDrained ForkAssignmentOutcome = iota
	ForkAssignmentUnavailable
	ForkAssignmentSelected
)

type ForkAssignmentRequest struct {
	AuthorityRepo string
	Fork          forkspace.Identity
	WorkspaceRoot string
	BaselineHead  string
	LeaseOwner    TaskLeaseOwner
}

type ForkAssignment struct {
	AuthorityRepo string
	Task          QueuedTask
	Owner         ForkTaskOwner
	Lease         *TaskLease
	Outcome       ForkAssignmentOutcome
}

func projectionPath(workspace string, fork forkspace.Identity, assignmentID string) string {
	return filepath.Join(workspace, ".coop", "task-executions", string(fork.Generation), assignmentID, "tasks")
}

func validateForkAssignmentRequest(request ForkAssignmentRequest) error {
	if !forkspace.ValidExistingName(request.Fork.Name) || !forkspace.ValidGeneration(request.Fork.Generation) {
		return errors.New("invalid fork assignment identity")
	}
	if !filepath.IsAbs(request.WorkspaceRoot) || filepath.Clean(request.WorkspaceRoot) != request.WorkspaceRoot {
		return errors.New("fork assignment workspace must be a clean absolute path")
	}
	if !filepath.IsAbs(request.AuthorityRepo) || filepath.Clean(request.AuthorityRepo) != request.AuthorityRepo {
		return errors.New("fork assignment authority repo must be a clean absolute path")
	}
	if !validCommitID(request.BaselineHead) || request.BaselineHead == "" {
		return errors.New("fork assignment requires an exact baseline HEAD")
	}
	return nil
}

func forkAssignmentCandidates(hosts []string) (owned, unowned []QueuedTask, foreign bool, err error) {
	index := queueIndex{}
	for _, root := range hosts {
		items, readErr := ReadTaskTree(root)
		if readErr != nil {
			return nil, nil, false, readErr
		}
		for _, item := range items {
			index.add(item)
		}
		for _, item := range items {
			if item.State != StateInProgress && item.State != StateTodo {
				continue
			}
			record, ok, readErr := readTaskOwnerRecordOf(root, item)
			if readErr != nil {
				return nil, nil, false, fmt.Errorf("read owner record for task %s: %w", item.ID, readErr)
			}
			candidate := QueuedTask{Root: root, Item: item}
			if !ok {
				unowned = append(unowned, candidate)
				continue
			}
			if record.Kind == TaskOwnerFork {
				owned = append(owned, candidate)
			} else {
				foreign = true
			}
		}
	}
	less := func(a, b QueuedTask) bool {
		ai, bi := 1, 1
		if a.Item.State == StateInProgress {
			ai = 0
		}
		if b.Item.State == StateInProgress {
			bi = 0
		}
		if ai != bi {
			return ai < bi
		}
		if ai == 1 && priorityRank(a.Item) != priorityRank(b.Item) {
			return priorityRank(a.Item) < priorityRank(b.Item) // between todo tasks, a person's priority first
		}
		if a.Root != b.Root {
			return a.Root < b.Root
		}
		return a.Item.ID < b.Item.ID
	}
	// A fork takes no task that waits for another (wait.go); the plain loop settles those waits.
	unowned = slices.DeleteFunc(unowned, func(candidate QueuedTask) bool {
		return candidate.Item.State == StateTodo && index.waiting(candidate.Item)
	})
	sort.Slice(owned, func(i, j int) bool { return less(owned[i], owned[j]) })
	sort.Slice(unowned, func(i, j int) bool { return less(unowned[i], unowned[j]) })
	return owned, unowned, foreign, nil
}

// AssignForkTask acquires or resumes exactly one durable canonical assignment. The returned short
// lease is held only while the caller materializes/reconciles the projection; model and Git work
// happen later under the durable owner record, never under this flock.
func AssignForkTask(hosts []string, request ForkAssignmentRequest) (ForkAssignment, error) {
	if err := validateForkAssignmentRequest(request); err != nil {
		return ForkAssignment{}, err
	}
	unlockFork, err := forkspace.LockState(request.AuthorityRepo, request.Fork.Name)
	if err != nil {
		return ForkAssignment{}, fmt.Errorf("lock fork assignment lifecycle: %w", err)
	}
	defer unlockFork()
	if err := forkspace.ValidateGenerationWorkspace(request.AuthorityRepo, request.Fork); err != nil {
		return ForkAssignment{}, fmt.Errorf("validate fork assignment generation: %w", err)
	}
	// A discard that crashed mid-way leaves its intent behind; until it is replayed the fork must
	// take no new work — index recovery below would otherwise drop the half-discarded assignment
	// as an orphan, and a later replay would remove the generation under the new assignment.
	if _, pending, err := readForkDiscard(request.AuthorityRepo, request.Fork); err != nil {
		return ForkAssignment{}, err
	} else if pending {
		return ForkAssignment{}, fmt.Errorf("fork %s has an interrupted discard — replay it with 'coop fork rm %s --force' before assigning work", request.Fork.Name, request.Fork.Name)
	}
	if err := recoverForkAssignmentIndexes(request.AuthorityRepo, request.Fork); err != nil {
		return ForkAssignment{}, err
	}
	if err := recoverForkBlockingAssignmentsLocked(request.AuthorityRepo, request.Fork); err != nil {
		return ForkAssignment{}, err
	}
	// A queue that cannot be read stops the fork even when the check below closes this generation:
	// a drained fork goes on to review and publish its candidate.
	for _, root := range hosts {
		if _, err := ReadTaskTree(root); err != nil {
			return ForkAssignment{}, err
		}
	}
	// Any candidate lifecycle record freezes the generation's assignment set. Transitional
	// reviewing/publication state is deliberately non-landable, but it is still live authority and
	// may not be mistaken for permission to claim another task. Terminal history also closes this
	// generation; a kept landed workspace is evidence, not a fresh scheduler.
	state, frozen, err := readForkCandidateState(request.AuthorityRepo, request.Fork)
	if err != nil {
		return ForkAssignment{}, err
	}
	var rework map[string]bool
	if frozen {
		if state.manifest.Phase != forkCandidateReworking {
			return ForkAssignment{Outcome: ForkAssignmentExecutorDrained}, nil
		}
		if err := replayForkCandidateReworkLocked(request.AuthorityRepo, state.manifest); err != nil {
			return ForkAssignment{}, err
		}
		rework = make(map[string]bool)
		for _, task := range state.manifest.Rework.Tasks {
			rework[task.AssignmentID] = true
		}
	}
	for attempt := 0; attempt < maxLeaseRescans; attempt++ {
		owned, unowned, foreign, err := forkAssignmentCandidates(hosts)
		if err != nil {
			return ForkAssignment{}, err
		}
		var candidates []QueuedTask
		for _, candidate := range owned {
			record, ok, err := ReadTaskOwnerRecord(candidate.Root, candidate.Item.ID)
			if err != nil {
				return ForkAssignment{}, err
			}
			if !ok || record.Kind != TaskOwnerFork || record.Fork.Fork != request.Fork {
				foreign = true
				continue
			}
			if frozen && !rework[record.Fork.AssignmentID] {
				continue
			}
			switch record.Fork.Phase {
			case ForkAssignmentPreparing, ForkAssignmentWorking, ForkAssignmentPaused:
				candidates = append(candidates, candidate)
			default:
				// reviewing/ready/blocked is settled for this executor and must not be reworked.
			}
		}
		if !frozen {
			candidates = append(candidates, unowned...)
		}
		var busy TaskLeaseSummary
		for _, candidate := range candidates {
			lease, observation, leaseErr := TryTaskLease(candidate.Root, candidate.Item, request.LeaseOwner)
			if leaseErr != nil {
				if errors.Is(leaseErr, errLeaseCandidateGone) {
					continue
				}
				return ForkAssignment{}, leaseErr
			}
			if lease == nil {
				busy.add(observation)
				continue
			}
			lock, err := lockTaskOwner(candidate.Root, candidate.Item.ID)
			if err != nil {
				return ForkAssignment{}, errors.Join(err, lease.Release())
			}
			record, ok, err := lock.Read()
			if err != nil {
				_ = lock.Close()
				return ForkAssignment{}, errors.Join(err, lease.Release())
			}
			current, currentOK, currentErr := CurrentTask(candidate.Root, candidate.Item.ID)
			if currentErr != nil {
				_ = lock.Close()
				return ForkAssignment{}, errors.Join(currentErr, lease.Release())
			}
			if !currentOK || current.State != candidate.Item.State {
				_ = lock.Close()
				_ = lease.Release()
				continue
			}
			if ok {
				if record.Kind != TaskOwnerFork || record.Fork.Fork != request.Fork ||
					(record.Fork.Phase != ForkAssignmentPreparing && record.Fork.Phase != ForkAssignmentWorking &&
						record.Fork.Phase != ForkAssignmentPaused) {
					foreign = true
					_ = lock.Close()
					_ = lease.Release()
					continue
				}
				owner := *record.Fork
				// The owner intent is durable before the folder move. A crash in that window leaves
				// an exact owned todo task; resuming the same generation completes the move while
				// holding both the canonical lease and owner lock. Paused tasks follow the same path
				// after an explicit unblock.
				if current.State == StateTodo {
					if err := MoveTaskDir(candidate.Root, current, StateInProgress); err != nil {
						_ = lock.Close()
						return ForkAssignment{}, errors.Join(err, lease.Release())
					}
					current.State = StateInProgress
					current.Dir = filepath.Join(candidate.Root, StateInProgress, current.ID)
				}
				index := ForkAssignmentIndex{
					Version: forkAssignmentIndexVersion, Fork: owner.Fork, AssignmentID: owner.AssignmentID,
					CanonicalRoot: candidate.Root, Task: *record.Task, Projection: owner.Projection,
					BaselineHead: owner.BaselineHead, CreatedAt: owner.AssignedAt,
				}
				if err := RegisterForkAssignmentIndex(request.AuthorityRepo, index); err != nil {
					_ = lock.Close()
					return ForkAssignment{}, errors.Join(err, lease.Release())
				}
				if err := lock.Close(); err != nil {
					return ForkAssignment{}, errors.Join(err, lease.Release())
				}
				selected := ForkAssignment{AuthorityRepo: request.AuthorityRepo, Task: QueuedTask{Root: candidate.Root, Item: current},
					Owner: owner, Lease: lease, Outcome: ForkAssignmentSelected}
				if err := MaterializeForkProjection(selected); err != nil {
					return ForkAssignment{}, errors.Join(err, lease.Release())
				}
				return selected, nil
			}

			instance, err := EnsureTaskInstance(candidate.Root, current)
			if err != nil {
				_ = lock.Close()
				return ForkAssignment{}, errors.Join(err, lease.Release())
			}
			assignmentID := newAssignmentID()
			now := time.Now().UTC()
			owner := ForkTaskOwner{
				Fork: request.Fork, AssignmentID: assignmentID, Phase: ForkAssignmentPreparing,
				Projection:   projectionPath(request.WorkspaceRoot, request.Fork, assignmentID),
				BaselineHead: request.BaselineHead, AssignedAt: now, UpdatedAt: now,
			}
			newRecord := TaskOwnerRecord{
				Version: taskOwnershipRecordVersion, TaskID: current.ID, Kind: TaskOwnerFork,
				Task: &instance, Fork: &owner,
			}
			index := ForkAssignmentIndex{
				Version: forkAssignmentIndexVersion, Fork: owner.Fork, AssignmentID: owner.AssignmentID,
				CanonicalRoot: candidate.Root, Task: instance, Projection: owner.Projection,
				BaselineHead: owner.BaselineHead, CreatedAt: owner.AssignedAt,
			}
			if err := RegisterForkAssignmentIndex(request.AuthorityRepo, index); err != nil {
				_ = lock.Close()
				return ForkAssignment{}, errors.Join(err, lease.Release())
			}
			if err := lock.Write(newRecord); err != nil {
				_ = RemoveForkAssignmentIndex(request.AuthorityRepo, index)
				_ = lock.Close()
				return ForkAssignment{}, errors.Join(err, lease.Release())
			}
			if current.State == StateTodo {
				if err := MoveTaskDir(candidate.Root, current, StateInProgress); err != nil {
					rollbackErr := removeTaskOwnerRecordFile(candidate.Root, current.ID)
					rollbackErr = errors.Join(rollbackErr, RemoveForkAssignmentIndex(request.AuthorityRepo, index))
					_ = lock.Close()
					return ForkAssignment{}, errors.Join(err, rollbackErr, lease.Release())
				}
				current.State = StateInProgress
				current.Dir = filepath.Join(candidate.Root, StateInProgress, current.ID)
			}
			if err := lock.Close(); err != nil {
				return ForkAssignment{}, errors.Join(err, lease.Release())
			}
			selected := ForkAssignment{AuthorityRepo: request.AuthorityRepo, Task: QueuedTask{Root: candidate.Root, Item: current},
				Owner: owner, Lease: lease, Outcome: ForkAssignmentSelected}
			if err := MaterializeForkProjection(selected); err != nil {
				return ForkAssignment{}, errors.Join(err, lease.Release())
			}
			return selected, nil
		}
		if busy.Busy+busy.Stalled > 0 {
			return ForkAssignment{Outcome: ForkAssignmentUnavailable}, nil
		}
		if frozen || len(unowned) == 0 || foreign {
			return ForkAssignment{Outcome: ForkAssignmentExecutorDrained}, nil
		}
	}
	return ForkAssignment{Outcome: ForkAssignmentUnavailable}, nil
}

// recoverForkBlockingAssignmentsLocked finishes both sides of the blocked transition. The owner
// enters blocking before the canonical folder move, so either crash boundary is replayable without
// treating a blocked task as available work. The working+blocked case migrates the old write order
// that could crash after the move but before its owner update.
func recoverForkBlockingAssignmentsLocked(repo string, identity forkspace.Identity) error {
	assignments, err := ForkAssignments(repo, identity)
	if err != nil {
		return err
	}
	for _, assignment := range assignments {
		owner := *assignment.Record.Fork
		recoverable := owner.Phase == ForkAssignmentBlocking ||
			assignment.Item.State == StateBlocked &&
				(owner.Phase == ForkAssignmentPreparing || owner.Phase == ForkAssignmentWorking)
		if !recoverable {
			continue
		}
		if _, err := acceptForkProjectionLocked(repo, assignment.Root, assignment.Item.ID, owner); err != nil {
			return fmt.Errorf("recover blocked fork assignment %s: %w", owner.AssignmentID, err)
		}
	}
	return nil
}

type LocatedForkAssignment struct {
	Root   string
	Item   Item
	Record TaskOwnerRecord
	Index  ForkAssignmentIndex
}

func ForkAssignments(repo string, identity forkspace.Identity) ([]LocatedForkAssignment, error) {
	return forkAssignmentsIn(repo, identity, map[string][]Item{})
}

// ForkAssignments is the package ForkAssignments over the queues this snapshot already read, so a
// listing of every fork reads each queue once, not once per fork. A queue the snapshot could not
// read, or did not read, is read here.
func (s ProjectSnapshot) ForkAssignments(identity forkspace.Identity) ([]LocatedForkAssignment, error) {
	trees := map[string][]Item{}
	for _, queue := range s.Queues {
		if queue.Error == "" {
			trees[queue.Root] = nil
		}
	}
	for _, task := range s.Tasks {
		if items, read := trees[task.Queue]; read {
			trees[task.Queue] = append(items, task.Item)
		}
	}
	return forkAssignmentsIn(s.Repository, identity, trees)
}

// forkAssignmentsIn reads each canonical queue at most once for the whole call, not once per
// assignment and again per owner record: these trees hold every archived task. trees holds the
// queues already read and gains the ones read here.
func forkAssignmentsIn(repo string, identity forkspace.Identity, trees map[string][]Item) ([]LocatedForkAssignment, error) {
	indexes, problems := IndexedForkAssignments(repo, identity)
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	var out []LocatedForkAssignment
	for _, index := range indexes {
		items, read := trees[index.CanonicalRoot]
		if !read {
			var err error
			if items, err = ReadTaskTree(index.CanonicalRoot); err != nil {
				return nil, err
			}
			trees[index.CanonicalRoot] = items
		}
		i := slices.IndexFunc(items, func(item Item) bool { return item.ID == index.Task.Ref.ID })
		if i < 0 {
			return nil, fmt.Errorf("indexed fork assignment %s lost canonical task %s", index.AssignmentID, index.Task.Ref.ID)
		}
		item := items[i]
		instance, err := ReadTaskInstance(index.CanonicalRoot, item)
		if err != nil || !sameTaskInstance(index.Task, instance) {
			return nil, errors.Join(err, fmt.Errorf("indexed fork assignment %s names a replaced task", index.AssignmentID))
		}
		record, owned, err := readTaskOwnerRecordOf(index.CanonicalRoot, item)
		if err != nil {
			return nil, err
		}
		if !owned || record.Kind != TaskOwnerFork || record.Fork == nil ||
			record.Fork.Fork != identity || record.Fork.AssignmentID != index.AssignmentID {
			return nil, fmt.Errorf("indexed fork assignment %s has no matching canonical owner", index.AssignmentID)
		}
		out = append(out, LocatedForkAssignment{Root: index.CanonicalRoot, Item: item, Record: record, Index: index})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Root != out[j].Root {
			return out[i].Root < out[j].Root
		}
		return out[i].Item.ID < out[j].Item.ID
	})
	return out, nil
}

// PauseForkAssignments is how coop fork stop leaves the fork's assignments: those at work are paused,
// for the fork's next pass to resume. The caller holds the fork's lifecycle lock and has stopped the
// worker. A task whose copy that worker had just parked on a question is accepted as blocked instead,
// as the worker would have accepted it had its acceptance not waited on the stop's lock: a paused
// copy keeps the question in the fork, where coop tasks decisions does not show it, and the next pass
// would restore the copy as if the project had answered it.
func PauseForkAssignments(repo string, identity forkspace.Identity) error {
	assignments, err := ForkAssignments(repo, identity)
	if err != nil {
		return err
	}
	workspace := forkspace.ValidateGenerationWorkspace(repo, identity) == nil
	var joined error
	for _, assignment := range assignments {
		owner := *assignment.Record.Fork
		if owner.Phase != ForkAssignmentWorking && owner.Phase != ForkAssignmentPreparing && owner.Phase != ForkAssignmentPaused {
			continue
		}
		if workspace {
			// A copy that cannot be read is paused, as before: only a parked one has a question to carry.
			if parked, _ := forkCopyParked(repo, assignment.Root, assignment.Item, owner); parked {
				_, acceptErr := acceptForkProjectionLocked(repo, assignment.Root, assignment.Item.ID, owner)
				joined = errors.Join(joined, acceptErr)
				continue
			}
		}
		if owner.Phase == ForkAssignmentPaused {
			continue
		}
		_, updateErr := UpdateForkTaskAssignment(assignment.Root, assignment.Item.ID, owner, func(next *ForkTaskOwner) error {
			next.Phase = ForkAssignmentPaused
			return nil
		})
		joined = errors.Join(joined, updateErr)
	}
	return joined
}

// recoverForkAssignmentIndexes completes the only harmless partial registry transition: an index
// published before its canonical owner, while the exact task still remains todo. Every later state
// is ambiguous and therefore fails closed for operator recovery.
func recoverForkAssignmentIndexes(repo string, identity forkspace.Identity) error {
	indexes, problems := IndexedForkAssignments(repo, identity)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	for _, index := range indexes {
		item, ok, err := CurrentTask(index.CanonicalRoot, index.Task.Ref.ID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("recover assignment %s: canonical task is missing", index.AssignmentID)
		}
		instance, err := ReadTaskInstance(index.CanonicalRoot, item)
		if err != nil || !sameTaskInstance(index.Task, instance) {
			return errors.Join(err, fmt.Errorf("recover assignment %s: canonical task was replaced", index.AssignmentID))
		}
		record, owned, err := ReadTaskOwnerRecord(index.CanonicalRoot, item.ID)
		if err != nil {
			return err
		}
		if owned {
			if record.Kind != TaskOwnerFork || record.Fork == nil || record.Fork.Fork != identity ||
				record.Fork.AssignmentID != index.AssignmentID {
				return fmt.Errorf("recover assignment %s: canonical owner changed", index.AssignmentID)
			}
			continue
		}
		if item.State != StateTodo {
			return fmt.Errorf("recover assignment %s: owner is missing after canonical state changed", index.AssignmentID)
		}
		if err := RemoveForkAssignmentIndex(repo, index); err != nil {
			return err
		}
	}
	return nil
}
