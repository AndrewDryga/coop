package tasks

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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	LeaseAuthorityVersion          = "v1"
	leaseAuthorityIdentityAttempts = 5
	auditReopenVersion             = 3
	auditReopenPendingVersion      = 4
	leaseHeartbeatInterval         = 10 * time.Second
	leaseStaleAfter                = time.Minute
	leaseMetadataVersion           = 1
)

const TestLeaseAuthorityRootEnv = "COOP_TEST_LEASE_AUTHORITY_ROOT"

var (
	errLeaseCandidateGone     = errors.New("lease candidate changed state")
	errLeaseAuthorityIdentity = errors.New("task lease authority changed identity while locking")
)

type leaseCompletionReceipt struct {
	Version               int    `json:"version"`
	Device                uint64 `json:"device"`
	Inode                 uint64 `json:"inode"`
	Nonce                 string `json:"nonce"`
	AuditReopenGeneration string `json:"audit_reopen_generation,omitempty"`
}

type AuditReopenCommit struct {
	TaskID        string `json:"task_id"`
	ChangeTree    string `json:"change_tree"`
	AuthorName    string `json:"author_name"`
	AuthorEmail   string `json:"author_email"`
	AuthorDate    string `json:"author_date"`
	CommitMessage string `json:"commit_message"`
}

type AuditReopenRecord struct {
	Version        int                 `json:"version"`
	Generation     string              `json:"generation"`
	TaskID         string              `json:"task_id"`
	BaselineHead   string              `json:"baseline_head,omitempty"`
	Subject        AuditReopenCommit   `json:"subject"`
	History        []AuditReopenCommit `json:"history"`
	UnblockPending bool                `json:"unblock_pending,omitempty"`
}

// taskLeaseOwner identifies the controller in lease metadata. The kernel lock, rather than any
// one of these fields, is the authority: run ids and PIDs exist for recovery evidence and UI only.
type TaskLeaseOwner struct {
	RunID    string
	PID      int
	Provider string
	Target   string
	Now      func() time.Time
	Ticker   func(time.Duration) (<-chan time.Time, func())
}

func (o TaskLeaseOwner) now() time.Time {
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

// TaskLease holds the host-only authoritative flock for one agent iteration. Metadata is keyed by
// repository and task id, so heartbeats remain stable while the worker moves the task folder.
type TaskLease struct {
	root      string
	id        string
	authority *os.File
	meta      taskLeaseMetadata
	now       func() time.Time
	ticker    func(time.Duration) (<-chan time.Time, func())
	Reopen    *AuditReopenRecord

	releaseOnce sync.Once
	quiesceOnce sync.Once
	releaseErr  error
	stop        chan struct{}
	done        chan struct{}
}

func (l *TaskLease) startHeartbeat() {
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

func (l *TaskLease) refresh() error {
	l.meta.HeartbeatAt = l.now()
	return writeLeaseAuthorityMetadata(l.root, l.id, l.meta)
}

// quiesce stops heartbeat writes while retaining the authoritative flock. Completion validation
// and cleanup call it before mutating the task directory, so metadata cannot race those changes.
func (l *TaskLease) Quiesce() {
	l.quiesceOnce.Do(func() { close(l.stop) })
	<-l.done
}

// markCompleted records host-only evidence on the already-persistent authority inode while its
// flock is held. Task-local state is provider-writable and cannot distinguish a foreign controller
// from an agent that moved an unleased folder.
func (l *TaskLease) MarkCompleted(taskDir string) error {
	generation := ""
	if l.Reopen != nil {
		generation = l.Reopen.Generation
	}
	return writeLeaseCompletionReceipt(l.authority, taskDir, generation)
}

func (l *TaskLease) ClearCompleted() error {
	return clearLeaseCompletionReceipt(l.authority)
}

func (l *TaskLease) ConsumeAuditReopen() error {
	if l.Reopen == nil {
		return nil
	}
	return removeAuditReopenRecordIfMatches(l.root, l.id, l.Reopen.Generation)
}

// Release stops metadata mutation before removing the evidence and dropping the flock. Authority
// lock files persist in the host-only registry so every controller opens the same inode.
func (l *TaskLease) Release() error {
	l.releaseOnce.Do(func() {
		l.Quiesce()
		l.releaseErr = errors.Join(
			removeLeaseAuthorityMetadata(l.root, l.id),
			unlockLeaseFile(l.authority),
		)
	})
	return l.releaseErr
}

func LeaseAuthorityKey(root, id string) (string, error) {
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

// leaseAuthorityRoot resolves where the host-global completion-trust registry lives. Everything
// in this registry — lock-authority inodes, completion receipts, audit-reopen authority, departure
// records, and the completion-window journal are DURABLE TRUST STATE, so they live with the session
// store's state root (see defaultSessionStateRoot in internal/sessionsvc/http.go), not under
// os.UserCacheDir(). A cache is OS-deletable BY CONTRACT: macOS
// purges ~/Library/Caches under pressure and cleaners empty it wholesale. A purge mid-run unlinks
// lock files whose fds are still flocked, so the next open recreates the name as a new inode and
// two controllers each hold an "exclusive" lock on a different one; a purge between runs erases the
// receipts crash recovery reads, degrading it to restore-and-redo and stranding audit-reopened
// tasks behind manual repair.
func leaseAuthorityRoot() (string, error) {
	if strings.HasSuffix(filepath.Base(os.Args[0]), ".test") {
		if root := os.Getenv(TestLeaseAuthorityRootEnv); root != "" {
			return root, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "coop", "task-leases", LeaseAuthorityVersion), nil
}

func legacyLeaseAuthorityRoot() (string, error) {
	cache, cacheErr := os.UserCacheDir()
	if cacheErr != nil {
		return "", cacheErr
	}
	return filepath.Join(cache, "coop", "task-leases", LeaseAuthorityVersion), nil
}

func OpenLeaseAuthorityRoot() (*os.Root, error) {
	dir, err := leaseAuthorityRoot()
	if err != nil {
		return nil, err
	}
	return openLeaseAuthorityRootAt(dir, legacyLeaseAuthorityRoot)
}

func openLeaseAuthorityRootAt(dir string, resolveLegacy func() (string, error)) (*os.Root, error) {
	// Current authority wins without even resolving the retired cache path. Besides keeping the
	// steady state small, this means a stale or unreadable cache cannot break a current install.
	if _, err := os.Lstat(dir); err == nil {
		return openLeaseAuthorityRoot(dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	legacy, err := resolveLegacy()
	if err != nil {
		return nil, fmt.Errorf(
			"resolve pre-v8 task lease authority from the OS cache before creating %q: %w; stop all older Coop processes and migrate the whole pre-v8 registry directory before retrying",
			dir, err,
		)
	}
	if err := rejectLegacyLeaseAuthorityRoot(dir, legacy); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return openLeaseAuthorityRoot(dir)
}

func openLeaseAuthorityRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("task lease authority %q is not a real directory", dir)
	}
	return os.OpenRoot(dir)
}

func rejectLegacyLeaseAuthorityRoot(dir, legacy string) error {
	// Any entry may be trust state or evidence of an interrupted write. V9 does not interpret,
	// merge, move, or delete it; the operator must migrate the whole directory while old Coop
	// processes are stopped. This check cannot synchronize with binaries that still write there.
	root, err := os.OpenFile(legacy, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return legacyLeaseAuthorityRefusal(
			dir, legacy, fmt.Errorf("the retired path is not a readable real directory: %w", err),
		)
	}
	_, readErr := root.Readdirnames(1)
	closeErr := root.Close()
	if errors.Is(readErr, io.EOF) && closeErr == nil {
		return nil
	}
	if readErr != nil {
		return legacyLeaseAuthorityRefusal(dir, legacy, errors.Join(readErr, closeErr))
	}
	if closeErr != nil {
		return legacyLeaseAuthorityRefusal(dir, legacy, closeErr)
	}
	return legacyLeaseAuthorityRefusal(dir, legacy, errors.New("the retired registry is not empty"))
}

func legacyLeaseAuthorityRefusal(dir, legacy string, cause error) error {
	return fmt.Errorf(
		"pre-v8 task lease authority at %q cannot be ignored before creating %q: %w; stop all older Coop processes and migrate the whole directory before retrying",
		legacy, dir, cause,
	)
}

// leaseAuthorityIsCurrent proves, with the kernel lock already held, that the locked inode is still
// the one the registry name resolves to. flock binds a process to an INODE, never to a name: if the
// record is unlinked and recreated between open and flock, two controllers each hold an "exclusive"
// lock on a different inode and the single-writer invariant dissolves in silence. Comparing
// fstat(fd) against a fresh lstat(name) is the only evidence that the lock we hold is the lock every
// other controller contends for.
func leaseAuthorityIsCurrent(file *os.File, name string) (bool, error) {
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return false, err
	}
	defer registry.Close()
	named, err := registry.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // unlinked underfoot: the lock we hold now guards an orphan
	}
	if err != nil {
		return false, err
	}
	locked, err := file.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(locked, named), nil
}

// lockLeaseAuthorityWith opens the registry record for id, takes the caller's kernel lock on it, and
// only then proves the locked inode still answers the registry name. On a mismatch it drops the lock
// and retries the whole open-and-lock, because the inode that answers the name NOW is the one every
// other controller will contend for. Retries are bounded: a name whose identity keeps changing is a
// purge or an attack, and the caller must see an error rather than silently hold an exclusive lock
// on an inode nobody else can reach.
func lockLeaseAuthorityWith(root, id string, create bool, lock func(*os.File) error) (*os.File, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return nil, err
	}
	name := key + ".lock"
	for attempt := 0; ; attempt++ {
		file, err := openLeaseAuthorityRecord(name, create)
		if err != nil {
			return nil, err
		}
		if err := lock(file); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		current, err := leaseAuthorityIsCurrent(file, name)
		if err != nil {
			return nil, errors.Join(err, unlockLeaseFile(file))
		}
		if current {
			return file, nil
		}
		if err := unlockLeaseFile(file); err != nil {
			return nil, err
		}
		if attempt+1 >= leaseAuthorityIdentityAttempts {
			return nil, fmt.Errorf("%w: %s", errLeaseAuthorityIdentity, name)
		}
	}
}

func lockLeaseAuthority(root, id string, create bool, how int) (*os.File, error) {
	return lockLeaseAuthorityWith(root, id, create, func(file *os.File) error {
		return syscall.Flock(int(file.Fd()), how)
	})
}

func lockLeaseAuthorityForAudit(root, id string, create bool, label string, owned func() bool) (*os.File, error) {
	return lockLeaseAuthorityWith(root, id, create, func(file *os.File) error {
		return lockExclusiveForCompletionAudit(file, label, owned)
	})
}

// openLeaseAuthority opens a task's authority record WITHOUT locking it. Production code takes the
// lock through lockLeaseAuthority instead, so that every held lock has been proved to be on the
// inode the registry name still resolves to; an unlocked open carries no such guarantee.
func OpenLeaseAuthority(root, id string, create bool) (*os.File, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return nil, err
	}
	return openLeaseAuthorityRecord(key+".lock", create)
}

func openLeaseAuthorityRecord(name string, create bool) (*os.File, error) {
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return nil, err
	}
	defer registry.Close()
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

func writeLeaseCompletionReceipt(authority *os.File, taskDir string, generation ...string) error {
	receipt, err := completionReceiptFor(taskDir)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	receipt.Nonce = hex.EncodeToString(nonce)
	if len(generation) > 0 {
		receipt.AuditReopenGeneration = generation[0]
	}
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

const (
	trustedDoneDepartureVersion = 1
	trustedDoneDepartureLimit   = 32
)

// trustedDoneDeparture records host-authorized exits from 99_done. Completion receipts are
// deliberately cleared before a task is made actionable again, so a later integrity window needs
// this bounded nonce history to distinguish that host action from an unreceipted folder move.
type trustedDoneDeparture struct {
	Version int      `json:"version"`
	TaskID  string   `json:"task_id"`
	Nonces  []string `json:"nonces"`
}

func trustedDoneDepartureName(root, id string) (string, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return "", err
	}
	return key + ".departure.json", nil
}

func validateTrustedDoneDeparture(record trustedDoneDeparture, id string) error {
	if record.Version != trustedDoneDepartureVersion || record.TaskID != id || len(record.Nonces) == 0 || len(record.Nonces) > trustedDoneDepartureLimit {
		return errors.New("invalid trusted done departure record")
	}
	seen := make(map[string]bool, len(record.Nonces))
	for _, nonce := range record.Nonces {
		if len(nonce) != 32 || seen[nonce] {
			return errors.New("invalid trusted done departure record")
		}
		if _, err := hex.DecodeString(nonce); err != nil {
			return errors.New("invalid trusted done departure record")
		}
		seen[nonce] = true
	}
	return nil
}

func readTrustedDoneDeparture(root, id string) (trustedDoneDeparture, bool, error) {
	name, err := trustedDoneDepartureName(root, id)
	if err != nil {
		return trustedDoneDeparture{}, false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return trustedDoneDeparture{}, false, err
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, name)
	if errors.Is(err, os.ErrNotExist) {
		return trustedDoneDeparture{}, false, nil
	}
	if err != nil {
		return trustedDoneDeparture{}, false, err
	}
	var record trustedDoneDeparture
	if err := json.Unmarshal(data, &record); err != nil {
		return trustedDoneDeparture{}, false, err
	}
	if err := validateTrustedDoneDeparture(record, id); err != nil {
		return trustedDoneDeparture{}, false, err
	}
	return record, true, nil
}

func writeTrustedDoneDeparture(root string, record trustedDoneDeparture) error {
	if err := validateTrustedDoneDeparture(record, record.TaskID); err != nil {
		return err
	}
	name, err := trustedDoneDepartureName(root, record.TaskID)
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

func removeTrustedDoneDeparture(root, id string) error {
	name, err := trustedDoneDepartureName(root, id)
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

func appendTrustedDoneDeparture(root, id, nonce string) error {
	record, ok, err := readTrustedDoneDeparture(root, id)
	if err != nil {
		return err
	}
	if !ok {
		record = trustedDoneDeparture{Version: trustedDoneDepartureVersion, TaskID: id}
	}
	if !slices.Contains(record.Nonces, nonce) {
		record.Nonces = append(record.Nonces, nonce)
		if len(record.Nonces) > trustedDoneDepartureLimit {
			record.Nonces = record.Nonces[len(record.Nonces)-trustedDoneDepartureLimit:]
		}
	}
	return writeTrustedDoneDeparture(root, record)
}

const taskOwnerRecordVersion = 1

// taskOwnerSourceInteractiveClaim is the only source `coop tasks claim` ever writes. The field
// exists so a future host-only claim path is provable from the record instead of merely assumed.
const taskOwnerSourceInteractiveClaim = "interactive-claim"

// taskOwnerRecord is durable evidence that a HUMAN, not a process, owns a task. A lease is a kernel
// flock held for one loop iteration; `coop tasks claim` exits immediately and holds nothing, so a
// human's claim needs a record instead of a lock — the same host-only registry the lease authority
// and audit-reopen records already live in, keyed the same way (see leaseAuthorityKey). It is
// cleared ONLY by an explicit lifecycle act — done, block, unblock, or `coop tasks release` — never
// inferred from mtime, PID, or heartbeat age: none of those can tell "gone quiet for a good reason"
// from "abandoned," and guessing wrong is the exact incident this record exists to prevent. See
// .agent/kb/task-authority-model.md for how this sits alongside the lease, checkout, and ref
// authorities.
type TaskOwnerRecord struct {
	Version   int       `json:"version"`
	TaskID    string    `json:"task_id"`
	Source    string    `json:"source"`
	User      string    `json:"user"`
	Host      string    `json:"host"`
	ClaimedAt time.Time `json:"claimed_at"`
}

func taskOwnerRecordName(root, id string) (string, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return "", err
	}
	return key + ".owner.json", nil
}

// validateTaskOwnerRecord mirrors validateAuditReopenRecord/validateTrustedDoneDeparture: a record
// whose TaskID doesn't match the id being read (or that's otherwise malformed) is rejected outright
// rather than silently treated as absent — a corrupt or mismatched record must fail closed, not
// quietly stop protecting the task it claims to own.
func validateTaskOwnerRecord(record TaskOwnerRecord, id string) error {
	if record.Version != taskOwnerRecordVersion || record.TaskID != id || record.Source == "" ||
		strings.TrimSpace(record.User) == "" || strings.TrimSpace(record.Host) == "" || record.ClaimedAt.IsZero() {
		return errors.New("invalid task owner record")
	}
	return nil
}

// readTaskOwnerRecord reports a task's durable claim, if any. A missing record reads as (zero,
// false, nil) — the common case, since most tasks are never interactively claimed — but a present,
// invalid record (corrupt JSON, mismatched id) surfaces as an error rather than "no owner": the
// caller (assignLoopTaskOnly) must fail closed rather than adopt work it could not actually verify
// is unowned.
func ReadTaskOwnerRecord(root, id string) (TaskOwnerRecord, bool, error) {
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
	var record TaskOwnerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return TaskOwnerRecord{}, false, err
	}
	if err := validateTaskOwnerRecord(record, id); err != nil {
		return TaskOwnerRecord{}, false, err
	}
	return record, true, nil
}

func writeTaskOwnerRecord(root string, record TaskOwnerRecord) error {
	if err := validateTaskOwnerRecord(record, record.TaskID); err != nil {
		return err
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

// removeTaskOwnerRecord is idempotent — a missing record is not an error — because most lifecycle
// transitions (done/block/unblock on a task the loop, not a human, adopted) legitimately have none.
func removeTaskOwnerRecord(root, id string) error {
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

func auditReopenRecordName(root, id string) (string, error) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return "", err
	}
	return key + ".reopen.json", nil
}

func validateAuditReopenRecord(record AuditReopenRecord, id string) error {
	current := record.Version == auditReopenVersion && !record.UnblockPending ||
		record.Version == auditReopenPendingVersion && record.UnblockPending
	if !current || record.Generation == "" || record.TaskID != id ||
		record.Subject.TaskID != id || record.Subject.ChangeTree == "" {
		return errors.New("invalid audit reopen record")
	}
	if !validAuditReopenHead(record.BaselineHead) || record.History == nil ||
		len(record.History) > auditReopenHistoryLimit {
		return errors.New("invalid complete-history audit reopen record")
	}
	seen := map[string]bool{id: true}
	for _, commit := range record.History {
		if commit.ChangeTree == "" || (commit.TaskID != "" && seen[commit.TaskID]) {
			return errors.New("invalid audit reopen history")
		}
		if commit.TaskID != "" {
			seen[commit.TaskID] = true
		}
	}
	return nil
}

func validAuditReopenHead(head string) bool {
	if len(head) != 40 && len(head) != 64 {
		return false
	}
	_, err := hex.DecodeString(head)
	return err == nil
}

func auditReopenRecordActive(record AuditReopenRecord) bool {
	return record.Version == auditReopenVersion && !record.UnblockPending
}

func AuditReopenRecordsEqual(a, b AuditReopenRecord) bool {
	return a.Version == b.Version && a.Generation == b.Generation && a.TaskID == b.TaskID &&
		a.BaselineHead == b.BaselineHead && a.Subject == b.Subject &&
		slices.Equal(a.History, b.History) && a.UnblockPending == b.UnblockPending
}

func ReadAuditReopenRecord(root, id string) (AuditReopenRecord, bool, error) {
	name, err := auditReopenRecordName(root, id)
	if err != nil {
		return AuditReopenRecord{}, false, err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return AuditReopenRecord{}, false, err
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, name)
	if errors.Is(err, os.ErrNotExist) {
		return AuditReopenRecord{}, false, nil
	}
	if err != nil {
		return AuditReopenRecord{}, false, err
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return AuditReopenRecord{}, false, err
	}
	if envelope.Version != auditReopenVersion && envelope.Version != auditReopenPendingVersion {
		return AuditReopenRecord{}, false, fmt.Errorf("unsupported audit reopen record version %d", envelope.Version)
	}
	var record AuditReopenRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return AuditReopenRecord{}, false, err
	}
	if err := validateAuditReopenRecord(record, id); err != nil {
		return AuditReopenRecord{}, false, err
	}
	return record, true, nil
}

func WriteAuditReopenRecord(root string, record AuditReopenRecord) error {
	if err := validateAuditReopenRecord(record, record.TaskID); err != nil {
		return err
	}
	name, err := auditReopenRecordName(root, record.TaskID)
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

func replaceAuditReopenRecordIfMatches(root string, previous, replacement AuditReopenRecord) error {
	current, ok, err := ReadAuditReopenRecord(root, previous.TaskID)
	if err != nil {
		return err
	}
	if !ok || !AuditReopenRecordsEqual(current, previous) {
		return fmt.Errorf("audit reopen authority changed for task %s", previous.TaskID)
	}
	if replacement.Generation != previous.Generation || replacement.TaskID != previous.TaskID {
		return fmt.Errorf("audit reopen replacement changed authority for task %s", previous.TaskID)
	}
	return WriteAuditReopenRecord(root, replacement)
}

func removeAuditReopenRecord(root, id string) error {
	name, err := auditReopenRecordName(root, id)
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

func auditReopenRecordExists(root, id string) bool {
	name, err := auditReopenRecordName(root, id)
	if err != nil {
		return false
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return false
	}
	defer registry.Close()
	info, err := registry.Lstat(name)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return info.Mode().IsRegular() && ok && stat.Nlink == 1
}

func removeAuditReopenRecordIfMatches(root, id, generation string) error {
	record, ok, err := ReadAuditReopenRecord(root, id)
	if err != nil || !ok {
		return err
	}
	if record.Generation != generation {
		return fmt.Errorf("audit reopen generation changed for task %s", id)
	}
	return removeAuditReopenRecord(root, id)
}

func newAuditReopenGeneration() (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
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

func inspectTaskCompletionReceipt(root string, task Item) (leaseCompletionReceipt, bool, bool) {
	authority, err := lockLeaseAuthority(root, task.ID, false, syscall.LOCK_SH|syscall.LOCK_NB)
	if errors.Is(err, os.ErrNotExist) {
		return leaseCompletionReceipt{}, false, false
	}
	if err != nil {
		return leaseCompletionReceipt{}, false, true
	}
	receipt, ok := readLeaseCompletionReceipt(authority, task.Dir)
	if err := unlockLeaseFile(authority); err != nil {
		return leaseCompletionReceipt{}, false, true
	}
	return receipt, ok, false
}

func clearTaskCompletionReceipt(root, id string) error {
	authority, err := lockLeaseAuthority(root, id, false, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil // a new owner cleared the old receipt while acquiring this same flock
	}
	if err != nil {
		return err
	}
	return errors.Join(clearLeaseCompletionReceipt(authority), unlockLeaseFile(authority))
}

// clearTaskCompletionReceiptIfMatches invalidates only the generation the caller observed. The
// exclusive authority lock closes the gap between comparing a stale receipt and clearing it, so a
// concurrent trusted completion cannot publish a fresh nonce that this audit then erases.
func clearTaskCompletionReceiptIfMatches(root string, task Item, nonce string) (bool, error) {
	if nonce == "" {
		return false, nil
	}
	authority, err := lockLeaseAuthorityForAudit(root, task.ID, false, "task "+task.ID+" authority", func() bool {
		return leaseAuthorityMetadataExists(root, task.ID)
	})
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errCompletionAuditLockOwned) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current, ok := CurrentTask(root, task.ID)
	if !ok || current.State != StateDone || current.Dir != task.Dir {
		return false, unlockLeaseFile(authority)
	}
	receipt, ok := readLeaseCompletionReceipt(authority, current.Dir)
	if !ok || receipt.Nonce != nonce {
		return false, unlockLeaseFile(authority)
	}
	return true, errors.Join(clearLeaseCompletionReceipt(authority), unlockLeaseFile(authority))
}

func writeLeaseAuthorityMetadata(root, id string, meta taskLeaseMetadata) error {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return err
	}
	defer registry.Close()
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return AtomicWriteTaskFile(registry, key+".json", append(data, '\n'))
}

func readLeaseAuthorityMetadata(root, id string) (taskLeaseMetadata, bool) {
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return taskLeaseMetadata{}, false
	}
	registry, err := OpenLeaseAuthorityRoot()
	if err != nil {
		return taskLeaseMetadata{}, false
	}
	defer registry.Close()
	data, err := ReadTaskMetadataFile(registry, key+".json")
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
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return false
	}
	registry, err := OpenLeaseAuthorityRoot()
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
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
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
	key, err := LeaseAuthorityKey(root, id)
	if err != nil {
		return err
	}
	registry, err := OpenLeaseAuthorityRoot()
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

type TaskLeaseObservation struct {
	State    taskLeaseState
	Provider string
}

func (o TaskLeaseObservation) label() string {
	switch o.State {
	case leaseStalled:
		return "stalled " + o.Provider
	case leaseBusy:
		return "busy " + o.Provider
	default:
		return "unleased"
	}
}

type TaskLeaseSummary struct {
	Owned   int // a durable human claim (taskOwnerRecord) — never a lease state, so add() can't set it
	Busy    int
	Stalled int
}

func (s *TaskLeaseSummary) add(o TaskLeaseObservation) {
	switch o.State {
	case leaseBusy:
		s.Busy++
	case leaseStalled:
		s.Stalled++
	}
}

func (s TaskLeaseSummary) String() string {
	parts := make([]string, 0, 3)
	if s.Owned > 0 {
		parts = append(parts, fmt.Sprintf("%d owned", s.Owned))
	}
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

func CurrentTask(root, id string) (Item, bool) {
	for _, t := range ReadTaskTree(root) {
		if t.ID == id {
			return t, true
		}
	}
	return Item{}, false
}

// TryTaskLease locks a candidate without waiting, writes metadata only after the authoritative
// flock succeeds, then rechecks that the candidate did not move during acquisition.
func TryTaskLease(root string, item Item, owner TaskLeaseOwner) (*TaskLease, TaskLeaseObservation, error) {
	authority, err := lockLeaseAuthority(root, item.ID, true, syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, TaskLeaseObservation{}, err
		}
		observed := observeHeldTaskLease(item, owner.now())
		if observed.State == leaseUnleased {
			return nil, TaskLeaseObservation{}, errLeaseCandidateGone
		}
		return nil, observed, nil
	}
	reject := func(cause error) (*TaskLease, TaskLeaseObservation, error) {
		return nil, TaskLeaseObservation{}, errors.Join(cause, unlockLeaseFile(authority))
	}
	current, ok := CurrentTask(root, item.ID)
	if !ok || current.State != item.State {
		return reject(errLeaseCandidateGone)
	}
	var reopen *AuditReopenRecord
	if record, reopened, err := ReadAuditReopenRecord(root, item.ID); err != nil {
		return reject(fmt.Errorf("read audit reopen authority for task %s: %w", item.ID, err))
	} else if reopened {
		if record.UnblockPending {
			recovery := "coop tasks unblock " + item.ID
			if item.State != StateTodo {
				recovery = "coop tasks block " + item.ID + " && " + recovery
			}
			return reject(fmt.Errorf(
				"task %s has a non-authorizing pending audit unblock — no lease started; recover explicitly: %s",
				item.ID, recovery,
			))
		}
		reopen = &record
	}
	abort := func(cause error) (*TaskLease, TaskLeaseObservation, error) {
		cleanup := errors.Join(
			removeLeaseAuthorityMetadata(root, item.ID),
			unlockLeaseFile(authority),
		)
		return nil, TaskLeaseObservation{}, errors.Join(cause, cleanup)
	}
	now := owner.now()
	l := &TaskLease{
		root:      root,
		id:        item.ID,
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
		Reopen: reopen,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if l.ticker == nil {
		l.ticker = realLeaseTicker
	}
	if err := l.refresh(); err != nil {
		return abort(err)
	}
	current, ok = CurrentTask(root, item.ID)
	if !ok || current.State != item.State {
		// Metadata follows the id across legitimate moves once an iteration owns the task, but a
		// move DURING acquisition means this stale candidate must be rescanned, not launched.
		return abort(errLeaseCandidateGone)
	}
	// A new owned attempt invalidates any receipt from an earlier accepted completion. The same
	// authority flock serializes this with concurrent completion scans.
	if err := clearLeaseCompletionReceipt(authority); err != nil {
		return abort(err)
	}
	l.startHeartbeat()
	return l, TaskLeaseObservation{}, nil
}

func observeTaskLease(item Item, now time.Time) TaskLeaseObservation {
	return observeHeldTaskLease(item, now)
}

// observeHeldTaskLease never creates lease state: task/watch is read-only. An unlocked or missing
// lock is unleased even if a crashed controller left stale metadata behind; only a HELD lock means
// another controller owns the work.
func observeHeldTaskLease(item Item, now time.Time) TaskLeaseObservation {
	root := filepath.Dir(filepath.Dir(item.Dir))
	authority, err := lockLeaseAuthority(root, item.ID, false, syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		_ = unlockLeaseFile(authority)
	case errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN):
		meta, ok := readLeaseAuthorityMetadata(root, item.ID)
		return leaseObservationFromMetadata(meta, ok, now)
	case !errors.Is(err, os.ErrNotExist):
		return TaskLeaseObservation{State: leaseBusy, Provider: "unknown"}
	}
	return TaskLeaseObservation{State: leaseUnleased}
}

func leaseObservationFromMetadata(meta taskLeaseMetadata, ok bool, now time.Time) TaskLeaseObservation {
	provider := "unknown"
	if ok {
		provider = leaseProvider(meta.Provider)
		if now.Sub(meta.HeartbeatAt) > leaseStaleAfter {
			return TaskLeaseObservation{State: leaseStalled, Provider: provider}
		}
	}
	return TaskLeaseObservation{State: leaseBusy, Provider: provider}
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
