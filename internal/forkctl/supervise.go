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
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/tasks"
	"github.com/AndrewDryga/coop/internal/ui"
)

// forkStopReapTimeout bounds each exact-label box removal in fork stop. It guards a wedged runtime,
// not speed: a busy Docker answers ps and rm in seconds (3 s failed under make check, 2026-10-02),
// and a timeout here fails the stop with "fix the container runtime", which a slow daemon isn't.
const forkStopReapTimeout = 30 * time.Second

// ForkContainerOwner scopes runtime cleanup to one parent repo, fork name, and—on every new
// launch—the immutable workspace generation. The variadic form keeps the pre-generation value
// readable for stopping legacy workers; callers that can mutate a current fork always pass one.
func ForkContainerOwner(repo, name string, generation ...forkspace.Generation) string {
	canonical := repo
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		canonical = resolved
	} else if absolute, absErr := filepath.Abs(repo); absErr == nil {
		canonical = absolute
	}
	version, material := "v1", canonical+"\x00"+name
	if len(generation) > 0 && generation[0] != "" {
		version = "v2"
		material += "\x00" + string(generation[0])
	}
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("%s-%x", version, sum[:12])
}

func workerStateFormatError(repo, name string, err error) error {
	path := forkspace.PidPath(repo, name)
	switch {
	case errors.Is(err, forkspace.ErrUnsupportedWorkerStateVersion):
		return fmt.Errorf("fork %s uses an unsupported detached-worker state version at %q — Coop left the exact file unchanged; use the Coop version that wrote it and do not edit its header", name, path)
	default:
		// This includes the start and cleanup records of Coop 11.3.0 and earlier. One of those can
		// name a worker that still runs, so the file may go only once no loop for the fork remains.
		// The worker's command line names no repository, so the check is by its working directory.
		return fmt.Errorf("fork %s state at %q is malformed — Coop left the exact file unchanged and will not infer a process identity from it (inspect it with: sed -n '1,4p' %q). To clear it, list this fork's loops with: pgrep -fl -- '[f]ork %s .*--_detached'; stop only one whose working directory is this repository (lsof -a -d cwd -p <pid>) with: kill -TERM -<pid>, or kill -KILL -<pid> if it stays; then remove the file: rm -- %q; then run: coop fork stop %s", name, path, path, name, path, name)
	}
}

// CheckWorkerStateFormat rejects state this Coop cannot own without locking or touching it. Command
// paths call it before runtime probes, prompts, or workspace/metadata changes; locked start and stop
// still parse again so a state replacement after this read cannot bypass the lifecycle authority.
func CheckWorkerStateFormat(repo, name string) error {
	data, err := os.ReadFile(forkspace.PidPath(repo, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read fork %s state: %w", name, err)
	}
	if _, err := forkspace.ParseWorkerState(string(data)); err != nil {
		return workerStateFormatError(repo, name, err)
	}
	return nil
}

// requireNoWorkerLocked refuses a detached start while the fork has a lifecycle record: a live
// worker is already looping, and any other record names a worker Coop cannot confirm (gone, or
// alive but unverifiable) that may have left a box behind, so `coop fork stop` deals with it first.
// The caller holds the lifecycle lock.
func requireNoWorkerLocked(repo, name string) error {
	state, err := forkspace.ReadWorkerState(repo, name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return fmt.Errorf("read fork %s state: %w", name, err)
		}
		return workerStateFormatError(repo, name, err)
	}
	switch identity := forkspace.ProcessIdentityOf(state.Pid, state.Token); {
	case identity == forkspace.ProcessIdentityMatch:
		return fmt.Errorf("fork %s already has a loop running (pid %d) — stop it first: coop fork stop %s", name, state.Pid, name)
	case forkspace.OwnerProvablyDead(identity):
		return fmt.Errorf("fork %s has a leftover record from an earlier background loop — clean it up first: coop fork stop %s", name, name)
	default: // a live pid whose start token cannot be read: stop prints how to check it by hand
		return fmt.Errorf("fork %s still has a background loop record — clean it up first: coop fork stop %s", name, name)
	}
}

func forkWorkerRecovery(name string, pid int) string {
	return fmt.Sprintf("inspect it with: ps -p %d -o pid=,lstart=,command=; after verifying it is this fork's worker, run: kill -TERM -%d; if it remains, run: kill -KILL -%d; then retry: coop fork stop %s", pid, pid, pid, name)
}

// runningForkNames returns the subset that still needs stop, in order — a live worker, or a leftover
// record whose exact-label cleanup has not run. Merge/rm share this guard so they strand neither.
func runningForkNames(repo string, names []string) []string {
	var live []string
	for _, n := range names {
		if forkspace.NeedsStop(repo, n) {
			live = append(live, n)
		}
	}
	return live
}

// recordStartedFork writes the child's record while detach still holds the lifecycle lock. If that
// fails, the child is killed and reaped before returning: it would find no record and exit on its
// own, but no loop may run, even briefly, without a stop handle.
func recordStartedFork(repo, name string, cmd *exec.Cmd, generation forkspace.Generation) error {
	if err := forkspace.WritePid(repo, name, cmd.Process.Pid, generation); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	return nil
}

// DetachForkLoop re-execs coop as a session-leader background worker whose stdio is
// the fork's log, records its pid, and returns immediately. An explicit tasks path
// (absolute, resolved by the caller) is forwarded so the worker selects the same canonical queue; an
// empty tasks (the monorepo-aware default) is omitted so the worker re-derives it. The
// who-runs slot (a preset name or the composed target) and the --peer set are forwarded too,
// so the worker re-loads the same recipe and scope. network carries the launch's
// egress flags verbatim, because the worker admits its own policy at loop start.
func (c *Control) DetachForkLoop(repo, name, agent, tasks, credential, model, effort, presetName string, peers, network []string, identities ...forkspace.Identity) (int, error) {
	var generation forkspace.Generation
	if len(identities) > 0 {
		if identities[0].Name != name || !forkspace.ValidGeneration(identities[0].Generation) {
			return -1, errors.New("detached fork received an invalid generation identity")
		}
		generation = identities[0].Generation
	}
	// Hold the per-fork lock that stop uses from this check until the child's record exists. The
	// child's first act is to take the same lock and confirm the record names it, so a start that
	// fails, or a parent that dies, before the record is written leaves a child that exits untouched.
	// Two starts, or a start and a stop, cannot interleave; unrelated forks are not serialized.
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w — check permissions on %s, then retry the original coop fork command", name, err, forkspace.StateDir(repo))
	}
	defer unlock()
	if !pathExists(forkspace.Workspace(repo, name)) {
		return 1, fmt.Errorf("fork %s was removed before its detached worker could start", name)
	}
	if err := requireNoWorkerLocked(repo, name); err != nil {
		return 1, err
	}
	logf, err := OpenForkLog(forkspace.LogPath(repo, name))
	if err != nil {
		return -1, err
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate coop binary: %w", err)
	}
	// The worker re-parses the who-runs positional, so forward ONE token: a preset name (the worker
	// re-loads it), or the composed target (composeTarget round-trips the fork's one-off
	// model/account). A fork picks one, so a preset means no target to compose.
	who := presetName
	if who == "" {
		who, err = composeTarget(agent, model, effort, credential)
		if err != nil {
			return -1, err
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
	reExec = append(reExec, network...) // the worker admits this policy itself, at its own loop start
	cmd := exec.Command(self, reExec...)
	cmd.Dir = repo // ResolveRepo finds the parent repo, then the worker resumes the fork
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	if err := recordStartedFork(repo, name, cmd, generation); err != nil {
		return -1, fmt.Errorf("record fork %s worker state: %w — the worker was stopped; fix %s, then retry the original coop fork command", name, err, forkspace.StateDir(repo))
	}
	// The worker launched. That is all this confirms — the task work itself has not started yet,
	// so the lines below are the two commands that watch and end it, not a result.
	ui.OK("Started fork %s in the background", name)
	ui.Note("")
	ui.Note("  Agent: %s", agent)
	ui.Note("  Logs:  coop fork logs %s --follow", name)
	ui.Note("  Stop:  coop fork stop %s", name)
	return 0, nil
}

// OpenForkLog creates or truncates one owner-only fork log without following a final link.
// Both foreground CLI loops and detached supervision use this boundary. Writes append: a child
// whose start died before its record still holds an earlier open, and its refusal must land after a
// newer start's output, not over it.
func OpenForkLog(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return nil, fmt.Errorf("fork log %s must be a regular file", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("fork log %s must be a regular file", path)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
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
	if err := forkspace.EnsureStateDir(repo); err != nil {
		return -1, fmt.Errorf("prepare fork log state: %w", err)
	}
	var mu sync.Mutex
	if name != "" {
		if !pathExists(forkspace.Workspace(repo, name)) {
			return -1, fmt.Errorf("no such fork: %s", name) // match fork path/review, not a silent exit 0
		}
		opened, err := streamLog(forkspace.LogPath(repo, name), "", follow, os.Stdout, &mu)
		if err != nil {
			return 1, forkLogReadError(repo, name, err)
		}
		if !opened {
			ui.Note("Fork %s has no log output yet.", name)
		}
		return 0, nil
	}
	names, err := forkspace.Names(repo)
	if err != nil {
		return -1, err
	}
	if len(names) == 0 {
		ui.Note("No forks yet.")
		return 0, nil
	}
	if !follow {
		var failures []error
		for _, n := range names {
			opened, err := streamLog(forkspace.LogPath(repo, n), n, false, os.Stdout, &mu)
			if err != nil {
				failures = append(failures, forkLogReadError(repo, n, err))
			} else if !opened {
				ui.Note("Fork %s has no log output yet.", n)
			}
		}
		if len(failures) > 0 {
			return 1, fmt.Errorf("read fork logs: %w", errors.Join(failures...))
		}
		return 0, nil
	}
	// Follow every fork at once, prefixed (compose-style). Followers never return,
	// so this blocks until Ctrl-C.
	var wg sync.WaitGroup
	results := make(chan error, len(names))
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			opened, err := streamLog(forkspace.LogPath(repo, name), name, true, os.Stdout, &mu)
			if err != nil {
				err = forkLogReadError(repo, name, err)
				mu.Lock()
				ui.Error("%v", err)
				mu.Unlock()
			} else if !opened {
				mu.Lock()
				ui.Note("Fork %s has no log output yet.", name)
				mu.Unlock()
			}
			results <- err
		}(n)
	}
	wg.Wait()
	close(results)
	failed := 0
	for err := range results {
		if err != nil {
			failed++
		}
	}
	if failed > 0 {
		return 1, fmt.Errorf("%s failed; see the errors above, fix the log paths, and retry", ui.Count(failed, "fork log stream"))
	}
	return 0, nil
}

func forkLogReadError(repo, name string, err error) error {
	return fmt.Errorf("fork %s log %s: %w — fix ownership or permissions under %s and retry", name, forkspace.LogPath(repo, name), err, forkspace.StateDir(repo))
}

// streamLog prints a log file (optionally prefixed and followed) to w under mu.
func streamLog(path, prefix string, follow bool, w io.Writer, mu *sync.Mutex) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // no output yet
		}
		return false, err
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
				return true, nil
			}
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if err != nil {
			return true, err
		}
	}
}

func (c *Control) ForkStop(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, ui.MissingArgument("fork name", "coop fork stop", "coop fork stop <name>")
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
	generationExists := pathExists(forkspace.GenerationPath(repo, name))
	if !workspaceExists && !stateExists && !generationExists {
		return 1, fmt.Errorf("no such fork: %s", name) // match ls/path/rm, not "not running"
	}
	if err := CheckWorkerStateFormat(repo, name); err != nil {
		return 1, err
	}
	unlock, err := forkspace.LockState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkspace.StateDir(repo), name)
	}
	defer unlock()
	workspaceExists = pathExists(forkspace.Workspace(repo, name))
	forkIdentity, hasGeneration, err := forkspace.ReadGeneration(repo, name)
	if err != nil {
		return 1, fmt.Errorf("read fork %s generation before stop: %w", name, err)
	}
	data, err := os.ReadFile(forkspace.PidPath(repo, name))
	if errors.Is(err, os.ErrNotExist) {
		// Stop owns detached workers, not live foreground/ACP sessions. It does, however, own stale
		// cleanup evidence for this exact generation: a dead foreground supervisor cannot reap its
		// own runtime or activity record, and retaining that proof forever would make rm/merge/fresh
		// permanently impossible. Reap only provably-dead records by their exact execution label.
		needsWorkerReap := hasGeneration && !workspaceExists
		var stale []forkspace.ExecutionObservation
		if hasGeneration {
			observations, problems := forkspace.Executions(repo)
			if len(problems) > 0 {
				return 1, fmt.Errorf("fork %s activity registry is unreadable: %w", name, errors.Join(problems...))
			}
			for _, observation := range observations {
				if observation.Record.Fork == nil || *observation.Record.Fork != forkIdentity {
					continue
				}
				if observation.Record.Role == forkspace.ExecutionRoleDetachedWorker && !observation.Stale {
					return 1, fmt.Errorf("fork %s has detached-worker activity %s without worker state; its process identity is still live or unverified", name, observation.Record.ID)
				}
				if observation.Stale {
					stale = append(stale, observation)
					if observation.Record.Role == forkspace.ExecutionRoleDetachedWorker {
						needsWorkerReap = true
					}
				}
			}
		}
		if needsWorkerReap || len(stale) > 0 {
			if err := c.ensureRuntime(); err != nil {
				return -1, fmt.Errorf("fork %s orphan cleanup needs its container runtime: %w", name, err)
			}
			removed := 0
			for _, observation := range stale {
				labels := map[string]string{
					box.LabelForkOwner:      ForkContainerOwner(repo, name, forkIdentity.Generation),
					box.LabelForkGeneration: string(forkIdentity.Generation),
					box.LabelExecution:      observation.Record.ID,
				}
				reapCtx, cancelReap := context.WithTimeout(context.Background(), forkStopReapTimeout)
				n, reapErr := c.rt.RemoveByLabels(reapCtx, labels)
				cancelReap()
				if reapErr != nil {
					return 1, fmt.Errorf("fork %s stale sandbox %s cleanup failed: %w", name, observation.Record.ID, reapErr)
				}
				removed += n
				if err := forkspace.EndExecution(repo, observation.Record); err != nil {
					return 1, fmt.Errorf("fork %s runtime is gone, but stale activity %s could not be retired: %w", name, observation.Record.ID, err)
				}
			}
			if removed > 0 {
				ui.Detail("removed %s", ui.Count(removed, "orphaned box container"))
			}
		}
		if needsWorkerReap {
			labels := map[string]string{
				box.LabelForkOwner:      ForkContainerOwner(repo, name, forkIdentity.Generation),
				box.LabelForkGeneration: string(forkIdentity.Generation),
				box.LabelForkWorker:     box.LabelOn,
			}
			reapCtx, cancelReap := context.WithTimeout(context.Background(), forkStopReapTimeout)
			n, reapErr := c.rt.RemoveByLabels(reapCtx, labels)
			cancelReap()
			if reapErr != nil {
				return 1, fmt.Errorf("fork %s has no worker state, but exact-generation box cleanup failed: %w", name, reapErr)
			}
			if n > 0 {
				ui.Detail("removed %s", ui.Count(n, "orphaned box container"))
			}
			if err := forkspace.RemoveDeadForkExecutionsLocked(repo, forkIdentity, forkspace.ExecutionRoleDetachedWorker); err != nil {
				return 1, fmt.Errorf("fork %s runtime is gone, but its activity record cleanup failed: %w", name, err)
			}
		}
		if hasGeneration && (needsWorkerReap || len(stale) > 0) {
			if err := pauseForkTaskAssignments(c.cfg, repo, forkIdentity); err != nil {
				return 1, fmt.Errorf("fork %s stopped, but pausing its task assignment failed: %w", name, err)
			}
		}
		ui.Note("Fork %s is not running.", name)
		return 0, nil
	}
	if err != nil {
		return -1, fmt.Errorf("read fork %s state: %w — check permissions on %s, then retry: coop fork stop %s", name, err, forkspace.PidPath(repo, name), name)
	}
	state, err := forkspace.ParseWorkerState(string(data))
	if err != nil {
		return 1, workerStateFormatError(repo, name, err)
	}
	// From here a worker really is being signalled, so the narration starts — a "stopping" line
	// above the not-running branch would describe work that never happened.
	ui.Note("Stopping fork %s", name)
	if state.Generation != "" {
		if !hasGeneration || forkIdentity.Generation != state.Generation {
			return 1, fmt.Errorf("fork %s worker state generation does not match host generation authority", name)
		}
	} else if hasGeneration {
		return 1, fmt.Errorf("fork %s has legacy worker state beside generation authority — inspect both before cleanup", name)
	}
	pid, token := state.Pid, state.Token
	identity := forkspace.ProcessIdentityOf(pid, token)
	if identity == forkspace.ProcessIdentityUnknown {
		return 1, fmt.Errorf("fork %s worker identity for pid %d could not be verified — %s", name, pid, forkWorkerRecovery(name, pid))
	}
	// The record stays as it is until the box is gone. If anything below fails, a dead worker's
	// record is what the next stop retries with, and it keeps a new start from stranding an orphan.
	if err := c.ensureRuntime(); err != nil {
		return -1, fmt.Errorf("fork %s cleanup needs its container runtime: %w — fix the runtime, then retry: coop fork stop %s", name, err, name)
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
	if identity == forkspace.ProcessIdentityMatch {
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
	// Tear down the loop's box if a SIGKILL'd `docker run` client orphaned it (--rm never fires on
	// SIGKILL): the box has a repo-scoped owner label, so remove exactly this fork's container(s).
	// rm -f (not just kill) so the orphan doesn't linger Exited — its run client is dead and won't.
	reapCtx, cancelReap := context.WithTimeout(context.Background(), forkStopReapTimeout)
	labels := map[string]string{box.LabelForkOwner: ForkContainerOwner(repo, name, state.Generation)}
	if state.Generation != "" {
		labels[box.LabelForkGeneration] = string(state.Generation)
		labels[box.LabelForkWorker] = box.LabelOn
	}
	n, reapErr := c.rt.RemoveByLabels(reapCtx, labels)
	cancelReap()
	if reapErr != nil {
		return 1, fmt.Errorf("fork %s worker stopped, but its box reap failed: %w — fix the container runtime, then retry: coop fork stop %s", name, reapErr, name)
	}
	if hasGeneration {
		// A killed loop can leave its child supervisor's sandbox record behind. Retire only dead
		// loop children after verifying their exact runtime is gone; live editor sessions stay held.
		observations, problems := forkspace.Executions(repo)
		if len(problems) > 0 {
			return 1, fmt.Errorf("fork %s activity registry is unreadable: %w — cleanup state is kept; resolve the reported registry error, then retry: coop fork stop %s", name, errors.Join(problems...), name)
		}
		for _, observation := range observations {
			record := observation.Record
			if !observation.Stale || record.Fork == nil || *record.Fork != forkIdentity ||
				record.Kind != forkspace.ExecutionForkLoop || record.Role != forkspace.ExecutionRoleSandbox {
				continue
			}
			labels := map[string]string{
				box.LabelForkOwner:      ForkContainerOwner(repo, name, forkIdentity.Generation),
				box.LabelForkGeneration: string(forkIdentity.Generation),
				box.LabelExecution:      record.ID,
			}
			reapCtx, cancelReap := context.WithTimeout(context.Background(), forkStopReapTimeout)
			removed, reapErr := c.rt.RemoveByLabels(reapCtx, labels)
			cancelReap()
			if reapErr != nil {
				return 1, fmt.Errorf("fork %s stale loop sandbox %s cleanup failed: %w — fix the container runtime, then retry: coop fork stop %s", name, record.ID, reapErr, name)
			}
			n += removed
			if err := forkspace.EndExecution(repo, record); err != nil {
				return 1, fmt.Errorf("fork %s runtime is gone, but stale loop activity %s could not be retired: %w — cleanup state is kept; resolve the reported registry error, then retry: coop fork stop %s", name, record.ID, err, name)
			}
		}
		if err := forkspace.RemoveDeadForkExecutionsLocked(repo, forkIdentity, forkspace.ExecutionRoleDetachedWorker); err != nil {
			return 1, fmt.Errorf("fork %s worker and box are gone, but its activity record cleanup failed: %w", name, err)
		}
	}
	if n > 0 {
		ui.Detail("removed %s", ui.Count(n, "orphaned box container"))
	}
	if err := os.Remove(forkspace.PidPath(repo, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 1, fmt.Errorf("fork %s box is gone, but its cleanup state could not be cleared: %w — inspect it and its parent with: ls -ld %q %q; remove any obstruction or restore parent write permission, then retry: coop fork stop %s", name, err, forkspace.PidPath(repo, name), forkspace.StateDir(repo), name)
	}
	if hasGeneration {
		if err := pauseForkTaskAssignments(c.cfg, repo, forkIdentity); err != nil {
			return 1, fmt.Errorf("fork %s worker and box stopped, but pausing its task assignment failed: %w", name, err)
		}
	}
	ui.Note("")
	ui.OK("Fork %s stopped", name)
	if remembered := ReadForkAgent(forkspace.Workspace(repo, name)); remembered != "" && remembered != "?" {
		ui.Note("")
		ui.Note("  Continue: coop fork %s %s --loop", name, remembered)
	}
	return 0, nil
}

func pauseForkTaskAssignments(_ *config.Config, repo string, identity forkspace.Identity) error {
	return tasks.PauseForkAssignments(repo, identity)
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
