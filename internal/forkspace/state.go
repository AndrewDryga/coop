package forkspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

// The wire header stays line-oriented, so the file remains readable with `sed -n 1,4p` when an
// operator has to recover one by hand.
const (
	OwnerStateV1  = "owner-v1\n"
	OwnerStateV2  = "owner-v2\n"
	GenerationTag = "generation="
)

// workerStateLimit caps a lifecycle record read. A real record is about 150 bytes.
const workerStateLimit = 4 << 10

var (
	// ErrUnsupportedWorkerStateVersion keeps a future writer's state fail-closed until that version
	// can interpret its own lifecycle contract.
	ErrUnsupportedWorkerStateVersion = errors.New("unsupported detached-worker state version")
	// ErrNotAStateFile matches an error that says what Coop found at a fork state file's name in place
	// of a file it wrote: a link, a named pipe, a folder, a socket, a device, or a lifecycle record
	// larger than any Coop writes. A box whose mount holds the .coop folder can put any of them there,
	// and reading one could wait forever or never end.
	ErrNotAStateFile = errors.New("not a file Coop wrote")
	// Injectable only inside this package to qualify first-create parent-directory barriers.
	syncForkDirectoryEntry = func(dir *os.File) error { return dir.Sync() }
)

// SignalPID is the kill(2) the lifecycle probes and the supervisor share, as one seam so a test can
// stub liveness once for both.
var SignalPID = syscall.Kill

// Per-fork process state (logs + pidfiles) lives in <repo>-forks/.coop/.
func StateDir(repo string) string       { return filepath.Join(Home(repo), ".coop") }
func LogPath(repo, name string) string  { return filepath.Join(StateDir(repo), name+".log") }
func PidPath(repo, name string) string  { return filepath.Join(StateDir(repo), name+".pid") }
func LockPath(repo, name string) string { return filepath.Join(StateDir(repo), name+".lock") }

// EnsureStateDir creates or tightens the central fork control directory. Fork workspaces remain
// ordinary project directories; only the host-owned .coop authority and its descendants are
// owner-private.
func EnsureStateDir(repo string) error {
	if err := ensureForkHome(repo); err != nil {
		return err
	}
	return ensurePrivateStateDir(StateDir(repo))
}

func ensureForkHome(repo string) error {
	home := Home(repo)
	// A fork home is a direct sibling of an existing project, so its parent already exists. Avoid
	// MkdirAll here: every directory Coop creates must have a specific parent durability barrier.
	if err := os.Mkdir(home, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(home)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fork home path %q is not a real directory", home)
	}
	return confirmForkDirectoryEntry(home)
}

func ensurePrivateStateDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("fork state path %q is not a real directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("make fork state path %q owner-only: %w", path, err)
	}
	// Repeat this barrier even when the directory was already visible. A previous creation may
	// have returned an fsync error after mkdir; the idempotent retry is what confirms publication.
	return confirmForkDirectoryEntry(path)
}

func confirmForkDirectoryEntry(path string) error {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(syncForkDirectoryEntry(parent), parent.Close())
}

// LockState serializes a detached start through its child's record, the child's confirmation and
// cleanup, and stop for one fork. The lock file persists, but flock ownership does not: the kernel
// releases it if a coop process crashes.
func LockState(repo, name string) (func(), error) {
	return LockStateContext(context.Background(), repo, name)
}

// LockStateContext acquires a lifecycle lock without making service shutdown
// wait forever behind another process. The ordinary command path passes a
// background context and retains the historical blocking behavior.
func LockStateContext(ctx context.Context, repo, name string) (func(), error) {
	if err := EnsureStateDir(repo); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(LockPath(repo, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// TryLockState is used by worker-exit cleanup: if stop already owns the lifecycle lock, the
// worker must be allowed to exit rather than wait behind the command that's waiting for its exit.
func TryLockState(repo, name string) (func(), bool) {
	if err := EnsureStateDir(repo); err != nil {
		return nil, false
	}
	f, err := os.OpenFile(LockPath(repo, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

// ProcessIdentity is what the kernel can prove about the process a fork's state names.
type ProcessIdentity uint8

const (
	ProcessGone ProcessIdentity = iota
	ProcessIdentityMatch
	ProcessIdentityMismatch
	ProcessIdentityUnknown
)

// ProcessIdentityOf separates conservative liveness from authorization to signal. Destructive
// guards retain unknown state, but status calls only a corroborated identity running.
func ProcessIdentityOf(pid int, token string) ProcessIdentity {
	if pid <= 1 { // a detached worker cannot be init; -1 is kill(2)'s broadcast target
		return ProcessGone
	}
	if err := SignalPID(pid, 0); errors.Is(err, syscall.ESRCH) {
		return ProcessGone
	} else if err != nil {
		return ProcessIdentityUnknown
	}
	if token == "" {
		return ProcessIdentityUnknown
	}
	if !StableProcToken(token) {
		return ProcessIdentityUnknown
	}
	cur := ProcStartToken(pid)
	if cur == "" {
		// The process may have exited between kill(0) and the identity read. Recheck so that ordinary
		// exit is not misreported as an unverifiable live PID, while PID reuse still fails closed.
		if err := SignalPID(pid, 0); errors.Is(err, syscall.ESRCH) {
			return ProcessGone
		}
		return ProcessIdentityUnknown
	}
	if cur != token {
		return ProcessIdentityMismatch
	}
	return ProcessIdentityMatch
}

// processAlive reports the one identity that authorizes treating a fork as running.
func processAlive(pid int, token string) bool {
	return ProcessIdentityOf(pid, token) == ProcessIdentityMatch
}

// OwnerProvablyDead is the ONE test the fork lifecycle shares for "the process this record names is
// not running any more": the kernel says its pid is gone, or that pid now belongs to a different
// process than the one recorded. A live match and — deliberately — an identity coop could not read
// are both unproven, so every caller (stop, the merge's rebase recovery) fails closed on them.
// Identity is pid + start token or nothing: no file age, no elapsed time.
func OwnerProvablyDead(identity ProcessIdentity) bool {
	return identity == ProcessGone || identity == ProcessIdentityMismatch
}

// StateOwner reports the worker a fork's lifecycle record still names as possibly running, so a
// recovery that would disturb the fork can keep its hands off one somebody else owns. held is false
// ONLY when there is no record at all, or its worker is provably dead; a record coop cannot read and
// an identity it cannot verify both hold the fork. pid is the recorded worker (0 when unreadable).
func StateOwner(repo, name string) (pid int, held bool) {
	state, err := ReadWorkerState(repo, name)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false
	}
	if err != nil {
		return 0, true
	}
	return state.Pid, !OwnerProvablyDead(ProcessIdentityOf(state.Pid, state.Token))
}

// RunningPid returns the live pid of a detached loop for name, or 0. It deliberately preserves a
// dead or reused worker's record: that worker may have orphaned its box, and only a successful
// forkStop may discard the exact-label reap handle.
func RunningPid(repo, name string) int {
	state, err := ReadWorkerState(repo, name)
	if err != nil || !processAlive(state.Pid, state.Token) {
		return 0
	}
	return state.Pid
}

// NeedsStop is the destructive-operation guard: any lifecycle record — a live worker, a dead or
// reused one, or bytes Coop cannot read — must be resolved by `fork stop` before the worktree can be
// merged, replaced, pruned, or removed.
func NeedsStop(repo, name string) bool {
	return pathExists(PidPath(repo, name))
}

// parsePidfile reads a record's "<pid>\n<start-token>" body. pid 0 means unparseable.
func parsePidfile(s string) (int, string) {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", 2)
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, ""
	}
	if len(lines) == 2 {
		return pid, strings.TrimSpace(lines[1])
	}
	return pid, ""
}

// WorkerState is the one parse/validate/marshal boundary for a fork's lifecycle record: the
// detached worker's pid and start token, scoped to the fork generation it serves.
type WorkerState struct {
	Pid        int
	Token      string
	Generation Generation
}

func ParseWorkerState(raw string) (WorkerState, error) {
	first, _, _ := strings.Cut(raw, "\n")
	version := 0
	body := raw
	switch {
	case strings.HasPrefix(raw, OwnerStateV1):
		version, body = 1, strings.TrimPrefix(raw, OwnerStateV1)
	case strings.HasPrefix(raw, OwnerStateV2):
		version, body = 2, strings.TrimPrefix(raw, OwnerStateV2)
	}
	if version == 0 {
		if strings.HasPrefix(first, "owner-") {
			return WorkerState{}, fmt.Errorf("%w %q", ErrUnsupportedWorkerStateVersion, first)
		}
		return WorkerState{}, errors.New("detached worker state is missing a supported owner header")
	}
	state := WorkerState{}
	if version == 2 {
		line, rest, ok := strings.Cut(body, "\n")
		if !ok || !strings.HasPrefix(line, GenerationTag) {
			return WorkerState{}, errors.New("detached worker state is missing its fork generation")
		}
		state.Generation = Generation(strings.TrimPrefix(line, GenerationTag))
		if !ValidGeneration(state.Generation) {
			return WorkerState{}, errors.New("detached worker state has an invalid fork generation")
		}
		body = rest
	}
	state.Pid, state.Token = parsePidfile(body)
	if state.Pid <= 1 {
		return WorkerState{}, fmt.Errorf("invalid detached worker pid %d", state.Pid)
	}
	return state, nil
}

func (state WorkerState) Marshal() ([]byte, error) {
	prefix := OwnerStateV1
	if state.Generation != "" {
		if !ValidGeneration(state.Generation) {
			return nil, errors.New("invalid fork state: malformed generation")
		}
		prefix = OwnerStateV2 + GenerationTag + string(state.Generation) + "\n"
	}
	if state.Pid <= 1 {
		return nil, fmt.Errorf("invalid detached worker pid %d", state.Pid)
	}
	return []byte(fmt.Sprintf("%s%d\n%s\n", prefix, state.Pid, state.Token)), nil
}

// ReadWorkerState reads and parses a fork's lifecycle record. A failure to read it is an
// *os.PathError, os.ErrNotExist when there is no record. Any other error means a record is there
// that Coop cannot own: ErrNotAStateFile, or bytes ParseWorkerState refuses.
func ReadWorkerState(repo, name string) (WorkerState, error) {
	data, err := readWorkerRecord(PidPath(repo, name))
	if err != nil {
		return WorkerState{}, err
	}
	return ParseWorkerState(string(data))
}

// OpenStateFile opens a file in a fork's .coop folder for reading. A box whose mount holds that
// folder can put anything at a name there, so the open neither follows a link nor waits on a named
// pipe, and anything but a regular file is refused with an error that matches ErrNotAStateFile and
// says what is there. Any other failure, a missing file included, is an *os.PathError.
func OpenStateFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		// O_NOFOLLOW refuses a link with ELOOP, and open(2) refuses a socket with ENXIO on Linux and
		// EOPNOTSUPP on macOS. Lstat names what is there; if that changed meanwhile, the open's error
		// stands.
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.EOPNOTSUPP) {
			if info, statErr := os.Lstat(path); statErr == nil && !info.Mode().IsRegular() {
				return nil, &notAStateFile{path, fileKind(info.Mode())}
			}
		}
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &notAStateFile{path, fileKind(info.Mode())}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// notAStateFile says what Coop found at a fork state file's name. It is deliberately not an
// *os.PathError: callers tell a failed read from a file Coop cannot own by that type.
type notAStateFile struct{ path, found string }

func (e *notAStateFile) Error() string        { return e.path + " is " + e.found }
func (e *notAStateFile) Is(target error) bool { return target == ErrNotAStateFile }

// fileKind names the type of a file that is not a regular file, in plain words.
func fileKind(mode os.FileMode) string {
	switch {
	case mode&os.ModeSymlink != 0:
		return "a link"
	case mode&os.ModeNamedPipe != 0:
		return "a named pipe"
	case mode.IsDir():
		return "a folder"
	case mode&os.ModeSocket != 0:
		return "a socket"
	case mode&os.ModeDevice != 0:
		return "a device"
	default:
		return "not a regular file"
	}
}

// readWorkerRecord returns a lifecycle record's bytes, opened by OpenStateFile and never more than
// workerStateLimit of them.
func readWorkerRecord(path string) ([]byte, error) {
	f, err := OpenStateFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, workerStateLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > workerStateLimit {
		size := int64(len(data))
		if info, err := f.Stat(); err == nil {
			size = max(size, info.Size())
		}
		return nil, &notAStateFile{path, fmt.Sprintf("%d bytes, larger than any record", size)}
	}
	return data, nil
}

// WriteWorkerState atomically replaces the record, so a reader sees either the previous complete
// record or the new one, never a truncated identity.
func WriteWorkerState(repo, name string, state WorkerState) error {
	data, err := state.Marshal()
	if err != nil {
		return err
	}
	if err := EnsureStateDir(repo); err != nil {
		return err
	}
	f, err := os.CreateTemp(StateDir(repo), "."+name+".pid-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, PidPath(repo, name))
}

// WritePid records a detached worker: its pid, its kernel start token, so a pid reused after a crash
// or a reboot is never mistaken for it, and the fork generation it serves. The caller holds the
// lifecycle lock. A process without a stable token is refused: stop could never prove it is the same.
func WritePid(repo, name string, pid int, generation Generation) error {
	state, err := workerStateForPID(pid, generation)
	if err != nil {
		return err
	}
	return WriteWorkerState(repo, name, state)
}

func workerStateForPID(pid int, generation Generation) (WorkerState, error) {
	token := ProcStartToken(pid)
	if !StableProcToken(token) {
		return WorkerState{}, fmt.Errorf("detached worker pid %d has no stable process identity", pid)
	}
	return WorkerState{Pid: pid, Token: token, Generation: generation}, nil
}

// ConfirmWorker is a detached worker's first act. Its parent holds the lifecycle lock from before the
// child starts until the child's record is written, so once this process holds the lock the record
// is final. It must name this exact process, pid and start token. A parent that died before writing
// it, or a stop that removed it first, leaves nothing to confirm: the child exits untouched.
func ConfirmWorker(repo, name string) (WorkerState, error) {
	unlock, err := LockState(repo, name)
	if err != nil {
		return WorkerState{}, err
	}
	defer unlock()
	recorded, err := ReadWorkerState(repo, name)
	if errors.Is(err, os.ErrNotExist) {
		return WorkerState{}, errors.New("no lifecycle record names this process")
	}
	if err != nil {
		return WorkerState{}, err
	}
	mine, err := workerStateForPID(os.Getpid(), recorded.Generation)
	if err != nil {
		return WorkerState{}, err
	}
	if recorded != mine {
		return WorkerState{}, errors.New("the lifecycle record names another process")
	}
	return recorded, nil
}

// ClearPidIfMine removes the fork's record only while it still names THIS process for generation,
// so an exiting worker never deletes a replacement that reused its numeric pid. It gives up rather
// than wait when stop holds the lock: stop is waiting for this process to exit and removes the
// record itself once the box is gone.
func ClearPidIfMine(repo, name string, generation Generation) {
	unlock, ok := TryLockState(repo, name)
	if !ok {
		return
	}
	defer unlock()
	data, err := readWorkerRecord(PidPath(repo, name))
	if err != nil {
		return
	}
	mine, err := workerStateForPID(os.Getpid(), generation)
	if err != nil {
		return
	}
	if expected, err := mine.Marshal(); err == nil && bytes.Equal(data, expected) {
		_ = os.Remove(PidPath(repo, name))
	}
}

// ReadProcStartToken returns an opaque identity for pid that's fixed for the process's lifetime —
// its numeric kernel start time. A pid reused by a later process reports a different token. Empty
// means the caller cannot authorize a signal and must retain cleanup state. It is a var so a test
// can make identity unreadable.
var ReadProcStartToken = processidentity.StartToken

func ProcStartToken(pid int) string { return ReadProcStartToken(pid) }

func StableProcToken(token string) bool {
	return processidentity.Stable(token)
}
