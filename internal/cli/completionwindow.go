package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	errCompletionWindowSetup = errors.New("completion window setup failed")
	errCompletionWindowAudit = errors.New("completion window audit failed")
)

type completionFingerprint struct {
	Device      uint64 `json:"device"`
	Inode       uint64 `json:"inode"`
	ChangeSec   int64  `json:"change_sec"`
	ChangeNsec  int64  `json:"change_nsec"`
	Receipt     string `json:"receipt,omitempty"`
	ReceiptBusy bool   `json:"receipt_busy,omitempty"`
	Tree        string `json:"tree"`
}

type completionWindowRecord struct {
	Baseline              map[string]completionFingerprint `json:"baseline"`
	AllowedDoneDeparture  string                           `json:"allowed_done_departure,omitempty"`
	AllowedDoneDepartures []string                         `json:"allowed_done_departures,omitempty"`
	ReviewWindow          bool                             `json:"review_window,omitempty"`
	ReviewSubjects        []string                         `json:"review_subjects,omitempty"`
	ReviewSubjectScoped   bool                             `json:"review_subject_scoped,omitempty"`
	WorkWindow            bool                             `json:"work_window,omitempty"`
	WorkSubject           string                           `json:"work_subject,omitempty"`
}

type completionWindowIndex struct {
	Version int                               `json:"version"`
	Windows map[string]completionWindowRecord `json:"windows"`
}

type completionWindow struct {
	root   string
	id     string
	record completionWindowRecord
	live   *os.File
}

type completionWindowSet struct {
	windows []completionWindow
	scan    func(string, map[string]completionFingerprint) ([]queuedTask, error)
}

func snapshotDoneCompletions(root string) (map[string]completionFingerprint, error) {
	snapshot := map[string]completionFingerprint{}
	for _, task := range readTaskTree(root) {
		if task.State != stateDone {
			continue
		}
		fingerprint, err := completionFingerprintFor(root, task)
		if err != nil {
			return nil, err
		}
		snapshot[task.ID] = fingerprint
	}
	return snapshot, nil
}

func completionFingerprintFor(root string, task taskItem) (completionFingerprint, error) {
	info, err := os.Lstat(task.Dir)
	if err != nil {
		return completionFingerprint{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok {
		return completionFingerprint{}, fmt.Errorf("task completion path %q is not a real directory", task.Dir)
	}
	sec, nsec := statChangeTime(stat)
	tree, err := completionTreeMetadataDigest(task.Dir)
	if err != nil {
		return completionFingerprint{}, err
	}
	accepted, valid, busy := inspectTaskCompletionReceipt(root, task)
	if !valid {
		accepted.Nonce = ""
	}
	return completionFingerprint{
		Device: uint64(stat.Dev), Inode: uint64(stat.Ino), ChangeSec: sec, ChangeNsec: nsec,
		Receipt: accepted.Nonce, ReceiptBusy: busy, Tree: tree,
	}, nil
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

func changedDoneCompletions(root string, baseline map[string]completionFingerprint) ([]queuedTask, error) {
	var changed []queuedTask
	for _, task := range readTaskTree(root) {
		if task.State != stateDone {
			continue
		}
		current, err := completionFingerprintFor(root, task)
		if err != nil {
			return nil, err
		}
		before, existed := baseline[task.ID]
		if !existed || before.Device != current.Device || before.Inode != current.Inode ||
			before.ChangeSec != current.ChangeSec || before.ChangeNsec != current.ChangeNsec ||
			before.Tree != current.Tree || before.Receipt != current.Receipt || before.ReceiptBusy || current.ReceiptBusy {
			changed = append(changed, queuedTask{Root: root, Item: task})
		}
	}
	slices.SortFunc(changed, func(a, b queuedTask) int {
		if byID := strings.Compare(a.Item.ID, b.Item.ID); byID != 0 {
			return byID
		}
		return strings.Compare(a.Root, b.Root)
	})
	return changed, nil
}

func invalidateStaleCandidateReceipts(root string, baseline map[string]completionFingerprint, candidates []queuedTask) error {
	var errs []error
	for _, candidate := range candidates {
		if candidate.Root != root {
			continue
		}
		before, existed := baseline[candidate.Item.ID]
		if !existed || before.Receipt == "" {
			continue
		}
		current, err := completionFingerprintFor(root, candidate.Item)
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

func completionWindowIndexName(root string) (string, error) {
	key, err := leaseAuthorityKey(root, completionWindowIndexID)
	return key + ".windows.json", err
}

func readCompletionWindowIndex(root string) (completionWindowIndex, error) {
	name, err := completionWindowIndexName(root)
	if err != nil {
		return completionWindowIndex{}, err
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return completionWindowIndex{}, err
	}
	defer registry.Close()
	before, err := registry.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return completionWindowIndex{Version: completionWindowVersion, Windows: map[string]completionWindowRecord{}}, nil
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
	name, err := completionWindowIndexName(root)
	if err != nil {
		return err
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	return atomicWriteTaskFile(registry, name, append(data, '\n'))
}

func lockCompletionWindowIndex(root string) (*os.File, completionWindowIndex, error) {
	file, err := openLeaseAuthority(root, completionWindowIndexID, true)
	if err != nil {
		return nil, completionWindowIndex{}, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, completionWindowIndex{}, err
	}
	index, err := readCompletionWindowIndex(root)
	if err != nil {
		_ = unlockLeaseFile(file)
		return nil, completionWindowIndex{}, err
	}
	return file, index, nil
}

func beginCompletionWindows(hosts []string) (*completionWindowSet, error) {
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
func beginReviewCompletionWindows(hosts, subjects []string) (*completionWindowSet, error) {
	if duplicates := duplicateReviewTaskIDs(hosts, subjects); len(duplicates) > 0 {
		return nil, fmt.Errorf("review subject id(s) %s exist in multiple task queues", strings.Join(duplicates, ", "))
	}
	return beginCompletionWindowsWithPolicy(hosts, nil, subjects, true)
}

// beginWorkCompletionWindows journals the exact leased task. A work box cannot change its own
// subject's completion state outside the normal finalization path, but a host-receipted move of a
// different archived task is concurrent queue activity rather than box ownership churn.
func beginWorkCompletionWindows(hosts []string, subject string) (*completionWindowSet, error) {
	if subject == "" {
		return nil, errors.New("work completion window is missing its assigned subject")
	}
	if duplicates := duplicateReviewTaskIDs(hosts, []string{subject}); len(duplicates) > 0 {
		return nil, fmt.Errorf("work subject id(s) %s exist in multiple task queues", strings.Join(duplicates, ", "))
	}
	return beginCompletionWindowsWithPolicy(hosts, nil, nil, false, subject)
}

func beginCompletionWindowsAllowing(hosts []string, taskID string) (*completionWindowSet, error) {
	return beginCompletionWindowsAllowingTasks(hosts, []string{taskID})
}

func beginCompletionWindowsAllowingTasks(hosts, taskIDs []string) (*completionWindowSet, error) {
	return beginCompletionWindowsWithPolicy(hosts, taskIDs, nil, false)
}

func beginCompletionWindowsWithPolicy(hosts, allowedDoneDepartures, reviewSubjects []string, reviewWindow bool, workSubjects ...string) (*completionWindowSet, error) {
	if len(hosts) == 0 {
		return &completionWindowSet{}, nil
	}
	id, err := newSupervisorID()
	if err != nil {
		return nil, err
	}
	set := &completionWindowSet{}
	for _, root := range hosts {
		// Snapshot and register under one index critical section. Task deletion uses the same
		// boundary, so a window cannot capture a task before deletion and register that stale
		// baseline after deletion has purged the index.
		indexFile, index, err := lockCompletionWindowIndex(root)
		if err != nil {
			return nil, errors.Join(err, set.close())
		}
		baseline, err := snapshotDoneCompletions(root)
		if err != nil {
			return nil, errors.Join(err, unlockLeaseFile(indexFile), set.close())
		}
		live, err := openLeaseAuthority(root, completionWindowLockPrefix+id, true)
		if err != nil {
			return nil, errors.Join(err, unlockLeaseFile(indexFile), set.close())
		}
		if err := syscall.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			closeErr := live.Close()
			removeErr := removeLeaseAuthorityLock(root, completionWindowLockPrefix+id)
			return nil, errors.Join(err, closeErr, removeErr, unlockLeaseFile(indexFile), set.close())
		}
		record := completionWindowRecord{
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
			return nil, errors.Join(writeErr, unlockErr, liveUnlockErr, removeErr, set.close())
		}
		if unlockErr != nil {
			return nil, errors.Join(unlockErr, unlockLeaseFile(live), set.close())
		}
		set.windows = append(set.windows, completionWindow{root: root, id: id, record: record, live: live})
	}
	return set, nil
}

func (s *completionWindowSet) auditDoneCandidates(assigned queuedTask) ([]queuedTask, []string, error) {
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

func completionWindowBaselineChanges(root string, record completionWindowRecord) ([]taskItem, []string) {
	currentByID := map[string]taskItem{}
	for _, task := range readTaskTree(root) {
		currentByID[task.ID] = task
	}
	var departed []taskItem
	var missing []string
	for id := range record.Baseline {
		current, ok := currentByID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if current.State != stateDone {
			departed = append(departed, current)
		}
	}
	slices.SortFunc(departed, func(a, b taskItem) int { return strings.Compare(a.ID, b.ID) })
	slices.Sort(missing)
	return departed, missing
}

func completionWindowDepartures(root string, record completionWindowRecord) ([]string, error) {
	departed, missing := completionWindowBaselineChanges(root, record)
	if len(missing) > 0 {
		return nil, fmt.Errorf("task(s) %s left the queue during this completion window; restore the missing task folder to a lifecycle state, then re-run `coop loop`", strings.Join(missing, ", "))
	}
	var disallowed []string
	for _, task := range departed {
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
		if task.ID != record.AllowedDoneDeparture && !slices.Contains(record.AllowedDoneDepartures, task.ID) {
			disallowed = append(disallowed, task.ID)
		}
	}
	return disallowed, nil
}

func (s *completionWindowSet) departures() ([]string, error) {
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

func (s *completionWindowSet) candidates() ([]queuedTask, error) {
	if s == nil {
		return nil, nil
	}
	var candidates []queuedTask
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
	slices.SortFunc(candidates, func(a, b queuedTask) int {
		if byID := strings.Compare(a.Item.ID, b.Item.ID); byID != 0 {
			return byID
		}
		return strings.Compare(a.Root, b.Root)
	})
	return candidates, nil
}

func (s *completionWindowSet) close() error {
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
func (s *completionWindowSet) abandon() error {
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

func (s *completionWindowSet) rejectAndClose(assigned queuedTask) error {
	_, rejected, err := s.auditDoneCandidates(assigned)
	if err != nil {
		return errors.Join(err, s.abandon())
	}
	departed, err := s.departures()
	if err != nil {
		return errors.Join(err, s.abandon())
	}
	var ownershipErr error
	if len(rejected) > 0 {
		ownershipErr = unownedCompletionError(rejected, nil)
	}
	var departureErr error
	if len(departed) > 0 {
		departureErr = fmt.Errorf("completion ownership changed: archived task(s) %s left done during a stage that may not reopen them", strings.Join(departed, ", "))
	}
	return errors.Join(ownershipErr, departureErr, s.close())
}

func classifyReviewCompletionCandidates(candidates []queuedTask, rejected, subjectIDs []string, subjectScoped bool) (changed, concurrent []string) {
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

func (s *completionWindowSet) reviewScope() (hosts, subjectIDs []string, subjectScoped bool) {
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
func (s *completionWindowSet) auditReview(hosts, subjectIDs []string, subjectScoped bool) (concurrent []string, scanErr, reviewErr error) {
	candidates, rejected, err := s.auditDoneCandidates(queuedTask{})
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
		ownershipErr = unownedCompletionError(rejected, nil)
	}
	relevant := slices.Clone(subjectIDs)
	for _, candidate := range candidates {
		relevant = append(relevant, candidate.Item.ID)
	}
	var duplicateErr error
	if duplicates := duplicateReviewTaskIDs(hosts, relevant); len(duplicates) > 0 {
		duplicateErr = fmt.Errorf("review task id(s) %s became ambiguous across task queues", strings.Join(duplicates, ", "))
	}
	departed, departureErr := s.departures()
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
func (s *completionWindowSet) finishReview() ([]string, error) {
	hosts, subjectIDs, subjectScoped := s.reviewScope()
	closed := &completionWindowSet{windows: slices.Clone(s.windows), scan: s.scan}
	for i := range closed.windows {
		closed.windows[i].live = nil
	}
	concurrent, scanErr, reviewErr := s.auditReview(hosts, subjectIDs, subjectScoped)
	if scanErr != nil {
		return nil, errors.Join(scanErr, s.abandon())
	}
	if err := errors.Join(reviewErr, s.close()); err != nil {
		return nil, err
	}
	afterClose, scanErr, reviewErr := closed.auditReview(hosts, subjectIDs, subjectScoped)
	if err := errors.Join(scanErr, reviewErr); err != nil {
		return nil, err
	}
	return slices.Compact(slices.Sorted(slices.Values(append(concurrent, afterClose...)))), nil
}

func reconcileCompletionWindowsWithActivity(hosts []string) ([]string, error) {
	var concurrent []string
	var errs []error
	for _, root := range hosts {
		indexFile, index, err := lockCompletionWindowIndex(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for id, record := range index.Windows {
			live, openErr := openLeaseAuthority(root, completionWindowLockPrefix+id, true)
			if openErr != nil {
				errs = append(errs, openErr)
				continue
			}
			if lockErr := syscall.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
				_ = live.Close()
				if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EAGAIN) {
					errs = append(errs, lockErr)
				}
				continue
			}
			candidates, candidateErr := changedDoneCompletions(root, record.Baseline)
			invalidateErr := invalidateStaleCandidateReceipts(root, record.Baseline, candidates)
			rejected, rejectErr := rejectUnownedCompletions(candidates, queuedTask{})
			var reviewErr error
			var observed []string
			if record.ReviewWindow {
				subjectScoped := record.ReviewSubjectScoped || len(record.ReviewSubjects) > 0
				changed, recovered := classifyReviewCompletionCandidates(candidates, rejected, record.ReviewSubjects, subjectScoped)
				observed = recovered
				if len(changed) > 0 {
					reviewErr = fmt.Errorf("review completion set changed before recovery for %s", strings.Join(changed, ", "))
				}
				relevant := slices.Clone(record.ReviewSubjects)
				for _, candidate := range candidates {
					relevant = append(relevant, candidate.Item.ID)
				}
				if duplicates := duplicateReviewTaskIDs(hosts, relevant); len(duplicates) > 0 {
					reviewErr = errors.Join(reviewErr, fmt.Errorf("review task id(s) %s became ambiguous across task queues before recovery", strings.Join(duplicates, ", ")))
				}
			}
			departed, departureErr := completionWindowDepartures(root, record)
			if err := errors.Join(candidateErr, invalidateErr, rejectErr, reviewErr, departureErr); err != nil {
				errs = append(errs, err, unlockLeaseFile(live))
				continue
			}
			if len(departed) > 0 {
				errs = append(errs, fmt.Errorf("completion ownership changed before recovery: archived task(s) %s left done", strings.Join(departed, ", ")))
			}
			delete(index.Windows, id)
			if writeErr := writeCompletionWindowIndex(root, index); writeErr != nil {
				errs = append(errs, writeErr, unlockLeaseFile(live))
				index.Windows[id] = record
				continue
			}
			unlockErr := unlockLeaseFile(live)
			errs = append(errs, unlockErr)
			if unlockErr == nil {
				errs = append(errs, removeLeaseAuthorityLock(root, completionWindowLockPrefix+id))
			}
			concurrent = append(concurrent, observed...)
		}
		errs = append(errs, unlockLeaseFile(indexFile))
	}
	return slices.Compact(slices.Sorted(slices.Values(concurrent))), errors.Join(errs...)
}

func reconcileCompletionWindows(hosts []string) error {
	_, err := reconcileCompletionWindowsWithActivity(hosts)
	return err
}
