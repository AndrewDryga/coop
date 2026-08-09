package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/processidentity"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

const (
	forkStopReapTimeout = 3 * time.Second
	forkReapPending     = "reap-pending\n"
	forkOwnerStateV1    = "owner-v1\n"
	forkStartClaim      = "start-claim\n"    // a start reservation: its owner has launched no worker yet
	forkStartLaunched   = "start-launched\n" // that reservation forked a worker whose identity isn't recorded
)

var signalPID = syscall.Kill

// agentLoopCmd builds the headless, autonomous command for one loop iteration of the
// given agent, carrying prompt (each agent's non-interactive form lives in its adapter).
func (a *app) agentLoopCmd(agent, prompt string) []string {
	if ag, ok := agents.Get(agent); ok {
		return ag.Headless(a.cfg, prompt)
	}
	return append([]string{agent}, prompt)
}

// Per-fork process state (logs + pidfiles) lives in <repo>-forks/.coop/.
func forkStateDir(repo string) string   { return filepath.Join(forkHome(repo), ".coop") }
func forkLog(repo, name string) string  { return filepath.Join(forkStateDir(repo), name+".log") }
func forkPid(repo, name string) string  { return filepath.Join(forkStateDir(repo), name+".pid") }
func forkLock(repo, name string) string { return filepath.Join(forkStateDir(repo), name+".lock") }

// forkContainerOwner scopes the runtime cleanup label to one parent repo and fork name. Fork state
// already lives under this path-derived home, so a path-derived owner has the same move semantics.
func forkContainerOwner(repo, name string) string {
	canonical := repo
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		canonical = resolved
	} else if absolute, absErr := filepath.Abs(repo); absErr == nil {
		canonical = absolute
	}
	sum := sha256.Sum256([]byte(canonical + "\x00" + name))
	return fmt.Sprintf("v1-%x", sum[:12])
}

// lockForkState serializes start, worker cleanup, and stop for one fork. The lock file persists,
// but flock ownership does not: the kernel releases it if a coop process crashes.
func lockForkState(repo, name string) (func(), error) {
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(forkLock(repo, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// lockSessionProducer excludes every Coop-owned interactive producer from one native history
// scope while a fork attributes a new ID. ConfigDir/.locks is host-only and shared across repos.
// Contention fails fast because an interactive session can remain open for hours.
// lockLoopCheckout makes `coop loop` exclusive PER CHECKOUT. Two loops sharing one working tree
// each commit their own task, and each one's completion range then contains the other's
// task-bound commit — so unbindableTasks rejects BOTH and reopens finished work. Measured: two
// commits five minutes apart, bound to different tasks, both completions rejected. The per-task
// lease in completionwindow.go cannot catch this; it stops two loops taking the SAME task, not
// two loops working DIFFERENT tasks in the same tree.
//
// The key is the resolved checkout path, never the repo name: a fleet runs one loop per fork
// WORKTREE (fork_loop.go hands a.loop each fork's own ws), so concurrent forks hold different
// locks and stay fully parallel. Isolate the state, don't serialize the fleet.
func lockLoopCheckout(cfg *config.Config, repo string) (func(), error) {
	// Production always carries a ConfigDir; an in-process test app may not, and joining "" would
	// drop a .locks/ directory into whatever the working directory happens to be.
	base := cfg.ConfigDir
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, ".locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	checkout := repo
	if resolved, err := filepath.EvalSymlinks(checkout); err == nil {
		checkout = resolved
	} else if absolute, absErr := filepath.Abs(checkout); absErr == nil {
		checkout = absolute
	}
	sum := sha256.Sum256([]byte(checkout))
	path := filepath.Join(dir, fmt.Sprintf("loop-%x.lock", sum[:12]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Best effort only: flock is the authority, the file body just names the holder so the
		// operator doesn't have to walk a process tree to find it (that hunt is why it's here).
		owner, _ := os.ReadFile(path)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			held := ""
			if who := strings.TrimSpace(string(owner)); who != "" {
				held = " (" + who + ")"
			}
			return nil, fmt.Errorf("another coop loop is already working %s%s — two loops in one checkout commit over each other and both completions get rejected; wait for it to finish, or give this one its own worktree with 'coop fork add'", checkout, held)
		}
		return nil, err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(fmt.Sprintf("pid %d\n", os.Getpid())), 0)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func lockSessionProducer(cfg *config.Config, provider, cwd string) (func(), error) {
	dir := filepath.Join(cfg.ConfigDir, ".locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	profile := cfg.AgentDir(provider)
	if resolved, err := filepath.EvalSymlinks(profile); err == nil {
		profile = resolved
	} else if absolute, absErr := filepath.Abs(profile); absErr == nil {
		profile = absolute
	}
	sum := sha256.Sum256([]byte(profile + "\x00" + cwd))
	path := filepath.Join(dir, fmt.Sprintf("session-%x.lock", sum[:12]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("another interactive %s session is active for account %q in workdir %q", provider, cfg.ActiveProfile(provider), cwd)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// tryLockForkState is used by worker-exit cleanup: if stop already owns the lifecycle lock, the
// worker must be allowed to exit rather than wait behind the command that's waiting for its exit.
func tryLockForkState(repo, name string) (func(), bool) {
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		return nil, false
	}
	f, err := os.OpenFile(forkLock(repo, name), os.O_CREATE|os.O_RDWR, 0o644)
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

type processIdentity uint8

const (
	processGone processIdentity = iota
	processIdentityMatch
	processIdentityMismatch
	processIdentityUnknown
)

// forkProcessIdentity separates conservative liveness from authorization to signal. Destructive
// guards retain unknown state, but status calls only a corroborated identity running.
func forkProcessIdentity(pid int, token string) processIdentity {
	if pid <= 1 { // a detached worker cannot be init; -1 is kill(2)'s broadcast target
		return processGone
	}
	if err := signalPID(pid, 0); errors.Is(err, syscall.ESRCH) {
		return processGone
	} else if err != nil {
		return processIdentityUnknown
	}
	if token == "" {
		return processIdentityUnknown
	}
	if !stableProcToken(token) {
		return processIdentityUnknown
	}
	cur := procStartToken(pid)
	if cur == "" {
		// The process may have exited between kill(0) and the identity read. Recheck so that ordinary
		// exit is not misreported as an unverifiable live PID, while PID reuse still fails closed.
		if err := signalPID(pid, 0); errors.Is(err, syscall.ESRCH) {
			return processGone
		}
		return processIdentityUnknown
	}
	if cur != token {
		return processIdentityMismatch
	}
	return processIdentityMatch
}

func forkProcessAlive(pid int, token string) bool {
	return forkProcessIdentity(pid, token) == processIdentityMatch
}

// ownerProvablyDead is the ONE test the fork lifecycle shares for "the process this state names is
// not running any more": the kernel says its pid is gone, or that pid now belongs to a different
// process than the one recorded. A live match and — deliberately — an identity coop could not read
// are both unproven, so every caller (stop, the start reclaim, the merge's rebase recovery) fails
// closed on them. Identity is pid + start token or nothing: no file age, no elapsed time.
func ownerProvablyDead(identity processIdentity) bool {
	return identity == processGone || identity == processIdentityMismatch
}

// forkStateOwner reports the process a fork's lifecycle state still names as possibly running, so a
// recovery that would disturb the fork can keep its hands off one somebody else owns. held is false
// ONLY when there is no state at all, or its recorded owner is provably dead; everything coop cannot
// read or disprove — an unreadable or malformed file, an ownerless cleanup tombstone, an
// unverifiable identity, a worker launched but never recorded — holds the fork. pid is the recorded
// owner when the state names one (0 when it names none).
func forkStateOwner(repo, name string) (pid int, held bool) {
	state, err := readForkWorkerState(repo, name)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false
	}
	if err != nil {
		return 0, true
	}
	if state.pid <= 1 {
		return 0, true // a cleanup tombstone names no identity, so there is none to disprove
	}
	if state.launched {
		return state.pid, true // its worker exists but was never recorded: nothing here can rule it out
	}
	return state.pid, !ownerProvablyDead(forkProcessIdentity(state.pid, state.token))
}

// forkRunningPid returns the live pid of a detached loop for name, or 0. It deliberately preserves
// dead/reused state: a crashed worker may have orphaned its box, and only a successful forkStop may
// discard the exact-label reap handle.
func forkRunningPid(repo, name string) int {
	state, err := readForkWorkerState(repo, name)
	if err != nil || state.pid <= 0 {
		return 0
	}
	if state.claim {
		return 0 // a reservation names the coop process starting the fork, never a running loop
	}
	if !forkProcessAlive(state.pid, state.token) {
		return 0
	}
	return state.pid
}

// forkNeedsStop is the destructive-operation guard: besides a live worker, any remaining state file
// is dead/reused, reap-pending, or malformed and must be resolved by `fork stop` before the worktree
// can be merged, replaced, pruned, or removed.
func forkNeedsStop(repo, name string) bool {
	if forkRunningPid(repo, name) != 0 {
		return true
	}
	return pathExists(forkPid(repo, name))
}

// parsePidfile reads a fork pidfile's "<pid>\n<start-token>" form. The token is optional, so an
// older pid-only file still parses for fail-closed cleanup. pid 0 means unparseable.
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

// forkWorkerState is the one parse/validate/marshal boundary for durable loop lifecycle state.
// pending with pid=0 is the bare dead-worker cleanup tombstone; every retained identity has pid>1.
// A claim is the start reservation, and its identity is the coop process that MADE it, not a worker:
// nothing may ever signal it, and only its owner's death makes it reclaimable.
type forkWorkerState struct {
	pid      int
	token    string
	pending  bool
	claim    bool // a start reservation held by the coop process at pid
	launched bool // that reservation already forked a worker whose identity it never recorded
	legacy   bool
}

func parseForkWorkerState(raw string) (forkWorkerState, error) {
	state := forkWorkerState{}
	body := raw
	if strings.HasPrefix(body, forkOwnerStateV1) {
		body = strings.TrimPrefix(body, forkOwnerStateV1)
	} else {
		state.legacy = true
	}
	switch {
	case strings.HasPrefix(body, forkStartClaim):
		state.claim = true
		body = strings.TrimPrefix(body, forkStartClaim)
	case strings.HasPrefix(body, forkStartLaunched):
		state.claim, state.launched = true, true
		body = strings.TrimPrefix(body, forkStartLaunched)
	case strings.HasPrefix(body, forkReapPending):
		state.pending = true
		body = strings.TrimPrefix(body, forkReapPending)
		if strings.TrimSpace(body) == "" {
			return state, nil
		}
	}
	state.pid, state.token = parsePidfile(body)
	if state.pid <= 1 {
		return forkWorkerState{}, fmt.Errorf("invalid detached worker pid %d", state.pid)
	}
	return state, nil
}

func (state forkWorkerState) marshal() ([]byte, error) {
	prefix := ""
	if !state.legacy {
		prefix = forkOwnerStateV1
	}
	if state.claim && state.pending {
		return nil, errors.New("invalid fork state: a start reservation is never cleanup-pending")
	}
	if state.pending && state.pid == 0 {
		return []byte(prefix + forkReapPending), nil
	}
	if state.pid <= 1 {
		return nil, fmt.Errorf("invalid detached worker pid %d", state.pid)
	}
	switch {
	case state.launched:
		prefix += forkStartLaunched
	case state.claim:
		prefix += forkStartClaim
	case state.pending:
		prefix += forkReapPending
	}
	return []byte(fmt.Sprintf("%s%d\n%s\n", prefix, state.pid, state.token)), nil
}

func readForkWorkerState(repo, name string) (forkWorkerState, error) {
	data, err := os.ReadFile(forkPid(repo, name))
	if err != nil {
		return forkWorkerState{}, err
	}
	return parseForkWorkerState(string(data))
}

// writeForkState atomically replaces a pid/cleanup record, so an interrupted stop sees either the
// old complete worker identity or the new complete marker — never a truncated state that loses the
// process it still needs to signal.
var replaceForkState = writeForkStateAtomic

func writeForkState(repo, name string, data []byte) error {
	return replaceForkState(repo, name, data)
}

func writeForkWorkerState(repo, name string, state forkWorkerState) error {
	data, err := state.marshal()
	if err != nil {
		return err
	}
	return writeForkState(repo, name, data)
}

func writeForkStateAtomic(repo, name string, data []byte) error {
	f, err := os.CreateTemp(forkStateDir(repo), "."+name+".pid-")
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
	return os.Rename(tmp, forkPid(repo, name))
}

// writeForkPid records the worker's pid plus a start-time token, so forkRunningPid can later tell a
// live worker from an unrelated process that reused the pid after a crash.
func writeForkPid(repo, name string, pid int) error {
	unlock, err := lockForkState(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	return writeForkPidUnlocked(repo, name, pid)
}

func writeForkPidUnlocked(repo, name string, pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refuse invalid detached worker pid %d", pid)
	}
	token := procStartToken(pid)
	if !stableProcToken(token) {
		return fmt.Errorf("detached worker pid %d has no stable process identity", pid)
	}
	return writeForkWorkerState(repo, name, forkWorkerState{pid: pid, token: token})
}

// claimForkPid atomically reserves a fork's pidfile BEFORE its worker starts, so two concurrent
// detach attempts (a hand-run `fork -d` racing `fleet up`, or two of either) can't both pass a
// check-then-write and leave two loops racing one worktree/branch. O_EXCL fails if the file exists;
// a live loop is refused, while dead/reused/pending state requires forkStop to reap labels before a
// new start. On success the file holds this process's own reservation until the worker replaces it.
func claimForkPid(repo, name string) error {
	unlock, err := lockForkState(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	return claimForkPidUnlocked(repo, name)
}

func claimForkPidUnlocked(repo, name string) error {
	path := forkPid(repo, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		// A reservation owns no signalable worker yet. If this process crashes here, stop can safely
		// reap the scoped runtime label and clear the reservation without guessing at a pid.
		data, marshalErr := forkClaimState(false).marshal()
		if marshalErr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return marshalErr
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return err
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return err
		}
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	if pid := forkRunningPid(repo, name); pid != 0 {
		return fmt.Errorf("fork %s already has a loop running (pid %d) — stop it first: coop fork stop %s", name, pid, name)
	}
	// A reservation names the coop process that made it, so a start crashed between the claim and its
	// worker is recoverable: the owner is provably gone AND it never forked a worker, which together
	// prove nothing is running and no box was ever started. Reclaim that; refuse everything else,
	// because a live owner is mid-start and an unverifiable one could be either.
	if state, err := readForkWorkerState(repo, name); err == nil && state.claim && !state.legacy {
		switch identity := forkProcessIdentity(state.pid, state.token); {
		case identity == processIdentityMatch:
			return fmt.Errorf("fork %s is already being started by coop pid %d — wait for it, or stop it first: coop fork stop %s", name, state.pid, name)
		case !ownerProvablyDead(identity):
			return fmt.Errorf("fork %s holds a start reservation from pid %d whose identity coop cannot verify — finish it with: coop fork stop %s", name, state.pid, name)
		case state.launched:
			return fmt.Errorf("fork %s: a start by coop pid %d was interrupted after launching its worker, which may still be looping unrecorded — finish it with: coop fork stop %s", name, state.pid, name)
		}
		if err := writeForkWorkerState(repo, name, forkClaimState(false)); err != nil {
			return fmt.Errorf("fork %s: reclaim the start reservation abandoned by coop pid %d: %w — check permissions on %s, then retry the original coop fork command", name, state.pid, err, forkStateDir(repo))
		}
		ui.Warn("fork %s: reclaimed a start reservation from coop pid %d, which is no longer running and never launched a worker", name, state.pid)
		return nil
	}
	return fmt.Errorf("fork %s is stopped or stopping but still needs box cleanup — finish it with: coop fork stop %s", name, name)
}

// forkClaimState is the reservation a detaching coop writes over the fork's pidfile: its OWN
// identity, so a later start can tell a claim a live coop is still working through from one whose
// owner died holding it. launched marks the instant a worker has been forked but not yet recorded —
// the one window where a dead owner does NOT prove that nothing is running.
func forkClaimState(launched bool) forkWorkerState {
	pid := os.Getpid()
	return forkWorkerState{claim: true, launched: launched, pid: pid, token: procStartToken(pid)}
}

// clearForkClaimUnlocked releases only the reservation written by this detach attempt: it verifies
// the state still names THIS process before removing it, so a failed startup can never erase a
// worker — or another coop's claim — that replaced it. Called under the lifecycle lock.
func clearForkClaimUnlocked(repo, name string) error {
	state, err := readForkWorkerState(repo, name)
	if err != nil {
		return err
	}
	if !state.claim || state.legacy || state.pid != os.Getpid() {
		return errors.New("fork reservation changed before startup failed")
	}
	return os.Remove(forkPid(repo, name))
}

// clearForkPidIfMine removes the fork's pidfile only if it still names THIS process, so an exiting
// worker (or a failed parent claim) never deletes a pidfile a different live worker owns.
func clearForkPidIfMine(repo, name string) {
	unlock, ok := tryLockForkState(repo, name)
	if !ok {
		return
	}
	defer unlock()
	clearForkPidIfMineUnlocked(repo, name)
}

func clearForkPidIfMineUnlocked(repo, name string) {
	data, err := os.ReadFile(forkPid(repo, name))
	if err != nil {
		return
	}
	state, err := parseForkWorkerState(string(data))
	if err == nil && !state.pending && !state.claim && state.pid == os.Getpid() {
		_ = os.Remove(forkPid(repo, name))
	}
}

// procStartToken returns an opaque identity for pid that's fixed for the process's lifetime — its
// numeric kernel start time. A pid reused by a later process reports a different token. Empty means
// the caller cannot authorize a signal and must retain cleanup state.
var readProcStartToken = processidentity.StartToken

func procStartToken(pid int) string { return readProcStartToken(pid) }

func stableProcToken(token string) bool {
	return processidentity.Stable(token)
}

func forkWorkerRecovery(name string, pid int) string {
	return fmt.Sprintf("inspect it with: ps -p %d -o pid=,lstart=,command=; after verifying it is this fork's worker, run: kill -TERM -%d; if it remains, run: kill -KILL -%d; then retry: coop fork stop %s", pid, pid, pid, name)
}

// runningForkNames returns the subset that still needs stop, in order — either a live worker or
// pending exact-label cleanup. Merge/rm share this guard so they cannot strand either state.
func runningForkNames(repo string, names []string) []string {
	var live []string
	for _, n := range names {
		if forkNeedsStop(repo, n) {
			live = append(live, n)
		}
	}
	return live
}

// seedForkQueues copies the task queue(s) into a fork's workspace and returns the repo-relative
// queue list the in-fork loop should work. An explicit --tasks source (tasks != "") seeds that one
// tree into .agent/tasks — the single-queue rule. The default (tasks == "") seeds every
// project.TaskDirs queue at its own relative path, so a monorepo fork carries all its subprojects'
// queues and the in-fork loop aggregates them via the copied .agent/project.yaml. A queue the fork
// already has is left as-is (a resumed loop keeps its progress); a monorepo member with no queue yet
// is skipped. onKept, when non-nil, is called for an already-seeded explicit source (to say --tasks
// wasn't re-applied). Single repo: TaskDirs is [.agent/tasks], so the default seeds exactly that one
// tree — byte-identical to the old single-queue path.
func seedForkQueues(repo, ws, tasks string, onKept func()) ([]string, error) {
	type seed struct{ src, rel string }
	var seeds []seed
	var queues []string
	if tasks != "" {
		rel := filepath.FromSlash(tasksRoot)
		seeds = []seed{{src: tasks, rel: rel}}
		queues = []string{rel}
	} else {
		dirs, err := project.TaskDirs(repo)
		if err != nil {
			return nil, err
		}
		for _, rel := range dirs {
			seeds = append(seeds, seed{src: filepath.Join(repo, rel), rel: rel})
			queues = append(queues, rel)
		}
	}
	for _, s := range seeds {
		dst := filepath.Join(ws, s.rel)
		switch {
		case pathExists(dst):
			if tasks != "" && onKept != nil {
				onKept() // the fork already has its queue; the explicit --tasks isn't re-applied
			}
		case !pathExists(s.src):
			// a monorepo member may not have created its queue yet — nothing to seed
		default:
			if err := copyTree(s.src, dst); err != nil {
				return nil, err
			}
			// The source may predate the four-state scaffold (or be a slice with only 00_todo); guarantee
			// all four in the seeded queue so the in-box move protocol can't rename a task into a missing dir.
			if err := scaffoldStateDirs(dst); err != nil {
				return nil, err
			}
		}
	}
	return queues, nil
}

// runForkLoop seeds the fork's queue(s) from the tasks tree(s) — an explicit --tasks source or,
// by default, every project.TaskDirs queue (only queues the fork doesn't yet have, so a resumed
// loop keeps its own progress) — then runs the unattended loop with the chosen agent, capturing
// output to the fork's log.
// detached=true means this process IS the background worker (its stdio is already the
// log, and it owns the pidfile). tasks is an absolute path resolved by the caller
// (empty = the monorepo-aware default);
// credential/model are the fork target's decomposed one-off (model@account allowed);
// the fork's preset (already loaded into a.preset by forkCreate) supplies the rotation
// ladder when neither flag is given; consult opts each iteration into peer consultation.
func (a *app) runForkLoop(repo, ws, name, agent, tasks, credential, model, effort string, peers []agents.Target, detached bool) (int, error) {
	// Seed the fork's queue(s) from the source tree(s) into the worktree and get back the
	// repo-relative queue list the in-fork loop works. An explicit --tasks seeds that one tree
	// into .agent/tasks (the single-queue rule); the default (no --tasks) seeds every
	// project.TaskDirs queue at its own relative path, so a monorepo fork carries all its
	// subprojects' queues. A queue the fork already has is kept (a resumed loop keeps its progress).
	forkQueue, err := seedForkQueues(repo, ws, tasks, func() {
		ui.Info("%s already has a queue — keeping its progress; --tasks not re-applied (use --fresh to reseed)", name)
	})
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	var sink io.Writer
	if detached {
		// This process IS the worker: stamp our OWN pid + a start-token computed now (we're
		// unambiguously alive, so pid-reuse detection is reliable — unlike the parent stamping us
		// the instant after Start, when ps may not see us yet), and on a clean exit clear the
		// pidfile only if it still names us.
		if err := writeForkPid(repo, name, os.Getpid()); err != nil {
			return -1, fmt.Errorf("fork %s worker could not record its state: %w — run: coop fork stop %s; then restart the fork", name, err, name)
		}
		defer clearForkPidIfMine(repo, name)
	} else {
		// Foreground: tee to a log so `coop fork logs` works after the fact too.
		if err := os.MkdirAll(forkStateDir(repo), 0o755); err == nil {
			if f, err := os.Create(forkLog(repo, name)); err == nil {
				defer f.Close()
				sink = f
			}
		}
	}
	a.selectRunEffort(agent, effort) // the fork target's /effort (top tier, persists across rotations)
	// The fork's rotation ladder: the fork target's one-off model/account wins; else its
	// preset's ladder (a.preset, loaded by forkCreate); else the default (agent model across
	// all accounts).
	ladder, err := oneOffLadder(model, credential, effort)
	if err != nil {
		return -1, err
	}
	if ladder == nil && a.preset != nil && agent == a.preset.LeadAgent {
		ladder = a.preset.LeadLadder
	}
	rot, err := a.buildRotation(agent, ladder)
	if err != nil {
		return -1, fmt.Errorf("fork %s: %w", name, err)
	}
	// A fork works its own seeded queue(s) in the worktree.
	previousOwner := a.forkOwner
	a.forkOwner = forkContainerOwner(repo, name)
	defer func() { a.forkOwner = previousOwner }()
	code, err := a.loop(ws, img, agent, name, rot, forkQueue, sink, peers, false, false, 0) // detached/fork loops aren't interactive; no pre-flight/task limit
	if err == nil && !detached {
		forkNextSteps(name)
	}
	return code, err
}

// recordStartedFork publishes the child identity while detach still owns the lifecycle lock. If
// persistence fails, the child is killed and reaped before returning so no live loop can escape
// without a durable stop handle.
func recordStartedFork(repo, name string, cmd *exec.Cmd) error {
	if err := writeForkPidUnlocked(repo, name, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(forkPid(repo, name))
		return err
	}
	return nil
}

// detachForkLoop re-execs coop as a session-leader background worker whose stdio is
// the fork's log, records its pid, and returns immediately. An explicit tasks path
// (absolute, resolved by the caller) is forwarded so the worker seeds the same queue; an
// empty tasks (the monorepo-aware default) is omitted so the worker re-derives it. The
// who-runs slot (a preset name or the composed target) and the --peer set are forwarded too,
// so the worker re-loads the same recipe and scope.
func (a *app) detachForkLoop(repo, name, agent, tasks, credential, model, effort, presetName string, peers []string) (int, error) {
	// Hold the same per-fork lock used by stop through the reservation and child start. This closes
	// both double-start and stop/start races without serializing unrelated forks.
	unlock, err := lockForkState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w — check permissions on %s, then retry the original coop fork command", name, err, forkStateDir(repo))
	}
	defer unlock()
	if !pathExists(forkWorkspace(repo, name)) {
		return 1, fmt.Errorf("fork %s was removed before its detached worker could start", name)
	}
	if err := claimForkPidUnlocked(repo, name); err != nil {
		return 1, err
	}
	failStart := func(cause error) (int, error) {
		if releaseErr := clearForkClaimUnlocked(repo, name); releaseErr != nil {
			return -1, fmt.Errorf("%w; release fork %s startup reservation: %v", cause, name, releaseErr)
		}
		return -1, cause
	}
	logf, err := os.Create(forkLog(repo, name))
	if err != nil {
		return failStart(err)
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return failStart(fmt.Errorf("locate coop binary: %w", err))
	}
	// The worker re-parses the who-runs positional, so forward ONE token: a preset name (the worker
	// re-loads it), or the composed target (composeTarget round-trips the fork's one-off model/account;
	// --model/--credential are retired). A fork picks one, so a preset means no target to compose.
	who := presetName
	if who == "" {
		who, err = composeTarget(agent, model, effort, credential)
		if err != nil {
			return failStart(err)
		}
	}
	reExec := []string{"fork", name, who, "--loop", "--_detached"}
	if tasks != "" {
		// An explicit --tasks is forwarded; the default (empty) is omitted so the worker re-derives
		// the monorepo-aware queue set from project.TaskDirs itself.
		reExec = append(reExec, "--tasks", tasks)
	}
	for _, peer := range peers { // one --peer per named peer (repeatable), re-resolved by the worker
		reExec = append(reExec, "--peer", peer)
	}
	cmd := exec.Command(self, reExec...)
	cmd.Dir = repo // ResolveRepo finds the parent repo, then the worker resumes the fork
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Say a worker is about to exist BEFORE forking it. If this coop dies in the gap between Start
	// and recordStartedFork, the reservation alone cannot tell "nothing was started" from "a loop is
	// out there unrecorded" — and a later start reclaiming the second case would put two loops on one
	// worktree, exactly what the claim exists to prevent. Marked, that start refuses and asks for stop.
	if err := writeForkWorkerState(repo, name, forkClaimState(true)); err != nil {
		return failStart(fmt.Errorf("record fork %s worker launch: %w", name, err))
	}
	if err := cmd.Start(); err != nil {
		return failStart(err)
	}
	if err := recordStartedFork(repo, name, cmd); err != nil {
		return -1, fmt.Errorf("record fork %s worker state: %w — the worker was stopped; fix %s, then retry the original coop fork command", name, err, forkStateDir(repo))
	}
	ui.Info("started fork %s (%s) in the background", name, agent)
	ui.Info("  coop fork logs %s -f   ·   coop fork stop %s", name, name)
	return 0, nil
}

func (a *app) forkLogs(args []string) (int, error) {
	follow := false
	var pos []string
	for _, x := range args {
		switch x {
		case "-f", "--follow":
			follow = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork logs: unknown flag %q", x)
			}
			pos = append(pos, x)
		}
	}
	name, err := oneForkName("logs", pos)
	if err != nil {
		return 2, err
	}
	if name != "" && !validExistingForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	var mu sync.Mutex
	if name != "" {
		if !pathExists(forkWorkspace(repo, name)) {
			return -1, fmt.Errorf("no such fork: %s", name) // match fork path/review, not a silent exit 0
		}
		return 0, streamLog(forkLog(repo, name), "", follow, os.Stdout, &mu)
	}
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Note("no forks yet")
		return 0, nil
	}
	if !follow {
		for _, n := range names {
			_ = streamLog(forkLog(repo, n), n, false, os.Stdout, &mu)
		}
		return 0, nil
	}
	// Follow every fork at once, prefixed (compose-style). Followers never return,
	// so this blocks until Ctrl-C.
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_ = streamLog(forkLog(repo, name), name, true, os.Stdout, &mu)
		}(n)
	}
	wg.Wait()
	return 0, nil
}

// streamLog prints a log file (optionally prefixed and followed) to w under mu.
func streamLog(path, prefix string, follow bool, w io.Writer, mu *sync.Mutex) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no output yet
		}
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			mu.Lock()
			if prefix != "" {
				fmt.Fprintf(w, "%s | %s", prefix, line)
			} else {
				fmt.Fprint(w, line)
			}
			mu.Unlock()
		}
		if err == io.EOF {
			if !follow {
				return nil
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
	}
}

func (a *app) forkStop(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork stop <name>")
	}
	name, err := oneForkName("stop", args)
	if err != nil {
		return 2, err
	}
	if !validExistingForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	workspaceExists := pathExists(forkWorkspace(repo, name))
	stateExists := pathExists(forkPid(repo, name))
	if !workspaceExists && !stateExists {
		return 1, fmt.Errorf("no such fork: %s", name) // match ls/path/rm, not "not running"
	}
	unlock, err := lockForkState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkStateDir(repo), name)
	}
	defer unlock()
	data, err := os.ReadFile(forkPid(repo, name))
	if errors.Is(err, os.ErrNotExist) {
		ui.Note("fork %s is not running", name)
		return 0, nil
	}
	if err != nil {
		return -1, fmt.Errorf("read fork %s state: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkPid(repo, name), name)
	}
	state, err := parseForkWorkerState(string(data))
	if err != nil {
		return 1, fmt.Errorf("fork %s state is malformed — inspect it with: sed -n '1,3p' %q; restore a complete pid record or reap-pending marker, then retry: coop fork stop %s", name, forkPid(repo, name), name)
	}
	pid, token := state.pid, state.token
	if state.claim {
		// A reservation names the coop process that was starting this fork, never a worker: clear it
		// (and reap whatever its interrupted start left behind) instead of signalling an innocent pid.
		pid, token = 0, ""
	}
	identity := forkProcessIdentity(pid, token)
	if pid > 0 && token != "" && !stableProcToken(token) && identity != processGone {
		return 1, fmt.Errorf("fork %s has legacy state for live pid %d, so coop will not signal an unverified process — %s", name, pid, forkWorkerRecovery(name, pid))
	}
	if identity == processIdentityUnknown {
		return 1, fmt.Errorf("fork %s worker identity for pid %d could not be verified — %s", name, pid, forkWorkerRecovery(name, pid))
	}
	if ownerProvablyDead(identity) {
		pid = 0 // stale worker state or a retryable reap marker: the exact-label reap still must run
	}
	// Preserve a live worker's identity if runtime detection fails; stale/retry state becomes a
	// tombstone so another start cannot strand the orphan before the operator retries stop.
	if pid == 0 {
		if err := writeForkWorkerState(repo, name, forkWorkerState{pending: true, legacy: state.legacy}); err != nil {
			return -1, fmt.Errorf("mark fork %s cleanup pending: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkPid(repo, name), name)
		}
	}
	if !state.legacy {
		if err := a.ensureRuntime(); err != nil {
			return -1, fmt.Errorf("fork %s cleanup needs its container runtime: %w — fix the runtime, then retry: coop fork stop %s", name, err, name)
		}
	}
	if pid > 0 {
		if err := writeForkWorkerState(repo, name, forkWorkerState{pid: pid, token: token, pending: true, legacy: state.legacy}); err != nil {
			return -1, fmt.Errorf("mark fork %s cleanup pending: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkPid(repo, name), name)
		}
	}
	// The worker is a session leader (Setsid); signal its whole group, falling back to the single
	// pid. Revalidate the start token immediately before every signal so PID reuse cannot target an
	// unrelated same-user process.
	killGroup := func(sig syscall.Signal) error {
		if pid <= 1 {
			return fmt.Errorf("refuse invalid detached worker pid %d", pid)
		}
		switch identity := forkProcessIdentity(pid, token); {
		case ownerProvablyDead(identity):
			return nil
		case identity == processIdentityUnknown:
			return errors.New("worker identity became unreadable")
		}
		if signalPID(-pid, sig) != nil {
			_ = signalPID(pid, sig)
		}
		return nil
	}
	if pid > 0 {
		if err := killGroup(syscall.SIGTERM); err != nil {
			return 1, fmt.Errorf("fork %s was not signaled because %w — %s", name, err, forkWorkerRecovery(name, pid))
		}
		exited, err := waitForExit(pid, token, 3*time.Second)
		if err != nil {
			return 1, fmt.Errorf("fork %s stop paused because %w — %s", name, err, forkWorkerRecovery(name, pid))
		}
		if !exited {
			if err := killGroup(syscall.SIGKILL); err != nil {
				return 1, fmt.Errorf("fork %s was not killed because %w — %s", name, err, forkWorkerRecovery(name, pid))
			}
			exited, err = waitForExit(pid, token, 2*time.Second)
			if err != nil {
				return 1, fmt.Errorf("fork %s stop paused because %w — %s", name, err, forkWorkerRecovery(name, pid))
			}
		}
		if !exited {
			return 1, fmt.Errorf("fork %s (pid %d) did not exit after SIGKILL — retry: coop fork stop %s", name, pid, name)
		}
	}
	if state.legacy {
		return 1, fmt.Errorf("fork %s worker stopped, but its state predates repository-scoped container ownership — coop will not risk removing another repository's namesake container; inspect your runtime for label %s=%s, remove only this fork's container, then remove %q", name, box.LabelFork, name, forkPid(repo, name))
	}
	// Tear down the loop's box if a SIGKILL'd `docker run` client orphaned it (--rm never fires on
	// SIGKILL): the box has a repo-scoped owner label, so remove exactly this fork's container(s).
	// rm -f (not just kill) so the orphan doesn't linger Exited — its run client is dead and won't.
	reapCtx, cancelReap := context.WithTimeout(context.Background(), forkStopReapTimeout)
	n, reapErr := a.rt.RemoveByLabel(reapCtx, box.LabelForkOwner, forkContainerOwner(repo, name))
	cancelReap()
	if reapErr != nil {
		return 1, fmt.Errorf("fork %s worker stopped, but its box reap failed: %w — fix the container runtime, then retry: coop fork stop %s", name, reapErr, name)
	}
	if n > 0 {
		ui.Detail("removed %s", ui.Count(n, "orphaned box container"))
	}
	if err := os.Remove(forkPid(repo, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 1, fmt.Errorf("fork %s box is gone, but its cleanup state could not be cleared: %w — inspect it and its parent with: ls -ld %q %q; remove any obstruction or restore parent write permission, then retry: coop fork stop %s", name, err, forkPid(repo, name), forkStateDir(repo), name)
	}
	ui.OK("stopped fork %s", name)
	return 0, nil
}

// waitForExit polls until the recorded worker is gone or timeout elapses; a reused PID is not the
// worker and therefore counts as exited.
func waitForExit(pid int, token string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		switch identity := forkProcessIdentity(pid, token); {
		case ownerProvablyDead(identity):
			return true, nil
		case identity == processIdentityUnknown:
			return false, errors.New("worker identity became unreadable")
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
