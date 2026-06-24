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

// forkPool builds a fork's rotation pool: its explicit per-fork profiles (from .agent/fleet's
// `profile=` or `coop fork --profile`) when given — so a fleet can put each fork on its own account
// instead of all contending for the repo pool's first profile — else the repo pool / all signed-in.
func (a *app) forkPool(repo, agent, name string, profiles []string) (*profilePool, error) {
	if len(profiles) > 0 {
		pool, err := authedPool(a.cfg, agent, profiles)
		if err != nil {
			return nil, fmt.Errorf("fork %s: %w", name, err)
		}
		return pool, nil
	}
	return buildPool(a.cfg, repo, agent)
}

// runForkLoop seeds the fork's queue from the tasks file given to --tasks (only when
// the fork has none yet, so a resumed loop keeps its own progress), then runs the
// unattended loop with the chosen agent, capturing output to the fork's log.
// detached=true means this process IS the background worker (its stdio is already the
// log, and it owns the pidfile). tasks is an absolute path resolved by the caller.
func (a *app) runForkLoop(repo, ws, name, agent, tasks string, profiles []string, detached bool) (int, error) {
	dst := filepath.Join(ws, ".agent", "TASKS.md")
	if tasks != "" && !fileExists(dst) {
		if err := os.MkdirAll(filepath.Join(ws, ".agent"), 0o755); err != nil {
			return -1, err
		}
		if err := copyFile(tasks, dst); err != nil {
			return -1, err
		}
	} else if tasks != "" {
		// The fork already has a queue (a resumed loop keeps its progress), so the
		// tasks file isn't re-seeded — say so instead of silently ignoring it.
		ui.Info("%s already has a queue — keeping its progress; --tasks not re-applied (use --fresh to reseed)", name)
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	var sink io.Writer
	if detached {
		defer os.Remove(forkPid(repo, name)) // the worker clears its pidfile when done
	} else {
		// Foreground: tee to a log so `coop fork logs` works after the fact too.
		if err := os.MkdirAll(forkStateDir(repo), 0o755); err == nil {
			if f, err := os.Create(forkLog(repo, name)); err == nil {
				defer f.Close()
				sink = f
			}
		}
	}
	pool, err := a.forkPool(repo, agent, name, profiles)
	if err != nil {
		return -1, err
	}
	// A fork works its own seeded queue in the worktree (the fleet file carries the
	// per-component source path; here it's the standard .agent/TASKS.md).
	forkQueue := []string{filepath.Join(".agent", "TASKS.md")}
	code, err := a.loop(ws, img, agent, pool, forkQueue, sink, false, false) // detached/fork loops aren't interactive; no pre-flight
	if err == nil && !detached {
		forkNextSteps(name)
	}
	return code, err
}

// detachForkLoop re-execs coop as a session-leader background worker whose stdio is
// the fork's log, records its pid, and returns immediately. tasks is an absolute path
// (resolved by the caller) forwarded so the worker seeds the same queue.
func (a *app) detachForkLoop(repo, name, agent, tasks string, profiles []string) (int, error) {
	// Refuse to start a second worker for a fork that's already looping: the pidfile write below
	// would overwrite the first worker's pid, orphaning it untracked, and two loops would then race
	// the same worktree's queue and branch. (fleetUp skips running forks before reaching here.)
	if pid := forkRunningPid(repo, name); pid != 0 {
		return 1, fmt.Errorf("fork %s already has a loop running (pid %d) — stop it first: coop fork stop %s", name, pid, name)
	}
	if err := os.MkdirAll(forkStateDir(repo), 0o755); err != nil {
		return -1, err
	}
	logf, err := os.Create(forkLog(repo, name))
	if err != nil {
		return -1, err
	}
	defer logf.Close()
	self, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("locate coop binary: %w", err)
	}
	reExec := []string{"fork", name, agent, "--loop", "--tasks", tasks, "--_detached"}
	if len(profiles) > 0 {
		reExec = append(reExec, "--profile", strings.Join(profiles, ","))
	}
	cmd := exec.Command(self, reExec...)
	cmd.Dir = repo // ResolveRepo finds the parent repo, then the worker resumes the fork
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logf, logf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	_ = writeForkPid(repo, name, cmd.Process.Pid)
	ui.Info("started fork %s (%s) in the background", name, agent)
	ui.Info("  coop fork logs %s -f   ·   coop fork stop %s", name, name)
	return 0, nil
}

func (a *app) forkLogs(args []string) (int, error) {
	name, follow := "", false
	for _, x := range args {
		switch x {
		case "-f", "--follow":
			follow = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork logs: unknown flag %q", x)
			}
			name = x
		}
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
		ui.Info("no forks yet")
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
	name := args[0]
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
	// The worker is a session leader (Setsid); signal its whole group, falling back
	// to the single pid.
	if syscall.Kill(-pid, syscall.SIGTERM) != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = os.Remove(forkPid(repo, name))
	ui.Info("stopped fork %s", name)
	return 0, nil
}
