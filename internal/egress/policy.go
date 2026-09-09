// Package egress owns the destination policy shared by launchers and the gateway.
// It neither opens network connections nor treats repository requests as grants.
package egress

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
	"gopkg.in/yaml.v3"
)

const (
	Version              = 1
	MaxDocumentBytes     = 64 << 10
	MaxRules             = 128
	MaxGrants            = 512
	MaxConstraints       = 64
	MaxFeatureExpansions = 512
	MaxExpandedRules     = 4096
	MaxGrantOrigins      = 4096
)

type Mode string

const (
	Open     Mode = "open"
	None     Mode = "none"
	Filtered Mode = "filtered"
)

func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case Open, None, Filtered:
		return Mode(value), nil
	default:
		return "", errors.New("egress must be open, none or filtered")
	}
}

type Destination struct {
	Domain   string   `json:"domain,omitempty" yaml:"domain,omitempty"`
	IP       string   `json:"ip,omitempty" yaml:"ip,omitempty"`
	CIDR     string   `json:"cidr,omitempty" yaml:"cidr,omitempty"`
	Provider string   `json:"provider,omitempty" yaml:"provider,omitempty"`
	Features []string `json:"features,omitempty" yaml:"features,omitempty"`
}

type Rule struct {
	To       Destination `json:"to" yaml:"to"`
	Protocol string      `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Ports    []int       `json:"ports,omitempty" yaml:"ports,omitempty"`
	Types    []string    `json:"types,omitempty" yaml:"types,omitempty"`
	Codes    []int       `json:"codes,omitempty" yaml:"codes,omitempty"`
}

// Keep field presence until shape validation is complete. Decoding straight into
// a struct would make an extra `ip: null` or forbidden `ports: []` disappear.
func (d *Destination) UnmarshalYAML(node *yaml.Node) error {
	fields, err := yamlFields(node, "domain", "ip", "cidr", "provider", "features")
	if err != nil {
		return err
	}
	selectors := 0
	for _, name := range []string{"domain", "ip", "cidr", "provider"} {
		if _, ok := fields[name]; ok {
			selectors++
		}
	}
	if selectors != 1 {
		return errors.New("to requires exactly one selector field")
	}
	if _, ok := fields["features"]; ok {
		if _, provider := fields["provider"]; !provider {
			return errors.New("features require a provider selector")
		}
	}
	type plain Destination
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	*d = Destination(value)
	return nil
}

func (r *Rule) UnmarshalYAML(node *yaml.Node) error {
	fields, err := yamlFields(node, "to", "protocol", "ports", "types", "codes")
	if err != nil {
		return err
	}
	if _, ok := fields["to"]; !ok {
		return errors.New("egress rule requires to")
	}
	type plain Rule
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	if value.To.Provider != "" && len(fields) != 1 {
		return errors.New("provider rules forbid transport fields")
	}
	*r = Rule(value)
	return nil
}

func yamlFields(node *yaml.Node, allowed ...string) (map[string]bool, error) {
	if node.Kind != yaml.MappingNode {
		return nil, errors.New("network rule requires a mapping")
	}
	fields := map[string]bool{}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!str" || !slices.Contains(allowed, key.Value) || fields[key.Value] || value.Tag == "!!null" {
			return nil, errors.New("network rule has an unknown, duplicate or null field")
		}
		fields[key.Value] = true
	}
	return fields, nil
}

// Origin explains authority; values are not instructions or user-facing prose.
type Origin struct {
	Kind     string `json:"kind"`
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	Feature  string `json:"feature,omitempty"`
	Provider string `json:"provider,omitempty"`
	Client   Client `json:"client,omitempty"`
	Backend  string `json:"backend,omitempty"`
	AuthMode string `json:"auth_mode,omitempty"`
	// Version identifies the requesting source; BundleVersion identifies the
	// dependency used to expand that source. Neither may overwrite the other.
	BundleVersion string `json:"bundle_version,omitempty"`
}

type Grant struct {
	ID      string   `json:"id"`
	Rule    Rule     `json:"rule"`
	Origins []Origin `json:"origins"`
}

// Bundle is supplied by a selected agent adapter, never by repository YAML.
type Bundle struct {
	Provider string            `json:"provider"`
	Client   Client            `json:"client"`
	Version  string            `json:"version"`
	Backend  string            `json:"backend"`
	AuthMode string            `json:"auth_mode"`
	Core     []Rule            `json:"core"`
	Features map[string][]Rule `json:"features,omitempty"`
	Sources  []string          `json:"sources"`
}

type Dependency struct {
	Provider string `json:"provider"`
	Client   Client `json:"client,omitempty"`
	Version  string `json:"version"`
	Backend  string `json:"backend"`
	AuthMode string `json:"auth_mode"`
}

// Snapshot contains concrete, frozen authority. Its fingerprint is keyed so a
// public policy digest cannot be used to guess private endpoint names offline.
type Snapshot struct {
	Version            int          `json:"version"`
	Scope              string       `json:"scope"`
	Mode               Mode         `json:"mode"`
	Grants             []Grant      `json:"grants"`
	Dependencies       []Dependency `json:"dependencies,omitempty"`
	ExportDestinations bool         `json:"export_destinations"`
	KeyID              string       `json:"key_id"`
	Fingerprint        string       `json:"fingerprint"`
}

type Requested struct {
	Rules []Rule `yaml:"egress_rules" json:"egress_rules"`
}

// DecodeRules accepts exactly one bounded YAML document and only the public rule
// shape. In particular, a file cannot smuggle approval, source or fingerprint data.
func DecodeRules(data []byte) ([]Rule, error) {
	if len(data) == 0 || len(data) > MaxDocumentBytes {
		return nil, fmt.Errorf("egress rules must contain 1..%d bytes", MaxDocumentBytes)
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	var value Requested
	if err := d.Decode(&value); err != nil {
		return nil, fmt.Errorf("egress rules: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("egress rules require exactly one YAML document")
	}
	return NormalizeRules(value.Rules)
}

func NormalizeRules(rules []Rule) ([]Rule, error) {
	return normalizeRules(rules, normalizeRule)
}

// CanonicalRules checks only the bounded, canonical rule syntax. It is for
// previously authenticated snapshots and private approval records, whose
// meaning must not change when the current public-suffix catalog changes.
// This is NOT admission: new requests, approvals and bundles use NormalizeRules.
func CanonicalRules(rules []Rule) ([]Rule, error) {
	return normalizeRules(rules, canonicalRule)
}

func normalizeRules(rules []Rule, normalize func(Rule) (Rule, error)) ([]Rule, error) {
	if len(rules) > MaxRules {
		return nil, fmt.Errorf("egress rules exceed %d entries", MaxRules)
	}
	byKey := make(map[string]Rule, len(rules))
	for i, rule := range rules {
		normalized, err := normalize(rule)
		if err != nil {
			return nil, fmt.Errorf("egress rule %d: %w", i+1, err)
		}
		byKey[ruleKey(normalized)] = normalized
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]Rule, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result, nil
}

func normalizeRule(rule Rule) (Rule, error) {
	rule, err := canonicalRule(rule)
	if err == nil && rule.To.Domain != "" {
		_, err = NormalizeDomain(rule.To.Domain, true)
	}
	if err != nil {
		return Rule{}, err
	}
	return rule, nil
}

func canonicalRule(rule Rule) (Rule, error) {
	selectors := 0
	for _, value := range []string{rule.To.Domain, rule.To.IP, rule.To.CIDR, rule.To.Provider} {
		if value != "" {
			selectors++
		}
	}
	if selectors != 1 {
		return Rule{}, errors.New("to requires exactly one domain, ip, cidr or provider")
	}
	if rule.To.Provider != "" {
		if !validLabel(rule.To.Provider) || rule.Protocol != "" || len(rule.Ports)+len(rule.Types)+len(rule.Codes) != 0 {
			return Rule{}, errors.New("provider selectors accept features, not transport constraints")
		}
		if len(rule.To.Features) > MaxConstraints {
			return Rule{}, errors.New("too many provider features")
		}
		rule.To.Features = slices.Clone(rule.To.Features)
		for _, feature := range rule.To.Features {
			if !validLabel(feature) {
				return Rule{}, errors.New("invalid provider feature")
			}
		}
		slices.Sort(rule.To.Features)
		rule.To.Features = slices.Compact(rule.To.Features)
		return rule, nil
	}
	if len(rule.To.Features) != 0 {
		return Rule{}, errors.New("features require a provider selector")
	}
	var prefix netip.Prefix
	if rule.To.Domain != "" {
		name, err := canonicalDomain(rule.To.Domain, true)
		if err != nil {
			return Rule{}, err
		}
		rule.To.Domain = name
	} else {
		var err error
		if rule.To.IP != "" {
			var ip netip.Addr
			ip, err = netip.ParseAddr(rule.To.IP)
			if err == nil && (ip.Zone() != "" || ip.Is4In6()) {
				err = errors.New("ambiguous address")
			}
			if err == nil {
				prefix = netip.PrefixFrom(ip, ip.BitLen())
			}
		} else {
			prefix, err = netip.ParsePrefix(rule.To.CIDR)
			if err == nil && (prefix != prefix.Masked() || prefix.Addr().Is4In6() || prefix.Bits() == 0) {
				err = errors.New("noncanonical or all-address prefix")
			}
		}
		if err != nil {
			return Rule{}, errors.New("ip/cidr must be canonical, unmapped and narrower than /0")
		}
		rule.To.IP, rule.To.CIDR = "", prefix.String()
	}
	var err error
	switch rule.Protocol {
	case "tls", "tcp", "udp":
		if (rule.Protocol == "tls") != (rule.To.Domain != "") {
			return Rule{}, errors.New("tls requires a domain; raw tcp/udp require an ip or cidr")
		}
		if len(rule.Types)+len(rule.Codes) != 0 {
			return Rule{}, errors.New("transport rules forbid ICMP types/codes")
		}
		rule.Ports, err = normalizeInts(rule.Ports, 1, 65535, true)
	case "icmp", "icmpv6":
		if !prefix.IsValid() || (rule.Protocol == "icmp") != prefix.Addr().Is4() || len(rule.Ports) != 0 {
			return Rule{}, errors.New("ICMP requires the matching address family and forbids ports")
		}
		if len(rule.Types) == 0 || len(rule.Types) > MaxConstraints {
			return Rule{}, errors.New("ICMP requires bounded types")
		}
		rule.Types = slices.Clone(rule.Types)
		for i, kind := range rule.Types {
			if kind == "echo-request" {
				if rule.Protocol == "icmp" {
					kind = "8"
				} else {
					kind = "128"
				}
			}
			if kind == "echo-reply" {
				if rule.Protocol == "icmp" {
					kind = "0"
				} else {
					kind = "129"
				}
			}
			n, e := strconv.Atoi(kind)
			if e != nil || n < 0 || n > 255 || strconv.Itoa(n) != kind {
				return Rule{}, errors.New("ICMP type must be a canonical 0..255 integer, echo-request or echo-reply")
			}
			rule.Types[i] = kind
		}
		slices.Sort(rule.Types)
		rule.Types = slices.Compact(rule.Types)
		rule.Codes, err = normalizeInts(rule.Codes, 0, 255, false)
	default:
		return Rule{}, errors.New("protocol must be tls, tcp, udp, icmp or icmpv6")
	}
	if err != nil {
		return Rule{}, err
	}
	return rule, nil
}

func normalizeInts(values []int, min, max int, required bool) ([]int, error) {
	if len(values) > MaxConstraints || (required && len(values) == 0) {
		return nil, errors.New("missing or excessive transport constraints")
	}
	result := slices.Clone(values)
	for _, value := range result {
		if value < min || value > max {
			return nil, fmt.Errorf("constraint must be in %d..%d", min, max)
		}
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func validLabel(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}

func NormalizeDomain(value string, wildcard bool) (string, error) {
	value, err := canonicalDomain(value, wildcard)
	if err != nil {
		return "", err
	}
	if name, isWildcard := strings.CutPrefix(value, "*."); isWildcard {
		if suffix, _ := publicsuffix.PublicSuffix(name); suffix == name {
			return "", errors.New("wildcard cannot cover a public suffix")
		}
	}
	return value, nil
}

func canonicalDomain(value string, wildcard bool) (string, error) {
	if len(value) > 254 {
		return "", errors.New("domain exceeds DNS length")
	}
	for i := range value {
		if value[i] > 127 {
			return "", errors.New("domain must use ASCII or explicit punycode")
		}
	}
	value = strings.ToLower(strings.TrimSuffix(value, "."))
	if len(value) > 253 {
		return "", errors.New("domain exceeds DNS length")
	}
	isWildcard := strings.HasPrefix(value, "*.")
	name := value
	if isWildcard {
		if !wildcard {
			return "", errors.New("an observed domain cannot be a wildcard")
		}
		name = strings.TrimPrefix(name, "*.")
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", errors.New("domain must be a fully qualified ASCII DNS name")
	}
	for _, label := range labels {
		if !validLabel(label) {
			return "", errors.New("domain contains an invalid ASCII label")
		}
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return "", errors.New("domain cannot be an IP address")
	}
	return value, nil
}

func safeProvenanceText(value string) bool {
	return utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) < 0
}

func MatchesDomain(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := pattern[1:]
	if !strings.HasSuffix(name, suffix) {
		return false
	}
	label := strings.TrimSuffix(name, suffix)
	return validLabel(label)
}

func ruleKey(rule Rule) string { data, _ := json.Marshal(rule); return string(data) }

func keyed(key []byte, scope string, data []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(scope + "\x00"))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}
