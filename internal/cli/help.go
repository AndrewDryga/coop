package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ui"
)

func printHelp(cfg *config.Config) {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	row := func(cmd, desc string) { fmt.Fprintf(&b, "  %-27s%s\n", cmd, desc) }

	p("%s %s — run a coding agent in a box it can't escape.\n\n", ui.Bold("coop"), resolveVersion())

	p("%s\n", ui.Bold("usage"))
	row("coop claude|codex|gemini", "a sandboxed agent, with its autonomous flags")
	row("coop login <agent>", "authenticate an agent (persists in the config dir)")
	row("coop acp [agent]", "run as an ACP agent over stdio (point Zed at this)")
	row("coop fusion [--governor g]", "a council: g leads, the other two advise read-only")
	row("coop run -- <cmd...>", "run any command in the box")
	row("coop shell", "a shell in the box")
	row("coop fork <name> [agent]", "open/resume a fork (clone) + an agent; --loop [-d] to loop")
	row("coop fork <verb>", "ls · logs · review · merge · rm · stop — the fork lifecycle")
	row("coop up | down", "start/stop sibling services (db, redis) for this repo")
	row("coop loop", "work .agent/TASKS.md unattended until done")
	row("coop dispatch <name>", "fork + that agent's queue slice + loop (fleet unit)")
	row("coop fleet up|ls|down", ".agent/fleet → start/list/stop a fleet of forks")
	row("coop init [--stack asdf]", "scaffold the queue + hooks (+ toolchain & services)")
	row("coop doctor", "verify the box still contains the agent")
	row("coop build", "build the box image (per-project if Dockerfile.agent)")
	row("coop update", "rebuild fresh — pull latest agent CLIs + ACP adapters + base")
	row("coop help | version", "")
	p("\n")

	p("%s\n", ui.Bold("per-project environment"))
	p("  coop init                  # a repo with .tool-versions → an asdf box at those exact versions\n")
	p("  coop init --stack asdf     # force a baked asdf Dockerfile.agent + compose.agent.yml\n")
	p("  coop build && coop up      # build the image, start db/redis\n")
	p("  coop                       # the box has the toolchain + reaches db/redis by name\n\n")

	p("%s  split the queue into .agent/TASKS.<name>.md, then hand each slice to a model:\n", ui.Bold("a fleet"))
	p("  coop fork perf codex --loop -d     # own fork + branch + model, detached\n")
	p("  coop fork deps gemini --loop -d\n")
	p("  coop fork ls  ·  coop fork logs -f  ·  coop fork stop perf  ·  coop fork merge perf\n\n")

	p("%s  one model leads, the other two advise read-only, the leader synthesizes:\n", ui.Bold("fusion"))
	p("  coop fusion                     # codex leads (COOP_FUSION_GOVERNOR); peers advise\n")
	p("  coop fusion --governor claude   # claude leads instead\n")
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

	p("%s  env vars (COOP_IMAGE, COOP_REPO, COOP_RUNTIME, COOP_CLAUDE_CMD,\n", ui.Bold("config"))
	p("        COOP_CODEX_CMD, COOP_GEMINI_CMD, COOP_GATE, ...) or %s\n", tildeify(filepath.Join(cfg.BoxHome, "coop.conf")))

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
