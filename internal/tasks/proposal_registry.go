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
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const (
	forkProposalRecordVersion = 1
	forkProposalRecordLimit   = 512 << 10
)

type ForkProposalPhase string

const (
	ForkProposalPrepared ForkProposalPhase = "prepared"
	ForkProposalImported ForkProposalPhase = "imported"
)

// ForkProposalRecord is host authority for one stable sandbox proposal. Imported receipts remain
// until the owning assignment lands or is explicitly discarded, so recreating a source file can
// never create a second canonical task.
type ForkProposalRecord struct {
	Version       int                `json:"version"`
	Fork          forkspace.Identity `json:"fork"`
	AssignmentID  string             `json:"assignment_id"`
	CanonicalRoot string             `json:"canonical_root"`
	QueueID       string             `json:"queue_id"`
	ProposalID    string             `json:"proposal_id"`
	SourceDigest  string             `json:"source_digest"`
	Task          TaskInstance       `json:"task"`
	Phase         ForkProposalPhase  `json:"phase"`
	Proposal      *ForkTaskProposal  `json:"proposal,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

func forkProposalRecordRoot(repo string, identity forkspace.Identity) string {
	return filepath.Join(forkspace.StateDir(repo), "proposals", identity.Name+"."+string(identity.Generation))
}

func forkProposalAssignmentRecordRoot(repo string, identity forkspace.Identity, assignmentID string) string {
	return filepath.Join(forkProposalRecordRoot(repo, identity), assignmentID)
}

func ensureForkProposalAssignmentRecordRoot(repo string, identity forkspace.Identity, assignmentID string) error {
	if !validAssignmentID(assignmentID) {
		return errors.New("invalid proposal assignment identity")
	}
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{forkspace.Home(repo), 0o755},
		{forkspace.StateDir(repo), 0o755},
		{filepath.Join(forkspace.StateDir(repo), "proposals"), 0o700},
		{forkProposalRecordRoot(repo, identity), 0o700},
		{forkProposalAssignmentRecordRoot(repo, identity, assignmentID), 0o700},
	} {
		if err := ensureRealDirectory(dir.path, dir.mode); err != nil {
			return err
		}
	}
	return nil
}

func validateForkProposalRecord(record ForkProposalRecord) error {
	if record.Version != forkProposalRecordVersion ||
		!forkspace.ValidExistingName(record.Fork.Name) || !forkspace.ValidGeneration(record.Fork.Generation) ||
		!validAssignmentID(record.AssignmentID) || !filepath.IsAbs(record.CanonicalRoot) ||
		filepath.Clean(record.CanonicalRoot) != record.CanonicalRoot || !durableIdentityRE.MatchString(record.QueueID) ||
		!validAssignmentID(record.ProposalID) || !validDigest(record.SourceDigest) ||
		record.Task.Ref.QueueID != record.QueueID || record.Task.Ref.ID == "" ||
		!durableIdentityRE.MatchString(record.Task.Ref.TaskID) || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return errors.New("invalid fork proposal record")
	}
	switch record.Phase {
	case ForkProposalPrepared:
		if record.Proposal == nil || record.Task.Generation != (TaskGeneration{}) {
			return errors.New("prepared fork proposal has invalid task state")
		}
	case ForkProposalImported:
		if record.Task.Generation.Device == 0 || record.Task.Generation.Inode == 0 {
			return errors.New("imported fork proposal has no task generation")
		}
	default:
		return errors.New("invalid fork proposal phase")
	}
	if record.Proposal != nil {
		if err := validateForkTaskProposal(*record.Proposal); err != nil || record.Proposal.ID != record.ProposalID {
			return errors.Join(err, errors.New("fork proposal payload identity mismatch"))
		}
	}
	return nil
}

func decodeForkProposalRecord(data []byte) (ForkProposalRecord, error) {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), forkProposalRecordLimit+1))
	dec.DisallowUnknownFields()
	var record ForkProposalRecord
	if err := dec.Decode(&record); err != nil {
		return ForkProposalRecord{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ForkProposalRecord{}, errors.New("fork proposal record contains multiple JSON values")
		}
		return ForkProposalRecord{}, err
	}
	if err := validateForkProposalRecord(record); err != nil {
		return ForkProposalRecord{}, err
	}
	return record, nil
}

func readForkProposalRecordFile(root *os.Root, name string) (ForkProposalRecord, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return ForkProposalRecord{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > forkProposalRecordLimit {
		return ForkProposalRecord{}, errors.New("fork proposal record is not a bounded single-link regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ForkProposalRecord{}, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return ForkProposalRecord{}, err
		}
		return ForkProposalRecord{}, errors.New("fork proposal record changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, forkProposalRecordLimit+1))
	if err != nil || len(data) > forkProposalRecordLimit {
		if err != nil {
			return ForkProposalRecord{}, err
		}
		return ForkProposalRecord{}, errors.New("fork proposal record exceeds its size limit")
	}
	return decodeForkProposalRecord(data)
}

func writeForkProposalRecordFile(root *os.Root, name string, record ForkProposalRecord, replace bool) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if !replace {
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		writeErr := error(nil)
		if _, err := f.Write(append(body, '\n')); err != nil {
			writeErr = err
		} else {
			writeErr = f.Sync()
		}
		writeErr = errors.Join(writeErr, f.Close())
		if writeErr != nil {
			_ = root.Remove(name)
		}
		return writeErr
	}
	tmp := fmt.Sprintf(".%s-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

func createForkProposalRecord(repo string, record ForkProposalRecord) error {
	if err := validateForkProposalRecord(record); err != nil {
		return err
	}
	if err := ensureForkProposalAssignmentRecordRoot(repo, record.Fork, record.AssignmentID); err != nil {
		return err
	}
	root, err := os.OpenRoot(forkProposalAssignmentRecordRoot(repo, record.Fork, record.AssignmentID))
	if err != nil {
		return err
	}
	defer root.Close()
	name := record.ProposalID + ".json"
	if current, err := readForkProposalRecordFile(root, name); err == nil {
		if reflect.DeepEqual(current, record) {
			return nil
		}
		return errors.New("fork proposal id was reused with different content")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeForkProposalRecordFile(root, name, record, false)
}

func updateForkProposalRecord(repo string, expected, next ForkProposalRecord) error {
	if err := validateForkProposalRecord(expected); err != nil {
		return err
	}
	if err := validateForkProposalRecord(next); err != nil {
		return err
	}
	if expected.Fork != next.Fork || expected.AssignmentID != next.AssignmentID ||
		expected.ProposalID != next.ProposalID || expected.CanonicalRoot != next.CanonicalRoot ||
		expected.QueueID != next.QueueID || expected.SourceDigest != next.SourceDigest ||
		expected.Task.Ref != next.Task.Ref || expected.CreatedAt != next.CreatedAt {
		return errors.New("fork proposal immutable identity changed")
	}
	root, err := os.OpenRoot(forkProposalAssignmentRecordRoot(repo, expected.Fork, expected.AssignmentID))
	if err != nil {
		return err
	}
	defer root.Close()
	name := expected.ProposalID + ".json"
	current, err := readForkProposalRecordFile(root, name)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return errors.New("fork proposal record changed before update")
	}
	return writeForkProposalRecordFile(root, name, next, true)
}

func removeForkProposalRecord(repo string, expected ForkProposalRecord) error {
	root, err := os.OpenRoot(forkProposalAssignmentRecordRoot(repo, expected.Fork, expected.AssignmentID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	name := expected.ProposalID + ".json"
	current, err := readForkProposalRecordFile(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, expected) {
		return errors.New("fork proposal record changed before removal")
	}
	return root.Remove(name)
}

// readDirBounded enumerates at most limit+1 entries without letting an untrusted directory make
// os.ReadDir allocate its entire contents first.
func readDirBounded(root *os.Root, limit int) ([]os.DirEntry, error) {
	dir, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > limit {
		return nil, fmt.Errorf("directory exceeds its %d-entry limit", limit)
	}
	return entries, nil
}

func forkProposalRecordsForAssignment(repo string, identity forkspace.Identity, assignmentID string) ([]ForkProposalRecord, []error) {
	dir := forkProposalAssignmentRecordRoot(repo, identity, assignmentID)
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	defer root.Close()
	var records []ForkProposalRecord
	var problems []error
	opened, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, []error{err}
	}
	for {
		entries, readErr := opened.ReadDir(128)
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				problems = append(problems, fmt.Errorf("%s: unsupported fork proposal record entry", entry.Name()))
				continue
			}
			record, err := readForkProposalRecordFile(root, entry.Name())
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), err))
				continue
			}
			if record.Fork != identity || record.AssignmentID != assignmentID || entry.Name() != record.ProposalID+".json" {
				problems = append(problems, fmt.Errorf("%s: fork proposal record identity mismatch", entry.Name()))
				continue
			}
			records = append(records, record)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			problems = append(problems, readErr)
			break
		}
	}
	if err := opened.Close(); err != nil {
		problems = append(problems, err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ProposalID < records[j].ProposalID })
	return records, problems
}

func forkProposalRecords(repo string, identity forkspace.Identity) ([]ForkProposalRecord, []error) {
	root, err := os.OpenRoot(forkProposalRecordRoot(repo, identity))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	defer root.Close()
	assignments, err := readDirBounded(root, 4096)
	if err != nil {
		return nil, []error{err}
	}
	var records []ForkProposalRecord
	var problems []error
	for _, entry := range assignments {
		if !entry.IsDir() || !validAssignmentID(entry.Name()) {
			problems = append(problems, fmt.Errorf("%s: unsupported fork proposal assignment entry", entry.Name()))
			continue
		}
		found, errs := forkProposalRecordsForAssignment(repo, identity, entry.Name())
		records = append(records, found...)
		problems = append(problems, errs...)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].AssignmentID != records[j].AssignmentID {
			return records[i].AssignmentID < records[j].AssignmentID
		}
		return records[i].ProposalID < records[j].ProposalID
	})
	return records, problems
}
