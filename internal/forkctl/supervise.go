package forkctl

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

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

const forkStopReapTimeout = 3 * time.Second

// ForkContainerOwner scopes the runtime cleanup label to one parent repo and fork name. Fork state
// already lives under this path-derived home, so a path-derived owner has the same move semantics.
func ForkContainerOwner(repo, name string) string {
	canonical := repo
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		canonical = resolved
	} else if absolute, absErr := filepath.Abs(repo); absErr == nil {
		canonical = absolute
	}
	sum := sha256.Sum256([]byte(canonical + "\x00" + name))
	return fmt.Sprintf("v1-%x", sum[:12])
}

// claimForkPid atomically reserves a fork's pidfile BEFORE its worker starts, so two concurrent
// detach attempts racing for the same fork can't both pass a
// check-then-write and leave two loops racing one worktree/branch. O_EXCL fails if the file exists;
// a live loop is refused, while dead/reused/pending state requires ForkStop to reap labels before a
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

// SeedForkQueues copies the task queue(s) into a fork's workspace and returns the repo-relative
// queue list the in-fork loop should work. An explicit --tasks source (tasks != "") seeds that one
// tree into .agent/tasks — the single-queue rule. The default (tasks == "") seeds every
// project.TaskDirs queue at its own relative path, so a monorepo fork carries all its subprojects'
// queues and the in-fork loop aggregates them via the copied .agent/project.yaml. A queue the fork
// already has is left as-is (a resumed loop keeps its progress); a monorepo member with no queue yet
// is skipped. onKept, when non-nil, is called for an already-seeded explicit source (to say --tasks
// wasn't re-applied). Single repo: TaskDirs is [.agent/tasks], so the default seeds exactly that one
// tree — byte-identical to the old single-queue path.
func SeedForkQueues(repo, ws, tasksOverride string, onKept func()) ([]string, error) {
	type seed struct{ src, rel string }
	var seeds []seed
	var queues []string
	if tasksOverride != "" {
		rel := filepath.FromSlash(tasks.TasksRoot)
		seeds = []seed{{src: tasksOverride, rel: rel}}
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
			if tasksOverride != "" && onKept != nil {
				onKept() // the fork already has its queue; the explicit --tasks isn't re-applied
			}
		case !pathExists(s.src):
			// a monorepo member may not have created its queue yet — nothing to seed
		default:
			if err := tasks.CopyTree(s.src, dst); err != nil {
				return nil, err
			}
			// The source may predate the four-state scaffold (or be a slice with only 00_todo); guarantee
			// all four in the seeded queue so the in-box move protocol can't rename a task into a missing dir.
			if err := tasks.ScaffoldStateDirs(dst); err != nil {
				return nil, err
			}
		}
	}
	return queues, nil
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

// DetachForkLoop re-execs coop as a session-leader background worker whose stdio is
// the fork's log, records its pid, and returns immediately. An explicit tasks path
// (absolute, resolved by the caller) is forwarded so the worker seeds the same queue; an
// empty tasks (the monorepo-aware default) is omitted so the worker re-derives it. The
// who-runs slot (a preset name or the composed target) and the --peer set are forwarded too,
// so the worker re-loads the same recipe and scope.
func (c *Control) DetachForkLoop(repo, name, agent, tasks, credential, model, effort, presetName string, peers []string) (int, error) {
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

func (c *Control) ForkLogs(args []string) (int, error) {
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
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
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

func (c *Control) ForkStop(args []string) (int, error) {
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
	repo, err := box.ResolveRepo(c.cfg.RepoOverride)
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
		if err := c.ensureRuntime(); err != nil {
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
	n, reapErr := c.rt.RemoveByLabel(reapCtx, box.LabelForkOwner, ForkContainerOwner(repo, name))
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
