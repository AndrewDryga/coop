package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const (
	completionWindowIndexID    = "\x00completion-windows"
	completionWindowLockPrefix = "\x00completion-window:"
	completionWindowVersion    = 1
	completionWindowIndexLimit = 16 << 20
)

var (
	ErrCompletionWindowSetup = errors.New("completion window setup failed")
	ErrCompletionWindowAudit = errors.New("completion window audit failed")
)

type CompletionFingerprint struct {
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
	ChangeSec   int64  `json:"change_sec"`
	ChangeNsec  int64  `json:"change_nsec"`
	Receipt     string `json:"receipt,omitempty"`
	ReceiptBusy bool   `json:"receipt_busy,omitempty"`
	Tree        string `json:"tree"`
}

type CompletionWindowRecord struct {
	Baseline              map[string]CompletionFingerprint `json:"baseline"`
	AllowedDoneDeparture  string                           `json:"allowed_done_departure,omitempty"`
	AllowedDoneDepartures []string                         `json:"allowed_done_departures,omitempty"`
	ReviewWindow          bool                             `json:"review_window,omitempty"`
	ReviewSubjects        []string                         `json:"review_subjects,omitempty"`
	ReviewSubjectScoped   bool                             `json:"review_subject_scoped,omitempty"`
	WorkWindow            bool                             `json:"work_window,omitempty"`
	WorkSubject           string                           `json:"work_subject,omitempty"`
	BaselineMutations     []string                         `json:"baseline_mutations,omitempty"`
	RecoveredDepartures   []string                         `json:"recovered_departures,omitempty"`
}

type completionWindowIndex struct {
	Version int                               `json:"version"`
	Windows map[string]CompletionWindowRecord `json:"windows"`
}

type completionWindow struct {
	root   string
	id     string
	record CompletionWindowRecord
	live   *os.File
}

type CompletionWindowSet struct {
	windows []completionWindow
	scan    func(string, map[string]CompletionFingerprint) ([]QueuedTask, error)
}

func snapshotDoneCompletions(root string) (map[string]CompletionFingerprint, error) {
	snapshot := map[string]CompletionFingerprint{}
	for _, task := range ReadTaskTree(root) {
		if task.State != StateDone {
			continue
		}
		fingerprint, err := CompletionFingerprintFor(root, task)
		if err != nil {
			return nil, err
		}
		snapshot[task.ID] = fingerprint
	}
	return snapshot, nil
}

func CompletionFingerprintFor(root string, task Item) (CompletionFingerprint, error) {
	accepted, valid, busy := inspectTaskCompletionReceipt(root, task)
	if !valid {
		accepted.Nonce = ""
	}
	return completionFingerprintWithReceipt(task, accepted.Nonce, busy)
}

func completionFingerprintWithReceipt(task Item, receipt string, receiptBusy bool) (CompletionFingerprint, error) {
	info, err := os.Lstat(task.Dir)
	if err != nil {
		return CompletionFingerprint{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok {
		return CompletionFingerprint{}, fmt.Errorf("task completion path %q is not a real directory", task.Dir)
	}
	sec, nsec := statChangeTime(stat)
	tree, err := completionTreeMetadataDigest(task.Dir)
	if err != nil {
		return CompletionFingerprint{}, err
	}
	return CompletionFingerprint{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), ChangeSec: sec, ChangeNsec: nsec,
		Receipt: receipt, ReceiptBusy: receiptBusy, Tree: tree,
	}, nil
}

func completionFingerprintLocked(task Item, lock crashCompletionLock) (CompletionFingerprint, error) {
	receipt, valid := lock.completionReceipt(task.Dir)
	if !valid {
		receipt.Nonce = ""
	}
	return completionFingerprintWithReceipt(task, receipt.Nonce, false)
}

// completionTreeMetadataDigest detects in-place writes below a done task without reading task
// contents. File ctime cannot be restored by the boxed provider, while sorted relative paths and
// inode/type/size metadata also bind adds, removals, replacements, links, and nested artifacts.
func completionTreeMetadataDigest(taskDir string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(taskDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == taskDir {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("task completion child %q has unsupported file metadata", path)
		}
		rel, err := filepath.Rel(taskDir, path)
		if err != nil {
			return err
		}
		sec, nsec := statChangeTime(stat)
		_, err = fmt.Fprintf(hash, "%d:%s\x00%d:%d:%d:%d:%d:%d\x00", len(rel), rel, uint32(info.Mode()), info.Size(), uint64(stat.Dev), uint64(stat.Ino), sec, nsec)
		return err
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func changedDoneCompletions(root string, baseline map[string]CompletionFingerprint) ([]QueuedTask, error) {
	var changed []QueuedTask
	for _, task := range ReadTaskTree(root) {
		if task.State != StateDone {
			continue
		}
		current, err := CompletionFingerprintFor(root, task)
		if err != nil {
			return nil, err
		}
		before, existed := baseline[task.ID]
		if !existed || before.Device != current.Device || before.Inode != current.Inode ||
			before.ChangeSec != current.ChangeSec || before.ChangeNsec != current.ChangeNsec ||
			before.Tree != current.Tree || before.Receipt != current.Receipt || before.ReceiptBusy || current.ReceiptBusy {
			changed = append(changed, QueuedTask{Root: root, Item: task})
		}
	}
	slices.SortFunc(changed, func(a, b QueuedTask) int {
		if byID := strings.Compare(a.Item.ID, b.Item.ID); byID != 0 {
			return byID
		}
		return strings.Compare(a.Root, b.Root)
	})
	return changed, nil
}

type completionCandidateKind uint8

const (
	completionCandidateUnchanged completionCandidateKind = iota
	completionCandidateReplay
	completionCandidateMutation
)

func completionFingerprintChanged(before, current CompletionFingerprint) bool {
	return before.Device != current.Device || before.Inode != current.Inode ||
		before.ChangeSec != current.ChangeSec || before.ChangeNsec != current.ChangeNsec ||
		before.Tree != current.Tree || before.Receipt != current.Receipt ||
		before.ReceiptBusy || current.ReceiptBusy
}

func classifyCompletionCandidate(
	record CompletionWindowRecord,
	id string,
	current CompletionFingerprint,
) completionCandidateKind {
	before, existed := record.Baseline[id]
	if !existed {
		return completionCandidateReplay
	}
	if !completionFingerprintChanged(before, current) &&
		!slices.Contains(record.BaselineMutations, id) {
		return completionCandidateUnchanged
	}
	// A fresh host receipt can supersede a crash-persisted mutation marker. A window whose
	// baseline already carries that same receipt still proves any later metadata mutation.
	if current.Receipt != "" && (before.ReceiptBusy || before.Receipt != current.Receipt) {
		return completionCandidateReplay
	}
	// Trusted reopen invalidates the old receipt before moving the folder. If the controller dies
	// in that gap, startup must finish the intended departure instead of blessing the old archive.
	if current.Receipt == "" && completionWindowAllowsDoneDeparture(record, id) {
		return completionCandidateReplay
	}
	return completionCandidateMutation
}

// completionReplayCandidates separates lifecycle arrivals from mutations of folders that were
// already archived when the window began. Startup recovery re-runs this classification from
// authority-locked fingerprints before it writes markers or mutates lifecycle state.
func completionReplayCandidates(
	root string,
	record CompletionWindowRecord,
	candidates []QueuedTask,
) (replay []QueuedTask, baselineMutations []QueuedTask, err error) {
	for _, candidate := range candidates {
		current, fingerprintErr := CompletionFingerprintFor(root, candidate.Item)
		if fingerprintErr != nil {
			return nil, nil, fingerprintErr
		}
		if current.ReceiptBusy {
			return nil, nil, fmt.Errorf("task %s completion receipt is busy", candidate.Item.ID)
		}
		switch classifyCompletionCandidate(record, candidate.Item.ID, current) {
		case completionCandidateReplay:
			replay = append(replay, candidate)
		case completionCandidateMutation:
			baselineMutations = append(baselineMutations, candidate)
		}
	}
	return replay, baselineMutations, nil
}

func baselineCompletionMutationError(tasks []QueuedTask) error {
	if len(tasks) == 0 {
		return nil
	}
	ids := make([]string, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].Item.ID
	}
	return fmt.Errorf(
		"archived task metadata changed before completion-window recovery for %s; "+
			"the tasks were already in the window baseline and remain done — inspect or restore "+
			"their task metadata manually",
		strings.Join(ids, ", "),
	)
}

func invalidateStaleCandidateReceipts(root string, baseline map[string]CompletionFingerprint, candidates []QueuedTask) error {
	var errs []error
	for _, candidate := range candidates {
		if candidate.Root != root {
			continue
		}
		before, existed := baseline[candidate.Item.ID]
		if !existed || before.Receipt == "" {
			continue
		}
		current, err := CompletionFingerprintFor(root, candidate.Item)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		pathChanged := before.Device != current.Device || before.Inode != current.Inode ||
			before.ChangeSec != current.ChangeSec || before.ChangeNsec != current.ChangeNsec || before.Tree != current.Tree
		if pathChanged && current.Receipt == before.Receipt && !current.ReceiptBusy {
			_, clearErr := clearTaskCompletionReceiptIfMatches(root, candidate.Item, before.Receipt)
			errs = append(errs, clearErr)
		}
	}
	return errors.Join(errs...)
}

func completionWindowAllowsDoneDeparture(record CompletionWindowRecord, id string) bool {
	return id == record.AllowedDoneDeparture || slices.Contains(record.AllowedDoneDepartures, id)
}

func CompletionWindowIndexName(root string) (string, error) {
	key, err := LeaseAuthorityKey(root, completionWindowIndexID)
	return key + ".windows.json", err
}

func ReadCompletionWindowIndex(root string) (completionWindowIndex, error) {
	name, err := CompletionWindowIndexName(root)
	if err != nil {
		return completionWindowIndex{}, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return completionWindowIndex{}, err
	}
	defer registry.Close()
	before, err := registry.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return completionWindowIndex{Version: completionWindowVersion, Windows: map[string]CompletionWindowRecord{}}, nil
	}
	if err != nil {
		return completionWindowIndex{}, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || !ok || stat.Nlink != 1 || before.Size() > completionWindowIndexLimit {
		return completionWindowIndex{}, fmt.Errorf("completion window index %q is not a bounded single-link regular file", name)
	}
	file, err := registry.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return completionWindowIndex{}, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		if err != nil {
			return completionWindowIndex{}, err
		}
		return completionWindowIndex{}, fmt.Errorf("completion window index %q changed while opening", name)
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !after.Mode().IsRegular() || !ok || afterStat.Nlink != 1 || after.Size() > completionWindowIndexLimit {
		return completionWindowIndex{}, fmt.Errorf("completion window index %q is not a bounded single-link regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, completionWindowIndexLimit+1))
	if err != nil {
		return completionWindowIndex{}, err
	}
	if len(data) > completionWindowIndexLimit {
		return completionWindowIndex{}, fmt.Errorf("completion window index %q exceeds %d bytes", name, completionWindowIndexLimit)
	}
	var index completionWindowIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return completionWindowIndex{}, fmt.Errorf("decode completion window index %q: %w", name, err)
	}
	if index.Version != completionWindowVersion {
		return completionWindowIndex{}, fmt.Errorf("completion window index %q has unsupported version %d", name, index.Version)
	}
	if index.Windows == nil {
		return completionWindowIndex{}, fmt.Errorf("completion window index %q is missing its windows map", name)
	}
	return index, nil
}

func writeCompletionWindowIndex(root string, index completionWindowIndex) error {
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if len(data)+1 > completionWindowIndexLimit {
		return fmt.Errorf("completion window index exceeds %d bytes", completionWindowIndexLimit)
	}
	name, err := CompletionWindowIndexName(root)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	return AtomicWriteTaskFile(registry, name, append(data, '\n'))
}

func lockCompletionWindowIndex(root string) (*os.File, completionWindowIndex, error) {
	file, err := lockLeaseAuthority(root, completionWindowIndexID, true, syscall.LOCK_EX)
	if err != nil {
		return nil, completionWindowIndex{}, err
	}
	index, err := ReadCompletionWindowIndex(root)
	if err != nil {
		_ = unlockLeaseFile(file)
		return nil, completionWindowIndex{}, err
	}
	return file, index, nil
}

func BeginCompletionWindows(hosts []string) (*CompletionWindowSet, error) {
	return beginCompletionWindowsWithPolicy(hosts, nil, nil, false)
}

func duplicateReviewTaskIDs(hosts, ids []string) []string {
	duplicates := aggregateDuplicateTaskIDs(hosts)
	var relevant []string
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		if slices.Contains(duplicates, id) {
			relevant = append(relevant, id)
		}
	}
	return relevant
}

// beginReviewCompletionWindows opens review-stage windows bound to the exact task ids under
// review, persisting the subject set in the journal record so finishReview and crash replay
// apply the same scope: subjects stay strict, other completions are classified by ownership.
// ReviewWindow stays explicit because an empty unscoped subject list is strict for preflight/verify,
// while ReviewSubjectScoped preserves concurrent-host semantics if authoritative deletion later
// removes the last id from a subject-scoped review.
func BeginReviewCompletionWindows(hosts, subjects []string) (*CompletionWindowSet, error) {
	if duplicates := duplicateReviewTaskIDs(hosts, subjects); len(duplicates) > 0 {
		return nil, fmt.Errorf("review subject id(s) %s exist in multiple task queues", strings.Join(duplicates, ", "))
	}
	return beginCompletionWindowsWithPolicy(hosts, nil, subjects, true)
}

// beginWorkCompletionWindows journals the exact leased task. A work box cannot change its own
// subject's completion state outside the normal finalization path, but a host-receipted move of a
// different archived task is concurrent queue activity rather than box ownership churn.
func BeginWorkCompletionWindows(hosts []string, subject string) (*CompletionWindowSet, error) {
	if subject == "" {
		return nil, errors.New("work completion window is missing its assigned subject")
	}
	if duplicates := duplicateReviewTaskIDs(hosts, []string{subject}); len(duplicates) > 0 {
		return nil, fmt.Errorf("work subject id(s) %s exist in multiple task queues", strings.Join(duplicates, ", "))
	}
	return beginCompletionWindowsWithPolicy(hosts, nil, nil, false, subject)
}

func beginCompletionWindowsAllowing(hosts []string, taskID string) (*CompletionWindowSet, error) {
	return beginCompletionWindowsAllowingTasks(hosts, []string{taskID})
}

func beginCompletionWindowsAllowingTasks(hosts, taskIDs []string) (*CompletionWindowSet, error) {
	return beginCompletionWindowsWithPolicy(hosts, taskIDs, nil, false)
}

func beginCompletionWindowsWithPolicy(hosts, allowedDoneDepartures, reviewSubjects []string, reviewWindow bool, workSubjects ...string) (*CompletionWindowSet, error) {
	if len(hosts) == 0 {
		return &CompletionWindowSet{}, nil
	}
	id, err := newSupervisorID()
	if err != nil {
		return nil, err
	}
	set := &CompletionWindowSet{}
	for _, root := range hosts {
		// Snapshot and register under one index critical section. Task deletion uses the same
		// boundary, so a window cannot capture a task before deletion and register that stale
		// baseline after deletion has purged the index.
		indexFile, index, err := lockCompletionWindowIndex(root)
		if err != nil {
			return nil, errors.Join(err, set.Close())
		}
		baseline, err := snapshotDoneCompletions(root)
		if err != nil {
			return nil, errors.Join(err, unlockLeaseFile(indexFile), set.Close())
		}
		// The window's lock name embeds a freshly minted supervisor id, so nothing else can own it:
		// on any failure to take it — including a failure to open it — the record is ours to remove.
		live, err := lockLeaseAuthority(root, completionWindowLockPrefix+id, true, syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil {
			removeErr := removeLeaseAuthorityLock(root, completionWindowLockPrefix+id)
			return nil, errors.Join(err, removeErr, unlockLeaseFile(indexFile), set.Close())
		}
		record := CompletionWindowRecord{
			Baseline:              baseline,
			AllowedDoneDepartures: slices.Compact(slices.Sorted(slices.Values(allowedDoneDepartures))),
			ReviewWindow:          reviewWindow,
			ReviewSubjects:        slices.Compact(slices.Sorted(slices.Values(reviewSubjects))),
			ReviewSubjectScoped:   len(reviewSubjects) > 0,
		}
		if len(workSubjects) == 1 {
			record.WorkWindow, record.WorkSubject = true, workSubjects[0]
		}
		index.Windows[id] = record
		writeErr := writeCompletionWindowIndex(root, index)
		unlockErr := unlockLeaseFile(indexFile)
		if writeErr != nil {
			liveUnlockErr := unlockLeaseFile(live)
			var removeErr error
			if liveUnlockErr == nil {
				removeErr = removeLeaseAuthorityLock(root, completionWindowLockPrefix+id)
			}
			return nil, errors.Join(writeErr, unlockErr, liveUnlockErr, removeErr, set.Close())
		}
		if unlockErr != nil {
			return nil, errors.Join(unlockErr, unlockLeaseFile(live), set.Close())
		}
		set.windows = append(set.windows, completionWindow{root: root, id: id, record: record, live: live})
	}
	return set, nil
}

func (s *CompletionWindowSet) AuditDoneCandidates(assigned QueuedTask) ([]QueuedTask, []string, error) {
	candidates, err := s.candidates()
	if err != nil {
		return nil, nil, err
	}
	for _, window := range s.windows {
		if err := invalidateStaleCandidateReceipts(window.root, window.record.Baseline, candidates); err != nil {
			return candidates, nil, err
		}
	}
	rejected, err := rejectUnownedCompletions(candidates, assigned)
	return candidates, rejected, err
}

func completionWindowBaselineChanges(root string, record CompletionWindowRecord) ([]Item, []string) {
	currentByID := map[string]Item{}
	for _, task := range ReadTaskTree(root) {
		currentByID[task.ID] = task
	}
	var departed []Item
	var missing []string
	for id := range record.Baseline {
		current, ok := currentByID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if current.State != StateDone {
			departed = append(departed, current)
		}
	}
	slices.SortFunc(departed, func(a, b Item) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(missing)
	return departed, missing
}

func completionWindowDepartures(root string, record CompletionWindowRecord) ([]string, error) {
	departed, missing := completionWindowBaselineChanges(root, record)
	if len(missing) > 0 {
		return nil, fmt.Errorf("task(s) %s left the queue during this completion window; restore the missing task folder to a lifecycle state, then re-run `coop loop`", strings.Join(missing, ", "))
	}
	var disallowed []string
	for _, task := range departed {
		if slices.Contains(record.RecoveredDepartures, task.ID) {
			continue
		}
		if record.WorkWindow {
			if task.ID == record.WorkSubject {
				disallowed = append(disallowed, task.ID)
				continue
			}
			before := record.Baseline[task.ID]
			departure, ok, err := readTrustedDoneDeparture(root, task.ID)
			if err != nil {
				return nil, fmt.Errorf("read trusted departure for task %s: %w", task.ID, err)
			}
			if before.Receipt == "" || before.ReceiptBusy || !ok || !slices.Contains(departure.Nonces, before.Receipt) {
				disallowed = append(disallowed, task.ID)
			}
			continue
		}
		if !completionWindowAllowsDoneDeparture(record, task.ID) {
			disallowed = append(disallowed, task.ID)
		}
	}
	return disallowed, nil
}

func (s *CompletionWindowSet) Departures() ([]string, error) {
	if s == nil {
		return nil, nil
	}
	var departed []string
	for _, window := range s.windows {
		ids, err := completionWindowDepartures(window.root, window.record)
		if err != nil {
			return nil, err
		}
		departed = append(departed, ids...)
	}
	slices.Sort(departed)
	return slices.Compact(departed), nil
}

// baselineDoneIDs returns every task id that was already archived (in 99_done) when this window's
// baseline snapshot was captured — for the work window (beginWorkCompletionWindows), that snapshot is
// taken before the box is even launched, so it is queue state the box could not have influenced.
// internal/loop folds this into the foreign-binding touched set: a Coop-Task trailer naming an already
// archived task is always authority-touching, even when that task's folder never moves during the
// iteration, because an archived task's history is meant to be closed — a forged extra commit
// corrupts that closed record without needing to touch its folder at all. See unbindableTasks and
// .agent/kb/loop-range-rejects-outside-commits.md.
func (s *CompletionWindowSet) BaselineDoneIDs() map[string]bool {
	ids := map[string]bool{}
	if s == nil {
		return ids
	}
	for _, window := range s.windows {
		for id := range window.record.Baseline {
			ids[id] = true
		}
	}
	return ids
}

func (s *CompletionWindowSet) candidates() ([]QueuedTask, error) {
	if s == nil {
		return nil, nil
	}
	var candidates []QueuedTask
	scan := s.scan
	if scan == nil {
		scan = changedDoneCompletions
	}
	for _, window := range s.windows {
		changed, err := scan(window.root, window.record.Baseline)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, changed...)
	}
	slices.SortFunc(candidates, func(a, b QueuedTask) int {
		if byID := strings.Compare(a.Item.ID, b.Item.ID); byID != 0 {
			return byID
		}
		return strings.Compare(a.Root, b.Root)
	})
	return candidates, nil
}

func (s *CompletionWindowSet) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	for i := range s.windows {
		window := &s.windows[i]
		indexFile, index, err := lockCompletionWindowIndex(window.root)
		if err == nil {
			delete(index.Windows, window.id)
			writeErr := writeCompletionWindowIndex(window.root, index)
			err = errors.Join(writeErr, unlockLeaseFile(indexFile))
			if writeErr == nil {
				unlockErr := unlockLeaseFile(window.live)
				err = errors.Join(err, unlockErr)
				if unlockErr == nil {
					err = errors.Join(err, removeLeaseAuthorityLock(window.root, completionWindowLockPrefix+window.id))
				}
				window.live = nil
			}
		}
		if window.live != nil {
			err = errors.Join(err, unlockLeaseFile(window.live))
			window.live = nil
		}
		errs = append(errs, err)
	}
	s.windows = nil
	return errors.Join(errs...)
}

// abandon releases liveness while retaining the durable record. Startup can then replay the same
// window; close is reserved for an audit whose every candidate was accepted or restored.
func (s *CompletionWindowSet) Abandon() error {
	if s == nil {
		return nil
	}
	var errs []error
	for i := range s.windows {
		if s.windows[i].live != nil {
			errs = append(errs, unlockLeaseFile(s.windows[i].live))
			s.windows[i].live = nil
		}
	}
	s.windows = nil
	return errors.Join(errs...)
}

func (s *CompletionWindowSet) rejectAndClose(assigned QueuedTask) error {
	_, rejected, err := s.AuditDoneCandidates(assigned)
	if err != nil {
		return errors.Join(err, s.Abandon())
	}
	departed, err := s.Departures()
	if err != nil {
		return errors.Join(err, s.Abandon())
	}
	var ownershipErr error
	if len(rejected) > 0 {
		ownershipErr = UnownedCompletionError(rejected, nil)
	}
	var departureErr error
	if len(departed) > 0 {
		departureErr = fmt.Errorf("completion ownership changed: archived task(s) %s left done during a stage that may not reopen them", strings.Join(departed, ", "))
	}
	return errors.Join(ownershipErr, departureErr, s.Close())
}

func classifyReviewCompletionCandidates(candidates []QueuedTask, rejected, subjectIDs []string, subjectScoped bool) (changed, concurrent []string) {
	subjects := make(map[string]bool, len(subjectIDs))
	for _, id := range subjectIDs {
		subjects[id] = true
	}
	rejectedSet := make(map[string]bool, len(rejected))
	for _, id := range rejected {
		rejectedSet[id] = true
	}
	for _, candidate := range candidates {
		switch id := candidate.Item.ID; {
		case (!subjectScoped && len(subjects) == 0) || subjects[id]:
			changed = append(changed, id)
		case !rejectedSet[id]:
			concurrent = append(concurrent, id)
		}
	}
	return slices.Compact(changed), slices.Compact(concurrent) // candidates are sorted by id
}

func (s *CompletionWindowSet) reviewScope() (hosts, subjectIDs []string, subjectScoped bool) {
	for _, window := range s.windows {
		hosts = append(hosts, window.root)
		subjectIDs = append(subjectIDs, window.record.ReviewSubjects...)
		subjectScoped = subjectScoped || window.record.ReviewSubjectScoped || len(window.record.ReviewSubjects) > 0
	}
	return slices.Compact(slices.Sorted(slices.Values(hosts))),
		slices.Compact(slices.Sorted(slices.Values(subjectIDs))),
		subjectScoped
}

// auditReview applies one snapshot comparison without closing the journal. scanErr means the
// comparison itself could not be trusted and the live window must be abandoned for replay;
// reviewErr is a completed comparison that found lifecycle or ownership churn.
func (s *CompletionWindowSet) auditReview(hosts, subjectIDs []string, subjectScoped bool) (concurrent []string, scanErr, reviewErr error) {
	candidates, rejected, err := s.AuditDoneCandidates(QueuedTask{})
	if err != nil {
		return nil, err, nil
	}
	changed, concurrent := classifyReviewCompletionCandidates(candidates, rejected, subjectIDs, subjectScoped)
	var changedErr error
	if len(changed) > 0 {
		changedErr = fmt.Errorf("review completion set changed for %s", strings.Join(changed, ", "))
	}
	var ownershipErr error
	if len(rejected) > 0 {
		ownershipErr = UnownedCompletionError(rejected, nil)
	}
	relevant := slices.Clone(subjectIDs)
	for _, candidate := range candidates {
		relevant = append(relevant, candidate.Item.ID)
	}
	var duplicateErr error
	if duplicates := duplicateReviewTaskIDs(hosts, relevant); len(duplicates) > 0 {
		duplicateErr = fmt.Errorf("review task id(s) %s became ambiguous across task queues", strings.Join(duplicates, ", "))
	}
	departed, departureErr := s.Departures()
	if departureErr == nil && len(departed) > 0 {
		departureErr = fmt.Errorf("review task lifecycle changed for %s; reviews must report verdicts for host application", strings.Join(departed, ", "))
	}
	return concurrent, nil, errors.Join(changedErr, ownershipErr, duplicateErr, departureErr)
}

// finishReview closes a review-stage window, scoping strictness to the exact subjects the
// window recorded at begin. Any completion churn on a subject fails closed, as does a
// completion no host authority finalized (rejected as unowned and restored). A non-subject
// completion that survives ownership rejection was finalized by a parallel host controller —
// concurrent host activity, not this review's mutation — so it is returned to the caller for
// the next review round's baseline instead of failing the run. After closing the durable window,
// the saved snapshot is audited once more so activity in the scan-to-close handoff cannot escape.
// Tolerance exists ONLY under an explicit subject contract: a window with no recorded subjects
// and no persisted subject-scoped marker (preflight, a subject-free verify) stays fully strict.
func (s *CompletionWindowSet) FinishReview() ([]string, error) {
	hosts, subjectIDs, subjectScoped := s.reviewScope()
	closed := &CompletionWindowSet{windows: slices.Clone(s.windows), scan: s.scan}
	for i := range closed.windows {
		closed.windows[i].live = nil
	}
	concurrent, scanErr, reviewErr := s.auditReview(hosts, subjectIDs, subjectScoped)
	if scanErr != nil {
		return nil, errors.Join(scanErr, s.Abandon())
	}
	if err := errors.Join(reviewErr, s.Close()); err != nil {
		return nil, err
	}
	afterClose, scanErr, reviewErr := closed.auditReview(hosts, subjectIDs, subjectScoped)
	if err := errors.Join(scanErr, reviewErr); err != nil {
		return nil, err
	}
	return slices.Compact(slices.Sorted(slices.Values(append(concurrent, afterClose...)))), nil
}

type completionWindowRecovery struct {
	id         string
	record     CompletionWindowRecord
	live       *os.File
	candidates []QueuedTask
	replay     []QueuedTask
}

func markedCompletionCandidates(
	root string,
	candidates []QueuedTask,
	marked []string,
) []QueuedTask {
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		seen[candidate.Item.ID] = true
	}
	for _, id := range marked {
		if seen[id] {
			continue
		}
		task, ok := CurrentTask(root, id)
		if ok && task.State == StateDone {
			candidates = append(candidates, QueuedTask{Root: root, Item: task})
			seen[id] = true
		}
	}
	slices.SortFunc(candidates, func(a, b QueuedTask) int {
		return strings.Compare(a.Item.ID, b.Item.ID)
	})
	return candidates
}

func setRecordBaselineMutations(record *CompletionWindowRecord, protected map[string]bool) bool {
	var marked []string
	for id := range record.Baseline {
		if protected[id] {
			marked = append(marked, id)
		}
	}
	slices.Sort(marked)
	if slices.Equal(marked, record.BaselineMutations) {
		return false
	}
	record.BaselineMutations = marked
	return true
}

func recordRecoveredDepartures(record *CompletionWindowRecord, restored map[string]bool) bool {
	recovered := slices.Clone(record.RecoveredDepartures)
	for id := range record.Baseline {
		if restored[id] {
			recovered = append(recovered, id)
		}
	}
	recovered = slices.Compact(slices.Sorted(slices.Values(recovered)))
	if slices.Equal(recovered, record.RecoveredDepartures) {
		return false
	}
	record.RecoveredDepartures = recovered
	return true
}

type lockedCompletionCandidate struct {
	task        QueuedTask
	current     Item
	fingerprint CompletionFingerprint
	lock        crashCompletionLock
}

// lockCompletionCandidates closes classification's receipt race. Recovery derives its final
// disposition from these locked fingerprints, persists the matching journal evidence, and retains
// every lock until task mutation and journal retirement are durable.
func lockCompletionCandidates(
	root string,
	tasks map[string]QueuedTask,
) (map[string]lockedCompletionCandidate, error) {
	ids := slices.Sorted(maps.Keys(tasks))
	locked := make(map[string]lockedCompletionCandidate, len(ids))
	release := func() error {
		return releaseLockedCompletionCandidates(locked)
	}
	for _, id := range ids {
		candidate := tasks[id]
		lock, current, acquired, err := lockCrashCompletion(root, candidate.Item)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("lock completion-window task %s: %w", id, err),
				release(),
			)
		}
		if !acquired {
			return nil, errors.Join(
				fmt.Errorf("completion-window task %s changed state or has an authoritative owner; retry recovery", id),
				release(),
			)
		}
		fingerprint, err := completionFingerprintLocked(current, lock)
		if err != nil {
			_ = lock.release()
			return nil, errors.Join(
				fmt.Errorf("fingerprint locked completion-window task %s: %w", id, err),
				release(),
			)
		}
		locked[id] = lockedCompletionCandidate{
			task: candidate, current: current, fingerprint: fingerprint, lock: lock,
		}
	}
	return locked, nil
}

func releaseLockedCompletionCandidates(locked map[string]lockedCompletionCandidate) error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(locked)) {
		errs = append(errs, locked[id].lock.release())
	}
	return errors.Join(errs...)
}

func verifyLockedCompletionCandidates(locked map[string]lockedCompletionCandidate) error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(locked)) {
		candidate := locked[id]
		current, err := completionFingerprintLocked(candidate.current, candidate.lock)
		if err != nil {
			errs = append(errs, fmt.Errorf("re-fingerprint locked completion-window task %s: %w", id, err))
			continue
		}
		if completionFingerprintChanged(candidate.fingerprint, current) {
			errs = append(errs, fmt.Errorf(
				"completion-window task %s metadata changed while recovery held task authority; retry recovery",
				id,
			))
		}
	}
	return errors.Join(errs...)
}

func ReconcileCompletionWindowsWithActivity(hosts []string) ([]string, error) {
	var concurrent []string
	var errs []error
	for _, root := range hosts {
		indexFile, index, err := lockCompletionWindowIndex(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		releaseRecoveries := func(recoveries []completionWindowRecovery, remove map[string]bool) error {
			var releaseErrs []error
			for _, recovery := range recoveries {
				unlockErr := unlockLeaseFile(recovery.live)
				releaseErrs = append(releaseErrs, unlockErr)
				if unlockErr == nil && remove[recovery.id] {
					releaseErrs = append(releaseErrs, removeLeaseAuthorityLock(
						root,
						completionWindowLockPrefix+recovery.id,
					))
				}
			}
			return errors.Join(releaseErrs...)
		}

		candidateTasks := map[string]QueuedTask{}
		var recoveries []completionWindowRecovery
		var acquireErrs []error
		for id, record := range index.Windows {
			live, lockErr := lockLeaseAuthority(root, completionWindowLockPrefix+id, true, syscall.LOCK_EX|syscall.LOCK_NB)
			if lockErr != nil {
				// A held window lock means a live owner is still replaying it; anything else is a
				// real failure to acquire and must be reported rather than silently skipped.
				if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EAGAIN) {
					acquireErrs = append(acquireErrs, lockErr)
				}
				continue
			}
			candidates, candidateErr := changedDoneCompletions(root, record.Baseline)
			candidates = markedCompletionCandidates(root, candidates, record.BaselineMutations)
			if candidateErr != nil {
				acquireErrs = append(acquireErrs, candidateErr, unlockLeaseFile(live))
				continue
			}
			for _, candidate := range candidates {
				candidateTasks[candidate.Item.ID] = candidate
			}
			recoveries = append(recoveries, completionWindowRecovery{
				id: id, record: record, live: live,
			})
		}
		if err := errors.Join(acquireErrs...); err != nil {
			errs = append(errs, err, releaseRecoveries(recoveries, nil), unlockLeaseFile(indexFile))
			continue
		}

		locked, lockErr := lockCompletionCandidates(root, candidateTasks)
		if lockErr != nil {
			errs = append(errs, lockErr, releaseRecoveries(recoveries, nil), unlockLeaseFile(indexFile))
			continue
		}

		// A candidate that appeared while the known set was being locked must be handled by the
		// next retry. Consuming a window from a partial locked set would reopen the same race.
		var stabilizationErrs []error
		stabilizationErrs = append(stabilizationErrs, verifyLockedCompletionCandidates(locked))
		for _, recovery := range recoveries {
			latest, latestErr := changedDoneCompletions(root, recovery.record.Baseline)
			latest = markedCompletionCandidates(root, latest, recovery.record.BaselineMutations)
			if latestErr != nil {
				stabilizationErrs = append(stabilizationErrs, latestErr)
				continue
			}
			for _, candidate := range latest {
				if _, ok := locked[candidate.Item.ID]; !ok {
					stabilizationErrs = append(stabilizationErrs, fmt.Errorf(
						"completion-window task %s changed while recovery acquired task authority; retry recovery",
						candidate.Item.ID,
					))
				}
			}
		}
		if err := errors.Join(stabilizationErrs...); err != nil {
			errs = append(
				errs,
				err,
				releaseLockedCompletionCandidates(locked),
				releaseRecoveries(recoveries, nil),
				unlockLeaseFile(indexFile),
			)
			continue
		}

		replaySeen := map[string]bool{}
		unownedReplaySeen := map[string]bool{}
		mutationSeen := map[string]bool{}
		lockedIDs := slices.Sorted(maps.Keys(locked))
		for i := range recoveries {
			recoveries[i].candidates = nil
			recoveries[i].replay = nil
			for _, id := range lockedIDs {
				candidate := locked[id]
				switch classifyCompletionCandidate(recoveries[i].record, id, candidate.fingerprint) {
				case completionCandidateReplay:
					recoveries[i].candidates = append(recoveries[i].candidates, candidate.task)
					recoveries[i].replay = append(recoveries[i].replay, candidate.task)
					replaySeen[id] = true
					if candidate.fingerprint.Receipt == "" {
						unownedReplaySeen[id] = true
					}
				case completionCandidateMutation:
					recoveries[i].candidates = append(recoveries[i].candidates, candidate.task)
					mutationSeen[id] = true
				}
			}
		}

		protectedMutations := map[string]bool{}
		for id := range mutationSeen {
			// An unreceipted arrival was never accepted as an archive, so recovery restores it
			// even if a later overlapping baseline observed provider-written metadata.
			if !unownedReplaySeen[id] {
				protectedMutations[id] = true
			}
		}
		restored := map[string]bool{}
		for id := range replaySeen {
			if unownedReplaySeen[id] && !protectedMutations[id] {
				restored[id] = true
			}
		}

		indexChanged := false
		for i := range recoveries {
			if setRecordBaselineMutations(&recoveries[i].record, protectedMutations) {
				index.Windows[recoveries[i].id] = recoveries[i].record
				indexChanged = true
			}
			if recordRecoveredDepartures(&recoveries[i].record, restored) {
				index.Windows[recoveries[i].id] = recoveries[i].record
				indexChanged = true
			}
		}
		if indexChanged {
			if err := writeCompletionWindowIndex(root, index); err != nil {
				errs = append(
					errs,
					err,
					releaseLockedCompletionCandidates(locked),
					releaseRecoveries(recoveries, nil),
					unlockLeaseFile(indexFile),
				)
				continue
			}
		}

		var actionErrs []error
		for _, id := range slices.Sorted(maps.Keys(protectedMutations)) {
			if err := locked[id].lock.clearCompleted(); err != nil {
				actionErrs = append(actionErrs, fmt.Errorf(
					"invalidate baseline mutation task %s completion receipt: %w",
					id,
					err,
				))
			}
		}
		for _, id := range slices.Sorted(maps.Keys(restored)) {
			candidate := locked[id]
			actionErrs = append(
				actionErrs,
				restoreUnownedCompletion(QueuedTask{Root: root, Item: candidate.current}),
				candidate.lock.clearCompleted(),
			)
		}
		if err := errors.Join(actionErrs...); err != nil {
			errs = append(
				errs,
				err,
				releaseLockedCompletionCandidates(locked),
				releaseRecoveries(recoveries, nil),
				unlockLeaseFile(indexFile),
			)
			continue
		}

		restoredIDs := slices.Sorted(maps.Keys(restored))
		deleteWindows := map[string]bool{}
		var recoveredConcurrent []string
		for _, recovery := range recoveries {
			id, record := recovery.id, recovery.record
			reviewCandidates := slices.DeleteFunc(slices.Clone(recovery.replay), func(candidate QueuedTask) bool {
				return protectedMutations[candidate.Item.ID]
			})
			var reviewErr error
			var observed []string
			if record.ReviewWindow {
				subjectScoped := record.ReviewSubjectScoped || len(record.ReviewSubjects) > 0
				changed, recovered := classifyReviewCompletionCandidates(
					reviewCandidates,
					restoredIDs,
					record.ReviewSubjects,
					subjectScoped,
				)
				observed = recovered
				if len(changed) > 0 {
					reviewErr = fmt.Errorf("review completion set changed before recovery for %s", strings.Join(changed, ", "))
				}
				relevant := slices.Clone(record.ReviewSubjects)
				for _, candidate := range recovery.candidates {
					relevant = append(relevant, candidate.Item.ID)
				}
				if duplicates := duplicateReviewTaskIDs(hosts, relevant); len(duplicates) > 0 {
					reviewErr = errors.Join(reviewErr, fmt.Errorf("review task id(s) %s became ambiguous across task queues before recovery", strings.Join(duplicates, ", ")))
				}
			}
			departed, departureErr := completionWindowDepartures(root, record)
			if err := errors.Join(reviewErr, departureErr); err != nil {
				errs = append(errs, err)
				continue
			}
			if len(departed) > 0 {
				errs = append(errs, fmt.Errorf("completion ownership changed before recovery: archived task(s) %s left done", strings.Join(departed, ", ")))
			}
			deleteWindows[id] = true
			recoveredConcurrent = append(recoveredConcurrent, observed...)
		}

		for id := range deleteWindows {
			delete(index.Windows, id)
		}
		if len(deleteWindows) > 0 {
			if err := writeCompletionWindowIndex(root, index); err != nil {
				errs = append(
					errs,
					err,
					releaseLockedCompletionCandidates(locked),
					releaseRecoveries(recoveries, nil),
					unlockLeaseFile(indexFile),
				)
				continue
			}
			concurrent = append(concurrent, recoveredConcurrent...)
		}
		mutations := make([]QueuedTask, 0, len(protectedMutations))
		for _, id := range slices.Sorted(maps.Keys(protectedMutations)) {
			mutations = append(mutations, locked[id].task)
		}
		slices.SortFunc(mutations, func(a, b QueuedTask) int {
			return strings.Compare(a.Item.ID, b.Item.ID)
		})
		errs = append(errs, baselineCompletionMutationError(mutations))
		errs = append(errs, releaseLockedCompletionCandidates(locked))
		errs = append(errs, releaseRecoveries(recoveries, deleteWindows))
		errs = append(errs, unlockLeaseFile(indexFile))
	}
	return slices.Compact(slices.Sorted(slices.Values(concurrent))), errors.Join(errs...)
}

func ReconcileCompletionWindows(hosts []string) error {
	_, err := ReconcileCompletionWindowsWithActivity(hosts)
	return err
}
