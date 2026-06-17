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
	row("coop loop [agent]", "work .agent/TASKS.md until done, then audit (default claude)")
	row("coop fork <name> <agent> --loop --tasks <p>", "loop one fork on a tasks file (-d detaches)")
	row("coop fleet up|down|split", "drive a fleet declared in .agent/fleet")

	group("set up & maintain")
	row("coop init [--stack asdf]", "scaffold the queue, hooks, skills (+ toolchain)")
	row("coop up | down [-v]", "start/stop sibling services (db, redis)")
	row("coop build | update", "build the box image | rebuild it fresh")
	row("coop doctor", "prove isolation — attack the box, check it holds")
	row("coop help | version", "this help · the version")
	p("\n")

	p("%s\n", ui.Bold("per-project environment"))
	p("  coop init                  # a repo with .tool-versions → an asdf box at those exact versions\n")
	p("  coop init --stack asdf     # force a baked asdf Dockerfile.agent + compose.agent.yml\n")
	p("  coop build && coop up      # build the image, start db/redis\n")
	p("  coop                       # the box has the toolchain + reaches db/redis by name\n\n")

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

// tildeify shortens a path under the home dir to ~/… for readable help.
func tildeify(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join("~", rel))
		}
	}
	return path
}
