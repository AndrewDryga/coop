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
	switch {
	case a.To.Service != "" || r.To.Service != "":
		// A sidecar grant is bound to a NAME, not an address: the launch reads
		// the one container that name resolves to. Coverage is that same name.
		if a.To.Service == "" || a.To.Service != r.To.Service {
			return false
		}
	case a.To.Domain != "" || r.To.Domain != "":
		if a.To.Domain == "" || r.To.Domain == "" || !MatchesDomain(a.To.Domain, r.To.Domain) {
			return false
		}
	default:
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

// TLSPorts is the exact set of ports this policy grants TLS on, sorted. It is
// what the gateway captures and therefore what the guard may be asked to route:
// a port outside it never reaches the guard, and the guard refuses one anyway.
func (s Snapshot) TLSPorts() []int {
	var ports []int
	if s.Mode != Filtered {
		return nil
	}
	for _, grant := range s.Grants {
		if grant.Rule.Protocol != "tls" {
			continue
		}
		for _, port := range grant.Rule.Ports {
			if !slices.Contains(ports, port) {
				ports = append(ports, port)
			}
		}
	}
	slices.Sort(ports)
	return ports
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

// RequireSupported is the capability gate every accepted rule passes: this
// runtime either enforces the rule or refuses it BY NAME. A grant that reaches
// a gateway unenforced would be the one failure mode the design forbids, so the
// same check runs at admission, at launch and inside the box.
func (s Snapshot) RequireSupported() error {
	if s.Mode != Filtered {
		return errors.New("gateway requires filtered authority")
	}
	counters := map[string]bool{}
	for _, grant := range s.Grants {
		if err := SupportedRule(grant.Rule); err != nil {
			return err
		}
		if name := grant.CounterName(); name != "" {
			if counters[name] {
				return errors.New("two address grants share one kernel counter name")
			}
			counters[name] = true
		}
	}
	// The gateway captures the agent's TCP on every port a tls grant names, so a
	// raw tcp grant on one of those ports would be redirected to the guard and
	// never reach its destination. That is the unenforced grant this gate exists
	// to refuse, and only the whole policy can see the collision.
	captured := s.TLSPorts()
	for _, grant := range s.Grants {
		rule := grant.Rule
		if rule.Protocol != "tcp" {
			continue
		}
		for _, port := range rule.Ports {
			if slices.Contains(captured, port) {
				return fmt.Errorf("raw tcp to %s port %d is not supported alongside a tls grant on port %d: the gateway captures that port for TLS", ruleDestination(rule), port, port)
			}
		}
	}
	return nil
}

// ruleDestination is the destination as a user wrote it, for a message about a
// rule this runtime refuses.
func ruleDestination(rule Rule) string {
	switch {
	case rule.To.Service != "":
		return "service " + rule.To.Service
	case rule.To.Domain != "":
		return rule.To.Domain
	}
	return rule.To.CIDR
}

// CounterName is the kernel counter this address grant's packets are counted
// on, or "" for a rule the packet filter never sees by itself (a TLS name is
// routed by the guard). nft counter names are bounded identifiers, so the
// stable rule ID is truncated; the snapshot itself remains the ID mapping.
func (g Grant) CounterName() string {
	if g.Rule.Protocol == "tls" || g.Rule.To.Provider != "" || len(g.ID) < 24 {
		return ""
	}
	return "grant_" + g.ID[:24]
}

// GrantByCounter maps an observed kernel counter back to the grant that owns
// it. An unknown name belongs to no grant and is never attributed to one.
func (s Snapshot) GrantByCounter(counter string) (Grant, bool) {
	for _, grant := range s.Grants {
		if name := grant.CounterName(); name != "" && name == counter {
			return grant, true
		}
	}
	return Grant{}, false
}

// SupportedRule reports whether the qualified runtime enforces this exact
// combination. Its errors are the message a user sees, so each one names the
// unsupported thing and does not suggest the rule was accepted.
func SupportedRule(rule Rule) error {
	if rule.To.Provider != "" {
		return errors.New("provider selectors expand into concrete rules before enforcement")
	}
	switch rule.Protocol {
	case "tls":
		if rule.To.Domain == "" {
			return errors.New("tls requires a domain; TLS to a bare address is not supported")
		}
		for _, port := range rule.Ports {
			if port == 53 {
				return errors.New("tls on port 53 is not supported: the gateway captures port 53 for its own DNS handling")
			}
		}
		return nil
	case "tcp", "udp":
		if rule.To.Service != "" {
			return supportedPorts(rule, "service "+rule.To.Service)
		}
		prefix, err := supportedPrefix(rule)
		if err != nil {
			return err
		}
		return supportedPorts(rule, prefix.String())
	case "icmp":
		prefix, err := supportedPrefix(rule)
		if err != nil {
			return err
		}
		for _, kind := range rule.Types {
			if kind != "8" {
				return fmt.Errorf("ICMP type %s to %s is not supported yet; only echo-request is qualified", kind, prefix)
			}
		}
		for _, code := range rule.Codes {
			if code != 0 {
				return fmt.Errorf("ICMP code %d is not supported yet; echo-request uses code 0", code)
			}
		}
		return nil
	case "icmpv6":
		return errors.New("IPv6 destinations are refused: this runtime is qualified for IPv4 only")
	}
	return errors.New("protocol must be tls, tcp, udp or icmp")
}

func supportedPrefix(rule Rule) (netip.Prefix, error) {
	if rule.To.Service != "" {
		return netip.Prefix{}, errors.New("a service grant is raw tcp or udp to that container, not ICMP")
	}
	prefix, err := netip.ParsePrefix(rule.To.CIDR)
	if err != nil {
		return netip.Prefix{}, errors.New("raw transports require a canonical ip or cidr destination")
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, errors.New("IPv6 destinations are refused: this runtime is qualified for IPv4 only")
	}
	if prefix.Bits() == 0 {
		return netip.Prefix{}, errors.New("a /0 grant is not filtered access; use explicit open egress for that intent")
	}
	// A destination entirely inside a permanent denial can never pass a packet:
	// accepting it would be exactly the unenforced grant this gate exists for.
	// A WIDER range that merely overlaps one stays valid — the kernel's
	// protected drop still wins inside it.
	for _, protected := range protectedPrefixes {
		if protected.Bits() <= prefix.Bits() && protected.Contains(prefix.Addr()) {
			return netip.Prefix{}, fmt.Errorf("%s is a protected address range (host, loopback, link-local or metadata); no rule can grant it", prefix)
		}
	}
	return prefix, nil
}

// 443 and 53 belong to the gateway's own capture of agent TLS and DNS. A raw
// grant on them would be silently redirected, so it is refused by name.
func supportedPorts(rule Rule, destination string) error {
	for _, port := range rule.Ports {
		if port == 443 || port == 53 {
			return fmt.Errorf("raw %s to %s port %d is not supported: the gateway captures port %d for its own TLS and DNS handling", rule.Protocol, destination, port, port)
		}
	}
	return nil
}
