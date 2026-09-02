package tasks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

type ForkTaskStateSummary struct {
	Assignments       int    `json:"assignments"`
	Candidate         bool   `json:"candidate"`
	PreparedProposals int    `json:"prepared_proposals"`
	ImportedReceipts  int    `json:"imported_proposal_receipts"`
	PendingProposals  int    `json:"pending_proposals"`
	DiscardPending    bool   `json:"discard_pending"`
	Fingerprint       string `json:"fingerprint"`
}

func (summary ForkTaskStateSummary) Active() bool {
	return summary.Assignments > 0 || summary.Candidate || summary.PreparedProposals > 0 ||
		summary.ImportedReceipts > 0 || summary.PendingProposals > 0 || summary.DiscardPending
}

// ReadForkTaskStateSummary returns both the operator-facing blast radius and an exact fingerprint
// over every generation-scoped authority record plus pending proposal bytes. Destructive callers
// capture it before confirmation and compare it under the lifecycle lock, so newly-acquired work
// can never be silently folded into an earlier "yes".
func ReadForkTaskStateSummary(repo string, identity forkspace.Identity) (ForkTaskStateSummary, error) {
	indexes, problems := IndexedForkAssignments(repo, identity)
	if len(problems) > 0 {
		return ForkTaskStateSummary{}, errors.Join(problems...)
	}
	candidate, hasCandidate, err := ReadForkCandidate(repo, identity)
	if err != nil {
		return ForkTaskStateSummary{}, err
	}
	discard, hasDiscard, err := readForkDiscard(repo, identity)
	if err != nil {
		return ForkTaskStateSummary{}, err
	}
	records, proposalProblems := forkProposalRecords(repo, identity)
	if len(proposalProblems) > 0 {
		return ForkTaskStateSummary{}, errors.Join(proposalProblems...)
	}
	summary := ForkTaskStateSummary{Assignments: len(indexes), Candidate: hasCandidate, DiscardPending: hasDiscard}
	hash := sha256.New()
	write := func(kind string, value any) error {
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		fmt.Fprintf(hash, "%s\x00", kind)
		hash.Write(body)
		hash.Write([]byte{0})
		return nil
	}
	for _, index := range indexes {
		if err := write("assignment", index); err != nil {
			return ForkTaskStateSummary{}, err
		}
	}
	if hasCandidate {
		if err := write("candidate", candidate); err != nil {
			return ForkTaskStateSummary{}, err
		}
	}
	if hasDiscard {
		if err := write("discard", discard); err != nil {
			return ForkTaskStateSummary{}, err
		}
	}
	for _, record := range records {
		if record.Phase == ForkProposalPrepared {
			summary.PreparedProposals++
		} else if record.Phase == ForkProposalImported {
			summary.ImportedReceipts++
		}
		if err := write("proposal-record", record); err != nil {
			return ForkTaskStateSummary{}, err
		}
	}
	var pending []string
	for _, index := range indexes {
		owner, err := ownerForProposalIndex(index)
		if err != nil {
			return ForkTaskStateSummary{}, err
		}
		root, err := openForkProposalOutbox(repo, owner)
		if err != nil {
			return ForkTaskStateSummary{}, err
		}
		entries, readErr := readDirBounded(root, forkProposalCountLimit)
		if readErr == nil {
			for _, entry := range entries {
				data, _, entryErr := readForkProposalFile(root, entry.Name())
				if entryErr != nil {
					readErr = entryErr
					break
				}
				pending = append(pending, index.AssignmentID+"/"+entry.Name()+"/"+proposalDigest(data))
			}
		}
		closeErr := root.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return ForkTaskStateSummary{}, err
		}
	}
	sort.Strings(pending)
	summary.PendingProposals = len(pending)
	if err := write("pending-proposals", pending); err != nil {
		return ForkTaskStateSummary{}, err
	}
	summary.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	return summary, nil
}

const (
	forkDiscardVersion = 1
	forkDiscardLimit   = 8 << 20
)

type forkDiscardIntent struct {
	Version     int                   `json:"version"`
	Fork        forkspace.Identity    `json:"fork"`
	Assignments []ForkAssignmentIndex `json:"assignments"`
	CandidateID string                `json:"candidate_id,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
}

func forkDiscardPath(repo string, identity forkspace.Identity) string {
	return filepath.Join(forkspace.StateDir(repo), identity.Name+"."+string(identity.Generation)+".discard.json")
}

func validateForkDiscard(intent forkDiscardIntent) error {
	if intent.Version != forkDiscardVersion || !forkspace.ValidExistingName(intent.Fork.Name) ||
		!forkspace.ValidGeneration(intent.Fork.Generation) || intent.CreatedAt.IsZero() ||
		(intent.CandidateID != "" && !validAssignmentID(intent.CandidateID)) {
		return errors.New("invalid fork discard intent")
	}
	seen := map[string]bool{}
	for _, assignment := range intent.Assignments {
		if err := validateForkAssignmentIndex(assignment); err != nil || assignment.Fork != intent.Fork ||
			seen[assignment.AssignmentID] {
			return errors.New("invalid assignment in fork discard intent")
		}
		seen[assignment.AssignmentID] = true
	}
	return nil
}

func readForkDiscard(repo string, identity forkspace.Identity) (forkDiscardIntent, bool, error) {
	path := forkDiscardPath(repo, identity)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return forkDiscardIntent{}, false, nil
	}
	if err != nil {
		return forkDiscardIntent{}, false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > forkDiscardLimit {
		return forkDiscardIntent{}, false, errors.New("fork discard intent is not a bounded single-link regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return forkDiscardIntent{}, false, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, forkDiscardLimit+1))
	if err != nil || len(data) > forkDiscardLimit {
		if err != nil {
			return forkDiscardIntent{}, false, err
		}
		return forkDiscardIntent{}, false, errors.New("fork discard intent exceeds its size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var intent forkDiscardIntent
	if err := dec.Decode(&intent); err != nil {
		return forkDiscardIntent{}, false, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return forkDiscardIntent{}, false, errors.New("fork discard intent contains multiple JSON values")
		}
		return forkDiscardIntent{}, false, err
	}
	if err := validateForkDiscard(intent); err != nil {
		return forkDiscardIntent{}, false, err
	}
	if intent.Fork != identity {
		return forkDiscardIntent{}, false, errors.New("fork discard intent belongs to another generation")
	}
	return intent, true, nil
}

func writeForkDiscard(repo string, intent forkDiscardIntent) error {
	if err := validateForkDiscard(intent); err != nil {
		return err
	}
	body, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	path := forkDiscardPath(repo, intent.Fork)
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

// ForkTaskState reports generation-scoped assignment/candidate/discard authority. Corruption is an
// error, never an empty result that would authorize workspace destruction.
func ForkTaskState(repo string, identity forkspace.Identity) (bool, error) {
	summary, err := ReadForkTaskStateSummary(repo, identity)
	return summary.Active(), err
}

// DiscardForkTaskStateLocked durably returns exact sandbox-owned tasks to canonical control before
// a forced workspace deletion. In-progress tasks become todo, blocked tasks stay blocked, and a
// crash replays from the immutable intent instead of guessing from current configuration.
func DiscardForkTaskStateLocked(repo string, identity forkspace.Identity) error {
	intent, exists, err := readForkDiscard(repo, identity)
	if err != nil {
		return err
	}
	if !exists {
		indexes, problems := IndexedForkAssignments(repo, identity)
		if len(problems) > 0 {
			return errors.Join(problems...)
		}
		candidate, hasCandidate, err := ReadForkCandidate(repo, identity)
		if err != nil {
			return err
		}
		intent = forkDiscardIntent{
			Version: forkDiscardVersion, Fork: identity, Assignments: indexes, CreatedAt: time.Now().UTC(),
		}
		if hasCandidate {
			intent.CandidateID = candidate.ID
		}
		if err := writeForkDiscard(repo, intent); err != nil {
			return err
		}
	}
	if err := discardForkProposalsLocked(repo, identity); err != nil {
		return err
	}
	for _, index := range intent.Assignments {
		if err := discardForkAssignment(repo, identity, index); err != nil {
			return err
		}
	}
	if candidate, ok, err := ReadForkCandidate(repo, identity); err != nil {
		return err
	} else if ok {
		if intent.CandidateID == "" || candidate.ID != intent.CandidateID {
			return errors.New("fork candidate changed during discard")
		}
		if err := RemoveForkCandidateIfMatchesLocked(repo, candidate); err != nil {
			return err
		}
	}
	if err := os.Remove(forkDiscardPath(repo, identity)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func discardForkAssignment(repo string, identity forkspace.Identity, index ForkAssignmentIndex) (retErr error) {
	root, id := index.CanonicalRoot, index.Task.Ref.ID
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
	item, ok, err := CurrentTask(root, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("discard assignment %s: canonical task is missing", index.AssignmentID)
	}
	instance, err := ReadTaskInstance(root, item)
	if err != nil || !sameTaskInstance(instance, index.Task) {
		return errors.Join(err, fmt.Errorf("discard assignment %s: canonical task was replaced", index.AssignmentID))
	}
	record, owned, err := ownerLock.Read()
	if err != nil {
		return err
	}
	if owned {
		if record.Kind != TaskOwnerFork || record.Fork == nil || record.Fork.Fork != identity ||
			record.Fork.AssignmentID != index.AssignmentID {
			return errors.New("canonical task owner changed during fork discard")
		}
		if item.State == StateDone {
			return errors.New("cannot discard a canonical task already moved to done; replay its land journal")
		}
		if item.State != StateBlocked {
			if item.State != StateTodo {
				if err := MoveTaskDir(root, item, StateTodo); err != nil {
					return err
				}
				item.State = StateTodo
				item.Dir = filepath.Join(root, StateTodo, id)
			}
			if err := NormalizeTaskState(id, item.Dir, "ready — sandbox assignment discarded", "claim or assign this canonical task again", "sandbox workspace was explicitly discarded", "review retained artifacts before retrying"); err != nil {
				return err
			}
		}
		if err := removeTaskOwnerRecordFile(root, id); err != nil {
			return err
		}
	} else if item.State != StateTodo && item.State != StateBlocked {
		return errors.New("fork discard found an unowned canonical task in an ambiguous state")
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		return err
	}
	return RemoveForkAssignmentIndex(repo, index)
}
