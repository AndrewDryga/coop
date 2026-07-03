package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// helpRequested reports whether args contains -h or --help.
func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false // everything after -- is passthrough to the agent, not a request for coop's help
		}
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

func printHelp(cfg *config.Config) { fmt.Print(helpText(cfg)) }

// groupHelp prints a command group's focused help (its commandHelp entry) and returns exit
// 0 — the response to a bare `coop <group>` with no subcommand. A group must never answer a
// missing subcommand with an `unknown <group> command ""` error; that path is for a *mistyped*
// subcommand. See .agent/rules/bare-subcommand-shows-help.md.
func groupHelp(cmd string) (int, error) {
	if h, ok := commandHelp[cmd]; ok {
		printCommandHelp(h)
		return 0, nil
	}
	printHelp(config.Load())
	return 0, nil
}

// helpText renders the top-level command reference: one command per line, grouped, with a
// pointer to per-command help. Flags, sub-verbs, and examples live in `coop <cmd> --help`
// and the README, so this stays a clean, scannable overview.
func helpText(cfg *config.Config) string { return renderHelp(cfg, false) }

// renderHelp renders the top-level command reference. ref=true is the DETERMINISTIC reference form
// for docs/`coop help --all`: forced no-color, no state-aware FIRST RUN hint, and the canonical
// (compose-less) up/down rows — so the output is identical on every machine (gendocs -check depends
// on it). ref=false is the live, state-aware terminal view.
func renderHelp(cfg *config.Config, ref bool) string {
	var b strings.Builder
	p := ui.For(os.Stdout) // help is a stdout view — gate color on stdout so a pipe stays clean
	if ref {
		p = ui.Palette{} // forced plain: the reference must be byte-identical regardless of the terminal
	}
	group := func(label string) { fmt.Fprintf(&b, "\n%s\n", p.Bold(label)) }
	// row keeps a column gap even when a command is long, so a description never glues to it.
	// Width is counted in runes, not bytes, so a command with a "…" doesn't shift its column.
	row := func(cmd, desc string) {
		gap := 34 - utf8.RuneCountInString(cmd)
		if gap < 2 {
			gap = 2
		}
		fmt.Fprintf(&b, "  %s%s%s\n", cmd, strings.Repeat(" ", gap), desc)
	}
	// dimRow is row for a command not available in this repo right now (e.g. `coop up` with no
	// compose.agent.yml) — the whole line recedes (gap computed on plain text, then dimmed, so the
	// command column still aligns). Dim is a no-op when color is off, so a pipe keeps the text.
	dimRow := func(cmd, desc string) {
		gap := 34 - utf8.RuneCountInString(cmd)
		if gap < 2 {
			gap = 2
		}
		fmt.Fprintf(&b, "  %s\n", p.Dim(cmd+strings.Repeat(" ", gap)+desc))
	}

	if ref { // the reference omits the build version — its bytes must not depend on the tag/commit
		fmt.Fprintf(&b, "%s — run a coding agent all night long in a box it can't escape.\n", p.Bold("coop"))
	} else {
		fmt.Fprintf(&b, "%s %s — run a coding agent all night long in a box it can't escape.\n", p.Bold("coop"), resolveVersion())
	}
	fmt.Fprint(&b, "Usage: coop <command> [args]\n")
	// A newcomer (no agent signed in) gets the day-one order up front. Pure-local check (no runtime),
	// so `coop help` still works before Docker exists — same state-aware style as the up/down rows below.
	if !ref && !anyAgentSignedIn(cfg) {
		fmt.Fprintf(&b, "\n%s  set up in order:  coop build → coop login <agent> → coop doctor\n", p.Bold("FIRST RUN"))
	}

	group("AGENTS")
	row("coop claude|codex|gemini [args]", "a sandboxed agent (its flags + your args)")
	row("coop fusion [agent]", "one leads, the others advise + synthesize")
	row("coop run -- <cmd...>", "run a raw command in the box")
	row("coop shell", "an interactive shell in the box")
	row("coop login <agent> [--credential <name>]", "sign in an agent (a subscription)")
	row("coop credentials [agent]", "stored credentials + which are signed in")
	row("coop presets [name]", "orchestration recipes (lead + roles)")
	row("coop models [agent]", "the model menu per agent")
	row("coop acp [agent|fusion]", "serve as an editor agent (ACP; e.g. Zed)")
	row("coop <agent> --consult", "a read-only second opinion from the others")
	row("coop <agent> --model <model>", "run on a chosen model (fusion/fork/loop too)")

	group("FORKS — review and land work like a PR")
	row("coop fork <name> [agent]", "open or re-enter a fork and run an agent")
	row("coop fork ls", "list this repo's forks")
	row("coop fork review <name>", "show a fork's brief + diff")
	row("coop fork merge <name>", "rebase the fork onto your branch and land it")
	row("coop fork logs [name]", "tail a fork's loop log (no name: every fork)")
	row("coop fork rm <name>", "discard a fork")
	row("coop fork stop <name>", "stop a detached loop")
	row("coop fork open <name>", "open the fork in your editor")
	row("coop fork path <name>", "print the fork's filesystem path")

	group("UNATTENDED")
	row("coop loop [agent] [--tasks <path>]", "work the queue(s) until done, then audit")
	row("coop loop pool add|rm|clear", "subscriptions to rotate when rate-limited")
	row("coop fleet init|up|down|split|watch|prune", "parallel forks from .agent/fleet")

	group("TASKS — a folder-per-task queue in .agent/tasks/")
	row("coop tasks ls", "show the queue, grouped by state")
	row("coop tasks watch", "live board of the queue + active forks")
	row("coop tasks add \"<title>\"", "add a task (then claim/block/unblock/done)")
	row("coop tasks decisions", "what's blocked on a decision (-i to answer)")

	group("SETUP & MAINTENANCE")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, skills, subagents")
	row("coop build", "build the box image (stable, pinned)")
	row("coop update", "self-update coop, then rebuild the box")
	row("coop completion <shell>", "shell tab-completion (bash, zsh)")
	// `coop up`/`down` act on this repo's compose.agent.yml — always NAME the file (it's what makes
	// the rows obvious), listing its real services when present, and dim the pair when there's none to
	// act on. Repo resolution is best-effort (help runs anywhere; outside a repo, or with no compose).
	repo, _ := box.ResolveRepo(cfg.RepoOverride)
	switch {
	case ref: // the reference form is machine-independent: no per-repo service list
		row("coop up", "start the compose.agent.yml services")
		row("coop down", "stop the compose.agent.yml services")
	case len(scaffold.ComposeServiceNames(box.ComposeFile(repo))) > 0:
		services := scaffold.ComposeServiceNames(box.ComposeFile(repo))
		row("coop up", "start the compose.agent.yml services ("+strings.Join(services, ", ")+")")
		row("coop down", "stop the compose.agent.yml services")
	default:
		dimRow("coop up", "none in compose.agent.yml yet")
		dimRow("coop down", "stop the compose.agent.yml services")
	}
	row("coop doctor", "attack the box, prove isolation holds")
	row("coop check-secrets", "scan the working tree for committed secrets")
	row("coop help", "this help")
	row("coop version", "print the version")

	fmt.Fprint(&b, "\nRun 'coop help <command>' or 'coop <command> --help' for a command's details —\n"+
		"for an agent (claude/codex/gemini), --help is the agent's own.\n")
	if ref { // machine-independent footer for the reference/manual (no host-specific paths)
		fmt.Fprint(&b, "\nConfig  coop.conf (COOP_CONF), or COOP_* env vars\nAuth    the config dir (COOP_CONFIG_DIR)\nDocs    https://coop.dryga.com\n")
	} else {
		fmt.Fprintf(&b, "\nConfig  %s, or COOP_* env vars\nAuth    %s\nDocs    https://coop.dryga.com\n",
			tildeify(filepath.Join(cfg.BoxHome, "coop.conf")), tildeify(cfg.ConfigDir))
	}

	return b.String()
}

// anyAgentSignedIn reports whether any agent has a signed-in credential — its default/flat login or a
// named profile. Pure-local (reads the config dir + env file, no runtime), so `coop help` can key its
// FIRST RUN hint on it without breaking the runtime-free help path.
func anyAgentSignedIn(cfg *config.Config) bool {
	for _, agent := range agents.Names() {
		if box.ProfileAuthed(cfg, agent, cfg.DefaultProfileOf(agent)) {
			return true
		}
		for _, p := range cfg.Profiles(agent) {
			if box.ProfileAuthed(cfg, agent, p) {
				return true
			}
		}
	}
	return false
}

// RenderManual is the entire CLI reference as ONE deterministic, plain-text document: the reference-
// form top-level overview, then every command's page in a stable order. It's the single source shared
// by `coop help --all`, docs/cli.md, and site/llms.txt — so terminal, docs, and the offline reference
// are provably identical (tools/gendocs -check enforces it). Plain (ui.Palette{}) and state-free, so
// its bytes never depend on the terminal, the repo's compose file, the config paths, or logins.
func RenderManual(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString(renderHelp(cfg, true))
	b.WriteString("\n" + strings.Repeat("=", 78) + "\n\n")
	b.WriteString(forkHelpText(ui.Palette{}) + "\n")
	b.WriteString(runHelp + "\n")
	for _, name := range topLevelCommands { // stable order; fork/run have their own pages above
		if h := commandHelp[name]; h != "" {
			b.WriteString("\n" + h + "\n")
		}
	}
	return b.String()
}

// runHelp is `coop run`'s page. It's deliberately NOT in commandHelp: the dispatch's help check
// is `--`-blind, so `coop run -- --help` (which must run --help in the box) would otherwise print
// this instead. cmdRun (which honors --) and helpForCommand print it directly.
const runHelp = `coop run — run a raw command in the box.

  Usage: coop run -- <cmd...>

  Everything after -- runs verbatim in the sandbox — same mounts, secret-shadowing,
  and network as an agent. "coop run echo hi" works too; use -- when the command has
  flags coop would otherwise read (e.g. coop run -- npm test --watch).`

// agentHelp is `coop help <agent>`: it documents coop's OWN wrapper flags (the ones coop consumes
// before a --), since `coop <agent> --help` forwards to the agent's real CLI. Kept short per
// help-output-style — the detail is in coop credentials / coop models.
const agentHelp = `coop <agent> — run a sandboxed coding agent (claude, codex, or gemini).

  Usage: coop <agent> [coop flags] [-- <agent args>]

  These flags are coop's own, read before a -- (everything after -- goes to the agent):
    --credential <name>  run on a stored credential — one account/login (see coop credentials)
    --model <name>       run on a chosen model (see coop models)
    --preset <name>      run under an orchestration preset from .agent/presets/<name>/
                         (lead + roles + models + credentials; see coop help presets)
    --consult            add a read-only second opinion from the other agents on a hard call
    --                   pass the rest verbatim to the agent, e.g. coop claude -- --help

  Sign in first with 'coop login <agent>'. For the agent's own flags: coop <agent> -- --help.`

// commandHelp is the focused text for `coop <cmd> --help`, per subcommand. fork has its
// own richer forkHelp, run has runHelp, and the agents use agentHelp (their `--help` forwards to
// the agent's own CLI). Each value's first line is the synopsis.
var commandHelp = map[string]string{
	"shell": `coop shell — open an interactive shell in the box.

  Usage: coop shell

  A shell in the sandbox at your repo — same mounts, secret-shadowing, and
  network as an agent run. Exit to return.`,

	"login": `coop login <agent> — sign in to an agent (token persists in the config dir).

  Usage: coop login <claude|codex|gemini> [--credential <name>]

  Runs the agent's sign-in (paste a code, no browser). Re-run any time to
  refresh or switch accounts — e.g. after a usage limit.

  --credential <name> signs in a second (or third) account under a name, so
  one agent can hold several subscriptions. The unattended loop rotates across a
  repo's credentials when one is rate limited (see 'coop loop pool'). Without the
  flag the sign-in targets the default credential.`,

	"credentials": `coop credentials — list stored credentials; a path grammar edits one.

  Usage: coop credentials [agent [credential]]
         coop credentials <agent> <credential> model [<model> | --clear]
         coop credentials <agent> <credential> default
         coop credentials <agent> <credential> rm

  A CREDENTIAL is one stored account/login — its own rate-limit pool. (Orchestration
  recipes are PRESETS; see coop help presets. 'coop profiles' was the pre-v3 name.)
  Each token narrows: no args lists every agent, an agent lists its credentials
  (signed in? default? marked model?), a credential shows its detail, and a trailing
  attribute reads or writes one property of it. A credential is one subscription;
  add more with 'coop login <agent> --credential <name>', then let the loop rotate
  across them on a rate limit ('coop loop pool').

  model [<m> | --clear]  the credential's default model — every run on it
                         (interactive, loop, fork, consult peer) uses it unless
                         --model overrides; bare 'model' prints the current mark.
                         E.g. work on the big model, personal on a cheap one:
                             coop credentials claude work model opus
                             coop credentials claude personal model haiku
                         See 'coop models' for the menu and the precedence.
  default                mark this credential as what a plain 'coop <agent>' runs —
                         a mark you set, not whichever is named "default".
  rm                     delete the credential (its login token, session history,
                         and model mark). Set a different default first if
                         you're removing the marked one.

  Run on a specific credential without changing the default — works on any agent
  launch: 'coop claude --credential <name>', 'coop fusion <agent> --credential
  <name>', and 'coop acp <agent> --credential <name>' (so an editor entry can pin
  an account). An agent's own --profile goes after a --, e.g.
  'coop codex -- --profile <name>'.`,

	"models": `coop models [agent] — the model menu per agent.

  Usage: coop models [claude|codex|gemini]

  One line per agent with its known models (examples — model ids churn, so ANY
  id the agent's CLI accepts works), then how to pick one. Per-credential marks
  live in 'coop credentials' (shown as a column there); set one with
  'coop credentials <agent> <credential> model <model>'.

  Pick per run instead with --model on any launch: 'coop claude --model fable',
  'coop fusion claude --model opus', 'coop loop --model haiku',
  'coop fork risky claude --model opus', 'coop acp claude --model sonnet'.

  Precedence: --model flag > the pool target's model (a loop's credential@model)
  > the preset lead's model > COOP_LOOP_MODEL (loop runs) > the credential's mark >
  COOP_<AGENT>_MODEL (agent-wide) > a model baked into COOP_<AGENT>_CMD > the
  agent CLI's own default. coop never validates a model id — a bad one fails in
  the agent's own error.`,

	"pool": `coop loop pool — which credentials this repo's loops rotate.

  Usage: coop loop pool                                     show this repo's pool
         coop loop pool add <agent> <credential[@model]>...   add rotation targets
         coop loop pool rm  <agent> <credential[@model]>...   drop rotation targets
         coop loop pool clear <agent>                       drop the agent's pool

  When an unattended loop hits a rate/usage limit it switches to the next pool
  target and keeps going, so a long run survives a subscription cap. A target is a
  credential (a stored account) with an optional model: 'work@opus, work@sonnet,
  other' falls back from opus to sonnet on the SAME login (no re-auth) before
  rotating to the other account — limits are tracked per target, so work@sonnet
  stays available while work@opus cools down. A bare credential runs its own
  default model (its mark, else COOP_LOOP_MODEL, else the agent default).

  With no pool set the loop rotates across ALL signed-in credentials for the
  agent. Pools are per-repo and stored outside the repo (names only). It's a
  setting of the loop — hence the home under 'coop loop' (the old top-level
  'coop pool' was retired in v3).`,

	"acp": `coop acp [agent|fusion] — serve as an ACP agent over stdio (for editors).

  Usage: coop acp [claude|codex|gemini | fusion [agent]] [--credential <name>] [--model <model>] [--preset <name>] [--supervise] [--consult]

  Speaks the Agent Client Protocol on stdin/stdout. Point your editor's ACP
  command at e.g. ["acp","claude"] — one entry per agent or governor.

  --credential <name> pins the session to one stored account, so an
  editor can run two entries on different ones, e.g. ["acp","claude","--credential","work"].

  --model <m> pins the session's model (see 'coop models'), e.g.
  ["acp","claude","--model","opus"].

  --preset <name> runs the session under an orchestration preset (its lead is the
  default agent when none is named; see 'coop help presets').

  --consult lets the session ask the other signed-in agents for a read-only second
  opinion (their credentials are mounted) — the orchestrator pattern, from your editor.

  --supervise keeps the editor connected across a box restart: it runs the agent
  in a child and, if the container dies, starts a new one and replays the ACP
  handshake (initialize + session/load) so the editor doesn't see a crash. Set it
  in your editor's args, e.g. ["acp","claude","--supervise"].`,

	"fusion": `coop fusion [agent] — one agent leads, the other two advise, it synthesizes.

  Usage: coop fusion [claude|codex|gemini] [--credential <name>] [--model <model>] [--preset <name>] [args...]

  Defaults to COOP_FUSION_GOVERNOR. Peers advise read-only; only the leader
  writes. Lighter, opt-in variant: coop <agent> --consult

  --credential <name> pins the governor's stored account; each peer keeps its own.

  --model picks the governor's model; each peer keeps its own default (its
  profile's mark or COOP_<AGENT>_MODEL — see 'coop models').

  --preset <name> loads an orchestration preset: its lead is the default governor,
  and its role routing rides along with the council directive ('coop help presets').

  Like coop <agent>, it forwards extra args to the governor — a leading agent name
  picks the governor; anything else (or anything after a --) passes through.`,

	"presets": `coop presets — YAML orchestration recipes under .agent/presets/<name>/.

  Usage: coop presets [name]        list them, or show one recipe in full

  A PRESET is a runtime recipe: which agent leads, and which roles it can route
  work to — each role an agent + model + credentials + routing hints. A CREDENTIAL
  is just a stored account/login (a rate-limit slot; see coop credentials). Presets
  reference credentials; they never store secrets.

  Load one with --preset <name> on: coop <agent> · loop · fusion · acp ·
  fork <name> --loop — or per fork in .agent/fleet.yaml (preset: <name>).
  An explicitly named agent wins over the preset's lead; explicit --model/
  --credential win over the preset's values.

  .agent/presets/frontier/preset.yaml:

    lead:
      agent: claude
      model: claude-fable-5
      credentials: [work]
      prompt: lead.md            # optional Markdown, appended to the generated contract
    roles:
      thinker:                   # native Claude subagent — deep thinking in-session
        mode: native
        agent: claude
        model: claude-opus-4-8
        subagent: deep-reasoner
        when: [architecture, debugging, code-review]
      critic:                    # read-only peer via coop-consult
        mode: consult
        agent: codex
        model: gpt-5.5
        credentials: [work]
        when: [plan-review, security]
      fast:                      # write-capable delegate via coop-delegate
        mode: delegate
        agent: gemini
        model: gemini-3.5-flash
        credentials: [work]
        when: [boilerplate, bulk-edits, test-scaffolding]
        commit: never            # the delegate edits; the LEAD reviews, gates, commits
        concurrent: never        # delegate runs are serialized

  coop generates the lead's routing contract from this (roles, when-to-use, the
  exact coop-consult/coop-delegate invocations) and mounts the wrappers. Markdown
  prompt files (lead.md, roles/<name>.md) append to the generated text, never
  replace it. A delegate may edit the worktree but must not commit — coop-delegate
  fails loud if HEAD moved — and the lead owns the diff review, the gate, and the
  commit. Model ids for the recipe: coop models.`,

	"fleet": `coop fleet — run a declarative fleet of forks from .agent/fleet.yaml.

  Usage: coop fleet <init|up|down|split|watch|prune>

  init           write a .agent/fleet.yaml template
  up             start every fork in the fleet, looping its tasks, detached
  down           stop the fleet's running loops
  split <n>      slice the queue into n forks (= coop tasks split) + write .agent/fleet.yaml
  watch          live dashboard of every fork's progress (auto-exits when the fleet's
                 done; Ctrl-C anytime). Task-centric view: coop tasks watch
  prune          remove forks no longer in the fleet file (kept: running, dirty, or
                 unmerged — pass --force to remove those too)

  up and down take --prune (with optional --force) to prune in the same step.

  .agent/fleet.yaml is a forks: map — each fork needs tasks: (the tree that seeds its
  loop) and may set agent:, preset: (an orchestration preset; its lead is the fork's
  default agent), credentials: (rotated on a rate limit — give each fork a DIFFERENT
  account so they run in parallel instead of contending; a member may carry a model
  for same-account fallback: "work@opus" or {name: work, model: opus}), model:, and
  consult: true (iterations may ask the other signed-in agents read-only):

    forks:
      core:
        tasks: .agent/tasks.core
        preset: frontier
        credentials: [work]

  Per-fork credentials/model/consult override the preset for that fork only. The
  pre-v3 one-line .agent/fleet is NOT read — its presence is an error until you
  translate it (see MIGRATING.md) and delete it. List forks: coop fork ls`,

	"tasks": `coop tasks — drive the task queue (a folder per task under .agent/tasks/).

  Usage: coop tasks [--tasks <path>]... <command>

  ls [--all]       list tasks by state, with counts (recent done capped; --all shows all)
  watch            live board: the queue + any active forks, deduped by id (auto-exits when done).
                   Per-fork fleet view: coop fleet watch
  add "<title>"    scaffold a task folder in todo (or fill it inline: --context/--acceptance/--approach/--subtask)
  claim <id>       claim a task before you start it (todo -> in_progress)
  block <id>       park it on a decision (-> blocked) and write a decision.md stub
  unblock <id>     move it back to todo (claim it to start); add "<answer>" to record in decision.md
  done <id>        move it to done (the archive)
  path <id>        print a task's resolved folder path
  rm <id>          delete a task folder; --all-done (alias: clear) clears the done archive
  decisions [-i]   list open decisions; -i walks them one by one to answer (records + unblocks)
  lint             check the tree (blocked<->decision.md, no status field, ...; exits 1)
  split <n>        slice the todo tasks into n trees (.agent/tasks.sliceN); coop fleet split makes a fleet

  A task's state is its directory — 00_todo/ 10_in_progress/ 50_blocked/ 99_done/, the
  numeric prefix just sorts 'ls' in lifecycle order — so each transition is a folder move.
  Removing tasks is a MANUAL step: the loop and skills only ever move a finished task to
  done, never delete it, so 'coop tasks rm --all-done' is how you prune the archive.
  Defaults to .agent/tasks/; point --tasks at another tasks dir, or set COOP_TASKS. Paths
  are repo-relative. With several queues configured (a monorepo), ls, lint, and decisions
  (including -i) roll up across all of them, and the id commands (claim/block/unblock/done/
  rm) find their task in whichever queue holds it — erroring only when an id matches in
  more than one queue (split slices share ids with their source). Only add and split need
  a single --tasks, since they create into a queue.`,

	"check-secrets": `coop check-secrets — scan the working tree for committed secrets, by content.

  Usage: coop check-secrets [--include-ignored]

  Scans for token shapes and high-entropy values, reporting file:line. Exits
  non-zero on a hit, for use as a pre-flight or CI check. Hide a flagged file
  with .coopignore.

  By default it scans the commit-candidate files (tracked + untracked; gitignored
  excluded). A 'coop run'/'shell'/'loop' mounts the WHOLE tree, though, so a
  gitignored-but-not-shadowed file is still visible to the agent — pass
  --include-ignored to scan the full visible tree too (deps/build dirs and
  shadowed files are still skipped).`,

	"loop": `coop loop [agent] — work the task queue until done, then audit.

  Usage: coop loop [claude|codex|gemini] [--tasks <path>]... [--model <model>] [--preset <name>] [--consult] [--preflight] [--debug-on-fail]

  A fresh agent per iteration works the todo tasks; when the queue empties, an
  auditor re-checks every shipped task. On a rate limit it switches to another
  signed-in credential (see 'coop loop pool'), or waits out the reset when there's only one.

  --preset <name> runs the loop under an orchestration preset: its lead is the
  default agent, its lead credentials the rotation pool, and each iteration gets
  the preset's role routing + wrappers ('coop help presets').

  --model <m> pins the loop's model; else the preset lead's model; COOP_LOOP_MODEL
  is the standing default below those — so overnight runs can grind on a cheaper
  model than your interactive sessions. With none, each iteration uses the rotated
  credential's marked default ('coop models'), then COOP_<AGENT>_MODEL.

  --consult lets each iteration ask the other signed-in agents for a read-only
  second opinion (coop-consult on PATH, peers' credentials mounted) — the
  orchestrator pattern running unattended. Off by default: it widens each box's
  credential scope to the authed peers. Also on fork loops:
  coop fork <name> <agent> --loop --consult.

  On macOS, coop holds a caffeinate assertion for the run so the machine doesn't
  idle-sleep mid-drain and stall an overnight loop (COOP_CAFFEINATE=0 to disable).

  Ctrl-C is a soft stop: the current iteration finishes and commits, then the loop
  stops before claiming the next task. Press Ctrl-C again to stop now (tearing the
  running box down). (A detached fork has no terminal — stop it with 'coop fork stop'.)

  Defaults to .agent/tasks/. Repeat --tasks (or set COOP_TASKS) to drain several
  queues at once — the loop keeps going while any
  has unfinished work, so one loop can cover a monorepo's components. The whole repo is
  still mounted.

  --preflight       run one cleanup pass before working: unblock blocked/ tasks whose
                    decision now has an answer (opt-in; COOP_PREFLIGHT=1 to default it
                    on, --no-preflight to override). Makes no code changes or commits.
  --debug-on-fail   on a failure at a terminal, open a box shell, then retry
                    on exit (a no-op in unattended runs)

  Exit codes: 0 = queue verified done (or the audit reopened work — re-run); 1 = failure;
  2 = usage; 3 = stopped with a task blocked on a human decision (resolve with
  'coop tasks decisions', then re-run). So cron/CI can branch without parsing output.

  COOP_LOOP_CMD overrides the per-iteration command.`,

	"up": `coop up — start the repo's sibling services so the box can reach them by name.

  Usage: coop up

  Brings up the services in compose.agent.yml on coop's network; an agent in
  the box reaches db/redis/... by hostname. Stop them with: coop down`,

	"down": `coop down [-v] — stop the repo's sibling services.

  Usage: coop down [-v | --volumes]

  -v, --volumes   also remove the services' volumes (their data)`,

	"init": `coop init [--stack asdf] — scaffold coop's working set into the repo.

  Usage: coop init [--stack asdf] [--services postgres,redis]

  Writes AGENTS.md, the .agent/ queue, the Claude + git commit hooks, the
  workflow skills, and two starter subagents for the orchestrator pattern —
  .claude/agents/deep-reasoner.md (pinned to Opus, for reasoning-heavy phases)
  and fast-worker.md (pinned to Sonnet, for mechanical work) — so a lead on a
  bigger model spends its tokens on planning and synthesis (see the README's
  "orchestrator pattern"). The commit hooks' format gate matches the repo's stack —
  detected from go.mod / *.tf / mix.exs / Cargo.toml or .tool-versions (gofmt,
  terraform fmt, mix format, cargo fmt). With nothing detected the gate is left
  neutral (it imposes no checks); at a terminal it asks which gate to add. A
  .tool-versions (or --stack asdf) also scaffolds an asdf Dockerfile.agent.
  Sibling services (db/redis) are opt-in: at a terminal it asks which to add as a
  compose.agent.yml — none by default, or pass --services. If the repo already has
  its own Docker and no Dockerfile.agent yet, it suggests how to build the box on it.
  Also seeds an empty ~/.config/coop/agents/mcp.json (the shared MCP source of truth,
  inert until you add a server) so there's an obvious place to declare MCP servers.
  Never clobbers existing files.`,

	"doctor": `coop doctor — prove the box's isolation: attack it, inside and from the host.

  Usage: coop doctor

  Runs the escape/leak checks — secret shadowing, network limits, host reach,
  the fork handoff — and prints a pass/fail report. Honors COOP_RUNTIME.`,

	"build": `coop build — build the box image (stable, pinned).

  Usage: coop build

  Builds the shared base, or a per-project image if the repo has a
  Dockerfile.agent — pinning versions for reproducibility. Re-run after
  changing Dockerfile.agent or .tool-versions. For the latest, use coop update.

  New runs use the fresh image automatically. Supervised editor sessions
  (coop acp --supervise) are restarted onto it transparently — they reconnect, so
  you don't lose the session. Other running boxes (a loop, an un-supervised
  session) keep the old image until they next start.`,

	"update": `coop update — self-update coop, then rebuild the box image fresh.

  Usage: coop update [--self-only | --box-only | --check]

  First replaces the coop binary with the latest GitHub release — fetched and
  verified the same way install.sh does (checksum + cosign), then swapped in
  atomically so replacing the running binary is safe. Then rebuilds the image like
  coop build but --pull --no-cache and unpinned, so the node base and agent CLIs
  jump to latest. Use coop build for a reproducible image. Supervised editor
  sessions are restarted onto the new image transparently, same as build.

    --self-only   update just the coop binary, skip the image rebuild
    --box-only    rebuild just the image, skip the self-update (the old behavior)
    --check       dry-run: report the binary vs the latest release and the box
                  image's build age/staleness, changing nothing (no runtime needed)

  A dev/source build, an already-current binary, or a coop installed somewhere
  unwritable (a package-manager prefix) skips the self-update with a note.

  Once a day, any TTY command also checks for a newer release in the background
  and mentions it after the command's output; COOP_NO_UPDATE_CHECK=1 turns that
  notice off.`,

	"version": `coop version — print coop's version and exit.

  Usage: coop version

  Prints coop's build version (git tag or commit). Takes no arguments; -v and
  --version are aliases.`,

	"completion": `coop completion <bash|zsh> — print a shell completion script.

  Usage: coop completion <bash|zsh>

  bash: coop completion bash > ~/.local/share/bash-completion/completions/coop
  zsh:  coop completion zsh  > "${fpath[1]}/_coop"   (then restart your shell)

  Completes commands and verbs, and — via a hidden 'coop __complete' — live values
  (fork names, task ids, credentials), all from local reads.`,
}

// printCommandHelp prints one subcommand's focused help: synopsis line bolded, body as-is,
// then a pointer to the full command list.
func printCommandHelp(text string) {
	p := ui.For(os.Stdout) // stdout view — keep pipes clean
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		fmt.Println(p.Bold(text[:i]) + text[i:])
	} else {
		fmt.Println(p.Bold(text))
	}
	fmt.Println("\nRun 'coop help' for all commands.")
}

// tildeify shortens a path under the home dir to ~/… for readable help.
func tildeify(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join("~", rel))
		}
	}
	return path
}
