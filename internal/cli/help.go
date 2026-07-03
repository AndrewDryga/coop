package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/scaffold"
	"github.com/AndrewDryga/coop/internal/ui"
)

// helpRequested reports whether args contains -h or --help.
func helpRequested(args []string) bool {
	for _, a := range args {
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
func helpText(cfg *config.Config) string {
	var b strings.Builder
	p := ui.For(os.Stdout) // help is a stdout view — gate color on stdout so a pipe stays clean
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

	fmt.Fprintf(&b, "%s %s — run a coding agent all night long in a box it can't escape.\n", p.Bold("coop"), resolveVersion())
	fmt.Fprint(&b, "Usage: coop <command> [args]\n")

	group("AGENTS")
	row("coop claude|codex|gemini [args]", "a sandboxed agent (its autonomous flags + your args)")
	row("coop fusion [agent]", "a council: one agent leads, the others advise, it synthesizes")
	row("coop run -- <cmd...>", "run a raw command in the box")
	row("coop shell", "an interactive shell in the box")
	row("coop login <agent> [--profile p]", "authenticate an agent (a profile = one subscription)")
	row("coop profiles [agent]", "list stored credential profiles and which are signed in")
	row("coop models [agent]", "the model menu per agent (profile marks live in coop profiles)")
	row("coop acp [agent|fusion]", "serve as an ACP agent over stdio (for editors like Zed)")
	row("coop <agent> --consult", "add a read-only second opinion from the other agents on a hard call")
	row("coop <agent> --model <m>", "run on a chosen model (works on fusion/fork/loop/acp too)")

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
	row("coop loop [agent] [--tasks p]…", "work the queue(s) until done, then audit")
	row("coop loop pool add|rm|clear", "pick which subscriptions the loop rotates on a rate limit")
	row("coop fleet init|up|down|split|watch|prune", "drive a fleet of forks from .agent/fleet")

	group("TASKS — a folder-per-task queue in .agent/tasks/")
	row("coop tasks ls", "show the queue, grouped by state")
	row("coop tasks watch", "live board: the queue + any active forks, deduped (auto-exits when done)")
	row("coop tasks add \"<title>\"", "add a task; claim/block/unblock/done move it through its states")
	row("coop tasks decisions", "show what's blocked on a human decision (-i to answer them)")

	group("SETUP & MAINTENANCE")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, skills, and starter subagents")
	row("coop build", "build the box image (stable, pinned)")
	row("coop update", "self-update coop, then rebuild the box image fresh (latest base + agents)")
	// `coop up`/`down` act on this repo's compose.agent.yml — name its real services, and dim the
	// pair when there's no compose file to act on. Repo resolution is best-effort (help runs
	// anywhere; outside a repo, or with no compose, the rows dim).
	repo, _ := box.ResolveRepo(cfg.RepoOverride)
	if services := scaffold.ComposeServiceNames(box.ComposeFile(repo)); len(services) > 0 {
		row("coop up", "start the compose.agent.yml services ("+strings.Join(services, ", ")+")")
		row("coop down", "stop the compose.agent.yml services")
	} else {
		dimRow("coop up", "start sibling services (none in compose.agent.yml yet)")
		dimRow("coop down", "stop sibling services")
	}
	row("coop doctor", "prove isolation — attack the box, check it holds")
	row("coop check-secrets", "scan the working tree for committed secrets")
	row("coop help", "this help")
	row("coop version", "print the version")

	fmt.Fprint(&b, "\nRun 'coop help <command>' or 'coop <command> --help' for a command's details —\n"+
		"for an agent (claude/codex/gemini), --help is the agent's own.\n")
	fmt.Fprintf(&b, "\nConfig  %s, or COOP_* env vars\nAuth    %s\nDocs    https://coop.dryga.com\n",
		tildeify(filepath.Join(cfg.BoxHome, "coop.conf")), tildeify(cfg.ConfigDir))

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

// commandHelp is the focused text for `coop <cmd> --help`, per subcommand. fork has its
// own richer forkHelp, run has runHelp, and the agents aren't here so `--help` forwards to the
// agent's own CLI. Each value's first line is the synopsis.
var commandHelp = map[string]string{
	"shell": `coop shell — open an interactive shell in the box.

  Usage: coop shell

  A shell in the sandbox at your repo — same mounts, secret-shadowing, and
  network as an agent run. Exit to return.`,

	"login": `coop login <agent> — sign in to an agent (token persists in the config dir).

  Usage: coop login <claude|codex|gemini> [--profile <name>]

  Runs the agent's sign-in (paste a code, no browser). Re-run any time to
  refresh or switch accounts — e.g. after a usage limit.

  --profile <name> signs in a second (or third) account into a named profile, so
  one agent can hold several subscriptions. The unattended loop rotates across a
  repo's profiles when one is rate limited (see 'coop loop pool'). Without --profile
  the sign-in targets the default profile.`,

	"profiles": `coop profiles — list credential profiles; a path grammar edits one.

  Usage: coop profiles [agent [profile]]
         coop profiles <agent> <profile> model [<model> | --clear]
         coop profiles <agent> <profile> default
         coop profiles <agent> <profile> rm

  Each token narrows: no args lists every agent, an agent lists its profiles
  (signed in? default? marked model?), a profile shows its detail, and a trailing
  attribute reads or writes one property of it. A profile is one subscription;
  add more with 'coop login <agent> --profile <name>', then let the loop rotate
  across them on a rate limit ('coop loop pool').

  model [<m> | --clear]  the profile's default model — every run on that profile
                         (interactive, loop, fork, consult peer) uses it unless
                         --model overrides; bare 'model' prints the current mark.
                         E.g. work on the big model, personal on a cheap one:
                             coop profiles claude work model opus
                             coop profiles claude personal model haiku
                         See 'coop models' for the menu and the precedence.
  default                mark this profile as what a plain 'coop <agent>' runs —
                         a mark you set, not whichever is named "default".
  rm                     delete the profile (its login token, session history,
                         and model mark). Set a different default first if
                         you're removing the marked one.

  Run on a specific profile without changing the default — works on any agent launch:
  'coop claude --profile <name>', 'coop fusion <agent> --profile <name>', and
  'coop acp <agent> --profile <name>' (so an editor entry can pin an account). An
  agent's own --profile goes after a --, e.g. 'coop codex -- --profile <name>'.`,

	"models": `coop models [agent] — the model menu per agent.

  Usage: coop models [claude|codex|gemini]

  One line per agent with its known models (examples — model ids churn, so ANY
  id the agent's CLI accepts works), then how to pick one. Per-profile marks
  live in 'coop profiles' (shown as a column there); set one with
  'coop profiles <agent> <profile> model <model>'.

  Pick per run instead with --model on any launch: 'coop claude --model fable',
  'coop fusion claude --model opus', 'coop loop --model haiku',
  'coop fork risky claude --model opus', 'coop acp claude --model sonnet'.

  Precedence: --model flag > COOP_LOOP_MODEL (loop runs) > the profile's mark >
  COOP_<AGENT>_MODEL (agent-wide) > a model baked into COOP_<AGENT>_CMD > the
  agent CLI's own default. coop never validates a model id — a bad one fails in
  the agent's own error.`,

	"pool": `coop loop pool (alias: coop pool) — which profiles this repo's loops rotate.

  Usage: coop loop pool                          show this repo's pool
         coop loop pool add <agent> <profile…>   add profiles to the rotation
         coop loop pool rm  <agent> <profile…>   remove profiles
         coop loop pool clear <agent>            drop the agent's pool

  When an unattended loop hits a rate/usage limit it switches to another profile in
  the pool and keeps going, so a long run survives a subscription cap. With no pool
  set the loop rotates across ALL signed-in profiles for the agent; a pool narrows
  that to a chosen set. Pools are per-repo and stored outside the repo (names only).
  It's a setting of the loop — hence the home under 'coop loop'; the old top-level
  'coop pool' still works as an alias.`,

	"acp": `coop acp [agent|fusion] — serve as an ACP agent over stdio (for editors).

  Usage: coop acp [claude|codex|gemini | fusion [agent]] [--profile <name>] [--model <m>] [--supervise] [--consult]

  Speaks the Agent Client Protocol on stdin/stdout. Point your editor's ACP
  command at e.g. ["acp","claude"] — one entry per agent or governor.

  --profile <name> pins the session to one credential profile, so an editor can run
  two entries on different accounts, e.g. ["acp","claude","--profile","work"].

  --model <m> pins the session's model (see 'coop models'), e.g.
  ["acp","claude","--model","opus"].

  --consult lets the session ask the other signed-in agents for a read-only second
  opinion (their credentials are mounted) — the orchestrator pattern, from your editor.

  --supervise keeps the editor connected across a box restart: it runs the agent
  in a child and, if the container dies, starts a new one and replays the ACP
  handshake (initialize + session/load) so the editor doesn't see a crash. Set it
  in your editor's args, e.g. ["acp","claude","--supervise"].`,

	"fusion": `coop fusion [agent] — one agent leads, the other two advise, it synthesizes.

  Usage: coop fusion [claude|codex|gemini] [--profile <name>] [--model <m>] [args...]

  Defaults to COOP_FUSION_GOVERNOR. Peers advise read-only; only the leader
  writes. Lighter, opt-in variant: coop <agent> --consult

  --profile <name> pins the governor's credential profile; each peer keeps its own.

  --model picks the governor's model; each peer keeps its own default (its
  profile's mark or COOP_<AGENT>_MODEL — see 'coop models').

  Like coop <agent>, it forwards extra args to the governor — a leading agent name
  picks the governor; anything else (or anything after a --) passes through.`,

	"fleet": `coop fleet — run a declarative fleet of forks from .agent/fleet.

  Usage: coop fleet <init|up|down|split|watch|prune>

  init           write a .agent/fleet template
  up             start every fork in the fleet, looping its tasks, detached
  down           stop the fleet's running loops
  split <n>      slice the queue into n forks (= coop tasks split) + write .agent/fleet
  watch          live dashboard of every fork's progress (auto-exits when the fleet's
                 done; Ctrl-C anytime). Task-centric view: coop tasks watch
  prune          remove forks no longer in .agent/fleet (kept: running, dirty, or
                 unmerged — pass --force to remove those too)

  up and down take --prune (with optional --force) to prune in the same step.

  Each fleet line is "<name> [agent] <tasks-path> [profile=a,b] [model=m] [consult=1]".
  Add profile= to put a fork's loop on specific account(s) — give each fork a different
  one so they run in parallel instead of contending for the same rate limit. Add
  model= to pick that fork's model (see 'coop models'), and consult=1 to let its
  iterations ask the other signed-in agents read-only (like coop loop --consult).

  List forks: coop fork ls`,

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
  rm <id>          delete a task folder; --all-done clears the whole done archive
  decisions [-i]   list open decisions; -i walks them one by one to answer (records + unblocks)
  lint             check the tree (blocked<->decision.md, no status field, …; exits 1)
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

  Usage: coop loop [claude|codex|gemini] [--tasks <path>]... [--model <m>] [--consult] [--preflight] [--debug-on-fail]

  A fresh agent per iteration works the todo tasks; when the queue empties, an
  auditor re-checks every shipped task. On a rate limit it switches to another
  signed-in profile (see 'coop loop pool'), or waits out the reset when there's only one.

  --model <m> pins the loop's model; COOP_LOOP_MODEL is its standing default —
  so overnight runs can grind on a cheaper model than your interactive sessions.
  With neither, each iteration uses the rotated profile's marked default
  ('coop models'), then COOP_<AGENT>_MODEL.

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
  the box reaches db/redis/… by hostname. Stop them with: coop down`,

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

  Usage: coop update [--self-only | --box-only]

  First replaces the coop binary with the latest GitHub release — fetched and
  verified the same way install.sh does (checksum + cosign), then swapped in
  atomically so replacing the running binary is safe. Then rebuilds the image like
  coop build but --pull --no-cache and unpinned, so the node base and agent CLIs
  jump to latest. Use coop build for a reproducible image. Supervised editor
  sessions are restarted onto the new image transparently, same as build.

    --self-only   update just the coop binary, skip the image rebuild
    --box-only    rebuild just the image, skip the self-update (the old behavior)

  A dev/source build, an already-current binary, or a coop installed somewhere
  unwritable (a package-manager prefix) skips the self-update with a note.`,

	"version": `coop version — print coop's version and exit.

  Usage: coop version

  Prints coop's build version (git tag or commit). Takes no arguments; -v and
  --version are aliases.`,
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
