package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/AndrewDryga/coop/internal/config"
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

// helpText renders the top-level command reference: one command per line, grouped, with a
// pointer to per-command help. Flags, sub-verbs, and examples live in `coop <cmd> --help`
// and the README, so this stays a clean, scannable overview.
func helpText(cfg *config.Config) string {
	var b strings.Builder
	group := func(label string) { fmt.Fprintf(&b, "\n%s\n", ui.Bold(label)) }
	// row keeps a column gap even when a command is long, so a description never glues to it.
	// Width is counted in runes, not bytes, so a command with a "…" doesn't shift its column.
	row := func(cmd, desc string) {
		gap := 34 - utf8.RuneCountInString(cmd)
		if gap < 2 {
			gap = 2
		}
		fmt.Fprintf(&b, "  %s%s%s\n", cmd, strings.Repeat(" ", gap), desc)
	}

	fmt.Fprintf(&b, "%s %s — run a coding agent all night long in a box it can't escape.\n", ui.Bold("coop"), resolveVersion())
	fmt.Fprint(&b, "Usage: coop <command> [args]\n")

	group("AGENTS")
	row("coop claude|codex|gemini [args]", "a sandboxed agent (its autonomous flags + your args)")
	row("coop fusion [agent]", "a council: one agent leads, the others advise, it synthesizes")
	row("coop run -- <cmd...>", "run a raw command in the box")
	row("coop shell", "an interactive shell in the box")
	row("coop login <agent> [--profile p]", "authenticate an agent (a profile = one subscription)")
	row("coop profiles [agent]", "list stored credential profiles and which are signed in")
	row("coop acp [agent|fusion]", "serve as an ACP agent over stdio (for editors like Zed)")
	row("coop <agent> --consult", "add a read-only second opinion from the other agents on a hard call")

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
	row("coop pool add|rm|clear", "pick which subscriptions the loop rotates on a rate limit")
	row("coop fleet init|up|down|split|watch|prune", "drive a fleet of forks from .agent/fleet")
	row("coop status [--watch]", "fleet roll-up: per-fork progress, running/idle, blockers")
	row("coop tasks add|claim|block|done|list|…", "drive the .agent/tasks/ queue (folder per task)")

	group("SETUP & MAINTENANCE")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, and skills")
	row("coop build", "build the box image (stable, pinned)")
	row("coop update", "self-update coop, then rebuild the box image fresh (latest base + agents)")
	row("coop up", "start sibling services (db, redis)")
	row("coop down", "stop sibling services")
	row("coop doctor", "prove isolation — attack the box, check it holds")
	row("coop check-secrets", "scan the working tree for committed secrets")
	row("coop help", "this help")
	row("coop version", "print the version")

	fmt.Fprint(&b, "\nRun 'coop help <command>' or 'coop <command> --help' for a command's details —\n"+
		"for an agent (claude/codex/gemini), --help is the agent's own.\n")
	fmt.Fprintf(&b, "\nConfig  %s, or COOP_* env vars\nAuth    %s\nDocs    https://github.com/AndrewDryga/coop\n",
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
  repo's profiles when one is rate limited (see 'coop pool'). Without --profile
  the sign-in targets the default profile.`,

	"profiles": `coop profiles [agent] — list stored credential profiles.

  Usage: coop profiles [claude|codex|gemini]
         coop profiles default <agent> <profile>
         coop profiles rm <agent> <profile>

  Shows each agent's profiles and whether they're signed in. A profile is one
  subscription; add more with 'coop login <agent> --profile <name>', then let the
  loop rotate across them on a rate limit ('coop pool').

  'coop profiles default <agent> <profile>' marks which profile an interactive run
  (plain 'coop claude') uses — so the default is a mark you set, not whichever one
  happens to be named "default".

  'coop profiles rm <agent> <profile>' deletes a stored profile (its login token and
  session history). Set a different default first if you're removing the marked one.

  Run on a specific profile without changing the default — works on any agent launch:
  'coop claude --profile <name>', 'coop fusion <agent> --profile <name>', and
  'coop acp <agent> --profile <name>' (so an editor entry can pin an account). An
  agent's own --profile goes after a --, e.g. 'coop codex -- --profile <name>'.`,

	"pool": `coop pool — which credential profiles this repo's loop rotates.

  Usage: coop pool                          show this repo's pool
         coop pool add <agent> <profile…>   add profiles to the rotation
         coop pool rm  <agent> <profile…>   remove profiles
         coop pool clear <agent>            drop the agent's pool

  When the unattended loop hits a rate/usage limit it switches to another profile in
  the pool and keeps going, so a long run survives a subscription cap. With no pool
  set the loop rotates across ALL signed-in profiles for the agent; a pool narrows
  that to a chosen set. Pools are per-repo and stored outside the repo (names only).`,

	"acp": `coop acp [agent|fusion] — serve as an ACP agent over stdio (for editors).

  Usage: coop acp [claude|codex|gemini | fusion [agent]] [--profile <name>] [--supervise]

  Speaks the Agent Client Protocol on stdin/stdout. Point your editor's ACP
  command at e.g. ["acp","claude"] — one entry per agent or governor.

  --profile <name> pins the session to one credential profile, so an editor can run
  two entries on different accounts, e.g. ["acp","claude","--profile","work"].

  --supervise keeps the editor connected across a box restart: it runs the agent
  in a child and, if the container dies, starts a new one and replays the ACP
  handshake (initialize + session/load) so the editor doesn't see a crash. Set it
  in your editor's args, e.g. ["acp","claude","--supervise"].`,

	"fusion": `coop fusion [agent] — one agent leads, the other two advise, it synthesizes.

  Usage: coop fusion [claude|codex|gemini] [args...]

  Defaults to COOP_FUSION_GOVERNOR. Peers advise read-only; only the leader
  writes. Lighter, opt-in variant: coop <agent> --consult

  Like coop <agent>, it forwards extra args to the governor — a leading agent name
  picks the governor; anything else (e.g. --model opus, or after a --) passes through.`,

	"fleet": `coop fleet — run a declarative fleet of forks from .agent/fleet.

  Usage: coop fleet <init|up|down|split|watch|prune>

  init           write a .agent/fleet template
  up             start every fork in the fleet, looping its tasks, detached
  down           stop the fleet's running loops
  split <n>      split the task queue into n fork slices, then write .agent/fleet
  watch          live dashboard: every fork's progress, refreshing (Ctrl-C to exit)
  prune          remove forks no longer in .agent/fleet (kept: running, dirty, or
                 unmerged — pass --force to remove those too)

  up and down take --prune (with optional --force) to prune in the same step.

  Each fleet line is "<name> [agent] <tasks-path> [profile=a,b]". Add profile= to
  put a fork's loop on specific account(s) — give each fork a different one so they
  run in parallel instead of contending for the same rate limit.

  List forks: coop fork ls`,

	"status": `coop status — fleet roll-up: where every fork stands, without tailing N logs.

  Usage: coop status [--watch]

  Per fork: running/idle, tasks done/total, blockers, diff size, and the task
  it's on — plus fleet totals. Reads queues, git, and loop state; no daemon.
  --watch (-w) refreshes it live as a dashboard (same as 'coop fleet watch').`,

	"tasks": `coop tasks — drive the task queue (a folder per task under .agent/tasks/).

  Usage: coop tasks [--tasks <path>]... <command>

  list             list tasks by state (todo/in_progress/blocked/done), with counts
  add "<title>"    scaffold a new task folder in todo/
  claim <id>       move a task todo/ -> in_progress/ (claim it before you start)
  block <id>       move it to blocked/ and write a decision.md stub
  unblock <id>     move it back blocked/ -> in_progress/
  done <id>        move it to done/ (the archive)
  drop <id>        remove a task folder (the record stays in git history)
  decisions        list the open decisions (one per blocked/ task)
  lint             check the tree (blocked<->decision.md, no status field, …; exits 1)
  split <n>        split the todo tasks into n per-fork slices (.agent/tasks.N)

  A task's state is its directory, so each transition is a folder move. Defaults to
  .agent/tasks/ (a legacy single .agent/TASKS.md is auto-detected as a fallback). Point
  --tasks at another tasks dir or file, or set COOP_TASKS. Paths are repo-relative.`,

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

  Usage: coop loop [claude|codex|gemini] [--tasks <path>]... [--preflight] [--debug-on-fail]

  A fresh agent per iteration works the todo tasks; when the queue empties, an
  auditor re-checks every shipped task. On a rate limit it switches to another
  signed-in profile (see 'coop pool'), or waits out the reset when there's only one.

  Defaults to .agent/tasks/ (a legacy .agent/TASKS.md is auto-detected). Repeat --tasks
  (or set COOP_TASKS) to drain several queues at once — the loop keeps going while any
  has unfinished work, so one loop can cover a monorepo's components. The whole repo is
  still mounted.

  --preflight       run one cleanup pass before working: unblock blocked/ tasks whose
                    decision now has an answer (opt-in; COOP_PREFLIGHT=1 to default it
                    on, --no-preflight to override). Makes no code changes or commits.
  --debug-on-fail   on a failure at a terminal, open a box shell, then retry
                    on exit (a no-op in unattended runs)

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

  Writes AGENTS.md, the .agent/ queue, the Claude + git commit hooks, and the
  workflow skills. The commit hooks' format gate matches the repo's stack —
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
}

// printCommandHelp prints one subcommand's focused help: synopsis line bolded, body as-is,
// then a pointer to the full command list.
func printCommandHelp(text string) {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		fmt.Println(ui.Bold(text[:i]) + text[i:])
	} else {
		fmt.Println(ui.Bold(text))
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
