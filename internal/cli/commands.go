package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/fusion"
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
// homes/network/cache toggles (the common interactive path). lead names the agent
// being driven (claude/codex/gemini) so it gets the optional consult directive;
// pass "" for raw commands (coop run/shell/login) that aren't an agent session.
func (a *app) runInBox(cmd []string, lead string) (int, error) {
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, ConsultLead: lead,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

func (a *app) cmdRun(args []string) (int, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		args = a.defaultCmd("claude") // bare `coop run` opens claude
	}
	return a.runInBox(args, "") // raw command runner — not an agent session
}

// launchAgent runs a named agent: its autonomous default command, with any extra CLI
// args you pass appended — so `coop claude --continue` keeps coop's autonomy + MCP
// flags and just adds yours. The agents' autonomous flags are global, so this is safe
// even before subcommands (e.g. `coop codex resume --last`). coop's own --consult is
// stripped first so it isn't forwarded to the agent.
func (a *app) launchAgent(tool string, args []string) (int, error) {
	consult, args := extractConsult(args)
	// `coop claude login` reads as "log in to claude", not "prompt claude with the
	// word login" — route it to the sign-in flow like `coop login claude` (honoring
	// `--profile`, e.g. `coop claude login --profile work`). Only the login path reads
	// --profile; an interactive run forwards args untouched so codex's own --profile
	// (a config.toml profile) still reaches codex.
	if len(args) >= 1 && args[0] == "login" {
		profile, _ := extractProfile(args[1:])
		return a.loginTo(tool, profile)
	}
	lead := "" // ConsultLead is set only with --consult, so the directive is opt-in
	if consult {
		lead = tool
	}
	return a.runInBox(append(append([]string{}, a.defaultCmd(tool)...), args...), lead)
}

// extractConsult pulls coop's own --consult flag out of an agent's args (so it is
// not forwarded to the agent CLI) and reports whether it was present. --consult
// opts a normal run into the second-opinion directive — letting the agent consult
// its authenticated peers read-only on hard calls (see box.RunSpec.ConsultLead).
func extractConsult(args []string) (consult bool, rest []string) {
	for _, a := range args {
		if a == "--consult" {
			consult = true
			continue
		}
		rest = append(rest, a)
	}
	return consult, rest
}

// defaultCmd is the agent's autonomous interactive command; an unknown name runs as a
// raw passthrough (so `coop npm test` still works).
func (a *app) defaultCmd(tool string) []string {
	if ag, ok := agents.Get(tool); ok {
		return ag.Interactive(a.cfg)
	}
	return []string{tool}
}

func (a *app) cmdLogin(args []string) (int, error) {
	profile, rest := extractProfile(args)
	tool := agents.Default()
	if len(rest) > 0 {
		tool = rest[0]
	}
	return a.loginTo(tool, profile)
}

// extractProfile pulls coop's own `--profile <name>` (or `--profile=<name>`) flag out of
// args, returning the chosen credential profile (config.DefaultProfile if absent) and the
// remaining args. It lets a login target one of several stored subscriptions.
func extractProfile(args []string) (profile string, rest []string) {
	profile = config.DefaultProfile
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--profile" && i+1 < len(args):
			profile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--profile="):
			profile = strings.TrimPrefix(args[i], "--profile=")
		default:
			rest = append(rest, args[i])
		}
	}
	return profile, rest
}

// loginTo runs an agent's sign-in flow in the box; its token persists in the agent's
// config dir for the chosen profile. Shared by `coop login [agent] [--profile p]` and
// `coop <agent> login [--profile p]`.
func (a *app) loginTo(tool, profile string) (int, error) {
	ag, ok := agents.Get(tool)
	if !ok {
		return 2, fmt.Errorf("unknown agent %q — use %s", tool, strings.Join(agents.Names(), ", "))
	}
	if profile == "" {
		profile = config.DefaultProfile
	}
	// A named profile needs the profiles/ layout; EnsureProfilesDir also migrates a
	// pre-existing flat login into profiles/default the first time, so it isn't orphaned.
	if profile != config.DefaultProfile {
		if err := box.EnsureProfilesDir(a.cfg, tool); err != nil {
			return -1, err
		}
	}
	a.cfg.SetActiveProfile(tool, profile)
	where := ""
	if profile != config.DefaultProfile {
		where = fmt.Sprintf(" (profile %s)", profile)
	}
	ui.Info("logging in to %s%s — credentials persist in %s/", tool, where, a.cfg.AgentDir(tool))
	return a.runInBox(ag.Login(a.cfg), "") // logging in, not an agent session
}

// acpCommand maps an agent to its ACP adapter command inside the box.
func acpCommand(tool string) ([]string, bool) {
	if ag, ok := agents.Get(tool); ok {
		return ag.ACP(), true
	}
	return nil, false
}

// cmdACP runs the box as an ACP agent over stdio: the repo mounts at its real
// host path (so the editor's absolute paths resolve, and the session history
// matches `coop`/`coop loop` — see resolveWorkdir) and no tty is allocated. The
// explicit Workdir forces the real path even if COOP_WORKDIR is set.
//
// `coop acp fusion [governor]` fronts the governor's adapter as a normal ACP
// agent (so Zed drives it like any other) but wired for fusion: it consults its
// peers read-only and synthesizes (see cmdFusion). Add one Zed agent_servers
// entry per governor to switch which model leads.
func (a *app) cmdACP(args []string) (int, error) {
	consult, args := extractConsult(args)
	tool := agents.Default()
	if len(args) > 0 {
		tool = args[0]
	}
	governor := ""
	if tool == "fusion" {
		governor = a.cfg.FusionGovernor
		if len(args) > 1 {
			governor = args[1]
		}
		if !fusion.Valid(governor, agents.Names()) {
			return 2, fmt.Errorf("unknown governor %q — use claude, codex, or gemini", governor)
		}
		tool = governor
	}
	cmd, ok := acpCommand(tool)
	if !ok {
		return 2, errors.New("usage: coop acp [claude|codex|gemini|fusion [governor]]")
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	lead := "" // --consult opts into the second-opinion directive (a no-op under fusion)
	if consult {
		lead = tool
	}
	// ACP speaks to an editor over stdio, not a human, so run quiet: Quiet drops coop's
	// own progress lines, and COOP_QUIET tells the box to provision the toolchain silently.
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Workdir: repo, Cmd: cmd, ForceNoTTY: true,
		FusionGovernor: governor, ConsultLead: lead, Quiet: true,
		ExtraArgs: []string{"-e", "COOP_QUIET=1"},
		Homes:     a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

// cmdFusion runs a council: the governor agent (a leading `claude|codex|gemini`, else
// COOP_FUSION_GOVERNOR) runs normally — it edits and does the real work — while a fusion
// instruction injected into its instruction file tells it to consult its two peers
// read-only and synthesize. It behaves like `coop <agent>`: `coop fusion claude` opens
// claude interactively; trailing `<args>` pass through to the governor.
func (a *app) cmdFusion(args []string) (int, error) {
	governor, rest := a.parseGovernor(args)
	if !fusion.Valid(governor, agents.Names()) {
		return 2, fmt.Errorf("unknown governor %q — use claude, codex, or gemini", governor)
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	// The governor's autonomous default command, plus any extra args you pass through.
	cmd := append(append([]string{}, a.defaultCmd(governor)...), rest...)
	ui.Info("fusion: %s governs; peers %s consulted read-only", governor,
		strings.Join(fusion.Peers(governor, agents.Names()), " + "))
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, FusionGovernor: governor,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

// parseGovernor takes a leading `claude|codex|gemini` token as the governor (else
// COOP_FUSION_GOVERNOR); everything else passes through to the governor.
func (a *app) parseGovernor(args []string) (governor string, rest []string) {
	governor = a.cfg.FusionGovernor
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			return governor, append(rest, args[i+1:]...) // everything after passes through
		case len(rest) == 0 && agents.Valid(args[i]):
			// A leading agent name is the governor: `coop fusion claude` (matches
			// `coop acp fusion claude`); otherwise the default / COOP_FUSION_GOVERNOR.
			governor = args[i]
		default:
			rest = append(rest, args[i])
		}
	}
	return governor, rest
}

func (a *app) cmdBuild(args []string) (int, error) {
	if err := rejectArgs("build", args); err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if err := box.Build(a.rt, a.cfg, repo, false); err != nil {
		return -1, err
	}
	if n := a.rt.KillByLabel("coop", "box"); n > 0 {
		ui.Info("killed %d running container(s) — new sessions will use the updated image", n)
	}
	return 0, nil
}

// cmdUpdate force-rebuilds the box image (--pull --no-cache) so the base image
// and the npm-installed agent CLIs + ACP adapters refresh to their latest, then
// reports the versions it landed on. ACP/agent packages ship features often.
func (a *app) cmdUpdate(args []string) (int, error) {
	if err := rejectArgs("update", args); err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ui.Info("updating the box: newer base image + latest agent CLIs and ACP adapters")
	if err := box.Build(a.rt, a.cfg, repo, true); err != nil {
		return -1, err
	}
	if n := a.rt.KillByLabel("coop", "box"); n > 0 {
		ui.Info("killed %d running container(s) — new sessions use the updated image", n)
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	ui.Info("installed versions:")
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Batch: true, Quiet: true,
		Cmd:       []string{"sh", "-c", "npm ls -g --depth=0 2>/dev/null | grep -iE 'claude|codex|gemini|acp' || true"},
		ExtraArgs: []string{"-e", "COOP_NO_ASDF=1"}, // skip the .tool-versions provision for a quick version print
	})
	return 0, nil
}

func (a *app) cmdUp(args []string) (int, error) {
	if err := rejectArgs("up", args); err != nil {
		return 2, err
	}
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

// loopAgent picks the model for `coop loop [claude|codex|gemini]` (default claude),
// erroring on any unexpected token.
func loopAgent(args []string) (string, error) {
	agent := agents.Default()
	for _, x := range args {
		if !agents.Valid(x) {
			return "", fmt.Errorf("coop loop: unexpected argument %q (usage: coop loop [%s])", x, strings.Join(agents.Names(), "|"))
		}
		agent = x
	}
	return agent, nil
}

func (a *app) cmdLoop(args []string) (int, error) {
	flags, rest := extractTasksFlags(args)
	agent, debugOnFail, err := parseLoopArgs(rest)
	if err != nil {
		return 2, err
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	queues, err := taskQueues(a.cfg, repo, flags)
	if err != nil {
		return 2, err
	}
	pool, err := buildPool(a.cfg, repo, agent)
	if err != nil {
		return -1, err
	}
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	return a.loop(repo, img, agent, pool, queues, nil, debugOnFail)
}

// parseLoopArgs pulls the --debug-on-fail flag (alias --debug) out of `coop loop` args;
// what remains must be at most one agent name.
func parseLoopArgs(args []string) (agent string, debugOnFail bool, err error) {
	var rest []string
	for _, x := range args {
		switch x {
		case "--debug-on-fail", "--debug":
			debugOnFail = true
		default:
			rest = append(rest, x)
		}
	}
	agent, err = loopAgent(rest)
	return agent, debugOnFail, err
}

// loopWorkPrompt and loopAuditPrompt name the queue file(s) the iteration works. With a
// single .agent/TASKS.md they read exactly as before; with several (a monorepo's
// per-component queues) they list them so the agent works the union.
func loopWorkPrompt(queues []string) string {
	return fmt.Sprintf("Read %s and AGENTS.md, then work the next unchecked items per the protocol: claim with [w], do it, run the gate, commit, log it, flip to [x]. Do not stop while a [ ] remains.", strings.Join(queues, ", "))
}

func loopAuditPrompt(queues []string) string {
	return fmt.Sprintf("Audit: for every [x] in %s, verify its gate passes and a commit implementing it exists in the git log. Reopen any that fail — flip [x] back to [ ] and note what is missing. Do not fix anything yourself.", strings.Join(queues, ", "))
}

// loop works .agent/TASKS.md unattended until no "[ ]" remains, then (unless a
// custom COOP_LOOP_CMD is set) runs a one-shot audit pass over the results. A
// model rate/usage limit is not a failure: the loop waits for the reset — parsed
// from the agent's own output when possible — and resumes the same iteration, so
// a long run survives hitting the limit and continues once it clears.
func (a *app) loop(repo, img, agent string, pool *profilePool, queues []string, sink io.Writer, debugOnFail bool) (int, error) {
	hosts := make([]string, len(queues)) // the queues' absolute host paths
	for i, q := range queues {
		hosts[i] = filepath.Join(repo, q)
	}
	anyTodo := func() bool {
		for _, h := range hosts {
			if queueHasTodo(h) {
				return true
			}
		}
		return false
	}
	if !slices.ContainsFunc(hosts, fileExists) {
		return -1, fmt.Errorf("no task file found (%s) — run 'coop init' or pass --tasks", strings.Join(queues, ", "))
	}
	if !box.ImageExists(a.rt, img) {
		return -1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	custom := a.cfg.LoopCmd
	work, audit := loopWorkPrompt(queues), loopAuditPrompt(queues)
	// iterCmd builds one iteration's command: a raw COOP_LOOP_CMD override if set,
	// otherwise the chosen agent's headless form carrying the work/audit prompt.
	iterCmd := func(prompt string) []string {
		if len(custom) > 0 {
			return custom
		}
		return a.agentLoopCmd(agent, prompt)
	}
	label := strings.Join(queues, ", ")
	c0, _ := queueProgress(hosts)
	if len(custom) == 0 {
		ui.Info("starting unattended loop on %s with %s — %d/%d done (Ctrl-C to stop)", label, agent, c0.Done, c0.total())
	} else {
		ui.Info("starting unattended loop on %s — %d/%d done (Ctrl-C to stop)", label, c0.Done, c0.total())
	}
	fails, waits, completed := 0, 0, 0
	for n := 1; anyTodo(); {
		// Run this iteration on the pool's active subscription; the mount and the agent
		// command both resolve cfg.AgentDir, so pointing cfg here is all it takes.
		a.cfg.SetActiveProfile(agent, pool.active())
		// Surface queue progress + the task being worked, so a long run shows movement
		// instead of a bare counter (the same scanTasks `coop status`/`coop tasks` use).
		c, active := queueProgress(hosts)
		banner := progressBanner(n, c, active)
		if pool.rotates() {
			banner += fmt.Sprintf(" · profile %s", pool.active())
		}
		ui.Info("%s", banner)
		code, out, err := a.runIteration(repo, img, iterCmd(work), sink)
		action, wait, resetAt := decideIteration(code, err, out, time.Now(), &fails, &waits)
		// --debug-on-fail: on a non-rate-limit failure, open an interactive box shell
		// (same repo/image) to inspect, then retry — instead of the auto-retry/stop.
		if (action == actRetry || action == actStop) && debugOnFail && ui.IsTerminal(os.Stdin) {
			ui.Info("iteration failed — opening a debug shell in the box (exit it to retry; Ctrl-C to stop)")
			a.debugShell(repo, img)
			fails = 0 // the developer intervened; don't count this toward the stop cap
			continue
		}
		switch action {
		case actContinue:
			completed++
			n++
		case actWait:
			// A rate/usage limit is expected on long runs. With more than one profile in
			// the pool, switch to another subscription and retry immediately; otherwise wait
			// for the reset. Either way the same iteration is retried, not burned.
			if pool.rotates() {
				a.rotateOnLimit(agent, pool, resetAt, &waits)
			} else {
				sleepForLimit(wait, resetAt)
			}
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
		_, _, _ = a.runIteration(repo, img, iterCmd(audit), sink)
	}
	if anyTodo() {
		ui.Info("audit reopened items — run 'coop loop' again")
	} else {
		cf, _ := queueProgress(hosts)
		msg := fmt.Sprintf("✓ queue verified done — %d/%d", cf.Done, cf.total())
		if completed > 0 {
			msg += fmt.Sprintf(" in %d iterations", completed)
		}
		fmt.Fprintln(os.Stderr, ui.Bold(ui.Green(msg)))
	}
	return 0, nil
}

// debugShell opens an interactive shell in the box against the same repo/image as the
// loop iteration, so --debug-on-fail can inspect the failed state. The box is disposable
// per iteration, so this is a fresh shell in the same context, not the failed container.
func (a *app) debugShell(repo, img string) {
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: []string{a.cfg.Shell},
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

const (
	heartbeatTick = 15 * time.Second // how often to check whether the agent has gone quiet
	heartbeatIdle = 30 * time.Second // silence longer than this prints a "still working" tick
)

// runIteration runs one boxed command in batch mode, teeing its output to the
// terminal while capturing the tail so a rate-limit notice can be detected.
func (a *app) runIteration(repo, img string, cmd []string, sink io.Writer) (code int, output string, err error) {
	tail := &tailWriter{max: 64 << 10, last: time.Now()}
	outW := []io.Writer{os.Stdout, tail}
	errW := []io.Writer{os.Stderr, tail}
	if sink != nil { // fork loops also capture to ../<repo>-forks/.coop/<name>.log
		outW = append(outW, sink)
		errW = append(errW, sink)
	}
	// On a TTY, tick a "still working" heartbeat whenever the agent goes quiet, so a long
	// silent iteration (claude -p prints only its final message) never looks hung.
	if ui.IsTerminal(os.Stderr) {
		stop := make(chan struct{})
		defer close(stop)
		go heartbeat(tail, time.Now(), stop)
	}
	code, err = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Batch: true,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
		Stdout: io.MultiWriter(outW...),
		Stderr: io.MultiWriter(errW...),
	})
	return code, tail.String(), err
}

// heartbeat prints a dimmed "still working" line whenever the agent has been silent for
// longer than heartbeatIdle, so a quiet iteration shows liveness instead of a frozen
// screen. It returns when the iteration closes stop.
func heartbeat(tail *tailWriter, start time.Time, stop <-chan struct{}) {
	t := time.NewTicker(heartbeatTick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			if now.Sub(tail.lastWrite()) >= heartbeatIdle {
				ui.Info("…still working (%s elapsed)", now.Sub(start).Round(time.Second))
			}
		}
	}
}
