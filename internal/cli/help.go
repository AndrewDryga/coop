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

	fmt.Fprintf(&b, "%s %s — run a coding agent in a box it can't escape.\n", ui.Bold("coop"), resolveVersion())
	fmt.Fprint(&b, "usage: coop <command> [args]   ·   'coop <command> --help' for a command's flags and details\n")

	group("agents")
	row("coop claude|codex|gemini [args]", "a sandboxed agent (its autonomous flags + your args)")
	row("coop fusion [agent]", "a council: one agent leads, the others advise, it synthesizes")
	row("coop run -- <cmd...>", "run a raw command in the box")
	row("coop shell", "an interactive shell in the box")
	row("coop login <agent>", "authenticate an agent (token persists in the config dir)")
	row("coop acp [agent|fusion]", "serve as an ACP agent over stdio (for editors like Zed)")

	group("forks — review and land work like a PR")
	row("coop fork <name> [agent]", "open or re-enter a fork and run an agent")
	row("coop fork ls", "list this repo's forks")
	row("coop fork review <name>", "show a fork's brief + diff")
	row("coop fork merge <name>", "rebase the fork onto your branch and land it")
	row("coop fork logs [name]", "tail a fork's loop log (no name: every fork)")
	row("coop fork rm <name>", "discard a fork")
	row("coop fork stop <name>", "stop a detached loop")
	row("coop fork open|path <name>", "open the fork in your editor · print its path")

	group("unattended")
	row("coop loop [agent]", "work .agent/TASKS.md until done, then audit")
	row("coop fleet up|down|split", "drive a fleet of forks from .agent/fleet")
	row("coop status", "fleet roll-up: per-fork progress, running/idle, blockers")
	row("coop tasks list|lint|add|split", "inspect and validate .agent/TASKS.md")

	group("setup & maintenance")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, and skills")
	row("coop build|update", "build the box image · rebuild it fresh")
	row("coop up|down", "start/stop sibling services (db, redis)")
	row("coop doctor", "prove isolation — attack the box, check it holds")
	row("coop check-secrets", "scan the working tree for committed secrets")
	row("coop help|version", "this help · the version")

	fmt.Fprintf(&b, "\nconfig: COOP_* or %s   ·   auth: %s   ·   docs: the README\n",
		tildeify(filepath.Join(cfg.BoxHome, "coop.conf")), tildeify(cfg.ConfigDir))

	return b.String()
}

// commandHelp is the focused text for `coop <cmd> --help`, per subcommand. fork has its
// own richer forkHelp; `run` and the agents deliberately aren't here so `--help` forwards
// to the box command / the agent's own CLI. Each value's first line is the synopsis.
var commandHelp = map[string]string{
	"shell": `coop shell — open an interactive shell in the box.

  usage: coop shell

  Drops you into the configured shell (default bash) in the sandbox at your repo, with the
  same mounts, secret-shadowing, and network as an agent run. Exit to return.`,

	"login": `coop login <agent> — authenticate an agent; its token persists in coop's config dir.

  usage: coop login <claude|codex|gemini>

  Runs the agent's sign-in (paste a code, no browser) so later runs are authenticated.
  Re-run any time to refresh or switch accounts — e.g. after a subscription usage limit.`,

	"acp": `coop acp [agent|fusion] — front coop as an ACP agent over stdio (drive it from an editor).

  usage: coop acp [claude|codex|gemini | fusion [agent]]

  Speaks the Agent Client Protocol on stdin/stdout. Point your editor's ACP command at
  e.g. ["acp","claude"] (one entry per agent or governor). See the README's Zed section.`,

	"fusion": `coop fusion [agent] — a council: the named agent leads, the other two advise read-only, then it synthesizes.

  usage: coop fusion [claude|codex|gemini]

  Defaults to the COOP_FUSION_GOVERNOR governor. Peers are consulted read-only; only the
  leader writes. For a lighter, opt-in variant, run: coop <agent> --consult`,

	"fleet": `coop fleet up|down|split — drive a declarative fleet of forks from .agent/fleet.

  usage: coop fleet up | down | split <n>

  up         start every fork in .agent/fleet, each looping its tasks file, detached
  down       stop the fleet's running loops
  split <n>  round-robin .agent/TASKS.md into n self-contained slices, then write .agent/fleet

  List forks with: coop fork ls   ·   watch them with: coop status`,

	"status": `coop status — a fleet roll-up: where every fork stands, without tailing N logs.

  usage: coop status

  Per fork: running/idle, tasks done/total, blockers, diff size, and the task it's on, plus
  fleet totals. Reads the fork queues, git, and loop state — no daemon.`,

	"tasks": `coop tasks list|lint|add|split — treat .agent/TASKS.md as a validated surface.

  usage: coop tasks list | lint | add "<title>" | split <n>

  list       states + titles, with a count summary
  lint       flag stale [w] claims, non-self-contained tasks, and malformed entries (exit 1 on findings)
  add        append a well-shaped [ ] task stub to fill in
  split <n>  carve the open tasks into n slices (whole task blocks, bodies included)`,

	"check-secrets": `coop check-secrets — scan the files the box can see for committed secrets, by content.

  usage: coop check-secrets

  Walks the non-shadowed working tree (what an agent actually sees), flags provider token
  shapes and high-entropy values as file:line, and exits non-zero on a hit (good for CI or
  pre-flight). Already-shadowed files are skipped; hide a flagged file with .coopignore.`,

	"loop": `coop loop [agent] [--debug-on-fail] — work .agent/TASKS.md unattended until done, then audit.

  usage: coop loop [claude|codex|gemini] [--debug-on-fail]

  A fresh agent per iteration works the unchecked [ ] items; when the queue empties an
  auditor re-checks every [x]. Rides out rate limits by waiting for the reset.

  --debug-on-fail   on an iteration failure at a terminal, open a box shell to inspect,
                    then retry when you exit it (a no-op in unattended runs)

  COOP_LOOP_CMD overrides the whole per-iteration command.`,

	"up": `coop up — start the repo's sibling services so the box can reach them by name.

  usage: coop up

  Brings up the services in compose.agent.yml on coop's network; an agent in the box then
  reaches db/redis/… by hostname. Stop them with: coop down`,

	"down": `coop down [-v] — stop the repo's sibling services.

  usage: coop down [-v | --volumes]

  -v, --volumes   also remove the services' volumes (their data), not just the containers`,

	"init": `coop init [--stack asdf] — scaffold coop's working set into the repo.

  usage: coop init [--stack asdf]

  Writes AGENTS.md, the .agent/ queue, the Claude + git pre-commit hooks, and the workflow
  skills. With a .tool-versions present (or --stack asdf) it also scaffolds an asdf
  Dockerfile.agent + compose.agent.yml. Existing files are never clobbered.`,

	"doctor": `coop doctor — prove the box's isolation: attack it from inside and from the host.

  usage: coop doctor

  Runs the escape/leak checks — secret shadowing, network limits, host reach, the fork
  handoff — and prints a pass/fail report. No flags. Honors COOP_RUNTIME.`,

	"build": `coop build — build the box image (the stable, pinned path).

  usage: coop build

  Builds the shared base, or a per-project image if the repo has a Dockerfile.agent,
  pinning the base + agent versions for a reproducible image. Re-run after changing
  Dockerfile.agent or .tool-versions. For the latest base/agents instead, use: coop update`,

	"update": `coop update — rebuild the box image fresh, floating to the latest base + agents.

  usage: coop update

  Like coop build but --pull --no-cache and unpinned, so you get the newest node base and
  agent CLIs. Use it when you want updates; use coop build for a reproducible image.`,
}

// printCommandHelp prints one subcommand's focused help: synopsis line bolded, body as-is,
// then a pointer to the full command list.
func printCommandHelp(text string) {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		fmt.Println(ui.Bold(text[:i]) + text[i:])
	} else {
		fmt.Println(ui.Bold(text))
	}
	fmt.Println("\n  see all commands: coop help")
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
