package loop

import (
	"time"

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

// rotateOnLimit handles a rate limit with more than one target: advance, point cfg at the
// new active target, and either switch immediately (a free rotation is progress) or, when
// every target is limited, sleep until the soonest reset. Returns the new active provider.
func (c *Control) rotateOnLimit(r *ladder.Rotation, resetAt time.Time, waits *int, wake <-chan struct{}) string {
	prev := r.Active()
	sleep, until := r.OnLimit(resetAt, *waits, time.Now())
	agent := c.applyTarget(r)
	if sleep > 0 {
		ui.Info("all %d targets are rate limited — waiting for the soonest reset", r.Len())
		sleepForLimit(sleep, until, wake)
		r.ClearExpired(time.Now())
		return agent
	}
	ui.Info("target %q rate limited — switching to %q", prev, r.Active())
	*waits = 0 // only consecutive all-limited waits count toward the stop cap
	return agent
}
