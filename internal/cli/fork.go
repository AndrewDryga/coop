package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/project"
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
// Forks live in a sibling directory <repo>-forks/, one subdirectory per fork.

const forkSuffix = "-forks"

// forkVerbs are the canonical `coop fork` subcommands — the source for did-you-mean suggestions and
// the help Usage line, so those name only real, canonically-spelled commands. "acp" is here too:
// `coop fork <name> acp` fronts a fork over ACP, so a fork literally named "acp" would shadow it.
var forkVerbs = map[string]bool{
	"ls": true, "review": true, "merge": true, "rm": true, "open": true,
	"logs": true, "stop": true, "path": true, "acp": true,
}

// forkReserved reports whether name is off-limits for a fork (validForkName refuses it), so no fork
// can shadow a subcommand. It's forkVerbs plus "watch" (reserved so a fork can't be confused with the
// fleet-level `coop fleet watch`). Kept separate from forkVerbs so "watch" never leaks into a
// did-you-mean suggestion for a command that doesn't exist on `coop fork`.
func forkReserved(name string) bool {
	if name == "watch" {
		return true
	}
	return forkVerbs[name]
}

// forkHome is the sibling directory that holds every fork of repo.
func forkHome(repo string) string {
	return filepath.Join(filepath.Dir(repo), filepath.Base(repo)+forkSuffix)
}

// forkWorkspace is the clone directory for one named fork.
func forkWorkspace(repo, name string) string {
	return filepath.Join(forkHome(repo), name)
}

// pinForkWorkspace keeps the confirmed directory inode open through a destructive operation.
// Lstat rejects a swapped symlink, while the open handle prevents unlink/recreate inode reuse.
func pinForkWorkspace(path string) (*os.File, os.FileInfo, error) {
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
		return nil, nil, errors.New("workspace is not a directory")
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := handle.Stat()
	if err != nil || !os.SameFile(entry, info) {
		_ = handle.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("workspace changed while opening")
	}
	return handle, info, nil
}

func samePinnedForkWorkspace(path string, original os.FileInfo) bool {
	current, err := os.Lstat(path)
	return err == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 && os.SameFile(original, current)
}

// validExistingForkName accepts path-safe references to existing forks, including names that became
// reserved after creation. validForkName adds the creation-time reserved-word policy.
func validExistingForkName(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") ||
		strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validForkName(name string) bool {
	return !forkReserved(name) && validExistingForkName(name)
}

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
	fmt.Fprint(&b, "        --gate rebases in a scratch clone and runs the parent's gate read-only; red/conflict exits 1 (not with --open).\n")
	fmt.Fprintf(&b, "%s   new fork actions are verb-first (coop fork <verb> <name>); a fork can't be named a reserved verb.\n", p.Bold("NAMES"))
	fmt.Fprint(&b, "\nRun 'coop help' for all commands.\n") // match every other command's help footer
	return b.String()
}

// cmdFork is the `coop fork` family. Bare `coop fork` prints the family help; a
// reserved verb runs that subcommand; anything else opens (or resumes) a fork by name.
func (a *app) cmdFork(args []string) (int, error) {
	if len(args) == 0 {
		return forkHelp()
	}
	switch args[0] {
	case "ls":
		return a.forkLs(args[1:])
	case "review":
		return a.forkReview(args[1:])
	case "merge":
		return a.forkMerge(args[1:])
	case "rm":
		return a.forkRm(args[1:])
	case "open":
		return a.forkOpenEditor(args[1:])
	case "path":
		return a.forkPath(args[1:])
	case "logs":
		return a.forkLogs(args[1:])
	case "stop":
		return a.forkStop(args[1:])
	default:
		// `coop fork <name> acp [agent]` — front the fork as an ACP agent (for Zed).
		if len(args) >= 2 && args[1] == "acp" {
			return a.forkACP(args[0], args[2:])
		}
		// A typo'd subcommand would otherwise become a NEW fork name and silently clone + branch +
		// launch an agent. Catch a near-miss of a real subcommand and suggest it instead of creating.
		if repo, err := box.ResolveRepo(a.cfg.RepoOverride); err == nil {
			if verb, ok := forkVerbNearMiss(args, pathExists(forkWorkspace(repo, args[0]))); ok {
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
	return nearestCommand(args[0], forkVerbList())
}

// forkVerbList is the reserved fork subcommands as a sorted slice, for did-you-mean matching on a
// mistyped subcommand (so it isn't silently turned into a new fork name).
func forkVerbList() []string {
	v := make([]string, 0, len(forkVerbs))
	for k := range forkVerbs {
		v = append(v, k)
	}
	sort.Strings(v)
	return v
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
	if !validForkName(fa.name) {
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
	// fast and never leaves a stray fork behind (setupFork would otherwise clone first, then fail).
	if fa.credential != "" && !slices.Contains(box.EffectiveProfiles(a.cfg, fa.agent), fa.credential) {
		return 2, fmt.Errorf("%s has no account %q — sign in first: coop login %s@%s", fa.agent, fa.credential, fa.agent, fa.credential)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, fa.name)
	existed := pathExists(ws)
	// Read provider memory before --fresh destroys it, and reject a brand-new provider-less fork
	// before clone/image work. An explicit target or preset already set agentSet and always wins.
	if !fa.agentSet {
		if remembered := readForkAgent(ws); remembered != "" {
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
			handle, info, openErr := pinForkWorkspace(ws)
			if openErr != nil {
				return -1, fmt.Errorf("open fork %s before recreation: %w", fa.name, openErr)
			}
			originalHandle = handle
			originalWS = info
		}
		needsStop := forkNeedsStop(repo, fa.name)
		if needsStop && !fa.force {
			return 1, fmt.Errorf("--fresh: fork %q is running or awaiting cleanup — stop it first: coop fork stop %s (or add --force to stop it automatically)", fa.name, fa.name)
		}
		if existed {
			if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), fa.force); err != nil {
				return 1, fmt.Errorf("--fresh: %w (add --force to recreate anyway)", err)
			}
			if err := destroyGate("delete fork "+fa.name+" before recreating it", fa.yes); err != nil {
				return 2, err
			}
		}
		if needsStop {
			if code, err := a.forkStop([]string{fa.name}); err != nil {
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
	if fa.fresh {
		unlock, err := lockForkState(repo, fa.name)
		if err != nil {
			return -1, fmt.Errorf("lock fork %s state: %w", fa.name, err)
		}
		existsNow := pathExists(ws)
		if existsNow != existed {
			unlock()
			return 1, fmt.Errorf("--fresh: fork %q changed while awaiting recreation", fa.name)
		}
		if existed {
			if !samePinnedForkWorkspace(ws, originalWS) {
				unlock()
				return 1, fmt.Errorf("--fresh: fork %q was replaced while awaiting recreation", fa.name)
			}
		}
		if forkNeedsStop(repo, fa.name) {
			unlock()
			return 1, fmt.Errorf("--fresh: fork %q started or entered cleanup while awaiting recreation — stop it first: coop fork stop %s", fa.name, fa.name)
		}
		if existed {
			if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), fa.force); err != nil {
				unlock()
				return 1, fmt.Errorf("--fresh: fork %q changed while awaiting recreation: %w", fa.name, err)
			}
			if err := destroyFork(repo, fa.name); err != nil {
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
		if _, err := setupFork(repo, fa.name); err != nil {
			return -1, err
		}
	} else if !fa.worker {
		ui.Info("resuming fork %s (%s)", fa.name, ws)
	}
	saveForkAgent(ws, fa.agent)
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
			return a.detachForkLoop(repo, fa.name, fa.agent, fa.tasks, fa.credential, fa.model, fa.effort, fa.preset, fa.peers)
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

			hint := readForkSession(ws, fa.agent, account)
			snapshot := discoverer.SessionIDs(a.cfg, sessionCWD)
			if hint == "" && !fa.newSession && existed && !fa.fresh && a.cfg.Workdir == "" {
				if legacy := discoverer.LatestSessionID(a.cfg, sessionCWD); agents.ValidSessionID(legacy) && slices.Contains(snapshot, legacy) {
					saveForkSession(ws, fa.agent, account, legacy)
					hint = legacy
				}
			}
			captureNewSession = fa.newSession || hint == "" || !slices.Contains(snapshot, hint)
			if captureNewSession {
				sessionsBefore = snapshot
			}
			if fa.newSession {
				clearForkSession(ws, fa.agent, account)
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
		forkNextSteps(fa.name) // the box ran (the work is in the fork); print next steps even on a nonzero agent exit
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
		id = readForkSession(ws, fa.agent, account)
	}
	if ag.PresetSessionID() {
		if !fa.newSession {
			// Old forks stored one provider-only id with no account metadata. Adopt it only
			// when the selected account's adapter can prove that exact session exists.
			if id == "" {
				legacy := readLegacyForkSession(ws, fa.agent)
				if legacy != "" {
					if _, resumed := ag.Resume(a.cfg, sessionCWD, legacy); resumed {
						id = legacy
						saveForkSession(ws, fa.agent, account, id)
					}
				}
			}
		}
		if id == "" {
			if sid, err := newSessionID(); err == nil {
				id = sid
				saveForkSession(ws, fa.agent, account, id)
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
		saveForkSession(ws, provider, a.cfg.ActiveProfile(provider), id)
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

func forkNextSteps(name string) {
	ui.Steps(
		fmt.Sprintf("coop fork review %s", name),
		fmt.Sprintf("coop fork merge %s", name),
		fmt.Sprintf("coop fork rm %s", name),
	)
}

// setupFork creates the clone and its branch (the git half of forkCreate, with no
// agent run — so the lifecycle is testable without a container).
func setupFork(repo, name string) (string, error) {
	ws := forkWorkspace(repo, name)
	if err := os.MkdirAll(forkHome(repo), 0o755); err != nil {
		return ws, err
	}
	if err := gitClone(repo, ws); err != nil {
		return ws, fmt.Errorf("couldn't clone the repo into the fork workspace: %w", err)
	}
	_ = gitCheckoutNewBranch(ws, name) // branch may already exist in origin; fine
	propagateGitEnv(repo, ws)
	excludeFork(ws, ".coop/") // trusted setup only; never re-open agent-writable .git metadata later
	return ws, nil
}

// propagateGitEnv carries the parent's git environment into a fresh fork. A clone
// keeps no local identity and the box has no ambient ~/.gitconfig, so without this an
// agent couldn't commit and the user's global ignores wouldn't apply:
//   - user.name / user.email — so the agent's commits have an author;
//   - the global gitignore (core.excludesfile) content into .git/info/exclude — git's
//     local, uncommitted ignore file, so no host config path dangles inside the box.
func propagateGitEnv(repo, ws string) {
	propagateGitIdentity(repo, ws)
	// Signing materials (key + format) travel to the fork so commits can be signed
	// with your key when they're rebased on land — on the host, where the key lives.
	// commit.gpgsign is deliberately NOT copied: the keyless box must commit unsigned.
	for _, k := range []string{"user.signingkey", "gpg.format"} {
		if v := gitOut(repo, "config", "--get", k); v != "" {
			_ = gitRun(ws, "config", k, v)
		}
	}
	// Read core.excludesfile from your GLOBAL config, never the agent-writable repo: a poisoned
	// repo could otherwise point it at a host secret (e.g. ~/.ssh/id_rsa) and we'd copy that file's
	// content into the fork the agent reads. `--path` expands a leading ~ in the configured path.
	if gi := gitGlobalOut("--path", "core.excludesfile"); gi != "" {
		if data, err := os.ReadFile(gi); err == nil && len(data) > 0 {
			excl := filepath.Join(ws, ".git", "info", "exclude")
			if f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				_, _ = f.WriteString("\n# carried from your global core.excludesfile\n")
				_, _ = f.Write(data)
				_ = f.Close()
			}
		}
	}
}

// propagateGitIdentity gives a clone the trusted parent's resolved commit identity. Git clone does
// not copy local config, and a preview rebase must work even when the host has no global identity.
func propagateGitIdentity(repo, ws string) {
	if email := gitOut(repo, "config", "user.email"); email != "" {
		_ = gitRun(ws, "config", "user.email", email)
	}
	if name := gitOut(repo, "config", "user.name"); name != "" {
		_ = gitRun(ws, "config", "user.name", name)
	}
}

// forkAgentFile records which agent a fork was created/last run with — inside the fork,
// but git-excluded so it never lands. Re-entry without an explicit agent reads it back.
func forkAgentFile(ws string) string { return filepath.Join(ws, ".coop", "agent") }

func readForkAgent(ws string) string {
	if a := readForkMeta(ws, forkAgentFile(ws)); agents.Valid(a) {
		return a
	}
	return ""
}

func saveForkAgent(ws, agent string) { saveForkMeta(ws, forkAgentFile(ws), agent) }

const forkMetadataFileLimit = 4 << 10

// Fork metadata is provider-writable between launches. Reads and writes reuse the task metadata
// no-follow root/file primitives so a planted .coop or file symlink cannot reach a host path.
// Errors remain best-effort because these hints can be re-derived or re-prompted next run.
func readForkMeta(ws, path string) string {
	meta := filepath.Join(ws, ".coop")
	if filepath.Dir(path) != meta {
		return ""
	}
	root, err := openTaskMetadataRoot(meta)
	if err != nil {
		return ""
	}
	defer root.Close()
	data, err := readTaskMetadataFile(root, filepath.Base(path))
	if err != nil || len(data) > forkMetadataFileLimit {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveForkMeta(ws, path, value string) {
	meta := filepath.Join(ws, ".coop")
	if value == "" || len(value) > forkMetadataFileLimit || filepath.Dir(path) != meta {
		return
	}
	if err := os.Mkdir(meta, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return
	}
	root, err := openTaskMetadataRoot(meta)
	if err != nil {
		return
	}
	defer root.Close()
	_ = atomicWriteTaskFile(root, filepath.Base(path), []byte(value+"\n"))
}

// forkSessionFile records the coop-owned session id for a fork+agent+account,
// inside the fork but git-excluded, so re-entry resumes exactly that session.
func forkSessionFile(ws, agent, account string) string {
	return filepath.Join(ws, ".coop", "session."+agent+"."+account)
}

func legacyForkSessionFile(ws, agent string) string {
	return filepath.Join(ws, ".coop", "session."+agent)
}

func readForkSession(ws, agent, account string) string {
	id := readForkMeta(ws, forkSessionFile(ws, agent, account))
	if !agents.ValidSessionID(id) {
		return ""
	}
	return id
}

func readLegacyForkSession(ws, agent string) string {
	id := readForkMeta(ws, legacyForkSessionFile(ws, agent))
	if !agents.ValidSessionID(id) {
		return ""
	}
	return id
}

func saveForkSession(ws, agent, account, id string) {
	saveForkMeta(ws, forkSessionFile(ws, agent, account), id)
}

func clearForkSession(ws, agent, account string) {
	meta := filepath.Join(ws, ".coop")
	root, err := openTaskMetadataRoot(meta)
	if err != nil {
		return
	}
	defer root.Close()
	_ = root.Remove(filepath.Base(forkSessionFile(ws, agent, account)))
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

// excludeFork appends a pattern to the fork's local .git/info/exclude (git's uncommitted
// ignore file) if absent, so coop's per-fork bookkeeping never shows in a review diff or
// lands on merge.
func excludeFork(ws, pattern string) {
	excl := filepath.Join(ws, ".git", "info", "exclude")
	if data, err := os.ReadFile(excl); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return
			}
		}
	}
	if f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = f.WriteString("\n# coop: per-fork state, never committed\n" + pattern + "\n")
		_ = f.Close()
	}
}

// destroyFork removes a fork's workspace and its review/<name> ref, then prunes an
// empty forks home. Best-effort on the ref so it works for partially-built forks.
func destroyFork(repo, name string) error {
	_ = gitRun(repo, "branch", "-q", "-D", "review/"+name)
	if err := os.RemoveAll(forkWorkspace(repo, name)); err != nil {
		return err
	}
	if entries, _ := os.ReadDir(forkHome(repo)); len(entries) == 0 {
		_ = os.Remove(forkHome(repo))
	}
	return nil
}

// forkNames lists the forks of repo (subdirectories of the forks home, skipping
// the hidden state dir).
func forkNames(repo string) []string {
	entries, _ := os.ReadDir(forkHome(repo))
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// forkLifecycleNames includes pidfile-only forks whose workspace was removed after a worker crash.
// They remain visible until `coop fork stop` can finish exact-owner runtime cleanup.
func forkLifecycleNames(repo string) []string {
	seen := map[string]bool{}
	for _, name := range forkNames(repo) {
		seen[name] = true
	}
	entries, _ := os.ReadDir(forkStateDir(repo))
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".pid")
		if ok && !entry.IsDir() && validExistingForkName(name) {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (a *app) forkLs(args []string) (int, error) {
	asJSON := false
	rest := make([]string, 0, len(args))
	for _, x := range args {
		if x == "--json" {
			asJSON = true
			continue
		}
		rest = append(rest, x)
	}
	if err := rejectArgs("fork ls", rest); err != nil {
		return 2, err // a stray token should fail, not be silently ignored
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	if asJSON {
		return a.forkLsJSON(repo)
	}
	names := forkLifecycleNames(repo)
	if len(names) == 0 {
		ui.Note("no forks yet — open one with 'coop fork <name>'")
		return 0, nil
	}
	// Size the NAME column to the longest fork name (clamped). Rune-pad EVERY cell (padRight) rather
	// than %-Ns: a glyph like ⚠/⚑ in TASKS/CHANGES (or a "…" in a truncated name) is multi-byte, so
	// %-Ns would count bytes, short-pad, and shove later columns out from under their headers.
	nw := colWidth(names, len("NAME"), 24)
	const format = "  %s %s %s %s %s %s %s %s\n"
	// Bold the whole rendered line, not each cell: bolding a cell first would put ANSI
	// escape bytes inside the width count and misalign the header against the rows.
	fmt.Print(ui.For(os.Stdout).Bold(fmt.Sprintf(format, padRight("NAME", nw), padRight("AGENT", 8), padRight("BRANCH", 12), padRight("STATE", 9), padRight("TASKS", 8), padRight("CHANGES", 15), padRight("COST", 8), "UPDATED")))
	for _, n := range names {
		s := gatherForkStatus(repo, n)
		fmt.Printf(format, padRight(truncate(s.Name, nw), nw), padRight(s.Agent, 8), padRight(s.Branch, 12), padRight(s.stateCell(), 9), padRight(s.tasksCell(), 8), padRight(s.changesCell(), 15), padRight(s.costCell(), 8), s.Updated)
	}
	// A fork whose name is (or became) a reserved verb is unreachable by `coop fork <name>` — that
	// spelling runs the subcommand. validForkName now refuses such names, so this only catches forks
	// made before that guard; point at the escape hatch (path/rm still take it as an explicit arg).
	for _, n := range names {
		if forkReserved(n) {
			ui.Warn("fork %q shadows the '%s' subcommand — reach it via 'coop fork path %s' or 'coop fork rm %s'", n, n, n, n)
		}
	}
	return 0, nil
}

// forkLsJSON prints the repo's workspaces (root first, then forks) as JSON, each with its path and
// per-port serve URLs — machine-readable discovery for host tooling (screenshots, config
// generation) so it never reproduces coop's host-port hash. Each URL is keyed on the WORKSPACE
// path, so a fork's URLs are its own — matching what that fork's box publishes.
func (a *app) forkLsJSON(repo string) (int, error) {
	p, _ := project.Load(repo) // serve.ports config, best-effort (a broken project.yaml → no URLs)
	serveURLs := func(ws string) map[string]string {
		if len(p.Serve.Ports) == 0 {
			return nil
		}
		m := make(map[string]string, len(p.Serve.Ports))
		for _, port := range p.Serve.Ports {
			m[strconv.Itoa(port)] = fmt.Sprintf("http://localhost:%d", project.HostPort(ws, port))
		}
		return m
	}
	// Sidecar URLs need the compose config; skip the docker call for workspaces without a compose
	// file, and stay best-effort (no docker / parse error → omitted, never an error).
	svcURLs := func(ws string) map[string]string {
		cf := box.ComposeFile(ws)
		if cf == "" {
			return nil
		}
		m := map[string]string{}
		for _, sp := range box.ServicePorts(a.rt, ws, cf) {
			m[fmt.Sprintf("%s:%d", sp.Service, sp.ContainerPort)] = fmt.Sprintf("http://localhost:%d", sp.HostPort)
		}
		if len(m) == 0 {
			return nil
		}
		return m
	}
	type workspace struct {
		Name     string            `json:"name"`
		Path     string            `json:"path"`
		Serve    map[string]string `json:"serve,omitempty"`
		Services map[string]string `json:"services,omitempty"`
	}
	out := []workspace{{Name: "root", Path: repo, Serve: serveURLs(repo), Services: svcURLs(repo)}}
	for _, n := range forkLifecycleNames(repo) {
		ws := forkWorkspace(repo, n)
		out = append(out, workspace{Name: n, Path: ws, Serve: serveURLs(ws), Services: svcURLs(ws)})
	}
	b, err := json.MarshalIndent(map[string]any{"workspaces": out}, "", "  ")
	if err != nil {
		return 1, err
	}
	fmt.Println(string(b))
	return 0, nil
}

// forkBranch / forkUpdated read a fork's state (for `coop fork ls` and `coop fleet watch`).
// They run against an agent-controlled tree (post-work), so they use the hardened
// helpers — `diff`/`log` would otherwise fire a planted core.fsmonitor or diff.external.
func forkBranch(ws string) string { return gitOut(ws, "rev-parse", "--abbrev-ref", "HEAD") }

func forkUpdated(repo, ws string) string {
	// Show the fork's OWN latest commit. A fresh fork has none, so `git log -1` would report the base
	// commit it inherited from the clone — misreading a seconds-old fork as hours/days stale (and a
	// truly idle fork as fresh). When there are no commits beyond the base, fall back to the clone's
	// own age instead of the inherited time.
	if base := gitOut(repo, "rev-parse", "HEAD"); base != "" {
		if n := gitOut(ws, "rev-list", "--count", base+"..HEAD"); n != "" && n != "0" {
			if rel := gitOut(ws, "log", "-1", "--format=%cr"); rel != "" {
				return rel
			}
		}
	}
	if fi, err := os.Stat(ws); err == nil {
		return relativeAge(fi.ModTime())
	}
	return "—"
}

// relativeAge renders how long ago t was in git's `%cr` idiom, for timestamps that aren't git commits
// (a fork's clone time). Coarse buckets — it labels staleness, not exact durations.
func relativeAge(t time.Time) string {
	switch d := time.Since(t); {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return ui.Count(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return ui.Count(int(d.Hours()), "hour") + " ago"
	default:
		return ui.Count(int(d.Hours()/24), "day") + " ago"
	}
}

// gitFetchInto fetches a fork's branch into review/<name> in the parent repo. The
// fetch is forced (+) because landing rebases the fork branch, so a re-fetch after a
// rebase is not a fast-forward of the prior review ref.
func gitFetchInto(repo, ws, name string) error {
	return gitRun(repo, "fetch", "--quiet", ws, "+"+name+":review/"+name)
}

// forkReviewCandidate is a disposable, rebased view of a fork. base remains the parent commit the
// clone captured; name is the candidate branch. The caller owns cleanup whenever dir is non-empty.
type forkReviewCandidate struct {
	dir      string
	base     string
	name     string
	conflict bool
}

type forkReviewGateOutcome uint8

const (
	forkReviewGateUnchecked forkReviewGateOutcome = iota
	forkReviewGateNone
	forkReviewGateGreen
	forkReviewGateRed
	forkReviewGateConflict
)

func (o forkReviewGateOutcome) exitCode() int {
	if o == forkReviewGateRed || o == forkReviewGateConflict {
		return 1
	}
	return 0
}

func (c forkReviewCandidate) cleanup() { _ = os.RemoveAll(c.dir) }

func (c forkReviewCandidate) detachBase() error {
	return gitRun(c.dir, "checkout", "--quiet", "--detach", c.base)
}

// prepareForkReviewCandidate clones the parent's committed HEAD, fetches the fork's named branch,
// and rebases that branch in the scratch clone. Neither source repo is modified: local clone/fetch
// reads objects only, and every checkout/rebase occurs under c.dir. Preview rebases stay unsigned;
// signing changes commit identity, not the tree the gate checks, and must not invoke pinentry here.
func prepareForkReviewCandidate(repo, ws, name string) (c forkReviewCandidate, err error) {
	c.dir, err = os.MkdirTemp("", "coop-fork-review-")
	if err != nil {
		return c, err
	}
	keep := false
	defer func() {
		if !keep {
			c.cleanup()
			c = forkReviewCandidate{}
		}
	}()
	if err = gitClone(repo, c.dir); err != nil {
		return c, fmt.Errorf("clone parent into review scratch: %w", err)
	}
	propagateGitIdentity(repo, c.dir)
	c.base = gitOut(c.dir, "rev-parse", "HEAD")
	if c.base == "" {
		return c, errors.New("review scratch has no parent HEAD")
	}
	// Detach before the forced fetch so a fork named after the parent's checked-out branch cannot
	// collide with Git's refusal to update the current branch.
	if err = c.detachBase(); err != nil {
		return c, fmt.Errorf("detach review scratch base: %w", err)
	}
	c.name = name
	if err = gitRun(c.dir, "fetch", "--quiet", ws, "+"+name+":refs/heads/"+name); err != nil {
		return c, fmt.Errorf("fetch fork into review scratch: %w", err)
	}
	if err = gitRun(c.dir, "rebase", c.base, name); err != nil {
		if abortErr := gitRun(c.dir, "rebase", "--abort"); abortErr != nil {
			return c, fmt.Errorf("rebase review scratch failed and abort failed: %v; %w", err, abortErr)
		}
		c.conflict = true
	}
	keep = true
	return c, nil
}

func (a *app) forkReview(args []string) (int, error) {
	name, stat, tool, open, gate := "", false, false, false, false
	for _, x := range args {
		switch x {
		case "--stat":
			stat = true
		case "--tool":
			tool = true
		case "--open":
			open = true
		case "--gate":
			gate = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork review: unknown flag %q", x)
			}
			name = x
		}
	}
	if name == "" {
		return 2, errors.New("usage: coop fork review <name> [--stat | --tool | --open] [--gate]")
	}
	if !validExistingForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	if gate && open {
		return 2, errors.New("coop fork review: --gate cannot be combined with --open; use --stat or --tool so the review scratch can be removed reliably")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	reviewRepo, ref := repo, "review/"+name
	outcome := forkReviewGateUnchecked
	if gate {
		candidate, err := prepareForkReviewCandidate(repo, ws, name)
		if err != nil {
			return -1, err
		}
		defer candidate.cleanup()
		reviewRepo, ref = candidate.dir, candidate.name
		if candidate.conflict {
			outcome = forkReviewGateConflict
		} else {
			img, gateErr := a.mergeGate(repo)
			if gateErr != nil {
				return -1, gateErr
			}
			if img == "" {
				outcome = forkReviewGateNone
			} else {
				green, gateErr := a.reviewGatePasses(repo, candidate.dir, img)
				if gateErr != nil {
					return -1, fmt.Errorf("run review gate: %w", gateErr)
				}
				if green {
					outcome = forkReviewGateGreen
				} else {
					outcome = forkReviewGateRed
				}
			}
		}
		// The candidate branch stays at its rebased tip while HEAD returns to the captured parent base,
		// preserving the existing HEAD...ref dossier/diff contract inside the scratch clone.
		if err := candidate.detachBase(); err != nil {
			return -1, fmt.Errorf("detach review scratch after gate: %w", err)
		}
	} else if err := gitFetchInto(repo, ws, name); err != nil {
		return -1, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	a.forkBrief(reviewRepo, ws, name, ref, outcome)
	if s := costSummary(costForRepo(ws)); s != "" {
		ui.Info("cost: %s", s)
	}
	finish := func(code int, err error) (int, error) {
		if err != nil || code != 0 {
			return code, err
		}
		return outcome.exitCode(), nil
	}

	switch {
	case open: // open the fork in your IDE; review via its SCM panel
		return a.openInEditor(ws)
	case tool: // hand the diff to your GLOBAL git difftool (diff.tool), forced via -c so a
		// repo-poisoned diff.tool / difftool.<tool>.cmd can't run on `coop fork review --tool`.
		if t := gitGlobalOut("diff.tool"); t != "" {
			cargs := []string{"-c", "diff.tool=" + t}
			// Pin the tool's command from global too (empty neutralizes any repo override and lets
			// git use the built-in for a known tool), so the repo can't redirect even a named tool.
			cargs = append(cargs, "-c", "difftool."+t+".cmd="+gitGlobalOut("difftool."+t+".cmd"))
			_ = gitInteractive(reviewRepo, append(cargs, "difftool", "HEAD..."+ref)...)
		} else {
			ui.Note("no global git diff.tool set — showing the diff (--tool ignores repo config, for safety)")
			_ = gitInteractive(reviewRepo, "diff", "--no-ext-diff", "HEAD..."+ref) // internal diff (see default case)
		}
		return finish(0, nil)
	case stat:
		return finish(0, nil) // the brief already lists the files
	case a.cfg.ReviewCmd != "": // a user-defined review command
		return finish(a.runReviewCmd(reviewRepo, ws, name, ref))
	default:
		// --no-ext-diff: a broken diff.external / GIT_EXTERNAL_DIFF in the host environment would
		// otherwise make the diff "external diff died" (the -c diff.external= hardening blanks the
		// config but can't override the env var). Force git's internal diff so review always renders.
		_ = gitInteractive(reviewRepo, "diff", "--no-ext-diff", "HEAD..."+ref)
		return finish(0, nil)
	}
}

// resolveEditor picks the command used to open a fork for review, in order:
// $COOP_EDITOR, then your GLOBAL git core.editor, then a detected GUI editor, then
// $VISUAL/$EDITOR. Returns "" if nothing is configured or found. The editor is read from
// GLOBAL config only — never the agent-writable repo, which could otherwise point core.editor
// at a planted binary that runs on `coop fork review --open`.
func resolveEditor(cfgEditor string) string {
	if cfgEditor != "" {
		return cfgEditor
	}
	if e := gitGlobalOut("core.editor"); e != "" {
		return e // honor your global `git config core.editor`, e.g. "zed --wait"
	}
	return detectEditor()
}

// openInEditor opens the fork directory in an editor so you can review via its SCM
// panel. See resolveEditor for how the editor is chosen.
func (a *app) openInEditor(ws string) (int, error) {
	editor := resolveEditor(a.cfg.Editor)
	if editor == "" {
		return 1, errors.New("no editor found — set $COOP_EDITOR, git config core.editor, or $VISUAL/$EDITOR (or install code/cursor/zed/idea)")
	}
	parts := append(strings.Fields(editor), ws)
	ui.Note("opening %s in %s", ws, parts[0])
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return 1, fmt.Errorf("couldn't launch your editor %q: %w — check $COOP_EDITOR / git core.editor", parts[0], err)
	}
	return 0, nil
}

// forkACP fronts an existing fork as an ACP agent over stdio, pinned to the fork's
// path and the parent's image — so an editor (Zed) drives the fork's agent like any
// other ACP agent. Resuming the prior conversation is the editor's call (ACP
// session/load, which Zed drives); coop just exposes the fork, so its session history
// is right there to load.
func (a *app) forkACP(name string, rest []string) (int, error) {
	if !validExistingForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
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
	ws := forkWorkspace(repo, name)
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
	})
}

// detectEditor finds a GUI editor on PATH (for opening a fork as a folder), falling
// back to $VISUAL/$EDITOR.
func detectEditor() string {
	for _, e := range []string{"cursor", "code", "zed", "idea", "subl"} {
		if _, err := exec.LookPath(e); err == nil {
			return e
		}
	}
	if v := os.Getenv("VISUAL"); v != "" {
		return v
	}
	return os.Getenv("EDITOR")
}

// runReviewCmd runs COOP_REVIEW_CMD via sh -c from the parent repo, with the fork's
// path/name/ref in the environment so the command can use them.
func (a *app) runReviewCmd(repo, ws, name, ref string) (int, error) {
	cmd := exec.Command("sh", "-c", a.cfg.ReviewCmd)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"COOP_FORK_PATH="+ws, "COOP_FORK_NAME="+name, "COOP_REVIEW_REF="+ref)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return 1, fmt.Errorf("COOP_REVIEW_CMD failed: %w", err)
	}
	return 0, nil
}

// forkBrief prints the review dossier before the diff — commits, the agent's claim,
// policy findings, risk-ordered files, and the gate status — so a reviewer gets a map
// of the risk before reading the patch. Everything except the task log is computed by
// the parent from git facts; the log is the fork's own voice and is labeled as such,
// so a fork can't steer its review via its narrative.
func (a *app) forkBrief(repo, ws, name, ref string, gateOutcome forkReviewGateOutcome) {
	ins, del := parseShortstat(gitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	files := gitOut(repo, "diff", "--name-status", "HEAD..."+ref)
	nfiles := 0
	if files != "" {
		nfiles = len(strings.Split(files, "\n"))
	}
	ahead := gitOut(repo, "rev-list", "--count", "HEAD.."+ref)
	ui.Note("%s ← %s  ·  %s commit(s), +%d -%d across %d file(s)", ref, name, ahead, ins, del, nfiles)
	if log := gitOut(repo, "log", "--oneline", "--no-decorate", "HEAD.."+ref); log != "" {
		fmt.Println(ui.Bold("commits:"))
		fmt.Println(indent(log))
	}
	if why := latestTaskLog(ws, 12); strings.TrimSpace(why) != "" {
		fmt.Println(ui.Bold("why (agent's claim — latest task log):"))
		fmt.Println(indent(why))
	} else {
		fmt.Printf("%s no completed task yet\n", ui.Bold("why:"))
	}
	if files != "" { // the sections below are diff-derived; an empty diff has nothing to map
		// The SAME scan `coop fork merge` enforces — printed here so findings surface at
		// review, not as a failed merge. Advisory: review's exit code stays 0.
		if warns := policyScan(repo, ref); len(warns) == 0 {
			fmt.Printf("%s %s nothing flagged\n", ui.Bold("policy:"), ui.Green("✓"))
		} else {
			fmt.Printf("%s %s %s — these block 'coop fork merge' without --force\n",
				ui.Bold("policy:"), ui.Yellow("⚠"), ui.Count(len(warns), "finding"))
			for _, w := range warns {
				fmt.Println(indent(w))
			}
		}
		fmt.Println(ui.Bold("files:"))
		for _, sec := range classifyChanged(files, gitOut(repo, "diff", "--numstat", "HEAD..."+ref)) {
			fmt.Println(indent(sec.title + ":"))
			for _, f := range sec.files {
				fmt.Println(indent(indent(f.render())))
			}
		}
		if gateOutcome == forkReviewGateUnchecked {
			if len(a.gateFor(repo)) == 0 {
				fmt.Printf("%s none configured (COOP_GATE or .agent/project.yaml gate:)\n", ui.Bold("gate:"))
			} else {
				fmt.Printf("%s runs at merge — rolled back on failure\n", ui.Bold("gate:"))
			}
		}
	}
	switch gateOutcome {
	case forkReviewGateNone:
		fmt.Printf("%s none configured — rebase clean\n", ui.Bold("gate:"))
	case forkReviewGateGreen:
		fmt.Printf("%s %s green on rebased scratch (read-only)\n", ui.Bold("gate:"), ui.Green("✓"))
	case forkReviewGateRed:
		fmt.Printf("%s %s red on rebased scratch (read-only)\n", ui.Bold("gate:"), ui.Red("✗"))
	case forkReviewGateConflict:
		fmt.Printf("%s %s conflict while rebasing onto current parent — gate not run\n", ui.Bold("gate:"), ui.Yellow("⚠"))
	}
	fmt.Println(ui.Bold("diff:"))
}

// oneForkName returns the single fork name from the parsed positionals, rejecting a second one. The
// rm/merge/stop/logs families used to let a later positional silently overwrite the first — acting on
// only the last and printing success — a data-loss footgun (`fork rm a b` looks like it removed both).
// Zero positionals returns "" so callers can apply their own "name required" usage error.
func oneForkName(verb string, pos []string) (string, error) {
	if len(pos) > 1 {
		return "", fmt.Errorf("coop fork %s takes one name (got %s)", verb, strings.Join(pos, ", "))
	}
	if len(pos) == 0 {
		return "", nil
	}
	return pos[0], nil
}

// forkRmSafe is the guard for `rm`: never silently drop an agent's work.
func forkRmSafe(unmerged, dirty, force bool) error {
	if force {
		return nil
	}
	if dirty {
		return errors.New("fork has uncommitted changes — use --force to discard")
	}
	if unmerged {
		return errors.New("fork has unmerged commits — merge it first, or use --force")
	}
	return nil
}

// forkUnmerged reports whether the fork's branch tip is NOT yet an ancestor of the
// parent repo's HEAD (unknown-to-parent counts as unmerged, which is the safe side).
func forkUnmerged(repo, ws string) bool {
	sha := gitOut(ws, "rev-parse", "HEAD")
	if sha == "" {
		return false
	}
	return gitRun(repo, "merge-base", "--is-ancestor", sha, "HEAD") != nil
}

func (a *app) forkRm(args []string) (int, error) {
	force := false
	var pos []string
	for _, x := range args {
		switch x {
		case "--force", "-f":
			force = true
		case "--yes", "-y": // accepted so `--yes` skips the confirm below (read via hasYes)
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork rm: unknown flag %q", x)
			}
			pos = append(pos, x)
		}
	}
	name, err := oneForkName("rm", pos)
	if err != nil {
		return 2, err
	}
	if name == "" {
		return 2, errors.New("usage: coop fork rm <name> [--force] [--yes]")
	}
	if !validExistingForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	handle, originalWS, err := pinForkWorkspace(ws)
	if err != nil {
		return -1, fmt.Errorf("open fork %s before removal: %w", name, err)
	}
	defer handle.Close()
	// A running loop has the worktree bind-mounted RW; deleting it would orphan the worker +
	// container and strand the pidfile. Refuse (like merge/prune do) — or with --force, stop the
	// loop first so its container is reaped before the worktree goes.
	needsStop := forkNeedsStop(repo, name)
	if needsStop && !force {
		return 1, fmt.Errorf("fork %q is running or awaiting cleanup — stop it first: coop fork stop %s (or use --force)", name, name)
	}
	if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
		return 1, err
	}
	// Confirm the (unrecoverable) delete — default-No at a TTY, refuse piped without --yes. Distinct
	// from --force above, which overrides the unmerged/dirty guard, not this prompt.
	if err := destroyGate("delete fork "+name, hasYes(args)); err != nil {
		return 2, err
	}
	if needsStop {
		if code, err := a.forkStop([]string{name}); err != nil {
			return code, err
		}
	}
	unlock, err := lockForkState(repo, name)
	if err != nil {
		return -1, fmt.Errorf("lock fork %s state: %w", name, err)
	}
	defer unlock()
	// State may change while the confirmation prompt is open. Re-check under the same lock used by
	// detached startup so a newly-starting worker cannot lose its workspace underneath it.
	if !pathExists(ws) {
		return 1, fmt.Errorf("fork %q changed while awaiting confirmation — it no longer exists", name)
	}
	if !samePinnedForkWorkspace(ws, originalWS) {
		return 1, fmt.Errorf("fork %q was replaced while awaiting confirmation", name)
	}
	if forkNeedsStop(repo, name) {
		return 1, fmt.Errorf("fork %q started while awaiting confirmation — stop it first: coop fork stop %s", name, name)
	}
	if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
		return 1, fmt.Errorf("fork %q changed while awaiting confirmation: %w", name, err)
	}
	if err := destroyFork(repo, name); err != nil {
		return -1, err
	}
	ui.OK("removed fork %s", name)
	return 0, nil
}

// forkOpen prints a fork's path (for `cd "$(coop fork open <name>)"`).
// forkPath prints a fork's filesystem path (for `cd "$(coop fork path <name>)"` and the
// like). It's the plumbing companion to `coop fork open`, which opens it in your editor.
func (a *app) forkPath(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork path <name>")
	}
	name := args[0]
	if !validExistingForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	fmt.Println(ws)
	return 0, nil
}

// forkOpenEditor opens a fork in your editor (see resolveEditor for how it's chosen) so
// you can work in or eyeball it on the host. Opening is a host-side action, so it
// doesn't need the box image built.
func (a *app) forkOpenEditor(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork open <name>")
	}
	name := args[0]
	if !validExistingForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	return a.openInEditor(ws)
}
