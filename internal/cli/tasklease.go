package cli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	leaseLockName          = "lease.lock"
	leaseMetadataName      = "lease.json"
	leaseAuthorityVersion  = "v1"
	leaseHeartbeatInterval = 10 * time.Second
	leaseStaleAfter        = time.Minute
	leaseMetadataVersion   = 1
)

const testLeaseAuthorityRootEnv = "COOP_TEST_LEASE_AUTHORITY_ROOT"

var errLeaseCandidateGone = errors.New("lease candidate changed state")

type leaseCompletionReceipt struct {
	Version int    `json:"version"`
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	Nonce   string `json:"nonce"`
}

// taskLeaseOwner identifies the controller in lease metadata. The kernel lock, rather than any
// one of these fields, is the authority: run ids and PIDs exist for recovery evidence and UI only.
type taskLeaseOwner struct {
	RunID    string
	PID      int
	Provider string
	Target   string
	Now      func() time.Time
	Ticker   func(time.Duration) (<-chan time.Time, func())
}

func (o taskLeaseOwner) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

type taskLeaseMetadata struct {
	Version       int       `json:"version"`
	RunID         string    `json:"run_id"`
	ControllerPID int       `json:"controller_pid"`
	Provider      string    `json:"provider"`
	Target        string    `json:"target"`
	AcquiredAt    time.Time `json:"acquired_at"`
	HeartbeatAt   time.Time `json:"heartbeat_at"`
}

// taskLease holds the host-only authoritative flock and a task-local compatibility flock for one
// agent iteration. Metadata is resolved by id on every heartbeat because the worker can move its
// folder while both descriptors remain valid across that rename.
type taskLease struct {
	root      string
	id        string
	local     *os.File
	authority *os.File
	meta      taskLeaseMetadata
	now       func() time.Time
	ticker    func(time.Duration) (<-chan time.Time, func())
	legacy    bool

	releaseOnce sync.Once
	quiesceOnce sync.Once
	releaseErr  error
	stop        chan struct{}
	done        chan struct{}
}

func (l *taskLease) startHeartbeat() {
	ticks, stopTicker := l.ticker(leaseHeartbeatInterval)
	go func() {
		defer stopTicker()
		defer close(l.done)
		for {
			select {
			case <-l.stop:
				return
			case <-ticks:
				// A heartbeat is evidence, not authority. Losing a race with a task move must not
				// recreate an old state path or drop the still-held inode lock.
				_ = l.refresh()
			}
		}
	}()
}

func realLeaseTicker(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

func (l *taskLease) refresh() error {
	l.meta.HeartbeatAt = l.now()
	return errors.Join(
		writeLeaseAuthorityMetadata(l.root, l.id, l.meta),
		writeLeaseMetadata(l.root, l.id, l.meta),
	)
}

// quiesce stops heartbeat writes while retaining the authoritative flock. Completion validation
// and cleanup call it before mutating the task directory, so metadata cannot race those changes.
func (l *taskLease) quiesce() {
	l.quiesceOnce.Do(func() { close(l.stop) })
	<-l.done
}

// markCompleted records host-only evidence on the already-persistent authority inode while its
// flock is held. Task-local state is provider-writable and cannot distinguish a foreign controller
// from an agent that moved an unleased folder.
func (l *taskLease) markCompleted(taskDir string) error {
	return writeLeaseCompletionReceipt(l.authority, taskDir)
}

func (l *taskLease) clearCompleted() error {
	return clearLeaseCompletionReceipt(l.authority)
}

// release stops metadata mutation before removing the evidence while still holding both flocks.
// Authority lock files persist in the host-only registry so every controller opens the same inode.
func (l *taskLease) release() error {
	l.releaseOnce.Do(func() {
		l.quiesce()
		l.releaseErr = errors.Join(
			removeLeaseAuthorityMetadata(l.root, l.id),
			removeLeaseMetadata(l.root, l.id),
			unlockLeaseFile(l.local),
			unlockLeaseFile(l.authority),
		)
	})
	return l.releaseErr
}

func leaseAuthorityKey(root, id string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs) + "\x00" + id))
	return fmt.Sprintf("%x", sum), nil
}

func openLeaseAuthorityRoot() (*os.Root, error) {
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		if dir := os.Getenv(testLeaseAuthorityRootEnv); dir != "" {
			return os.OpenRoot(dir)
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cache, "coop", "task-leases", leaseAuthorityVersion)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("task lease authority %q is not a real directory", dir)
	}
	return os.OpenRoot(dir)
}

func openLeaseAuthority(root, id string, create bool) (*os.File, error) {
	key, err := leaseAuthorityKey(root, id)
	if err != nil {
		return nil, err
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return nil, err
	}
	defer registry.Close()
	name := key + ".lock"
	file, err := registry.OpenFile(name, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) && create {
		file, err = registry.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if errors.Is(err, os.ErrExist) {
			file, err = registry.OpenFile(name, os.O_RDWR|syscall.O_NOFOLLOW, 0)
		}
	}
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.New("task lease authority is not a single-link regular file")
	}
	return file, nil
}

func completionReceiptFor(taskDir string) (leaseCompletionReceipt, error) {
	info, err := os.Lstat(taskDir)
	if err != nil {
		return leaseCompletionReceipt{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok {
		return leaseCompletionReceipt{}, fmt.Errorf("task completion path %q is not a real directory", taskDir)
	}
	return leaseCompletionReceipt{Version: 1, Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func clearLeaseCompletionReceipt(authority *os.File) error {
	if err := authority.Truncate(0); err != nil {
		return err
	}
	if _, err := authority.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return authority.Sync()
}

func writeLeaseCompletionReceipt(authority *os.File, taskDir string) error {
	receipt, err := completionReceiptFor(taskDir)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	receipt.Nonce = hex.EncodeToString(nonce)
	return writeLeaseCompletionReceiptValue(authority, receipt)
}

func writeLeaseCompletionReceiptValue(authority *os.File, receipt leaseCompletionReceipt) error {
	if receipt.Version != 1 || receipt.Nonce == "" {
		return errors.New("invalid task completion receipt")
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		return err
	}
	if _, err := authority.Write(append(data, '\n')); err != nil {
		return err
	}
	return authority.Sync()
}

func readLeaseCompletionReceipt(authority *os.File, taskDir string) (leaseCompletionReceipt, bool) {
	want, err := completionReceiptFor(taskDir)
	if err != nil {
		return leaseCompletionReceipt{}, false
	}
	if _, err := authority.Seek(0, io.SeekStart); err != nil {
		return leaseCompletionReceipt{}, false
	}
	data, err := io.ReadAll(io.LimitReader(authority, 4<<10))
	if err != nil {
		return leaseCompletionReceipt{}, false
	}
	var got leaseCompletionReceipt
	if json.Unmarshal(data, &got) != nil || got.Version != want.Version || got.Device != want.Device ||
		got.Inode != want.Inode || got.Nonce == "" {
		return leaseCompletionReceipt{}, false
	}
	return got, true
}

func leaseCompletionReceiptMatches(authority *os.File, taskDir string) bool {
	_, ok := readLeaseCompletionReceipt(authority, taskDir)
	return ok
}

func inspectTaskCompletionReceipt(root string, task taskItem) (leaseCompletionReceipt, bool, bool) {
	authority, err := openLeaseAuthority(root, task.ID, false)
	if errors.Is(err, os.ErrNotExist) {
		return leaseCompletionReceipt{}, false, false
	}
	if err != nil {
		return leaseCompletionReceipt{}, false, true
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		_ = authority.Close()
		return leaseCompletionReceipt{}, false, true
	}
	receipt, ok := readLeaseCompletionReceipt(authority, task.Dir)
	if err := unlockLeaseFile(authority); err != nil {
		return leaseCompletionReceipt{}, false, true
	}
	return receipt, ok, false
}

func clearTaskCompletionReceipt(root, id string) error {
	authority, err := openLeaseAuthority(root, id, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = authority.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil // a new owner cleared the old receipt while acquiring this same flock
		}
		return err
	}
	return errors.Join(clearLeaseCompletionReceipt(authority), unlockLeaseFile(authority))
}

// clearTaskCompletionReceiptIfMatches invalidates only the generation the caller observed. The
// exclusive authority lock closes the gap between comparing a stale receipt and clearing it, so a
// concurrent trusted completion cannot publish a fresh nonce that this audit then erases.
func clearTaskCompletionReceiptIfMatches(root string, task taskItem, nonce string) (bool, error) {
	if nonce == "" {
		return false, nil
	}
	authority, err := openLeaseAuthority(root, task.ID, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := lockExclusiveForCompletionAudit(authority, "task "+task.ID+" authority", func() bool {
		return leaseAuthorityMetadataExists(root, task.ID)
	}); err != nil {
		_ = authority.Close()
		if errors.Is(err, errCompletionAuditLockOwned) {
			return false, nil
		}
		return false, err
	}
	current, ok := currentTask(root, task.ID)
	if !ok || current.State != stateDone || current.Dir != task.Dir {
		return false, unlockLeaseFile(authority)
	}
	receipt, ok := readLeaseCompletionReceipt(authority, current.Dir)
	if !ok || receipt.Nonce != nonce {
		return false, unlockLeaseFile(authority)
	}
	return true, errors.Join(clearLeaseCompletionReceipt(authority), unlockLeaseFile(authority))
}

func writeLeaseAuthorityMetadata(root, id string, meta taskLeaseMetadata) error {
	key, err := leaseAuthorityKey(root, id)
	if err != nil {
		return err
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return atomicWriteTaskFile(registry, key+".json", append(data, '\n'))
}

func readLeaseAuthorityMetadata(root, id string) (taskLeaseMetadata, bool) {
	key, err := leaseAuthorityKey(root, id)
	if err != nil {
		return taskLeaseMetadata{}, false
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return taskLeaseMetadata{}, false
	}
	defer registry.Close()
	data, err := readTaskMetadataFile(registry, key+".json")
	if err != nil {
		return taskLeaseMetadata{}, false
	}
	var meta taskLeaseMetadata
	if err := json.Unmarshal(data, &meta); err != nil || meta.Version != leaseMetadataVersion {
		return taskLeaseMetadata{}, false
	}
	return meta, true
}

func leaseAuthorityMetadataExists(root, id string) bool {
	key, err := leaseAuthorityKey(root, id)
	if err != nil {
		return false
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return false
	}
	defer registry.Close()
	info, err := registry.Lstat(key + ".json")
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && ok && stat.Nlink == 1
}

func removeLeaseAuthorityMetadata(root, id string) error {
	key, err := leaseAuthorityKey(root, id)
	if err != nil {
		return err
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	if err := registry.Remove(key + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeLeaseAuthorityLock(root, id string) error {
	key, err := leaseAuthorityKey(root, id)
	if err != nil {
		return err
	}
	registry, err := openLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	if err := registry.Remove(key + ".lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type taskLeaseState uint8

const (
	leaseUnleased taskLeaseState = iota
	leaseBusy
	leaseStalled
)

type taskLeaseObservation struct {
	State    taskLeaseState
	Provider string
}

func (o taskLeaseObservation) label() string {
	switch o.State {
	case leaseStalled:
		return "stalled " + o.Provider
	case leaseBusy:
		return "busy " + o.Provider
	default:
		return "unleased"
	}
}

type taskLeaseSummary struct {
	Busy    int
	Stalled int
}

func (s *taskLeaseSummary) add(o taskLeaseObservation) {
	switch o.State {
	case leaseBusy:
		s.Busy++
	case leaseStalled:
		s.Stalled++
	}
}

func (s taskLeaseSummary) String() string {
	parts := make([]string, 0, 2)
	if s.Busy > 0 {
		parts = append(parts, fmt.Sprintf("%d busy", s.Busy))
	}
	if s.Stalled > 0 {
		parts = append(parts, fmt.Sprintf("%d stalled", s.Stalled))
	}
	if len(parts) == 0 {
		return "no available task"
	}
	return strings.Join(parts, " - ")
}

// taskLeaseDir creates only the literal task tmp child. In particular it never uses MkdirAll: a
// contender that sees a task move must fail and rescan, not recreate an obsolete state directory.
func taskLeaseDir(taskDir string) (string, error) {
	root, err := openTaskMetadataRoot(taskDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", errLeaseCandidateGone
	}
	if err != nil {
		return "", err
	}
	defer root.Close()
	if _, err := root.Lstat("tmp"); errors.Is(err, os.ErrNotExist) {
		if err := root.Mkdir("tmp", 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	tmp, err := openTaskTmpRoot(taskDir)
	if err != nil {
		return "", err
	}
	_ = tmp.Close()
	return filepath.Join(taskDir, "tmp"), nil
}

func currentTask(root, id string) (taskItem, bool) {
	for _, t := range readTaskTree(root) {
		if t.ID == id {
			return t, true
		}
	}
	return taskItem{}, false
}

func openTaskTmpRoot(taskDir string) (*os.Root, error) {
	taskRoot, err := openTaskMetadataRoot(taskDir)
	if err != nil {
		return nil, err
	}
	defer taskRoot.Close()
	before, err := taskRoot.Lstat("tmp")
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("task lease tmp %q is not a real directory", filepath.Join(taskDir, "tmp"))
	}
	root, err := taskRoot.OpenRoot("tmp")
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("task lease tmp changed while opening")
	}
	return root, nil
}

func openLeaseLock(taskDir string, create bool) (*os.File, error) {
	root, err := openTaskTmpRoot(taskDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	if create {
		flags |= os.O_CREATE
	}
	f, err := root.OpenFile(leaseLockName, flags, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || stat.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("task lease lock in %q is not a single-link regular file", taskDir)
	}
	return f, nil
}

// tryTaskLease locks a candidate without waiting. It records whether this was a legacy adoption
// before creating lease.lock, then writes metadata only after the authoritative flock succeeds.
func tryTaskLease(root string, item taskItem, owner taskLeaseOwner) (*taskLease, taskLeaseObservation, error) {
	dir, err := taskLeaseDir(item.Dir)
	if err != nil {
		return nil, taskLeaseObservation{}, err
	}
	lockPath := filepath.Join(dir, leaseLockName)
	_, statErr := os.Lstat(lockPath)
	legacy := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !legacy {
		return nil, taskLeaseObservation{}, statErr
	}
	authority, err := openLeaseAuthority(root, item.ID, true)
	if err != nil {
		return nil, taskLeaseObservation{}, err
	}
	if err := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = authority.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, taskLeaseObservation{}, err
		}
		observed := observeHeldTaskLease(item, owner.now())
		if observed.State == leaseUnleased {
			return nil, taskLeaseObservation{}, errLeaseCandidateGone
		}
		return nil, observed, nil
	}
	f, err := openLeaseLock(item.Dir, true)
	if errors.Is(err, os.ErrNotExist) {
		_ = unlockLeaseFile(authority)
		return nil, taskLeaseObservation{}, errLeaseCandidateGone
	}
	if err != nil {
		_ = unlockLeaseFile(authority)
		return nil, taskLeaseObservation{}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		_ = unlockLeaseFile(authority)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, taskLeaseObservation{}, err
		}
		observed := observeHeldTaskLease(item, owner.now())
		if observed.State == leaseUnleased {
			// The owner released between our failed flock and the status read. Rescan so this
			// now-available task is adopted instead of reporting a false all-busy queue.
			return nil, taskLeaseObservation{}, errLeaseCandidateGone
		}
		return nil, observed, nil
	}
	now := owner.now()
	l := &taskLease{
		root:      root,
		id:        item.ID,
		local:     f,
		authority: authority,
		meta: taskLeaseMetadata{
			Version:       leaseMetadataVersion,
			RunID:         owner.RunID,
			ControllerPID: owner.PID,
			Provider:      owner.Provider,
			Target:        owner.Target,
			AcquiredAt:    now,
			HeartbeatAt:   now,
		},
		now:    owner.now,
		ticker: owner.Ticker,
		legacy: legacy && item.State == stateInProgress,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if l.ticker == nil {
		l.ticker = realLeaseTicker
	}
	if err := l.refresh(); err != nil {
		_ = removeLeaseAuthorityMetadata(root, item.ID)
		_ = unlockLeaseFile(f)
		_ = unlockLeaseFile(authority)
		return nil, taskLeaseObservation{}, err
	}
	current, ok := currentTask(root, item.ID)
	if !ok || current.State != item.State {
		// Metadata follows the id across legitimate moves once an iteration owns the task, but a
		// move DURING acquisition means this stale candidate must be rescanned, not launched.
		_ = removeLeaseMetadata(root, item.ID)
		_ = removeLeaseAuthorityMetadata(root, item.ID)
		_ = unlockLeaseFile(f)
		_ = unlockLeaseFile(authority)
		return nil, taskLeaseObservation{}, errLeaseCandidateGone
	}
	// A new owned attempt invalidates any receipt from an earlier accepted completion. The same
	// authority flock serializes this with concurrent completion scans.
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		_ = removeLeaseMetadata(root, item.ID)
		_ = removeLeaseAuthorityMetadata(root, item.ID)
		_ = unlockLeaseFile(f)
		_ = unlockLeaseFile(authority)
		return nil, taskLeaseObservation{}, err
	}
	l.startHeartbeat()
	return l, taskLeaseObservation{}, nil
}

func observeTaskLease(item taskItem, now time.Time) taskLeaseObservation {
	return observeHeldTaskLease(item, now)
}

// observeHeldTaskLease never creates lease state: task/watch is read-only. An unlocked or missing
// lock is unleased even if a crashed controller left stale metadata behind; only a HELD lock means
// another controller owns the work.
func observeHeldTaskLease(item taskItem, now time.Time) taskLeaseObservation {
	root := filepath.Dir(filepath.Dir(item.Dir))
	authority, err := openLeaseAuthority(root, item.ID, false)
	if err == nil {
		if lockErr := syscall.Flock(int(authority.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
			_ = authority.Close()
			if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EAGAIN) {
				return taskLeaseObservation{State: leaseBusy, Provider: "unknown"}
			}
			meta, ok := readLeaseAuthorityMetadata(root, item.ID)
			return leaseObservationFromMetadata(meta, ok, now)
		}
		_ = unlockLeaseFile(authority)
	} else if !errors.Is(err, os.ErrNotExist) {
		return taskLeaseObservation{State: leaseBusy, Provider: "unknown"}
	}
	// A task-local lock is retained for compatibility with older controllers and lets the in-box
	// fixture verify that the host claimed the task. New controllers additionally require authority.
	f, err := openLeaseLock(item.Dir, false)
	if errors.Is(err, os.ErrNotExist) {
		return taskLeaseObservation{State: leaseUnleased}
	}
	if err != nil {
		return taskLeaseObservation{State: leaseBusy, Provider: "unknown"}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return taskLeaseObservation{State: leaseUnleased}
	}
	_ = f.Close()
	meta, ok := readLeaseMetadata(item.Dir)
	return leaseObservationFromMetadata(meta, ok, now)
}

func leaseObservationFromMetadata(meta taskLeaseMetadata, ok bool, now time.Time) taskLeaseObservation {
	provider := "unknown"
	if ok {
		provider = leaseProvider(meta.Provider)
		if now.Sub(meta.HeartbeatAt) > leaseStaleAfter {
			return taskLeaseObservation{State: leaseStalled, Provider: provider}
		}
	}
	return taskLeaseObservation{State: leaseBusy, Provider: provider}
}

func leaseProvider(provider string) string {
	p := sanitizeCell(strings.TrimSpace(provider))
	if p == "" {
		return "unknown"
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return "unknown"
		}
	}
	return truncate(p, 20)
}

func readLeaseMetadata(taskDir string) (taskLeaseMetadata, bool) {
	root, err := openTaskTmpRoot(taskDir)
	if err != nil {
		return taskLeaseMetadata{}, false
	}
	defer root.Close()
	data, err := readTaskMetadataFile(root, leaseMetadataName)
	if err != nil {
		return taskLeaseMetadata{}, false
	}
	var meta taskLeaseMetadata
	if err := json.Unmarshal(data, &meta); err != nil || meta.Version != leaseMetadataVersion || meta.HeartbeatAt.IsZero() {
		return taskLeaseMetadata{}, false
	}
	return meta, true
}

func writeLeaseMetadata(root, id string, meta taskLeaseMetadata) error {
	task, ok := currentTask(root, id)
	if !ok {
		return nil // the task was removed after completion; never recreate its previous path
	}
	tmpRoot, err := openTaskTmpRoot(task.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer tmpRoot.Close()
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return atomicWriteTaskFile(tmpRoot, leaseMetadataName, append(data, '\n'))
}

func removeLeaseMetadata(root, id string) error {
	task, ok := currentTask(root, id)
	if !ok {
		return nil
	}
	tmpRoot, err := openTaskTmpRoot(task.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer tmpRoot.Close()
	if err := tmpRoot.Remove(leaseMetadataName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
