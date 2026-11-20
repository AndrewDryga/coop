package workerproto

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/preset"
)

// JobPreset is the approved recipe frozen by the controller. Targets for the
// lead live in JobSpec.Targets, so there is only one authority for its ladder.
// Role prompts are contents, never paths relative to a checked-out PR.
type JobPreset struct {
	Version    int             `json:"version"`
	Name       string          `json:"name"`
	LeadPrompt string          `json:"lead_prompt"`
	Roles      []JobPresetRole `json:"roles"`
}

type JobPresetRole struct {
	Name    string   `json:"name"`
	Mode    string   `json:"mode"`
	Targets []string `json:"targets"`
	When    []string `json:"when"`
	Prompt  string   `json:"prompt"`
}

var jobRoleName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

// ValidJobRoleName reports whether a frozen role name can appear in a job or its history.
func ValidJobRoleName(name string) bool { return jobRoleName.MatchString(name) }

// Recipe rebuilds box execution state from the admitted document. It does no
// filesystem discovery and never resolves a role through the job's source.
func (p JobPreset) Recipe(leadTargets []string, readOnly bool) (*preset.Preset, error) {
	if p.Version != 1 || !preset.ValidName(p.Name) || len(p.Name) > 64 || len(p.LeadPrompt) > 32<<10 || p.Roles == nil || len(p.Roles) > 16 {
		return nil, errors.New("invalid frozen job preset")
	}
	lead, err := frozenTargets(leadTargets)
	if err != nil {
		return nil, err
	}
	result := &preset.Preset{Name: p.Name, LeadPromptText: p.LeadPrompt, LeadTargets: lead}
	seen := make(map[string]bool)
	for _, role := range p.Roles {
		if !ValidJobRoleName(role.Name) || seen[role.Name] || role.When == nil || len(role.When) > 16 || len(role.Prompt) > 32<<10 {
			return nil, errors.New("invalid frozen preset role")
		}
		for _, hint := range role.When {
			if strings.TrimSpace(hint) == "" || len(hint) > 512 {
				return nil, errors.New("invalid preset routing hint")
			}
		}
		seen[role.Name] = true
		switch role.Mode {
		case preset.ModeConsult:
		case preset.ModeDelegate:
			if readOnly {
				return nil, fmt.Errorf("read-only job cannot grant a delegate role %q", role.Name)
			}
		case preset.ModeNative:
			if len(role.Targets) != 1 {
				return nil, fmt.Errorf("native role %q needs one target", role.Name)
			}
		default:
			return nil, fmt.Errorf("unknown preset role mode %q", role.Mode)
		}
		targets, err := frozenTargets(role.Targets)
		if err != nil {
			return nil, err
		}
		result.Roles = append(result.Roles, preset.Role{Name: role.Name, Mode: role.Mode, Targets: targets, When: role.When, PromptText: role.Prompt})
	}
	providers := make([]string, 0, len(lead))
	for _, t := range lead {
		if _, err := PresetAccounts(result, t); err != nil {
			return nil, err
		}
		providers = append(providers, t.Provider)
	}
	if err := result.CheckLeads(providers...); err != nil {
		return nil, err
	}
	return result, nil
}

// PresetAccounts returns the active lead and the exact accounts used by its roles,
// in recipe order. The box mounts one account for each provider. Native peers
// inherit the lead's login, including when the lead rotates to another rung.
func PresetAccounts(recipe *preset.Preset, lead agent.Target) ([]agent.Target, error) {
	selected := map[string]string{lead.Provider: lead.Account()}
	targets := []agent.Target{lead}
	for _, role := range recipe.Roles {
		for _, peer := range role.Targets {
			if account, exists := selected[peer.Provider]; exists {
				if account != peer.Account() {
					return nil, fmt.Errorf("role %q requests a different %s account in the same box", role.Name, peer.Provider)
				}
				continue
			}
			selected[peer.Provider] = peer.Account()
			targets = append(targets, peer)
		}
	}
	return targets, nil
}

func frozenTargets(specs []string) ([]agent.Target, error) {
	if len(specs) == 0 || len(specs) > 4 {
		return nil, errors.New("frozen preset target ladder must contain 1..4 targets")
	}
	targets := make([]agent.Target, 0, len(specs))
	for _, spec := range specs {
		t, err := agent.ParseTarget(spec)
		if len(spec) > 256 || err != nil || t.String() != spec || len(t.Accounts) != 1 {
			return nil, errors.New("frozen preset target needs a canonical target with one explicit account")
		}
		targets = append(targets, t)
	}
	return targets, nil
}
