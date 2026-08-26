package forkspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/processidentity"
)

// The current wire header and tagged state prefixes stay line-oriented, so the file remains
// readable with `sed -n 1,4p` when an operator has to recover one by hand. A plain worker body has
// no state prefix after the header.
const (
	ReapPending   = "reap-pending\n"
	OwnerStateV1  = "owner-v1\n"
	StartClaim    = "start-claim\n"    // a start reservation: its owner has launched no worker yet
	StartLaunched = "start-launched\n" // that reservation forked a worker whose identity isn't recorded
)

var (
	// ErrPreV8WorkerState identifies a headerless state file without decoding it into an identity.
	// V9 cannot prove which repository owns any container that old state may have launched.
	ErrPreV8WorkerState = errors.New("unsupported pre-v8 detached-worker state")
	// ErrUnsupportedWorkerStateVersion keeps a future writer's state fail-closed until that version
	// can interpret its own lifecycle contract.
	ErrUnsupportedWorkerStateVersion = errors.New("unsupported detached-worker state version")
)

// SignalPID is the kill(2) the lifecycle probes and the supervisor share, as one seam so a test can
// stub liveness once for both.
var SignalPID = syscall.Kill

// Per-fork process state (logs + pidfiles) lives in <repo>-forks/.coop/.
func StateDir(repo string) string       { return filepath.Join(Home(repo), ".coop") }
func LogPath(repo, name string) string  { return filepath.Join(StateDir(repo), name+".log") }
func PidPath(repo, name string) string  { return filepath.Join(StateDir(repo), name+".pid") }
func LockPath(repo, name string) string { return filepath.Join(StateDir(repo), name+".lock") }

// LockState serializes start, worker cleanup, and stop for one fork. The lock file persists,
// but flock ownership does not: the kernel releases it if a coop process crashes.
func LockState(repo, name string) (func(), error) {
	return LockStateContext(context.Background(), repo, name)
}

// LockStateContext acquires a lifecycle lock without making service shutdown
// wait forever behind another process. The ordinary command path passes a
// background context and retains the historical blocking behavior.
func LockStateContext(ctx context.Context, repo, name string) (func(), error) {
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(LockPath(repo, name), os.O_CREATE|os.O_RDWR, 0o644)
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
	if err := os.MkdirAll(StateDir(repo), 0o755); err != nil {
		return nil, false
	}
	f, err := os.OpenFile(LockPath(repo, name), os.O_CREATE|os.O_RDWR, 0o644)
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

// OwnerProvablyDead is the ONE test the fork lifecycle shares for "the process this state names is
// not running any more": the kernel says its pid is gone, or that pid now belongs to a different
// process than the one recorded. A live match and — deliberately — an identity coop could not read
// are both unproven, so every caller (stop, the start reclaim, the merge's rebase recovery) fails
// closed on them. Identity is pid + start token or nothing: no file age, no elapsed time.
func OwnerProvablyDead(identity ProcessIdentity) bool {
	return identity == ProcessGone || identity == ProcessIdentityMismatch
}

// StateOwner reports the process a fork's lifecycle state still names as possibly running, so a
// recovery that would disturb the fork can keep its hands off one somebody else owns. held is false
// ONLY when there is no state at all, or its recorded owner is provably dead; everything coop cannot
// read or disprove — an unreadable or malformed file, an ownerless cleanup tombstone, an
// unverifiable identity, a worker launched but never recorded — holds the fork. pid is the recorded
// owner when the state names one (0 when it names none).
func StateOwner(repo, name string) (pid int, held bool) {
	state, err := ReadWorkerState(repo, name)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false
	}
	if err != nil {
		return 0, true
	}
	if state.Pid <= 1 {
		return 0, true // a cleanup tombstone names no identity, so there is none to disprove
	}
	if state.Launched {
		return state.Pid, true // its worker exists but was never recorded: nothing here can rule it out
	}
	return state.Pid, !OwnerProvablyDead(ProcessIdentityOf(state.Pid, state.Token))
}

// RunningPid returns the live pid of a detached loop for name, or 0. It deliberately preserves
// dead/reused state: a crashed worker may have orphaned its box, and only a successful forkStop may
// discard the exact-label reap handle.
func RunningPid(repo, name string) int {
	state, err := ReadWorkerState(repo, name)
	if err != nil || state.Pid <= 0 {
		return 0
	}
	if state.Claim {
		return 0 // a reservation names the coop process starting the fork, never a running loop
	}
	if !processAlive(state.Pid, state.Token) {
		return 0
	}
	return state.Pid
}

// NeedsStop is the destructive-operation guard: besides a live worker, any remaining state file
// is dead/reused, reap-pending, or malformed and must be resolved by `fork stop` before the worktree
// can be merged, replaced, pruned, or removed.
func NeedsStop(repo, name string) bool {
	if RunningPid(repo, name) != 0 {
		return true
	}
	return pathExists(PidPath(repo, name))
}

// parsePidfile reads the current state's "<pid>\n<start-token>" body. pid 0 means unparseable.
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

// WorkerState is the one parse/validate/marshal boundary for durable loop lifecycle state.
// Pending with Pid=0 is the bare dead-worker cleanup tombstone; every retained identity has Pid>1.
// A claim is the start reservation, and its identity is the coop process that MADE it, not a worker:
// nothing may ever signal it, and only its owner's death makes it reclaimable.
type WorkerState struct {
	Pid      int
	Token    string
	Pending  bool
	Claim    bool // a start reservation held by the coop process at Pid
	Launched bool // that reservation already forked a worker whose identity it never recorded
}

func ParseWorkerState(raw string) (WorkerState, error) {
	first, _, _ := strings.Cut(raw, "\n")
	if !strings.HasPrefix(raw, OwnerStateV1) {
		if first == strings.TrimSpace(ReapPending) {
			return WorkerState{}, fmt.Errorf("%w: headerless %s record", ErrPreV8WorkerState, first)
		}
		if _, err := strconv.Atoi(strings.TrimSpace(first)); err == nil {
			return WorkerState{}, fmt.Errorf("%w: headerless numeric pid record", ErrPreV8WorkerState)
		}
		if strings.HasPrefix(first, "owner-") && first != strings.TrimSpace(OwnerStateV1) {
			return WorkerState{}, fmt.Errorf("%w %q", ErrUnsupportedWorkerStateVersion, first)
		}
		return WorkerState{}, errors.New("detached worker state is missing the owner-v1 header")
	}
	state := WorkerState{}
	body := strings.TrimPrefix(raw, OwnerStateV1)
	switch {
	case strings.HasPrefix(body, StartClaim):
		state.Claim = true
		body = strings.TrimPrefix(body, StartClaim)
	case strings.HasPrefix(body, StartLaunched):
		state.Claim, state.Launched = true, true
		body = strings.TrimPrefix(body, StartLaunched)
	case strings.HasPrefix(body, ReapPending):
		state.Pending = true
		body = strings.TrimPrefix(body, ReapPending)
		if strings.TrimSpace(body) == "" {
			return state, nil
		}
	}
	state.Pid, state.Token = parsePidfile(body)
	if state.Pid <= 1 {
		return WorkerState{}, fmt.Errorf("invalid detached worker pid %d", state.Pid)
	}
	return state, nil
}

func (state WorkerState) Marshal() ([]byte, error) {
	prefix := OwnerStateV1
	if state.Claim && state.Pending {
		return nil, errors.New("invalid fork state: a start reservation is never cleanup-pending")
	}
	if state.Pending && state.Pid == 0 {
		return []byte(prefix + ReapPending), nil
	}
	if state.Pid <= 1 {
		return nil, fmt.Errorf("invalid detached worker pid %d", state.Pid)
	}
	switch {
	case state.Launched:
		prefix += StartLaunched
	case state.Claim:
		prefix += StartClaim
	case state.Pending:
		prefix += ReapPending
	}
	return []byte(fmt.Sprintf("%s%d\n%s\n", prefix, state.Pid, state.Token)), nil
}

func ReadWorkerState(repo, name string) (WorkerState, error) {
	data, err := os.ReadFile(PidPath(repo, name))
	if err != nil {
		return WorkerState{}, err
	}
	return ParseWorkerState(string(data))
}

// ReplaceState atomically replaces a pid/cleanup record, so an interrupted stop sees either the
// old complete worker identity or the new complete marker — never a truncated state that loses the
// process it still needs to signal. It is a var so a test can make the write fail.
var ReplaceState = writeStateAtomic

func writeState(repo, name string, data []byte) error {
	return ReplaceState(repo, name, data)
}

func WriteWorkerState(repo, name string, state WorkerState) error {
	data, err := state.Marshal()
	if err != nil {
		return err
	}
	return writeState(repo, name, data)
}

func writeStateAtomic(repo, name string, data []byte) error {
	f, err := os.CreateTemp(StateDir(repo), "."+name+".pid-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o644); err != nil {
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

// WritePid records the worker's pid plus a start-time token, so RunningPid can later tell a
// live worker from an unrelated process that reused the pid after a crash.
func WritePid(repo, name string, pid int) error {
	unlock, err := LockState(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	return WritePidUnlocked(repo, name, pid)
}

func WritePidUnlocked(repo, name string, pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refuse invalid detached worker pid %d", pid)
	}
	token := ProcStartToken(pid)
	if !StableProcToken(token) {
		return fmt.Errorf("detached worker pid %d has no stable process identity", pid)
	}
	return WriteWorkerState(repo, name, WorkerState{Pid: pid, Token: token})
}

// ClaimState is the reservation a detaching coop writes over the fork's pidfile: its OWN
// identity, so a later start can tell a claim a live coop is still working through from one whose
// owner died holding it. launched marks the instant a worker has been forked but not yet recorded —
// the one window where a dead owner does NOT prove that nothing is running.
func ClaimState(launched bool) WorkerState {
	pid := os.Getpid()
	return WorkerState{Claim: true, Launched: launched, Pid: pid, Token: ProcStartToken(pid)}
}

// ClearPidIfMine removes the fork's pidfile only if it still names THIS process, so an exiting
// worker (or a failed parent claim) never deletes a pidfile a different live worker owns.
func ClearPidIfMine(repo, name string) {
	unlock, ok := TryLockState(repo, name)
	if !ok {
		return
	}
	defer unlock()
	clearPidIfMineUnlocked(repo, name)
}

func clearPidIfMineUnlocked(repo, name string) {
	data, err := os.ReadFile(PidPath(repo, name))
	if err != nil {
		return
	}
	state, err := ParseWorkerState(string(data))
	if err == nil && !state.Pending && !state.Claim && state.Pid == os.Getpid() {
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
