package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const projectSnapshotVersion = 1

type ProjectQueueSnapshot struct {
	Root   string     `json:"root"`
	Label  string     `json:"label"`
	ID     string     `json:"queue_id,omitempty"`
	Counts TaskCounts `json:"counts"`
}

type ProjectTaskSnapshot struct {
	Queue        string                           `json:"queue"`
	QueueLabel   string                           `json:"queue_label"`
	QueueID      string                           `json:"queue_id,omitempty"`
	ID           string                           `json:"id"`
	Title        string                           `json:"title"`
	State        string                           `json:"state"`
	Path         string                           `json:"path"`
	Task         *TaskInstance                    `json:"task,omitempty"`
	Owner        string                           `json:"owner,omitempty"`
	Fork         *forkspace.Identity              `json:"fork,omitempty"`
	AssignmentID string                           `json:"assignment_id,omitempty"`
	Phase        ForkAssignmentPhase              `json:"phase,omitempty"`
	Executions   []forkspace.ExecutionObservation `json:"executions,omitempty"`
	Item         Item                             `json:"-"`
}

type ProjectForkSnapshot struct {
	Name             string                           `json:"name"`
	Identity         *forkspace.Identity              `json:"identity,omitempty"`
	WorkspaceValid   bool                             `json:"workspace_valid"`
	Starting         bool                             `json:"starting"`
	DetachedRunning  bool                             `json:"detached_running"`
	CleanupPending   bool                             `json:"cleanup_pending"`
	Assignments      int                              `json:"assignments"`
	Candidate        bool                             `json:"candidate"`
	PendingLand      bool                             `json:"pending_land"`
	Reservation      *forkspace.WorkspaceReservation  `json:"reservation,omitempty"`
	Executions       []forkspace.ExecutionObservation `json:"executions,omitempty"`
	ActiveExecutions int                              `json:"active_executions"`
}

type ProjectSnapshot struct {
	Version     int                              `json:"version"`
	Repository  string                           `json:"repository"`
	GeneratedAt time.Time                        `json:"generated_at"`
	Queues      []ProjectQueueSnapshot           `json:"queues"`
	Tasks       []ProjectTaskSnapshot            `json:"tasks"`
	Executions  []forkspace.ExecutionObservation `json:"executions"`
	Forks       []ProjectForkSnapshot            `json:"forks"`
	Problems    []string                         `json:"problems,omitempty"`
}

func snapshotTaskKey(queueID, taskID, root, id string) string {
	if queueID != "" && taskID != "" {
		return queueID + "\x00" + taskID
	}
	return filepath.Clean(root) + "\x00" + id
}

func appendSnapshotProblem(snapshot *ProjectSnapshot, scope string, err error) {
	if err != nil {
		snapshot.Problems = append(snapshot.Problems, scope+": "+err.Error())
	}
}

func queueSnapshotLabel(repo, root string) string {
	label, err := filepath.Rel(repo, root)
	if err != nil || label == "." || label == ".." || strings.HasPrefix(label, ".."+string(filepath.Separator)) {
		return root
	}
	return label
}

// ReadProjectSnapshot is the one read model for the canonical queue and every host-observed
// sandbox. It never reads a fork's copied task tree: one assigned task is projected for execution,
// while canonical folders and host-side records remain the lifecycle authority.
func ReadProjectSnapshot(repo string, roots []string) ProjectSnapshot {
	repo = filepath.Clean(repo)
	snapshot := ProjectSnapshot{Version: projectSnapshotVersion, Repository: repo, GeneratedAt: time.Now().UTC()}

	indexes, indexProblems := AllIndexedForkAssignments(repo)
	for _, problem := range indexProblems {
		appendSnapshotProblem(&snapshot, "assignment registry", problem)
	}
	indexByID := make(map[string]ForkAssignmentIndex, len(indexes))
	identitySet := map[forkspace.Identity]bool{}
	rootSet := map[string]bool{}
	for _, root := range roots {
		if root != "" {
			rootSet[filepath.Clean(root)] = true
		}
	}
	for _, index := range indexes {
		if previous, exists := indexByID[index.AssignmentID]; exists && previous != index {
			appendSnapshotProblem(&snapshot, "assignment "+index.AssignmentID, errors.New("identity collision across generation indexes"))
			continue
		}
		indexByID[index.AssignmentID] = index
		identitySet[index.Fork] = true
		rootSet[index.CanonicalRoot] = true
	}

	observations, executionProblems := forkspace.Executions(repo)
	snapshot.Executions = observations
	for _, problem := range executionProblems {
		appendSnapshotProblem(&snapshot, "execution registry", problem)
	}
	for _, observation := range observations {
		if observation.Record.Fork != nil {
			identitySet[*observation.Record.Fork] = true
		}
	}

	reservations, reservationProblems := forkspace.WorkspaceReservations(repo)
	reservationByFork := make(map[forkspace.Identity]forkspace.WorkspaceReservation, len(reservations))
	for _, problem := range reservationProblems {
		appendSnapshotProblem(&snapshot, "workspace reservation", problem)
	}
	for _, reservation := range reservations {
		identitySet[reservation.Fork] = true
		reservationByFork[reservation.Fork] = reservation
	}

	generations, generationProblems := forkspace.GenerationIdentities(repo)
	currentGeneration := make(map[string]forkspace.Identity, len(generations))
	for _, problem := range generationProblems {
		appendSnapshotProblem(&snapshot, "fork generation", problem)
	}
	for _, identity := range generations {
		identitySet[identity] = true
		currentGeneration[identity.Name] = identity
	}

	queueRoots := make([]string, 0, len(rootSet))
	for root := range rootSet {
		queueRoots = append(queueRoots, root)
	}
	sort.Strings(queueRoots)
	taskIndexesByDurableKey := map[string][]int{}
	taskIndexesByReadableID := map[string][]int{}
	for _, root := range queueRoots {
		items, err := ReadTaskTree(root)
		if err != nil {
			appendSnapshotProblem(&snapshot, "queue "+root, err)
		}
		counts, _ := TaskTreeCounts(items)
		label := queueSnapshotLabel(repo, root)
		queueID := ""
		if identity, identityErr := readQueueIdentity(root); identityErr == nil {
			queueID = identity.ID
		} else if !errors.Is(identityErr, os.ErrNotExist) {
			appendSnapshotProblem(&snapshot, "queue "+root+" identity", identityErr)
		}
		snapshot.Queues = append(snapshot.Queues, ProjectQueueSnapshot{Root: root, Label: label, ID: queueID, Counts: counts})
		for _, item := range items {
			view := ProjectTaskSnapshot{
				Queue: root, QueueLabel: label, QueueID: queueID,
				ID: item.ID, Title: item.Title, State: item.State, Path: item.Dir, Item: item,
			}
			key := snapshotTaskKey("", "", root, item.ID)
			if instance, err := ReadTaskInstance(root, item); err == nil {
				view.Task = &instance
				view.QueueID = instance.Ref.QueueID
				key = snapshotTaskKey(instance.Ref.QueueID, instance.Ref.TaskID, root, item.ID)
			} else if owner, owned, ownerErr := ReadTaskOwnerRecord(root, item.ID); ownerErr == nil && owned && owner.Task != nil {
				instance := *owner.Task
				view.Task = &instance
				view.QueueID = instance.Ref.QueueID
				key = snapshotTaskKey(instance.Ref.QueueID, instance.Ref.TaskID, root, item.ID)
			}
			owner, owned, ownerErr := ReadTaskOwnerRecord(root, item.ID)
			if ownerErr != nil {
				appendSnapshotProblem(&snapshot, "task "+item.ID+" owner", ownerErr)
			} else if owned {
				view.Owner = TaskOwnerLabel(owner)
				if owner.Fork != nil {
					index, indexed := indexByID[owner.Fork.AssignmentID]
					exact := indexed && index.Fork == owner.Fork.Fork && index.CanonicalRoot == root &&
						index.Task.Ref.ID == item.ID && owner.Task != nil && sameTaskInstance(index.Task, *owner.Task)
					if !exact {
						appendSnapshotProblem(&snapshot, "task "+item.ID+" assignment", errors.New("owner does not match its host reverse index"))
					} else {
						fork := owner.Fork.Fork
						view.Fork = &fork
						view.AssignmentID = owner.Fork.AssignmentID
						view.Phase = owner.Fork.Phase
					}
				}
			}
			position := len(snapshot.Tasks)
			snapshot.Tasks = append(snapshot.Tasks, view)
			taskIndexesByDurableKey[key] = append(taskIndexesByDurableKey[key], position)
			taskIndexesByReadableID[item.ID] = append(taskIndexesByReadableID[item.ID], position)
		}
	}

	for _, observation := range observations {
		ref := observation.Record.Task
		if ref == nil {
			continue
		}
		var candidates []int
		if ref.QueueID != "" && ref.TaskID != "" {
			candidates = taskIndexesByDurableKey[snapshotTaskKey(ref.QueueID, ref.TaskID, "", ref.ID)]
		} else if observation.Record.Fork == nil {
			candidates = taskIndexesByReadableID[ref.ID]
		}
		if len(candidates) != 1 {
			appendSnapshotProblem(&snapshot, "execution "+observation.Record.ID, errors.New("task binding is missing or ambiguous"))
			continue
		}
		view := &snapshot.Tasks[candidates[0]]
		if observation.Record.Fork != nil &&
			(view.Fork == nil || *view.Fork != *observation.Record.Fork || ref.Assignment == "" || ref.Assignment != view.AssignmentID) {
			appendSnapshotProblem(&snapshot, "execution "+observation.Record.ID, errors.New("fork task binding does not match canonical assignment"))
			continue
		}
		if observation.Record.Fork == nil && view.Fork != nil {
			appendSnapshotProblem(&snapshot, "execution "+observation.Record.ID, errors.New("local execution cannot claim a fork-owned task"))
			continue
		}
		view.Executions = append(view.Executions, observation)
	}

	nameSet := map[string]bool{}
	lifecycleNames, err := forkspace.LifecycleNames(repo)
	if err != nil {
		appendSnapshotProblem(&snapshot, "fork discovery", err)
	}
	for _, name := range lifecycleNames {
		nameSet[name] = true
	}
	for identity := range identitySet {
		nameSet[identity.Name] = true
	}
	identities := make([]forkspace.Identity, 0, len(identitySet))
	for identity := range identitySet {
		identities = append(identities, identity)
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Name != identities[j].Name {
			return identities[i].Name < identities[j].Name
		}
		return identities[i].Generation < identities[j].Generation
	})
	for _, identity := range identities {
		identityCopy := identity
		fork := ProjectForkSnapshot{Name: identity.Name, Identity: &identityCopy}
		if current, ok := currentGeneration[identity.Name]; ok && current == identity {
			if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
				appendSnapshotProblem(&snapshot, "fork "+identity.Name+" workspace", err)
			} else {
				fork.WorkspaceValid = true
			}
			state, stateErr := forkspace.ReadWorkerState(repo, identity.Name)
			if stateErr == nil {
				if state.Generation != "" && state.Generation != identity.Generation {
					appendSnapshotProblem(&snapshot, "fork "+identity.Name+" worker", errors.New("worker belongs to another generation"))
				} else {
					fork.Starting = state.Claim
					fork.DetachedRunning = forkspace.RunningPid(repo, identity.Name) != 0
					fork.CleanupPending = forkspace.NeedsStop(repo, identity.Name) && !fork.DetachedRunning && !fork.Starting
				}
			} else if !errors.Is(stateErr, os.ErrNotExist) {
				appendSnapshotProblem(&snapshot, "fork "+identity.Name+" worker", stateErr)
				fork.CleanupPending = true
			}
		}
		for _, index := range indexes {
			if index.Fork == identity {
				fork.Assignments++
			}
		}
		if candidate, ok, err := ReadForkCandidate(repo, identity); err != nil {
			appendSnapshotProblem(&snapshot, "fork "+identity.Name+" candidate", err)
		} else if ok {
			fork.Candidate = candidate.ID != ""
		}
		if info, err := os.Lstat(forkspace.LandIntentPath(repo, identity)); err == nil {
			fork.PendingLand = true
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				appendSnapshotProblem(&snapshot, "fork "+identity.Name+" land", errors.New("land journal is not a regular file"))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			appendSnapshotProblem(&snapshot, "fork "+identity.Name+" land", err)
		}
		if reservation, ok := reservationByFork[identity]; ok {
			copy := reservation
			fork.Reservation = &copy
		}
		for _, observation := range observations {
			if observation.Record.Fork != nil && *observation.Record.Fork == identity {
				fork.Executions = append(fork.Executions, observation)
				if observation.Active {
					fork.ActiveExecutions++
				}
			}
		}
		snapshot.Forks = append(snapshot.Forks, fork)
		delete(nameSet, identity.Name)
	}
	for name := range nameSet {
		fork := ProjectForkSnapshot{Name: name, DetachedRunning: forkspace.RunningPid(repo, name) != 0}
		fork.CleanupPending = forkspace.NeedsStop(repo, name) && !fork.DetachedRunning
		if _, err := forkspace.ReadWorkerState(repo, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			appendSnapshotProblem(&snapshot, "fork "+name+" worker", err)
		}
		snapshot.Forks = append(snapshot.Forks, fork)
	}

	sort.Slice(snapshot.Tasks, func(i, j int) bool {
		if snapshot.Tasks[i].Queue != snapshot.Tasks[j].Queue {
			return snapshot.Tasks[i].Queue < snapshot.Tasks[j].Queue
		}
		return snapshot.Tasks[i].ID < snapshot.Tasks[j].ID
	})
	sort.Slice(snapshot.Forks, func(i, j int) bool {
		if snapshot.Forks[i].Name != snapshot.Forks[j].Name {
			return snapshot.Forks[i].Name < snapshot.Forks[j].Name
		}
		if snapshot.Forks[i].Identity == nil {
			return true
		}
		if snapshot.Forks[j].Identity == nil {
			return false
		}
		return snapshot.Forks[i].Identity.Generation < snapshot.Forks[j].Identity.Generation
	})
	sort.Strings(snapshot.Problems)
	snapshot.Problems = compactSnapshotProblems(snapshot.Problems)
	return snapshot
}

func compactSnapshotProblems(problems []string) []string {
	if len(problems) < 2 {
		return problems
	}
	out := problems[:1]
	for _, problem := range problems[1:] {
		if problem != out[len(out)-1] {
			out = append(out, problem)
		}
	}
	return out
}

func (snapshot ProjectSnapshot) Counts() TaskCounts {
	var counts TaskCounts
	for _, queue := range snapshot.Queues {
		counts.Todo += queue.Counts.Todo
		counts.Doing += queue.Counts.Doing
		counts.Blocked += queue.Counts.Blocked
		counts.Done += queue.Counts.Done
	}
	return counts
}

func (snapshot ProjectSnapshot) ActiveExecutions() int {
	n := 0
	for _, execution := range snapshot.Executions {
		if execution.Active {
			n++
		}
	}
	return n
}

// RunningExecutions remains the compatibility name for the first snapshot shape. Warm and probe
// sandboxes are visible but deliberately do not keep a watcher live.
func (snapshot ProjectSnapshot) RunningExecutions() int { return snapshot.ActiveExecutions() }
