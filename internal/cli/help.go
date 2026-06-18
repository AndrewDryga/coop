package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	row := func(cmd, desc string) {
		gap := 34 - len(cmd)
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
	row("coop pool add <agent> <profile…>", "pick which subscriptions the loop rotates on a rate limit")
	row("coop fleet init|up|down|split", "drive a fleet of forks from .agent/fleet")
	row("coop status", "fleet roll-up: per-fork progress, running/idle, blockers")
	row("coop tasks list|lint|add|split", "inspect/validate the queue(s) (--tasks, COOP_TASKS)")

	group("SETUP & MAINTENANCE")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, and skills")
	row("coop build", "build the box image (stable, pinned)")
	row("coop update", "rebuild the box image fresh (latest base + agents)")
	row("coop up", "start sibling services (db, redis)")
	row("coop down", "stop sibling services")
	row("coop doctor", "prove isolation — attack the box, check it holds")
	row("coop check-secrets", "scan the working tree for committed secrets")
	row("coop help", "this help")
	row("coop version", "print the version")

	fmt.Fprint(&b, "\nRun 'coop <command> --help' for any command's flags and details.\n")
	fmt.Fprintf(&b, "\nConfig  %s, or COOP_* env vars\nAuth    %s\nDocs    https://github.com/AndrewDryga/coop\n",
		tildeify(filepath.Join(cfg.BoxHome, "coop.conf")), tildeify(cfg.ConfigDir))

	return b.String()
}

// commandHelp is the focused text for `coop <cmd> --help`, per subcommand. fork has its
// own richer forkHelp; `run` and the agents deliberately aren't here so `--help` forwards
// to the box command / the agent's own CLI. Each value's first line is the synopsis.
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

  Shows each agent's profiles and whether they're signed in. A profile is one
  subscription; add more with 'coop login <agent> --profile <name>', then let the
  loop rotate across them on a rate limit ('coop pool').

  'coop profiles default <agent> <profile>' marks which profile an interactive run
  (plain 'coop claude') uses — so the default is a mark you set, not whichever one
  happens to be named "default".`,

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

  Usage: coop acp [claude|codex|gemini | fusion [agent]]

  Speaks the Agent Client Protocol on stdin/stdout. Point your editor's ACP
  command at e.g. ["acp","claude"] — one entry per agent or governor.`,

	"fusion": `coop fusion [agent] — one agent leads, the other two advise, it synthesizes.

  Usage: coop fusion [claude|codex|gemini]

  Defaults to COOP_FUSION_GOVERNOR. Peers advise read-only; only the leader
  writes. Lighter, opt-in variant: coop <agent> --consult`,

	"fleet": `coop fleet — run a declarative fleet of forks from .agent/fleet.

  Usage: coop fleet <init|up|down|split>

  init       write a .agent/fleet template
  up         start every fork in the fleet, looping its tasks, detached
  down       stop the fleet's running loops
  split <n>  split .agent/TASKS.md into n fork slices, then write .agent/fleet

  List forks: coop fork ls    Watch: coop status`,

	"status": `coop status — fleet roll-up: where every fork stands, without tailing N logs.

  Usage: coop status

  Per fork: running/idle, tasks done/total, blockers, diff size, and the task
  it's on — plus fleet totals. Reads queues, git, and loop state; no daemon.`,

	"tasks": `coop tasks — inspect and validate the task queue(s).

  Usage: coop tasks [--tasks <path>]... <list|lint|add|split>

  list        list tasks by state, with a count
  lint        flag stale claims, unshaped or malformed tasks (exits 1 if any)
  add "..."   append a new task stub
  split <n>   split the open tasks into n files

  Defaults to .agent/TASKS.md. Repeat --tasks to span several queues (a monorepo's
  per-component files, e.g. --tasks portal/.agent/TASKS.md --tasks runner/.agent/
  TASKS.md) — list and lint cover them all; add and split target a single one. Or set
  COOP_TASKS to a space-separated list. Paths are relative to the repo root.`,

	"check-secrets": `coop check-secrets — scan the working tree for committed secrets, by content.

  Usage: coop check-secrets

  Scans every file the box can see (not shadowed) for token shapes and
  high-entropy values, reporting file:line. Exits non-zero on a hit, for use
  as a pre-flight or CI check. Hide a flagged file with .coopignore.`,

	"loop": `coop loop [agent] — work the task queue until done, then audit.

  Usage: coop loop [claude|codex|gemini] [--tasks <path>]... [--debug-on-fail]

  A fresh agent per iteration works the [ ] items; when the queue empties, an
  auditor re-checks every [x]. On a rate limit it switches to another signed-in
  profile (see 'coop pool'), or waits out the reset when there's only one.

  Defaults to .agent/TASKS.md. Repeat --tasks (or set COOP_TASKS) to drain several
  queues at once — the loop keeps going while any of them has a [ ], so one loop can
  cover a monorepo's components. The whole repo is still mounted.

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

  Usage: coop init [--stack asdf]

  Writes AGENTS.md, the .agent/ queue, the Claude + git commit hooks, and the
  workflow skills. A .tool-versions (or --stack asdf) also scaffolds an asdf
  Dockerfile.agent + compose.agent.yml. Never clobbers existing files.`,

	"doctor": `coop doctor — prove the box's isolation: attack it, inside and from the host.

  Usage: coop doctor

  Runs the escape/leak checks — secret shadowing, network limits, host reach,
  the fork handoff — and prints a pass/fail report. Honors COOP_RUNTIME.`,

	"build": `coop build — build the box image (stable, pinned).

  Usage: coop build

  Builds the shared base, or a per-project image if the repo has a
  Dockerfile.agent — pinning versions for reproducibility. Re-run after
  changing Dockerfile.agent or .tool-versions. For the latest, use coop update.`,

	"update": `coop update — rebuild the box image fresh, to the latest base + agents.

  Usage: coop update

  Like coop build but --pull --no-cache and unpinned, so you get the newest
  node base and agent CLIs. Use coop build for a reproducible image.`,
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
