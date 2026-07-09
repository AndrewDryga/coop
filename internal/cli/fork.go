package cli

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
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

// validForkName keeps a name to a single safe path/branch segment.
func validForkName(name string) bool {
	if name == "" || forkReserved(name) {
		return false
	}
	if name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	// A fork name is also a git branch and a whitespace-/`=`-delimited token in .agent/fleet, so
	// whitespace or `=` would break the fleet-file round-trip (parseFleet re-splits on Fields).
	return !strings.ContainsAny(name, "/\\ \t\r\n=")
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
		{"coop fork <name> [agent]", "open or re-enter a fork and run an agent"},
		{"coop fork <name> <agent> --loop", "loop the fork on a tasks folder (-d detaches)"},
		{"coop fork ls", "list this repo's forks"},
		{"coop fork logs [name]", "tail a fork's loop log (no name: all forks)"},
		{"coop fork review <name>", "dossier + diff (--stat, --tool, --open)"},
		{"coop fork <name> acp [agent]", "front the fork as an ACP agent (for editors)"},
		{"coop fork merge <name>", "rebase onto your branch and land it (--all = fleet)"},
		{"coop fork rm <name>", "discard a fork (confirms; refuses unmerged/dirty without --force)"},
		{"coop fork open <name>", "open the fork in your editor"},
		{"coop fork path <name>", "print the fork's filesystem path"},
		{"coop fork stop <name>", "stop a detached loop"},
	}
	flags := []struct{ flag, desc string }{
		{"-c, --continue", "resume the prior session (the default on re-entry)"},
		{"    --new", "start a fresh agent session on re-entry"},
		{"    --fresh", "recreate the fork from scratch (refuses unmerged/dirty without --force)"},
		{"-d, --detach", "with --loop, run it in the background"},
		{"-t, --tasks", "with --loop, the tasks folder that seeds the queue (defaults to .agent/tasks)"},
		{"    --credential", "pin this fork's account (else the preset/default)"},
		{"    --model", "model for this fork's agent (see 'coop models')"},
		{"    --preset", "orchestration preset for this fork (see 'coop help presets')"},
		{"    --consult", "with --loop, iterations may consult the authed peers read-only"},
		{"-f, --force", "merge/rm/--fresh: override the gate/policy/unmerged-dirty guard (not the confirm)"},
		{"-y, --yes", "merge/rm: skip the delete confirm (required without a TTY)"},
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
	fmt.Fprint(&b, "  Usage: coop fork <name> [agent] | ls | review | merge | logs | rm | stop | open | path\n\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "  %s%s\n", pad(r.cmd, 34), r.desc)
	}
	fmt.Fprintf(&b, "\n%s (every short flag has a long form):\n", p.Bold("FLAGS"))
	for _, f := range flags {
		fmt.Fprintf(&b, "  %s%s\n", pad(f.flag, 16), f.desc)
	}
	fmt.Fprintf(&b, "\n%s  --open opens $COOP_EDITOR (else your global git core.editor); --tool uses your global git diff.tool.\n", p.Bold("REVIEW"))
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
	cont       bool // -c/--continue: force-resume the prior session (now the default on re-entry)
	newSession bool // --new: start a fresh agent session even when re-entering a fork
	loop       bool
	detach     bool
	tasks      string // --tasks <path>: the tasks folder to seed the loop's queue (defaults to .agent/tasks with --loop)
	credential string // --credential <name>: pin the fork's account (else the ladder default)
	model      string // --model <m[@account]>: the fork's model, optionally pinning the account
	consult    bool   // --consult: loop iterations may ask the authed peers (interactive forks always may)
	preset     string // --preset <name>: the orchestration preset this fork's loop runs under
	worker     bool   // internal: this process IS the detached loop worker (--_detached)
}

func parseForkCreate(args []string) (forkArgs, error) {
	fa := forkArgs{agent: agents.Default()}
	if len(args) == 0 || args[0] == "" {
		return fa, errors.New("usage: coop fork <name> [claude|codex|gemini] [--loop --tasks <path> [-d]]")
	}
	fa.name = args[0]
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		x := rest[i]
		switch {
		case agents.Valid(x):
			fa.agent = x
			fa.agentSet = true
		case x == "--fresh":
			fa.fresh = true
		case x == "--force", x == "-f":
			fa.force = true
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
		case x == "--credential", x == "--credentials":
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				return fa, fmt.Errorf("coop fork %s needs an account name", x)
			}
			i++
			fa.credential = rest[i]
		case strings.HasPrefix(x, "--credential="), strings.HasPrefix(x, "--credentials="):
			if _, val, _ := strings.Cut(x, "="); val == "" {
				return fa, fmt.Errorf("coop fork %s needs an account name", strings.SplitN(x, "=", 2)[0])
			} else {
				fa.credential = val
			}
		case x == "--preset":
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				return fa, errors.New("coop fork --preset needs a preset name")
			}
			i++
			fa.preset = rest[i]
		case strings.HasPrefix(x, "--preset="):
			if fa.preset = strings.TrimPrefix(x, "--preset="); fa.preset == "" {
				return fa, errors.New("coop fork --preset needs a preset name")
			}
		case x == "--model":
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				return fa, errors.New("coop fork --model needs a model name")
			}
			i++
			fa.model = rest[i]
		case strings.HasPrefix(x, "--model="):
			if fa.model = strings.TrimPrefix(x, "--model="); fa.model == "" {
				return fa, errors.New("coop fork --model needs a model name")
			}
		case x == "--consult":
			fa.consult = true
		case x == "--_detached": // hidden: re-exec target for a detached loop
			fa.worker = true
			fa.loop = true
		default:
			return fa, fmt.Errorf("coop fork: unexpected argument %q", x)
		}
	}
	if !validForkName(fa.name) {
		return fa, fmt.Errorf("invalid fork name %q (no slashes, not a reserved verb)", fa.name)
	}
	if !fa.loop && fa.tasks != "" {
		return fa, errors.New("coop fork --tasks only applies with --loop")
	}
	// An interactive fork is ALWAYS a consult lead (forkCreate sets it unconditionally), so the
	// flag only means something for a loop — accepting it elsewhere would imply it toggles a thing
	// it doesn't.
	if fa.consult && !fa.loop {
		return fa, errors.New("coop fork --consult only applies with --loop (an interactive fork may always consult its peers)")
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
	// --preset: load + fail fast (pure local reads), then default the fork's agent,
	// credentials, and model from the preset's lead — explicit flags still win, and the
	// lead's model/credentials only apply when the fork actually runs the lead agent.
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
	// Validate a pinned --credential before any image/clone work, so a typo'd account fails
	// fast and never leaves a stray fork behind (setupFork would otherwise clone first, then fail).
	if fa.credential != "" && !slices.Contains(a.cfg.Profiles(fa.agent), fa.credential) {
		return 2, fmt.Errorf("%s has no account %q — sign in first: coop login %s --credential %s", fa.agent, fa.credential, fa.agent, fa.credential)
	}
	// --loop with no --tasks defaults to the repo's own queue (.agent/tasks); per-fork slices are
	// the explicit case. resolveImage below re-resolves the repo, but that does image work too.
	if fa.loop && fa.tasks == "" {
		repo, err := box.ResolveRepo(a.cfg.RepoOverride)
		if err != nil {
			return -1, err
		}
		fa.tasks = filepath.Join(repo, ".agent", "tasks")
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
	if fa.fresh {
		repo, err := box.ResolveRepo(a.cfg.RepoOverride)
		if err != nil {
			return -1, err
		}
		if ws := forkWorkspace(repo, fa.name); pathExists(ws) {
			if err := forkRmSafe(forkUnmerged(repo, fa.name), gitDirty(ws), fa.force); err != nil {
				return 1, fmt.Errorf("--fresh: %w (add --force to recreate anyway)", err)
			}
		}
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, fa.name)
	existed := pathExists(ws) // a re-entry (vs a fresh fork) — resume by default below
	if fa.fresh && pathExists(ws) {
		if err := destroyFork(repo, fa.name); err != nil { // guard already passed above
			return -1, err
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
	// A fork remembers its agent: an explicit one wins (and updates the memory);
	// otherwise re-entry uses the agent the fork was created with, so resume-by-default
	// finds the right session instead of silently falling back to claude.
	if !fa.agentSet {
		if remembered := readForkAgent(ws); remembered != "" {
			if existed && !fa.worker && remembered != fa.agent {
				ui.Info("using this fork's agent: %s (pass an agent to switch)", remembered)
			}
			fa.agent = remembered
		}
	}
	saveForkAgent(ws, fa.agent)
	if fa.loop {
		switch {
		case fa.worker:
			return a.runForkLoop(repo, ws, fa.name, fa.agent, fa.tasks, fa.credential, fa.model, fa.consult, true)
		case fa.detach:
			return a.detachForkLoop(repo, fa.name, fa.agent, fa.tasks, fa.credential, fa.model, fa.preset, fa.consult)
		default:
			return a.runForkLoop(repo, ws, fa.name, fa.agent, fa.tasks, fa.credential, fa.model, fa.consult, false)
		}
	}
	// Pin this interactive session's account/model from --credential / --model (model@account
	// shortcut allowed), below any preset the fork carries.
	if err := a.applyOneOff(fa.agent, fa.model, fa.credential); err != nil {
		return 2, err
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
		forkNextSteps(fa.name) // the box ran (the work is in the fork); print next steps even on a nonzero agent exit
	}
	return code, err // propagate the agent's exit code, like every other launch path
}

// forkLaunchCmd builds the agent command for entering a fork: resume the fork's prior
// session on re-entry (when one exists), else start fresh. For agents that honor a
// coop-owned session id (claude/gemini) coop allocates one per (fork, agent), persists
// it in the fork's git-excluded .coop state, starts the session under it, and resumes
// exactly it later — so a loop or consult that shares the cwd can never hijack the
// "continue". codex can't preset an id, so it scans for its most-recent interactive
// session instead.
func (a *app) forkLaunchCmd(fa forkArgs, ws string, existed bool) []string {
	ag, ok := agents.Get(fa.agent)
	if !ok {
		return a.defaultCmd(fa.agent)
	}
	id := ""
	if ag.PresetSessionID() {
		if id = readForkSession(ws, fa.agent); id == "" {
			if sid, err := newSessionID(); err == nil {
				id = sid
				saveForkSession(ws, fa.agent, id)
			}
		}
	}
	if (existed && !fa.fresh && !fa.newSession) || fa.cont {
		if rc, resumed := ag.Resume(a.cfg, ws, id); resumed {
			ui.Info("continuing your last %s session in this fork", fa.agent)
			return rc
		}
	}
	return ag.StartSession(a.cfg, id)
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
	return ws, nil
}

// propagateGitEnv carries the parent's git environment into a fresh fork. A clone
// keeps no local identity and the box has no ambient ~/.gitconfig, so without this an
// agent couldn't commit and the user's global ignores wouldn't apply:
//   - user.name / user.email — so the agent's commits have an author;
//   - the global gitignore (core.excludesfile) content into .git/info/exclude — git's
//     local, uncommitted ignore file, so no host config path dangles inside the box.
func propagateGitEnv(repo, ws string) {
	if email := gitOut(repo, "config", "user.email"); email != "" {
		_ = gitRun(ws, "config", "user.email", email)
	}
	if name := gitOut(repo, "config", "user.name"); name != "" {
		_ = gitRun(ws, "config", "user.name", name)
	}
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

// forkAgentFile records which agent a fork was created/last run with — inside the fork,
// but git-excluded so it never lands. Re-entry without an explicit agent reads it back.
func forkAgentFile(ws string) string { return filepath.Join(ws, ".coop", "agent") }

func readForkAgent(ws string) string {
	data, err := os.ReadFile(forkAgentFile(ws))
	if err != nil {
		return ""
	}
	if a := strings.TrimSpace(string(data)); agents.Valid(a) {
		return a
	}
	return ""
}

func saveForkAgent(ws, agent string) { saveForkMeta(ws, forkAgentFile(ws), agent) }

// saveForkMeta writes one of a fork's .coop/ bookkeeping files (its agent, its session id),
// best-effort: an empty value is a no-op, and any write failure is swallowed since the file
// is a convenience re-derived or re-prompted next run. On a successful write it excludes
// .coop/ from the fork's diff so the bookkeeping never lands in a merge.
func saveForkMeta(ws, path, value string) {
	if value == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if os.WriteFile(path, []byte(value+"\n"), 0o644) == nil {
		excludeFork(ws, ".coop/")
	}
}

// forkSessionFile records the coop-owned session id for a fork+agent (claude/gemini),
// inside the fork but git-excluded, so re-entry resumes exactly that session.
func forkSessionFile(ws, agent string) string {
	return filepath.Join(ws, ".coop", "session."+agent)
}

func readForkSession(ws, agent string) string {
	data, err := os.ReadFile(forkSessionFile(ws, agent))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func saveForkSession(ws, agent, id string) { saveForkMeta(ws, forkSessionFile(ws, agent), id) }

// newSessionID returns a random RFC-4122 v4 UUID — the form claude and gemini require
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

func (a *app) forkLs(args []string) (int, error) {
	if err := rejectArgs("fork ls", args); err != nil {
		return 2, err // a stray token should fail, not be silently ignored
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Note("no forks yet — open one with 'coop fork <name>'")
		return 0, nil
	}
	// Size the NAME column to the longest fork name (clamped). Rune-pad EVERY cell (padRight) rather
	// than %-Ns: a glyph like ⚠/⚑ in TASKS/CHANGES (or a "…" in a truncated name) is multi-byte, so
	// %-Ns would count bytes, short-pad, and shove later columns out from under their headers.
	nw := colWidth(names, len("NAME"), 24)
	const format = "  %s %s %s %s %s %s %s\n"
	// Bold the whole rendered line, not each cell: bolding a cell first would put ANSI
	// escape bytes inside the width count and misalign the header against the rows.
	fmt.Print(ui.For(os.Stdout).Bold(fmt.Sprintf(format, padRight("NAME", nw), padRight("AGENT", 8), padRight("BRANCH", 12), padRight("STATE", 9), padRight("TASKS", 8), padRight("CHANGES", 15), "UPDATED")))
	for _, n := range names {
		s := gatherForkStatus(repo, n)
		fmt.Printf(format, padRight(truncate(s.Name, nw), nw), padRight(s.Agent, 8), padRight(s.Branch, 12), padRight(s.stateCell(), 9), padRight(s.tasksCell(), 8), padRight(s.changesCell(), 15), s.Updated)
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

func (a *app) forkReview(args []string) (int, error) {
	name, stat, tool, open := "", false, false, false
	for _, x := range args {
		switch x {
		case "--stat":
			stat = true
		case "--tool":
			tool = true
		case "--open":
			open = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork review: unknown flag %q", x)
			}
			name = x
		}
	}
	if name == "" {
		return 2, errors.New("usage: coop fork review <name> [--stat | --tool | --open]")
	}
	if !validForkName(name) {
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
	if err := gitFetchInto(repo, ws, name); err != nil {
		return -1, fmt.Errorf("%s: git fetch: %w", name, err)
	}
	ref := "review/" + name
	a.forkBrief(repo, ws, name, ref)

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
			_ = gitInteractive(repo, append(cargs, "difftool", "HEAD..."+ref)...)
		} else {
			ui.Note("no global git diff.tool set — showing the diff (--tool ignores repo config, for safety)")
			_ = gitInteractive(repo, "diff", "--no-ext-diff", "HEAD..."+ref) // internal diff (see default case)
		}
		return 0, nil
	case stat:
		return 0, nil // the brief already lists the files
	case a.cfg.ReviewCmd != "": // a user-defined review command
		return a.runReviewCmd(repo, ws, name, ref)
	default:
		// --no-ext-diff: a broken diff.external / GIT_EXTERNAL_DIFF in the host environment would
		// otherwise make the diff "external diff died" (the -c diff.external= hardening blanks the
		// config but can't override the env var). Force git's internal diff so review always renders.
		_ = gitInteractive(repo, "diff", "--no-ext-diff", "HEAD..."+ref)
		return 0, nil
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
	if !validForkName(name) {
		return 2, fmt.Errorf("invalid fork name %q", name)
	}
	consult, rest := extractConsult(rest)
	// --credential picks the account, like plain `coop acp` / `coop <agent>` — accepted here too
	// so `coop fork <name> acp --credential p` works, like plain acp.
	profile, rest, err := extractRunProfile(rest)
	if err != nil {
		return 2, err
	}
	// --model pins the fork's ACP session model, like `coop acp --model` (an editor entry
	// can carry it); applied before acpCommand so gemini's own-binary adapter takes the flag.
	model, rest, err := extractRunModel(rest)
	if err != nil {
		return 2, err
	}
	agent := agents.Default()
	for _, x := range rest {
		switch {
		case agents.Valid(x):
			agent = x
		default:
			return 2, fmt.Errorf("usage: coop fork %s acp [%s] [--credential <name>] [--model <model>]", name, strings.Join(agents.Names(), "|"))
		}
	}
	if err := a.applyOneOff(agent, model, profile); err != nil {
		return 2, err
	}
	cmd, ok := acpCommand(a.cfg, agent)
	if !ok {
		return 2, errors.New("usage: coop fork <name> acp [claude|codex|gemini]")
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s (open it first: coop fork %s)", name, name)
	}
	lead := ""
	if consult {
		lead = agent
	}
	return box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: ws, Workdir: ws, Cmd: cmd, ForceNoTTY: true, Agent: agent, ConsultLead: lead,
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
func (a *app) forkBrief(repo, ws, name, ref string) {
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
		if len(a.cfg.Gate) == 0 {
			fmt.Printf("%s none configured (COOP_GATE)\n", ui.Bold("gate:"))
		} else {
			fmt.Printf("%s runs at merge — rolled back on failure\n", ui.Bold("gate:"))
		}
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
	if !validForkName(name) {
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
	// A running loop has the worktree bind-mounted RW; deleting it would orphan the worker +
	// container and strand the pidfile. Refuse (like merge/prune do) — or with --force, stop the
	// loop first so its container is reaped before the worktree goes.
	if len(runningForkNames(repo, []string{name})) > 0 {
		if !force {
			return 1, fmt.Errorf("fork %q is still running its loop — stop it first: coop fork stop %s (or use --force)", name, name)
		}
		if code, err := a.forkStop([]string{name}); err != nil {
			return code, err
		}
	}
	if err := forkRmSafe(forkUnmerged(repo, ws), gitDirty(ws), force); err != nil {
		return 1, err
	}
	// Confirm the (unrecoverable) delete — default-No at a TTY, refuse piped without --yes. Distinct
	// from --force above, which overrides the unmerged/dirty guard, not this prompt.
	if err := destroyGate("delete fork "+name, hasYes(args)); err != nil {
		return 2, err
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
	if !validForkName(name) {
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
	if !validForkName(name) {
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
