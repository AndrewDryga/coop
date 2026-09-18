package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists {
		return ForkCandidate{}, exists, err
	}
	if state.legacy != nil {
		return *state.legacy, true, nil
	}
	if state.manifest.Phase != forkCandidateActive {
		return ForkCandidate{}, false, nil
	}
	candidate, err := candidateFromManifest(repo, *state.manifest)
	return candidate, err == nil, err
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
		if err := RequireCompletedChecklist(result.Item); err != nil {
			return nil, err
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
		item, ok, err := CurrentTask(current[i].Index.CanonicalRoot, current[i].Index.Task.Ref.ID)
		if err != nil {
			return err
		}
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

// ForkCandidateReview names the exact frozen candidate-wide review cohort. Required is false only
// when the same snapshot is already reviewed or its reviewed publication is being replayed.
type ForkCandidateReview struct {
	Candidate     ForkCandidate
	Round         uint64
	PreviousRound uint64
	Required      bool
}

func newForkCandidate(identity forkspace.Identity, head, tree string, assignments []ForkCandidateAssignment) (ForkCandidate, error) {
	id, err := newAssignmentID()
	if err != nil {
		return ForkCandidate{}, err
	}
	return ForkCandidate{
		Version: forkCandidateVersion, ID: id, Fork: identity, Head: head, Tree: tree,
		Assignments: assignments, CreatedAt: time.Now().UTC(),
	}, nil
}

func requireNoForkLifecycleIntent(repo string, identity forkspace.Identity) error {
	if pending, err := pendingForkLandPath(repo, identity); err != nil {
		return err
	} else if pending {
		return unfinishedForkLandError(identity.Name)
	}
	if _, pending, err := readForkDiscard(repo, identity); err != nil {
		return err
	} else if pending {
		return fmt.Errorf("fork %s has an interrupted discard; finish it before candidate review", identity.Name)
	}
	return nil
}

func validateCandidateOwnersForReview(candidate ForkCandidate) error {
	for _, assignment := range candidate.Assignments {
		root, id := assignment.Index.CanonicalRoot, assignment.Index.Task.Ref.ID
		record, owned, err := ReadTaskOwnerRecord(root, id)
		if err != nil || !owned || record.Fork == nil || record.Fork.Fork != candidate.Fork ||
			record.Fork.AssignmentID != assignment.Index.AssignmentID {
			return errors.Join(err, fmt.Errorf("candidate task %s is no longer owned by this fork", id))
		}
		if record.Fork.Phase != ForkAssignmentReviewing || record.Fork.CandidateID != "" ||
			record.Fork.ProjectionDigest != assignment.ProjectionDigest {
			return fmt.Errorf("candidate task %s is not waiting for this exact review", id)
		}
	}
	return nil
}

// BeginForkCandidateReview freezes one exact HEAD/tree/assignment cohort before the reviewer is
// launched. Re-entry returns the same candidate ID. If inputs changed while a review was running,
// a new pending ID replaces only that unreviewed attempt; immutable reviewed rounds are untouched.
func BeginForkCandidateReview(repo string, identity forkspace.Identity, head, tree string) (ForkCandidateReview, bool, error) {
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		return ForkCandidateReview{}, false, err
	}
	defer unlock()
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return ForkCandidateReview{}, false, err
	}
	if err := requireNoForkLifecycleIntent(repo, identity); err != nil {
		return ForkCandidateReview{}, false, err
	}
	assignments, err := candidateAssignments(repo, identity)
	if err != nil {
		return ForkCandidateReview{}, false, err
	}
	if len(assignments) == 0 {
		return ForkCandidateReview{}, false, nil
	}
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil {
		return ForkCandidateReview{}, false, err
	}
	if exists && state.legacy != nil {
		manifest, err := adoptLegacyForkCandidateLocked(repo, identity, *state.legacy, forkCandidateActive)
		if err != nil {
			return ForkCandidateReview{}, false, err
		}
		state = forkCandidateState{manifest: &manifest}
	}
	if exists && state.manifest.Phase == forkCandidateActive {
		candidate, err := candidateFromManifest(repo, *state.manifest)
		if err != nil {
			return ForkCandidateReview{}, false, err
		}
		if !sameForkCandidateSnapshot(candidate, head, tree, assignments) {
			return ForkCandidateReview{}, false, errors.New("fork changed after its reviewed candidate was published; begin another review first")
		}
		return ForkCandidateReview{Candidate: candidate, Round: state.manifest.Current.Round}, true, nil
	}
	if exists && (state.manifest.Phase == forkCandidateLanded || state.manifest.Phase == forkCandidateDiscarded) {
		return ForkCandidateReview{}, false, fmt.Errorf("fork candidate history is %s and cannot be reused", state.manifest.Phase)
	}
	if exists && state.manifest.Phase == forkCandidateSuperseding {
		previous, err := candidateFromManifest(repo, *state.manifest)
		if err != nil {
			return ForkCandidateReview{}, false, err
		}
		if err := returnForkCandidateOwnersToReview(previous); err != nil {
			return ForkCandidateReview{}, false, err
		}
	}
	if exists && state.manifest.Phase == forkCandidatePublishing {
		candidate := *state.manifest.Pending
		if !sameForkCandidateSnapshot(candidate, head, tree, assignments) {
			return ForkCandidateReview{}, false, errors.New("fork changed after candidate review; another review is required")
		}
		return ForkCandidateReview{
			Candidate: candidate, Round: pendingRound(*state.manifest),
			PreviousRound: pendingRound(*state.manifest) - 1,
		}, true, nil
	}
	if exists && state.manifest.Phase == forkCandidateReviewing &&
		sameForkCandidateSnapshot(*state.manifest.Pending, head, tree, assignments) {
		candidate := *state.manifest.Pending
		if err := validateCandidateOwnersForReview(candidate); err != nil {
			return ForkCandidateReview{}, false, err
		}
		return ForkCandidateReview{
			Candidate: candidate, Round: pendingRound(*state.manifest),
			PreviousRound: pendingRound(*state.manifest) - 1, Required: true,
		}, true, nil
	}
	if exists && state.manifest.Current != nil {
		current, err := candidateFromManifest(repo, *state.manifest)
		if err != nil {
			return ForkCandidateReview{}, false, err
		}
		workspace := forkspace.Workspace(repo, identity.Name)
		if _, err := gitOutErr(workspace, "merge-base", "--is-ancestor", current.Head, head); err != nil {
			return ForkCandidateReview{}, false, errors.New("fork HEAD no longer descends from its reviewed candidate; merge or discard it before more work")
		}
	}
	candidate, err := newForkCandidate(identity, head, tree, assignments)
	if err != nil {
		return ForkCandidateReview{}, false, err
	}
	manifest := forkCandidateManifest{
		Version: forkCandidateManifestVersion, Fork: identity, Phase: forkCandidateReviewing,
		Pending: &candidate, UpdatedAt: time.Now().UTC(),
	}
	if exists {
		manifest.Current = state.manifest.Current
	}
	if err := validateCandidateOwnersForReview(candidate); err != nil {
		return ForkCandidateReview{}, false, err
	}
	if err := writeForkCandidateManifest(repo, manifest); err != nil {
		return ForkCandidateReview{}, false, err
	}
	round := pendingRound(manifest)
	return ForkCandidateReview{Candidate: candidate, Round: round, PreviousRound: round - 1, Required: true}, true, nil
}

// AuthorizeForkCandidateReview durably records a successful host-observed review. Candidate ID,
// Git identity, assignment set, projections and owner phases are all rechecked under the lifecycle
// lock; a pass for an earlier snapshot cannot authorize a later one.
func AuthorizeForkCandidateReview(repo string, identity forkspace.Identity, candidateID string) error {
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		return err
	}
	defer unlock()
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return err
	}
	if err := requireNoForkLifecycleIntent(repo, identity); err != nil {
		return err
	}
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists || state.manifest == nil {
		return errors.Join(err, errors.New("fork candidate review is no longer current"))
	}
	manifest := *state.manifest
	if manifest.Phase == forkCandidatePublishing && manifest.Pending.ID == candidateID {
		return nil
	}
	if manifest.Phase != forkCandidateReviewing || manifest.Pending.ID != candidateID {
		return errors.New("fork candidate review is no longer current")
	}
	workspace := forkspace.Workspace(repo, identity.Name)
	head, err := gitOutErr(workspace, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	tree, err := gitOutErr(workspace, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return err
	}
	assignments, err := candidateAssignments(repo, identity)
	if err != nil {
		return err
	}
	if !sameForkCandidateSnapshot(*manifest.Pending, head, tree, assignments) {
		return errors.New("fork changed during candidate review")
	}
	if err := validateCandidateOwnersForReview(*manifest.Pending); err != nil {
		return err
	}
	manifest.Phase, manifest.UpdatedAt = forkCandidatePublishing, time.Now().UTC()
	return writeForkCandidateManifest(repo, manifest)
}

// PublishForkCandidate consumes only a durable successful review (or replays an existing active
// candidate). It writes the immutable round before making exact owners ready, then publishes the
// active pointer last. Every partial boundary is idempotent.
func PublishForkCandidate(repo string, identity forkspace.Identity, head, tree string) (ForkCandidate, bool, error) {
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	defer unlock()
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return ForkCandidate{}, false, err
	}
	if err := requireNoForkLifecycleIntent(repo, identity); err != nil {
		return ForkCandidate{}, false, err
	}
	assignments, err := candidateAssignments(repo, identity)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	if len(assignments) == 0 {
		return ForkCandidate{}, false, nil
	}
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil {
		return ForkCandidate{}, false, err
	}
	if exists && state.legacy != nil {
		manifest, err := adoptLegacyForkCandidateLocked(repo, identity, *state.legacy, forkCandidateActive)
		if err != nil {
			return ForkCandidate{}, false, err
		}
		state = forkCandidateState{manifest: &manifest}
	}
	if !exists || state.manifest.Phase == forkCandidateReviewing || state.manifest.Phase == forkCandidateSuperseding {
		return ForkCandidate{}, false, errors.New("fork candidate requires a fresh final review before publication")
	}
	manifest := *state.manifest
	var candidate ForkCandidate
	switch manifest.Phase {
	case forkCandidateActive:
		candidate, err = candidateFromManifest(repo, manifest)
	case forkCandidatePublishing:
		candidate = *manifest.Pending
	case forkCandidateLanded, forkCandidateDiscarded:
		return ForkCandidate{}, false, fmt.Errorf("fork candidate history is %s and cannot be published", manifest.Phase)
	default:
		return ForkCandidate{}, false, errors.New("unsupported fork candidate publication state")
	}
	if err != nil {
		return ForkCandidate{}, false, err
	}
	if !sameForkCandidateSnapshot(candidate, head, tree, assignments) {
		return ForkCandidate{}, false, errors.New("fork changed after candidate review")
	}
	if manifest.Phase == forkCandidatePublishing {
		round := pendingRound(manifest)
		supersedes := ""
		if manifest.Current != nil {
			supersedes = manifest.Current.ID
		}
		if err := writeForkCandidateRound(repo, forkCandidateRoundRecord{
			Version: forkCandidateRoundVersion, Round: round, Candidate: candidate,
			Supersedes: supersedes, CreatedAt: candidate.CreatedAt,
		}); err != nil {
			return ForkCandidate{}, false, err
		}
	}
	for _, assignment := range candidate.Assignments {
		item, ok, err := CurrentTask(assignment.Index.CanonicalRoot, assignment.Index.Task.Ref.ID)
		if err != nil {
			return ForkCandidate{}, false, err
		}
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
		if expected.Phase != ForkAssignmentReviewing || expected.CandidateID != "" ||
			expected.AssignmentID != assignment.Index.AssignmentID || expected.ProjectionDigest != assignment.ProjectionDigest {
			return ForkCandidate{}, false, fmt.Errorf("candidate task %s changed after review", item.ID)
		}
		if _, err := UpdateForkTaskAssignment(assignment.Index.CanonicalRoot, item.ID, expected, func(owner *ForkTaskOwner) error {
			owner.Phase = ForkAssignmentReady
			owner.CandidateID = candidate.ID
			return nil
		}); err != nil {
			return ForkCandidate{}, false, err
		}
	}
	if manifest.Phase == forkCandidatePublishing {
		ref := forkCandidateRef{Round: pendingRound(manifest), ID: candidate.ID}
		manifest.Phase, manifest.Current, manifest.Pending, manifest.UpdatedAt =
			forkCandidateActive, &ref, nil, time.Now().UTC()
		if err := writeForkCandidateManifest(repo, manifest); err != nil {
			return ForkCandidate{}, false, err
		}
	}
	return candidate, true, nil
}

func sameForkCandidateSnapshot(candidate ForkCandidate, head, tree string, assignments []ForkCandidateAssignment) bool {
	if candidate.Head != head || candidate.Tree != tree || len(candidate.Assignments) != len(assignments) {
		return false
	}
	for i := range assignments {
		if candidate.Assignments[i] != assignments[i] {
			return false
		}
	}
	return true
}

func returnForkCandidateOwnersToReview(candidate ForkCandidate) error {
	for _, assignment := range candidate.Assignments {
		root, id := assignment.Index.CanonicalRoot, assignment.Index.Task.Ref.ID
		record, owned, err := ReadTaskOwnerRecord(root, id)
		if err != nil || !owned || record.Fork == nil || record.Fork.Fork != candidate.Fork ||
			record.Fork.AssignmentID != assignment.Index.AssignmentID ||
			record.Fork.ProjectionDigest != assignment.ProjectionDigest {
			return errors.Join(err, fmt.Errorf("candidate task %s is no longer owned by this fork", id))
		}
		expected := *record.Fork
		if expected.Phase == ForkAssignmentReviewing && expected.CandidateID == "" {
			continue
		}
		if expected.Phase != ForkAssignmentReady || expected.CandidateID != candidate.ID {
			return fmt.Errorf("candidate task %s is not ready for this exact candidate", id)
		}
		if _, err := UpdateForkTaskAssignment(root, id, expected, func(owner *ForkTaskOwner) error {
			owner.Phase = ForkAssignmentReviewing
			owner.CandidateID = ""
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// ForkCandidateSupersession describes the one visible transition begun by this call. Replays that
// merely finish partially returned owners report Started=false, so operators do not see duplicate
// round announcements.
type ForkCandidateSupersession struct {
	Started       bool
	PreviousRound uint64
	NextRound     uint64
}

func unfinishedForkLandError(name string) error {
	return fmt.Errorf("cannot start another review while %s has an unfinished merge; finish it: coop fork merge %s", name, name)
}

// SupersedeStaleForkCandidate retains the immutable reviewed round, journals that it is no longer
// landable, then returns its exact owners to reviewing. Descendant-only recovery remains the only
// automatic history transition, and any land journal fences it.
func SupersedeStaleForkCandidate(repo string, identity forkspace.Identity, head string) (ForkCandidateSupersession, error) {
	unlock, err := forkspace.LockState(repo, identity.Name)
	if err != nil {
		return ForkCandidateSupersession{}, err
	}
	defer unlock()
	if err := forkspace.ValidateGenerationWorkspace(repo, identity); err != nil {
		return ForkCandidateSupersession{}, err
	}
	if err := requireNoForkLifecycleIntent(repo, identity); err != nil {
		return ForkCandidateSupersession{}, err
	}
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists {
		return ForkCandidateSupersession{}, err
	}
	if state.legacy != nil {
		manifest, err := adoptLegacyForkCandidateLocked(repo, identity, *state.legacy, forkCandidateActive)
		if err != nil {
			return ForkCandidateSupersession{}, err
		}
		state = forkCandidateState{manifest: &manifest}
	}
	manifest := *state.manifest
	if manifest.Phase == forkCandidateLanded || manifest.Phase == forkCandidateDiscarded {
		return ForkCandidateSupersession{}, fmt.Errorf("fork candidate history is %s and cannot start another review", manifest.Phase)
	}
	if manifest.Phase == forkCandidateReviewing || manifest.Phase == forkCandidatePublishing {
		if manifest.Pending.Head == head {
			return ForkCandidateSupersession{}, nil
		}
		return ForkCandidateSupersession{}, errors.New("fork changed during candidate review")
	}
	candidate, err := candidateFromManifest(repo, manifest)
	if err != nil {
		return ForkCandidateSupersession{}, err
	}
	if manifest.Phase == forkCandidateActive && candidate.Head == head {
		return ForkCandidateSupersession{}, nil
	}
	workspace := forkspace.Workspace(repo, identity.Name)
	if _, err := gitOutErr(workspace, "merge-base", "--is-ancestor", candidate.Head, head); err != nil {
		return ForkCandidateSupersession{}, errors.New("fork HEAD no longer descends from its reviewed candidate; merge or discard it before more work")
	}
	transition := ForkCandidateSupersession{PreviousRound: manifest.Current.Round, NextRound: manifest.Current.Round + 1}
	if manifest.Phase == forkCandidateActive {
		manifest.Phase, manifest.UpdatedAt = forkCandidateSuperseding, time.Now().UTC()
		if err := writeForkCandidateManifest(repo, manifest); err != nil {
			return ForkCandidateSupersession{}, err
		}
		transition.Started = true
	}
	if err := returnForkCandidateOwnersToReview(candidate); err != nil {
		return ForkCandidateSupersession{}, err
	}
	return transition, nil
}

// RetireStaleForkCandidate is kept as the compatibility surface for callers that only need to know
// whether this invocation began the supersession.
func RetireStaleForkCandidate(repo string, identity forkspace.Identity, head string) (bool, error) {
	transition, err := SupersedeStaleForkCandidate(repo, identity, head)
	return transition.Started, err
}

func adoptLegacyForkCandidateLocked(repo string, identity forkspace.Identity, candidate ForkCandidate, phase string) (forkCandidateManifest, error) {
	if candidate.Fork != identity {
		return forkCandidateManifest{}, errors.New("legacy fork candidate belongs to another generation")
	}
	if phase != forkCandidateActive && phase != forkCandidateLanded && phase != forkCandidateDiscarded {
		return forkCandidateManifest{}, errors.New("invalid legacy fork candidate adoption phase")
	}
	if phase == forkCandidateActive {
		current, err := candidateAssignments(repo, identity)
		if err != nil {
			return forkCandidateManifest{}, err
		}
		if !sameForkCandidateSnapshot(candidate, candidate.Head, candidate.Tree, current) {
			return forkCandidateManifest{}, errors.New("legacy fork candidate disagrees with current assignments")
		}
		for _, assignment := range candidate.Assignments {
			record, owned, err := ReadTaskOwnerRecord(assignment.Index.CanonicalRoot, assignment.Index.Task.Ref.ID)
			if err != nil || !owned || record.Fork == nil || record.Fork.Phase != ForkAssignmentReady ||
				record.Fork.CandidateID != candidate.ID {
				return forkCandidateManifest{}, errors.Join(err, errors.New("legacy fork candidate owner state is inconsistent"))
			}
		}
	}
	record := forkCandidateRoundRecord{
		Version: forkCandidateRoundVersion, Round: 1, Candidate: candidate, CreatedAt: candidate.CreatedAt,
	}
	if err := writeForkCandidateRound(repo, record); err != nil {
		return forkCandidateManifest{}, err
	}
	ref := forkCandidateRef{Round: 1, ID: candidate.ID}
	terminalID := ""
	if phase == forkCandidateLanded || phase == forkCandidateDiscarded {
		terminalID = candidate.ID
	}
	manifest := forkCandidateManifest{
		Version: forkCandidateManifestVersion, Fork: identity, Phase: phase,
		Current: &ref, TerminalID: terminalID, UpdatedAt: time.Now().UTC(),
	}
	if err := writeForkCandidateManifest(repo, manifest); err != nil {
		return forkCandidateManifest{}, err
	}
	return manifest, nil
}

func markForkCandidateTerminalLocked(repo string, expected ForkCandidate, phase string) error {
	if err := validateForkCandidate(expected); err != nil {
		return err
	}
	state, exists, err := readForkCandidateState(repo, expected.Fork)
	if err != nil {
		return err
	}
	if !exists {
		if _, err := os.Lstat(forkCandidateHistoryDir(repo, expected.Fork)); err == nil {
			return errors.New("fork candidate manifest disappeared while retained history exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Compatibility recovery for an old v1 land transaction: the old order removed its only
		// candidate pointer before deleting the still-authoritative land journal. That journal
		// embeds the exact candidate, so it can be retained as round one without inventing state.
		return writeLegacyTerminalCandidateLocked(repo, expected, phase)
	}
	if state.legacy != nil {
		if !reflect.DeepEqual(*state.legacy, expected) {
			return errors.New("fork candidate changed before finalization")
		}
		_, err := adoptLegacyForkCandidateLocked(repo, expected.Fork, *state.legacy, phase)
		return err
	}
	manifest := *state.manifest
	if manifest.Phase == phase {
		if manifest.TerminalID != expected.ID {
			return errors.New("fork candidate changed before finalization")
		}
		current, err := candidateFromManifest(repo, manifest)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return errors.New("fork candidate changed before finalization")
		}
		return nil
	}
	if manifest.Phase != forkCandidateActive {
		return errors.New("fork candidate is changing before finalization")
	}
	current, err := candidateFromManifest(repo, manifest)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return errors.New("fork candidate changed before finalization")
	}
	manifest.Phase, manifest.TerminalID, manifest.UpdatedAt = phase, expected.ID, time.Now().UTC()
	return writeForkCandidateManifest(repo, manifest)
}

func writeLegacyTerminalCandidateLocked(repo string, candidate ForkCandidate, phase string) error {
	if phase != forkCandidateLanded && phase != forkCandidateDiscarded {
		return errors.New("invalid terminal candidate phase")
	}
	if err := writeForkCandidateRound(repo, forkCandidateRoundRecord{
		Version: forkCandidateRoundVersion, Round: 1, Candidate: candidate, CreatedAt: candidate.CreatedAt,
	}); err != nil {
		return err
	}
	ref := forkCandidateRef{Round: 1, ID: candidate.ID}
	return writeForkCandidateManifest(repo, forkCandidateManifest{
		Version: forkCandidateManifestVersion, Fork: candidate.Fork, Phase: phase,
		Current: &ref, TerminalID: candidate.ID, UpdatedAt: time.Now().UTC(),
	})
}

// MarkForkCandidateLandedLocked makes the live pointer passive while retaining immutable history.
func MarkForkCandidateLandedLocked(repo string, expected ForkCandidate) error {
	return markForkCandidateTerminalLocked(repo, expected, forkCandidateLanded)
}

// DiscardForkCandidateStateLocked retires candidate authority in any live phase. An unreviewed
// pending attempt is dropped; a successfully reviewed publishing attempt is first retained as an
// immutable round. The caller holds the lifecycle lock and has already fenced the exact discard.
func DiscardForkCandidateStateLocked(repo string, identity forkspace.Identity) error {
	state, exists, err := readForkCandidateState(repo, identity)
	if err != nil || !exists {
		return err
	}
	if state.legacy != nil {
		_, err := adoptLegacyForkCandidateLocked(repo, identity, *state.legacy, forkCandidateDiscarded)
		return err
	}
	manifest := *state.manifest
	if manifest.Phase == forkCandidateDiscarded {
		return nil
	}
	if manifest.Phase == forkCandidateLanded {
		return errors.New("fork candidate history is already landed")
	}
	terminalID := ""
	if manifest.Pending != nil {
		terminalID = manifest.Pending.ID
	} else if manifest.Current != nil {
		terminalID = manifest.Current.ID
	}
	if manifest.Phase == forkCandidatePublishing {
		candidate := *manifest.Pending
		round := pendingRound(manifest)
		supersedes := ""
		if manifest.Current != nil {
			supersedes = manifest.Current.ID
		}
		if err := writeForkCandidateRound(repo, forkCandidateRoundRecord{
			Version: forkCandidateRoundVersion, Round: round, Candidate: candidate,
			Supersedes: supersedes, CreatedAt: candidate.CreatedAt,
		}); err != nil {
			return err
		}
		manifest.Current = &forkCandidateRef{Round: round, ID: candidate.ID}
	}
	manifest.Phase, manifest.Pending, manifest.TerminalID, manifest.UpdatedAt =
		forkCandidateDiscarded, nil, terminalID, time.Now().UTC()
	return writeForkCandidateManifest(repo, manifest)
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
	current, ok, err := CurrentTask(root, id)
	if err != nil {
		return err
	}
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
	if err != nil || !sameTaskInstance(manifest.Task, assignment.Index.Task) {
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
