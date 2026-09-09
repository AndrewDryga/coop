package egress

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

type Input struct {
	Rules  []Rule
	Origin Origin
}

// Compile combines already-authorized sources. It does not approve Input.Rules:
// the launcher must first check repository requests against host-owned approval.
func Compile(scope string, mode Mode, inputs []Input, bundles []Bundle, exportDestinations bool, key []byte) (Snapshot, error) {
	if !validScope(scope) {
		return Snapshot{}, errors.New("network capture requires an opaque owner scope")
	}
	if _, err := ParseMode(string(mode)); err != nil {
		return Snapshot{}, err
	}
	if len(key) != 32 {
		return Snapshot{}, errors.New("network authority requires a 32-byte owner key")
	}
	if len(inputs) > MaxRules || len(bundles) > MaxRules {
		return Snapshot{}, errors.New("too many network authority sources")
	}
	selected, err := SelectedBundles(bundles)
	if err != nil {
		return Snapshot{}, err
	}
	result := Snapshot{Version: Version, Scope: scope, Mode: mode, Grants: []Grant{}, ExportDestinations: exportDestinations,
		KeyID: keyed(key, "key-id", nil)[:16]}
	grants := map[string]Grant{}
	originEdges := 0
	add := func(rules []Rule, origin Origin) error {
		if !validOrigin(origin) {
			return errors.New("invalid network grant provenance")
		}
		normalized, err := NormalizeRules(rules)
		if err != nil {
			return err
		}
		if mode != Filtered && len(normalized) != 0 {
			return errors.New("egress rules require filtered mode")
		}
		for _, rule := range normalized {
			if rule.To.Provider != "" {
				return errors.New("bundle expansion must contain concrete rules")
			}
			canonical := ruleKey(rule)
			grant, exists := grants[canonical]
			if !exists {
				grant = Grant{ID: keyed(key, "rule", []byte(canonical))[:32], Rule: rule}
			}
			if !slices.Contains(grant.Origins, origin) {
				if originEdges >= MaxGrantOrigins {
					return errors.New("network grants exceed provenance budget")
				}
				originEdges++
				grant.Origins = append(grant.Origins, origin)
			}
			grants[canonical] = grant
			if len(grants) > MaxGrants {
				return errors.New("network bundle expansion exceeds grant limit")
			}
		}
		return nil
	}
	for _, bundle := range selected {
		if mode != Filtered {
			continue
		} // none never has an implicit provider exception.
		if err := add(bundle.Core, providerOrigin(Origin{Kind: "provider"}, bundle.Dependency(), "")); err != nil {
			return Snapshot{}, err
		}
		result.Dependencies = append(result.Dependencies, bundle.Dependency())
	}
	var featureRules []Rule
	seenFeatureRules := map[string]bool{}
	featureOrigins := map[featureKey][]Origin{}
	requestOrigins := 0
	for _, input := range inputs {
		rules, err := NormalizeRules(input.Rules)
		if err != nil {
			return Snapshot{}, err
		}
		if mode != Filtered && len(rules) != 0 {
			return Snapshot{}, errors.New("egress rules require filtered mode")
		}
		if len(rules) != 0 && (!validOrigin(input.Origin) || input.Origin.Provider != "" || input.Origin.Client != "" ||
			input.Origin.Backend != "" || input.Origin.AuthMode != "" || input.Origin.Feature != "" || input.Origin.BundleVersion != "") {
			return Snapshot{}, errors.New("invalid network request provenance")
		}
		for _, rule := range rules {
			if rule.To.Provider == "" {
				if err := add([]Rule{rule}, input.Origin); err != nil {
					return Snapshot{}, err
				}
				continue
			}
			canonical := ruleKey(rule)
			if !seenFeatureRules[canonical] {
				if len(featureRules) >= MaxExpandedRules {
					return Snapshot{}, errors.New("provider requests exceed aggregate budget")
				}
				seenFeatureRules[canonical] = true
				featureRules = append(featureRules, rule)
			}
			for _, feature := range rule.To.Features {
				request := featureKey{rule.To.Provider, feature}
				if !slices.Contains(featureOrigins[request], input.Origin) {
					if requestOrigins >= MaxGrantOrigins {
						return Snapshot{}, errors.New("provider requests exceed provenance budget")
					}
					requestOrigins++
					featureOrigins[request] = append(featureOrigins[request], input.Origin)
				}
			}
		}
	}
	expansions, err := FeatureExpansions(featureRules, selected)
	if err != nil {
		return Snapshot{}, err
	}
	for _, expansion := range expansions {
		for _, origin := range featureOrigins[featureKey{expansion.Provider, expansion.Feature}] {
			if err := add(expansion.Rules, providerOrigin(origin, expansion.Dependency, expansion.Feature)); err != nil {
				return Snapshot{}, err
			}
		}
	}
	keys := make([]string, 0, len(grants))
	for canonical := range grants {
		keys = append(keys, canonical)
	}
	slices.Sort(keys)
	for _, canonical := range keys {
		grant := grants[canonical]
		slices.SortFunc(grant.Origins, func(a, b Origin) int { return strings.Compare(originKey(a), originKey(b)) })
		result.Grants = append(result.Grants, grant)
	}
	slices.SortFunc(result.Dependencies, compareDependencies)
	result.Fingerprint = result.fingerprint(key)
	return result, nil
}

func validOrigin(origin Origin) bool {
	fields := []string{origin.Kind, origin.Name, origin.Version, origin.Feature, origin.Provider,
		string(origin.Client), origin.Backend, origin.AuthMode, origin.BundleVersion}
	size := 0
	for _, field := range fields {
		size += len(field)
		if size > 1024 || !safeProvenanceText(field) {
			return false
		}
	}
	return origin.Kind != ""
}

func providerOrigin(origin Origin, dependency Dependency, feature string) Origin {
	origin.Provider, origin.Client, origin.Backend, origin.AuthMode = dependency.Provider, dependency.Client, dependency.Backend, dependency.AuthMode
	origin.BundleVersion, origin.Feature = dependency.Version, feature
	return origin
}

func originKey(value Origin) string { data, _ := json.Marshal(value); return string(data) }

func validScope(value string) bool {
	return len(value) > 0 && len(value) <= 128 && strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789-") == ""
}

// Covers permits a smaller request inside an approved envelope, never a union
// accumulated from previous requests or a wildcard inferred from observations.
func Covers(approved, request Rule) bool {
	a, err := canonicalRule(approved)
	if err != nil {
		return false
	}
	r, err := normalizeRule(request)
	if err != nil || a.Protocol != r.Protocol {
		return false
	}
	if a.To.Provider != "" || r.To.Provider != "" {
		return a.To.Provider != "" && a.To.Provider == r.To.Provider && subset(r.To.Features, a.To.Features)
	}
	if a.To.Domain != "" || r.To.Domain != "" {
		if a.To.Domain == "" || r.To.Domain == "" || !MatchesDomain(a.To.Domain, r.To.Domain) {
			return false
		}
	} else {
		ap, ae := netip.ParsePrefix(a.To.CIDR)
		rp, re := netip.ParsePrefix(r.To.CIDR)
		if ae != nil || re != nil || ap.Addr().BitLen() != rp.Addr().BitLen() || ap.Bits() > rp.Bits() || !ap.Contains(rp.Addr()) {
			return false
		}
	}
	if !subset(r.Ports, a.Ports) || !subset(r.Types, a.Types) {
		return false
	}
	return len(a.Codes) == 0 || (len(r.Codes) != 0 && subset(r.Codes, a.Codes))
}

func subset[T comparable](values, allowed []T) bool {
	for _, value := range values {
		if !slices.Contains(allowed, value) {
			return false
		}
	}
	return true
}

func (s Snapshot) fingerprint(key []byte) string {
	s.Fingerprint = ""
	data, _ := json.Marshal(s)
	return keyed(key, "snapshot-v1", data)
}

// Verify checks the captured bytes, not a re-expansion of mutable bundle names.
func (s Snapshot) Verify(key []byte) error {
	if len(key) != 32 || s.Version != Version || !validScope(s.Scope) || s.KeyID != keyed(key, "key-id", nil)[:16] || s.Fingerprint == "" || !hmac.Equal([]byte(s.Fingerprint), []byte(s.fingerprint(key))) {
		return errors.New("network snapshot identity or fingerprint mismatch")
	}
	if _, err := ParseMode(string(s.Mode)); err != nil {
		return err
	}
	if len(s.Grants) > MaxGrants || (s.Mode != Filtered && len(s.Grants) != 0) {
		return errors.New("invalid captured network grants")
	}
	seen := map[string]bool{}
	for _, grant := range s.Grants {
		rule, err := canonicalRule(grant.Rule)
		if err != nil || rule.To.Provider != "" || ruleKey(rule) != ruleKey(grant.Rule) || len(grant.Origins) == 0 {
			return errors.New("noncanonical captured network rule")
		}
		id := keyed(key, "rule", []byte(ruleKey(rule)))[:32]
		if grant.ID != id || seen[id] {
			return errors.New("duplicate or invalid captured rule identity")
		}
		seen[id] = true
	}
	return nil
}

// Clone keeps every mutable slice/map out of a caller's frozen authority.
func (s Snapshot) Clone() Snapshot {
	data, _ := json.Marshal(s)
	var clone Snapshot
	_ = json.Unmarshal(data, &clone)
	return clone
}

type Decision struct {
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason"`
	RuleID     string `json:"rule_id,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

func (s Snapshot) Domain(name string, port int) Decision {
	normalized, err := NormalizeDomain(name, false)
	if err != nil {
		return Decision{Reason: "unapproved_name", Resolution: "not_evaluated"}
	}
	if s.Mode == Open {
		return Decision{Allowed: true, Reason: "open", Resolution: "not_evaluated"}
	}
	if s.Mode != Filtered {
		return Decision{Reason: "protocol_not_allowed", Resolution: "not_evaluated"}
	}
	for _, grant := range s.Grants {
		if grant.Rule.Protocol == "tls" && MatchesDomain(grant.Rule.To.Domain, normalized) && slices.Contains(grant.Rule.Ports, port) {
			return Decision{Allowed: true, Reason: "rule_allowed", RuleID: grant.ID, Resolution: "not_evaluated"}
		}
	}
	return Decision{Reason: "unapproved_name", Resolution: "not_evaluated"}
}

// AdmitsName is a DNS admission check, deliberately not an inferred TLS attempt.
func (s Snapshot) AdmitsName(name string) bool {
	normalized, err := NormalizeDomain(name, false)
	if err != nil || s.Mode == None {
		return false
	}
	if s.Mode == Open {
		return true
	}
	if s.Mode != Filtered {
		return false
	}
	for _, grant := range s.Grants {
		if grant.Rule.Protocol == "tls" && MatchesDomain(grant.Rule.To.Domain, normalized) {
			return true
		}
	}
	return false
}

func (s Snapshot) Address(ip netip.Addr, protocol string, port, kind, code int, protected []netip.Prefix) Decision {
	ip = ip.Unmap()
	if Protected(ip, protected) {
		return Decision{Reason: "protected_destination"}
	}
	if s.Mode == Open {
		return Decision{Allowed: true, Reason: "open"}
	}
	if s.Mode != Filtered {
		return Decision{Reason: "protocol_not_allowed"}
	}
	for _, grant := range s.Grants {
		rule := grant.Rule
		prefix, err := netip.ParsePrefix(rule.To.CIDR)
		if err != nil || !prefix.Contains(ip) || rule.Protocol != protocol {
			continue
		}
		if (protocol == "tcp" || protocol == "udp") && slices.Contains(rule.Ports, port) {
			return Decision{Allowed: true, Reason: "rule_allowed", RuleID: grant.ID}
		}
		if (protocol == "icmp" || protocol == "icmpv6") && slices.Contains(rule.Types, fmt.Sprint(kind)) && (len(rule.Codes) == 0 || slices.Contains(rule.Codes, code)) {
			return Decision{Allowed: true, Reason: "rule_allowed", RuleID: grant.ID}
		}
	}
	return Decision{Reason: "protocol_not_allowed"}
}

var protectedPrefixes = prefixes("0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "100.100.100.200/32", "168.63.129.16/32", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "fe80::/10", "ff00::/8", "fd00:ec2::254/128")
var nonPublicPrefixes = prefixes("100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")

func prefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, len(values))
	for i, value := range values {
		result[i] = netip.MustParsePrefix(value)
	}
	return result
}

func Protected(ip netip.Addr, additional []netip.Prefix) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return true
	}
	ip = ip.Unmap()
	for _, prefix := range protectedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	for _, prefix := range additional {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// ProtectedRanges returns a copy for the namespace enforcer. Domain admission
// and packet enforcement must share these hard denials, not separate lists.
func ProtectedRanges(additional []netip.Prefix) []netip.Prefix {
	return append(slices.Clone(protectedPrefixes), additional...)
}

// PublicAnswer is intentionally stricter than IsGlobalUnicast, which includes
// private, shared, documentation and benchmarking address space.
func PublicAnswer(ip netip.Addr, protected []netip.Prefix) bool {
	if !ip.IsValid() || ip.Is4In6() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || Protected(ip, protected) {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip)
}

// RequireTLS443 is a capability gate, not a lossy compiler. Wider rules remain
// rejected until the corresponding runtime and observation paths are qualified.
func (s Snapshot) RequireTLS443(wildcards bool) error {
	if s.Mode != Filtered {
		return errors.New("gateway requires filtered authority")
	}
	for _, grant := range s.Grants {
		rule := grant.Rule
		if rule.Protocol != "tls" || len(rule.Ports) != 1 || rule.Ports[0] != 443 || (!wildcards && strings.HasPrefix(rule.To.Domain, "*.")) {
			return errors.New("unsupported_capability: this gateway requires qualified TLS443 domain rules")
		}
	}
	return nil
}
