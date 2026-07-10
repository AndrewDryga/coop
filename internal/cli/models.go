package cli

import (
	"fmt"
	"os"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdModels is the model MENU: one line per agent with its models — the LIVE list when a
// fresh one is cached (dim "(live)"), else the curated static Models() (dim "(examples)") —
// then a short how-to. A model is picked per run (--model) or as a preset's models: ladder;
// coop never validates --model against any list, so any id the agent's CLI accepts works.
//
// The plain command stays instant and Docker-free: it only reads the per-agent cache, never
// spawning a box. `coop models --refresh [agent]` updates the cache from each agent's
// auth-free native CLI (grok/codex); claude/gemini refresh for free on every `coop acp`.
func (a *app) cmdModels(args []string) (int, error) {
	refresh := false
	var rest []string
	for _, arg := range args {
		if arg == "--refresh" {
			refresh = true
			continue
		}
		rest = append(rest, arg)
	}
	names := agents.Names()
	if len(rest) > 0 {
		if _, ok := agents.Get(rest[0]); !ok {
			return 2, unknownErr("agent", rest[0], agents.Names())
		}
		names = []string{rest[0]}
		if len(rest) > 1 {
			return 2, fmt.Errorf("unexpected argument %q (usage: coop models [agent] [--refresh]; pick a model with --model or a preset)", rest[1])
		}
	}
	p := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	if refresh {
		a.refreshModels(names, p)
		fmt.Println()
	}
	w := colWidth(names, 0, 12)
	anyLive := false
	for _, agent := range names {
		ag, _ := agents.Get(agent)
		ids, live := a.agentModels(agent, ag)
		tag := " (examples)"
		if live {
			tag, anyLive = " (live)", true
		}
		line := "  " + p.Bold(padRight(agent, w)) + "  " + strings.Join(ids, p.Dim(" · ")) + p.Dim(tag)
		// The agent-wide env default is config, not repo state — surface it only when set.
		if def := a.cfg.AgentModelDefault(agent); def != "" {
			line += p.Dim("  — default: "+def) + p.Dim(" (COOP_"+strings.ToUpper(agent)+"_MODEL)")
		}
		fmt.Println(line)
	}
	// A short how-to instead of a wall: the queried agent (or the first) seeds the examples.
	ex, _ := agents.Get(names[0])
	model := ex.Models()[0] // the static list is always non-empty — a stable example id
	pad := func(s string) string { return p.Dim(padRight(s, 13)) }
	fmt.Println()
	caption := "  (examples) — any model id the agent's CLI accepts works; (live) is fetched auth-free"
	if !anyLive {
		caption += " (coop models --refresh)"
	}
	fmt.Println(p.Dim(caption))
	fmt.Printf("  %s coop %s --model %s%s\n", pad("one run"), names[0], model, p.Dim("   (fusion, fork, loop, acp take it too)"))
	fmt.Printf("  %s a preset's models: ladder%s\n", pad("standing"), p.Dim("   (coop help presets)"))
	fmt.Printf("  %s COOP_%s_MODEL=%s%s\n", pad("everywhere"), strings.ToUpper(names[0]), model, p.Dim("   (loop runs only: COOP_LOOP_MODEL)"))
	if lm := a.cfg.LoopModel; lm != "" {
		fmt.Println(p.Dim("  loop model pinned: " + lm + " (COOP_LOOP_MODEL)"))
	}
	return 0, nil
}

// agentModels returns the ids to show for an agent and whether they're LIVE: a fresh cached
// list (live) or the curated static Models() (examples). Read-only and Docker-free — the
// cache is populated elsewhere (coop acp; coop models --refresh).
func (a *app) agentModels(agent string, ag agents.Agent) ([]string, bool) {
	if models, ok := loadModelsCache(a.cfg, agent); ok {
		ids := make([]string, 0, len(models))
		for _, m := range models {
			ids = append(ids, m.ID)
		}
		return ids, true
	}
	return ag.Models(), false
}

// refreshModels fetches live models for each named agent via its auth-free native CLI and
// writes the cache. Best-effort and Docker-free: an agent whose CLI isn't on PATH, times
// out, or fails to parse is skipped (the display then falls back to its last cache or static
// list). Only grok/codex have a cheap native list; claude/gemini refresh for free on every
// `coop acp` session, so this notes that instead of spawning a box.
func (a *app) refreshModels(names []string, p ui.Palette) {
	for _, agent := range names {
		fetch := nativeModelFetchers[agent]
		if fetch == nil {
			fmt.Println(p.Dim("  " + padRight(agent, 8) + " refreshes on `coop acp` (no native model list)"))
			continue
		}
		models, err := fetch()
		if err != nil || len(models) == 0 {
			fmt.Println(p.Dim("  " + padRight(agent, 8) + " refresh skipped (CLI unavailable or no models)"))
			continue
		}
		if err := writeModelsCache(a.cfg, agent, models); err != nil {
			fmt.Println(p.Dim("  " + padRight(agent, 8) + " refresh skipped (cache write failed)"))
			continue
		}
		fmt.Println(p.Dim(fmt.Sprintf("  %s refreshed %d models", padRight(agent, 8), len(models))))
	}
}
