package cli

import (
	"fmt"
	"os"
	"slices"

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
			for _, provider := range p.RunnableRoleAgents() {
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
	// One policy serves every box this session may start, and a provider's API-key and signed-in
	// accounts need opposite grants (the broker withholds the API a sign-in is granted). So a
	// provider's optional accounts follow the kind of the account the session names first — the
	// editor's, a peer's, a preset's — or else of its first qualified one, the default when it is.
	keyed := map[string]bool{}
	anchor := func(target agents.Target) {
		if _, set := keyed[target.Provider]; !set {
			keyed[target.Provider], _ = box.AccountBrokersKey(a.cfg, target.Provider, target.Account())
		}
	}
	sameKind := func(target agents.Target) bool {
		key, err := box.AccountBrokersKey(a.cfg, target.Provider, target.Account())
		return err == nil && key == keyed[target.Provider]
	}
	if len(initial.Accounts) != 0 {
		anchor(initial)
	}
	selectedTargets, err := a.acpPresetNetworkTargets(selected)
	if err != nil {
		return nil, err
	}
	for _, target := range selectedTargets {
		anchor(target)
	}
	// Explicit peers share the concrete lead account on matching rungs; other rungs
	// use their ordinary account. Never require an unsigned default for a named lead.
	if len(peers) > 0 {
		rungs := []agents.Target{initial}
		if selected != nil {
			rungs = selected.LeadTargets
		}
		leads, err := expandLadder(a.cfg, initial.Provider, rungs)
		if err != nil {
			leads = []agents.Target{initial} // retain required unavailable scope for admission to refuse
		}
		for _, peer := range peers {
			for _, lead := range leads {
				target := peer
				if peer.Provider == lead.Provider && lead.Account() != "" {
					target.Accounts = slices.Clone(lead.Accounts)
				} else if target.Account() == "" {
					target.Accounts = []string{a.cfg.ActiveProfile(target.Provider)}
				}
				anchor(target)
				scope = append(scope, target)
			}
		}
	}
	peerValues := make([]string, len(peers))
	for i, peer := range peers {
		peerValues[i] = peer.String()
	}
	peerClosureQualified := func(leads []agents.Target) bool {
		if _, err := a.resolvePeersForLeads("coop acp", peerValues, leads); err != nil {
			return false
		}
		for _, lead := range leads {
			for _, peer := range peers {
				account := a.cfg.ActiveProfile(peer.Provider)
				if peer.Provider == lead.Provider && lead.Account() != "" {
					account = lead.Account()
				}
				target := peer
				target.Accounts = []string{account}
				if !acpNetworkQualified(a.cfg, target) || !sameKind(target) {
					return false
				}
			}
		}
		return true
	}
	add := func(providers []string) {
		for _, provider := range providers {
			for _, account := range accountsFor(a.cfg, provider) {
				target := agents.Target{Provider: provider, Accounts: []string{account}}
				if !acpNetworkQualified(a.cfg, target) {
					continue
				}
				if !peerClosureQualified([]agents.Target{target}) {
					continue // a toolbar choice must keep its complete explicit-peer closure runnable
				}
				anchor(target)
				if sameKind(target) {
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
					anchor(target)
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
	names, _ := a.acpPresetNames(repo)
	for _, name := range names {
		p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
		if err != nil {
			continue
		}
		targets, err := a.acpPresetNetworkTargets(p)
		if err != nil {
			continue
		}
		qualified := true
		if len(peers) > 0 {
			leads, err := expandLadder(a.cfg, p.Lead().Provider, p.LeadTargets)
			if err != nil {
				continue
			}
			if !peerClosureQualified(leads) {
				continue
			}
		}
		for _, target := range targets {
			qualified = qualified && acpNetworkQualified(a.cfg, target) && sameKind(target)
		}
		if qualified {
			scope = append(scope, targets...)
		}
	}
	return scope, nil
}
