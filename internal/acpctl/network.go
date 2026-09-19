package acpctl

import (
	"fmt"
	"slices"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
)

// LimitNetworkTargets freezes the exact provider/account scope admitted by this supervisor.
// Call only before serving the editor. Open/offline sessions leave it unset.
// Account changes cannot reuse one portable family to reach a host-bound one.
func (c *Control) LimitNetworkTargets(targets []agents.Target) {
	c.networkAccounts = make(map[string]map[string]bool)
	for _, target := range targets {
		if target.Provider == "" {
			continue
		}
		if c.networkAccounts[target.Provider] == nil {
			c.networkAccounts[target.Provider] = map[string]bool{}
		}
		for _, account := range target.Accounts {
			c.networkAccounts[target.Provider][account] = true
		}
	}
	c.accounts = c.networkAccountsFor(c.lead)
	c.creds = slices.Clone(c.accounts)
}

func (c *Control) networkProviderAllowed(provider string) bool {
	return c.networkAccounts == nil || len(c.networkAccounts[provider]) > 0
}

func (c *Control) networkAccountAllowed(provider, account string) bool {
	return c.networkAccounts == nil || c.networkAccounts[provider][account]
}

func (c *Control) networkAccountsFor(provider string) []string {
	accounts := c.host.AccountsFor(c.cfg, provider)
	if c.networkAccounts == nil {
		return accounts
	}
	return slices.DeleteFunc(accounts, func(account string) bool { return !c.networkAccountAllowed(provider, account) })
}

// ResolveNetworkTarget gives a provider-only warm/model probe the first account
// in the same frozen order the selector exposes. Normal spawns already carry a
// concrete account and are returned unchanged.
func (c *Control) ResolveNetworkTarget(target agents.Target) agents.Target {
	if target.Account() != "" {
		return target
	}
	if accounts := c.networkAccountsFor(target.Provider); len(accounts) > 0 {
		target.Accounts = []string{accounts[0]}
	}
	return target
}

func (c *Control) networkPresetAllowed(name string) bool {
	return c.validateNetworkPreset(name) == nil
}

func (c *Control) validateNetworkPreset(name string) error {
	if c.networkAccounts == nil || name == "" {
		return nil
	}
	p, err := preset.Load(c.repo, c.cfg.GlobalPresetsDir(), name)
	if err != nil {
		return err
	}
	for _, rung := range p.LeadTargets {
		leads, expandErr := c.host.ExpandLadder(c.cfg, p.Lead().Provider, []agents.Target{rung})
		if expandErr != nil {
			provider := rung.Provider
			if provider == "" {
				provider = p.Lead().Provider
			}
			accounts := rung.Accounts
			if len(accounts) == 0 {
				accounts = c.host.AccountsFor(c.cfg, provider)
			}
			if len(accounts) == 0 {
				accounts = []string{c.cfg.ActiveProfile(provider)}
			}
			for _, account := range accounts {
				lead := rung
				lead.Provider, lead.Accounts = provider, []string{account}
				leads = append(leads, lead)
			}
		}
		for _, lead := range leads {
			if !c.networkAccountAllowed(lead.Provider, lead.Account()) {
				return fmt.Errorf("preset %s needs %s, which is unavailable under this session's network rules", name, lead.String())
			}
			for _, provider := range p.RunnableRoleAgents() {
				account := c.cfg.ActiveProfile(provider)
				if provider == lead.Provider && lead.Account() != "" {
					account = lead.Account()
				}
				if !c.networkAccountAllowed(provider, account) {
					return fmt.Errorf("preset %s needs %s account %q, which is unavailable under this session's network rules", name, provider, account)
				}
			}
		}
	}
	return nil
}

// ValidateNetworkTarget also guards restored state and presets edited since the
// toolbar was rendered. Refuse the complete selection before any box is reused
// or started; never discard a role/rung to make the remaining preset runnable.
func (c *Control) ValidateNetworkTarget(target agents.Target, presetName string) error {
	if !c.networkProviderAllowed(target.Provider) {
		return fmt.Errorf("%s is unavailable under this session's network rules", target.Provider)
	}
	if account := target.Account(); account != "" && !c.networkAccountAllowed(target.Provider, account) {
		return fmt.Errorf("%s account %q is unavailable under this session's network rules", target.Provider, account)
	}
	return c.validateNetworkPreset(presetName)
}
