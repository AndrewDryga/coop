package cli

// A preset is named in the WHO-RUNS positional slot (coop <preset>, loop <preset>, fusion
// <preset>, acp <preset>, fork <name> <preset>) or a loop.yaml/fleet.yaml agent: rung — any
// bare word there that isn't a target (see isTargetHead) is a preset name. loadRunPreset
// resolves .agent/presets/<name>/preset.yaml and applies it: the preset's lead agent is the
// run's agent, its lead/role models and credentials seed the run's selections (an explicit
// target still wins), and box.Run mounts the generated role contracts + wrappers from the
// RunSpec's Preset.

import (
	"fmt"
	"slices"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/preset"
)

// loadRunPreset resolves the repo and loads the named preset ("" loads nothing). Pure
// local reads — validation fails loud here, before any box or detached worker starts.
func (a *app) loadRunPreset(name string) (*preset.Preset, error) {
	if name == "" {
		return nil, nil
	}
	repo, err := box.ResolveRepo(a.cfg.RepoOverride)
	if err != nil {
		return nil, err
	}
	return preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
}

// presetNamed reports whether word names an EXISTING preset folder (.agent/presets/<word>/, or the
// global presets dir): ok=true with the loaded preset — or its load error, for a broken one. ok=false
// (not a valid preset name, not in a repo, or no such preset) leaves the caller to fall through. It's
// the top-level `coop <preset>` dispatch's existence check — distinct from loadRunPreset, which the
// launch surfaces use on a positional preset and which ERRORS on a miss. The command switch runs
// before this, so a command name is never shadowed by a same-named preset.
func (a *app) presetNamed(word string) (p *preset.Preset, ok bool, err error) {
	if !preset.ValidName(word) {
		return nil, false, nil
	}
	repo, rerr := box.ResolveRepo(a.cfg.RepoOverride)
	if rerr != nil {
		return nil, false, nil // not in a repo → no preset; fall to the unknown-command error
	}
	globalDir := a.cfg.GlobalPresetsDir()
	if !slices.Contains(preset.List(repo, globalDir), word) {
		return nil, false, nil
	}
	p, err = preset.Load(repo, globalDir, word)
	return p, true, err
}

// presetLeadAgent resolves the launched agent under a preset: an agent the command
// named explicitly wins; otherwise the preset's lead.agent is the default.
func presetLeadAgent(p *preset.Preset, agent string, explicit bool) string {
	if p != nil && !explicit {
		return p.LeadAgent
	}
	return agent
}

// fusionLadderGuard rejects a cross-provider lead ladder on a fusion run: fusion runs ONE
// governor for the whole council session, so a rung on another provider could never apply —
// erroring beats silently ignoring the preset's declared fallback. (The loop embraces the same
// ladder — rotation swaps the agent per rung.) Guarded only when the effective governor IS the
// preset's lead; otherwise the ladder is inert for this run anyway.
func fusionLadderGuard(p *preset.Preset, governor string) error {
	if p != nil && governor == p.LeadAgent && p.CrossProvider() {
		return fmt.Errorf("preset %s: lead.agent is a cross-provider ladder — fusion runs one governor for the whole council, so its fallback ladder must stay on %s; cross-provider fallback is a `coop loop` capability (make the ladder single-provider, or run the preset under the loop)", p.Name, p.LeadAgent)
	}
	return nil
}

// applyPreset seeds the run's model/credential selections from the preset lead, then
// remembers the preset on the app so every RunSpec this run builds can carry it. Only the
// LEAD's model/credentials are seeded into global provider state, and only when the effective
// lead IS the preset's lead agent — a loop work.agent ladder like [gemini, frontier] runs
// frontier's routing under a gemini lead, which must not inherit claude's model id. Callers
// apply explicit CLI flags AFTER this, so they override.
//
// A role's model/effort are deliberately NOT seeded into global provider state. A role rides
// its OWN target (provider:model/effort) in the coop-consult/coop-delegate wrapper env
// (Role.TargetList() → COOP_CONSULT/DELEGATE_<ROLE>_TARGETS), which every provider's arm passes
// straight through as `--model`. Per-provider global state can't represent a provider that plays
// several parts at once — frontier runs codex as the thinker (terra), the fast delegate (luna),
// AND a lead rung (sol) — and seeding a role's model into the active tier would shadow the lead's
// rotated target when that provider later becomes the lead (ModelFor prefers the active tier),
// launching the role's model under the lead's banner while telemetry reports the rotated one.
func (a *app) applyPreset(p *preset.Preset, lead string) {
	if p == nil {
		return
	}
	if lead == p.LeadAgent && len(p.LeadLadder) > 0 {
		// A run that doesn't rotate (single, or the loop before its first applyTarget) uses the
		// ladder's FIRST entry: its first account if pinned (else the marked default), and its
		// model in the target tier — below an explicit target model, above the account mark.
		first := p.LeadLadder[0]
		if acct := first.Account(); acct != "" {
			a.cfg.SetActiveProfile(lead, acct)
		}
		a.cfg.SetTargetModel(lead, first.Model)
		a.cfg.SetTargetEffort(lead, first.Effort)
	}
	a.preset = p
}
