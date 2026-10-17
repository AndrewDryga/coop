package forkspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/safefile"
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
	dir, err := openStateDir(repo)
	if err != nil {
		return err
	}
	return dir.Close()
}

// openStateDir is EnsureStateDir returning .coop open. A box whose mount holds the forks folder can
// swap .coop for a link after any check by name, and one whose mount holds the folder above it can
// swap the forks folder itself. So neither is opened through a link, .coop's mode is set through its
// descriptor, and the lock, log and worker record are opened, renamed and removed relative to the
// returned handle, never by path.
func openStateDir(repo string) (*os.File, error) {
	if err := ensureForkHome(repo); err != nil {
		return nil, err
	}
	home, err := safefile.OpenRoot(Home(repo))
	if err != nil {
		return nil, err
	}
	defer home.Close()
	path := StateDir(repo)
	if err := unix.Mkdirat(int(home.Fd()), ".coop", 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, &os.PathError{Op: "mkdir", Path: path, Err: err}
	}
	fd, err := unix.Openat(int(home.Fd()), ".coop", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("fork state path %q is not a real directory", path)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	dir := os.NewFile(uintptr(fd), path)
	// Repeat this barrier even when the directory was already visible. A previous creation may
	// have returned an fsync error after mkdir; the idempotent retry is what confirms publication.
	if err := syncForkDirectoryEntry(home); err != nil {
		_ = dir.Close()
		return nil, err
	}
	if err := dir.Chmod(0o700); err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("make fork state path %q owner-only: %w", path, err)
	}
	return dir, nil
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
	return confirmForkHomeEntry(home)
}

// confirmForkDirectoryEntry syncs the folder that holds path, in the forks tree.
func confirmForkDirectoryEntry(path string) error {
	return syncParentFolder(path, false)
}

// confirmForkHomeEntry syncs the fork home's parent: the project's own parent folder, which a box
// cannot write and a link may reach.
func confirmForkHomeEntry(home string) error {
	return syncParentFolder(home, true)
}

func syncParentFolder(path string, follow bool) error {
	parent, err := OpenFolder(filepath.Dir(path), follow)
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
	f, err := createStateFile(repo, name+".lock", os.O_RDWR)
	if err != nil {
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
	f, err := createStateFile(repo, name+".lock", os.O_RDWR)
	if err != nil {
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

// ProcessIdentity is what the kernel can prove about the process a fork's state names:
// processidentity's answer, under the names the fork lifecycle uses.
type ProcessIdentity = processidentity.State

const (
	ProcessGone             = processidentity.Gone
	ProcessIdentityMatch    = processidentity.Match
	ProcessIdentityMismatch = processidentity.Mismatch
	ProcessIdentityUnknown  = processidentity.Unknown
	ProcessIdentityForeign  = processidentity.Foreign // another operating system wrote the token
)

// ProcessIdentityOf is processidentity.Inspect through this package's SignalPID and
// ReadProcStartToken, so a test that stubs them steers every lifecycle probe. Destructive guards
// retain unknown and foreign state, but status calls only a corroborated identity running.
func ProcessIdentityOf(pid int, token string) ProcessIdentity {
	return processidentity.Kernel{Signal: SignalPID, StartToken: ReadProcStartToken}.Inspect(pid, token)
}

// processAlive reports the one identity that authorizes treating a fork as running.
func processAlive(pid int, token string) bool {
	return ProcessIdentityOf(pid, token) == ProcessIdentityMatch
}

// OwnerProvablyDead is the ONE test the fork lifecycle shares for "the process this record names is
// not running any more": the kernel says its pid is gone, or that pid now belongs to a different
// process than the one recorded. A live match and — deliberately — an identity coop could not read
// or one another operating system recorded are all unproven, so every caller (stop, the merge's
// rebase recovery) fails closed on them. Identity is pid + start token or nothing: no file age, no
// elapsed time.
func OwnerProvablyDead(identity ProcessIdentity) bool { return identity.ProvesGone() }

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

// WorkerElsewhere names the operating system that started the fork's background loop when another
// one wrote its record, as a coop in a Linux container sharing this folder with macOS does, or "".
// Such a record is neither running nor left for cleanup here: fork stop refuses it, so the fork
// listing and the task board both name that system instead.
func WorkerElsewhere(repo, name string) string {
	state, err := ReadWorkerState(repo, name)
	if err != nil || ProcessIdentityOf(state.Pid, state.Token) != ProcessIdentityForeign {
		return ""
	}
	return ProcTokenSystem(state.Token)
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
	if err := state.validate(); err != nil {
		return WorkerState{}, err
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
	if err := state.validate(); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("%s%d\n%s\n", prefix, state.Pid, state.Token)), nil
}

// validate refuses a record that names no process identity: a pid of 1 or below, or a start token
// of no known shape. Without the token no kernel can say which process, or even which system, the
// pid was, so stop could never prove that worker gone.
func (state WorkerState) validate() error {
	if state.Pid <= 1 {
		return fmt.Errorf("invalid detached worker pid %d", state.Pid)
	}
	if !StableProcToken(state.Token) {
		return fmt.Errorf("detached worker pid %d has no stable process identity", state.Pid)
	}
	return nil
}

// ReadWorkerState reads and parses a fork's lifecycle record. A failure to read it is an
// *os.PathError, os.ErrNotExist when there is no record. Any other error means a record is there
// that Coop cannot own: ErrNotAStateFile, or bytes ParseWorkerState refuses.
func ReadWorkerState(repo, name string) (WorkerState, error) {
	path := PidPath(repo, name)
	data, err := readWorkerRecord(unix.AT_FDCWD, path, path)
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
	return openStateFile(unix.AT_FDCWD, path, path, os.O_RDONLY)
}

// openStateFile is OpenStateFile for name relative to the folder dirfd (unix.AT_FDCWD for a path),
// with path naming it in errors and flag adding the access mode and how to open it. A file opened
// with O_CREATE, whether it was just made or was already there, is set to 0600 through its
// descriptor, never by path.
func openStateFile(dirfd int, name, path string, flag int) (*os.File, error) {
	fd, err := unix.Openat(dirfd, name, flag|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		// O_NOFOLLOW refuses a link with ELOOP, a folder opened to write fails with EISDIR, and
		// open(2) refuses a socket with ENXIO on Linux and EOPNOTSUPP on macOS, as it does a named
		// pipe opened to write that nobody reads with ENXIO. Lstat names what is there; if that
		// changed meanwhile, the open's error stands.
		var st unix.Stat_t
		if (errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EISDIR) || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.EOPNOTSUPP)) &&
			unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, &notAStateFile{path, fileKind(uint32(st.Mode))}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, &notAStateFile{path, fileKind(uint32(st.Mode))}
	}
	f := os.NewFile(uintptr(fd), path)
	if flag&os.O_CREATE != 0 {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// createStateFile opens base in repo's .coop folder through openStateDir's handle, creating it if it
// is missing. It is an owner-only regular file, or refused as OpenStateFile refuses a reader's.
func createStateFile(repo, base string, flag int) (*os.File, error) {
	dir, err := openStateDir(repo)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return openStateFile(int(dir.Fd()), base, filepath.Join(dir.Name(), base), flag|os.O_CREATE)
}

// OpenLog opens a fork's log for a loop's output, emptied. Writes append: a child whose start died
// before its record still holds an earlier open, and its refusal must land after a newer start's
// output, not over it.
func OpenLog(repo, name string) (*os.File, error) {
	return createStateFile(repo, name+".log", os.O_WRONLY|os.O_TRUNC|os.O_APPEND)
}

// notAStateFile says what Coop found at a fork state file's name. It is deliberately not an
// *os.PathError: callers tell a failed read from a file Coop cannot own by that type.
type notAStateFile struct{ path, found string }

func (e *notAStateFile) Error() string        { return e.path + " is " + e.found }
func (e *notAStateFile) Is(target error) bool { return target == ErrNotAStateFile }

// fileKind names, in plain words, the type of a file that is not a regular file from its stat mode.
func fileKind(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFLNK:
		return "a link"
	case unix.S_IFIFO:
		return "a named pipe"
	case unix.S_IFDIR:
		return "a folder"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFCHR, unix.S_IFBLK:
		return "a device"
	default:
		return "not a regular file"
	}
}

// readWorkerRecord returns the bytes of the lifecycle record name, relative to dirfd as
// openStateFile opens it, and never more than workerStateLimit of them.
func readWorkerRecord(dirfd int, name, path string) ([]byte, error) {
	f, err := openStateFile(dirfd, name, path, os.O_RDONLY)
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
// record or the new one, never a truncated identity. The new record is written, renamed into place
// and, on failure, removed through openStateDir's handle.
func WriteWorkerState(repo, name string, state WorkerState) error {
	data, err := state.Marshal()
	if err != nil {
		return err
	}
	dir, err := openStateDir(repo)
	if err != nil {
		return err
	}
	defer dir.Close()
	tmp := "." + name + ".pid-" + rand.Text()
	f, err := openStateFile(int(dir.Fd()), tmp, filepath.Join(dir.Name(), tmp), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(dir.Fd()), tmp, 0) // after the rename there is nothing left to remove
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
	if err := unix.Renameat(int(dir.Fd()), tmp, int(dir.Fd()), name+".pid"); err != nil {
		return &os.LinkError{Op: "rename", Old: f.Name(), New: PidPath(repo, name), Err: err}
	}
	return nil
}

// RemoveWorkerState deletes a fork's lifecycle record through openStateDir's handle, so a box that
// swaps .coop for a link cannot make it delete a file elsewhere. As with os.Remove, a missing record
// is an *os.PathError that matches os.ErrNotExist. The caller holds the lifecycle lock.
func RemoveWorkerState(repo, name string) error {
	dir, err := openStateDir(repo)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := unix.Unlinkat(int(dir.Fd()), name+".pid", 0); err != nil {
		return &os.PathError{Op: "remove", Path: PidPath(repo, name), Err: err}
	}
	return nil
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
// record itself once the box is gone. The record it compares is the one it removes: both go
// through one openStateDir handle.
func ClearPidIfMine(repo, name string, generation Generation) {
	unlock, ok := TryLockState(repo, name)
	if !ok {
		return
	}
	defer unlock()
	dir, err := openStateDir(repo)
	if err != nil {
		return
	}
	defer dir.Close()
	data, err := readWorkerRecord(int(dir.Fd()), name+".pid", PidPath(repo, name))
	if err != nil {
		return
	}
	mine, err := workerStateForPID(os.Getpid(), generation)
	if err != nil {
		return
	}
	if expected, err := mine.Marshal(); err == nil && bytes.Equal(data, expected) {
		_ = unix.Unlinkat(int(dir.Fd()), name+".pid", 0)
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

// ProcTokenSystem names where token's process runs ("Linux", "macOS", or another container or host
// for this system's token from another PID namespace), so a refusal of a foreign record can say
// where its worker can be checked.
func ProcTokenSystem(token string) string { return processidentity.Origin(token) }
