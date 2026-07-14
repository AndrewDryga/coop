package cli

import (
	"encoding/json"
	"errors"
	"fmt"
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
	leaseHeartbeatInterval = 10 * time.Second
	leaseStaleAfter        = time.Minute
	leaseMetadataVersion   = 1
)

var errLeaseCandidateGone = errors.New("lease candidate changed state")

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

// taskLease holds the task's stable lock inode for one whole agent iteration. Its metadata path is
// resolved by id on every heartbeat because the worker can move its folder while the descriptor
// remains valid across that rename.
type taskLease struct {
	root   string
	id     string
	file   *os.File
	meta   taskLeaseMetadata
	now    func() time.Time
	ticker func(time.Duration) (<-chan time.Time, func())
	legacy bool

	releaseOnce sync.Once
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
	return writeLeaseMetadata(l.root, l.id, l.meta)
}

// release stops metadata mutation before removing the evidence while still holding the inode lock.
// It intentionally leaves lease.lock in place: unlinking it would let another controller lock a new
// inode while this descriptor still protects the old one.
func (l *taskLease) release() error {
	l.releaseOnce.Do(func() {
		close(l.stop)
		<-l.done
		err := removeLeaseMetadata(l.root, l.id)
		unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		closeErr := l.file.Close()
		l.releaseErr = errors.Join(err, unlockErr, closeErr)
	})
	return l.releaseErr
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
	fi, err := os.Lstat(taskDir)
	if errors.Is(err, os.ErrNotExist) {
		return "", errLeaseCandidateGone
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", errLeaseCandidateGone
	}
	dir := filepath.Join(taskDir, "tmp")
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		if errors.Is(err, os.ErrNotExist) {
			return "", errLeaseCandidateGone
		}
		return "", err
	}
	fi, err = os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", errLeaseCandidateGone
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("task lease tmp %q is not a real directory", dir)
	}
	return dir, nil
}

func currentTask(root, id string) (taskItem, bool) {
	for _, t := range readTaskTree(root) {
		if t.ID == id {
			return t, true
		}
	}
	return taskItem{}, false
}

func leasePaths(root, id string) (string, string, bool) {
	t, ok := currentTask(root, id)
	if !ok {
		return "", "", false
	}
	dir := filepath.Join(t.Dir, "tmp")
	return filepath.Join(dir, leaseLockName), filepath.Join(dir, leaseMetadataName), true
}

func openLeaseLock(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("task lease lock %q is not a regular file", path)
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
	f, err := openLeaseLock(lockPath, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil, taskLeaseObservation{}, errLeaseCandidateGone
	}
	if err != nil {
		return nil, taskLeaseObservation{}, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
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
		root: root,
		id:   item.ID,
		file: f,
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
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, taskLeaseObservation{}, err
	}
	current, ok := currentTask(root, item.ID)
	if !ok || current.State != item.State {
		// Metadata follows the id across legitimate moves once an iteration owns the task, but a
		// move DURING acquisition means this stale candidate must be rescanned, not launched.
		_ = removeLeaseMetadata(root, item.ID)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, taskLeaseObservation{}, errLeaseCandidateGone
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
	lockPath := filepath.Join(item.Dir, "tmp", leaseLockName)
	f, err := openLeaseLock(lockPath, false)
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
	meta, ok := readLeaseMetadata(filepath.Join(item.Dir, "tmp", leaseMetadataName))
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

func readLeaseMetadata(path string) (taskLeaseMetadata, bool) {
	data, err := os.ReadFile(path)
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
	_, metadataPath, ok := leasePaths(root, id)
	if !ok {
		return nil // the task was removed after completion; never recreate its previous path
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(metadataPath), "."+leaseMetadataName+"-*")
	if errors.Is(err, os.ErrNotExist) {
		return nil // done cleanup raced after release; it is already gone
	}
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, metadataPath); err != nil {
		_ = os.Remove(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func removeLeaseMetadata(root, id string) error {
	_, metadataPath, ok := leasePaths(root, id)
	if !ok {
		return nil
	}
	if err := os.Remove(metadataPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
