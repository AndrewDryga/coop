package cli

// --preset <name> on the launch surfaces (coop <agent>, loop, fusion, acp, fork --loop)
// loads .agent/presets/<name>/preset.yaml and applies it: the preset's lead agent is the
// default when the command names none, its lead/role models and credentials seed the
// run's selections (explicit CLI flags still win), and box.Run mounts the generated
// role contracts + wrappers from the RunSpec's Preset.

import (
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
	return preset.Load(repo, name)
}

// presetLeadAgent resolves the launched agent under a preset: an agent the command
// named explicitly wins; otherwise the preset's lead.agent is the default.
func presetLeadAgent(p *preset.Preset, agent string, explicit bool) string {
	if p != nil && !explicit {
		return p.LeadAgent
	}
	return agent
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
	}
	if lead == p.LeadAgent && len(p.LeadModels) > 0 {
		// A run that doesn't rotate (single, or the loop before its first applyTarget) uses the
		// ladder's FIRST entry: its account if pinned (else the marked default), and its model in
		// the target tier — below an explicit --model, above the account mark / COOP_LOOP_MODEL.
		first := p.LeadModels[0]
		if first.Credential != "" {
			a.cfg.SetActiveProfile(lead, first.Credential)
		}
		a.cfg.SetTargetModel(lead, first.Model)
	}
	a.preset = p
}
