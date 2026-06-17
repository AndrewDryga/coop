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

func printHelp(cfg *config.Config) {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	row := func(cmd, desc string) { fmt.Fprintf(&b, "  %-33s%s\n", cmd, desc) }
	group := func(label string) { fmt.Fprintf(&b, "\n  %s\n", ui.Bold(label)) }

	p("%s %s — run a coding agent in a box it can't escape.\n\n", ui.Bold("coop"), resolveVersion())
	p("%s\n", ui.Bold("usage"))

	group("agents")
	row("coop claude|codex|gemini [args]", "a sandboxed agent — its autonomous flags, plus your args")
	row("coop fusion [agent]", "a council: that agent leads, the other two advise, it synthesizes")
	row("coop <agent> --consult", "opt-in: may ask authenticated peers on hard calls")
	row("coop run -- <cmd...>", "run any command in the box (raw)")
	row("coop shell", "a shell in the box")
	row("coop login <agent>", "authenticate an agent (token persists in the config dir)")
	row("coop acp [agent|fusion]", "run as an ACP agent over stdio (point Zed at this)")

	group("forks — review and land work like a PR")
	row("coop fork <name> [agent]", "open/re-enter a fork (re-entry resumes; --new resets)")
	row("coop fork <verb> <name>", "ls · logs · review · merge · rm · stop · open · path")

	group("run unattended")
	row("coop loop [agent] [--debug-on-fail]", "work .agent/TASKS.md until done, then audit (--debug-on-fail: box shell on a failure)")
	row("coop fork <name> <agent> --loop --tasks <p>", "loop one fork on a tasks file (-d detaches)")
	row("coop fleet up|down|split", "drive a fleet declared in .agent/fleet")
	row("coop status", "fleet roll-up — per-fork progress, running/idle, blockers")
	row("coop tasks list|lint|add|split", "inspect/validate .agent/TASKS.md (lint flags stale/unshaped tasks)")

	group("set up & maintain")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, skills (+ toolchain)")
	row("coop up | down [-v]", "start/stop sibling services (db, redis)")
	row("coop build | update", "build the box image | rebuild it fresh")
	row("coop doctor", "prove isolation — attack the box, check it holds")
	row("coop check-secrets", "scan the visible tree for committed secrets (content, not just names)")
	row("coop help | version", "this help · the version")
	p("\n")

	p("%s\n", ui.Bold("per-project environment"))
	p("  coop init                  # a repo with .tool-versions → an asdf box at those exact versions\n")
	p("  coop init --stack asdf     # force a baked asdf Dockerfile.agent + compose.agent.yml\n")
	p("  coop build && coop up      # build the image, start db/redis\n")
	p("  coop claude                # the box has the toolchain + reaches db/redis by name\n\n")

	p("%s  give each model its own fork, branch, and tasks file, looping detached:\n", ui.Bold("a fleet"))
	p("  coop fork perf codex  --loop -d --tasks .agent/TASKS.perf.md\n")
	p("  coop fork deps gemini --loop -d --tasks .agent/TASKS.deps.md\n")
	p("  coop fork ls  ·  coop fork logs -f  ·  coop fork stop perf  ·  coop fork merge perf\n\n")

	p("%s  one model leads, the other two advise read-only, the leader synthesizes:\n", ui.Bold("fusion"))
	p("  coop fusion                     # the default governor leads (COOP_FUSION_GOVERNOR); peers advise\n")
	p("  coop fusion claude              # claude leads instead\n")
	p("  coop <agent> --consult          # lighter & opt-in: may ask authed peers on hard calls\n\n")

	p("%s  add to Zed settings.json, then pick it in the agent panel:\n", ui.Bold("drive from Zed (ACP)"))
	p("  \"agent_servers\": { \"coop\": { \"type\": \"custom\",\n")
	p("      \"command\": \"coop\", \"args\": [\"acp\", \"claude\"], \"env\": {} } }\n")
	p("  one entry per governor for fusion, e.g. args [\"acp\", \"fusion\", \"codex\"]\n\n")

	p("%s  per-agent, in %s\n", ui.Bold("auth & settings"), tildeify(cfg.ConfigDir))
	p("  claude/ codex/ gemini/  ->  mounted at ~/.claude ~/.codex ~/.gemini in the box\n")
	p("  env                     ->  API keys (ANTHROPIC_API_KEY, OPENAI_API_KEY, ...)\n")
	p("  INSTRUCTIONS.md         ->  one instruction file, wired into all three agents\n")
	p("  mcp.json                ->  MCP servers, defined once for all three agents\n\n")

	p("%s  COOP_* env vars or %s — full list in the README\n", ui.Bold("config"), tildeify(filepath.Join(cfg.BoxHome, "coop.conf")))
	p("  common: COOP_RUNTIME · COOP_IMAGE · COOP_GATE · COOP_FUSION_GOVERNOR · COOP_EDITOR\n")

	fmt.Print(b.String())
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
