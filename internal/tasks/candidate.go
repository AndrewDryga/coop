package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const (
	forkCandidateVersion = 1
	forkCandidateLimit   = 4 << 20
)

type ForkCandidateAssignment struct {
	Index            ForkAssignmentIndex `json:"assignment"`
	ProjectionDigest string              `json:"projection_digest"`
}

// ForkCandidate freezes one reviewed generation-level snapshot. A fork can complete several
// tasks and signing can rewrite their commits; publishing only after final signoff binds every
// assignment to one final HEAD/tree instead of leaving a partially landable per-task set.
type ForkCandidate struct {
	Version     int                       `json:"version"`
	ID          string                    `json:"id"`
	Fork        forkspace.Identity        `json:"fork"`
	Head        string                    `json:"head"`
	Tree        string                    `json:"tree"`
	Assignments []ForkCandidateAssignment `json:"assignments"`
	CreatedAt   time.Time                 `json:"created_at"`
}

func ForkCandidatePath(repo string, identity forkspace.Identity) string {
	return filepath.Join(forkspace.StateDir(repo), identity.Name+"."+string(identity.Generation)+".candidate.json")
}

func validateForkCandidate(candidate ForkCandidate) error {
	if candidate.Version != forkCandidateVersion || !validAssignmentID(candidate.ID) ||
		!forkspace.ValidExistingName(candidate.Fork.Name) || !forkspace.ValidGeneration(candidate.Fork.Generation) ||
		!validCommitID(candidate.Head) || candidate.Head == "" || !validCommitID(candidate.Tree) || candidate.Tree == "" ||
		len(candidate.Assignments) == 0 || candidate.CreatedAt.IsZero() {
		return errors.New("invalid fork candidate")
	}
	seen := make(map[string]bool, len(candidate.Assignments))
	for _, assignment := range candidate.Assignments {
		if err := validateForkAssignmentIndex(assignment.Index); err != nil || assignment.Index.Fork != candidate.Fork ||
			!validDigest(assignment.ProjectionDigest) || assignment.ProjectionDigest == "" ||
			seen[assignment.Index.AssignmentID] {
			return errors.New("invalid fork candidate assignment")
		}
		seen[assignment.Index.AssignmentID] = true
	}
	return nil
}

// ValidateForkCandidateRecord is the strict wire validator shared by the host land journal.
func ValidateForkCandidateRecord(candidate ForkCandidate) error {
	return validateForkCandidate(candidate)
}

func decodeForkCandidate(data []byte) (ForkCandidate, error) {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), forkCandidateLimit+1))
	dec.DisallowUnknownFields()
	var candidate ForkCandidate
	if err := dec.Decode(&candidate); err != nil {
		return ForkCandidate{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ForkCandidate{}, errors.New("fork candidate contains multiple JSON values")
		}
		return ForkCandidate{}, err
	}
	if err := validateForkCandidate(candidate); err != nil {
		return ForkCandidate{}, err
	}
	return candidate, nil
}

func ReadForkCandidate(repo string, identity forkspace.Identity) (ForkCandidate, bool, error) {
	path := ForkCandidatePath(repo, identity)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ForkCandidate{}, false, nil
	}
	if err != nil {
		return ForkCandidate{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > forkCandidateLimit {
		return ForkCandidate{}, false, errors.New("fork candidate is not a bounded single-link regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return ForkCandidate{}, false, err
		}
		return ForkCandidate{}, false, errors.New("fork candidate changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, forkCandidateLimit+1))
	if err != nil || len(data) > forkCandidateLimit {
		if err != nil {
			return ForkCandidate{}, false, err
		}
		return ForkCandidate{}, false, errors.New("fork candidate exceeds its size limit")
	}
	candidate, err := decodeForkCandidate(data)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	if candidate.Fork != identity {
		return ForkCandidate{}, false, errors.New("fork candidate belongs to another generation")
	}
	return candidate, true, nil
}

func writeForkCandidate(repo string, candidate ForkCandidate) error {
	if err := validateForkCandidate(candidate); err != nil {
		return err
	}
	if err := ensureRealDirectory(forkspace.Home(repo), 0o755); err != nil {
		return err
	}
	if err := ensureRealDirectory(forkspace.StateDir(repo), 0o755); err != nil {
		return err
	}
	if _, ok, err := ReadForkCandidate(repo, candidate.Fork); err != nil {
		return err
	} else if ok {
		return errors.New("fork candidate already exists")
	}
	body, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	path := ForkCandidatePath(repo, candidate.Fork)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, err := f.Write(append(body, '\n')); err != nil {
		writeErr = err
	} else {
		writeErr = f.Sync()
	}
	if err := f.Close(); err != nil {
		writeErr = errors.Join(writeErr, err)
	}
	if writeErr != nil {
		_ = os.Remove(path)
	}
	return writeErr
}

func candidateAssignments(repo string, identity forkspace.Identity) ([]ForkCandidateAssignment, error) {
	if err := forkProposalsDrained(repo, identity); err != nil {
		return nil, err
	}
	assignments, err := ForkAssignments(repo, identity)
	if err != nil {
		return nil, err
	}
	out := make([]ForkCandidateAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		owner := assignment.Record.Fork
		if owner.Phase != ForkAssignmentReviewing && owner.Phase != ForkAssignmentReady {
			return nil, fmt.Errorf("task %s is %s, not reviewed for candidate publication", assignment.Item.ID, owner.Phase)
		}
		result, err := ValidateForkProjection(repo, assignment.Root, assignment.Item.ID, *owner)
		if err != nil {
			return nil, err
		}
		if result.State != StateDone || result.Digest != owner.ProjectionDigest {
			return nil, fmt.Errorf("task %s projection changed after review", assignment.Item.ID)
		}
		out = append(out, ForkCandidateAssignment{Index: assignment.Index, ProjectionDigest: result.Digest})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Index.AssignmentID < out[j].Index.AssignmentID })
	return out, nil
}

// ValidateForkCandidateLocked rechecks a published candidate while its caller holds the fork
// lifecycle lock. It binds current Git identity and every ready owner/projection to the immutable
// record before a land intent may be written.
func ValidateForkCandidateLocked(repo string, candidate ForkCandidate, head, tree string) error {
	if err := validateForkCandidate(candidate); err != nil {
		return err
	}
	if candidate.Head != head || candidate.Tree != tree {
		return errors.New("fork HEAD/tree changed after final review")
	}
	current, err := candidateAssignments(repo, candidate.Fork)
	if err != nil {
		return err
	}
	if len(current) != len(candidate.Assignments) {
		return errors.New("fork assignment set changed after final review")
	}
	for i := range current {
		if current[i] != candidate.Assignments[i] {
			return errors.New("fork projection changed after final review")
		}
		item, ok := CurrentTask(current[i].Index.CanonicalRoot, current[i].Index.Task.Ref.ID)
		if !ok {
			return errors.New("candidate task disappeared before landing")
		}
		record, owned, err := ReadTaskOwnerRecord(current[i].Index.CanonicalRoot, item.ID)
		if err != nil || !owned || record.Fork == nil || record.Fork.Phase != ForkAssignmentReady ||
			record.Fork.CandidateID != candidate.ID {
			return errors.Join(err, errors.New("candidate task is not ready for this exact candidate"))
		}
	}
	return nil
}

// PublishForkCandidate writes the immutable generation intent first and then makes each exact
// owner ready. Re-entry completes any partial owner updates left by a crash.
func PublishForkCandidate(repo string, identity forkspace.Identity, head, tree string) (ForkCandidate, bool, error) {
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	defer unlock()
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return ForkCandidate{}, false, err
	}
	assignments, err := candidateAssignments(repo, identity)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	if len(assignments) == 0 {
		return ForkCandidate{}, false, nil
	}
	candidate, exists, err := ReadForkCandidate(repo, identity)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	if exists {
		if candidate.Head != head || candidate.Tree != tree || len(candidate.Assignments) != len(assignments) {
			return ForkCandidate{}, false, errors.New("fork changed after its reviewed candidate was published; merge or discard it before more work")
		}
		for i := range assignments {
			if candidate.Assignments[i] != assignments[i] {
				return ForkCandidate{}, false, errors.New("fork task projection changed after candidate publication")
			}
		}
	} else {
		id, err := newAssignmentID()
		if err != nil {
			return ForkCandidate{}, false, err
		}
		candidate = ForkCandidate{
			Version: forkCandidateVersion, ID: id, Fork: identity, Head: head, Tree: tree,
			Assignments: assignments, CreatedAt: time.Now().UTC(),
		}
		if err := writeForkCandidate(repo, candidate); err != nil {
			return ForkCandidate{}, false, err
		}
	}
	for _, assignment := range candidate.Assignments {
		item, ok := CurrentTask(assignment.Index.CanonicalRoot, assignment.Index.Task.Ref.ID)
		if !ok {
			return ForkCandidate{}, false, errors.New("candidate task disappeared before ready publication")
		}
		record, owned, err := ReadTaskOwnerRecord(assignment.Index.CanonicalRoot, item.ID)
		if err != nil || !owned || record.Fork == nil {
			return ForkCandidate{}, false, errors.Join(err, errors.New("candidate task owner disappeared before ready publication"))
		}
		expected := *record.Fork
		if expected.Phase == ForkAssignmentReady {
			if expected.CandidateID != candidate.ID {
				return ForkCandidate{}, false, errors.New("candidate task is ready for another candidate")
			}
			continue
		}
		if expected.Phase != ForkAssignmentReviewing {
			return ForkCandidate{}, false, fmt.Errorf("candidate task %s changed to %s", item.ID, expected.Phase)
		}
		if _, err := UpdateForkTaskAssignment(assignment.Index.CanonicalRoot, item.ID, expected, func(owner *ForkTaskOwner) error {
			owner.Phase = ForkAssignmentReady
			owner.CandidateID = candidate.ID
			return nil
		}); err != nil {
			return ForkCandidate{}, false, err
		}
	}
	return candidate, true, nil
}

func RemoveForkCandidateIfMatchesLocked(repo string, expected ForkCandidate) error {
	current, ok, err := ReadForkCandidate(repo, expected.Fork)
	if err != nil || !ok {
		return err
	}
	if current.ID != expected.ID || current.Head != expected.Head || current.Tree != expected.Tree {
		return errors.New("fork candidate changed before removal")
	}
	if err := os.Remove(ForkCandidatePath(repo, expected.Fork)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FinalizeForkCandidateTask is the only canonical done transition for sandbox-owned work. The
// caller holds the fork lifecycle lock and has durably recorded a land intent whose Git commit is
// already (or is about to become) reachable from the parent. Every step is idempotent for replay.
func FinalizeForkCandidateTask(authorityRepo string, candidate ForkCandidate, assignment ForkCandidateAssignment) (retErr error) {
	if err := validateForkCandidate(candidate); err != nil {
		return err
	}
	if assignment.Index.Fork != candidate.Fork || assignment.ProjectionDigest == "" {
		return errors.New("candidate assignment does not belong to landing generation")
	}
	root, id := assignment.Index.CanonicalRoot, assignment.Index.Task.Ref.ID
	windows, err := BeginCompletionWindows([]string{root})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCompletionWindowSetup, err)
	}
	accepted := false
	var acceptedTask QueuedTask
	defer func() {
		if accepted {
			retErr = errors.Join(retErr, windows.rejectAndClose(acceptedTask))
		} else {
			retErr = errors.Join(retErr, windows.Abandon())
		}
	}()
	authority, err := lockLeaseAuthority(root, id, true, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, unlockLeaseFile(authority)) }()
	ownerLock, err := lockTaskOwner(root, id)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, ownerLock.Close()) }()
	current, ok := CurrentTask(root, id)
	if !ok {
		return errors.New("candidate canonical task disappeared before landing")
	}
	instance, err := ReadTaskInstance(root, current)
	if err != nil || !sameTaskInstance(instance, assignment.Index.Task) {
		return errors.Join(err, errors.New("candidate canonical task was replaced before landing"))
	}
	record, owned, err := ownerLock.Read()
	if err != nil {
		return err
	}
	if current.State != StateDone {
		if !owned || record.Kind != TaskOwnerFork || record.Fork == nil ||
			record.Fork.Fork != candidate.Fork || record.Fork.AssignmentID != assignment.Index.AssignmentID ||
			record.Fork.Phase != ForkAssignmentReady || record.Fork.CandidateID != candidate.ID {
			return errors.New("canonical task is not ready for this exact fork candidate")
		}
	} else if owned && (record.Kind != TaskOwnerFork || record.Fork == nil ||
		record.Fork.Fork != candidate.Fork || record.Fork.AssignmentID != assignment.Index.AssignmentID ||
		record.Fork.CandidateID != candidate.ID) {
		return errors.New("landed task owner changed before replay")
	}
	expected := ForkTaskOwner{
		Fork: candidate.Fork, AssignmentID: assignment.Index.AssignmentID,
		Projection: assignment.Index.Projection,
	}
	projection, err := openAssignedProjection(authorityRepo, expected)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, projection.Close()) }()
	manifest, err := readProjectionManifest(projection)
	if err != nil || manifest.Task != assignment.Index.Task {
		return errors.Join(err, errors.New("candidate projection identity changed before landing"))
	}
	result, snapshot, err := snapshotForkProjection(authorityRepo, root, id, expected, projection)
	if err != nil {
		return err
	}
	defer os.RemoveAll(filepath.Dir(snapshot))
	if result.State != StateDone || result.Digest != assignment.ProjectionDigest {
		return errors.New("candidate projection bytes changed before landing")
	}
	if err := syncProjectionTree(snapshot, current.Dir, true); err != nil {
		return err
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		return err
	}
	if current.State != StateDone {
		if err := MoveTaskDir(root, current, StateDone); err != nil {
			return err
		}
		current.State = StateDone
		current.Dir = filepath.Join(root, StateDone, id)
	}
	if err := finalizeCompletedTask(id, current.Dir); err != nil {
		return err
	}
	if err := writeLeaseCompletionReceipt(authority, current.Dir); err != nil {
		return err
	}
	if owned {
		if err := removeTaskOwnerRecordFile(root, id); err != nil {
			return err
		}
	}
	if err := finalizeForkProposalRecords(authorityRepo, assignment.Index); err != nil {
		return err
	}
	if err := RemoveForkAssignmentIndex(authorityRepo, assignment.Index); err != nil {
		return err
	}
	acceptedTask = QueuedTask{Root: root, Item: current}
	accepted = true
	return nil
}
