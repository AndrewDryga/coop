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
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/forkspace"
)

const (
	forkProposalVersion    = 1
	forkProposalFileLimit  = TaskProposalFileLimit
	forkProposalCountLimit = 32
	forkProposalTotalLimit = 1 << 20
)

type ForkProposalKind string

const (
	ForkProposalTask    ForkProposalKind = "task"
	ForkProposalBacklog ForkProposalKind = "backlog"
)

// ForkTaskProposal is the only sandbox-writable discovered-work contract. Destination paths are
// absent by design: kind selects todo/backlog and the host binds it to the assignment's queue.
type ForkTaskProposal struct {
	Version    int              `json:"version"`
	ID         string           `json:"id"`
	Kind       ForkProposalKind `json:"kind"`
	Title      string           `json:"title"`
	Context    string           `json:"context"`
	Acceptance string           `json:"acceptance"`
	Approach   string           `json:"approach"`
	Subtasks   []string         `json:"subtasks"`
}

type ImportedForkProposal struct {
	ProposalID string
	TaskID     string
	Root       string
	Kind       ForkProposalKind
}

func ForkProposalOutbox(owner ForkTaskOwner) string {
	return filepath.Join(filepath.Dir(owner.Projection), "proposals")
}

func ForkProposalOutboxRel(workspace string, owner ForkTaskOwner) (string, error) {
	return ProjectionQueueRel(workspace, ForkProposalOutbox(owner))
}

func validProposalText(value string, allowNewlines bool, limit int) bool {
	if strings.TrimSpace(value) == "" || len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == '\r' || r == 0 || unicode.IsControl(r) && !(allowNewlines && (r == '\n' || r == '\t')) {
			return false
		}
	}
	return true
}

func validateForkTaskProposal(proposal ForkTaskProposal) error {
	if proposal.Version != forkProposalVersion || !validAssignmentID(proposal.ID) {
		return errors.New("fork task proposal requires version 1 and a 32-character lowercase hex id")
	}
	if proposal.Kind != ForkProposalTask && proposal.Kind != ForkProposalBacklog {
		return errors.New("kind must be task or backlog")
	}
	if !validProposalText(proposal.Title, false, TaskTitleLimit) || slugify(proposal.Title) == "" {
		return fmt.Errorf("title must be %s, and contain a letter or digit for its task id", TaskTextRequirement(false, TaskTitleLimit))
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"context", proposal.Context},
		{"acceptance", proposal.Acceptance},
		{"approach", proposal.Approach},
	} {
		if !validProposalText(field.value, true, TaskBlockLimit) {
			return fmt.Errorf("%s must be %s", field.name, TaskTextRequirement(true, TaskBlockLimit))
		}
	}
	if len(proposal.Subtasks) == 0 || len(proposal.Subtasks) > TaskListLimit {
		return fmt.Errorf("subtasks needs 1 to %d entries", TaskListLimit)
	}
	for i, subtask := range proposal.Subtasks {
		if !validProposalText(subtask, false, TaskLineLimit) {
			return fmt.Errorf("subtasks[%d] must be %s", i, TaskTextRequirement(false, TaskLineLimit))
		}
	}
	return nil
}

func decodeForkTaskProposal(data []byte) (ForkTaskProposal, error) {
	dec := json.NewDecoder(io.LimitReader(bytes.NewReader(data), forkProposalFileLimit+1))
	dec.DisallowUnknownFields()
	var proposal ForkTaskProposal
	if err := dec.Decode(&proposal); err != nil {
		return ForkTaskProposal{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return ForkTaskProposal{}, errors.New("fork task proposal contains multiple JSON values")
		}
		return ForkTaskProposal{}, err
	}
	if err := validateForkTaskProposal(proposal); err != nil {
		return ForkTaskProposal{}, err
	}
	return proposal, nil
}

func openRealSubroot(base *os.Root, rel string) (*os.Root, error) {
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("rooted task path escapes its workspace")
	}
	root, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return root, nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			root.Close()
			return nil, errors.New("invalid rooted task path component")
		}
		before, err := root.Lstat(component)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			root.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("rooted task path component %q is not a real directory", component)
		}
		next, err := root.OpenRoot(component)
		if err != nil {
			root.Close()
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			root.Close()
			if err != nil {
				return nil, err
			}
			return nil, errors.New("rooted task path changed while opening")
		}
		root.Close()
		root = next
	}
	return root, nil
}

func ensureRealSubroot(base *os.Root, rel string, mode os.FileMode) (*os.Root, error) {
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("rooted task path escapes its workspace")
	}
	root, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return root, nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		if component == "" || component == "." || component == ".." {
			root.Close()
			return nil, errors.New("invalid rooted task path component")
		}
		before, err := root.Lstat(component)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(component, mode); err != nil && !errors.Is(err, os.ErrExist) {
				root.Close()
				return nil, err
			}
			before, err = root.Lstat(component)
		}
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			root.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("rooted task path component %q is not a real directory", component)
		}
		next, err := root.OpenRoot(component)
		if err != nil {
			root.Close()
			return nil, err
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			next.Close()
			root.Close()
			if err != nil {
				return nil, err
			}
			return nil, errors.New("rooted task path changed while creating")
		}
		root.Close()
		root = next
	}
	return root, nil
}

func ensureForkProposalOutbox(authorityRepo string, owner ForkTaskOwner) error {
	workspace, err := forkspace.OpenGenerationWorkspaceRoot(authorityRepo, owner.Fork)
	if err != nil {
		return err
	}
	defer workspace.Close()
	rel, err := forkProposalParentRel(authorityRepo, owner)
	if err != nil {
		return err
	}
	parent, err := openRealSubroot(workspace, rel)
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := parent.Mkdir("proposals", 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	before, err := parent.Lstat("proposals")
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return err
		}
		return errors.New("fork proposal outbox is not a real directory")
	}
	opened, err := parent.OpenRoot("proposals")
	if err != nil {
		return err
	}
	defer opened.Close()
	after, err := opened.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		if err != nil {
			return err
		}
		return errors.New("fork proposal outbox changed while opening")
	}
	return nil
}

func openForkProposalOutbox(authorityRepo string, owner ForkTaskOwner) (*os.Root, error) {
	workspace, err := forkspace.OpenGenerationWorkspaceRoot(authorityRepo, owner.Fork)
	if err != nil {
		return nil, err
	}
	defer workspace.Close()
	rel, err := forkProposalOutboxRel(authorityRepo, owner)
	if err != nil {
		return nil, err
	}
	return openRealSubroot(workspace, rel)
}

func forkProjectionRel(authorityRepo string, owner ForkTaskOwner) (string, error) {
	if !validAssignmentID(owner.AssignmentID) || !forkspace.ValidExistingName(owner.Fork.Name) ||
		!forkspace.ValidGeneration(owner.Fork.Generation) {
		return "", errors.New("invalid fork projection identity")
	}
	workspace := forkspace.Workspace(authorityRepo, owner.Fork.Name)
	rel := filepath.Join(".coop", "task-executions", string(owner.Fork.Generation), owner.AssignmentID, "tasks")
	if filepath.Clean(owner.Projection) != filepath.Join(workspace, rel) {
		return "", errors.New("fork projection path does not match its host-owned assignment")
	}
	return rel, nil
}

func forkProposalParentRel(authorityRepo string, owner ForkTaskOwner) (string, error) {
	projection, err := forkProjectionRel(authorityRepo, owner)
	if err != nil {
		return "", err
	}
	return filepath.Dir(projection), nil
}

func forkProposalOutboxRel(authorityRepo string, owner ForkTaskOwner) (string, error) {
	parent, err := forkProposalParentRel(authorityRepo, owner)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, "proposals"), nil
}

func readForkProposalFile(root *os.Root, name string) ([]byte, os.FileInfo, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || stat.Nlink != 1 ||
		before.Size() < 0 || before.Size() > forkProposalFileLimit {
		return nil, nil, errors.New("fork task proposal is not a bounded single-link regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("fork task proposal changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, forkProposalFileLimit+1))
	if err != nil || len(data) > forkProposalFileLimit {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("fork task proposal exceeds its size limit")
	}
	return data, before, nil
}

func proposalDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func ownerForProposalIndex(index ForkAssignmentIndex) (ForkTaskOwner, error) {
	record, ok, err := ReadTaskOwnerRecord(index.CanonicalRoot, index.Task.Ref.ID)
	if err != nil || !ok || record.Fork == nil || record.Fork.Fork != index.Fork ||
		record.Fork.AssignmentID != index.AssignmentID || record.Task == nil || *record.Task != index.Task {
		return ForkTaskOwner{}, errors.Join(err, errors.New("fork proposal assignment no longer matches canonical authority"))
	}
	return *record.Fork, nil
}

func proposalTaskFiles(record ForkProposalRecord) map[string]string {
	proposal := *record.Proposal
	values := map[string]string{
		"Context":             proposal.Context,
		"Acceptance criteria": proposal.Acceptance,
		"Approach":            proposal.Approach,
	}
	return newTaskFiles(record.Task.Ref.ID, proposal.Title, record.CreatedAt.Format(time.RFC3339), values, proposal.Subtasks)
}

func proposalTaskIdentity(record ForkProposalRecord) taskIdentityRecord {
	return taskIdentityRecord{Version: taskIdentityVersion, ID: record.Task.Ref.TaskID, CreatedAt: record.CreatedAt}
}

func exactImportedProposal(record ForkProposalRecord) (Item, bool, error) {
	item, ok, err := CurrentTask(record.CanonicalRoot, record.Task.Ref.ID)
	if err != nil {
		return Item{}, false, err
	}
	backlogItems, err := ReadBacklog(record.CanonicalRoot)
	if err != nil {
		return Item{}, false, err
	}
	for _, backlog := range backlogItems {
		if backlog.ID != record.Task.Ref.ID {
			continue
		}
		if ok {
			return Item{}, false, fmt.Errorf("proposal task id %s exists in both lifecycle and backlog", record.Task.Ref.ID)
		}
		item, ok = backlog, true
		break
	}
	if !ok {
		return Item{}, false, nil
	}
	instance, err := ReadTaskInstance(record.CanonicalRoot, item)
	if err != nil || instance.Ref != record.Task.Ref {
		return Item{}, false, errors.Join(err, fmt.Errorf("proposal task id %s collides with another task", record.Task.Ref.ID))
	}
	if record.Phase == ForkProposalImported && instance.Generation != record.Task.Generation {
		return Item{}, false, errors.New("imported proposal task was replaced")
	}
	return item, true, nil
}

func verifyProposalQueue(record ForkProposalRecord) error {
	queue, err := readQueueIdentity(record.CanonicalRoot)
	if err != nil {
		return err
	}
	if queue.ID != record.QueueID || record.Task.Ref.QueueID != record.QueueID {
		return errors.New("fork proposal canonical queue identity changed")
	}
	return nil
}

func materializeForkProposal(record ForkProposalRecord) (ForkProposalRecord, bool, error) {
	if record.Phase == ForkProposalImported {
		_, ok, err := exactImportedProposal(record)
		return record, false, errors.Join(err, func() error {
			if !ok && err == nil {
				return errors.New("imported proposal canonical task is missing")
			}
			return nil
		}())
	}
	if err := verifyProposalQueue(record); err != nil {
		return record, false, err
	}
	authority, err := lockLeaseAuthority(record.CanonicalRoot, record.Task.Ref.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return record, false, fmt.Errorf("proposal task %s is busy in another controller", record.Task.Ref.ID)
		}
		return record, false, err
	}
	defer unlockLeaseFile(authority)
	if _, imported, err := exactImportedProposal(record); err != nil || imported {
		if err != nil {
			return record, false, err
		}
		item, _, _ := exactImportedProposal(record)
		instance, err := ReadTaskInstance(record.CanonicalRoot, item)
		if err != nil {
			return record, false, err
		}
		next := record
		next.Task = instance
		next.Phase = ForkProposalImported
		next.UpdatedAt = time.Now().UTC()
		return next, true, nil
	}
	if pathExists(filepath.Join(record.CanonicalRoot, StateBacklog, record.Task.Ref.ID)) {
		return record, false, fmt.Errorf("proposal task id %s collides with a backlog item", record.Task.Ref.ID)
	}
	if err := ScaffoldStateDirs(record.CanonicalRoot); err != nil {
		return record, false, err
	}
	staging := filepath.Join(record.CanonicalRoot, ".coop-proposal-staging")
	if err := ensureRealDirectory(staging, 0o700); err != nil {
		return record, false, err
	}
	tmp, err := os.MkdirTemp(staging, "."+record.ProposalID+"-")
	if err != nil {
		return record, false, err
	}
	defer os.RemoveAll(tmp)
	opened, err := OpenTaskMetadataRoot(tmp)
	if err != nil {
		return record, false, err
	}
	for name, content := range proposalTaskFiles(record) {
		if err := AtomicWriteTaskFile(opened, name, []byte(content)); err != nil {
			opened.Close()
			return record, false, err
		}
	}
	identityBody, err := json.Marshal(proposalTaskIdentity(record))
	if err == nil {
		err = AtomicWriteTaskFile(opened, TaskIdentityFile, append(identityBody, '\n'))
	}
	if err := errors.Join(err, opened.Close()); err != nil {
		return record, false, err
	}
	destinationState := StateTodo
	if record.Proposal.Kind == ForkProposalBacklog {
		destinationState = StateBacklog
		if err := ensureRealDirectory(filepath.Join(record.CanonicalRoot, StateBacklog), 0o755); err != nil {
			return record, false, err
		}
	}
	target := filepath.Join(record.CanonicalRoot, destinationState, record.Task.Ref.ID)
	if err := os.Rename(tmp, target); err != nil {
		if _, imported, inspectErr := exactImportedProposal(record); inspectErr != nil || !imported {
			return record, false, errors.Join(err, inspectErr)
		}
	}
	item := Item{ID: record.Task.Ref.ID, State: destinationState, Dir: target}
	instance, err := ReadTaskInstance(record.CanonicalRoot, item)
	if err != nil || instance.Ref != record.Task.Ref {
		return record, false, errors.Join(err, errors.New("imported proposal task identity changed during publication"))
	}
	next := record
	next.Task = instance
	next.Phase = ForkProposalImported
	next.UpdatedAt = time.Now().UTC()
	return next, true, nil
}

func removeImportedProposalSource(authorityRepo string, owner ForkTaskOwner, record ForkProposalRecord) error {
	root, err := openForkProposalOutbox(authorityRepo, owner)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	name := record.ProposalID + ".json"
	data, before, err := readForkProposalFile(root, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if proposalDigest(data) != record.SourceDigest {
		return fmt.Errorf("fork task proposal %s was reused with different content", record.ProposalID)
	}
	tmp := fmt.Sprintf(".%s-imported-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	if err := root.Rename(name, tmp); err != nil {
		return err
	}
	moved, err := root.Lstat(tmp)
	if err != nil || !os.SameFile(before, moved) {
		restoreErr := root.Rename(tmp, name)
		if err != nil {
			return errors.Join(err, restoreErr)
		}
		return errors.Join(errors.New("fork task proposal changed before removal"), restoreErr)
	}
	return root.Remove(tmp)
}

func sourceMatchesRecord(authorityRepo string, owner ForkTaskOwner, record ForkProposalRecord, required bool) error {
	root, err := openForkProposalOutbox(authorityRepo, owner)
	if err != nil {
		return err
	}
	defer root.Close()
	data, _, err := readForkProposalFile(root, record.ProposalID+".json")
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return err
	}
	if proposalDigest(data) != record.SourceDigest {
		return fmt.Errorf("fork task proposal %s changed after host preparation", record.ProposalID)
	}
	return nil
}

func reconcileForkProposalRecord(authorityRepo string, owner ForkTaskOwner, record ForkProposalRecord, requireSource bool) (ImportedForkProposal, bool, error) {
	if owner.Fork != record.Fork || owner.AssignmentID != record.AssignmentID || owner.Projection == "" {
		return ImportedForkProposal{}, false, errors.New("fork proposal record does not match its assignment")
	}
	if record.Phase == ForkProposalPrepared {
		if err := sourceMatchesRecord(authorityRepo, owner, record, requireSource); err != nil {
			return ImportedForkProposal{}, false, err
		}
	}
	next, transitioned, err := materializeForkProposal(record)
	if err != nil {
		return ImportedForkProposal{}, false, err
	}
	if transitioned {
		if err := updateForkProposalRecord(authorityRepo, record, next); err != nil {
			return ImportedForkProposal{}, false, err
		}
		record = next
	}
	if err := removeImportedProposalSource(authorityRepo, owner, record); err != nil {
		return ImportedForkProposal{}, false, err
	}
	return ImportedForkProposal{
		ProposalID: record.ProposalID, TaskID: record.Task.Ref.ID,
		Root: record.CanonicalRoot, Kind: func() ForkProposalKind {
			if record.Proposal != nil {
				return record.Proposal.Kind
			}
			return ""
		}(),
	}, transitioned, nil
}

func newForkProposalRecord(index ForkAssignmentIndex, proposal ForkTaskProposal, data []byte) (ForkProposalRecord, error) {
	queue, err := readQueueIdentity(index.CanonicalRoot)
	if err != nil {
		return ForkProposalRecord{}, err
	}
	if queue.ID != index.Task.Ref.QueueID {
		return ForkProposalRecord{}, errors.New("fork proposal assignment queue identity changed")
	}
	taskIdentity, err := newDurableIdentity()
	if err != nil {
		return ForkProposalRecord{}, err
	}
	now := time.Now().UTC()
	readableID := now.Format("2006-01-02") + "-" + slugify(proposal.Title) + "-" + proposal.ID[:8]
	copyProposal := proposal
	record := ForkProposalRecord{
		Version: forkProposalRecordVersion, Fork: index.Fork, AssignmentID: index.AssignmentID,
		CanonicalRoot: index.CanonicalRoot, QueueID: queue.ID, ProposalID: proposal.ID,
		SourceDigest: proposalDigest(data),
		Task:         TaskInstance{Ref: TaskRef{QueueID: queue.ID, TaskID: taskIdentity, ID: readableID}},
		Phase:        ForkProposalPrepared, Proposal: &copyProposal, CreatedAt: now, UpdatedAt: now,
	}
	return record, validateForkProposalRecord(record)
}

func scanForkProposalOutbox(authorityRepo string, index ForkAssignmentIndex, owner ForkTaskOwner, existing []ForkProposalRecord) ([]ImportedForkProposal, error) {
	root, err := openForkProposalOutbox(authorityRepo, owner)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := readDirBounded(root, forkProposalCountLimit)
	if err != nil {
		return nil, fmt.Errorf("read fork proposal outbox: %w", err)
	}
	recordsByID := make(map[string]ForkProposalRecord, len(existing))
	for _, record := range existing {
		recordsByID[record.ProposalID] = record
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	total := 0
	var imported []ImportedForkProposal
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil, fmt.Errorf("fork proposal outbox contains unsupported entry %q", entry.Name())
		}
		data, _, err := readForkProposalFile(root, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read fork task proposal %s: %w", entry.Name(), err)
		}
		total += len(data)
		if total > forkProposalTotalLimit {
			return nil, fmt.Errorf("fork proposal outbox exceeds its %d-byte limit", forkProposalTotalLimit)
		}
		proposal, err := decodeForkTaskProposal(data)
		if err != nil {
			return nil, fmt.Errorf("validate fork task proposal %s: %w", entry.Name(), err)
		}
		if entry.Name() != proposal.ID+".json" {
			return nil, errors.New("fork task proposal filename must match its stable id")
		}
		record, exists := recordsByID[proposal.ID]
		if exists {
			if record.SourceDigest != proposalDigest(data) {
				return nil, fmt.Errorf("fork task proposal %s was reused with different content", proposal.ID)
			}
			result, transitioned, err := reconcileForkProposalRecord(authorityRepo, owner, record, true)
			if err != nil {
				return nil, err
			}
			if transitioned {
				imported = append(imported, result)
			}
			continue
		}
		record, err = newForkProposalRecord(index, proposal, data)
		if err != nil {
			return nil, err
		}
		if err := createForkProposalRecord(authorityRepo, record); err != nil {
			return nil, err
		}
		result, transitioned, err := reconcileForkProposalRecord(authorityRepo, owner, record, true)
		if err != nil {
			return nil, err
		}
		if transitioned {
			imported = append(imported, result)
		}
	}
	return imported, nil
}

// ImportForkProposals replays prepared host records and then captures new outbox entries for one
// exact generation. The fork lifecycle lock fences workspace replacement for the whole transaction.
func ImportForkProposals(authorityRepo string, identity forkspace.Identity) ([]ImportedForkProposal, error) {
	unlock, err := forkspace.LockState(authorityRepo, identity.Name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := forkspace.ValidateGenerationWorkspace(authorityRepo, identity); err != nil {
		return nil, err
	}
	indexes, problems := IndexedForkAssignments(authorityRepo, identity)
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	owners := make(map[string]ForkTaskOwner, len(indexes))
	indexesByAssignment := make(map[string]ForkAssignmentIndex, len(indexes))
	for _, index := range indexes {
		owner, err := ownerForProposalIndex(index)
		if err != nil {
			return nil, err
		}
		owners[index.AssignmentID] = owner
		indexesByAssignment[index.AssignmentID] = index
	}
	records, problems := forkProposalRecords(authorityRepo, identity)
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	recordsByAssignment := make(map[string][]ForkProposalRecord)
	var imported []ImportedForkProposal
	for _, record := range records {
		owner, ok := owners[record.AssignmentID]
		if !ok {
			return nil, fmt.Errorf("fork proposal %s has no exact assignment", record.ProposalID)
		}
		index := indexesByAssignment[record.AssignmentID]
		if record.CanonicalRoot != index.CanonicalRoot || record.QueueID != index.Task.Ref.QueueID {
			return nil, errors.New("fork proposal record changed canonical queue authority")
		}
		result, transitioned, err := reconcileForkProposalRecord(authorityRepo, owner, record, false)
		if err != nil {
			return nil, err
		}
		if transitioned {
			imported = append(imported, result)
		}
		updated, errs := forkProposalRecordsForAssignment(authorityRepo, identity, record.AssignmentID)
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		recordsByAssignment[record.AssignmentID] = updated
	}
	for _, index := range indexes {
		existing := recordsByAssignment[index.AssignmentID]
		if existing == nil {
			existing, problems = forkProposalRecordsForAssignment(authorityRepo, identity, index.AssignmentID)
			if len(problems) > 0 {
				return nil, errors.Join(problems...)
			}
		}
		results, err := scanForkProposalOutbox(authorityRepo, index, owners[index.AssignmentID], existing)
		if err != nil {
			return nil, err
		}
		imported = append(imported, results...)
	}
	return imported, nil
}

func forkProposalsDrained(authorityRepo string, identity forkspace.Identity) error {
	records, problems := forkProposalRecords(authorityRepo, identity)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	for _, record := range records {
		if record.Phase != ForkProposalImported {
			return errors.New("fork has an unfinished task proposal import")
		}
	}
	indexes, problems := IndexedForkAssignments(authorityRepo, identity)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	for _, index := range indexes {
		owner, err := ownerForProposalIndex(index)
		if err != nil {
			return err
		}
		root, err := openForkProposalOutbox(authorityRepo, owner)
		if err != nil {
			return err
		}
		entries, readErr := readDirBounded(root, forkProposalCountLimit)
		root.Close()
		if readErr != nil {
			return readErr
		}
		if len(entries) > 0 {
			return fmt.Errorf("fork assignment %s has unimported task proposals", index.AssignmentID)
		}
	}
	return nil
}

func finalizeForkProposalRecords(authorityRepo string, index ForkAssignmentIndex) error {
	records, problems := forkProposalRecordsForAssignment(authorityRepo, index.Fork, index.AssignmentID)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	for _, record := range records {
		if record.Phase != ForkProposalImported {
			return errors.New("cannot finalize assignment with a prepared task proposal")
		}
		if _, ok, err := exactImportedProposal(record); err != nil || !ok {
			return errors.Join(err, errors.New("imported proposal task is missing before assignment finalization"))
		}
		if err := removeForkProposalRecord(authorityRepo, record); err != nil {
			return err
		}
	}
	_ = os.Remove(forkProposalAssignmentRecordRoot(authorityRepo, index.Fork, index.AssignmentID))
	return nil
}

func discardForkProposalsLocked(authorityRepo string, identity forkspace.Identity) error {
	records, problems := forkProposalRecords(authorityRepo, identity)
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	for _, record := range records {
		if item, exists, err := exactImportedProposal(record); err != nil {
			return err
		} else if exists && record.Phase == ForkProposalPrepared {
			instance, err := ReadTaskInstance(record.CanonicalRoot, item)
			if err != nil {
				return err
			}
			next := record
			next.Task = instance
			next.Phase = ForkProposalImported
			next.UpdatedAt = time.Now().UTC()
			if err := updateForkProposalRecord(authorityRepo, record, next); err != nil {
				return err
			}
			record = next
		}
		if err := removeForkProposalRecord(authorityRepo, record); err != nil {
			return err
		}
	}
	_ = os.RemoveAll(forkProposalRecordRoot(authorityRepo, identity))
	return nil
}
