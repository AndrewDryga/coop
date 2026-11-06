package loop

import (
	"fmt"
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/ladder"
	"github.com/AndrewDryga/coop/internal/ui"
)

// This is the loop's half of a rotation: what it DOES with the active target. Building the
// rotation in the first place — expanding a preset's ladder against the signed-in accounts — is
// credential policy shared with `coop acp` and `coop fork`, so it stays in internal/cli/rotation.go
// and arrives here as Host.BuildRotation or a ready RunSpec.Rotation.

// applyTarget points cfg at the rotation's active target: the agent+account the next iteration
// mounts, and its model (empty clears the tier, so a bare-model target falls through to the
// account's mark / env / agent default). The one choke point for loop start + each rotation.
// Returns the active target's provider — the loop runs THAT agent this iteration (a cross-
// provider ladder swaps it).
func (c *Control) applyTarget(r *ladder.Rotation) string {
	t := r.Active()
	c.cfg.SetActiveProfile(t.Provider, t.Account())
	c.cfg.SetTargetModel(t.Provider, t.Model)
	c.cfg.SetTargetEffort(t.Provider, t.Effort)
	return t.Provider
}

// RecordedTarget is the target a run of t actually uses, as a task lease records it for coop tasks
// watch: t, with a model or effort it leaves open filled in as cfg resolves it for that provider
// (an explicit one-off choice or a COOP_<AGENT>_MODEL default), so the record names what ran. A
// part nothing at coop's level chose (the agent CLI's own default model) stays empty.
func RecordedTarget(cfg *config.Config, t agents.Target) string {
	if t.Model == "" {
		t.Model = cfg.ModelFor(t.Provider)
	}
	if t.Effort == "" {
		t.Effort = cfg.EffortFor(t.Provider)
	}
	return t.String()
}

// rotateOnLimit handles a rate limit with more than one target: advance, point cfg at the
// new active target, and either switch immediately (a free rotation is progress) or, when
// every target is limited, sleep until the soonest reset. Returns the new active provider.
func (c *Control) rotateOnLimit(r *ladder.Rotation, resetAt time.Time, waits *int, wake <-chan struct{}) string {
	prev := r.Active()
	sleep, until := r.OnLimit(resetAt, *waits, time.Now())
	agent := c.applyTarget(r)
	if sleep > 0 {
		ui.Note("All configured agents have reached their usage limits.")
		sleepForLimit(sleep, until, wake)
		r.ClearExpired(time.Now())
		return agent
	}
	printLoopCaution(fmt.Sprintf("%s reached its usage limit, continuing with %s",
		cleanDiagnosticLine(agents.DisplayTarget(prev.String())),
		cleanDiagnosticLine(agents.DisplayTarget(r.Active().String()))))
	*waits = 0 // only consecutive all-limited waits count toward the stop cap
	return agent
}

func printLoopCaution(message string) {
	lines := ui.WrapLines(cleanDiagnosticLine(message), loopOutputWidth(nil)-4)
	ui.Caution("%s", lines[0])
	for _, line := range lines[1:] {
		ui.Note("    %s", line)
	}
}
