package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndrewDryga/coop/internal/acpproxy"
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
// homes/network/cache toggles (the common interactive path). agent names the agent
// being driven (claude/codex/gemini) so its credentials are mounted and, with consult,
// it gets the second-opinion directive plus its authenticated peers' credentials. Pass
// "" for raw commands (coop run/shell) that aren't an agent session — they mount no
// agent credentials.
func (a *app) runInBox(cmd []string, agent string, consult bool) (int, error) {
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	lead := ""
	if consult {
		lead = agent
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, ConsultLead: lead,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

func (a *app) cmdRun(args []string) (int, error) {
	// Intercept the meta cases before entering the box. We can't lean on the dispatch's --help
	// handling here: it's `--`-blind, so it would mistake `coop run -- --help` (run --help in the
	// box) for a help request. Honor -- ourselves.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:] // everything after -- runs verbatim
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printCommandHelp(runHelp) // not forwarded to the box, where it would exec `--help` and crash
		return 0, nil
	}
	if len(args) == 0 {
		// `coop run` runs a raw command; it does not default to an agent (use `coop claude`).
		return 2, errors.New("usage: coop run -- <cmd...>")
	}
	return a.runInBox(args, "", false) // raw command runner — not an agent session
}

// launchAgent runs a named agent: its autonomous default command, with any extra CLI
// args you pass appended — so `coop claude --continue` keeps coop's autonomy + MCP
// flags and just adds yours. The agents' autonomous flags are global, so this is safe
// even before subcommands (e.g. `coop codex resume --last`). coop's own --consult and
// --profile are stripped first so they aren't forwarded to the agent.
func (a *app) launchAgent(tool string, args []string) (int, error) {
	consult, args := extractConsult(args)
	// `coop claude login` reads as "log in to claude", not "prompt claude with the
	// word login" — route it to the sign-in flow like `coop login claude` (honoring
	// `--profile`, e.g. `coop claude login --profile work`).
	if len(args) >= 1 && args[0] == "login" {
		profile, rest, err := extractProfile(args[1:])
		if err != nil {
			return 2, err
		}
		if len(rest) > 0 {
			return 2, fmt.Errorf("unexpected argument %q after 'coop %s login'", rest[0], tool)
		}
		return a.loginTo(tool, profile)
	}
	// `coop claude --profile work` runs on a chosen credential profile (one subscription);
	// coop consumes the flag so it isn't forwarded. It's read only before a `--`, so an agent's
	// own --profile (e.g. codex's config.toml profile) is still reachable as
	// `coop codex -- --profile <name>`.
	profile, args, err := extractRunProfile(args)
	if err != nil {
		return 2, err
	}
	if err := a.selectRunProfile(tool, profile); err != nil {
		return 2, err
	}
	return a.runInBox(append(append([]string{}, a.defaultCmd(tool)...), args...), tool, consult)
}

// selectRunProfile points cfg at the credential profile chosen with --profile for a run of tool
// (a no-op when profile is ""). It requires the profile to already exist — a typo otherwise
// silently creates an empty husk dir (box.Run pre-creates the active profile), the very clutter
// `coop profiles rm` cleans up — and notes (without blocking) one that exists but isn't signed in.
// Shared by every agent-launch path: launchAgent, cmdFusion, cmdACP.
func (a *app) selectRunProfile(tool, profile string) error {
	if profile == "" {
		return nil
	}
	if !slices.Contains(a.cfg.Profiles(tool), profile) {
		return fmt.Errorf("%s has no profile %q — sign in first: coop login %s --profile %s", tool, profile, tool, profile)
	}
	if !box.ProfileAuthed(a.cfg, tool, profile) {
		ui.Info("note: %s profile %q isn't signed in — run: coop login %s --profile %s", tool, profile, tool, profile)
	}
	a.cfg.SetActiveProfile(tool, profile)
	return nil
}

// extractConsult pulls coop's own --consult flag out of an agent's args (so it is
// not forwarded to the agent CLI) and reports whether it was present. --consult
// opts a normal run into the second-opinion directive — letting the agent consult
// its authenticated peers read-only on hard calls (see box.RunSpec.ConsultLead).
func extractConsult(args []string) (consult bool, rest []string) {
	for i, a := range args {
		if a == "--" { // everything after -- is the agent's own args, verbatim
			return consult, append(rest, args[i:]...)
		}
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
	profile, rest, err := extractProfile(args)
	if err != nil {
		return 2, err
	}
	// The agent is required — bare `coop login` must not silently default to one (it would open a
	// browser and block); name it explicitly, like the help shows. A stray extra arg is a typo,
	// not a second target, so reject it rather than silently ignore.
	if len(rest) == 0 {
		return 2, fmt.Errorf("usage: coop login <%s> [--profile <name>]", strings.Join(agents.Names(), "|"))
	}
	if len(rest) > 1 {
		return 2, fmt.Errorf("unexpected argument %q (usage: coop login <%s> [--profile <name>])", rest[1], strings.Join(agents.Names(), "|"))
	}
	return a.loginTo(rest[0], profile)
}

// flagValue extracts the value of a value-bearing flag at args[i], handling both
// `--flag value` and `--flag=value`. ok reports whether args[i] is this flag at all;
// consumed is how many tokens it spans (1 or 2). It errors when the value is missing — the
// flag is the last token, its value is another flag (a leading '-'), or `--flag=` is empty —
// so a typo'd flag fails loudly instead of silently falling back to a default. Values for
// coop's own flags never start with '-', so treating a '-' next token as "missing" is safe.
func flagValue(args []string, i int, flag string) (val string, consumed int, ok bool, err error) {
	switch a := args[i]; {
	case a == flag:
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
			return "", 0, true, fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], 2, true, nil
	case strings.HasPrefix(a, flag+"="):
		if v := strings.TrimPrefix(a, flag+"="); v != "" {
			return v, 1, true, nil
		}
		return "", 0, true, fmt.Errorf("%s needs a value", flag)
	}
	return "", 0, false, nil
}

// extractProfile pulls coop's own `--profile <name>` (or `--profile=<name>`) flag out of
// args, returning the chosen credential profile (config.DefaultProfile if absent) and the
// remaining args. It lets a login target one of several stored subscriptions. A `--profile`
// with no value is an error, not a silent fall-back to the default.
func extractProfile(args []string) (profile string, rest []string, err error) {
	profile = config.DefaultProfile
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--profile"); ok {
			if e != nil {
				return "", nil, e
			}
			profile = v
			i += n - 1
			continue
		}
		rest = append(rest, args[i])
	}
	return profile, rest, nil
}

// extractRunProfile pulls coop's own --profile <name> (or --profile=<name>) out of an agent
// RUN's args, returning the chosen credential profile ("" if none) and the remaining args.
// Unlike extractProfile (login), it stops at a "--" separator and forwards everything after it
// verbatim — so an agent's own --profile (e.g. codex's config.toml profile) is still reachable
// as `coop codex -- --profile <name>`. A --profile with no value is an error, like login's.
func extractRunProfile(args []string) (profile string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return profile, append(rest, args[i:]...), nil
		}
		if v, n, ok, e := flagValue(args, i, "--profile"); ok {
			if e != nil {
				return "", nil, e
			}
			profile = v
			i += n - 1
			continue
		}
		rest = append(rest, args[i])
	}
	return profile, rest, nil
}

// validProfileName keeps a credential profile name to a single safe path segment, so a name passed
// to --profile can't traverse or collide outside the agent's profiles/ vault (no '/', '\', '..',
// '.', empty, or leading '-'). Login is the path that CREATES the dir from the name, so it's the
// gate; runs/select/rm/default already require an existing profile.
func validProfileName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
}

// loginTo runs an agent's sign-in flow in the box; its token persists in the agent's
// config dir for the chosen profile. Shared by `coop login [agent] [--profile p]` and
// `coop <agent> login [--profile p]`.
func (a *app) loginTo(tool, profile string) (int, error) {
	ag, ok := agents.Get(tool)
	if !ok {
		return 2, unknownErr("agent", tool, agents.Names())
	}
	if profile == "" {
		profile = config.DefaultProfile
	}
	// Validate the profile name (a static arg) before the environment checks below, so a traversal
	// name like "../../x" can't escape the vault and fails the same way piped or at a tty.
	if !validProfileName(profile) {
		return 2, fmt.Errorf("invalid profile name %q — use a single segment (no '/', '..', or leading '-')", profile)
	}
	// Login is interactive — it prompts for a paste code (reading the tty directly). Refuse a
	// non-terminal stdin up front rather than blocking forever on a piped/redirected run.
	if !ui.IsTerminal(os.Stdin) {
		return 2, errors.New("login needs an interactive terminal (it prompts for a paste code) — run it directly")
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
	return a.runInBox(ag.Login(a.cfg), tool, false) // mounts only the agent being logged in to
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
	// --supervise keeps the editor's connection alive across a container restart by
	// running the normal `coop acp` as a child and proxying ACP through acpproxy. The
	// COOP_ACP_INNER guard makes the child run the box directly (no recursion).
	supervise, args := extractSupervise(args)
	if supervise && os.Getenv("COOP_ACP_INNER") == "" {
		return a.cmdACPSupervise(args)
	}
	consult, args := extractConsult(args)
	// --profile pins this ACP session to one credential profile — so an editor can point a
	// "claude (work)" agent_servers entry at ["acp","claude","--profile","work"]. Read before the
	// tool token; an agent's own --profile still passes through after a `--`.
	profile, args, err := extractRunProfile(args)
	if err != nil {
		return 2, err
	}
	tool := agents.Default()
	consumed := 0 // positional tokens accounted for (the agent, plus a governor under fusion)
	if len(args) > 0 {
		tool = args[0]
		consumed = 1
	}
	governor := ""
	if tool == "fusion" {
		governor = a.cfg.FusionGovernor
		if len(args) > 1 {
			governor = args[1]
			consumed = 2
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
	// Reject leftover tokens rather than silently ignore them (loop/fork do the same) — the ACP
	// adapter takes no extra args, so `coop acp claude foo`/`--nope` is a mistake worth surfacing.
	if leftover := args[consumed:]; len(leftover) > 0 {
		return 2, fmt.Errorf("coop acp: unexpected argument %q (usage: coop acp [claude|codex|gemini|fusion [governor]] [--profile p])", leftover[0])
	}
	if err := a.selectRunProfile(tool, profile); err != nil {
		return 2, err
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
	extra := []string{"-e", "COOP_QUIET=1"}
	// Under a supervisor, give the box a deterministic identity: --cidfile lets the supervisor
	// tear it down by id even before its labels are queryable (see cmdACPSupervise's stop()).
	if cid := os.Getenv("COOP_ACP_CIDFILE"); cid != "" {
		extra = append(extra, "--cidfile", cid)
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		// A supervisor (which reconnects the box) passes COOP_ACP_SUPERVISOR; that tags
		// the box so build/update can restart it and the supervisor can kill exactly it.
		Image: img, Repo: repo, Workdir: repo, Cmd: cmd, ForceNoTTY: true, Agent: tool,
		SupervisorID:   os.Getenv("COOP_ACP_SUPERVISOR"),
		FusionGovernor: governor, ConsultLead: lead, Quiet: true,
		ExtraArgs: extra,
		Homes:     a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

func extractSupervise(args []string) (supervise bool, rest []string) {
	for i, a := range args {
		if a == "--" { // everything after -- is the inner agent's own args, verbatim
			return supervise, append(rest, args[i:]...)
		}
		if a == "--supervise" {
			supervise = true
			continue
		}
		rest = append(rest, a)
	}
	return supervise, rest
}

// cmdACPSupervise serves the editor on stdio and runs the real `coop acp <rest>` as a
// child (COOP_ACP_INNER set so the child runs the box, not another supervisor). When
// the child's container dies, acpproxy starts a new child and replays the ACP
// handshake, so the editor never sees a disconnect (see internal/acpproxy).
func (a *app) cmdACPSupervise(rest []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, fmt.Errorf("acp --supervise: %w", err)
	}
	inner := append([]string{"acp"}, rest...)
	// A per-supervisor id, stamped on this supervisor's boxes (coop.sup=<id>) so it can
	// kill exactly its own box(es) on teardown — not other agents' supervised boxes.
	idbuf := make([]byte, 8)
	if _, err := rand.Read(idbuf); err != nil {
		return 1, err
	}
	superID := hex.EncodeToString(idbuf)

	factory := func(_ context.Context) (*acpproxy.Child, error) {
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			inR.Close()
			inW.Close()
			return nil, err
		}
		// A deterministic container identity (a per-generation --cidfile) so teardown can remove
		// the box even before its labels are queryable — closing the startup race where Stop fires
		// after `docker run` begins but before the container is labelled. docker writes the id to
		// this file the moment the container is created. A fresh dir per generation avoids the
		// "name already in use" hazard across swaps, and `--cidfile` requires the path not to exist.
		cidDir, cidPath := "", ""
		env := append(os.Environ(), "COOP_ACP_INNER=1", "COOP_ACP_SUPERVISOR="+superID)
		if a.rt.SupportsCIDFile() {
			if d, derr := os.MkdirTemp("", "coop-acp-cid-"); derr == nil {
				cidDir = d
				cidPath = filepath.Join(d, "cid")
				env = append(env, "COOP_ACP_CIDFILE="+cidPath)
			}
		}
		cmd := exec.Command(self, inner...)
		cmd.Env = env
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, os.Stderr
		// Own process group: a plain Process.Kill() reaps only the inner `coop` and orphans its
		// `docker run` grandchild; killing the whole group (-pgid) reaches the run client too.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			inR.Close()
			inW.Close()
			outR.Close()
			outW.Close()
			if cidDir != "" {
				os.RemoveAll(cidDir)
			}
			return nil, err
		}
		inR.Close()  // the child holds the read end now
		outW.Close() // ...and the write end; outR sees EOF when the child exits
		pid := cmd.Process.Pid
		go func() { _ = cmd.Wait() }()
		stop := func() {
			// Remove the box by its deterministic cidfile id first — works even mid-startup, before
			// labels exist; `rm -f` stops it too. Then kill the whole process group (inner coop +
			// its run client), the label backstop for any box that did get labelled, and the pipes.
			if cidPath != "" {
				if cid, rerr := os.ReadFile(cidPath); rerr == nil {
					a.rt.RemoveContainer(strings.TrimSpace(string(cid)))
				}
			}
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			a.rt.KillByLabel("coop.sup", superID)
			inW.Close()
			outR.Close()
			if cidDir != "" {
				os.RemoveAll(cidDir)
			}
		}
		return &acpproxy.Child{In: inW, Out: outR, Stop: stop}, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := acpproxy.Run(ctx, os.Stdin, os.Stdout, factory); err != nil && !errors.Is(err, context.Canceled) {
		return 1, err
	}
	return 0, nil
}

// cmdFusion runs a council: the governor agent (a leading `claude|codex|gemini`, else
// COOP_FUSION_GOVERNOR) runs normally — it edits and does the real work — while a fusion
// instruction injected into its instruction file tells it to consult its two peers
// read-only and synthesize. It behaves like `coop <agent>`: `coop fusion claude` opens
// claude interactively; trailing `<args>` pass through to the governor.
func (a *app) cmdFusion(args []string) (int, error) {
	// --profile picks the governor's credential profile, like a plain `coop <agent>` run; read it
	// before governor parsing so the governor's own --profile is still reachable after a `--`.
	profile, args, err := extractRunProfile(args)
	if err != nil {
		return 2, err
	}
	governor, rest := a.parseGovernor(args)
	if !fusion.Valid(governor, agents.Names()) {
		return 2, fmt.Errorf("unknown governor %q — use claude, codex, or gemini", governor)
	}
	if err := a.selectRunProfile(governor, profile); err != nil {
		return 2, err
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
		Image: img, Repo: repo, Cmd: cmd, Agent: governor, FusionGovernor: governor,
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
	a.recycleBoxes()
	return 0, nil
}

// recycleBoxes restarts supervised boxes after a rebuild so they reconnect on the new
// image — a coop acp supervisor replays the ACP handshake, so the editor doesn't
// notice. New runs use the fresh image anyway (containers are anonymous). Other
// running boxes (loops, forks, an un-supervised session) are left alone; SIGKILLing
// them would lose work, and they pick up the new image when they next start.
func (a *app) recycleBoxes() {
	total := a.rt.CountByLabel("coop", "box")
	supervised := a.rt.CountByLabel("coop.supervised", "1")
	if n := a.rt.KillByLabel("coop.supervised", "1"); n > 0 {
		ui.Info("restarted %d supervised session(s) onto the new image", n)
	}
	if others := total - supervised; others > 0 {
		ui.Info("%d other running container(s) keep the old image until they restart", others)
	}
}

// cmdUpdate self-updates the coop binary to the latest release, then force-rebuilds
// the box image (--pull --no-cache) so the base image and the npm-installed agent CLIs
// + ACP adapters refresh to their latest, then reports the versions it landed on.
// --self-only does just the binary; --box-only does just the image (the old behavior).
func (a *app) cmdUpdate(args []string) (int, error) {
	selfOnly, boxOnly, err := parseUpdateFlags(args)
	if err != nil {
		return 2, err
	}

	// Self-update the binary first. A failed *check* (offline/rate limit) is soft and
	// must not block the box rebuild; a write or install failure is loud and exits
	// non-zero, but the box still rebuilds (it's independent) so the run isn't wasted.
	selfFailed := false
	if !boxOnly {
		if _, err := selfUpdate(os.Stdout); err != nil {
			var ce checkError
			switch {
			case selfOnly:
				return -1, err
			case errors.As(err, &ce):
				ui.Info("coop self-update: couldn't check for a newer release (%v) — continuing with the box", err)
			default:
				ui.Error("coop self-update failed: %v", err)
				selfFailed = true
			}
		}
		if selfOnly {
			return 0, nil
		}
	}

	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ui.Info("updating the box: newer base image + latest agent CLIs and ACP adapters")
	if err := box.Build(a.rt, a.cfg, repo, true); err != nil {
		return -1, err
	}
	a.recycleBoxes()
	img := box.ImageForRepo(repo, a.cfg.BaseImage, a.cfg.ImageOverride)
	ui.Info("installed versions:")
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Batch: true, Quiet: true,
		Cmd:       []string{"sh", "-c", "npm ls -g --depth=0 2>/dev/null | grep -iE 'claude|codex|gemini|acp' || true"},
		ExtraArgs: []string{"-e", "COOP_NO_ASDF=1"}, // skip the .tool-versions provision for a quick version print
	})
	if selfFailed {
		return 1, nil // box updated, binary didn't — signal the partial failure
	}
	return 0, nil
}

// parseUpdateFlags parses `coop update`'s own flags: --self-only (just the binary) and
// --box-only (just the image), which are mutually exclusive.
func parseUpdateFlags(args []string) (selfOnly, boxOnly bool, err error) {
	for _, x := range args {
		switch x {
		case "--self-only":
			selfOnly = true
		case "--box-only":
			boxOnly = true
		default:
			return false, false, fmt.Errorf("update: unknown flag %q (usage: coop update [--self-only|--box-only])", x)
		}
	}
	if selfOnly && boxOnly {
		return false, false, errors.New("update: --self-only and --box-only are mutually exclusive")
	}
	return selfOnly, boxOnly, nil
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
	if err := box.EnsureServices(a.rt, repo, os.Stdout, os.Stderr); err != nil {
		return -1, err
	}
	ui.Info("up on network %s_default — the box reaches them by name (db, redis, ...)", proj)
	return 0, nil
}

func (a *app) cmdDown(args []string) (int, error) {
	// Validate flags before any runtime/compose work, so a typo fails clearly here instead of
	// later as an unrelated "no compose.agent.yml" — `coop down` takes only -v/--volumes.
	volumes := false
	for _, x := range args {
		if x != "-v" && x != "--volumes" {
			return 2, fmt.Errorf("unknown flag %q — coop down takes only -v/--volumes", x)
		}
		volumes = true
	}
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
	if volumes {
		cargs = append(cargs, "--volumes")
	}
	return a.rt.Run(os.Stdin, os.Stdout, os.Stderr, cargs...)
}

func (a *app) cmdInit(args []string) (int, error) {
	stack := ""
	var services []string
	servicesSet := false
	for i := 0; i < len(args); i++ {
		if v, n, ok, e := flagValue(args, i, "--stack"); ok {
			if e != nil {
				return 2, e
			}
			stack = v
			i += n - 1
			continue
		}
		if v, n, ok, e := flagValue(args, i, "--services"); ok {
			if e != nil {
				return 2, e
			}
			services, servicesSet = parseServices(v), true
			i += n - 1
			continue
		}
		// An unknown token is a typo — error before doing any scaffold work, rather than
		// silently ignoring it and acting as if a flag were never passed.
		return 2, unknownErr("init flag", args[i], []string{"--stack", "--services"})
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	// Detect the repo's stack(s) for the commit gate; if nothing's detected and we're at a
	// terminal, ask rather than guess — coop never imposes a check the repo doesn't use.
	langs := scaffold.DetectStacks(repo)
	if len(langs) == 0 && ui.IsTerminal(os.Stdin) {
		langs = promptGateLangs(os.Stdin)
	}
	// Sibling services (db/redis) are opt-in — coop doesn't add a compose file a project may
	// not want. Ask at a terminal unless --services already said.
	if !servicesSet && ui.IsTerminal(os.Stdin) {
		services = promptServices(os.Stdin)
	}
	if err := scaffold.Init(repo, stack, langs); err != nil {
		return 0, err
	}
	if err := scaffold.WriteCompose(repo, services); err != nil {
		return 0, err
	}
	if err := a.writeMCPStub(); err != nil {
		return 0, err
	}
	// One "coop:" anchor closes the dim per-file log; then the optional Docker-box guidance
	// (only when the repo has its own Docker and no Dockerfile.agent yet); then the actions you
	// need to take next stand on their own — derived from what actually landed, not a fixed script.
	ui.Info("scaffolded into %s", repo)
	scaffold.SuggestDocker(repo)
	ui.Steps(initNextSteps(repo, services)...)
	return 0, nil
}

// initNextSteps is the short list of actions to run after scaffolding, built from what landed: a
// build step when there's a Dockerfile.agent, a `coop up` when sibling services were added, and
// always the edit-then-loop step. Assembled here (not in scaffold) so the whole list is shown in
// one block.
func initNextSteps(repo string, services []string) []string {
	var steps []string
	if fileExists(filepath.Join(repo, "Dockerfile.agent")) {
		steps = append(steps, "review Dockerfile.agent, then `coop build`")
	}
	if len(services) > 0 {
		steps = append(steps, fmt.Sprintf("`coop up`  (starts %s for the box)", strings.Join(services, " + ")))
	}
	steps = append(steps, "`coop tasks add \"<title>\"`, then `coop loop`")
	return steps
}

// writeMCPStub seeds an empty shared mcp.json — coop's one MCP source of truth, translated to
// each agent — at the global config path if absent, so there's an obvious, correctly-shaped file
// to drop servers into. An empty (no-server) file is inactive (see Config.MCPActive), so the stub
// changes no run until you add a server. Never clobbers an existing config.
func (a *app) writeMCPStub() error {
	path := a.cfg.MCPFile
	if path == "" {
		return nil
	}
	if fileExists(path) {
		// init --help promises it seeds mcp.json; on a re-run say we kept it, so the file gets a
		// line like every other scaffolded path instead of silently vanishing from the output.
		ui.Detail("kept existing %s", filepath.Base(path))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte("{\n  \"mcpServers\": {}\n}\n"), 0o600); err != nil {
		return err
	}
	ui.Detail("wrote %s — add MCP servers under \"mcpServers\" to share them with every agent", path)
	return nil
}

// parseServices reads a --services value (comma/space-separated) into known service names,
// dropping blanks, "none", and unknowns.
func parseServices(s string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || r == ' ' }) {
		if tok != "none" && slices.Contains(scaffold.ComposeServices, tok) && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}

// promptServices asks (on a tty) which sibling services to scaffold into compose.agent.yml.
// Blank → none (coop adds no db/redis you didn't ask for); unknown tokens are ignored.
func promptServices(in io.Reader) []string {
	fmt.Fprintf(os.Stderr, "add sibling services for the box? [%s] (space-separated, blank for none): ",
		strings.Join(scaffold.ComposeServices, " "))
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return nil
	}
	var chosen []string
	for _, tok := range strings.Fields(strings.ToLower(sc.Text())) {
		if slices.Contains(scaffold.ComposeServices, tok) && !slices.Contains(chosen, tok) {
			chosen = append(chosen, tok)
		}
	}
	return chosen
}

// promptGateLangs asks (on a tty) which commit format gate(s) to scaffold when coop couldn't
// detect a stack. Blank → none; unknown tokens are ignored. Reads one line from in.
func promptGateLangs(in io.Reader) []string {
	fmt.Fprintf(os.Stderr, "no stack detected — add a commit format gate? [%s] (space-separated, blank for none): ",
		strings.Join(scaffold.GateLangs, " "))
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return nil
	}
	var chosen []string
	for _, tok := range strings.Fields(strings.ToLower(sc.Text())) {
		if slices.Contains(scaffold.GateLangs, tok) && !slices.Contains(chosen, tok) {
			chosen = append(chosen, tok)
		}
	}
	return chosen
}

// loopAgent picks the model for `coop loop [claude|codex|gemini]` (default claude),
// erroring on any unexpected token.
func loopAgent(args []string) (string, error) {
	agent, set := agents.Default(), false
	for _, x := range args {
		if !agents.Valid(x) {
			return "", fmt.Errorf("coop loop: unexpected argument %q (usage: coop loop [%s])", x, strings.Join(agents.Names(), "|"))
		}
		if set {
			return "", fmt.Errorf("coop loop: more than one agent given (%q and %q) — name just one", agent, x)
		}
		agent, set = x, true
	}
	return agent, nil
}

func (a *app) cmdLoop(args []string) (int, error) {
	flags, rest, err := extractTasksFlags(args)
	if err != nil {
		return 2, err
	}
	agent, debugOnFail, preflight, err := parseLoopArgs(rest, a.cfg.Preflight)
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
	return a.loop(repo, img, agent, "", pool, queues, nil, debugOnFail, preflight) // local loop: no fork label
}

// parseLoopArgs pulls the --debug-on-fail (alias --debug) and --preflight/--no-preflight
// flags out of `coop loop` args; what remains must be at most one agent name. preflight
// defaults to def (COOP_PREFLIGHT) and the flags override it.
func parseLoopArgs(args []string, def bool) (agent string, debugOnFail, preflight bool, err error) {
	preflight = def
	var rest []string
	for _, x := range args {
		switch x {
		case "--debug-on-fail", "--debug":
			debugOnFail = true
		case "--preflight":
			preflight = true
		case "--no-preflight":
			preflight = false
		default:
			rest = append(rest, x)
		}
	}
	agent, err = loopAgent(rest)
	return agent, debugOnFail, preflight, err
}

// loopWorkPrompt and loopAuditPrompt name the queue dir(s) the iteration works as ABSOLUTE
// in-box paths (the box's working dir is repo, bind-mounted at its real path). A relative
// ".agent/tasks" resolves fine for claude/codex (cwd-relative), but gemini's read_file rejects
// a relative path — so the queues (and AGENTS.md) are named absolute for every agent. With
// several queues (a monorepo's per-component trees) they're all listed so the agent works the union.
func loopWorkPrompt(repo string, queues []string) string {
	return fmt.Sprintf("Read %s and the task queue(s) %s, then work the queue per the protocol. A task is a folder under a queue dir and its state is its directory (named with a sort prefix): 00_todo/ · 10_in_progress/ · 50_blocked/ · xx_done/. First, if a task is already in 10_in_progress/, a previous attempt was interrupted before it committed: read that task's state.md (its resume note — where it stopped, the next action, traps), then run `git status` and `git diff` to see its uncommitted work, and continue it (or discard the partial work with `git restore`/`git checkout` and redo it if off-track) until done. Otherwise pick the next task in 00_todo/ and claim it with `coop tasks claim <id>` (moves it to 10_in_progress/; add `--tasks <dir>` to target a specific queue). As you work, keep that task's state.md current — a small, overwritten snapshot of the status, what is done, the next action, and any traps — refreshed before each commit and before you pause; append your reasoning to its log.md. Do the work, run the gate, commit (the folder move ships in that commit), then `coop tasks done <id>`. If you hit a one-way-door decision, run `coop tasks block <id>` and fill in its decision.md. Always update state.md as your final step, leaving it reflecting the finished state (do not blank it). Finish the in_progress task before claiming a new one. Do not stop while any 00_todo/ or 10_in_progress/ has a task.",
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues))
}

func loopAuditPrompt(repo string, queues []string) string {
	return fmt.Sprintf("Audit: for every task folder in the xx_done/ of the queue(s) %s, verify its gate passes and a commit implementing it exists in the git log. Reopen any that fail with `coop tasks claim <id>` (moves it back to 10_in_progress/) and note what is missing in its log.md. Do not fix anything yourself.", absJoin(repo, queues))
}

// loopPreflightPrompt is the one-shot cleanup pass run before the work loop when
// --preflight / COOP_PREFLIGHT is set: it resolves answered blockers, but works no task and
// changes no code (these files are git-ignored, so nothing is committed).
func loopPreflightPrompt(repo string, queues []string) string {
	return fmt.Sprintf("Pre-flight cleanup ONLY — do NOT work any task, write code, run the gate, or commit. Read %s and the queue(s) %s. Then, for each task in a 50_blocked/ dir, if its decision.md now has a filled-in Resolution, unblock it with `coop tasks unblock <id>`. Leave every 00_todo/ and 10_in_progress/ task untouched; change no code and make no commits.",
		filepath.Join(repo, "AGENTS.md"), absJoin(repo, queues))
}

// absJoin renders queues (repo-relative) as a comma-separated list of absolute in-box paths.
func absJoin(repo string, queues []string) string {
	abs := make([]string, len(queues))
	for i, q := range queues {
		abs[i] = filepath.Join(repo, q)
	}
	return strings.Join(abs, ", ")
}

// loop works the .agent/tasks queue unattended until nothing actionable remains (todo/ and
// in_progress/ both empty), then (unless a custom COOP_LOOP_CMD is set) runs a one-shot audit
// pass over the results. A model rate/usage limit is not a failure: the loop waits for the
// reset — parsed from the agent's own output when possible — and retries, so a long run
// survives the limit. A task left in in_progress/ by an interrupted iteration is continued (the
// work prompt points the next agent at its uncommitted partial work), not stranded; a
// run that completes no task for maxStalls iterations stops rather than spinning.
// forkName is non-empty only for a detached fork loop — it labels each iteration's box so
// `coop fork stop` can tear the container down by label (see RunSpec.ForkName); the local
// `coop loop` passes "".
func (a *app) loop(repo, img, agent, forkName string, pool *profilePool, queues []string, sink io.Writer, debugOnFail, preflight bool) (int, error) {
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
	// A queue is a directory (.agent/tasks), so check for one with isTaskDir — fileExists is
	// false for a directory and used to reject every folder queue, so the loop never ran.
	if !slices.ContainsFunc(hosts, isTaskDir) {
		return -1, fmt.Errorf("no task queue found (%s) — run 'coop init' or pass --tasks", strings.Join(queues, ", "))
	}
	if !box.ImageExists(a.rt, img) {
		return -1, fmt.Errorf("image %q not built — run 'coop build'", img)
	}
	custom := a.cfg.LoopCmd
	// Claude on a TTY streams its activity as JSON we decode into live lines; other agents, a
	// custom COOP_LOOP_CMD, or a non-terminal (pipe/CI/fork log) keep plain text output. The
	// stream-json marker in the command is what runIteration keys the decoder off.
	stream := agent == "claude" && len(custom) == 0 && ui.IsTerminal(os.Stdout) && ui.IsTerminal(os.Stderr)
	work, audit := loopWorkPrompt(repo, queues), loopAuditPrompt(repo, queues)
	// iterCmd builds one iteration's command: a raw COOP_LOOP_CMD override if set,
	// otherwise the chosen agent's headless form carrying the work/audit prompt.
	iterCmd := func(prompt string) []string {
		if len(custom) > 0 {
			return custom
		}
		cmd := a.agentLoopCmd(agent, prompt)
		if stream {
			cmd = append(cmd, "--output-format", "stream-json", "--verbose")
		}
		return cmd
	}
	// Pre-flight: one best-effort housekeeping pass before working the queue — unblock any
	// task whose decision.md now has a filled-in Resolution. It works no task and deletes
	// nothing: done tasks are pruned only by a human (`coop tasks remove --all-done`), never
	// by an agent. Opt-in (--preflight / COOP_PREFLIGHT); skipped under a custom COOP_LOOP_CMD
	// (not the agent's headless form). Best-effort like the audit pass — a failure never blocks work.
	if preflight && len(custom) == 0 {
		ui.Info("pre-flight: resolving answered blockers")
		_, _, _ = a.runIteration(repo, img, agent, forkName, iterCmd(loopPreflightPrompt(repo, queues)), hosts, sink)
	}
	label := strings.Join(queues, ", ")
	c0, _ := queueProgress(hosts)
	if len(custom) == 0 {
		ui.Info("starting unattended loop on %s with %s — %d/%d done (Ctrl-C to stop)", label, agent, c0.Done, c0.total())
	} else {
		ui.Info("starting unattended loop on %s — %d/%d done (Ctrl-C to stop)", label, c0.Done, c0.total())
	}
	fails, waits, completed, stalls := 0, 0, 0, 0
	settledBaseline := c0.Done + c0.Blocked // "settled" = tasks out of the actionable set (done OR blocked)
	for n := 1; ; {
		// Surface queue progress + the task being worked, so a long run shows movement
		// instead of a bare counter (the same scanTasks `coop status`/`coop tasks` use).
		c, active := queueProgress(hosts)
		// Keep going while anything is actionable — a todo/ task or an in_progress/ one an
		// interrupted iteration left mid-task. Stop only when both are empty (the rest is
		// done/ or blocked/), so a task in_progress when the box died is continued, not stranded.
		if c.Todo+c.Doing == 0 {
			break
		}
		// Run this iteration on the pool's active subscription; the mount and the agent
		// command both resolve cfg.AgentDir, so pointing cfg here is all it takes.
		a.cfg.SetActiveProfile(agent, pool.active())
		banner := progressBanner(n, c, active)
		if pool.rotates() {
			banner += fmt.Sprintf(" · profile %s", pool.active())
		}
		ui.Info("%s", banner)
		code, out, err := a.runIteration(repo, img, agent, forkName, iterCmd(work), hosts, sink)
		action, wait, resetAt := decideIteration(code, err, out, time.Now(), &fails, &waits)
		// --debug-on-fail: on a non-rate-limit failure, open an interactive box shell
		// (same repo/image) to inspect, then retry — instead of the auto-retry/stop.
		if (action == actRetry || action == actStop) && debugOnFail && ui.IsTerminal(os.Stdin) {
			ui.Info("iteration failed — opening a debug shell in the box (exit it to retry; Ctrl-C to stop)")
			a.debugShell(repo, img, agent)
			fails = 0 // the developer intervened; don't count this toward the stop cap
			continue
		}
		switch action {
		case actContinue:
			completed++
			n++
			// A clean iteration that neither finishes NOR blocks a task means the agent keeps
			// continuing an in_progress task it can't complete — bail after maxStalls rather than
			// loop forever. Blocking a one-way door is progress (the task leaves the actionable set).
			var stop bool
			after, _ := queueProgress(hosts)
			if settledBaseline, stalls, stop = progressStall(after.Done+after.Blocked, settledBaseline, stalls); stop {
				return code, fmt.Errorf("no task finished or blocked in %d iterations — stopping (stuck on %q?)", maxStalls, active)
			}
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
			return code, fmt.Errorf("iteration failed %d times since the last success — stopping", fails)
		}
	}
	if len(custom) == 0 {
		ui.Info("queue empty — running audit pass")
		_, _, _ = a.runIteration(repo, img, agent, forkName, iterCmd(audit), hosts, sink)
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
func (a *app) debugShell(repo, img, agent string) {
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: []string{a.cfg.Shell}, Agent: agent,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
}

const progressPoll = 2 * time.Second // how often the live bar re-reads the queue while an iteration runs

// runIteration runs one boxed command in batch mode, teeing its output to the terminal while
// capturing the tail so a rate-limit notice can be detected. hosts are the queue files the
// live bar watches for task progress. In a fully interactive run the agent's output is funneled
// into the scroll history above a sticky progress bar (a Docker-build-style live view);
// otherwise it goes straight to the terminal unchanged.
func (a *app) runIteration(repo, img, agent, forkName string, cmd, hosts []string, sink io.Writer) (code int, output string, err error) {
	tail := &tailWriter{max: 64 << 10}
	live := ui.IsTerminal(os.Stdout) && ui.IsTerminal(os.Stderr)

	termOut, termErr := io.Writer(os.Stdout), io.Writer(os.Stderr)
	var bar *loopBar
	var funnel *lineWriter
	if live {
		region := ui.NewRegion(os.Stderr, func() int { return ui.TermWidth(os.Stderr) })
		c0, a0 := queueProgress(hosts)
		bar = newLoopBar(region, time.Now(), c0, a0)
		funnel = &lineWriter{fn: bar.history} // agent/loop lines scroll above the bar
		termOut, termErr = funnel, funnel
	}

	outWs := []io.Writer{termOut}
	errWs := []io.Writer{termErr, tail}
	if sink != nil { // fork loops also capture to ../<repo>-forks/.coop/<name>.log
		outWs = append(outWs, sink)
		errWs = append(errWs, sink)
	}
	// Claude's loop command (set by iterCmd on a TTY) emits stream-json; decode it into human
	// activity lines, feeding only the human text to tail so rate-limit detection still works.
	var stdoutW io.Writer
	var dec *streamDecoder
	if slices.Contains(cmd, "stream-json") {
		dec = newStreamDecoder(io.MultiWriter(outWs...), tail)
		stdoutW = dec
	} else {
		stdoutW = io.MultiWriter(append(outWs, tail)...)
	}

	var wg sync.WaitGroup
	var stop chan struct{}
	if live {
		stop = make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); monitorProgress(hosts, stop, bar) }()
		go func() { defer wg.Done(); spinLoop(bar, stop) }()
	}
	code, err = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: repo, Cmd: cmd, Agent: agent, Batch: true, ForkName: forkName,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
		Stdout: stdoutW,
		Stderr: io.MultiWriter(errWs...),
	})
	if live {
		close(stop)
		wg.Wait() // no goroutine repaints the region after this, so the teardown below is clean
	}
	if dec != nil {
		dec.flush() // before tail.String(): the last events must reach the rate-limit tail
	}
	if live {
		funnel.flush()
		bar.stop()
	}
	return code, tail.String(), err
}

// monitorProgress watches the queue while an iteration runs and pushes each task state change
// into the live bar — the agent moves task folders between state dirs as it works and the host
// sees those moves through the bind mount, so the bar's count and active task move live even
// while the agent's own output is still buffered. It returns when stop is closed.
func monitorProgress(hosts []string, stop <-chan struct{}, bar *loopBar) {
	t := time.NewTicker(progressPoll)
	defer t.Stop()
	last, _ := queueProgress(hosts) // the bar was built with this baseline
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			// c.total()==0 while we had a baseline is a torn read (a folder caught mid-move) — a
			// running loop always has tasks; keep the last good counts rather than blink to 0/0.
			if c, active := queueProgress(hosts); c != last && (c.total() > 0 || last.total() == 0) {
				bar.setProgress(c, active)
				last = c
			}
		}
	}
}
