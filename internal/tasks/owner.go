package tasks

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/processidentity"
)

const (
	taskOwnerRecordVersion     = 1 // legacy human-only wire format; remains readable
	taskOwnershipRecordVersion = 2

	taskOwnerSourceInteractiveClaim = "interactive-claim"
	taskOwnerLockPrefix             = "\x00task-owner:"
)

type TaskOwnerKind string

const (
	TaskOwnerHuman TaskOwnerKind = "human"
	TaskOwnerFork  TaskOwnerKind = "fork"
)

type ForkAssignmentPhase string

const (
	ForkAssignmentPreparing ForkAssignmentPhase = "preparing"
	ForkAssignmentWorking   ForkAssignmentPhase = "working"
	ForkAssignmentPaused    ForkAssignmentPhase = "paused"
	ForkAssignmentBlocking  ForkAssignmentPhase = "blocking"
	ForkAssignmentReviewing ForkAssignmentPhase = "reviewing"
	ForkAssignmentReady     ForkAssignmentPhase = "ready"
	ForkAssignmentBlocked   ForkAssignmentPhase = "blocked"
)

type ForkTaskOwner struct {
	Fork             forkspace.Identity  `json:"fork"`
	AssignmentID     string              `json:"assignment_id"`
	Phase            ForkAssignmentPhase `json:"phase"`
	Projection       string              `json:"projection"`
	BaselineHead     string              `json:"baseline_head"`
	ProjectionDigest string              `json:"projection_digest,omitempty"`
	CandidateID      string              `json:"candidate_id,omitempty"`
	AssignedAt       time.Time           `json:"assigned_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

// TaskOwnerRecord deliberately keeps the v1 human fields flat. Version 2 adds a typed owner and
// exact task instance while preserving those fields for human claims; an older Coop sees version 2
// at the same registry name and fails closed instead of adopting a sandbox-owned task.
type TaskOwnerRecord struct {
	Version int           `json:"version"`
	TaskID  string        `json:"task_id"`
	Kind    TaskOwnerKind `json:"kind,omitempty"`
	Task    *TaskInstance `json:"task,omitempty"`

	Source    string    `json:"source,omitempty"`
	User      string    `json:"user,omitempty"`
	Host      string    `json:"host,omitempty"`
	ClaimedAt time.Time `json:"claimed_at,omitempty"`

	// A claim made by an agent process (no terminal) is bound to that process: Actor is the label
	// the queue shows (the process's command name unless --as named it), ActorPID/ActorStart its
	// kernel identity, read back through processidentity so a reused pid never counts as the same
	// owner. Liveness is inspected when a row is rendered, never stored. All three stay empty for
	// a person's claim.
	Actor      string `json:"actor,omitempty"`
	ActorPID   int    `json:"actor_pid,omitempty"`
	ActorStart string `json:"actor_start,omitempty"`

	Fork *ForkTaskOwner `json:"fork,omitempty"`
}

var ErrTaskSandboxOwned = errors.New("task is assigned to an isolated sandbox")

func taskOwnerRecordName(root, id string) (string, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return "", err
	}
	return key + ".owner.json", nil
}

func validAssignmentID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func validCommitID(id string) bool {
	if id == "" {
		return true
	}
	if len(id) != 40 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func validDigest(digest string) bool {
	if digest == "" {
		return true
	}
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil && digest == strings.ToLower(digest)
}

func validForkPhase(phase ForkAssignmentPhase) bool {
	switch phase {
	case ForkAssignmentPreparing, ForkAssignmentWorking, ForkAssignmentPaused, ForkAssignmentBlocking,
		ForkAssignmentReviewing, ForkAssignmentReady, ForkAssignmentBlocked:
		return true
	default:
		return false
	}
}

func validateTaskOwnerRecord(record TaskOwnerRecord, id string) error {
	if record.TaskID != id {
		return errors.New("invalid task owner record")
	}
	switch record.Version {
	case taskOwnerRecordVersion:
		if record.Kind != "" || record.Task != nil || record.Fork != nil || record.Source == "" ||
			strings.TrimSpace(record.User) == "" || strings.TrimSpace(record.Host) == "" || record.ClaimedAt.IsZero() ||
			record.Actor != "" || record.ActorPID != 0 || record.ActorStart != "" {
			return errors.New("invalid legacy task owner record")
		}
		return nil
	case taskOwnershipRecordVersion:
		if record.Task == nil || record.Task.Ref.ID != id ||
			!durableIdentityRE.MatchString(record.Task.Ref.QueueID) ||
			!durableIdentityRE.MatchString(record.Task.Ref.TaskID) ||
			record.Task.Generation.Device == 0 || record.Task.Generation.Inode == 0 {
			return errors.New("invalid task owner identity")
		}
		switch record.Kind {
		case TaskOwnerHuman:
			if record.Fork != nil || record.Source != taskOwnerSourceInteractiveClaim ||
				strings.TrimSpace(record.User) == "" || strings.TrimSpace(record.Host) == "" ||
				record.ClaimedAt.IsZero() {
				return errors.New("invalid human task owner record")
			}
			if record.Actor != claimActorLabel(record.Actor) ||
				(record.ActorPID != 0) != (record.ActorStart != "") ||
				(record.ActorPID != 0 && (record.ActorPID <= 1 || !processidentity.Stable(record.ActorStart))) {
				return errors.New("invalid claim actor identity")
			}
		case TaskOwnerFork:
			owner := record.Fork
			if owner == nil || record.Source != "" || record.User != "" || record.Host != "" ||
				!record.ClaimedAt.IsZero() || record.Actor != "" || record.ActorPID != 0 || record.ActorStart != "" ||
				owner.Fork.Name == "" ||
				!forkspace.ValidGeneration(owner.Fork.Generation) || !validAssignmentID(owner.AssignmentID) ||
				!validForkPhase(owner.Phase) || !filepath.IsAbs(owner.Projection) ||
				filepath.Clean(owner.Projection) != owner.Projection || !validCommitID(owner.BaselineHead) ||
				!validDigest(owner.ProjectionDigest) || owner.AssignedAt.IsZero() || owner.UpdatedAt.IsZero() ||
				owner.UpdatedAt.Before(owner.AssignedAt) {
				return errors.New("invalid fork task owner record")
			}
			if owner.Phase == ForkAssignmentReady && (owner.ProjectionDigest == "" || owner.CandidateID == "") {
				return errors.New("ready fork task owner has no candidate evidence")
			}
			if owner.CandidateID != "" && !validAssignmentID(owner.CandidateID) {
				return errors.New("fork task owner has an invalid candidate identity")
			}
		default:
			return errors.New("invalid typed task owner record")
		}
		return nil
	default:
		return errors.New("unsupported task owner record version")
	}
}

func decodeTaskOwnerRecord(data []byte) (TaskOwnerRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var record TaskOwnerRecord
	if err := dec.Decode(&record); err != nil {
		return TaskOwnerRecord{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return TaskOwnerRecord{}, errors.New("task owner record contains multiple JSON values")
		}
		return TaskOwnerRecord{}, err
	}
	return record, nil
}

func readTaskOwnerRecordUnvalidated(root, id string) (TaskOwnerRecord, bool, error) {
	name, err := taskOwnerRecordName(root, id)
	if err != nil {
		return TaskOwnerRecord{}, false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return TaskOwnerRecord{}, false, err
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, name)
	if errors.Is(err, os.ErrNotExist) {
		return TaskOwnerRecord{}, false, nil
	}
	if err != nil {
		return TaskOwnerRecord{}, false, err
	}
	record, err := decodeTaskOwnerRecord(data)
	if err != nil {
		return TaskOwnerRecord{}, false, err
	}
	if err := validateTaskOwnerRecord(record, id); err != nil {
		return TaskOwnerRecord{}, false, err
	}
	return record, true, nil
}

// ReadTaskOwnerRecord reports durable human or fork ownership. Version 2 is also fenced to the
// current folder instance; a copied/recreated task cannot inherit an old assignment merely because
// it reused the slug.
func ReadTaskOwnerRecord(root, id string) (TaskOwnerRecord, bool, error) {
	record, ok, err := readTaskOwnerRecordUnvalidated(root, id)
	if err != nil || !ok || record.Version == taskOwnerRecordVersion {
		return record, ok, err
	}
	item, exists, err := CurrentTask(root, id)
	if err != nil {
		return TaskOwnerRecord{}, false, err
	}
	if !exists {
		return TaskOwnerRecord{}, false, errors.New("owned task is missing from its canonical queue")
	}
	instance, err := ReadTaskInstance(root, item)
	if err != nil {
		return TaskOwnerRecord{}, false, err
	}
	if !sameTaskInstance(*record.Task, instance) {
		return TaskOwnerRecord{}, false, errors.New("task owner record names a replaced task instance")
	}
	return record, true, nil
}

func writeTaskOwnerRecord(root string, record TaskOwnerRecord) error {
	if err := validateTaskOwnerRecord(record, record.TaskID); err != nil {
		return err
	}
	if record.Version == taskOwnershipRecordVersion {
		item, ok, err := CurrentTask(root, record.TaskID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("cannot own a missing task")
		}
		instance, err := ReadTaskInstance(root, item)
		if err != nil {
			return err
		}
		if !sameTaskInstance(*record.Task, instance) {
			return errors.New("task changed before ownership was recorded")
		}
	}
	name, err := taskOwnerRecordName(root, record.TaskID)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return AtomicWriteTaskFile(registry, name, append(data, '\n'))
}

func removeTaskOwnerRecordFile(root, id string) error {
	name, err := taskOwnerRecordName(root, id)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	if err := registry.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type taskOwnerLock struct {
	root string
	id   string
	file *os.File
}

func lockTaskOwner(root, id string) (*taskOwnerLock, error) {
	file, err := lockLeaseAuthority(root, taskOwnerLockPrefix+id, true, syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	return &taskOwnerLock{root: root, id: id, file: file}, nil
}

func (lock *taskOwnerLock) Close() error { return unlockLeaseFile(lock.file) }

func (lock *taskOwnerLock) Read() (TaskOwnerRecord, bool, error) {
	return ReadTaskOwnerRecord(lock.root, lock.id)
}

func (lock *taskOwnerLock) Write(record TaskOwnerRecord) error {
	if record.TaskID != lock.id {
		return errors.New("task owner lock does not match record")
	}
	return writeTaskOwnerRecord(lock.root, record)
}

func (lock *taskOwnerLock) RemoveHumanOrAbsent() error {
	record, ok, err := lock.Read()
	if err != nil || !ok {
		return err
	}
	if record.Kind == TaskOwnerFork {
		return fmt.Errorf("%w: %s is owned by fork %s generation %s", ErrTaskSandboxOwned,
			lock.id, record.Fork.Fork.Name, record.Fork.Fork.Generation)
	}
	return removeTaskOwnerRecordFile(lock.root, lock.id)
}

// removeTaskOwnerRecord is the human/local lifecycle clear. It is idempotent for absent records
// and refuses a fork assignment; land/discard perform exact-match removal in their transactions.
func removeTaskOwnerRecord(root, id string) error {
	lock, err := lockTaskOwner(root, id)
	if err != nil {
		return err
	}
	defer lock.Close()
	return lock.RemoveHumanOrAbsent()
}

func newAssignmentID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func sameForkAssignment(a, b ForkTaskOwner) bool {
	return a.Fork == b.Fork && a.AssignmentID == b.AssignmentID
}

func UpdateForkTaskAssignment(root, id string, expected ForkTaskOwner, update func(*ForkTaskOwner) error) (TaskOwnerRecord, error) {
	lock, err := lockTaskOwner(root, id)
	if err != nil {
		return TaskOwnerRecord{}, err
	}
	defer lock.Close()
	record, ok, err := lock.Read()
	if err != nil {
		return TaskOwnerRecord{}, err
	}
	if !ok || record.Kind != TaskOwnerFork || record.Fork == nil || !sameForkAssignment(*record.Fork, expected) {
		return TaskOwnerRecord{}, errors.New("task assignment changed before update")
	}
	if err := update(record.Fork); err != nil {
		return TaskOwnerRecord{}, err
	}
	record.Fork.UpdatedAt = time.Now().UTC()
	if err := lock.Write(record); err != nil {
		return TaskOwnerRecord{}, err
	}
	return record, nil
}

// TaskOwnerLabel renders who holds a task. A human claim names its actor — the claiming agent's
// process, or the user when the claim was made at a terminal — and, when the claim is bound to a
// process, whether that process still exists. Liveness is read here, at render time, never stored.
func TaskOwnerLabel(record TaskOwnerRecord) string {
	if record.Kind == TaskOwnerFork && record.Fork != nil {
		return "assigned to fork " + record.Fork.Fork.Name + " (" + string(record.Fork.Phase) + ")"
	}
	who := record.User
	if record.Actor != "" {
		who = record.Actor
	}
	if record.ActorPID == 0 {
		return "claimed by " + who
	}
	label := fmt.Sprintf("claimed by %s (pid %d)", who, record.ActorPID)
	if !ownerProcessLive(record) {
		label += " · owner process gone"
	}
	return label
}

func refuseForkTaskOwner(root, id, action string) error {
	record, ok, err := ReadTaskOwnerRecord(root, id)
	if err != nil {
		return err
	}
	if ok && record.Kind == TaskOwnerFork {
		return fmt.Errorf("%w: cannot %s %s while it is %s", ErrTaskSandboxOwned, action, id, TaskOwnerLabel(record))
	}
	return nil
}
