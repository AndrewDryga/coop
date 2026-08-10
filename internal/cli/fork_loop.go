package cli

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/ui"
)

// What's left here is LOOP material that a fork happens to use, not fork lifecycle: the two
// exclusion locks and the two functions that turn `coop fork <name> --loop` into a run of the
// loop engine. The fork lifecycle itself — supervision, land, fleet, the boards — is
// internal/forkctl; these four stayed because they belong to the loop engine, which is the next
// thing to come out of this package.

// agentLoopCmd builds the headless, autonomous command for one loop iteration of the
// given agent, carrying prompt (each agent's non-interactive form lives in its adapter).
func (a *app) agentLoopCmd(agent, prompt string) []string {
	if ag, ok := agents.Get(agent); ok {
		return ag.Headless(a.cfg, prompt)
	}
	return append([]string{agent}, prompt)
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
	forkQueue, err := forkctl.SeedForkQueues(repo, ws, tasks, func() {
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
	a.forkOwner = forkctl.ForkContainerOwner(repo, name)
	defer func() { a.forkOwner = previousOwner }()
	code, err := a.loop(ws, img, agent, name, rot, forkQueue, sink, peers, false, false, 0) // detached/fork loops aren't interactive; no pre-flight/task limit
	if err == nil && !detached {
		forkctl.ForkNextSteps(name)
	}
	return code, err
}
