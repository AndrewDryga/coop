package cli

import (
	"fmt"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdModels is the read-only models view: each agent's known models and which model each
// credential profile is marked to run. It edits nothing — the mark is a PROFILE attribute,
// so `coop profiles <agent> <profile> model <model>` sets it (and `coop profiles` shows it
// too). The known list is a menu, not a gate: model ids churn faster than coop releases, so
// any id the agent CLI accepts works with --model and coop never validates against it.
func (a *app) cmdModels(args []string) (int, error) {
	names := agents.Names()
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); !ok {
			return 2, unknownErr("agent", args[0], agents.Names())
		}
		names = []string{args[0]}
		if len(args) > 1 {
			return 2, fmt.Errorf("unexpected argument %q (usage: coop models [agent]; mark a profile's model with 'coop profiles <agent> <profile> model <m>')", args[1])
		}
	}
	for _, agent := range names {
		ag, _ := agents.Get(agent)
		fmt.Println(ui.Bold(agent))
		fmt.Printf("  known models: %s  %s\n", strings.Join(ag.Models(), ", "), ui.Dim("(examples — any id the CLI accepts works)"))
		if def := a.cfg.AgentModelDefault(agent); def != "" {
			fmt.Printf("  agent-wide default: %s  %s\n", def, ui.Dim("(COOP_"+strings.ToUpper(agent)+"_MODEL)"))
		}
		profiles := a.cfg.Profiles(agent)
		if len(profiles) == 0 {
			fmt.Printf("  no profiles — run: coop login %s [--profile <name>]\n", agent)
			continue
		}
		width := colWidth(profiles, 0, 40)
		markedDefault := a.cfg.DefaultProfileOf(agent)
		for _, p := range profiles {
			model := a.cfg.ProfileModelOf(agent, p)
			cell := ui.Dim("—  (mark: coop profiles " + agent + " " + p + " model <model>)")
			if model != "" {
				cell = model
			}
			tag := ""
			if p == markedDefault {
				tag = ui.Dim("  (default profile)")
			}
			// Pad the plain name (rune-aware), then style the model cell — never color inside the width.
			fmt.Printf("  %s  %s%s\n", padRight(p, width), cell, tag)
		}
	}
	// The loop's own model override outranks every profile mark — say so when it's set, or a
	// marked default that never seems to apply to `coop loop` reads as a bug.
	if lm := a.cfg.LoopModel; lm != "" {
		ui.Note("loop runs override these with %s (COOP_LOOP_MODEL); --model on any run overrides everything", lm)
	} else {
		ui.Note("pick per run with --model on any launch (coop claude --model opus); precedence: --model > COOP_LOOP_MODEL (loop) > profile mark > COOP_<AGENT>_MODEL")
	}
	return 0, nil
}
