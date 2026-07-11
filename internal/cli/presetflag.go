package cli

// --preset <name> on the launch surfaces (coop <agent>, loop, fusion, acp, fork --loop)
// loads .agent/presets/<name>/preset.yaml and applies it: the preset's lead agent is the
// default when the command names none, its lead/role models and credentials seed the
// run's selections (explicit CLI flags still win), and box.Run mounts the generated
// role contracts + wrappers from the RunSpec's Preset.

import (
	"fmt"

	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/preset"
)

// extractRunPreset pulls coop's own `--preset <name>` (or `--preset=<name>`) out of run
// args, `--`-aware like extractRunProfile, so anything after `--` passes through.
func extractRunPreset(args []string) (name string, rest []string, err error) {
	return extractRunValue(args, "--preset")
}

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

// applyPreset seeds the run's model/credential selections from the preset, around the
// resolved lead: consult/delegate roles pin their agent's model/credentials (a native
// role runs inside the lead's session, so it pins nothing), and the lead's own
// model/credentials apply only when the effective lead IS the preset's lead agent — a
// `coop loop gemini --preset frontier` keeps the routing but must not inherit claude's
// model id. Callers apply explicit CLI flags AFTER this, so they override. It also
// remembers the preset on the app, so every RunSpec this run builds can carry it.
func (a *app) applyPreset(p *preset.Preset, lead string) {
	if p == nil {
		return
	}
	for _, r := range p.Roles {
		if r.Mode == preset.ModeNative || r.Agent == lead {
			continue // the lead's own selection is handled below and must not be clobbered by a role
		}
		if r.Model != "" {
			a.cfg.SetActiveModel(r.Agent, r.Model) // the role runs on its agent's default account
		}
		if r.Effort != "" {
			a.cfg.SetActiveEffort(r.Agent, r.Effort)
		}
	}
	if lead == p.LeadAgent && len(p.LeadLadder) > 0 {
		// A run that doesn't rotate (single, or the loop before its first applyTarget) uses the
		// ladder's FIRST entry: its first account if pinned (else the marked default), and its
		// model in the target tier — below an explicit target model, above the account mark /
		// COOP_LOOP_MODEL.
		first := p.LeadLadder[0]
		if acct := first.Account(); acct != "" {
			a.cfg.SetActiveProfile(lead, acct)
		}
		a.cfg.SetTargetModel(lead, first.Model)
		a.cfg.SetTargetEffort(lead, first.Effort)
	}
	a.preset = p
}
