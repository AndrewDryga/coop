package egress

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

type Client string

const (
	ClientCLI Client = "cli"
	ClientACP Client = "acp"
)

// Variant selects behavior; Dependency adds the immutable bundle release. No
// account name or credential path belongs in either identity.
type Variant struct {
	Provider string `json:"provider"`
	Client   Client `json:"client"`
	Backend  string `json:"backend"`
	AuthMode string `json:"auth_mode"`
}

func (b Bundle) Variant() Variant { return Variant{b.Provider, b.Client, b.Backend, b.AuthMode} }
func (b Bundle) Dependency() Dependency {
	return Dependency{Provider: b.Provider, Client: b.Client, Backend: b.Backend, AuthMode: b.AuthMode, Version: b.Version}
}

func compareDependencies(a, b Dependency) int {
	return cmp.Or(strings.Compare(a.Provider, b.Provider), strings.Compare(string(a.Client), string(b.Client)),
		strings.Compare(a.Backend, b.Backend), strings.Compare(a.AuthMode, b.AuthMode), strings.Compare(a.Version, b.Version))
}

// SelectedBundles validates every definition before expansion or persistence.
// Callers deduplicate repeated selected targets; competing releases of the same
// variant must never silently union their authority.
func SelectedBundles(bundles []Bundle) ([]Bundle, error) {
	if len(bundles) > MaxRules {
		return nil, errors.New("too many selected provider bundles")
	}
	result := make([]Bundle, 0, len(bundles))
	seen := map[Variant]bool{}
	occurrences := 0
	normalize := func(rules []Rule) ([]Rule, error) {
		if len(rules) > MaxExpandedRules-occurrences {
			return nil, errors.New("provider definitions exceed concrete rule budget")
		}
		occurrences += len(rules)
		normalized, err := NormalizeRules(rules)
		if err != nil {
			return nil, err
		}
		for _, rule := range normalized {
			if rule.To.Provider != "" {
				return nil, errors.New("nested provider bundle expansion")
			}
		}
		return normalized, nil
	}
	for _, bundle := range bundles {
		if !validLabel(bundle.Provider) || !validLabel(bundle.Backend) || !validLabel(bundle.AuthMode) ||
			(bundle.Client != ClientCLI && bundle.Client != ClientACP) || len(bundle.Version) == 0 || len(bundle.Version) > 128 ||
			strings.IndexFunc(bundle.Version, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 ||
			len(bundle.Features) > MaxConstraints || len(bundle.Sources) > MaxConstraints {
			return nil, errors.New("invalid provider bundle identity or definition bounds")
		}
		if seen[bundle.Variant()] {
			return nil, errors.New("duplicate selected provider variant")
		}
		seen[bundle.Variant()] = true
		for _, source := range bundle.Sources {
			if len(source) == 0 || len(source) > 2048 || !safeProvenanceText(source) {
				return nil, errors.New("invalid provider bundle source")
			}
		}
		core, err := normalize(bundle.Core)
		if err != nil {
			return nil, err
		}
		features := make(map[string][]Rule, len(bundle.Features))
		for feature, rules := range bundle.Features {
			if !validLabel(feature) || len(rules) == 0 {
				return nil, errors.New("invalid provider feature definition")
			}
			features[feature], err = normalize(rules)
			if err != nil {
				return nil, err
			}
		}
		bundle.Core, bundle.Features, bundle.Sources = core, features, append([]string{}, bundle.Sources...)
		slices.Sort(bundle.Sources)
		bundle.Sources = slices.Compact(bundle.Sources)
		if data, err := json.Marshal(bundle); err != nil || len(data) > MaxDocumentBytes {
			return nil, errors.New("normalized provider bundle exceeds 64 KiB byte limit")
		}
		result = append(result, bundle)
	}
	slices.SortFunc(result, func(a, b Bundle) int { return compareDependencies(a.Dependency(), b.Dependency()) })
	return result, nil
}

type featureKey struct{ Provider, Feature string }

// FeatureExpansion is shared by approval review and concrete policy compilation.
// Version is evidence; approval equivalence is variant + feature + exact rules.
type FeatureExpansion struct {
	Dependency
	Feature string `json:"feature"`
	Rules   []Rule `json:"rules"`
}

func FeatureExpansions(rules []Rule, bundles []Bundle) ([]FeatureExpansion, error) {
	selected, err := SelectedBundles(bundles)
	if err != nil {
		return nil, err
	}
	if len(rules) > MaxExpandedRules {
		return nil, errors.New("too many provider feature requests")
	}
	byProvider := map[string][]Bundle{}
	for _, bundle := range selected {
		byProvider[bundle.Provider] = append(byProvider[bundle.Provider], bundle)
	}
	requests := map[featureKey]bool{}
	for _, rule := range rules {
		normalized, err := normalizeRule(rule)
		if err != nil {
			return nil, err
		}
		if normalized.To.Provider == "" {
			continue
		}
		if len(byProvider[normalized.To.Provider]) == 0 {
			return nil, errors.New("provider request is not an authorized selected target")
		}
		for _, feature := range normalized.To.Features {
			requests[featureKey{normalized.To.Provider, feature}] = true
			if len(requests) > MaxFeatureExpansions {
				return nil, errors.New("provider feature requests exceed expansion budget")
			}
		}
	}
	var result []FeatureExpansion
	occurrences := 0
	for request := range requests {
		for _, bundle := range byProvider[request.Provider] {
			expansion := bundle.Features[request.Feature]
			if len(expansion) == 0 {
				return nil, errors.New("provider feature is unsupported by a selected client/backend/auth variant")
			}
			if len(result) >= MaxFeatureExpansions || len(expansion) > MaxExpandedRules-occurrences {
				return nil, errors.New("provider feature expansion exceeds its aggregate budget")
			}
			occurrences += len(expansion)
			result = append(result, FeatureExpansion{Dependency: bundle.Dependency(), Feature: request.Feature, Rules: expansion})
		}
	}
	slices.SortFunc(result, func(a, b FeatureExpansion) int {
		return cmp.Or(compareDependencies(a.Dependency, b.Dependency), strings.Compare(a.Feature, b.Feature))
	})
	return result, nil
}
