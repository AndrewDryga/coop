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
	Baseline             map[string]completionFingerprint `json:"baseline"`
	AllowDoneDepartures  bool                             `json:"allow_done_departures,omitempty"`
	AllowedDoneDeparture string                           `json:"allowed_done_departure,omitempty"`
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
	return beginCompletionWindowsWithPolicy(hosts, false, "")
}

func beginReviewCompletionWindows(hosts []string) (*completionWindowSet, error) {
	return beginCompletionWindowsWithPolicy(hosts, true, "")
}

func beginCompletionWindowsAllowing(hosts []string, taskID string) (*completionWindowSet, error) {
	return beginCompletionWindowsWithPolicy(hosts, false, taskID)
}

func beginCompletionWindowsWithPolicy(hosts []string, allowDoneDepartures bool, allowedDoneDeparture string) (*completionWindowSet, error) {
	if len(hosts) == 0 {
		return &completionWindowSet{}, nil
	}
	id, err := newSupervisorID()
	if err != nil {
		return nil, err
	}
	set := &completionWindowSet{}
	for _, root := range hosts {
		baseline, err := snapshotDoneCompletions(root)
		if err != nil {
			return nil, errors.Join(err, set.close())
		}
		live, err := openLeaseAuthority(root, completionWindowLockPrefix+id, true)
		if err != nil {
			return nil, errors.Join(err, set.close())
		}
		if err := syscall.Flock(int(live.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			closeErr := live.Close()
			removeErr := removeLeaseAuthorityLock(root, completionWindowLockPrefix+id)
			return nil, errors.Join(err, closeErr, removeErr, set.close())
		}
		indexFile, index, err := lockCompletionWindowIndex(root)
		if err != nil {
			unlockErr := unlockLeaseFile(live)
			var removeErr error
			if unlockErr == nil {
				removeErr = removeLeaseAuthorityLock(root, completionWindowLockPrefix+id)
			}
			return nil, errors.Join(err, unlockErr, removeErr, set.close())
		}
		record := completionWindowRecord{Baseline: baseline, AllowDoneDepartures: allowDoneDepartures, AllowedDoneDeparture: allowedDoneDeparture}
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
		if !record.AllowDoneDepartures && task.ID != record.AllowedDoneDeparture {
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

func (s *completionWindowSet) clearReopenedReceipts() ([]string, error) {
	if s == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, window := range s.windows {
		departed, missing := completionWindowBaselineChanges(window.root, window.record)
		if len(missing) > 0 {
			return nil, fmt.Errorf("task(s) %s left the queue during this review stage", strings.Join(missing, ", "))
		}
		for _, current := range departed {
			if err := clearTaskCompletionReceipt(window.root, current.ID); err != nil {
				return nil, fmt.Errorf("clear reopened task %s completion receipt: %w", current.ID, err)
			}
			seen[current.ID] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func (s *completionWindowSet) finishReview() ([]string, error) {
	reopened, reopenErr := s.clearReopenedReceipts()
	if reopenErr != nil {
		return reopened, errors.Join(reopenErr, s.abandon())
	}
	candidates, rejected, err := s.auditDoneCandidates(queuedTask{})
	if err != nil {
		return reopened, errors.Join(err, s.abandon())
	}
	var changedErr error
	if len(candidates) > 0 {
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.Item.ID)
		}
		ids = slices.Compact(ids)
		changedErr = fmt.Errorf("review completion set changed for %s", strings.Join(ids, ", "))
	}
	var ownershipErr error
	if len(rejected) > 0 {
		ownershipErr = unownedCompletionError(rejected, nil)
	}
	return reopened, errors.Join(changedErr, ownershipErr, s.close())
}

func reconcileCompletionWindows(hosts []string) error {
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
			_, rejectErr := rejectUnownedCompletions(candidates, queuedTask{})
			departed, departureErr := completionWindowDepartures(root, record)
			if err := errors.Join(candidateErr, invalidateErr, rejectErr, departureErr); err != nil {
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
		}
		errs = append(errs, unlockLeaseFile(indexFile))
	}
	return errors.Join(errs...)
}
