package cli

import (
	"fmt"
	"os"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/ui"
)

// cmdModels is the model MENU: one line per agent with its known models, then a short
// how-to. A model is picked per run (--model) or as a preset's models: ladder — it's an
// axis of its own, never a credential property. The known list is examples, not a gate:
// model ids churn faster than coop releases, so any id the agent CLI accepts works with
// --model and coop never validates against it.
func (a *app) cmdModels(args []string) (int, error) {
	names := agents.Names()
	if len(args) > 0 {
		if _, ok := agents.Get(args[0]); !ok {
			return 2, unknownErr("agent", args[0], agents.Names())
		}
		names = []string{args[0]}
		if len(args) > 1 {
			return 2, fmt.Errorf("unexpected argument %q (usage: coop models [agent]; pick a model with --model or a preset)", args[1])
		}
	}
	p := ui.For(os.Stdout) // stdout view — gate color on stdout so a pipe stays clean
	w := colWidth(names, 0, 12)
	for _, agent := range names {
		ag, _ := agents.Get(agent)
		line := "  " + p.Bold(padRight(agent, w)) + "  " + strings.Join(ag.Models(), p.Dim(" · "))
		// The agent-wide env default is config, not repo state — surface it only when set.
		if def := a.cfg.AgentModelDefault(agent); def != "" {
			line += p.Dim("  — default: "+def) + p.Dim(" (COOP_"+strings.ToUpper(agent)+"_MODEL)")
		}
		fmt.Println(line)
	}
	// A short how-to instead of a wall: the queried agent (or the first) seeds the examples.
	ex, _ := agents.Get(names[0])
	model := ex.Models()[0]
	pad := func(s string) string { return p.Dim(padRight(s, 13)) }
	fmt.Println()
	fmt.Println(p.Dim("  these are examples — any model id the agent's CLI accepts works"))
	fmt.Printf("  %s coop %s --model %s%s\n", pad("one run"), names[0], model, p.Dim("   (fusion, fork, loop, acp take it too)"))
	fmt.Printf("  %s a preset's models: ladder%s\n", pad("standing"), p.Dim("   (coop help presets)"))
	fmt.Printf("  %s COOP_%s_MODEL=%s%s\n", pad("everywhere"), strings.ToUpper(names[0]), model, p.Dim("   (loop runs only: COOP_LOOP_MODEL)"))
	if lm := a.cfg.LoopModel; lm != "" {
		fmt.Println(p.Dim("  loop model pinned: " + lm + " (COOP_LOOP_MODEL)"))
	}
	return 0, nil
}
