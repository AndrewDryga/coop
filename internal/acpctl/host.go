package acpctl

import (
	"time"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/preset"
)

// Host is what the ACP control needs from the CLI that owns rotation policy and cannot own itself:
// rotation.go's account/ladder expansion, fusion_council.go's council resolution, modelscache.go's
// cache write, and ratelimit.go's suspend-safe wait are all POLICY that stays with their caller (the
// loop, the ACP control, and the sessions service each hold their own — see internal/ladder's package
// comment), so this package injects the exact cli functions rather than re-implement or import them.
//
// Every field is required — unlike sessionsvc's optional Host, an ACP control with a nil seam cannot
// do its job (there is no sensible "no rotation" default), so New wires a real Host from the caller
// and a zero Host is a programming error, not a valid empty state.
type Host struct {
	// ExpandLadder turns a target ladder into concrete rungs against the signed-in accounts
	// (internal/cli/rotation.go — shared with the loop's own rotation building).
	ExpandLadder func(cfg *config.Config, defaultAgent string, rungs []agents.Target) ([]agents.Target, error)
	// AccountsFor lists an agent's signed-in accounts in rotation order (internal/cli/rotation.go).
	AccountsFor func(cfg *config.Config, agent string) []string
	// ResolveFusionCouncil resolves a fusion council for the current spawn, filtered to the active
	// governor (internal/cli/fusion_council.go — shared with non-ACP `coop fusion`).
	ResolveFusionCouncil func(governor string, peers []agents.Target, p *preset.Preset, available []string, reachable []agents.Target) (FusionCouncil, error)
	// WriteModelsCache persists a freshly-observed model list to the per-agent cache `coop models`
	// reads (internal/cli/modelscache.go). Best-effort — the control ignores its error, same as today.
	WriteModelsCache func(cfg *config.Config, agent string, models []Model) error
	// WaitUntilWall blocks until deadline passes on the WALL clock (suspend-safe — see
	// internal/cli/ratelimit.go), or stop fires. Shared with the loop's sleepForLimit.
	WaitUntilWall func(deadline time.Time, tickCap time.Duration, nowFn func() time.Time, stop <-chan struct{}, onSegment func(time.Duration)) bool
}

// FusionCouncil mirrors cli's fusionCouncil (internal/cli/fusion_council.go) — a plain DTO, so this
// package keeps its own copy rather than depend back on cli for it.
type FusionCouncil struct {
	Peers            []agents.Target
	Members          []string
	UnavailableRoles []string
}
