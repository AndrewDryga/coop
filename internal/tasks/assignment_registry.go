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
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const (
	forkAssignmentIndexVersion = 1
	forkAssignmentIndexLimit   = 64 << 10
	forkAssignmentIndexCount   = 4096
)

// ForkAssignmentIndex is the immutable host-side reverse index for one assignment. Task owner
// records remain the phase authority; this record makes the assignment discoverable without
// trusting a configured queue list or anything inside the fork workspace.
type ForkAssignmentIndex struct {
	Version       int                `json:"version"`
	Fork          forkspace.Identity `json:"fork"`
	AssignmentID  string             `json:"assignment_id"`
	CanonicalRoot string             `json:"canonical_root"`
	Task          TaskInstance       `json:"task"`
	Projection    string             `json:"projection"`
	BaselineHead  string             `json:"baseline_head"`
	CreatedAt     time.Time          `json:"created_at"`
}

func forkAssignmentIndexDir(repo string, identity forkspace.Identity) string {
	return filepath.Join(forkspace.StateDir(repo), "assignments", identity.Name+"."+string(identity.Generation))
}

func validateForkAssignmentIndex(record ForkAssignmentIndex) error {
	if record.Version != forkAssignmentIndexVersion ||
		!forkspace.ValidExistingName(record.Fork.Name) || !forkspace.ValidGeneration(record.Fork.Generation) ||
		!validAssignmentID(record.AssignmentID) || !filepath.IsAbs(record.CanonicalRoot) ||
		filepath.Clean(record.CanonicalRoot) != record.CanonicalRoot || record.Task.Ref.ID == "" ||
		!durableIdentityRE.MatchString(record.Task.Ref.QueueID) ||
		!durableIdentityRE.MatchString(record.Task.Ref.TaskID) ||
		record.Task.Generation.Device == 0 || record.Task.Generation.Inode == 0 ||
		!filepath.IsAbs(record.Projection) || filepath.Clean(record.Projection) != record.Projection ||
		!validCommitID(record.BaselineHead) || record.BaselineHead == "" || record.CreatedAt.IsZero() {
		return errors.New("invalid fork assignment index")
	}
	return nil
}

func ensureRealDirectory(path string, mode os.FileMode) error {
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("host authority path %q is not a real directory", path)
	}
	return nil
}

func ensureForkAssignmentIndexDir(repo string, identity forkspace.Identity) error {
	if err := ensureRealDirectory(forkspace.Home(repo), 0o755); err != nil {
		return err
	}
	if err := ensureRealDirectory(forkspace.StateDir(repo), 0o755); err != nil {
		return err
	}
	root := filepath.Join(forkspace.StateDir(repo), "assignments")
	if err := ensureRealDirectory(root, 0o700); err != nil {
		return err
	}
	return ensureRealDirectory(forkAssignmentIndexDir(repo, identity), 0o700)
}

func decodeForkAssignmentIndex(data []byte) (ForkAssignmentIndex, error) {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), forkAssignmentIndexLimit+1))
	dec.DisallowUnknownFields()
	var record ForkAssignmentIndex
	if err := dec.Decode(&record); err != nil {
		return ForkAssignmentIndex{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ForkAssignmentIndex{}, errors.New("fork assignment index contains multiple JSON values")
		}
		return ForkAssignmentIndex{}, err
	}
	if err := validateForkAssignmentIndex(record); err != nil {
		return ForkAssignmentIndex{}, err
	}
	return record, nil
}

func readForkAssignmentIndexFile(root *os.Root, name string) (ForkAssignmentIndex, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return ForkAssignmentIndex{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		info.Size() < 0 || info.Size() > forkAssignmentIndexLimit {
		return ForkAssignmentIndex{}, errors.New("fork assignment index is not a bounded single-link regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ForkAssignmentIndex{}, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		if err != nil {
			return ForkAssignmentIndex{}, err
		}
		return ForkAssignmentIndex{}, errors.New("fork assignment index changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, forkAssignmentIndexLimit+1))
	if err != nil || len(data) > forkAssignmentIndexLimit {
		if err != nil {
			return ForkAssignmentIndex{}, err
		}
		return ForkAssignmentIndex{}, errors.New("fork assignment index exceeds its size limit")
	}
	return decodeForkAssignmentIndex(data)
}

// RegisterForkAssignmentIndex publishes the reverse index before canonical ownership changes.
// Existing exact contents are an idempotent crash replay; any replacement is retained and refused.
func RegisterForkAssignmentIndex(repo string, record ForkAssignmentIndex) error {
	if err := validateForkAssignmentIndex(record); err != nil {
		return err
	}
	if err := ensureForkAssignmentIndexDir(repo, record.Fork); err != nil {
		return err
	}
	root, err := os.OpenRoot(forkAssignmentIndexDir(repo, record.Fork))
	if err != nil {
		return err
	}
	defer root.Close()
	name := record.AssignmentID + ".json"
	if current, err := readForkAssignmentIndexFile(root, name); err == nil {
		if current == record {
			return nil
		}
		return errors.New("fork assignment index identity collision")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
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
	if err := f.Close(); err != nil {
		writeErr = errors.Join(writeErr, err)
	}
	if writeErr != nil {
		_ = root.Remove(name)
	}
	return writeErr
}

// IndexedForkAssignments enumerates the exact generation registry. Per-record corruption is
// reported without hiding healthy siblings; callers must fail closed before lifecycle mutation.
func IndexedForkAssignments(repo string, identity forkspace.Identity) ([]ForkAssignmentIndex, []error) {
	dir := forkAssignmentIndexDir(repo, identity)
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	defer root.Close()
	handle, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, []error{err}
	}
	entries, readErr := handle.ReadDir(forkAssignmentIndexCount + 1)
	closeErr := handle.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, []error{errors.Join(readErr, closeErr)}
	}
	if closeErr != nil {
		return nil, []error{closeErr}
	}
	if len(entries) > forkAssignmentIndexCount {
		return nil, []error{fmt.Errorf("fork assignment registry exceeds %d entries", forkAssignmentIndexCount)}
	}
	var records []ForkAssignmentIndex
	var problems []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			problems = append(problems, fmt.Errorf("%s: unsupported fork assignment registry entry", entry.Name()))
			continue
		}
		record, err := readForkAssignmentIndexFile(root, entry.Name())
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		if record.Fork != identity {
			problems = append(problems, fmt.Errorf("%s: assignment belongs to another fork generation", entry.Name()))
			continue
		}
		if entry.Name() != record.AssignmentID+".json" {
			problems = append(problems, fmt.Errorf("%s: assignment filename does not match its identity", entry.Name()))
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].AssignmentID < records[j].AssignmentID })
	return records, problems
}

// AllIndexedForkAssignments enumerates every generation-scoped reverse index. It is the project
// view's source for external --tasks queues and for assignments whose workspace or generation
// record disappeared; configured queue discovery is deliberately not required.
func AllIndexedForkAssignments(repo string) ([]ForkAssignmentIndex, []error) {
	base := filepath.Join(forkspace.StateDir(repo), "assignments")
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, []error{errors.New("fork assignment registry root is not a real directory")}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, []error{err}
	}
	defer root.Close()
	handle, err := root.OpenFile(".", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, []error{err}
	}
	entries, readErr := handle.ReadDir(forkAssignmentIndexCount + 1)
	closeErr := handle.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, []error{errors.Join(readErr, closeErr)}
	}
	if closeErr != nil {
		return nil, []error{closeErr}
	}
	if len(entries) > forkAssignmentIndexCount {
		return nil, []error{fmt.Errorf("fork assignment generation registry exceeds %d entries", forkAssignmentIndexCount)}
	}
	var records []ForkAssignmentIndex
	var problems []error
	for _, entry := range entries {
		entryInfo, err := root.Lstat(entry.Name())
		if err != nil || !entryInfo.IsDir() || entryInfo.Mode()&os.ModeSymlink != 0 {
			problems = append(problems, fmt.Errorf("%s: fork assignment generation is not a real directory", entry.Name()))
			continue
		}
		name, generationText, ok := strings.Cut(entry.Name(), ".")
		if ok {
			// Fork names may contain dots; split from the immutable generation at the right edge.
			if last := strings.LastIndexByte(entry.Name(), '.'); last > 0 {
				name, generationText = entry.Name()[:last], entry.Name()[last+1:]
			}
		}
		identity := forkspace.Identity{Name: name, Generation: forkspace.Generation(generationText)}
		if !ok || !forkspace.ValidExistingName(identity.Name) || !forkspace.ValidGeneration(identity.Generation) {
			problems = append(problems, fmt.Errorf("%s: invalid fork assignment generation identity", entry.Name()))
			continue
		}
		generationRecords, generationProblems := IndexedForkAssignments(repo, identity)
		records = append(records, generationRecords...)
		for _, problem := range generationProblems {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), problem))
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Fork.Name != records[j].Fork.Name {
			return records[i].Fork.Name < records[j].Fork.Name
		}
		if records[i].Fork.Generation != records[j].Fork.Generation {
			return records[i].Fork.Generation < records[j].Fork.Generation
		}
		return records[i].AssignmentID < records[j].AssignmentID
	})
	return records, problems
}

// RemoveForkAssignmentIndex removes only an exact immutable record after landing or discard.
func RemoveForkAssignmentIndex(repo string, expected ForkAssignmentIndex) error {
	root, err := os.OpenRoot(forkAssignmentIndexDir(repo, expected.Fork))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := expected.AssignmentID + ".json"
	current, err := readForkAssignmentIndexFile(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return root.Close()
	}
	if err != nil {
		return errors.Join(err, root.Close())
	}
	if current != expected {
		return errors.Join(errors.New("fork assignment index changed before removal"), root.Close())
	}
	removeErr := root.Remove(name)
	closeErr := root.Close()
	if err := errors.Join(removeErr, closeErr); err != nil {
		return err
	}
	dir := forkAssignmentIndexDir(repo, expected.Fork)
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}
