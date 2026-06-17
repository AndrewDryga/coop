package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

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

// forkVerbs are the reserved subcommands of `coop fork`; a fork can't be named one.
var forkVerbs = map[string]bool{
	"ls": true, "review": true, "merge": true, "rm": true, "open": true,
	"logs": true, "stop": true, "path": true,
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
	if name == "" || forkVerbs[name] {
		return false
	}
	if name == "." || name == ".." || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
}

// forkHelp prints the fork family usage (shown for `coop fork [...] -h|--help`).
func forkHelp() (int, error) {
	rows := []struct{ cmd, desc string }{
		{"coop fork <name> [agent]", "open or re-enter a fork + run an agent (re-entry continues the last session)"},
		{"coop fork <name> <agent> --loop --tasks <path>", "loop a tasks file unattended in the fork (add -d to detach)"},
		{"coop fork ls", "list this repo's forks: agent, branch, changes, state, last activity"},
		{"coop fork logs [name] [-f|--follow]", "tail a fork's loop log (no name tails every fork, prefixed)"},
		{"coop fork review <name> [--stat|--tool|--open]", "brief + diff; --tool = git difftool, --open = your editor"},
		{"coop fork <name> acp [agent]", "front the fork as an ACP agent over stdio (drive it from Zed)"},
		{"coop fork merge <name> [--all] [-f|--force] [-y|--yes]", "rebase the fork onto your branch and land it (--all = queue)"},
		{"coop fork rm <name> [-f|--force]", "discard a fork (refuses unmerged/dirty work without --force)"},
		{"coop fork open <name>", "open the fork in your editor ($COOP_EDITOR / git core.editor / …)"},
		{"coop fork path <name>", "print the fork's filesystem path"},
		{"coop fork stop <name>", "stop a detached loop"},
	}
	flags := []struct{ flag, desc string }{
		{"-c, --continue", "force-resume the prior session (the default on re-entry)"},
		{"    --new", "start a fresh agent session on re-entry"},
		{"    --fresh", "recreate the fork from scratch (new clone + session)"},
		{"-d, --detach", "with --loop, run it in the background"},
		{"-t, --tasks", "with --loop, path to the tasks file that seeds the queue (required)"},
		{"-f, --force", "merge / rm: override the gate, policy, or unmerged-or-dirty guard"},
		{"-y, --yes", "merge: confirm landing + fork removal (required without a TTY)"},
		{"-f, --follow", "logs: keep streaming new output"},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — a throwaway local clone handed to an agent; review and merge it like a PR.\n\n", ui.Bold("coop fork"))
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-50s %s\n", r.cmd, r.desc)
	}
	fmt.Fprintf(&b, "\n%s (every short flag has a long form):\n", ui.Bold("flags"))
	for _, f := range flags {
		fmt.Fprintf(&b, "  %-16s %s\n", f.flag, f.desc)
	}
	fmt.Fprintf(&b, "\n%s --open uses $COOP_EDITOR, else `git config core.editor`, else a detected\n", ui.Bold("review"))
	fmt.Fprintf(&b, "         GUI editor; --tool uses `git config diff.tool`. Setup details in the README.\n")
	fmt.Print(b.String())
	return 0, nil
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
		return a.forkCreate(args)
	}
}

// forkArgs is the parsed form of `coop fork <name> [agent] [flags]`.
type forkArgs struct {
	name       string
	agent      string
	agentSet   bool // an agent was given explicitly (vs defaulted / remembered from the fork)
	fresh      bool
	cont       bool // -c/--continue: force-resume the prior session (now the default on re-entry)
	newSession bool // --new: start a fresh agent session even when re-entering a fork
	loop       bool
	detach     bool
	tasks      string // --tasks <path>: the tasks file to seed the loop's queue (required with --loop)
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
			if i+1 >= len(rest) {
				return fa, errors.New("coop fork --tasks needs a path to a tasks file")
			}
			i++
			fa.tasks = rest[i]
		case strings.HasPrefix(x, "--tasks="):
			fa.tasks = strings.TrimPrefix(x, "--tasks=")
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
	// A loop must say which tasks file seeds its queue — no implicit name→file mapping.
	if fa.loop && fa.tasks == "" {
		return fa, fmt.Errorf("coop fork %s --loop needs --tasks <path> (the tasks file to seed the queue)", fa.name)
	}
	if !fa.loop && fa.tasks != "" {
		return fa, errors.New("coop fork --tasks only applies with --loop")
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
	if fa.tasks != "" { // resolve to an absolute path now, so a detached worker still finds it
		abs, err := filepath.Abs(fa.tasks)
		if err != nil {
			return -1, err
		}
		if !fileExists(abs) {
			return -1, fmt.Errorf("coop fork --tasks: no such file: %s", fa.tasks)
		}
		fa.tasks = abs
	}
	repo, img, err := a.resolveImage()
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, fa.name)
	existed := pathExists(ws) // a re-entry (vs a fresh fork) — resume by default below
	if fa.fresh && pathExists(ws) {
		if err := destroyFork(repo, fa.name); err != nil {
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
			return a.runForkLoop(repo, ws, fa.name, fa.agent, fa.tasks, true)
		case fa.detach:
			return a.detachForkLoop(repo, fa.name, fa.agent, fa.tasks)
		default:
			return a.runForkLoop(repo, ws, fa.name, fa.agent, fa.tasks, false)
		}
	}
	// Resume the agent's prior session by default when re-entering a fork (opt out with
	// --new; --fresh recreates the fork, so it starts new too). Falls back to a fresh
	// run when no session for this fork exists.
	cmd := a.defaultCmd(fa.agent)
	if (existed && !fa.fresh && !fa.newSession) || fa.cont {
		if ag, ok := agents.Get(fa.agent); ok {
			if rc, resumed := ag.Resume(a.cfg, ws); resumed {
				cmd = rc
				ui.Info("continuing your last %s session in this fork", fa.agent)
			}
		}
	}
	_, _ = box.Run(a.cfg, a.rt, box.RunSpec{
		Image: img, Repo: ws, Cmd: cmd, ConsultLead: fa.agent,
		Homes: a.cfg.Homes, Network: a.cfg.Network, Cache: a.cfg.Cache,
	})
	forkNextSteps(fa.name)
	return 0, nil
}

func forkNextSteps(name string) {
	ui.Info("review · merge · discard:")
	ui.Info("  coop fork review %s   coop fork merge %s   coop fork rm %s", name, name, name)
}

// setupFork creates the clone and its branch (the git half of forkCreate, with no
// agent run — so the lifecycle is testable without a container).
func setupFork(repo, name string) (string, error) {
	ws := forkWorkspace(repo, name)
	if err := os.MkdirAll(forkHome(repo), 0o755); err != nil {
		return ws, err
	}
	if err := gitClone(repo, ws); err != nil {
		return ws, fmt.Errorf("git clone: %w", err)
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
	// `--path` expands a leading ~ in the configured excludesfile.
	if gi := gitOut(repo, "config", "--path", "core.excludesfile"); gi != "" {
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

func saveForkAgent(ws, agent string) {
	if agent == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(forkAgentFile(ws)), 0o755); err != nil {
		return
	}
	if os.WriteFile(forkAgentFile(ws), []byte(agent+"\n"), 0o644) == nil {
		excludeFork(ws, ".coop/")
	}
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

func (a *app) forkLs(_ []string) (int, error) {
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	names := forkNames(repo)
	if len(names) == 0 {
		ui.Info("no forks yet — open one with 'coop fork <name>'")
		return 0, nil
	}
	const format = "  %-16s %-8s %-12s %-9s %-8s %-15s %s\n"
	// Bold the whole rendered line, not each cell: bolding a cell first would put ANSI
	// escape bytes inside the %-Ns width count and misalign the header against the rows.
	fmt.Print(ui.Bold(fmt.Sprintf(format, "NAME", "AGENT", "BRANCH", "STATE", "TASKS", "CHANGES", "UPDATED")))
	for _, n := range names {
		s := gatherForkStatus(repo, n)
		fmt.Printf(format, s.Name, s.Agent, s.Branch, s.stateCell(), s.tasksCell(), s.changesCell(), s.Updated)
	}
	return 0, nil
}

// forkBranch / forkUpdated read a fork's state (for `coop fork ls` and `coop status`).
// They run against an agent-controlled tree (post-work), so they use the hardened
// helpers — `diff`/`log` would otherwise fire a planted core.fsmonitor or diff.external.
func forkBranch(ws string) string { return gitOutFork(ws, "rev-parse", "--abbrev-ref", "HEAD") }

func forkUpdated(ws string) string {
	if rel := gitOutFork(ws, "log", "-1", "--format=%cr"); rel != "" {
		return rel
	}
	return "—"
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
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	if err := gitFetchInto(repo, ws, name); err != nil {
		return -1, fmt.Errorf("git fetch: %w", err)
	}
	ref := "review/" + name
	a.forkBrief(repo, ws, name, ref)

	switch {
	case open: // open the fork in your IDE; review via its SCM panel
		return a.openInEditor(repo, ws)
	case tool: // hand the diff to your configured GUI difftool (git config diff.tool)
		_ = gitInteractive(repo, "difftool", "HEAD..."+ref)
		return 0, nil
	case stat:
		return 0, nil // the brief already lists the files
	case a.cfg.ReviewCmd != "": // a user-defined review command
		return a.runReviewCmd(repo, ws, name, ref)
	default:
		_ = gitInteractive(repo, "diff", "HEAD..."+ref)
		return 0, nil
	}
}

// resolveEditor picks the command used to open a fork for review, in order:
// $COOP_EDITOR, then git's own core.editor (your explicit choice — local config
// beats global), then a detected GUI editor, then $VISUAL/$EDITOR. Returns "" if
// nothing is configured or found.
func resolveEditor(cfgEditor, repo string) string {
	if cfgEditor != "" {
		return cfgEditor
	}
	if e := gitOut(repo, "config", "core.editor"); e != "" {
		return e // honor `git config core.editor`, e.g. "zed --wait"
	}
	return detectEditor()
}

// openInEditor opens the fork directory in an editor so you can review via its SCM
// panel. See resolveEditor for how the editor is chosen.
func (a *app) openInEditor(repo, ws string) (int, error) {
	editor := resolveEditor(a.cfg.Editor, repo)
	if editor == "" {
		return 1, errors.New("no editor found — set $COOP_EDITOR, git config core.editor, or $VISUAL/$EDITOR (or install code/cursor/zed/idea)")
	}
	parts := append(strings.Fields(editor), ws)
	ui.Info("opening %s in %s", ws, parts[0])
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Run(); err != nil {
		return 1, fmt.Errorf("open %q: %w", parts[0], err)
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
	agent := agents.Default()
	for _, x := range rest {
		switch {
		case agents.Valid(x):
			agent = x
		default:
			return 2, fmt.Errorf("usage: coop fork %s acp [%s]", name, strings.Join(agents.Names(), "|"))
		}
	}
	cmd, ok := acpCommand(agent)
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
		Image: img, Repo: ws, Workdir: ws, Cmd: cmd, ForceNoTTY: true, ConsultLead: lead,
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

// forkBrief prints a review summary before the diff — commits, files changed, and
// the agent's own reasoning from the fork's .agent/LOG.md — so a reviewer gets a map
// before reading the patch.
func (a *app) forkBrief(repo, ws, name, ref string) {
	ins, del := parseShortstat(gitOut(repo, "diff", "--shortstat", "HEAD..."+ref))
	files := gitOut(repo, "diff", "--name-status", "HEAD..."+ref)
	nfiles := 0
	if files != "" {
		nfiles = len(strings.Split(files, "\n"))
	}
	ahead := gitOut(repo, "rev-list", "--count", "HEAD.."+ref)
	ui.Info("%s ← %s  ·  %s commit(s), +%d -%d across %d file(s)", ref, name, ahead, ins, del, nfiles)
	if log := gitOut(repo, "log", "--oneline", "--no-decorate", "HEAD.."+ref); log != "" {
		fmt.Println(ui.Bold("commits:"))
		fmt.Println(indent(log))
	}
	if files != "" {
		fmt.Println(ui.Bold("files:"))
		fmt.Println(indent(files))
	}
	if data, err := os.ReadFile(filepath.Join(ws, ".agent", "LOG.md")); err == nil {
		if why := lastLines(string(data), 12); strings.TrimSpace(why) != "" {
			fmt.Println(ui.Bold("why (.agent/LOG.md, latest):"))
			fmt.Println(indent(why))
		}
	}
	fmt.Println(ui.Bold("diff:"))
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
	sha := gitOutFork(ws, "rev-parse", "HEAD")
	if sha == "" {
		return false
	}
	return gitRun(repo, "merge-base", "--is-ancestor", sha, "HEAD") != nil
}

func (a *app) forkRm(args []string) (int, error) {
	name, force := "", false
	for _, x := range args {
		switch x {
		case "--force", "-f":
			force = true
		default:
			if strings.HasPrefix(x, "-") {
				return 2, fmt.Errorf("coop fork rm: unknown flag %q", x)
			}
			name = x
		}
	}
	if name == "" {
		return 2, errors.New("usage: coop fork rm <name> [--force]")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	if err := forkRmSafe(forkUnmerged(repo, ws), forkDirty(ws), force); err != nil {
		return 1, err
	}
	if err := destroyFork(repo, name); err != nil {
		return -1, err
	}
	ui.Info("removed fork %s", name)
	return 0, nil
}

// forkOpen prints a fork's path (for `cd "$(coop fork open <name>)"`).
// forkPath prints a fork's filesystem path (for `cd "$(coop fork path <name>)"` and the
// like). It's the plumbing companion to `coop fork open`, which opens it in your editor.
func (a *app) forkPath(args []string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 2, errors.New("usage: coop fork path <name>")
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, args[0])
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", args[0])
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
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return -1, err
	}
	ws := forkWorkspace(repo, name)
	if !pathExists(ws) {
		return -1, fmt.Errorf("no such fork: %s", name)
	}
	return a.openInEditor(repo, ws)
}
