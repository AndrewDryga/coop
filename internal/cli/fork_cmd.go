package cli

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/forkctl"
	"github.com/AndrewDryga/coop/internal/forkspace"
	"github.com/AndrewDryga/coop/internal/loop"
	"github.com/AndrewDryga/coop/internal/project"
	"github.com/AndrewDryga/coop/internal/runtime"
	"github.com/AndrewDryga/coop/internal/sessionsvc"
	"github.com/AndrewDryga/coop/internal/ui"
)

// A fork is a throwaway local clone of your repo handed to an agent: its origin
// is a local path (so the agent has nowhere to push) and gitignored secrets never
// come along. The lifecycle mirrors a contractor's PR — open, review, merge, close.
//
//	coop fork perf codex   open (or resume) a fork; codex works in it
//	coop fork ls           the forks of this repo
//	coop fork review perf  fetch the fork's branch + show the diff
//	coop fork merge perf   merge it back into your working tree
//	coop fork rm perf      discard the fork
//
// Forks live in a sibling directory <repo>-forks/, one subdirectory per fork — that layout, its
// names, and its lifecycle state file are internal/forkspace's contract; this file is the commands.

// forkHelp prints the fork family usage (shown for `coop fork [...] -h|--help`).
func forkHelp() (int, error) {
	fmt.Print(forkHelpText(ui.For(os.Stdout)))
	return 0, nil
}

// forkHelpText builds the fork family usage with palette p — p == ui.Palette{} gives the plain,
// byte-stable reference render that `coop help --all` and gendocs concatenate into the manual.
func forkHelpText(p ui.Palette) string {
	rows := []struct{ cmd, desc string }{
		{"coop fork <name> [target|preset]", "open or re-enter a fork; run an agent (claude:opus@work) or a preset"},
		{"coop fork <name> <target|preset> --loop", "loop the fork on a tasks folder (-d detaches)"},
		{"coop fork ls [--json]", "list this repo's forks (--json adds per-workspace serve URLs)"},
		{"coop fork logs [name]", "tail a fork's loop log (no name: all forks)"},
		{"coop fork review <name>", "dossier + diff (--stat, --tool, --open, --gate)"},
		{"coop fork <name> acp [target]", "front the fork as an ACP agent (for editors)"},
		{"coop fork merge <name>", "rebase onto your branch and land it (--all = fleet)"},
		{"coop fork rm <name>", "discard a fork (confirms; refuses unmerged/dirty without --force)"},
		{"coop fork open <name>", "open the fork in your editor"},
		{"coop fork path <name>", "print the fork's filesystem path"},
		{"coop fork stop <name>", "stop a detached loop"},
	}
	flags := []struct{ flag, desc string }{
		{"-c, --continue", "resume the prior session (the default on re-entry)"},
		{"    --new", "start a fresh agent session on re-entry"},
		{"    --fresh", "recreate the fork from scratch (confirms; refuses unmerged/dirty without --force)"},
		{"-d, --detach", "with --loop, run it in the background"},
		{"-t, --tasks", "with --loop, the tasks folder that seeds the queue (default: every .agent/tasks queue, incl. a monorepo's subprojects)"},
		{"    --peer <agent>", "with --loop, a peer iterations may consult read-only (repeatable)"},
		{"-f, --force", "merge/rm/--fresh: override the gate/policy/unmerged-dirty guard (not the confirm)"},
		{"-y, --yes", "merge/rm/--fresh: skip the delete confirm (required without a TTY)"},
		{"-f, --follow", "logs: keep streaming new output"},
	}
	pad := func(s string, w int) string {
		n := w - len(s)
		if n < 2 {
			n = 2
		}
		return s + strings.Repeat(" ", n)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — a throwaway clone handed to an agent; review and land it like a PR.\n\n", p.Bold("coop fork"))
	fmt.Fprint(&b, "  Usage: coop fork <name> [target] | ls | review | merge | logs | rm | stop | open | path\n\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s%s\n", pad(r.cmd, 34), r.desc)
	}
	fmt.Fprintf(&b, "\n%s (every short flag has a long form):\n", p.Bold("FLAGS"))
	for _, f := range flags {
		fmt.Fprintf(&b, "  %s%s\n", pad(f.flag, 16), f.desc)
	}
	fmt.Fprintf(&b, "\n%s  --open opens $COOP_EDITOR (else your global git core.editor); --tool uses your global git diff.tool.\n", p.Bold("REVIEW"))
	fmt.Fprint(&b, "        --gate rebases in an isolated scratch clone and runs the parent's gate; source mutations fail review.\n")
	fmt.Fprintf(&b, "%s   new fork actions are verb-first (coop fork <verb> <name>); a fork can't be named a reserved verb.\n", p.Bold("NAMES"))
	fmt.Fprint(&b, "\nRun 'coop help' for all commands.\n") // match every other command's help footer
	return b.String()
}

// forkctl binds the fork/fleet control plane to this run: the config, the runtime as detected so
// far (the zero value is the honest "not yet"), and the three things internal/forkctl needs from
// the process — runtime detection, the shared live-board driver, and the loop's cost telemetry.
// One Control per command, so a verb that detects the runtime and a later step that tears a box
// down with it see the same one.
func (a *app) forkctl() *forkctl.Control {
	return forkctl.New(a.cfg, a.rt, forkctl.Host{
		EnsureRuntime: func() (runtime.Runtime, error) {
			if err := a.ensureRuntime(); err != nil {
				return runtime.Runtime{}, err
			}
			return a.rt, nil
		},
		RunWatchLoop: runWatchLoop,
		ForkCost:     loop.WorkspaceCost,
	})
}

// cmdFork is the `coop fork` family. Bare `coop fork` prints the family help; a
// reserved verb runs that subcommand; anything else opens (or resumes) a fork by name.
func (a *app) cmdFork(args []string) (int, error) {
	if len(args) == 0 {
		return forkHelp()
	}
	fc := a.forkctl()
	switch args[0] {
	case "ls":
		return fc.ForkLs(args[1:])
	case "review":
		return fc.ForkReview(args[1:])
	case "merge":
		return fc.ForkMerge(args[1:])
	case "rm":
		return fc.ForkRm(args[1:])
	case "open":
		return fc.ForkOpenEditor(args[1:])
	case "path":
		return fc.ForkPath(args[1:])
	case "logs":
		return fc.ForkLogs(args[1:])
	case "stop":
		return fc.ForkStop(args[1:])
	default:
		// `coop fork <name> acp [agent]` — front the fork as an ACP agent (for Zed).
		if len(args) >= 2 && args[1] == "acp" {
			return a.forkACP(args[0], args[2:])
		}
		// A typo'd subcommand would otherwise become a NEW fork name and silently clone + branch +
		// launch an agent. Catch a near-miss of a real subcommand and suggest it instead of creating.
		if repo, err := box.ResolveRepo(a.cfg.RepoOverride); err == nil {
			if verb, ok := forkVerbNearMiss(args, pathExists(forkspace.Workspace(repo, args[0]))); ok {
				return 2, fmt.Errorf("unknown fork command %q — did you mean 'coop fork %s'? (give an agent, e.g. 'coop fork %s claude', to make a fork by that name)", args[0], verb, args[0])
			}
		}
		return a.forkCreate(args)
	}
}

// forkVerbNearMiss reports the fork verb that a would-be fork name is a likely typo of, so cmdFork
// can refuse it (with a suggestion) instead of silently cloning a stray fork. It stays quiet when the
// name is already an existing fork, or when an explicit agent follows it — an agent is the deliberate
// signal that args[0] really is a new fork name (`coop fork lss claude` creates `lss` on purpose).
func forkVerbNearMiss(args []string, forkExists bool) (string, bool) {
	if forkExists || (len(args) >= 2 && agents.Valid(args[1])) {
		return "", false
	}
	return nearestCommand(args[0], forkspace.VerbList())
}

// forkArgs is the parsed form of `coop fork <name> [agent] [flags]`.
type forkArgs struct {
	name       string
	agent      string
	agentSet   bool // an agent was given explicitly (vs defaulted / remembered from the fork)
	fresh      bool
	force      bool // -f/--force: with --fresh, discard unmerged/dirty work when recreating
	yes        bool // -y/--yes: with --fresh, skip the destructive confirmation
	cont       bool // -c/--continue: force-resume the prior session (now the default on re-entry)
	newSession bool // --new: start a fresh agent session even when re-entering a fork
	loop       bool
	detach     bool
	tasks      string   // --tasks <path>: the tasks folder to seed the loop's queue (defaults to .agent/tasks with --loop)
	credential string   // the fork's account, from the positional target's @account (else the ladder default)
	model      string   // the fork's model, from the positional target's :model (else the CLI/preset default)
	effort     string   // the fork's reasoning effort, from the positional target's /effort (else the agent default)
	peers      []string // --peer <target> (repeatable): the peers a loop iteration may ask read-only
	preset     string   // the orchestration preset this fork runs under (named in the who-runs positional)
	worker     bool     // internal: this process IS the detached loop worker (--_detached)
}

func parseForkCreate(args []string) (forkArgs, error) {
	fa := forkArgs{} // no implicit default — provider required (positional target or the preset lead)
	if len(args) == 0 || args[0] == "" {
		return fa, errors.New("usage: coop fork <name> [<agent>[:model][/effort][@account]] [--loop --tasks <path> [-d]]")
	}
	fa.name = args[0]
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		x := rest[i]
		switch {
		case !strings.HasPrefix(x, "-"):
			// The fork's who-runs positional: a TARGET (provider[:model][/effort][@account], its
			// model + single account fold into the one-off selection) OR a PRESET NAME (loaded by
			// forkCreate). A run picks ONE, so a second bare word errors.
			if fa.agentSet || fa.preset != "" {
				return fa, fmt.Errorf("coop fork: unexpected argument %q (the fork's agent/preset is already set — a run picks one)", x)
			}
			if !isTargetHead(x) {
				fa.preset = x
				break
			}
			t, terr := agents.ParseTarget(x)
			if terr != nil {
				return fa, terr
			}
			acct, aerr := singleAccount(t)
			if aerr != nil {
				return fa, aerr
			}
			fa.agent, fa.agentSet, fa.model, fa.effort, fa.credential = t.Provider, true, t.Model, t.Effort, acct
		case x == "--fresh":
			fa.fresh = true
		case x == "--force", x == "-f":
			fa.force = true
		case x == "--yes", x == "-y":
			fa.yes = true
		case x == "--continue", x == "-c":
			fa.cont = true
		case x == "--new":
			fa.newSession = true
		case x == "--loop":
			fa.loop = true
		case x == "-d", x == "--detach":
			fa.detach = true
			fa.loop = true
		case x == "--tasks", x == "-t":
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				return fa, errors.New("coop fork --tasks needs a path to a tasks folder")
			}
			i++
			fa.tasks = rest[i]
		case strings.HasPrefix(x, "--tasks="):
			if fa.tasks = strings.TrimPrefix(x, "--tasks="); fa.tasks == "" {
				return fa, errors.New("coop fork --tasks needs a path to a tasks folder")
			}
		case x == "--peer":
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				return fa, errors.New("coop fork --peer needs a peer: --peer <agent> (repeatable)")
			}
			i++
			fa.peers = append(fa.peers, rest[i])
		case strings.HasPrefix(x, "--peer="):
			if v := strings.TrimPrefix(x, "--peer="); v == "" {
				return fa, errors.New("coop fork --peer needs a peer: --peer <agent> (repeatable)")
			} else {
				fa.peers = append(fa.peers, v)
			}
		case x == "--_detached": // hidden: re-exec target for a detached loop
			fa.worker = true
			fa.loop = true
		default:
			return fa, fmt.Errorf("coop fork: unexpected argument %q", x)
		}
	}
	if !forkspace.ValidName(fa.name) {
		return fa, fmt.Errorf("invalid fork name %q (use letters, digits, '.', '_', or '-'; not a reserved verb)", fa.name)
	}
	if !fa.loop && fa.tasks != "" {
		return fa, errors.New("coop fork --tasks only applies with --loop")
	}
	if fa.cont && fa.newSession {
		return fa, errors.New("coop fork: --continue and --new are mutually exclusive")
	}
	if fa.yes && !fa.fresh {
		return fa, errors.New("coop fork: --yes only applies with --fresh")
	}
	// --peer names loop peers; an interactive fork has no ad-hoc peer set (name them on a loop).
	if len(fa.peers) > 0 && !fa.loop {
		return fa, errors.New("coop fork --peer only applies with --loop (name each peer: --peer <agent>)")
	}
	return fa, nil
}

// forkCreate opens a new fork (clone + branch) or resumes an existing one, then
// runs the chosen agent in it. The agent's exit status doesn't fail the handoff.
func (a *app) forkCreate(args []string) (int, error) {
	fc := a.forkctl()
	fa, err := parseForkCreate(args)
	if err != nil {
		return 2, err
	}
	// The fork's preset (named in the positional who slot): load + fail fast (pure local reads),
	// then default the fork's agent, credentials, and model from the preset's lead — a positional
	// target instead pins them, and the lead's model/credentials only apply when the fork runs the lead.
	if fa.preset != "" {
		p, err := a.loadRunPreset(fa.preset)
		if err != nil {
			return 2, err
		}
		if !fa.agentSet {
			fa.agent, fa.agentSet = p.LeadAgent, true // the preset's lead wins over the remembered agent
		}
		// The preset's models ladder drives the fork's rotation (built in runForkLoop from
		// a.preset); credentials/model aren't merged into fa here.
		a.applyPreset(p, fa.agent)
	}
	// Validate a pinned @account before any image/clone work, so a typo'd account fails
	// fast and never leaves a stray fork behind (forkspace.Setup would otherwise clone first, then
	// fail).
	if fa.credential != "" && !slices.Contains(box.EffectiveProfiles(a.cfg, fa.agent), fa.credential) {
		return 2, fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", fa.agent, fa.credential, fa.agent, fa.credential)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkspace.Workspace(repo, fa.name)
	existed := pathExists(ws)
	// Read provider memory before --fresh destroys it, and reject a brand-new provider-less fork
	// before clone/image work. An explicit target or preset already set agentSet and always wins.
	if !fa.agentSet {
		if remembered := forkctl.ReadForkAgent(ws); remembered != "" {
			if existed && !fa.worker && remembered != fa.agent {
				ui.Info("using this fork's agent: %s (pass an agent to switch)", remembered)
			}
			fa.agent = remembered
		}
	}
	// --loop with no --tasks is the monorepo-aware default: runForkLoop seeds every
	// project.TaskDirs queue (just .agent/tasks in a single repo) at its own path. Leaving
	// fa.tasks empty is the signal for that; an explicit --tasks is the single-queue override,
	// resolved+validated just below. Fail fast HERE if the repo has no queue at all — before any
	// clone — so a queue-less repo can't leave a stray fork behind and its worker error in a log.
	if fa.loop && fa.tasks == "" {
		dirs, err := project.TaskDirs(repo)
		if err != nil {
			return -1, err
		}
		if !slices.ContainsFunc(dirs, func(rel string) bool { return pathExists(filepath.Join(repo, rel)) }) {
			return -1, fmt.Errorf("no task queue found (%s) — run 'coop init' or pass --tasks", strings.Join(dirs, ", "))
		}
	}
	if fa.tasks != "" { // resolve to an absolute path now, so a detached worker still finds it
		abs, err := filepath.Abs(fa.tasks)
		if err != nil {
			return -1, err
		}
		if !pathExists(abs) {
			return -1, fmt.Errorf("coop fork --tasks: no such tasks folder: %s", fa.tasks)
		}
		fa.tasks = abs
	}
	// --fresh recreates an existing fork by destroying it first — run the same guard `fork rm` uses so
	// it can't silently discard an agent's unmerged/uncommitted work (--fresh --force overrides). Do it
	// BEFORE resolveImage (like parseForkCreate's flag checks): fail fast, never spin up an image to refuse.
	var originalHandle *os.File
	var originalWS os.FileInfo
	defer func() {
		if originalHandle != nil {
			_ = originalHandle.Close()
		}
	}()
	if fa.fresh {
		if existed {
			handle, info, openErr := forkspace.Pin(ws)
			if openErr != nil {
				return -1, fmt.Errorf("open fork %s before recreation: %w", fa.name, openErr)
			}
			originalHandle = handle
			originalWS = info
		}
		needsStop := forkspace.NeedsStop(repo, fa.name)
		if needsStop && !fa.force {
			return 1, fmt.Errorf("--fresh: fork %q is running or awaiting cleanup — stop it first: coop fork stop %s (or add --force to stop it automatically)", fa.name, fa.name)
		}
		if existed {
			if err := forkctl.ForkRmSafe(forkctl.ForkUnmerged(repo, ws), gitDirty(ws), fa.force); err != nil {
				return 1, fmt.Errorf("--fresh: %w (add --force to recreate anyway)", err)
			}
			if err := ui.DestroyGate("delete fork "+fa.name+" before recreating it", fa.yes); err != nil {
				return 2, err
			}
		}
		if needsStop {
			if code, err := fc.ForkStop([]string{fa.name}); err != nil {
				return code, err
			}
		}
	}
	// Keep established queue and destructive-work refusals first, but still reject a brand-new
	// provider-less fork before image or clone work. Provider memory was read above so --fresh can
	// retain its target after the workspace is destroyed.
	if fa.agent == "" {
		return 2, noProviderErr("fork <name>")
	}
	_, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	// Starting a fork is one of the points that already reaps, so reap this repo's boxes whose coop
	// died holding them. The detached worker skips it: the start that launched it just swept this
	// repo, and the worker's own loop start sweeps the fork's workspace.
	if !fa.worker {
		a.sweepOrphanBoxes(repo)
	}
	if fa.fresh {
		unlock, err := forkspace.LockState(repo, fa.name)
		if err != nil {
			return -1, fmt.Errorf("lock fork %s state: %w", fa.name, err)
		}
		existsNow := pathExists(ws)
		if existsNow != existed {
			unlock()
			return 1, fmt.Errorf("--fresh: fork %q changed while awaiting recreation", fa.name)
		}
		if existed {
			if !forkspace.SamePinned(ws, originalWS) {
				unlock()
				return 1, fmt.Errorf("--fresh: fork %q was replaced while awaiting recreation", fa.name)
			}
		}
		if forkspace.NeedsStop(repo, fa.name) {
			unlock()
			return 1, fmt.Errorf("--fresh: fork %q started or entered cleanup while awaiting recreation — stop it first: coop fork stop %s", fa.name, fa.name)
		}
		if existed {
			if err := forkctl.ForkRmSafe(forkctl.ForkUnmerged(repo, ws), gitDirty(ws), fa.force); err != nil {
				unlock()
				return 1, fmt.Errorf("--fresh: fork %q changed while awaiting recreation: %w", fa.name, err)
			}
			if err := forkctl.DestroyFork(a.rt, repo, fa.name); err != nil {
				unlock()
				return -1, err
			}
		}
		unlock()
		if originalHandle != nil {
			_ = originalHandle.Close()
			originalHandle = nil
		}
	}
	if !pathExists(ws) {
		ui.Info("forking %s → %s (secrets are gitignored, so they don't come along)", filepath.Base(repo), ws)
		if _, err := forkspace.Setup(repo, fa.name); err != nil {
			return -1, err
		}
	} else if !fa.worker {
		ui.Info("resuming fork %s (%s)", fa.name, ws)
	}
	forkctl.SaveForkAgent(ws, fa.agent)
	if fa.loop {
		// The worker/foreground paths run the loop here, so resolve --peer to peer targets
		// (validate authed, reject an @account). The detach path re-execs `coop fork … --peer
		// <t>` and the worker re-resolves, so it forwards the raw values instead.
		peers, err := a.resolvePeers("--peer", fa.peers)
		if err != nil {
			return 2, err
		}
		switch {
		case fa.worker:
			return a.runForkLoop(repo, ws, fa.name, fa.agent, fa.tasks, fa.credential, fa.model, fa.effort, peers, true)
		case fa.detach:
			return fc.DetachForkLoop(repo, fa.name, fa.agent, fa.tasks, fa.credential, fa.model, fa.effort, fa.preset, fa.peers)
		default:
			return a.runForkLoop(repo, ws, fa.name, fa.agent, fa.tasks, fa.credential, fa.model, fa.effort, peers, false)
		}
	}
	// Pin this interactive session's account/model/effort from the positional target, below any
	// preset the fork carries.
	if err := a.applyOneOff(fa.agent, fa.model, fa.credential, fa.effort); err != nil {
		return 2, err
	}
	// Codex mints its own IDs. Preserve an existing exact hint; migrate an old default-cwd
	// fork to an exact hint before launch; otherwise snapshot IDs so the completed fresh run
	// can claim only one uniquely new native session.
	var discoverer agents.SessionDiscoverer
	var sessionsBefore []string
	captureNewSession := false
	if ag, ok := agents.Get(fa.agent); ok && !ag.PresetSessionID() {
		discoverer, _ = ag.(agents.SessionDiscoverer)
		if discoverer != nil {
			account := a.cfg.ActiveProfile(fa.agent)
			sessionCWD := box.Workdir(a.cfg, ws)
			release, err := lockSessionProducer(a.cfg, fa.agent, sessionCWD)
			if err != nil {
				return 1, err
			}
			defer release()

			hint := forkctl.ReadForkSession(ws, fa.agent, account)
			snapshot := discoverer.SessionIDs(a.cfg, sessionCWD)
			if hint == "" && !fa.newSession && existed && !fa.fresh && a.cfg.Workdir == "" {
				if legacy := discoverer.LatestSessionID(a.cfg, sessionCWD); agents.ValidSessionID(legacy) && slices.Contains(snapshot, legacy) {
					forkctl.SaveForkSession(ws, fa.agent, account, legacy)
					hint = legacy
				}
			}
			captureNewSession = fa.newSession || hint == "" || !slices.Contains(snapshot, hint)
			if captureNewSession {
				sessionsBefore = snapshot
			}
			if fa.newSession {
				forkctl.ClearForkSession(ws, fa.agent, account)
			}
		}
	}
	// Resume the agent's prior session by default when re-entering a fork (opt out with
	// --new; --fresh recreates the fork, so it starts new too). Falls back to a fresh
	// run when no session for this fork exists. See forkLaunchCmd.
	cmd := a.forkLaunchCmd(fa, ws, existed)
	code, err := box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: ws, Cmd: cmd, Agent: fa.agent, ConsultLead: fa.agent, Preset: a.preset,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
	if err == nil {
		if captureNewSession {
			a.rememberNewDiscoveredForkSession(ws, fa.agent, discoverer, sessionsBefore)
		}
		forkctl.ForkNextSteps(fa.name) // the box ran (the work is in the fork); print next steps even on a nonzero agent exit
	}
	return code, err // propagate the agent's exit code, like every other launch path
}

// forkLaunchCmd builds the agent command for entering a fork: resume the fork's prior
// session on re-entry (when one exists), else start fresh. For agents that honor a
// coop-owned session id (claude/gemini/grok) coop allocates one per (fork, agent, account), persists
// it in the fork's git-excluded .coop state, starts the session under it, and resumes
// exactly it later — so a loop or consult that shares the cwd can never hijack the
// "continue". codex can't preset an id, so coop persists the native id it discovers
// after a run and resumes that exact session later.
func (a *app) forkLaunchCmd(fa forkArgs, ws string, existed bool) []string {
	ag, ok := agents.Get(fa.agent)
	if !ok {
		return a.defaultCmd(fa.agent)
	}
	sessionCWD := box.Workdir(a.cfg, ws)
	account := a.cfg.ActiveProfile(fa.agent)
	id := ""
	if !fa.newSession {
		id = forkctl.ReadForkSession(ws, fa.agent, account)
	}
	if ag.PresetSessionID() {
		if !fa.newSession {
			// Old forks stored one provider-only id with no account metadata. Adopt it only
			// when the selected account's adapter can prove that exact session exists.
			if id == "" {
				legacy := forkctl.ReadLegacyForkSession(ws, fa.agent)
				if legacy != "" {
					if _, resumed := ag.Resume(a.cfg, sessionCWD, legacy); resumed {
						id = legacy
						forkctl.SaveForkSession(ws, fa.agent, account, id)
					}
				}
			}
		}
		if id == "" {
			if sid, err := newSessionID(); err == nil {
				id = sid
				forkctl.SaveForkSession(ws, fa.agent, account, id)
			}
		}
	}
	if (existed && !fa.fresh && !fa.newSession) || fa.cont {
		// A shared COOP_WORKDIR makes a legacy Codex cwd lookup ambiguous across forks. Start
		// fresh once when no exact persisted native ID exists; the completed run records it.
		ambiguousDiscovery := !ag.PresetSessionID() && id == "" && a.cfg.Workdir != ""
		if !ambiguousDiscovery {
			if rc, resumed := ag.Resume(a.cfg, sessionCWD, id); resumed {
				ui.Info("continuing your last %s session in this fork", fa.agent)
				return rc
			}
		}
	}
	return ag.StartSession(a.cfg, id)
}

func (a *app) rememberNewDiscoveredForkSession(ws, provider string, discoverer agents.SessionDiscoverer, before []string) {
	if discoverer == nil {
		return
	}
	id := uniquelyNewSessionID(before, discoverer.SessionIDs(a.cfg, box.Workdir(a.cfg, ws)))
	if agents.ValidSessionID(id) {
		forkctl.SaveForkSession(ws, provider, a.cfg.ActiveProfile(provider), id)
	}
}

func uniquelyNewSessionID(before, after []string) string {
	seen := make(map[string]bool, len(before))
	for _, id := range before {
		seen[id] = true
	}
	newID := ""
	for _, id := range after {
		if seen[id] {
			continue
		}
		if newID != "" && newID != id {
			return ""
		}
		newID = id
	}
	return newID
}

// newSessionID returns a random RFC-4122 v4 UUID — the form claude, gemini, and grok require
// for --session-id.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// forkACP fronts an existing fork as an ACP agent over stdio, pinned to the fork's
// path and the parent's image — so an editor (Zed) drives the fork's agent like any
// other ACP agent. Resuming the prior conversation is the editor's call (ACP
// session/load, which Zed drives); coop just exposes the fork, so its session history
// is right there to load.
func (a *app) forkACP(name string, rest []string) (int, error) {
	if !forkspace.ValidExistingName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	companionRepositories, err := sessionCompanionRepositoriesFromEnvironment()
	if err != nil {
		return -1, err
	}
	peerVals, rest, err := extractPeer(rest)
	if err != nil {
		return 2, err
	}
	// --model/--credential are retired — name the fork's ACP session in the positional target
	// (coop fork <name> acp claude:opus@work), like plain `coop acp`.
	agent, model, profile, effort := "", "", "", ""
	for _, x := range rest {
		switch {
		case isTargetHead(x):
			// provider[:model][/effort][@account]: model + single account fold into the session's one-off
			// selection, applied before acpCommand so gemini's own-binary adapter takes the flag.
			t, terr := agents.ParseTarget(x)
			if terr != nil {
				return 2, terr
			}
			agent = t.Provider
			if terr := foldTarget(t, &model, &profile); terr != nil {
				return 2, terr
			}
			effort = t.Effort
		default:
			return 2, fmt.Errorf("usage: coop fork %s acp [%s][:model][/effort][@account]", name, strings.Join(agents.Names(), "|"))
		}
	}
	if agent == "" {
		return 2, fmt.Errorf("name the agent — coop fork <name> acp <%s>; sign in with 'coop login <agent>' or see 'coop credentials'", strings.Join(agents.Names(), "|"))
	}
	if err := a.applyOneOff(agent, model, profile, effort); err != nil {
		return 2, err
	}
	// isTargetHead accepted only a registered provider, so the adapter lookup cannot miss.
	cmd := acpCommand(a.cfg, agent)
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	ws := forkspace.Workspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s (open it first: coop fork %s)", name, name)
	}
	peers, err := a.resolvePeers("--peer", peerVals)
	if err != nil {
		return 2, err
	}
	lead := ""
	if len(peers) > 0 {
		lead = agent
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: ws, Workdir: ws, Cmd: cmd, ForceNoTTY: true, Agent: agent, ConsultLead: lead, Peers: peers,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
		ForkName: name, ForkOwner: forkctl.ForkContainerOwner(repo, name),
		RunID: sessionsvc.RunIDFromEnv(), CompanionRepositories: companionRepositories,
	})
}

// cmdFleet manages a declarative fleet of forks from .agent/fleet.
func (a *app) cmdFleet(args []string) (int, error) {
	fc := a.forkctl()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "":
		return groupHelp("fleet") // bare `coop fleet` shows help, not an error (see rule)
	case "init":
		return fc.FleetInit()
	case "up":
		return a.fleetUp(args[1:])
	case "down":
		return fc.FleetDown(args[1:])
	case "watch":
		if err := rejectArgs("fleet watch", args[1:]); err != nil {
			return 2, err
		}
		return fc.FleetWatch()
	case "prune":
		return fc.FleetPrune(args[1:])
	case "ls":
		// A fleet is its forks — there's no fleet-level listing. Point at the two real views instead of
		// a bare "unknown command" (rule: `ls` is the list verb, so it must lead somewhere useful).
		return 2, fmt.Errorf("coop fleet has no %q — list the forks with `coop fork ls`, or watch the live board with `coop fleet watch`", sub)
	default:
		return 2, unknownErr("fleet command", sub, []string{"init", "up", "down", "watch", "prune"})
	}
}

// fleetAbortErr formats the error when `fleet up` fails fast partway through. Failing fast (over a
// silent partial fleet) is the intended behavior — but when forks already started, the error must
// be loud about it and name the cleanup, so a half-started fleet isn't discovered hours later.
func fleetAbortErr(name string, err error, started int) error {
	if started > 0 {
		return fmt.Errorf("fleet up: %q failed to start (%w) — aborted with %d fork(s) already running; stop them with 'coop fleet down' (or inspect via 'coop fork ls')", name, err, started)
	}
	return fmt.Errorf("fleet up: %q failed to start: %w", name, err)
}

func (a *app) fleetUp(args []string) (int, error) {
	fc := a.forkctl()
	prune, force, yes, err := forkctl.ParseFleetActionFlags("up", args)
	if err != nil {
		return 2, err
	}
	if prune && !yes && !ui.IsTerminal(os.Stdin) {
		return 2, errors.New("refusing to prune forks without confirmation — re-run with --yes (no terminal to prompt)")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	fleet, err := fc.LoadFleet(repo)
	if err != nil {
		return -1, err
	}
	// Validate per-fork profiles up front, so a typo fails loud here instead of silently in a
	// detached worker's log. (A fork with no profile= falls back to the repo pool / all signed-in.)
	unsigned := forkctl.UnsignedFleetAccounts(a.cfg, fleet)
	if len(unsigned) > 0 {
		return 2, fmt.Errorf("fleet up: these accounts aren't signed in: %s — run: coop login <provider>@<account>", strings.Join(unsigned, ", "))
	}
	// Bringing the fleet up is a natural reap point: clear this repo's boxes left behind by a coop
	// that was killed, once, before any fork starts (each fork's own start finds the sweep done).
	a.sweepOrphanBoxes(repo)
	started := 0
	for _, e := range fleet {
		if pid := forkspace.RunningPid(repo, e.Name); pid != 0 {
			ui.Note("fork %s already running (pid %d) — skipping", e.Name, pid)
			continue // idempotent: re-running `fleet up` leaves live loops alone
		}
		tasks := e.Tasks // fleet paths are repo-relative; make them absolute for the fork
		if !filepath.IsAbs(tasks) {
			tasks = filepath.Join(repo, tasks)
		}
		// The who-runs is the fork's positional: its target, or its preset name (forkctl.ParseFleetYAML
		// set exactly one). A run picks one, so pass it in the single who slot.
		who := e.Agent
		if who == "" {
			who = e.Preset
		}
		forkArgs := []string{e.Name, who, "--loop", "-d", "--tasks", tasks}
		if code, err := a.cmdFork(forkArgs); err != nil {
			return code, fleetAbortErr(e.Name, err, started)
		}
		started++
	}
	ui.OK("%s detached — coop fork ls · coop fork logs -f", ui.Count(started, "fork"))
	if prune {
		if code, err := fc.PruneFleet(repo, force, yes); err != nil {
			return code, err
		}
	}
	return 0, nil
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
	// A fork works its own seeded queue(s) in the worktree, and its boxes carry the fork's own
	// runtime owner label so `coop fork stop` can find them.
	code, err := a.loopctl().Run(loop.RunSpec{
		Repo: ws, Image: img, Agent: agent,
		ForkName: name, ForkOwner: forkctl.ForkContainerOwner(repo, name),
		Rotation: rot, Queues: forkQueue, Preset: a.preset, Peers: peers, Sink: sink,
		// Detached/fork loops aren't interactive: no debug shell, no pre-flight, no task limit.
	})
	if err == nil && !detached {
		forkctl.ForkNextSteps(name)
	}
	return code, err
}
