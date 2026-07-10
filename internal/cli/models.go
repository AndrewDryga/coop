package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdModels is the model MENU: a block per agent — a bold-cyan header, its models (the
// fresh cached list, else the curated static example Models()), and an explicit "Last
// refreshed" fact (green when fresh, yellow when stale, with the refresh channel as the
// hint) — then one caption and a short how-to. A model is picked per run (--model) or as
// a preset's models: ladder; coop never validates --model against any list, so any id the
// agent's CLI accepts works.
//
// The plain command stays instant and Docker-free: it only reads the per-agent cache, never
// spawning a box. `coop models --refresh [agent]` updates the cache from each agent's
// auth-free native CLI (grok/codex) and folds each outcome into that block's "Last
// refreshed" line; claude/gemini refresh for free on every `coop acp`.
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
			return 2, fmt.Errorf("unexpected argument %q (usage: coop models [<agent>] [--refresh]; pick a model with --model or a preset)", rest[1])
		}
	}
	p := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	var notes map[string]string
	if refresh {
		notes = a.refreshModels(names)
	}
	for _, agent := range names {
		ag, _ := agents.Get(agent)
		ids, fetchedAt, live := a.agentModels(agent, ag)
		fmt.Println(p.Bold(p.Cyan(titleName(agent))))
		fmt.Println("  " + p.Dim("Models:") + " " + strings.Join(ids, p.Dim(" · ")))
		fmt.Println("  " + p.Dim("Last refreshed:") + " " + refreshedLine(agent, fetchedAt, live, notes[agent], p))
		// The agent-wide env default is config, not repo state — surface it only when set.
		if def := a.cfg.AgentModelDefault(agent); def != "" {
			fmt.Println("  " + p.Dim("Default:") + " " + def + " " + p.Dim("(COOP_"+strings.ToUpper(agent)+"_MODEL)"))
		}
		fmt.Println()
	}
	fmt.Println(p.Dim("  any model id the agent's CLI accepts works — an unrefreshed list shows examples"))
	// A short how-to instead of a wall: the queried agent (or the first) seeds the examples.
	ex, _ := agents.Get(names[0])
	model := ex.Models()[0] // the static list is always non-empty — a stable example id
	rows := []struct{ label, cmd, note string }{
		{"one run", "coop " + names[0] + " --model " + model, "fusion, fork, loop, acp take it too"},
		{"standing", "a preset's models: ladder", "coop help presets"},
		{"everywhere", "COOP_" + strings.ToUpper(names[0]) + "_MODEL=" + model, "loop runs only: COOP_LOOP_MODEL"},
	}
	labels, cmds := make([]string, len(rows)), make([]string, len(rows))
	for i, r := range rows {
		labels[i], cmds[i] = r.label, r.cmd
	}
	lw, cw := colWidth(labels, 0, 12), colWidth(cmds, 0, 44)
	fmt.Println()
	for _, r := range rows {
		// Pad plain, then style — ANSI bytes inside a padded cell would break the columns.
		fmt.Printf("  %s  %s  %s\n", p.Dim(padRight(r.label, lw)), p.Cyan(padRight(r.cmd, cw)), p.Dim("("+r.note+")"))
	}
	if lm := a.cfg.LoopModel; lm != "" {
		fmt.Println(p.Dim("  loop model pinned: " + lm + " (COOP_LOOP_MODEL)"))
	}
	return 0, nil
}

// titleName renders an agent id as its block header — "claude" → "Claude" (ids are ASCII).
func titleName(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// agoStr says how long ago t was, in the coarsest unit that keeps meaning — the menu needs
// fresh-vs-stale, not seconds.
func agoStr(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// refreshedLine renders a block's "Last refreshed" value: the age of the live list (green),
// "never", or the stale age (yellow) — plus, in dim parens, how THIS agent's list refreshes
// whenever that isn't self-evident: claude/gemini always name `coop acp`, codex/grok name
// --refresh only while nothing is live. note is --refresh's failure for this agent, if any;
// it replaces the hint (the user just ran --refresh — say what went wrong instead).
func refreshedLine(agent string, fetchedAt time.Time, live bool, note string, p ui.Palette) string {
	native := nativeModelFetchers[agent] != nil
	hint := "refreshes on `coop acp`"
	var s string
	switch {
	case live:
		s = p.Green(agoStr(fetchedAt))
		if native {
			hint = ""
		}
	case fetchedAt.IsZero():
		s = "never"
	default:
		s = p.Yellow(agoStr(fetchedAt) + " — stale")
	}
	if !live && native {
		hint = "coop models --refresh"
	}
	if note != "" {
		return s + " " + p.Yellow("(refresh failed — "+note+")")
	}
	if hint != "" {
		return s + " " + p.Dim("("+hint+")")
	}
	return s
}

// agentModels returns the ids to show for an agent — the fresh cached list, else the curated
// static example Models() — plus when a cache was last written (zero: never) and whether it
// is live. Read-only and Docker-free — the cache is populated elsewhere (coop acp; --refresh).
func (a *app) agentModels(agent string, ag agents.Agent) ([]string, time.Time, bool) {
	mc, live := loadModelsCache(a.cfg, agent)
	if !live {
		return ag.Models(), mc.FetchedAt, false
	}
	ids := make([]string, 0, len(mc.Models))
	for _, m := range mc.Models {
		ids = append(ids, m.ID)
	}
	return ids, mc.FetchedAt, true
}

// refreshModels fetches live models for each named agent via its auth-free native CLI and
// writes the cache, returning a short failure note per agent that couldn't — the menu folds
// it into that block's "Last refreshed" line. Agents with no native list (claude/gemini) are
// skipped silently: their blocks already say they refresh on `coop acp`. Best-effort and
// Docker-free: a note never becomes an error, and the display falls back to the last cache
// or the static list.
func (a *app) refreshModels(names []string) map[string]string {
	notes := make(map[string]string, len(names))
	for _, agent := range names {
		fetch := nativeModelFetchers[agent]
		if fetch == nil {
			continue
		}
		models, err := fetch()
		if err != nil || len(models) == 0 {
			notes[agent] = "CLI unavailable or no models"
			continue
		}
		if err := writeModelsCache(a.cfg, agent, models); err != nil {
			notes[agent] = "cache write failed"
		}
	}
	return notes
}
