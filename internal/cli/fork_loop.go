package cli

import (
	"bufio"
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
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/ui"
)

// agentLoopCmd builds the headless, autonomous command for one loop iteration of the
// given agent, carrying prompt (each agent's non-interactive form lives in its adapter).
func (a *app) agentLoopCmd(agent, prompt string) []string {
	if ag, ok := agents.Get(agent); ok {
		return ag.Headless(a.cfg, prompt)
	}
	return append([]string{agent}, prompt)
}

// Per-fork process state (logs + pidfiles) lives in <repo>-forks/.coop/.
func forkStateDir(repo string) string  { return filepath.Join(forkHome(repo), ".coop") }
func forkLog(repo, name string) string { return filepath.Join(forkStateDir(repo), name+".log") }
func forkPid(repo, name string) string { return filepath.Join(forkStateDir(repo), name+".pid") }

// forkRunningPid returns the live pid of a detached loop for name, or 0. A stale pidfile is cleaned
// up — whether the process is gone, or its pid was reused by an unrelated (same-user) process after
// the worker crashed (caught by the start-time token).
func forkRunningPid(repo, name string) int {
	data, err := os.ReadFile(forkPid(repo, name))
	if err != nil {
		return 0
	}
	pid, token := parsePidfile(string(data))
	if pid <= 0 {
		return 0
	}
	if syscall.Kill(pid, 0) != nil {
		_ = os.Remove(forkPid(repo, name)) // process gone
		return 0
	}
	// The pid exists — but after a crash the OS may have handed it to an unrelated process. If we
	// recorded the worker's start time, a different start time now proves it's a different process.
	// Only a DEFINITE mismatch counts as dead: an empty/failed reading is treated as still-alive, so
	// a ps hiccup can never make us wrongly double-start a live loop.
	if token != "" {
		if cur := procStartToken(pid); cur != "" && cur != token {
			_ = os.Remove(forkPid(repo, name)) // pid reused by another process
			return 0
		}
	}
	return pid
}

// parsePidfile reads a fork pidfile's "<pid>\n<start-token>" form. The token is optional, so an
// older pid-only file still parses (without start-time corroboration). pid 0 means unparseable.
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

// writeForkPid records the worker's pid plus a start-time token, so forkRunningPid can later tell a
// live worker from an unrelated process that reused the pid after a crash.
func writeForkPid(repo, name string, pid int) error {
	return os.WriteFile(forkPid(repo, name), []byte(fmt.Sprintf("%d\n%s\n", pid, procStartToken(pid))), 0o644)
}

// claimForkPid atomically reserves a fork's pidfile BEFORE its worker starts, so two concurrent
// detach attempts (a hand-run `fork -d` racing `fleet up`, or two of either) can't both pass a
// check-then-write and leave two loops racing one worktree/branch. O_EXCL fails if the file exists;
// an existing LIVE loop is refused, a STALE file (dead/reused pid, which forkRunningPid removes) is
// reclaimed on the retry. On success the file holds a placeholder pid until the worker overwrites it.
func claimForkPid(repo, name string) error {
	path := forkPid(repo, name)
	for try := 0; try < 2; try++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			return f.Close()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if pid := forkRunningPid(repo, name); pid != 0 {
			return fmt.Errorf("fork %s already has a loop running (pid %d) — stop it first: coop fork stop %s", name, pid, name)
		}
		// forkRunningPid cleared a stale file; loop to retry the exclusive claim.
	}
	return fmt.Errorf("fork %s: another loop start is racing — try again", name)
}

// clearForkPidIfMine removes the fork's pidfile only if it still names THIS process, so an exiting
// worker (or a failed parent claim) never deletes a pidfile a different live worker owns.
func clearForkPidIfMine(repo, name string) {
	data, err := os.ReadFile(forkPid(repo, name))
	if err != nil {
		return
	}
	if pid, _ := parsePidfile(string(data)); pid == os.Getpid() {
		_ = os.Remove(forkPid(repo, name))
	}
}

// procStartToken returns an opaque identity for pid that's fixed for the process's lifetime — its
// start time, via ps(1) (portable across macOS and Linux). A pid reused by a later process reports a
// different start time. Empty if it can't be read (then liveness falls back to existence only). It
// only runs for a pid that already passed the existence check, i.e. a genuinely-running fork.
func procStartToken(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runningForkNames returns the subset of names whose detached loop is still alive, in order — the
// guard merge shares so it never rebases or deletes a fork out from under a live worker (prune
// refuses on the same signal). forkRunningPid cleans any stale pidfile as a side effect.
func runningForkNames(repo string, names []string) []string {
	var live []string
	for _, n := range names {
		if forkRunningPid(repo, n) != 0 {
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
		_ = writeForkPid(repo, name, os.Getpid())
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
	ladder, err := oneOffLadder(model, credential)
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
	code, err := a.loop(ws, img, agent, name, rot, forkQueue, sink, peers, false, false) // name labels each box (coop.fork=); detached/fork loops aren't interactive; no pre-flight
	if err == nil && !detached {
		forkNextSteps(name)
	}
	return code, err
}

// detachForkLoop re-execs coop as a session-leader background worker whose stdio is
// the fork's log, records its pid, and returns immediately. An explicit tasks path
// (absolute, resolved by the caller) is forwarded so the worker seeds the same queue; an
// empty tasks (the monorepo-aware default) is omitted so the worker re-derives it. model,
// preset, and consult are forwarded too, so the worker re-loads the same recipe and scope.
func (a *app) detachForkLoop(repo, name, agent, tasks, credential, model, effort, presetName string, consult []string) (int, error) {
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		return -1, err
	}
	// Atomically reserve the fork before starting its worker, so two concurrent detach attempts
	// can't both pass a check-then-write and race two loops on one worktree/branch. (fleetUp also
	// skips running forks, but that check has the same window without this.)
	if err := claimForkPid(repo, name); err != nil {
		return 1, err
	}
	logf, err := os.Create(forkLog(repo, name))
	if err != nil {
		clearForkPidIfMine(repo, name) // release the reservation on a failed start
		return -1, err
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate coop binary: %w", err)
	}
	// The worker re-parses these, so forward the agent+model+account as ONE positional target
	// (--model/--credential are retired) — composeTarget round-trips the fork's one-off selection.
	target, err := composeTarget(agent, model, effort, credential)
	if err != nil {
		clearForkPidIfMine(repo, name)
		return -1, err
	}
	reExec := []string{"fork", name, target, "--loop", "--_detached"}
	if tasks != "" {
		// An explicit --tasks is forwarded; the default (empty) is omitted so the worker re-derives
		// the monorepo-aware queue set from project.TaskDirs itself.
		reExec = append(reExec, "--tasks", tasks)
	}
	if presetName != "" {
		reExec = append(reExec, "--preset", presetName) // the worker re-loads the preset itself
	}
	for _, peer := range consult { // one --consult per named peer (repeatable), re-resolved by the worker
		reExec = append(reExec, "--consult", peer)
	}
	cmd := exec.Command(self, reExec...)
	cmd.Dir = repo // ResolveRepo finds the parent repo, then the worker resumes the fork
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		clearForkPidIfMine(repo, name) // release the reservation
		return -1, err
	}
	_ = writeForkPid(repo, name, cmd.Process.Pid) // name the child now; the worker re-stamps with its own token
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
	if name != "" && !validForkName(name) {
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
	if !validForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if !pathExists(forkWorkspace(repo, name)) {
		return 1, fmt.Errorf("no such fork: %s", name) // match ls/path/rm, not "not running"
	}
	pid := forkRunningPid(repo, name)
	if pid == 0 {
		return 1, fmt.Errorf("fork %s is not running", name)
	}
	// Reaping the loop's box container needs the runtime; a not-running fork (handled above) doesn't,
	// so detect only here rather than eagerly — `coop fork stop` on an idle fork works with no runtime.
	if err := a.ensureRuntime(); err != nil {
		return -1, err
	}
	// The worker is a session leader (Setsid); signal its whole group, falling back to the single
	// pid. SIGTERM first, then escalate to SIGKILL if it doesn't exit (mid-iteration / blocked).
	killGroup := func(sig syscall.Signal) {
		if syscall.Kill(-pid, sig) != nil {
			_ = syscall.Kill(pid, sig)
		}
	}
	killGroup(syscall.SIGTERM)
	if !waitForExit(pid, 3*time.Second) {
		killGroup(syscall.SIGKILL)
		waitForExit(pid, 2*time.Second)
	}
	// Only clear the pidfile once the worker is actually gone — removing it while the worker still
	// lives would make the fork invisible and re-open the double-start window claimForkPid closes.
	if syscall.Kill(pid, 0) == nil {
		return 1, fmt.Errorf("fork %s (pid %d) did not exit even after SIGKILL — leaving it tracked", name, pid)
	}
	// Tear down the loop's box if a SIGKILL'd `docker run` client orphaned it (--rm never fires on
	// SIGKILL): the box is labeled coop.fork=<name>, so remove exactly this fork's container(s).
	// rm -f (not just kill) so the orphan doesn't linger Exited — its run client is dead and won't.
	if n := a.rt.RemoveByLabel(box.LabelFork, name); n > 0 {
		ui.Info("  removed %s", ui.Count(n, "orphaned box container"))
	}
	_ = os.Remove(forkPid(repo, name))
	ui.OK("stopped fork %s", name)
	return 0, nil
}

// waitForExit polls until pid is gone or timeout elapses; it reports whether the process exited.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if syscall.Kill(pid, 0) != nil {
			return true // gone (ESRCH) — or not ours to signal, either way not the live worker
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
