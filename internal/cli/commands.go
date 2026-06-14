package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// resolveImage resolves the repo and its image, verifying the image is built.
func (a *app) resolveImage() (repo, img string, err error) {
	repo, err = box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return "", "", err
	}
	img = box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if !box.ImageExists(a.rt, img) {
		return "", "", fmt.Errorf("image %q not built — run 'coop build' (or ./install.sh)", img)
	}
	return repo, img, nil
}

// runInBox runs a command in the box against the current repo with the default
// homes/network/cache toggles (the common interactive path).
func (a *app) runInBox(cmd []string) (int, error) {
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

func (a *app) cmdRun(args []string) (int, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		args = a.cfg.ClaudeCmd
	}
	return a.runInBox(args)
}

// launchAgent runs a named agent: its autonomous default command when given no
// args, or a pass-through of the args you supply (without the default flags).
func (a *app) launchAgent(tool string, args []string) (int, error) {
	if len(args) == 0 {
		return a.runInBox(a.defaultCmd(tool))
	}
	return a.runInBox(append([]string{tool}, args...))
}

func (a *app) defaultCmd(tool string) []string {
	switch tool {
	case "claude":
		return a.cfg.ClaudeCmd
	case "codex":
		return a.cfg.CodexCmd
	case "gemini":
		return a.cfg.GeminiCmd
	default:
		return []string{tool}
	}
}

func (a *app) cmdLogin(args []string) (int, error) {
	tool := "claude"
	if len(args) > 0 {
		tool = args[0]
	}
	ui.Info("logging in to %s — credentials persist in %s/", tool, a.cfg.AgentDir(tool))
	cmd := []string{tool} // claude/gemini authenticate on first interactive run
	if tool == "codex" {
		cmd = []string{"codex", "login"}
	}
	return a.runInBox(cmd)
}

// cmdACP runs the box as an ACP agent over stdio: the repo mounts at its real
// host path (so the editor's absolute paths resolve, and the session history
// matches `coop`/`coop loop` — see resolveWorkdir) and no tty is allocated. The
// explicit Workdir forces the real path even if COOP_WORKDIR is set.
func (a *app) cmdACP(args []string) (int, error) {
	tool := "claude"
	if len(args) > 0 {
		tool = args[0]
	}
	var cmd []string
	switch tool {
	case "claude":
		cmd = []string{"claude-agent-acp"}
	case "codex":
		cmd = []string{"codex-acp"}
	case "gemini":
		cmd = []string{"gemini", "--acp"}
	default:
		return 2, errors.New("usage: coop acp [claude|codex|gemini]")
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Workdir: repo, Cmd: cmd, ForceNoTTY: true,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

func (a *app) cmdBuild(args []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	return 0, box.Build(a.rt, a.cfg, repo)
}

func (a *app) cmdUp(args []string) (int, error) {
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	if a.rt.Name == "container" {
		return -1, errors.New("the Apple 'container' runtime has no compose yet — use Docker or Podman for services")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	file := box.ComposeFile(repo)
	if file == "" {
		return -1, errors.New("no compose.agent.yml — run 'coop init --stack <name>' to scaffold one")
	}
	proj := box.ServicesProject(repo)
	rel, _ := filepath.Rel(repo, file)
	ui.Info("starting services from %s (waiting until healthy)", rel)
	code, err := a.rt.Run(os.Stdin, os.Stdout, os.Stderr, "compose", "-p", proj, "-f", file, "up", "-d", "--wait")
	if err != nil || code != 0 {
		return code, err
	}
	ui.Info("up on network %s_default — the box reaches them by name (db, redis, ...)", proj)
	return 0, nil
}

func (a *app) cmdDown(args []string) (int, error) {
	if err := a.rt.EnsureDaemon(); err != nil {
		return -1, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	file := box.ComposeFile(repo)
	if file == "" {
		return -1, errors.New("no compose.agent.yml here")
	}
	proj := box.ServicesProject(repo)
	cargs := []string{"compose", "-p", proj, "-f", file, "down"}
	if len(args) > 0 && (args[0] == "--volumes" || args[0] == "-v") {
		cargs = append(cargs, "--volumes")
	}
	return a.rt.Run(os.Stdin, os.Stdout, os.Stderr, cargs...)
}

func (a *app) cmdInit(args []string) (int, error) {
	stack := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--stack" && i+1 < len(args):
			stack = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--stack="):
			stack = strings.TrimPrefix(args[i], "--stack=")
		}
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	return 0, scaffold.Init(repo, stack)
}

// cmdClone hands off a secrets-free clone workspace and runs an agent in it.
func (a *app) cmdClone(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop clone <name>")
	}
	name := args[0]
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-agents", name)
	if pathExists(ws) {
		return -1, fmt.Errorf("workspace already exists: %s", ws)
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if !box.ImageExists(a.rt, img) {
		return -1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	ui.Info("cloning into %s (secrets are gitignored, so they don't come along)", ws)
	if err := gitClone(repo, ws); err != nil {
		return -1, fmt.Errorf("git clone: %w", err)
	}
	_ = gitCheckoutNewBranch(ws, name) // branch may already exist; that's fine
	// Run the agent in the clone; its exit status doesn't fail the handoff.
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: ws, Cmd: a.cfg.ClaudeCmd,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
	ui.Info("review the work, then merge from your repo:")
	ui.Info("  git fetch %q %s:review/%s && git diff @...review/%s", ws, name, name, name)
	return 0, nil
}

// cmdDispatch is the fleet unit: clone into an isolated workspace, seed it with
// that agent's slice of the queue, and run the loop there.
func (a *app) cmdDispatch(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop dispatch <name>   (reads .agent/TASKS.<name>.md)")
	}
	name := args[0]
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	if !box.ImageExists(a.rt, img) {
		return -1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	slice := filepath.Join(repo, ".agent", "TASKS."+name+".md")
	if !fileExists(slice) {
		return -1, fmt.Errorf("no .agent/TASKS.%s.md — split the queue into per-agent files first", name)
	}
	ws := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-agents", name)
	if pathExists(ws) {
		return -1, fmt.Errorf("workspace already exists: %s (remove it, or pick another name)", ws)
	}
	ui.Info("dispatching %q into %s", name, ws)
	if err := gitClone(repo, ws); err != nil {
		return -1, fmt.Errorf("git clone: %w", err)
	}
	if err := gitCheckoutNewBranch(ws, name); err != nil {
		return -1, fmt.Errorf("git checkout: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".agent"), 0o755); err != nil {
		return -1, err
	}
	if err := copyFile(slice, filepath.Join(ws, ".agent", "TASKS.md")); err != nil {
		return -1, err
	}
	// Run the loop in the clone, reusing the origin's image.
	if code, err := a.loop(ws, img); err != nil {
		return code, err
	}
	ui.Info("branch %q ready — merge from your repo:", name)
	ui.Info("  git fetch %q %s:review/%s && git diff @...review/%s", ws, name, name, name)
	return 0, nil
}

func (a *app) cmdLoop(args []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	return a.loop(repo, img)
}

const (
	loopWork  = "Read .agent/TASKS.md and AGENTS.md, then work the next unchecked items per the protocol: claim with [w], do it, run the gate, commit, log it, flip to [x]. Do not stop while a [ ] remains."
	loopAudit = "Audit: for every [x] in .agent/TASKS.md, verify its gate passes and a commit implementing it exists in the git log. Reopen any that fail — flip [x] back to [ ] and note what is missing. Do not fix anything yourself."
)

// loop works .agent/TASKS.md unattended until no "[ ]" remains, then (unless a
// custom COOP_LOOP_CMD is set) runs a one-shot audit pass over the results. A
// model rate/usage limit is not a failure: the loop waits for the reset — parsed
// from the agent's own output when possible — and resumes the same iteration, so
// a long run survives hitting the limit and continues once it clears.
func (a *app) loop(repo, img string) (int, error) {
	queue := filepath.Join(repo, ".agent", "TASKS.md")
	if !fileExists(queue) {
		return -1, errors.New("no .agent/TASKS.md found — run 'coop init' first")
	}
	if !box.ImageExists(a.rt, img) {
		return -1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	custom := a.cfg.LoopCmd
	base := custom
	if len(base) == 0 {
		base = a.cfg.ClaudeCmd
	}
	ui.Info("starting unattended loop on %s (Ctrl-C to stop)", queue)
	fails, waits := 0, 0
	for n := 1; queueHasTodo(queue); {
		ui.Info("iteration %d", n)
		cmd := base
		if len(custom) == 0 {
			cmd = append(append([]string{}, base...), "-p", loopWork)
		}
		code, out, err := a.runIteration(repo, img, cmd)
		switch action, wait, resetAt := decideIteration(code, err, out, time.Now(), &fails, &waits); action {
		case actContinue:
			n++
		case actWait:
			// A rate/usage limit is expected on long runs: wait for the reset,
			// then retry this same iteration rather than burning it.
			sleepForLimit(wait, resetAt)
		case actRetry:
			ui.Info("iteration failed (%d/%d) — retrying in 10s", fails, maxLoopFailures)
			time.Sleep(10 * time.Second)
		case actStop:
			if waits > maxLimitWaits {
				return code, fmt.Errorf("still rate limited after %d waits — stopping", maxLimitWaits)
			}
			return code, fmt.Errorf("iteration failed %d times in a row — stopping", fails)
		}
	}
	if len(custom) == 0 {
		ui.Info("queue empty — running audit pass")
		_, _, _ = a.runIteration(repo, img, append(append([]string{}, base...), "-p", loopAudit))
	}
	if queueHasTodo(queue) {
		ui.Info("audit reopened items — run 'coop loop' again")
	} else {
		fmt.Fprintln(os.Stderr, ui.Bold(ui.Green("✓ queue verified done")))
	}
	return 0, nil
}

// runIteration runs one boxed command in batch mode, teeing its output to the
// terminal while capturing the tail so a rate-limit notice can be detected.
func (a *app) runIteration(repo, img string, cmd []string) (code int, output string, err error) {
	tail := &tailWriter{max: 64 << 10}
	code, err = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Batch: true,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
		Stdout: io.MultiWriter(os.Stdout, tail),
		Stderr: io.MultiWriter(os.Stderr, tail),
	})
	return code, tail.String(), err
}
