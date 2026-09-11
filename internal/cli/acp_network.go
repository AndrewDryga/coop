package cli

import (
	"fmt"
	"os"

	"github.com/AndrewDryga/coop/internal/acpctl"
	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/egress"
	"github.com/AndrewDryga/coop/internal/preset"
)

func acpNetworkQualified(provider string) bool {
	ag, ok := agents.Get(provider)
	if !ok {
		return false
	}
	_, err := ag.NetworkBundle(agents.NetworkBundleInput{Client: egress.ClientACP})
	return err == nil
}

// This is admission-only scope: inner boxes still mount only their selected lead
// and explicit peers/roles. Open and offline admission ignores these bundles.
// Required providers stay intact so filtered admission rejects an unsupported
// request; optional choices contribute only whole, qualified provider closures.
func (a *app) acpNetworkScope(repo string, peers []agents.Target, selected *preset.Preset) ([]agents.Target, error) {
	scope := append([]agents.Target{}, peers...)
	add := func(providers []string) {
		for _, provider := range providers {
			scope = append(scope, agents.Target{Provider: provider})
		}
	}
	add(selected.RunnableProviders())
	// A reload restores its effective selection after admission. Include it as
	// required now, rather than admitting only the original editor arguments.
	if path := os.Getenv("COOP_ACP_RESUME_STATE"); path != "" {
		if state, err := acpctl.ReadResumeState(path); err == nil {
			a.acpResume = &state
			provider := state.Ctrl.Target.Provider
			if provider == "" {
				provider = state.Ctrl.Lead
			}
			if provider != "" {
				add([]string{provider})
			}
			if name := state.Ctrl.Selection.Preset; name != "" {
				p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
				if err != nil {
					return nil, err
				}
				add(p.RunnableProviders())
			}
		} else {
			fmt.Fprintf(os.Stderr, "⚠ Could not restore the editor session\n\n      The saved session state could not be read.\n      %v\n\n  Starting a new session.\n", err)
		}
	}
	for _, provider := range agents.Names() {
		if len(accountsFor(a.cfg, provider)) > 0 && acpNetworkQualified(provider) {
			add([]string{provider})
		}
	}
	for _, name := range a.acpPresetNames(repo) {
		p, err := preset.Load(repo, a.cfg.GlobalPresetsDir(), name)
		if err != nil {
			continue
		}
		providers := p.RunnableProviders()
		qualified := true
		for _, provider := range providers {
			qualified = qualified && acpNetworkQualified(provider)
		}
		if qualified {
			add(providers)
		}
	}
	return scope, nil
}
