package acpctl

import (
	"fmt"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
)

// LimitNetworkProviders freezes the provider scope admitted by this supervisor.
// Call only before serving the editor. Open/offline sessions leave it unset.
// Account changes cannot add a provider whose endpoints were never captured.
func (c *Control) LimitNetworkProviders(providers []string) {
	c.networkProviders = make(map[string]bool, len(providers))
	for _, provider := range providers {
		c.networkProviders[provider] = true
	}
}

func (c *Control) networkProviderAllowed(provider string) bool {
	return c.networkProviders == nil || c.networkProviders[provider]
}

func (c *Control) networkPresetAllowed(name string) bool {
	return c.validateNetworkPreset(name) == nil
}

func (c *Control) validateNetworkPreset(name string) error {
	if c.networkProviders == nil || name == "" {
		return nil
	}
	p, err := preset.Load(c.repo, c.cfg.GlobalPresetsDir(), name)
	if err != nil {
		return err
	}
	for _, provider := range p.RunnableProviders() {
		if !c.networkProviderAllowed(provider) {
			return fmt.Errorf("preset %s needs %s, which is unavailable under this session's network rules", name, provider)
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
	return c.validateNetworkPreset(presetName)
}
