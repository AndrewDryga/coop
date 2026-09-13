package cli

import (
	"fmt"
	"os"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/box"
	"github.com/AndrewDryga/coop/internal/config"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/preset"
)

func acpNetworkQualified(cfg *config.Config, target agents.Target) bool {
	_, err := box.NetworkTargetBundle(cfg, target, egress.ClientACP)
	return err == nil
}

// acpPresetNetworkTargets materializes the credential scope a preset can
// actually mount. Lead entries retain their normal account fan-out; role
// providers use the active profile, exactly as box credential projection does.
// Keeping concrete accounts here prevents one portable sibling from qualifying
// a different, host-bound default account.
func (a *app) acpPresetNetworkTargets(p *preset.Preset) ([]agents.Target, error) {
	if p == nil {
		return nil, nil
	}
	var targets []agents.Target
	seen := map[string]bool{}
	add := func(target agents.Target) {
		if !seen[target.String()] {
			seen[target.String()] = true
			targets = append(targets, target)
		}
	}
	for _, rung := range p.LeadTargets {
		leads, err := expandLadder(a.cfg, p.Lead().Provider, []agents.Target{rung})
		if err != nil {
			provider := rung.Provider
			if provider == "" {
				provider = p.Lead().Provider
			}
			accounts := rung.Accounts
			if len(accounts) == 0 {
				accounts = accountsFor(a.cfg, provider)
			}
			if len(accounts) == 0 {
				accounts = []string{a.cfg.ActiveProfile(provider)}
			}
			for _, account := range accounts {
				lead := rung
				lead.Provider, lead.Accounts = provider, []string{account}
				leads = append(leads, lead)
			}
		}
		for _, lead := range leads {
			add(lead)
			for _, provider := range p.RunnableRoleAgents(lead.Provider) {
				account := a.cfg.ActiveProfile(provider)
				if provider == lead.Provider && lead.Account() != "" {
					account = lead.Account()
				}
				add(agents.Target{Provider: provider, Accounts: []string{account}})
			}
		}
	}
	return targets, nil
}

// This is admission-only scope: inner boxes still mount only their selected lead
// and explicit peers/roles. Open and offline admission ignores these bundles.
// Required providers stay intact so filtered admission rejects an unsupported
// request; optional choices contribute only whole, qualified provider closures.
func (a *app) acpNetworkScope(repo string, initial agents.Target, peers []agents.Target, selected *preset.Preset) ([]agents.Target, error) {
	var scope []agents.Target
	// Ad-hoc peers use their active account in the child. Preserve that exact
	// identity so filtered admission cannot be borrowed from a portable sibling.
	for _, peer := range peers {
		if len(peer.Accounts) == 0 {
			peer.Accounts = []string{a.cfg.ActiveProfile(peer.Provider)}
		}
		scope = append(scope, peer)
	}
	add := func(providers []string) {
		for _, provider := range providers {
			for _, account := range accountsFor(a.cfg, provider) {
				target := agents.Target{Provider: provider, Accounts: []string{account}}
				if acpNetworkQualified(a.cfg, target) {
					scope = append(scope, target)
				}
			}
		}
	}
	addRequired := func(providers []string) {
		for _, provider := range providers {
			before := len(scope)
			add([]string{provider})
			if len(scope) == before {
				scope = append(scope, agents.Target{Provider: provider})
			}
		}
	}
	if initial.Provider != "" {
		if len(initial.Accounts) == 0 {
			addRequired([]string{initial.Provider})
		} else {
			scope = append(scope, initial)
		}
	}
	selectedTargets, err := a.acpPresetNetworkTargets(selected)
	if err != nil {
		return nil, err
	}
	scope = append(scope, selectedTargets...)
	// A reload restores its effective selection after admission. Include it as
	// required now, rather than admitting only the original editor arguments.
	if path := os.Getenv("COOP_ACP_RESUME_STATE"); path != "" {
		if state, err := acpctl.ReadResumeState(path); err == nil {
			a.acpResume = &state
			target := state.Ctrl.Target
			provider := target.Provider
			if provider == "" {
				provider = state.Ctrl.Lead
				target.Provider = provider
			}
			if provider != "" {
				if len(target.Accounts) == 0 {
					addRequired([]string{provider})
				} else {
					scope = append(scope, target)
				}
			}
			if name := state.Ctrl.Selection.Preset; name != "" {
				p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
				if err != nil {
					return nil, err
				}
				targets, err := a.acpPresetNetworkTargets(p)
				if err != nil {
					return nil, err
				}
				scope = append(scope, targets...)
			}
		} else {
			fmt.Fprintf(os.Stderr, "⚠ Could not restore the editor session\n\n      The saved session state could not be read.\n      %v\n\n  Starting a new session.\n", err)
		}
	}
	for _, provider := range agents.Names() {
		add([]string{provider})
	}
	for _, name := range a.acpPresetNames(repo) {
		p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
		if err != nil {
			continue
		}
		targets, err := a.acpPresetNetworkTargets(p)
		if err != nil {
			continue
		}
		qualified := true
		for _, target := range targets {
			qualified = qualified && acpNetworkQualified(a.cfg, target)
		}
		if qualified {
			scope = append(scope, targets...)
		}
	}
	return scope, nil
}
