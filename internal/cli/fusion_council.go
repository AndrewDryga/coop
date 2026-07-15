package cli

import (
	"fmt"
	"slices"
	"strings"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
)

type fusionSelfPolicy int

const (
	fusionRejectSelf fusionSelfPolicy = iota
	fusionExcludeSelf
)

// fusionCouncil is the one resolved council contract shared by launch and box assembly. Peers
// retain explicit target model/effort for credentials and env; Members are the exact ordered
// coop-consult labels the governor receives (provider names first, then effective role names).
type fusionCouncil struct {
	Peers            []agents.Target
	Members          []string
	UnavailableRoles []string
}

// resolveFusionCouncil validates and resolves one governor's council. Explicit peers are unique
// by provider. Preset roles are unique by role name and count only when one target provider has
// mounted credentials; a role targeting the governor provider rides the governor credential.
func resolveFusionCouncil(governor string, peers []agents.Target, p *preset.Preset, self fusionSelfPolicy, available []string) (fusionCouncil, error) {
	var roles []preset.Role
	if p != nil {
		roles = p.ConsultRoles(governor)
	}
	roleNames := make(map[string]bool, len(roles))
	for _, role := range roles {
		roleNames[role.Name] = true
	}

	seen := make(map[string]bool, len(peers))
	var out fusionCouncil
	for _, peer := range peers {
		provider := peer.Provider
		if seen[provider] {
			return fusionCouncil{}, fmt.Errorf("fusion: --peer %s appears more than once; name each provider once", provider)
		}
		seen[provider] = true
		if roleNames[provider] {
			return fusionCouncil{}, fmt.Errorf("fusion: --peer %s conflicts with preset role %q; rename the role or drop the peer", provider, provider)
		}
		if provider == governor {
			if self == fusionRejectSelf {
				return fusionCouncil{}, fmt.Errorf("fusion: governor %s cannot also be an explicit --peer", governor)
			}
			continue
		}
		out.Peers = append(out.Peers, peer)
		out.Members = append(out.Members, provider)
	}

	for _, role := range roles {
		usable := false
		for _, target := range role.TargetLadder() {
			if target.Provider == governor || slices.Contains(available, target.Provider) {
				usable = true
				break
			}
		}
		if usable {
			out.Members = append(out.Members, role.Name)
		} else {
			out.UnavailableRoles = append(out.UnavailableRoles, role.Name)
		}
	}
	if len(out.Members) == 0 {
		if len(out.UnavailableRoles) > 0 {
			return fusionCouncil{}, fmt.Errorf("fusion: preset council role(s) %s have no target with mounted credentials", strings.Join(out.UnavailableRoles, ", "))
		}
		return fusionCouncil{}, fmt.Errorf("fusion needs its council: name an explicit --peer or use a preset with an effective consult role")
	}
	return out, nil
}

// resolveACPFusionCouncil resolves the current spawn and rejects a preset whose council would
// collapse on any reachable lead provider. The inner process calls this again after every
// COOP_ACP_TARGET change, so the filtered Peers always match the active governor.
func resolveACPFusionCouncil(governor string, peers []agents.Target, p *preset.Preset, available []string, reachable []agents.Target) (fusionCouncil, error) {
	providers := []string{governor}
	if p != nil {
		providers = nil
		for _, target := range reachable {
			if !slices.Contains(providers, target.Provider) {
				providers = append(providers, target.Provider)
			}
		}
		if len(providers) == 0 {
			return fusionCouncil{}, fmt.Errorf("coop acp fusion: preset %s has no reachable lead target", p.Name)
		}
	}

	var current fusionCouncil
	var first fusionCouncil
	var unavailable []string
	for _, provider := range providers {
		council, err := resolveFusionCouncil(provider, peers, p, fusionExcludeSelf, available)
		if err != nil {
			if p != nil {
				return fusionCouncil{}, fmt.Errorf("coop acp fusion: preset %s lead provider %s: %w", p.Name, provider, err)
			}
			return fusionCouncil{}, fmt.Errorf("coop acp fusion: %w", err)
		}
		if len(first.Members) == 0 {
			first = council
		}
		for _, role := range council.UnavailableRoles {
			if !slices.Contains(unavailable, role) {
				unavailable = append(unavailable, role)
			}
		}
		if provider == governor {
			current = council
		}
	}
	if len(current.Members) == 0 {
		current = first // outer supervisor may begin on a skipped declared lead; its child uses rung one.
	}
	current.UnavailableRoles = unavailable
	return current, nil
}
