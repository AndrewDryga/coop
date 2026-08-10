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
	"strings"
	"sync"
	"syscall"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

const forkStopReapTimeout = 3 * time.Second

// agentLoopCmd builds the headless, autonomous command for one loop iteration of the
// given agent, carrying prompt (each agent's non-interactive form lives in its adapter).
func (a *app) agentLoopCmd(agent, prompt string) []string {
	if ag, ok := agents.Get(agent); ok {
		return ag.Headless(a.cfg, prompt)
	}
	return append([]string{agent}, prompt)
}

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

const refAuthorityIdentityAttempts = 5 // mirrors leaseAuthorityIdentityAttempts (tasklease.go)

var errRefAuthorityIdentity = errors.New("ref authority lock changed identity while locking")

// lockRefAuthority makes the short validate→finalize→consume window exclusive PER WORKTREE: from the
// moment a completion's HEAD is trusted through the moment task authority is actually consumed for
// it (a receipt, an audit-reopen generation), nothing else may move that ref. An interactive `coop
// run`, a host-side signing rewrite (signUnpushed), a fork land (fastForwardParent), a host-driven
// completion of audit-reopened work (completeTrustedTask, reached via `coop tasks done` / fork-merge
// reconciliation), or a human commit can all move HEAD — every one of coop's own mutators takes THIS
// SAME lock for its own ref-touching moment, so coop can never trigger its own refusal. Keyed
// identically to lockLoopCheckout — the resolved worktree path, never the repo name — so a fork
// fleet stays parallel (isolate the state, don't serialize the fleet).
//
// Unlike lockLoopCheckout's bare flock, this is proved after the fact, the same way
// lockLeaseAuthorityWith proves task-lease authority (tasklease.go): flock binds a process to an
// INODE, never a name, so a lock file removed and recreated between open and flock (a careless `rm
// -rf .locks`, a future migration) would let two controllers each hold an "exclusive" lock on a
// different inode. fstat(fd) vs a fresh lstat(path), compared with os.SameFile, is the only evidence
// the lock held is the lock every other controller contends for; a mismatch drops it and retries on
// the inode that answers the name now, bounded by refAuthorityIdentityAttempts.
func lockRefAuthority(cfg *config.Config, repo string) (func(), error) {
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
	path := filepath.Join(dir, fmt.Sprintf("ref-%x.lock", sum[:12]))

	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			// Best effort only: flock is the authority, the file body just names the holder so the
			// operator doesn't have to walk a process tree to find a stuck one (fail fast, don't
			// block the loop forever).
			owner, _ := os.ReadFile(path)
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
				held := ""
				if who := strings.TrimSpace(string(owner)); who != "" {
					held = " (" + who + ")"
				}
				return nil, fmt.Errorf("another coop process is validating or consuming a completion in %s%s — wait for it to finish, then retry", checkout, held)
			}
			return nil, err
		}
		current, err := refAuthorityIsCurrent(f, path)
		if err != nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			return nil, err
		}
		if !current {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			if attempt+1 >= refAuthorityIdentityAttempts {
				return nil, fmt.Errorf("%w: %s", errRefAuthorityIdentity, path)
			}
			continue
		}
		if err := f.Truncate(0); err == nil {
			_, _ = f.WriteAt([]byte(fmt.Sprintf("pid %d\n", os.Getpid())), 0)
		}
		return func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}, nil
	}
}

// refAuthorityIsCurrent proves, with the flock already held, that the locked inode is still the one
// path resolves to — see leaseAuthorityIsCurrent's identical technique in tasklease.go.
func refAuthorityIsCurrent(f *os.File, path string) (bool, error) {
	named, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // unlinked underfoot: the lock we hold now guards an orphan
	}
	if err != nil {
		return false, err
	}
	locked, err := f.Stat()
	if err != nil {
		return false, err
	}
	return os.SameFile(locked, named), nil
}

// errRefAuthorityMoved reports that HEAD moved between the value validation is about to trust and
// the first read inside the ref-authority window — the compare-and-swap failing closed. A sentinel
// (not just a formatted error) so callers can tell this apart from a lock-acquire failure and build
// their own message with the observed/expected SHAs.
var errRefAuthorityMoved = errors.New("ref authority: HEAD moved before validation could be trusted")

// enterRefAuthorityWindow acquires the per-worktree ref-authority lock and, as the FIRST action
// inside it, re-reads HEAD and compares it against headAfter — the value a completion was validated
// against. A mismatch (or an unreadable HEAD) fails closed and releases the lock itself before
// returning, so a caller that gets an error never holds anything and never has to release. On success
// the caller owns the returned release func and must call it exactly once on every subsequent exit
// from the window — validation and consumption both happen inside this same held lock, which is what
// makes the compare meaningful: once it succeeds, nothing else can move HEAD until release runs.
func (a *app) enterRefAuthorityWindow(repo, headAfter string) (release func(), liveHead string, err error) {
	if a.beforeRefAuthorityWindow != nil {
		a.beforeRefAuthorityWindow(repo, headAfter) // test seam: a concurrent process moves HEAD here
	}
	release, err = lockRefAuthority(a.cfg, repo)
	if err != nil {
		return nil, "", fmt.Errorf("acquire ref authority for %s: %w", repo, err)
	}
	liveHead, err = gitOutErr(repo, "rev-parse", "HEAD")
	if err != nil {
		release()
		return nil, "", fmt.Errorf("re-read HEAD of %s inside the ref authority window: %w", repo, err)
	}
	if liveHead != headAfter {
		release()
		return nil, liveHead, errRefAuthorityMoved
	}
	return release, liveHead, nil
}

// lockSessionProducer excludes every Coop-owned interactive producer from one native history
// scope while a fork attributes a new ID. ConfigDir/.locks is host-only and shared across repos.
// Contention fails fast because an interactive session can remain open for hours.
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

// claimForkPid atomically reserves a fork's pidfile BEFORE its worker starts, so two concurrent
// detach attempts (a hand-run `fork -d` racing `fleet up`, or two of either) can't both pass a
// check-then-write and leave two loops racing one worktree/branch. O_EXCL fails if the file exists;
// a live loop is refused, while dead/reused/pending state requires forkStop to reap labels before a
// new start. On success the file holds this process's own reservation until the worker replaces it.
func claimForkPid(repo, name string) error {
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return err
	}
	defer unlock()
	return claimForkPidUnlocked(repo, name)
}

func claimForkPidUnlocked(repo, name string) error {
	path := forkspace.PidPath(repo, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		// A reservation owns no signalable worker yet. If this process crashes here, stop can safely
		// reap the scoped runtime label and clear the reservation without guessing at a pid.
		data, marshalErr := forkspace.ClaimState(false).Marshal()
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
	if pid := forkspace.RunningPid(repo, name); pid != 0 {
		return fmt.Errorf("fork %s already has a loop running (pid %d) — stop it first: coop fork stop %s", name, pid, name)
	}
	// A reservation names the coop process that made it, so a start crashed between the claim and its
	// worker is recoverable: the owner is provably gone AND it never forked a worker, which together
	// prove nothing is running and no box was ever started. Reclaim that; refuse everything else,
	// because a live owner is mid-start and an unverifiable one could be either.
	if state, err := forkspace.ReadWorkerState(repo, name); err == nil && state.Claim && !state.Legacy {
		switch identity := forkspace.ProcessIdentityOf(state.Pid, state.Token); {
		case identity == forkspace.ProcessIdentityMatch:
			return fmt.Errorf("fork %s is already being started by coop pid %d — wait for it, or stop it first: coop fork stop %s", name, state.Pid, name)
		case !forkspace.OwnerProvablyDead(identity):
			return fmt.Errorf("fork %s holds a start reservation from pid %d whose identity coop cannot verify — finish it with: coop fork stop %s", name, state.Pid, name)
		case state.Launched:
			return fmt.Errorf("fork %s: a start by coop pid %d was interrupted after launching its worker, which may still be looping unrecorded — finish it with: coop fork stop %s", name, state.Pid, name)
		}
		if err := forkspace.WriteWorkerState(repo, name, forkspace.ClaimState(false)); err != nil {
			return fmt.Errorf("fork %s: reclaim the start reservation abandoned by coop pid %d: %w — check permissions on %s, then retry the original coop fork command", name, state.Pid, err, forkspace.StateDir(repo))
		}
		ui.Warn("fork %s: reclaimed a start reservation from coop pid %d, which is no longer running and never launched a worker", name, state.Pid)
		return nil
	}
	return fmt.Errorf("fork %s is stopped or stopping but still needs box cleanup — finish it with: coop fork stop %s", name, name)
}

// clearForkClaimUnlocked releases only the reservation written by this detach attempt: it verifies
// the state still names THIS process before removing it, so a failed startup can never erase a
// worker — or another coop's claim — that replaced it. Called under the lifecycle lock.
func clearForkClaimUnlocked(repo, name string) error {
	state, err := forkspace.ReadWorkerState(repo, name)
	if err != nil {
		return err
	}
	if !state.Claim || state.Legacy || state.Pid != os.Getpid() {
		return errors.New("fork reservation changed before startup failed")
	}
	return os.Remove(forkspace.PidPath(repo, name))
}

func forkWorkerRecovery(name string, pid int) string {
	return fmt.Sprintf("inspect it with: ps -p %d -o pid=,lstart=,command=; after verifying it is this fork's worker, run: kill -TERM -%d; if it remains, run: kill -KILL -%d; then retry: coop fork stop %s", pid, pid, pid, name)
}

// runningForkNames returns the subset that still needs stop, in order — either a live worker or
// pending exact-label cleanup. Merge/rm share this guard so they cannot strand either state.
func runningForkNames(repo string, names []string) []string {
	var live []string
	for _, n := range names {
		if forkspace.NeedsStop(repo, n) {
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
		if err := forkspace.WritePid(repo, name, os.Getpid()); err != nil {
			return -1, fmt.Errorf("fork %s worker could not record its state: %w — run: coop fork stop %s; then restart the fork", name, err, name)
		}
		defer forkspace.ClearPidIfMine(repo, name)
	} else {
		// Foreground: tee to a log so `coop fork logs` works after the fact too.
		if err := os.MkdirAll(forkspace.StateDir(repo), 0o755); err == nil {
			if f, err := os.Create(forkspace.LogPath(repo, name)); err == nil {
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
	if err := forkspace.WritePidUnlocked(repo, name, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.Remove(forkspace.PidPath(repo, name))
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
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w — check permissions on %s, then retry the original coop fork command", name, err, forkspace.StateDir(repo))
	}
	defer unlock()
	if !pathExists(forkspace.Workspace(repo, name)) {
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
	logf, err := os.Create(forkspace.LogPath(repo, name))
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
	if err := forkspace.WriteWorkerState(repo, name, forkspace.ClaimState(true)); err != nil {
		return failStart(fmt.Errorf("record fork %s worker launch: %w", name, err))
	}
	if err := cmd.Start(); err != nil {
		return failStart(err)
	}
	if err := recordStartedFork(repo, name, cmd); err != nil {
		return -1, fmt.Errorf("record fork %s worker state: %w — the worker was stopped; fix %s, then retry the original coop fork command", name, err, forkspace.StateDir(repo))
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
	if name != "" && !forkspace.ValidExistingName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	var mu sync.Mutex
	if name != "" {
		if !pathExists(forkspace.Workspace(repo, name)) {
			return -1, fmt.Errorf("no such fork: %s", name) // match fork path/review, not a silent exit 0
		}
		return 0, streamLog(forkspace.LogPath(repo, name), "", follow, os.Stdout, &mu)
	}
	names := forkspace.Names(repo)
	if len(names) == 0 {
		ui.Note("no forks yet")
		return 0, nil
	}
	if !follow {
		for _, n := range names {
			_ = streamLog(forkspace.LogPath(repo, n), n, false, os.Stdout, &mu)
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
			_ = streamLog(forkspace.LogPath(repo, name), name, true, os.Stdout, &mu)
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
	if !forkspace.ValidExistingName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	workspaceExists := pathExists(forkspace.Workspace(repo, name))
	stateExists := pathExists(forkspace.PidPath(repo, name))
	if !workspaceExists && !stateExists {
		return 1, fmt.Errorf("no such fork: %s", name) // match ls/path/rm, not "not running"
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkspace.StateDir(repo), name)
	}
	defer unlock()
	data, err := os.ReadFile(forkspace.PidPath(repo, name))
	if errors.Is(err, os.ErrNotExist) {
		ui.Note("fork %s is not running", name)
		return 0, nil
	}
	if err != nil {
		return -1, fmt.Errorf("read fork %s state: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkspace.PidPath(repo, name), name)
	}
	state, err := forkspace.ParseWorkerState(string(data))
	if err != nil {
		return 1, fmt.Errorf("fork %s state is malformed — inspect it with: sed -n '1,3p' %q; restore a complete pid record or reap-pending marker, then retry: coop fork stop %s", name, forkspace.PidPath(repo, name), name)
	}
	pid, token := state.Pid, state.Token
	if state.Claim {
		// A reservation names the coop process that was starting this fork, never a worker: clear it
		// (and reap whatever its interrupted start left behind) instead of signalling an innocent pid.
		pid, token = 0, ""
	}
	identity := forkspace.ProcessIdentityOf(pid, token)
	if pid > 0 && token != "" && !forkspace.StableProcToken(token) && identity != forkspace.ProcessGone {
		return 1, fmt.Errorf("fork %s has legacy state for live pid %d, so coop will not signal an unverified process — %s", name, pid, forkWorkerRecovery(name, pid))
	}
	if identity == forkspace.ProcessIdentityUnknown {
		return 1, fmt.Errorf("fork %s worker identity for pid %d could not be verified — %s", name, pid, forkWorkerRecovery(name, pid))
	}
	if forkspace.OwnerProvablyDead(identity) {
		pid = 0 // stale worker state or a retryable reap marker: the exact-label reap still must run
	}
	// Preserve a live worker's identity if runtime detection fails; stale/retry state becomes a
	// tombstone so another start cannot strand the orphan before the operator retries stop.
	if pid == 0 {
		if err := forkspace.WriteWorkerState(repo, name, forkspace.WorkerState{Pending: true, Legacy: state.Legacy}); err != nil {
			return -1, fmt.Errorf("mark fork %s cleanup pending: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkspace.PidPath(repo, name), name)
		}
	}
	if !state.Legacy {
		if err := a.ensureRuntime(); err != nil {
			return -1, fmt.Errorf("fork %s cleanup needs its container runtime: %w — fix the runtime, then retry: coop fork stop %s", name, err, name)
		}
	}
	if pid > 0 {
		if err := forkspace.WriteWorkerState(repo, name, forkspace.WorkerState{Pid: pid, Token: token, Pending: true, Legacy: state.Legacy}); err != nil {
			return -1, fmt.Errorf("mark fork %s cleanup pending: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkspace.PidPath(repo, name), name)
		}
	}
	// The worker is a session leader (Setsid); signal its whole group, falling back to the single
	// pid. Revalidate the start token immediately before every signal so PID reuse cannot target an
	// unrelated same-user process.
	killGroup := func(sig syscall.Signal) error {
		if pid <= 1 {
			return fmt.Errorf("refuse invalid detached worker pid %d", pid)
		}
		switch identity := forkspace.ProcessIdentityOf(pid, token); {
		case forkspace.OwnerProvablyDead(identity):
			return nil
		case identity == forkspace.ProcessIdentityUnknown:
			return errors.New("worker identity became unreadable")
		}
		if forkspace.SignalPID(-pid, sig) != nil {
			_ = forkspace.SignalPID(pid, sig)
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
	if state.Legacy {
		return 1, fmt.Errorf("fork %s worker stopped, but its state predates repository-scoped container ownership — coop will not risk removing another repository's namesake container; inspect your runtime for label %s=%s, remove only this fork's container, then remove %q", name, box.LabelFork, name, forkspace.PidPath(repo, name))
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
	if err := os.Remove(forkspace.PidPath(repo, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 1, fmt.Errorf("fork %s box is gone, but its cleanup state could not be cleared: %w — inspect it and its parent with: ls -ld %q %q; remove any obstruction or restore parent write permission, then retry: coop fork stop %s", name, err, forkspace.PidPath(repo, name), forkspace.StateDir(repo), name)
	}
	ui.OK("stopped fork %s", name)
	return 0, nil
}

// waitForExit polls until the recorded worker is gone or timeout elapses; a reused PID is not the
// worker and therefore counts as exited.
func waitForExit(pid int, token string, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		switch identity := forkspace.ProcessIdentityOf(pid, token); {
		case forkspace.OwnerProvablyDead(identity):
			return true, nil
		case identity == forkspace.ProcessIdentityUnknown:
			return false, errors.New("worker identity became unreadable")
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
